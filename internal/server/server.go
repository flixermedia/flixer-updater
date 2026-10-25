// Package server exposes the updater over HTTP, protected by a shared bearer token.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/flixermedia/flixer-updater/internal/fullbackup"
	"github.com/flixermedia/flixer-updater/internal/updater"
)

// New returns the HTTP handler. Backup routes are enabled when a service is provided.
func New(token func() string, version string, u *updater.Updater, backupServices ...*fullbackup.Service) http.Handler {
	mux := http.NewServeMux()
	var backups *fullbackup.Service
	if len(backupServices) > 0 {
		backups = backupServices[0]
	}
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
	})
	mux.HandleFunc("GET /v1/status", authed(token, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"version": version, "state": u.Snapshot()})
	}))
	mux.HandleFunc("POST /v1/update", authed(token, func(w http.ResponseWriter, r *http.Request) {
		var req updater.Request
		if err := decodeJSON(w, r, &req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "The request body must be valid JSON."})
			return
		}
		run, err := u.Start(req)
		switch {
		case errors.Is(err, updater.ErrBusy):
			writeJSON(w, 409, map[string]string{"error": err.Error(), "code": "busy"})
		case errors.Is(err, updater.ErrInvalid):
			writeJSON(w, 400, map[string]string{"error": err.Error()})
		case err != nil:
			writeJSON(w, 500, map[string]string{"error": "The update could not be started."})
		default:
			writeJSON(w, 202, run)
		}
	}))
	if backups != nil {
		registerBackups(mux, token, u, backups)
	}
	return mux
}

func registerBackups(mux *http.ServeMux, token func() string, u *updater.Updater, b *fullbackup.Service) {
	mux.HandleFunc("POST /v1/backups/{name}/restore", authed(token, func(w http.ResponseWriter, r *http.Request) {
		var req fullbackup.RestoreRequest
		if decodeJSON(w, r, &req) != nil {
			writeJSON(w, 400, map[string]string{"error": "The request body must be valid JSON."})
			return
		}
		stored, err := b.ReadPassphrase()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "Could not read passphrase state."})
			return
		}
		pass := req.Passphrase
		if pass == "" {
			pass = stored
		}
		prepared, err := b.PrepareRestore(r.PathValue("name"), pass, req.Components)
		if err != nil {
			switch {
			case errors.Is(err, fullbackup.ErrBadPassphrase):
				writeJSON(w, 400, map[string]string{"error": err.Error(), "code": "bad_passphrase"})
			case errors.Is(err, fullbackup.ErrCorrupt):
				writeJSON(w, 400, map[string]string{"error": err.Error(), "code": "corrupt"})
			case errors.Is(err, fullbackup.ErrInvalid):
				writeJSON(w, 400, map[string]string{"error": err.Error()})
			default:
				http.NotFound(w, r)
			}
			return
		}
		if err := b.ValidateDatabaseURL(req.DatabaseURL); err != nil {
			prepared.Close()
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		run, err := u.StartOperation("restore", "", prepared.Header.Encrypted, func(p updater.Progress) error {
			p.Step("validate-archive", "Archive header, manifest, and payload validated")
			safetyPassphrase, readErr := b.ReadPassphrase()
			if readErr != nil {
				return errors.New("could not read stored backup passphrase")
			}
			_, restoreErr := b.Restore(context.Background(), prepared, req, safetyPassphrase, p)
			return restoreErr
		})
		if errors.Is(err, updater.ErrBusy) {
			prepared.Close()
			writeJSON(w, 409, map[string]string{"error": err.Error(), "code": "busy"})
		} else if err != nil {
			prepared.Close()
			writeJSON(w, 400, map[string]string{"error": err.Error()})
		} else {
			writeJSON(w, 202, run)
		}
	}))
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func statModTime(path string) (t time.Time) {
	if fi, e := os.Stat(path); e == nil {
		return fi.ModTime()
	}
	return time.Time{}
}
func authed(token func() string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		supplied, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		expected := token()
		if !found || expected == "" || subtle.ConstantTimeCompare([]byte(supplied), []byte(expected)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, 401, map[string]string{"error": "A valid updater token is required."})
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
