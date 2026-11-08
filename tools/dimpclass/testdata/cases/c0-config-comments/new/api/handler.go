package api

import (
	"encoding/json"
	"net/http"
)

// Health reports liveness of the service. It is used by the Kubernetes
// liveness probe and must stay cheap.
func Health(w http.ResponseWriter,
	r *http.Request) {
	w.Header().Set("Content-Type", "application/json") // always JSON
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"});
}
