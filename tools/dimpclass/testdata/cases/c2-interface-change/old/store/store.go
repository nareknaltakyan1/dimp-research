package store

import "context"

// Store abstracts the session backend.
type Store interface {
	Get(ctx context.Context, key string) (string, bool)
	Put(ctx context.Context, key, val string)
}

type memStore struct{ m map[string]string }

func NewMem() Store { return &memStore{m: map[string]string{}} }

func (s *memStore) Get(ctx context.Context, key string) (string, bool) { v, ok := s.m[key]; return v, ok }
func (s *memStore) Put(ctx context.Context, key, val string)           { s.m[key] = val }

// Logout ends a session.
func Logout(ctx context.Context, s Store, key string) { s.Put(ctx, key, "") }
