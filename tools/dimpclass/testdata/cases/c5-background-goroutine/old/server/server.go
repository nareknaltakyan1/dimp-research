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
	return s
}

func (s *Server) Touch(ctx context.Context, id string) {
	s.mu.Lock()
	s.sessions[id] = time.Now()
	s.mu.Unlock()
}
