package handoff

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Keep-alive-safe draining.
//
// The HTTP/1.1 idle-connection close race (RFC 9112 section 9.5): a server
// that closes an idle persistent connection can never be sure the client has
// not already written the next request on it. If the request is in flight
// (or sitting unread in the server's receive buffer) when close(2) happens,
// the client sees a reset or EOF without any response bytes. Only the client
// can resolve this, by retrying - which RFC 9112 section 9.3.1 permits only
// for idempotent requests, and which many clients do not do automatically.
//
// The old process therefore must not close idle keep-alive connections at
// the handoff instant. Instead, while draining:
//
//  1. Every response written from now on carries "Connection: close"
//     (drainWriter). For a non-pipelining client this is race-free: the
//     client only sends its next request after reading a response, and that
//     response tells it to close the connection, so it opens a new one -
//     which lands on the successor's listener.
//  2. Connections that are idle at the start of the drain are left open for
//     Config.IdleGrace. If the client reuses one in that period, rule 1
//     applies (the old process serves or proxies the request and closes).
//  3. Only connections that have stayed idle for the whole grace period are
//     closed by the server. This is the one remaining unsafe close: it races
//     only with a client that picks a connection it has not used for
//     IdleGrace at exactly that instant (Go's transport, for example, reuses
//     the most recently used idle connection first, so such connections are
//     at the bottom of its idle stack).
//
// http.Server.SetKeepAlivesEnabled(false) and http.Server.Shutdown are
// deliberately NOT used to start the drain: both call closeIdleConns, which
// closes every idle connection immediately (rule 3 with a zero grace) - the
// cause of the request loss measured in the E2 campaign.

// connTrack is the per-connection state recorded by Server.connState.
type connTrack struct {
	state atomic.Int32 // http.ConnState
	since atomic.Int64 // unix ns of the last state change
}

func (s *Server) connState(c net.Conn, st http.ConnState) {
	now := time.Now().UnixNano()
	switch st {
	case http.StateNew:
		t := &connTrack{}
		t.state.Store(int32(st))
		t.since.Store(now)
		s.conns.Store(c, t)
		s.openConns.Add(1)
	case http.StateActive, http.StateIdle:
		if v, ok := s.conns.Load(c); ok {
			t := v.(*connTrack)
			t.state.Store(int32(st))
			t.since.Store(now)
		}
	case http.StateClosed, http.StateHijacked:
		if _, ok := s.conns.LoadAndDelete(c); ok {
			s.openConns.Add(-1)
		}
	}
}

// closeIdleFor closes connections that have been idle (or accepted without a
// request) for at least d, and returns how many it closed.
func (s *Server) closeIdleFor(d time.Duration) int {
	cut := time.Now().Add(-d).UnixNano()
	n := 0
	s.conns.Range(func(k, v any) bool {
		t := v.(*connTrack)
		st := http.ConnState(t.state.Load())
		if (st == http.StateIdle || st == http.StateNew) && t.since.Load() <= cut {
			_ = k.(net.Conn).Close() // conn.serve's Peek fails -> StateClosed
			n++
		}
		return true
	})
	return n
}

// beginDrain switches every subsequent response to "Connection: close".
// It does not close anything.
func (s *Server) beginDrain() { s.draining.Store(true) }

// endDrain reverts beginDrain (rollback).
func (s *Server) endDrain() { s.draining.Store(false) }

// drainStats describes the last drain (reported in old_version_drained).
type drainStats struct {
	Duration   time.Duration
	IdleClosed int // connections the server had to close while idle (rule 3)
	OpenAtEnd  int64
}

// drain waits until every connection the old process accepted has been
// served and closed, then calls Shutdown (which by then has nothing left to
// close). Must be called after beginDrain and after the listener is closed.
//
// We do NOT call http.Server.Shutdown first: besides closing idle
// connections (see above), net/http's conn.serve drops a connection without
// any response if it reads the request after Shutdown began
// (server.go: "if c.server.shuttingDown() { return }"), so connections
// accepted just before the handoff whose request bytes were not yet read
// would see EOF.
func (s *Server) drain() (drainStats, error) {
	start := time.Now()
	deadline := start.Add(s.cfg.DrainTimeout)
	graceEnd := start.Add(s.cfg.IdleGrace)
	var st drainStats
	for s.openConns.Load() > 0 && time.Now().Before(deadline) {
		if !time.Now().Before(graceEnd) {
			st.IdleClosed += s.closeIdleFor(s.cfg.IdleGrace)
		}
		time.Sleep(2 * time.Millisecond)
	}
	st.OpenAtEnd = s.openConns.Load()
	st.Duration = time.Since(start)
	ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline)+time.Second)
	defer cancel()
	return st, s.srv.Shutdown(ctx)
}

// drainHandler wraps h so that responses whose header is written while the
// server is draining carry "Connection: close" (net/http then closes the
// connection after the response instead of keeping it alive).
func (s *Server) drainHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dw := &drainWriter{ResponseWriter: w, s: s}
		h.ServeHTTP(dw, r)
		dw.mark() // handler wrote nothing: net/http writes the header after we return
	})
}

// drainWriter decides about "Connection: close" when the header is actually
// written, not when the request arrived: a request admitted before the seal
// whose response is written after it must still close the connection.
type drainWriter struct {
	http.ResponseWriter
	s      *Server
	marked bool
}

func (d *drainWriter) mark() {
	if d.marked {
		return
	}
	d.marked = true
	if d.s.draining.Load() {
		d.ResponseWriter.Header().Set("Connection", "close")
	}
}

func (d *drainWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 { // informational; the final header comes later
		d.ResponseWriter.WriteHeader(code)
		return
	}
	d.mark()
	d.ResponseWriter.WriteHeader(code)
}

func (d *drainWriter) Write(b []byte) (int, error) {
	d.mark()
	return d.ResponseWriter.Write(b)
}

func (d *drainWriter) Flush() {
	d.mark()
	if f, ok := d.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (d *drainWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }
