package server

import (
	"context"
	"sync"
	"time"
)

type Server struct {
	mu       sync.Mutex
	sessions map[string]time.Time
}

func New(ctx context.Context) *Server {
	s := &Server{sessions: map[string]time.Time{}}
	go s.janitor(ctx, time.Minute)
	return s
}

// janitor expires idle sessions until ctx is cancelled.
func (s *Server) janitor(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.mu.Lock()
			for id, seen := range s.sessions {
				if now.Sub(seen) > 30*time.Minute {
					delete(s.sessions, id)
				}
			}
			s.mu.Unlock()
		}
	}
}

func (s *Server) Touch(ctx context.Context, id string) {
	s.mu.Lock()
	s.sessions[id] = time.Now()
	s.mu.Unlock()
}
