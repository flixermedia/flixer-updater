package fullbackup

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var NamePattern = regexp.MustCompile(`^flixer-[0-9TZ-]+\.flxbak$`)
var ErrInvalid = errors.New("invalid backup request")

// Docker controls only the stateful application services during restore.
type Docker interface {
	Compose(context.Context, ...string) (string, error)
	Health(context.Context, string) (string, error)
}

// Progress is implemented by the updater's shared operation tracker.
type Progress interface {
	Step(string, string)
	Logf(string, ...any)
	SetBackup(string)
	FailStep()
}
type Service struct {
	Dir, PassphraseFile, ConfigDir, SecretsDir, MetadataDir string
	DatabaseURL                                             string
	Versions                                                func() map[string]string
	Docker                                                  Docker
	HealthWaiter                                            func(context.Context, string) error
	Dump                                                    func(context.Context, string, string) error
	RestoreDB                                               func(context.Context, string, string) error
	HealthTimeout, PollInterval                             time.Duration
}
type BackupRequest struct {
	DatabaseURL string   `json:"databaseUrl"`
	Components  []string `json:"components"`
}
type RestoreRequest struct {
	DatabaseURL string   `json:"databaseUrl"`
	Passphrase  string   `json:"passphrase"`
	Components  []string `json:"components"`
}
type PreparedRestore struct {
	stage      string
	Header     Header
	Manifest   Manifest
	Components []string
}

func (s *Service) Path(name string) (string, error) {
	if !NamePattern.MatchString(name) {
		return "", ErrInvalid
	}
	p := filepath.Join(s.Dir, name)
	fi, e := os.Lstat(p)
	if e != nil {
		return "", e
	}
	if !fi.Mode().IsRegular() {
		return "", os.ErrNotExist
	}
	return p, nil
}

func (s *Service) Create(ctx context.Context, req BackupRequest, passphrase string, progress Progress) (string, error) {
	components, e := normalizeComponents(req.Components)
	if e != nil {
		return "", e
	}
	if e = os.MkdirAll(s.Dir, 0700); e != nil {
		return "", e
	}
	timestamp := time.Now().UTC().Format("20060102T150405Z")
	name := "flixer-" + timestamp + ".flxbak"
	for i := 1; fileExists(filepath.Join(s.Dir, name)); i++ {
		name = fmt.Sprintf("flixer-%s-%dZ.flxbak", strings.TrimSuffix(timestamp, "Z"), i)
	}
	sources := map[string]string{"config": s.ConfigDir, "secrets": s.SecretsDir, "metadata": s.MetadataDir}
	var dumpFile string
	if req.DatabaseURL == "" {
		req.DatabaseURL = s.DatabaseURL
	}
	if contains(components, "database") {
		progress.Step("dump-database", "Dumping database")
		if req.DatabaseURL == "" {
			return "", fmt.Errorf("%w: databaseUrl is required when database is selected", ErrInvalid)
		}
		f, e := os.CreateTemp(s.Dir, ".database-*.dump")
		if e != nil {
			return "", e
		}
		dumpFile = f.Name()
		_ = f.Chmod(0600)
		_ = f.Close()
		defer os.Remove(dumpFile)
		dump := s.Dump
		if dump == nil {
			dump = runPGDump
		}
		if e = dump(ctx, req.DatabaseURL, dumpFile); e != nil {
			return "", e
		}
		sources["database"] = dumpFile
	}
	for _, c := range components {
		if c != "database" {
			progress.Step("archive-"+c, "Archiving "+c)
			if _, e = os.Stat(sources[c]); e != nil {
				return "", fmt.Errorf("%s directory unavailable: %w", c, e)
			}
		}
	}
	progress.Step("encrypt-finish", "Writing backup archive")
	dest := filepath.Join(s.Dir, name)
	header, e := WriteArchive(dest, components, sources, versions(s.Versions), passphrase)
	if e != nil {
		return "", e
	}
	progress.SetBackup(name)
	progress.Logf("Backup written: %s (encrypted: %t)", name, header.Encrypted)
	return name, nil
}

func (s *Service) PrepareRestore(name, passphrase string, requested []string) (*PreparedRestore, error) {
	p, e := s.Path(name)
	if e != nil {
		return nil, e
	}
	stage, e := os.MkdirTemp(s.Dir, ".restore-*")
	if e != nil {
		return nil, e
	}
	h, m, e := Extract(p, stage, passphrase)
	if e != nil {
		_ = os.RemoveAll(stage)
		return nil, e
	}
	selected, e := normalizeComponents(requested)
	if e != nil {
		_ = os.RemoveAll(stage)
		return nil, e
	}
	included := []string{}
	for _, c := range selected {
		if contains(h.Components, c) {
			included = append(included, c)
		}
	}
	if len(included) == 0 {
		_ = os.RemoveAll(stage)
		return nil, fmt.Errorf("%w: no requested components are in the archive", ErrInvalid)
	}
	return &PreparedRestore{stage: stage, Header: h, Manifest: m, Components: included}, nil
}
func (p *PreparedRestore) Close() {
	if p != nil && p.stage != "" {
		_ = os.RemoveAll(p.stage)
	}
}

func (s *Service) Restore(ctx context.Context, p *PreparedRestore, req RestoreRequest, safetyPassphrase string, progress Progress) (safetyName string, retErr error) {
	if p == nil {
		return "", fmt.Errorf("%w: restore was not prepared", ErrInvalid)
	}
	if req.DatabaseURL == "" {
		req.DatabaseURL = s.DatabaseURL
	}
	defer p.Close()
	// The safety copy is mandatory and is created before stopping any services.
	progress.Step("safety-backup", "Creating a safety backup of the current state")
	components := []string{"database", "config", "secrets", "metadata"}
	safetyName, retErr = s.Create(ctx, BackupRequest{DatabaseURL: req.DatabaseURL, Components: components}, safetyPassphrase, progress)
	if retErr != nil {
		return "", fmt.Errorf("safety backup failed; restore aborted: %w", retErr)
	}
	progress.SetBackup(safetyName)
	services := []string{"api", "indexer", "streamer"}
	stopped := false
	if s.Docker == nil {
		return safetyName, fmt.Errorf("safety backup %s created, but docker is unavailable", safetyName)
	}
	progress.Step("stop-services", "Stopping api, indexer, and streamer")
	stopped = true
	if _, e := s.Docker.Compose(ctx, append([]string{"stop"}, services...)...); e != nil {
		progress.FailStep()
		return safetyName, fmt.Errorf("safety backup %s created; stopping services failed: %w", safetyName, e)
	}
	defer func() {
		if stopped {
			progress.Step("start-services", "Starting application services")
			if _, e := s.Docker.Compose(context.Background(), append([]string{"up", "--detach", "--no-deps"}, services...)...); e != nil {
				if retErr == nil {
					retErr = fmt.Errorf("safety backup %s created; restore succeeded but services failed to start: %w", safetyName, e)
				} else {
					retErr = fmt.Errorf("safety backup %s created; %v; restarting services failed: %w", safetyName, retErr, e)
				}
				return
			}
			for _, service := range services {
				if e := s.waitHealthy(context.Background(), service); e != nil {
					if retErr == nil {
						retErr = fmt.Errorf("safety backup %s created; services failed health check: %w", safetyName, e)
					} else {
						retErr = fmt.Errorf("safety backup %s created; %v; services failed health check: %w", safetyName, retErr, e)
					}
					return
				}
			}
		}
	}()
	if contains(p.Components, "database") {
		progress.Step("restore-database", "Restoring database")
		dump := filepath.Join(p.stage, "database.dump")
		restore := s.RestoreDB
		if restore == nil {
			restore = runPGRestore
		}
		if e := restore(ctx, req.DatabaseURL, dump); e != nil {
			progress.FailStep()
			return safetyName, fmt.Errorf("safety backup %s created; database restore failed: %w", safetyName, e)
		}
	}
	for _, component := range []string{"config", "secrets", "metadata"} {
		if contains(p.Components, component) {
			progress.Step("restore-"+component, "Restoring "+component)
			target := map[string]string{"config": s.ConfigDir, "secrets": s.SecretsDir, "metadata": s.MetadataDir}[component]
			if e := replaceContents(filepath.Join(p.stage, component), target); e != nil {
				progress.FailStep()
				return safetyName, fmt.Errorf("safety backup %s created; restoring %s failed: %w", safetyName, component, e)
			}
		}
	}
	// Start is explicit here; the defer still runs on all error paths after stop.
	progress.Step("start-services", "Starting application services")
	if _, e := s.Docker.Compose(ctx, append([]string{"up", "--detach", "--no-deps"}, services...)...); e != nil {
		progress.FailStep()
		return safetyName, fmt.Errorf("safety backup %s created; starting services failed: %w", safetyName, e)
	}
	for _, service := range services {
		if e := s.waitHealthy(ctx, service); e != nil {
			progress.FailStep()
			return safetyName, fmt.Errorf("safety backup %s created; service %s failed health check: %w", safetyName, service, e)
		}
	}
	stopped = false
	return safetyName, nil
}

func (s *Service) waitHealthy(ctx context.Context, service string) error {
	if s.HealthWaiter != nil {
		return s.HealthWaiter(ctx, service)
	}
	timeout := s.HealthTimeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	interval := s.PollInterval
	if interval <= 0 {
		interval = 3 * time.Second
	}
	deadline := time.Now().Add(timeout)
	stable := 0
	for {
		state, e := s.Docker.Health(ctx, service)
		if e == nil && state == "healthy" {
			return nil
		}
		if e == nil && state == "running" {
			stable++
			if stable >= 3 {
				return nil
			}
		} else if e == nil && (state == "unhealthy" || state == "exited" || state == "dead" || state == "missing") {
			return fmt.Errorf("%s is %s", service, state)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s remained %q", service, state)
		}
		time.Sleep(interval)
	}
}

func replaceContents(source, target string) error {
	if e := os.MkdirAll(target, 0700); e != nil {
		return e
	}
	entries, e := os.ReadDir(target)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if e = os.RemoveAll(filepath.Join(target, entry.Name())); e != nil {
			return e
		}
	}
	src, e := os.ReadDir(source)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	for _, entry := range src {
		if e = copyTree(filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name())); e != nil {
			return e
		}
	}
	return nil
}
func copyTree(src, dst string) error {
	fi, e := os.Lstat(src)
	if e != nil {
		return e
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("symlink not allowed")
	}
	if fi.IsDir() {
		if e = os.MkdirAll(dst, fi.Mode().Perm()); e != nil {
			return e
		}
		children, e := os.ReadDir(src)
		if e != nil {
			return e
		}
		for _, c := range children {
			if e = copyTree(filepath.Join(src, c.Name()), filepath.Join(dst, c.Name())); e != nil {
				return e
			}
		}
		return nil
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("unsupported file type")
	}
	if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
		return e
	}
	in, e := os.Open(src)
	if e != nil {
		return e
	}
	defer in.Close()
	out, e := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode().Perm())
	if e != nil {
		return e
	}
	_, ce := ioCopy(out, in)
	closeErr := out.Close()
	if ce != nil {
		return ce
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Chmod(dst, fi.Mode().Perm())
}

func runPGDump(ctx context.Context, databaseURL, file string) error {
	return runPostgres(ctx, "pg_dump", databaseURL, file)
}
func runPGRestore(ctx context.Context, databaseURL, file string) error {
	parsed, env, e := databaseEnv(databaseURL)
	if e != nil {
		return e
	}
	args := []string{"--clean", "--if-exists", "--no-owner", "--single-transaction", "--dbname", strings.TrimPrefix(parsed.Path, "/"), file}
	cmd := exec.CommandContext(ctx, "pg_restore", args...)
	cmd.Env = env
	out, e := cmd.CombinedOutput()
	if e != nil {
		_ = out
		return errors.New("pg_restore failed")
	}
	return nil
}
func runPostgres(ctx context.Context, command, databaseURL, file string) error {
	parsed, env, e := databaseEnv(databaseURL)
	if e != nil {
		return e
	}
	args := []string{"--format=custom", "--no-owner", "--file", file, "--dbname", strings.TrimPrefix(parsed.Path, "/")}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Env = env
	out, e := cmd.CombinedOutput()
	if e != nil {
		_ = out
		return fmt.Errorf("%s failed", command)
	}
	return nil
}
func databaseEnv(raw string) (*url.URL, []string, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme == "" || u.Host == "" || strings.TrimPrefix(u.Path, "/") == "" {
		return nil, nil, fmt.Errorf("%w: invalid databaseUrl", ErrInvalid)
	}
	username, password := "", ""
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	env := append(os.Environ(), "PGHOST="+u.Hostname(), "PGPORT="+port, "PGUSER="+username, "PGPASSWORD="+password, "PGDATABASE="+strings.TrimPrefix(u.Path, "/"))
	return u, env, nil
}
func fileExists(p string) bool { _, e := os.Stat(p); return e == nil }
func versions(f func() map[string]string) map[string]string {
	if f == nil {
		return map[string]string{}
	}
	return f()
}
func contains(items []string, want string) bool {
	for _, v := range items {
		if v == want {
			return true
		}
	}
	return false
}

// ioCopy is a narrow alias to keep file-copy behavior centralized and testable.
func ioCopy(dst *os.File, src *os.File) (int64, error) { return dst.ReadFrom(src) }

// ReadPassphrase reads the Flixer-written passphrase file fresh; a missing or empty file means no passphrase.
func (s *Service) ReadPassphrase() (string, error) {
	raw, err := os.ReadFile(s.PassphraseFile)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(raw), "\n"), nil
}

// ValidateDatabaseURL checks that a connection string is usable by pg_dump and pg_restore.
func (s *Service) ValidateDatabaseURL(raw string) error {
	if raw == "" {
		raw = s.DatabaseURL
	}
	if raw == "" {
		return fmt.Errorf("%w: databaseUrl is required", ErrInvalid)
	}
	_, _, err := databaseEnv(raw)
	return err
}
