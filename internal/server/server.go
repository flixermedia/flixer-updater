// Package server exposes the updater over HTTP, protected by a shared bearer token.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/flixermedia/flixer-updater/internal/updater"
)

// New returns the HTTP handler.
func New(token, version string, u *updater.Updater) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
	})

	mux.HandleFunc("GET /v1/status", authed(token, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"version": version, "state": u.Snapshot()})
	}))

	mux.HandleFunc("POST /v1/update", authed(token, func(w http.ResponseWriter, r *http.Request) {
		var req updater.Request
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "The request body must be valid JSON."})
			return
		}
		run, err := u.Start(req)
		switch {
		case errors.Is(err, updater.ErrBusy):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case errors.Is(err, updater.ErrInvalid):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		case err != nil:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "The update could not be started."})
		default:
			writeJSON(w, http.StatusAccepted, run)
		}
	}))

	return mux
}

func authed(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		supplied, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !found || subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "A valid updater token is required."})
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
