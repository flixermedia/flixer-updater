package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/flixermedia/flixer-updater/internal/updater"
)

const token = "0123456789abcdef-token"

func handler(t *testing.T) http.Handler {
	t.Helper()
	u, err := updater.New(updater.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return New(func() string { return token }, "1.2.3", u)
}

func do(h http.Handler, method, path, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthIsOpen(t *testing.T) {
	if rec := do(handler(t), "GET", "/health", "", ""); rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestAuthRequired(t *testing.T) {
	h := handler(t)
	for _, auth := range []string{"", "Bearer wrong", "Basic " + token} {
		if rec := do(h, "GET", "/v1/status", auth, ""); rec.Code != 401 {
			t.Fatalf("auth %q got %d", auth, rec.Code)
		}
	}
	if rec := do(h, "GET", "/v1/status", "Bearer "+token, ""); rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestUpdateValidation(t *testing.T) {
	h := handler(t)
	auth := "Bearer " + token
	if rec := do(h, "POST", "/v1/update", auth, "not json"); rec.Code != 400 {
		t.Fatalf("bad json got %d", rec.Code)
	}
	if rec := do(h, "POST", "/v1/update", auth, `{"targets":{"web":"latest"}}`); rec.Code != 400 {
		t.Fatalf("bad version got %d", rec.Code)
	}
	if rec := do(h, "POST", "/v1/update", "", `{"targets":{"web":"1.0.0"}}`); rec.Code != 401 {
		t.Fatalf("unauthenticated got %d", rec.Code)
	}
}
