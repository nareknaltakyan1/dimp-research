package mw

import "net/http"

// RequireToken returns middleware that rejects requests without a token.
// Routers call it once while building their middleware chain.
func RequireToken(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Token") != token {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
