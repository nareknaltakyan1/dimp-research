package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
)

type User struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Directory is the in-memory user table.
type Directory struct {
	mu    sync.RWMutex
	users map[string]User
}

func (d *Directory) Lookup(ctx context.Context, id string) (User, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	u, ok := d.users[id]
	return u, ok
}

// GetUser handles GET /users?id=...
func (d *Directory) GetUser(w http.ResponseWriter, r *http.Request) {
	u, ok := d.Lookup(r.Context(), r.URL.Query().Get("id"))
	if !ok {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(u)
}
