package server

import (
	"context"
	"github.com/flixermedia/flixer-updater/internal/fullbackup"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

type backupDocker struct{ calls []string }

func (d *backupDocker) Compose(_ context.Context, args ...string) (string, error) {
	d.calls = append(d.calls, strings.Join(args, " "))
	return "", nil
}
func (d *backupDocker) Health(context.Context, string) (string, error) { return "healthy", nil }

func backupHandler(t *testing.T) (http.Handler, *updater.Updater, *fullbackup.Service, *backupDocker) {
	t.Helper()
	root := t.TempDir()
	docker := &backupDocker{}
	service := &fullbackup.Service{Dir: root + "/backups", PassphraseFile: root + "/backups/.passphrase", ConfigDir: root + "/config", SecretsDir: root + "/secrets", MetadataDir: root + "/metadata", DatabaseURL: "postgres://user:pass@localhost/db", Docker: docker, HealthTimeout: time.Second, PollInterval: time.Millisecond, Dump: func(_ context.Context, _ string, path string) error { return os.WriteFile(path, []byte("dump"), 0600) }, RestoreDB: func(context.Context, string, string) error { return nil }}
	for _, dir := range []string{service.ConfigDir, service.SecretsDir, service.MetadataDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	u, err := updater.New(updater.Options{StateDir: root + "/updater-state"})
	if err != nil {
		t.Fatal(err)
	}
	return New(func() string { return token }, "1.2.3", u, service), u, service, docker
}
func writeRestoreArchive(t *testing.T, s *fullbackup.Service, name, pass string) {
	t.Helper()
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(t.TempDir(), "db.dump")
	if err := os.WriteFile(dump, []byte("dump"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fullbackup.WriteArchive(filepath.Join(s.Dir, name), []string{"database"}, map[string]string{"database": dump}, nil, pass); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRequiresAuthAndRemovedRoutesAreGone(t *testing.T) {
	h, _, _, _ := backupHandler(t)
	if rec := do(h, "POST", "/v1/backups/flixer-20260101T000001Z.flxbak/restore", "", `{}`); rec.Code != 401 {
		t.Fatalf("unauthenticated got %d", rec.Code)
	}
	auth := "Bearer " + token
	for _, r := range [][2]string{{"GET", "/v1/backups"}, {"POST", "/v1/backups"}, {"GET", "/v1/backups/passphrase"}, {"PUT", "/v1/backups/passphrase"}, {"DELETE", "/v1/backups/flixer-20260101T000001Z.flxbak"}, {"GET", "/v1/backups/flixer-20260101T000001Z.flxbak"}, {"POST", "/v1/backups/upload"}} {
		if rec := do(h, r[0], r[1], auth, `{}`); rec.Code != 404 && rec.Code != 405 {
			t.Fatalf("%s %s got %d", r[0], r[1], rec.Code)
		}
	}
}

func TestRestoreBadPassphraseHasCodeAndTouchesNoServices(t *testing.T) {
	h, _, s, docker := backupHandler(t)
	name := "flixer-20260101T000001Z.flxbak"
	writeRestoreArchive(t, s, name, "correct-password")
	rec := do(h, "POST", "/v1/backups/"+name+"/restore", "Bearer "+token, `{"databaseUrl":"postgres://u:p@localhost/db","passphrase":"wrong-password"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"code":"bad_passphrase"`) {
		t.Fatalf("response=%d %s", rec.Code, rec.Body.String())
	}
	if len(docker.calls) != 0 {
		t.Fatalf("services touched: %v", docker.calls)
	}
}

func TestRestoreFallsBackToPassphraseFileAndBusy(t *testing.T) {
	h, u, s, _ := backupHandler(t)
	name := "flixer-20260101T000002Z.flxbak"
	writeRestoreArchive(t, s, name, "stored-password")
	if err := os.WriteFile(s.PassphraseFile, []byte("stored-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	hold := make(chan struct{})
	if _, err := u.StartOperation("restore", "", false, func(updater.Progress) error { <-hold; return nil }); err != nil {
		t.Fatal(err)
	}
	body := `{"databaseUrl":"postgres://u:p@localhost/db","components":["database"]}`
	rec := do(h, "POST", "/v1/backups/"+name+"/restore", "Bearer "+token, body)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), `"code":"busy"`) {
		t.Fatalf("busy response=%d %s", rec.Code, rec.Body.String())
	}
	close(hold)
	u.Wait()
	rec = do(h, "POST", "/v1/backups/"+name+"/restore", "Bearer "+token, body)
	if rec.Code != 202 || !strings.Contains(rec.Body.String(), `"kind":"restore"`) {
		t.Fatalf("restore response=%d %s", rec.Code, rec.Body.String())
	}
	u.Wait()
	if strings.Contains(rec.Body.String(), "stored-password") {
		t.Fatal("passphrase leaked")
	}
}
