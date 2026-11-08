package handoff

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// noRetryBody makes a GET non-replayable for net/http, disabling the
// transport's transparent retry on dead keep-alive connections (the same
// trick bench/loadgen uses), so the close race is not masked.
type noRetryBody struct{}

func (noRetryBody) Read([]byte) (int, error) { return 0, io.EOF }
func (noRetryBody) Close() error             { return nil }

func noRetryGet(ctx context.Context, cl *http.Client, url string) (*http.Response, bool, error) {
	var reused bool
	trace := &httptrace.ClientTrace{GotConn: func(ci httptrace.GotConnInfo) { reused = ci.Reused }}
	req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, url, nil)
	req.Body, req.GetBody, req.ContentLength = noRetryBody{}, nil, 0
	resp, err := cl.Do(req)
	return resp, reused, err
}

// Review M5: the spawned child may connect before the spawning goroutine
// waits on the channel; the delivery must not depend on that.
func TestWaiterAcceptsConnectionBeforeReceiverWaits(t *testing.T) {
	s := NewServer(Config{Mode: ModeFDPass, Addr: "127.0.0.1:0", CtlSock: "unused", Version: "v1"})
	a, b := unixPair(t)
	w := s.newWaiter()
	if !s.offerToWaiter(a) { // nobody is receiving yet
		t.Fatal("child connection rejected because the receiver was not yet waiting")
	}
	if s.offerToWaiter(b) {
		t.Fatal("second connection must not be delivered to the same waiter")
	}
	if got := <-w; got != a {
		t.Fatal("wrong connection delivered")
	}
	// A connection delivered but never picked up is rejected by clearWaiter.
	w = s.newWaiter()
	if !s.offerToWaiter(b) {
		t.Fatal("offer failed")
	}
	s.clearWaiter()
	if s.offerToWaiter(a) {
		t.Fatal("offer after clearWaiter succeeded")
	}
	select {
	case <-w:
		t.Fatal("stray connection left in waiter")
	default:
	}
}

// fakeOld is a hand-driven predecessor for successor-side tests (fd-pass).
type fakeOld struct {
	ln   net.Listener
	addr string
	c    *net.UnixConn
}

func startFakeOld(t *testing.T, ctl string) (*fakeOld, chan *net.UnixConn) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ul, err := net.ListenUnix("unix", &net.UnixAddr{Name: ctl, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ul.Close() })
	ch := make(chan *net.UnixConn, 1)
	go func() {
		c, err := ul.AcceptUnix()
		if err == nil {
			ch <- c
		}
	}()
	return &fakeOld{ln: ln, addr: ln.Addr().String()}, ch
}

// drive runs the predecessor protocol up to and including "ready".
func (f *fakeOld) drive(t *testing.T, c *net.UnixConn, count uint64) {
	f.c = c
	if _, _, err := Expect(c, MsgHello); err != nil {
		t.Fatal(err)
	}
	lf, _ := ListenerFile(f.ln)
	if err := WriteMsg(c, Msg{Type: MsgOffer}, lf); err != nil {
		t.Fatal(err)
	}
	lf.Close()
	if _, _, err := Expect(c, MsgPrepared); err != nil {
		t.Fatal(err)
	}
	if err := WriteMsg(c, Msg{Type: MsgState, State: &State{Count: count}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Expect(c, MsgReady); err != nil {
		t.Fatal(err)
	}
}

func served(addr string, wait time.Duration) bool {
	cl := &http.Client{Timeout: wait, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := cl.Get("http://" + addr + "/api")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// Review M6: the successor must not accept a single connection before the
// predecessor commits; without commit it exits having served nothing.
func TestSuccessorServesOnlyAfterCommit(t *testing.T) {
	t.Run("abort-after-ready", func(t *testing.T) {
		ctl := filepath.Join(shortDir(t), "ctl")
		old, conns := startFakeOld(t, ctl)
		nw := NewServer(Config{Mode: ModeFDPass, Addr: old.addr, CtlSock: ctl, Version: "v2", ReadyTimeout: 3 * time.Second, Takeover: true})
		res := make(chan error, 1)
		go func() { res <- nw.takeover() }()
		old.drive(t, <-conns, 10)
		if served(old.addr, 300*time.Millisecond) {
			t.Fatal("successor served a request after ready but before commit")
		}
		_ = WriteMsg(old.c, Msg{Type: MsgAbort, Err: "rolled back"})
		if err := <-res; err == nil {
			t.Fatal("takeover succeeded without commit")
		}
		if served(old.addr, 300*time.Millisecond) {
			t.Fatal("aborted successor served a request")
		}
		if nw.openConns.Load() != 0 {
			t.Fatal("aborted successor accepted connections")
		}
	})
	t.Run("commit", func(t *testing.T) {
		ctl := filepath.Join(shortDir(t), "ctl")
		old, conns := startFakeOld(t, ctl)
		nw := NewServer(Config{Mode: ModeFDPass, Addr: old.addr, CtlSock: ctl, Version: "v2", ReadyTimeout: 3 * time.Second, Takeover: true})
		t.Cleanup(func() { nw.srv.Close() })
		res := make(chan error, 1)
		go func() { res <- nw.takeover() }()
		old.drive(t, <-conns, 10)
		if err := WriteMsg(old.c, Msg{Type: MsgCommit}); err != nil {
			t.Fatal(err)
		}
		if err := <-res; err != nil {
			t.Fatal(err)
		}
		if r := get(t, old.addr, "/api"); r.Version != "v2" || r.Count != 11 {
			t.Fatalf("after commit got %+v", r)
		}
	})
}

// After commit the predecessor never rolls back, even if the successor dies
// immediately (otherwise two servers would hand out overlapping counts).
func TestNoRollbackAfterCommit(t *testing.T) {
	ctl := filepath.Join(shortDir(t), "ctl")
	old := newOld(t, ModeFDPass, ctl)
	get(t, old.cfg.Addr, "/api")
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: ctl, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	_ = WriteMsg(c, Msg{Type: MsgHello, Mode: string(ModeFDPass), Version: "v2"})
	_, files, err := Expect(c, MsgOffer)
	if err != nil {
		t.Fatal(err)
	}
	closeAll(files)
	_ = WriteMsg(c, Msg{Type: MsgPrepared})
	if _, _, err := Expect(c, MsgState); err != nil {
		t.Fatal(err)
	}
	_ = WriteMsg(c, Msg{Type: MsgReady})
	if _, _, err := Expect(c, MsgCommit); err != nil {
		t.Fatal(err)
	}
	c.Close() // successor "dies" right after commit
	select {
	case <-old.done: // handoff reported success; no resume
	case <-time.After(5 * time.Second):
		t.Fatal("old did not finish after commit")
	}
	old.Svc.mu.Lock()
	sealed := old.Svc.sealed
	old.Svc.mu.Unlock()
	if !sealed {
		t.Fatal("old process resumed after commit")
	}
}

// The old process must not close an idle keep-alive connection at the seal:
// a request on it after the seal is served and carries Connection: close.
func TestDrainServesReusedKeepAliveConn(t *testing.T) {
	ctl := filepath.Join(shortDir(t), "ctl")
	old := newOldGrace(t, ModeFDPass, ctl, 2*time.Second)
	cl := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4}}
	url := "http://" + old.cfg.Addr + "/api"
	resp, _, err := noRetryGet(context.Background(), cl, url)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	old.beginDrain() // what the handoff does at the seal
	drained := make(chan drainStats, 1)
	go func() { st, _ := old.drain(); drained <- st }()
	time.Sleep(50 * time.Millisecond)

	resp, reused, err := noRetryGet(context.Background(), cl, url)
	if err != nil {
		t.Fatalf("request on idle keep-alive conn during drain failed: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if !reused {
		t.Fatal("connection was not reused (test precondition)")
	}
	if !resp.Close { // net/http client consumes the Connection header into resp.Close
		t.Fatal("response during drain lacks Connection: close")
	}
	select {
	case st := <-drained:
		if st.IdleClosed != 0 {
			t.Fatalf("server closed %d idle conns although the client closed them", st.IdleClosed)
		}
		if st.Duration > time.Second {
			t.Fatalf("drain took %v; should end when the client closed its connection", st.Duration)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("drain did not finish")
	}
}

// Connections that stay idle are closed only after IdleGrace.
func TestDrainClosesIdleConnAfterGrace(t *testing.T) {
	ctl := filepath.Join(shortDir(t), "ctl")
	old := newOldGrace(t, ModeFDPass, ctl, 200*time.Millisecond)
	c, err := net.Dial("tcp", old.cfg.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	fmt.Fprintf(c, "GET /api HTTP/1.1\r\nHost: x\r\n\r\n")
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	old.beginDrain()
	start := time.Now()
	st, _ := old.drain()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) && !isReset(err) {
		t.Fatalf("idle conn not closed by drain: %v", err)
	}
	if el := time.Since(start); el < 200*time.Millisecond {
		t.Fatalf("idle conn closed after %v, before the grace period", el)
	}
	if st.IdleClosed != 1 {
		t.Fatalf("IdleClosed=%d, want 1", st.IdleClosed)
	}
}

func isReset(err error) bool { return err != nil && strings.Contains(err.Error(), "reset") }

// End-to-end regression for the E2 finding: keep-alive clients WITHOUT
// transparent retries across a handoff must see zero errors.
func TestInProcessHandoffKeepAliveNoRetry(t *testing.T) {
	for _, mode := range []Mode{ModeFDPass, ModeReusePort} {
		t.Run(string(mode), func(t *testing.T) {
			for round := 0; round < 3; round++ {
				ctl := filepath.Join(shortDir(t), "ctl")
				old := newOldGrace(t, mode, ctl, 300*time.Millisecond)
				addr := old.cfg.Addr
				var stop atomic.Bool
				var errs, oks atomic.Int64
				var firstErr atomic.Value
				var wg sync.WaitGroup
				cl := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
				for i := 0; i < 16; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for !stop.Load() {
							resp, _, err := noRetryGet(context.Background(), cl, "http://"+addr+"/api")
							if err != nil {
								errs.Add(1)
								firstErr.CompareAndSwap(nil, err.Error())
								continue
							}
							var r apiResp
							_ = json.NewDecoder(resp.Body).Decode(&r)
							resp.Body.Close()
							if resp.StatusCode != 200 {
								errs.Add(1)
								firstErr.CompareAndSwap(nil, resp.Status)
								continue
							}
							oks.Add(1)
						}
					}()
				}
				time.Sleep(150 * time.Millisecond)
				nw := NewServer(Config{Mode: mode, Addr: addr, CtlSock: ctl, Version: "v2", ReadyTimeout: 3 * time.Second, DrainTimeout: 3 * time.Second, Takeover: true})
				if err := nw.takeover(); err != nil {
					t.Fatal(err)
				}
				select {
				case <-old.done:
				case <-time.After(5 * time.Second):
					t.Fatal("old never finished")
				}
				time.Sleep(100 * time.Millisecond)
				stop.Store(true)
				wg.Wait()
				nw.srv.Close()
				t.Logf("round %d: ok=%d errors=%d first=%v", round, oks.Load(), errs.Load(), firstErr.Load())
				if errs.Load() != 0 {
					t.Fatalf("%d errors on keep-alive connections across the handoff (first: %v)", errs.Load(), firstErr.Load())
				}
			}
		})
	}
}

func countFDs(t *testing.T) int {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skip("no /proc/self/fd")
	}
	return len(ents)
}

// Review Low: descriptors installed by the kernel must not leak when the
// control message is truncated.
func TestRecvClosesFDsOnTruncation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("MSG_CTRUNC semantics checked on Linux")
	}
	a, b := unixPair(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	var files []*os.File
	for i := 0; i < maxFDsPerMsg+8; i++ {
		files = append(files, w)
	}
	base := countFDs(t)
	if err := WriteMsg(a, Msg{Type: MsgOffer}, files...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadMsg(b); err == nil {
		t.Fatal("expected truncation error")
	}
	if n := countFDs(t); n != base {
		t.Fatalf("fd count %d after truncated receive, want %d (leak)", n, base)
	}
}
