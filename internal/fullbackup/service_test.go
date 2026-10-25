package fullbackup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type testProgress struct {
	mu        sync.Mutex
	names     []string
	backup    string
	encrypted bool
}

func (p *testProgress) Step(n, m string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.names = append(p.names, n)
}
func (p *testProgress) Logf(string, ...any) {}
func (p *testProgress) SetBackup(n string)  { p.backup = n }
func (p *testProgress) FailStep()           {}

type testDocker struct {
	mu       sync.Mutex
	calls    []string
	failStop bool
}

func (d *testDocker) Compose(_ context.Context, args ...string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, strings.Join(args, " "))
	if d.failStop && args[0] == "stop" {
		return "", errors.New("fake stop failure")
	}
	return "", nil
}
func (d *testDocker) Health(context.Context, string) (string, error) { return "healthy", nil }

func testService(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	s := &Service{Dir: filepath.Join(root, "backups"), PassphraseFile: filepath.Join(root, "backups", ".passphrase"), ConfigDir: filepath.Join(root, "config"), SecretsDir: filepath.Join(root, "secrets"), MetadataDir: filepath.Join(root, "metadata"), DatabaseURL: "postgres://user:password@localhost/flixer", HealthTimeout: time.Second, PollInterval: time.Millisecond}
	for _, d := range []string{s.ConfigDir, s.SecretsDir, s.MetadataDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(s.ConfigDir, "flixer-config.db-wal"), []byte("wal-state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.ConfigDir, "secret-box-key"), []byte("sensitive-key"), 0600); err != nil {
		t.Fatal(err)
	}
	s.Dump = func(_ context.Context, _ string, file string) error {
		return os.WriteFile(file, []byte("pgdump"), 0600)
	}
	return s
}

func TestReadPassphraseFile(t *testing.T) {
	s := testService(t)
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		content *string
		want    string
	}{
		{"missing", nil, ""},
		{"exact", ptr("correct-horse"), "correct-horse"},
		{"single trailing newline", ptr("correct-horse\n"), "correct-horse"},
		{"only one newline stripped", ptr("pw\n\n"), "pw\n"},
		{"empty", ptr(""), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_ = os.Remove(s.PassphraseFile)
			if c.content != nil {
				if err := os.WriteFile(s.PassphraseFile, []byte(*c.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.ReadPassphrase()
			if err != nil || got != c.want {
				t.Fatalf("got %q err %v, want %q", got, err, c.want)
			}
		})
	}
	// Read fresh on every call: a change on disk is visible immediately.
	_ = os.WriteFile(s.PassphraseFile, []byte("first-pass"), 0600)
	_, _ = s.ReadPassphrase()
	_ = os.WriteFile(s.PassphraseFile, []byte("second-pass"), 0600)
	if got, _ := s.ReadPassphrase(); got != "second-pass" {
		t.Fatalf("cached passphrase %q", got)
	}
}

func ptr(s string) *string { return &s }

func TestSafetyBackupEncryptsOnlyWithPassphraseAndTouchesNothingElse(t *testing.T) {
	s := testService(t)
	if err := os.MkdirAll(filepath.Join(s.Dir, ".uploads"), 0700); err != nil {
		t.Fatal(err)
	}
	others := map[string]string{
		".passphrase":                    "secret-not-in-archive\n",
		"flixer-old.dump":                "old",
		"flixer-20250101T000000Z.flxbak": "flixer-written",
		"flixer-x.flxbak.tmp":            "partial",
		"notes.txt":                      "junk",
		".uploads/u1":                    "upload",
	}
	for n, c := range others {
		if err := os.WriteFile(filepath.Join(s.Dir, n), []byte(c), 0600); err != nil {
			t.Fatal(err)
		}
	}
	plain, err := s.Create(context.Background(), BackupRequest{Components: []string{"config"}}, "", &testProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if h, _, err := ReadHeaderFile(filepath.Join(s.Dir, plain)); err != nil || h.Encrypted {
		t.Fatalf("plain header %+v err %v", h, err)
	}
	pass := "secret-not-in-archive"
	enc, err := s.Create(context.Background(), BackupRequest{Components: []string{"config"}}, pass, &testProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if h, _, err := ReadHeaderFile(filepath.Join(s.Dir, enc)); err != nil || !h.Encrypted {
		t.Fatalf("encrypted header %+v err %v", h, err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.Dir, enc))
	if strings.Contains(string(raw), pass) {
		t.Fatal("passphrase appears in archive")
	}
	for n, c := range others {
		got, err := os.ReadFile(filepath.Join(s.Dir, n))
		if err != nil || string(got) != c {
			t.Fatalf("%s was touched: %q %v", n, got, err)
		}
	}
	entries, _ := os.ReadDir(s.Dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".flixer-backup-") || strings.HasPrefix(e.Name(), ".database-") || strings.HasPrefix(e.Name(), ".restore-") {
			t.Fatalf("leftover temp %s", e.Name())
		}
	}
	if !NamePattern.MatchString(plain) || !NamePattern.MatchString(enc) {
		t.Fatalf("names %s %s", plain, enc)
	}
}

func TestRestoreConfigReplacesContentsAndRemovesWalShm(t *testing.T) {
	s := testService(t)
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "flixer-config.db"), []byte("snapshot"), 0600)
	_ = os.WriteFile(filepath.Join(src, "secret-box-key"), []byte("restored-key"), 0600)
	_ = os.MkdirAll(s.Dir, 0700)
	archive := filepath.Join(s.Dir, "flixer-20260101T000003Z.flxbak")
	if _, err := WriteArchive(archive, []string{"config"}, map[string]string{"config": src}, nil, ""); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(s.ConfigDir, "flixer-config.db-shm"), []byte("shm"), 0600)
	prepared, err := s.PrepareRestore(filepath.Base(archive), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Docker = &testDocker{}
	if _, err = s.Restore(context.Background(), prepared, RestoreRequest{}, "", &testProgress{}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(s.ConfigDir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "flixer-config.db,secret-box-key" {
		t.Fatalf("config dir = %v", names)
	}
	if fi, _ := os.Stat(filepath.Join(s.ConfigDir, "secret-box-key")); fi.Mode().Perm() != 0600 {
		t.Fatalf("key mode %o", fi.Mode().Perm())
	}
}

func TestRestoreBadPassphraseBeforeStopping(t *testing.T) {
	s := testService(t)
	file := filepath.Join(s.Dir, "flixer-20260101T000000Z.flxbak")
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteArchive(file, []string{"database"}, map[string]string{"database": filepath.Join(t.TempDir(), "db.dump")}, nil, "secret-passphrase"); err == nil {
		t.Fatal("expected missing dump source error")
	}
	dump := filepath.Join(t.TempDir(), "database.dump")
	_ = os.WriteFile(dump, []byte("dump"), 0600)
	if _, err := WriteArchive(file, []string{"database"}, map[string]string{"database": dump}, nil, "secret-passphrase"); err != nil {
		t.Fatal(err)
	}
	docker := &testDocker{}
	s.Docker = docker
	if _, err := s.PrepareRestore(filepath.Base(file), "wrong-passphrase", nil); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("got %v", err)
	}
	if len(docker.calls) != 0 {
		t.Fatalf("services touched: %v", docker.calls)
	}
}

func TestRestoreRestartsServicesAfterDatabaseFailure(t *testing.T) {
	s := testService(t)
	dump := filepath.Join(t.TempDir(), "database.dump")
	_ = os.WriteFile(dump, []byte("dump"), 0600)
	archive := filepath.Join(s.Dir, "flixer-20260101T000001Z.flxbak")
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteArchive(archive, []string{"database"}, map[string]string{"database": dump}, nil, ""); err != nil {
		t.Fatal(err)
	}
	prepared, err := s.PrepareRestore(filepath.Base(archive), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	docker := &testDocker{}
	s.Docker = docker
	s.RestoreDB = func(context.Context, string, string) error { return errors.New("restore failed") }
	p := &testProgress{}
	name, err := s.Restore(context.Background(), prepared, RestoreRequest{}, "", p)
	if err == nil || !strings.Contains(err.Error(), name) {
		t.Fatalf("safety backup not identified in error: name=%s err=%v", name, err)
	}
	docker.mu.Lock()
	defer docker.mu.Unlock()
	joined := strings.Join(docker.calls, " | ")
	if !strings.Contains(joined, "stop api indexer streamer") || !strings.Contains(joined, "up --detach --no-deps api indexer streamer") {
		t.Fatalf("services not restarted: %s", joined)
	}
}

func TestBackupConfigIncludesWalAndSecretKey(t *testing.T) {
	s := testService(t)
	name, err := s.Create(context.Background(), BackupRequest{Components: []string{"config"}}, "", &testProgress{})
	if err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	if _, _, err = Extract(filepath.Join(s.Dir, name), stage, ""); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"flixer-config.db-wal", "secret-box-key"} {
		if _, err = os.Stat(filepath.Join(stage, "config", file)); err != nil {
			t.Fatalf("missing %s: %v", file, err)
		}
	}
}
