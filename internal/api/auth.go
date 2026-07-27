package api

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// requireAuth checks the Authorization: Bearer <api_key> header using a
// constant-time comparison, matching LibriNode's credential-check
// convention. Reads the key live from settings.APIKey() on every request, so
// a key regenerated through the settings API takes effect immediately.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(key), []byte(s.settings.APIKey())) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
