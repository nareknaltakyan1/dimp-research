package handoff

import (
	"encoding/json"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func init() { SetEventOutput(io.Discard) }

func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("/tmp", "hof")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func unixPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	p := filepath.Join(shortDir(t), "s")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ch := make(chan *net.UnixConn, 1)
	go func() { c, _ := l.AcceptUnix(); ch <- c }()
	a, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: p, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	b := <-ch
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func TestSCMRightsPassesWorkingFD(t *testing.T) {
	a, b := unixPair(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := WriteMsg(a, Msg{Type: MsgOffer}, w); err != nil {
		t.Fatal(err)
	}
	w.Close() // sender's copy gone; receiver's dup must still work
	m, files, err := ReadMsg(b)
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != MsgOffer || len(files) != 1 {
		t.Fatalf("got %+v with %d files", m, len(files))
	}
	if _, err := files[0].Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	files[0].Close()
	got, _ := io.ReadAll(r)
	if string(got) != "hi" {
		t.Fatalf("read %q via passed fd", got)
	}
}

func TestListenerFDPassAcceptsOnSameSocket(t *testing.T) {
	a, b := unixPair(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := ListenerFile(ln)
	// Queue a connection BEFORE the handoff and close the original listener:
	// the connection must survive in the shared accept queue.
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := WriteMsg(a, Msg{Type: MsgOffer}, f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	ln.Close()
	_, files, err := Expect(b, MsgOffer)
	if err != nil {
		t.Fatal(err)
	}
	ln2, err := net.FileListener(files[0])
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	_ = ln2.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second))
	sc, err := ln2.Accept()
	if err != nil {
		t.Fatalf("queued connection lost across handoff: %v", err)
	}
	sc.Close()
}

func TestProtocolErrors(t *testing.T) {
	a, b := unixPair(t)
	_ = WriteMsg(a, Msg{Type: MsgHello})
	if _, _, err := Expect(b, MsgReady); err == nil {
		t.Fatal("expected type mismatch error")
	}
	_ = WriteMsg(a, Msg{Type: MsgAbort, Err: "boom"})
	if _, _, err := ReadMsg(b); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("abort not surfaced: %v", err)
	}
	st := &State{Count: 42, Extra: map[string]string{"k": "v"}}
	_ = WriteMsg(a, Msg{Type: MsgState, State: st})
	m, _, err := Expect(b, MsgState)
	if err != nil || m.State.Count != 42 || m.State.Extra["k"] != "v" {
		t.Fatalf("state roundtrip: %+v %v", m.State, err)
	}
}

func TestSealReservesInflightNumbers(t *testing.T) {
	succ := NewService("v2", 0)
	succ.SetReady(true)
	sl, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(sl, succ.Handler())
	defer sl.Close()

	old := NewService("v1", 100)
	ol, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(ol, old.Handler())
	defer ol.Close()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); get(t, ol.Addr().String(), "/api?work_ms=200") }()
	}
	time.Sleep(50 * time.Millisecond)
	if n := old.Inflight(); n != 5 {
		t.Fatalf("inflight=%d", n)
	}
	next := old.Seal(sl.Addr().String())
	if next != 105 {
		t.Fatalf("Seal returned %d, want 105", next)
	}
	succ.SetCount(next)
	// A late request on the old server is forwarded to the successor.
	late := get(t, ol.Addr().String(), "/api")
	wg.Wait()
	if old.Count() != 105 {
		t.Fatalf("old count %d, want exactly the reserved 105", old.Count())
	}
	if late.Version != "v2" || late.Count != 106 {
		t.Fatalf("late request: %+v", late)
	}
}

type apiResp struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
	Count   uint64 `json:"count"`
}

func get(t *testing.T, addr, path string) apiResp {
	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		t.Errorf("GET %s: %v", path, err)
		return apiResp{}
	}
	defer resp.Body.Close()
	var r apiResp
	_ = json.NewDecoder(resp.Body).Decode(&r)
	return r
}

// newOld starts a "fresh" server in-process (no signal loop).
func newOld(t *testing.T, mode Mode, ctl string) *Server { return newOldGrace(t, mode, ctl, 0) }

// newOldGrace is newOld with an explicit Config.IdleGrace (0 = default).
func newOldGrace(t *testing.T, mode Mode, ctl string, grace time.Duration) *Server {
	s := NewServer(Config{Mode: mode, Addr: "127.0.0.1:0", CtlSock: ctl, Version: "v1", ReadyTimeout: 3 * time.Second, DrainTimeout: 3 * time.Second, IdleGrace: grace})
	if err := s.listenFresh(); err != nil {
		t.Fatal(err)
	}
	s.cfg.Addr = s.ln.Addr().String()
	s.serve()
	s.Svc.SetReady(true)
	if err := s.listenCtl(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.srv.Close(); s.closeCtl() })
	return s
}

func TestInProcessHandoffUnderLoad(t *testing.T) {
	for _, mode := range []Mode{ModeFDPass, ModeReusePort} {
		t.Run(string(mode), func(t *testing.T) {
			ctl := filepath.Join(shortDir(t), "ctl")
			old := newOld(t, mode, ctl)
			addr := old.cfg.Addr

			var stop atomic.Bool
			var errs, oks atomic.Int64
			var mu sync.Mutex
			seen := map[uint64]int{}
			var wg sync.WaitGroup
			cl := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for !stop.Load() {
						resp, err := cl.Get("http://" + addr + "/api?work_ms=2")
						if err != nil {
							errs.Add(1)
							continue
						}
						var r apiResp
						_ = json.NewDecoder(resp.Body).Decode(&r)
						resp.Body.Close()
						if resp.StatusCode != 200 {
							errs.Add(1)
							continue
						}
						oks.Add(1)
						mu.Lock()
						seen[r.Count]++
						mu.Unlock()
					}
				}()
			}
			time.Sleep(200 * time.Millisecond)

			nw := NewServer(Config{Mode: mode, Addr: addr, CtlSock: ctl, Version: "v2", ReadyTimeout: 3 * time.Second, DrainTimeout: 3 * time.Second, Takeover: true})
			if err := nw.takeover(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { nw.srv.Close() })
			select {
			case <-old.done:
			case <-time.After(5 * time.Second):
				t.Fatal("old never finished")
			}
			time.Sleep(200 * time.Millisecond)
			stop.Store(true)
			wg.Wait()

			st := get(t, addr, "/api")
			if st.Version != "v2" {
				t.Fatalf("after handoff served by %q", st.Version)
			}
			dups := 0
			for _, n := range seen {
				if n > 1 {
					dups++
				}
			}
			t.Logf("mode=%s ok=%d errors=%d dups=%d final=%d", mode, oks.Load(), errs.Load(), dups, st.Count)
			if dups != 0 {
				t.Fatalf("%d duplicate counts", dups)
			}
			if st.Count != uint64(oks.Load())+1 {
				t.Fatalf("count gap: final %d, successful requests %d", st.Count, oks.Load())
			}
			if mode == ModeFDPass && errs.Load() != 0 {
				t.Fatalf("fd-pass handoff produced %d errors", errs.Load())
			}
		})
	}
}

// A fake successor that receives state and then vanishes: the old server must
// roll back (resume on the same socket) and keep serving.
func TestRollbackAfterSeal(t *testing.T) {
	ctl := filepath.Join(shortDir(t), "ctl")
	old := newOld(t, ModeFDPass, ctl)
	addr := old.cfg.Addr
	get(t, addr, "/api")

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
	m, _, err := Expect(c, MsgState)
	if err != nil {
		t.Fatal(err)
	}
	if m.State.Count != 1 {
		t.Fatalf("state count %d", m.State.Count)
	}
	c.Close() // crash before ready
	deadline := time.Now().Add(3 * time.Second)
	for old.upgrading.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	r := get(t, addr, "/api")
	if r.Version != "v1" || r.Count != 2 {
		t.Fatalf("after rollback got %+v", r)
	}
	select {
	case <-old.done:
		t.Fatal("old exited despite failed upgrade")
	default:
	}
}

func TestLoopbackAndFlagArgs(t *testing.T) {
	if got := LoopbackAddr(&net.TCPAddr{IP: net.IPv6zero, Port: 8080}); got != "127.0.0.1:8080" {
		t.Fatal(got)
	}
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	fs.String("addr", ":8080", "")
	fs.String("version", "v1", "")
	_ = fs.Parse([]string{"-addr=:9090"})
	if got := strings.Join(FlagArgs(fs, "version"), " "); got != "-addr=:9090" {
		t.Fatalf("FlagArgs = %q", got)
	}
}
