package handoff

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Mode selects how the successor obtains the listening socket.
type Mode string

const (
	// ModeFDPass: the old process sends its listening socket to the new one
	// over SCM_RIGHTS (tableflip / Envoy hot restart / Meta socket takeover).
	ModeFDPass Mode = "fdpass"
	// ModeReusePort: the new process binds its own socket with SO_REUSEPORT.
	ModeReusePort Mode = "reuseport"
)

// EnvChild marks a process started as an upgrade successor.
const EnvChild = "DIMP_HANDOFF_CHILD"

// Config configures an upgradable server.
type Config struct {
	Mode          Mode
	Addr          string
	CtlSock       string // unix socket used for the handoff protocol
	Version       string
	NextBinary    string // default binary for SIGHUP / POST without bin=
	NextVersion   string // default "to" for SIGHUP
	AllowBinParam bool   // allow POST /admin/upgrade?bin=
	ReadyTimeout  time.Duration
	DrainTimeout  time.Duration
	// ChildArgs builds argv[1:] for the successor. sameBinary is true when
	// re-executing the current executable (then the version must be passed).
	ChildArgs func(toVersion string, sameBinary bool) []string
	// SimulateFail ("", "hang", "crash-after-state") makes THIS process
	// misbehave when it is a successor (rollback experiments).
	SimulateFail string
	// StallAccept makes the old process stop calling accept(2) for this long
	// before closing its listener (reuseport accept-queue pitfall demo).
	StallAccept time.Duration
	PIDFile     string
	Takeover    bool // connect to CtlSock and take over even if not spawned
}

// Server is an HTTP server that can hand itself off to a new binary.
type Server struct {
	cfg Config
	Svc *Service
	srv *http.Server

	mu     sync.Mutex
	ln     *gatedListener
	lnFile *os.File // fd-pass: our dup of the listening socket
	ctlLn  *net.UnixListener
	gen    int

	openConns atomic.Int64 // tracked via http.Server.ConnState

	upgrading atomic.Bool
	waiter    chan *net.UnixConn // non-nil while a spawned child is expected
	done      chan struct{}
	doneOnce  sync.Once
}

// NewServer builds a server; call Run.
func NewServer(cfg Config) *Server {
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 10 * time.Second
	}
	if cfg.DrainTimeout == 0 {
		cfg.DrainTimeout = 30 * time.Second
	}
	s := &Server{cfg: cfg, Svc: NewService(cfg.Version, 0), done: make(chan struct{})}
	s.srv = &http.Server{Handler: s.Svc.Handler(), ReadHeaderTimeout: 10 * time.Second, ConnState: s.connState}
	s.Svc.OnUpgrade = func(to, bin string) error {
		if bin != "" && !cfg.AllowBinParam {
			return errors.New("bin= not allowed (start with -allow-bin-param)")
		}
		return s.StartUpgrade(to, bin)
	}
	return s
}

// Run starts serving (fresh or as a successor) and blocks until this process
// has handed off and drained, or was told to stop.
func (s *Server) Run() error {
	child := os.Getenv(EnvChild) == "1" || s.cfg.Takeover
	_ = os.Unsetenv(EnvChild)
	if child {
		if err := s.takeover(); err != nil {
			return fmt.Errorf("takeover failed: %w", err)
		}
	} else {
		if err := s.listenFresh(); err != nil {
			return err
		}
		s.serve()
		s.Svc.SetReady(true)
	}
	if err := s.listenCtl(); err != nil {
		return err
	}
	s.writePID()

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)
	for {
		select {
		case <-s.done:
			return nil
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				if err := s.StartUpgrade("", ""); err != nil {
					Emit("upgrade_failed", map[string]any{"err": err.Error()})
				}
				continue
			}
			// Plain graceful stop (e.g. pod termination): stop accepting,
			// then serve everything already accepted before exiting.
			s.Svc.SetReady(false)
			s.closeCtl()
			s.mu.Lock()
			ln := s.ln
			s.mu.Unlock()
			s.srv.SetKeepAlivesEnabled(false)
			_ = ln.Close()
			_ = s.drain()
			return nil
		}
	}
}

func (s *Server) listenFresh() error {
	var ln net.Listener
	var err error
	switch s.cfg.Mode {
	case ModeReusePort:
		ln, err = ListenReusePort(context.Background(), s.cfg.Addr)
	default:
		ln, err = net.Listen("tcp", s.cfg.Addr)
	}
	if err != nil {
		return err
	}
	if s.cfg.Mode == ModeFDPass {
		f, err := ListenerFile(ln)
		if err != nil {
			return err
		}
		s.lnFile = f
	}
	s.ln = newGated(ln)
	return nil
}

func (s *Server) serve() {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	go func() { _ = s.srv.Serve(ln) }() // returns when ln is closed by a handoff
}

// ---------- successor side ----------

func (s *Server) takeover() error {
	deadline := time.Now().Add(s.cfg.ReadyTimeout)
	var c *net.UnixConn
	for {
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: s.cfg.CtlSock, Net: "unix"})
		if err == nil {
			c = conn
			break
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer c.Close()
	_ = c.SetDeadline(deadline)
	fail := func(err error) error {
		_ = WriteMsg(c, Msg{Type: MsgAbort, Err: err.Error()})
		return err
	}

	if err := WriteMsg(c, Msg{Type: MsgHello, PID: os.Getpid(), Version: s.cfg.Version, Mode: string(s.cfg.Mode)}); err != nil {
		return err
	}
	var ln net.Listener
	switch s.cfg.Mode {
	case ModeFDPass:
		m, files, err := Expect(c, MsgOffer)
		if err != nil {
			return err
		}
		if len(files) != 1 {
			closeAll(files)
			return fail(fmt.Errorf("expected 1 listener fd, got %d", len(files)))
		}
		l, err := net.FileListener(files[0])
		if err != nil {
			return fail(err)
		}
		s.lnFile = files[0] // keep for the next upgrade / our own rollback
		ln = l
		_ = m
	case ModeReusePort:
		l, err := ListenReusePort(context.Background(), s.cfg.Addr)
		if err != nil {
			return fail(err)
		}
		ln = l
	default:
		return fail(fmt.Errorf("unknown mode %q", s.cfg.Mode))
	}
	s.ln = newGated(ln)

	if s.cfg.SimulateFail == "hang" {
		time.Sleep(time.Hour) // never becomes prepared; old must time out and roll back
	}
	if err := WriteMsg(c, Msg{Type: MsgPrepared, PID: os.Getpid()}); err != nil {
		return err
	}
	m, _, err := Expect(c, MsgState)
	if err != nil {
		return err
	}
	if m.State == nil {
		return fail(errors.New("state message without state"))
	}
	if s.cfg.SimulateFail == "crash-after-state" {
		os.Exit(3)
	}
	s.Svc.SetCount(m.State.Count)
	s.gen = m.State.Generation + 1
	s.serve()
	s.Svc.SetReady(true)
	if err := selfCheck(LoopbackAddr(ln.Addr())); err != nil {
		return fail(fmt.Errorf("self-check: %w", err))
	}
	if err := WriteMsg(c, Msg{Type: MsgReady, PID: os.Getpid(), Version: s.cfg.Version}); err != nil {
		return err
	}
	Emit("new_version_serving", map[string]any{"version": s.cfg.Version, "count_start": m.State.Count, "generation": s.gen, "mode": string(s.cfg.Mode)})
	return nil
}

func selfCheck(addr string) error {
	cl := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := cl.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz status %d", resp.StatusCode)
	}
	return nil
}

// ---------- predecessor side ----------

func (s *Server) listenCtl() error {
	_ = os.Remove(s.cfg.CtlSock)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.cfg.CtlSock, Net: "unix"})
	if err != nil {
		return fmt.Errorf("ctl socket: %w", err)
	}
	l.SetUnlinkOnClose(false) // our successor re-creates the path
	s.mu.Lock()
	s.ctlLn = l
	s.mu.Unlock()
	go s.ctlLoop(l)
	return nil
}

func (s *Server) closeCtl() {
	s.mu.Lock()
	if s.ctlLn != nil {
		_ = s.ctlLn.Close()
	}
	s.mu.Unlock()
}

func (s *Server) ctlLoop(l *net.UnixListener) {
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			return
		}
		s.mu.Lock()
		w := s.waiter
		s.mu.Unlock()
		if w != nil {
			select {
			case w <- c:
				continue
			default:
			}
		}
		// Externally started successor (e.g. `kubectl exec ... -takeover`).
		if !s.upgrading.CompareAndSwap(false, true) {
			_ = WriteMsg(c, Msg{Type: MsgAbort, Err: "upgrade already in progress"})
			c.Close()
			continue
		}
		Emit("upgrade_start", map[string]any{"from": s.cfg.Version, "to": "external", "mode": string(s.cfg.Mode)})
		go s.finish(s.handoff(c, nil))
	}
}

// StartUpgrade spawns the successor binary and runs the handoff
// asynchronously. It returns an error only if an upgrade is already running.
func (s *Server) StartUpgrade(to, bin string) error {
	if !s.upgrading.CompareAndSwap(false, true) {
		return errors.New("upgrade already in progress")
	}
	if to == "" {
		to = s.cfg.NextVersion
	}
	self, _ := os.Executable()
	if bin == "" {
		bin = s.cfg.NextBinary
	}
	if bin == "" {
		bin = self
	}
	Emit("upgrade_start", map[string]any{"from": s.cfg.Version, "to": to, "bin": bin, "mode": string(s.cfg.Mode)})
	w := make(chan *net.UnixConn)
	s.mu.Lock()
	s.waiter = w
	s.mu.Unlock()
	go func() {
		err := s.spawnAndHandoff(w, to, bin, bin == self)
		s.mu.Lock()
		s.waiter = nil
		s.mu.Unlock()
		s.finish(err)
	}()
	return nil
}

func (s *Server) spawnAndHandoff(w chan *net.UnixConn, to, bin string, same bool) error {
	cmd := exec.Command(bin, s.cfg.ChildArgs(to, same)...)
	cmd.Env = append(os.Environ(), EnvChild+"=1")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn %s: %w", bin, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case c := <-w:
		return s.handoff(c, cmd)
	case err := <-exited:
		return fmt.Errorf("successor exited before connecting: %v", err)
	case <-time.After(s.cfg.ReadyTimeout):
		_ = cmd.Process.Kill()
		return errors.New("successor did not connect within ready timeout")
	}
}

func (s *Server) finish(err error) {
	if err != nil {
		Emit("upgrade_failed", map[string]any{"err": err.Error(), "version": s.cfg.Version, "count": s.Svc.Count()})
		s.upgrading.Store(false)
		return
	}
	s.doneOnce.Do(func() { close(s.done) })
}

// handoff runs the predecessor half of the protocol on c. cmd is the spawned
// successor (nil for an external one) and is killed on failure.
func (s *Server) handoff(c *net.UnixConn, cmd *exec.Cmd) (err error) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(s.cfg.ReadyTimeout))
	sealed := false
	defer func() {
		if err == nil {
			return
		}
		_ = WriteMsg(c, Msg{Type: MsgAbort, Err: err.Error()})
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		if sealed {
			if rerr := s.resumeServing(); rerr != nil {
				err = fmt.Errorf("%v; ROLLBACK FAILED: %v", err, rerr)
			}
		}
	}()

	hello, _, err := Expect(c, MsgHello)
	if err != nil {
		return err
	}
	if hello.Mode != string(s.cfg.Mode) {
		return fmt.Errorf("successor mode %q != %q", hello.Mode, s.cfg.Mode)
	}
	if s.cfg.Mode == ModeFDPass {
		if err := WriteMsg(c, Msg{Type: MsgOffer, Addr: s.cfg.Addr}, s.lnFile); err != nil {
			return err
		}
	}
	if _, _, err := Expect(c, MsgPrepared); err != nil {
		return fmt.Errorf("successor not prepared: %w", err)
	}

	// Stop accepting. For reuseport this removes our socket from the group,
	// resetting anything still in our accept queue (the known pitfall). For
	// fd-pass the socket stays open in the successor, so its queue survives.
	if s.cfg.StallAccept > 0 {
		s.ln.Pause()
		time.Sleep(s.cfg.StallAccept)
	}
	s.srv.SetKeepAlivesEnabled(false)
	_ = s.ln.Close()
	sealed = true
	next := s.Svc.Seal(LoopbackAddr(s.ln.Addr()))
	st := &State{Count: next, FromVersion: s.cfg.Version, FromPID: os.Getpid(), SealedAtNs: time.Now().UnixNano(), Generation: s.gen,
		Extra: map[string]string{"inflight_at_seal": strconv.FormatInt(s.Svc.Inflight(), 10)}}
	if err := WriteMsg(c, Msg{Type: MsgState, State: st}); err != nil {
		return err
	}
	if _, _, err := Expect(c, MsgReady); err != nil {
		return fmt.Errorf("successor not ready: %w", err)
	}

	// Successor serves; drain our in-flight (and proxied late) requests.
	s.closeCtl()
	derr := s.drain()
	Emit("old_version_drained", map[string]any{"version": s.cfg.Version, "count": s.Svc.Count(), "drain_err": errString(derr)})
	Emit("upgrade_done", map[string]any{"from": s.cfg.Version, "to": hello.Version, "new_pid": hello.PID})
	return nil
}

func (s *Server) connState(_ net.Conn, st http.ConnState) {
	switch st {
	case http.StateNew:
		s.openConns.Add(1)
	case http.StateClosed, http.StateHijacked:
		s.openConns.Add(-1)
	}
}

// drain waits until every connection the old process accepted has been
// served and closed, and only then calls Shutdown.
//
// We deliberately do NOT call http.Server.Shutdown first: net/http's
// conn.serve drops a connection without any response if it reads the
// request after Shutdown began (server.go: "if c.server.shuttingDown()
// { return }"), so connections accepted just before the handoff whose
// request bytes were not yet read would see EOF. With keep-alives disabled
// every such connection is closed right after its (served or proxied)
// response, so openConns reaches 0.
func (s *Server) drain() error {
	deadline := time.Now().Add(s.cfg.DrainTimeout)
	for s.openConns.Load() > 0 && time.Now().Before(deadline) {
		s.srv.SetKeepAlivesEnabled(false) // closes conns that went idle
		time.Sleep(2 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline)+time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// resumeServing is the post-seal rollback: re-acquire the listener and
// resume admitting requests.
func (s *Server) resumeServing() error {
	var ln net.Listener
	var err error
	if s.cfg.Mode == ModeFDPass {
		ln, err = net.FileListener(s.lnFile) // same socket: its queue never died
	} else {
		ln, err = ListenReusePort(context.Background(), s.cfg.Addr)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = newGated(ln)
	s.mu.Unlock()
	s.srv.SetKeepAlivesEnabled(true)
	s.Svc.Unseal()
	s.Svc.SetReady(true)
	s.serve()
	return nil
}

func (s *Server) writePID() {
	if s.cfg.PIDFile == "" {
		return
	}
	tmp := s.cfg.PIDFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())), 0o644); err == nil {
		_ = os.Rename(tmp, s.cfg.PIDFile)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// FlagArgs re-serialises the parsed flags of fs as argv, skipping names in
// skip, so a successor runs with the same configuration.
func FlagArgs(fs *flag.FlagSet, skip ...string) []string {
	sk := map[string]bool{}
	for _, n := range skip {
		sk[n] = true
	}
	var out []string
	fs.VisitAll(func(f *flag.Flag) {
		if !sk[f.Name] {
			out = append(out, "-"+f.Name+"="+f.Value.String())
		}
	})
	return out
}

// gatedListener lets the old process stop calling accept(2) without closing
// the socket (used to demonstrate the reuseport accept-queue pitfall).
type gatedListener struct {
	net.Listener
	mu     sync.Mutex
	paused chan struct{} // non-nil while paused
	closed chan struct{}
	once   sync.Once
}

func newGated(l net.Listener) *gatedListener {
	return &gatedListener{Listener: l, closed: make(chan struct{})}
}

func (g *gatedListener) Pause() {
	g.mu.Lock()
	if g.paused == nil {
		g.paused = make(chan struct{})
	}
	g.mu.Unlock()
}

func (g *gatedListener) Accept() (net.Conn, error) {
	g.mu.Lock()
	p := g.paused
	g.mu.Unlock()
	if p != nil {
		select {
		case <-p:
		case <-g.closed:
			return nil, net.ErrClosed
		}
	}
	return g.Listener.Accept()
}

func (g *gatedListener) Close() error {
	var err error
	g.once.Do(func() {
		close(g.closed)
		err = g.Listener.Close()
	})
	return err
}
