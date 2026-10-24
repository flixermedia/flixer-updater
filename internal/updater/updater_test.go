package updater

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeDocker struct {
	mu       sync.Mutex
	calls    []string
	health   map[string]string
	failPull bool
	// healthAfter lets a service be unhealthy only for the new version.
	unhealthyOnce map[string]bool
}

func (f *fakeDocker) Compose(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	if f.failPull && args[0] == "pull" {
		return "", errors.New("pull denied")
	}
	return "", nil
}

func (f *fakeDocker) Health(_ context.Context, service string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unhealthyOnce[service] {
		f.unhealthyOnce[service] = false
		return "unhealthy", nil
	}
	if state, ok := f.health[service]; ok {
		return state, nil
	}
	return "healthy", nil
}

type fakeBackup struct{ err error }

func (f fakeBackup) Backup(context.Context, string) (string, error) { return "flixer-test.dump", f.err }

func newUpdater(t *testing.T, d *fakeDocker, b Backuper) *Updater {
	t.Helper()
	u, err := New(Options{StateDir: t.TempDir(), HealthTimeout: time.Second, PollInterval: time.Millisecond, RestartServices: []string{"proxy"}, Docker: d, Backup: b})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestValidate(t *testing.T) {
	if _, err := Validate(Request{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := Validate(Request{Targets: map[string]string{"web": "latest"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad version: %v", err)
	}
	if _, err := Validate(Request{Targets: map[string]string{"nginx": "1.0.0"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown component: %v", err)
	}
	order, err := Validate(Request{Targets: map[string]string{"flixer": "1.0.0", "streamer": "1.0.0"}})
	if err != nil || order[0] != "streamer" || order[1] != "flixer" {
		t.Fatalf("order: %v %v", order, err)
	}
}

func TestSuccessfulUpdate(t *testing.T) {
	d := &fakeDocker{}
	u := newUpdater(t, d, fakeBackup{})
	if _, err := u.Start(Request{Targets: map[string]string{"web": "0.9.0"}, Current: map[string]string{"web": "0.8.0"}}); err != nil {
		t.Fatal(err)
	}
	u.Wait()
	run := u.Snapshot().Run
	if run.Status != StatusSucceeded || run.Backup != "flixer-test.dump" {
		t.Fatalf("run: %+v", run)
	}
	raw, _ := os.ReadFile(u.VersionsFile())
	if !strings.Contains(string(raw), "WEB_VERSION=0.9.0") {
		t.Fatalf("versions file: %s", raw)
	}
}

func TestBackupFailureChangesNothing(t *testing.T) {
	d := &fakeDocker{}
	u := newUpdater(t, d, fakeBackup{err: errors.New("disk full")})
	_, _ = u.Start(Request{Targets: map[string]string{"web": "0.9.0"}})
	u.Wait()
	if run := u.Snapshot().Run; run.Status != StatusFailed || len(d.calls) != 0 {
		t.Fatalf("run %+v calls %v", run, d.calls)
	}
}

func TestPullFailureRestoresVersions(t *testing.T) {
	d := &fakeDocker{failPull: true}
	u := newUpdater(t, d, nil)
	_, _ = u.Start(Request{Targets: map[string]string{"web": "0.9.0"}})
	u.Wait()
	if run := u.Snapshot().Run; run.Status != StatusFailed {
		t.Fatalf("run %+v", run)
	}
	if raw, _ := os.ReadFile(u.VersionsFile()); strings.Contains(string(raw), "0.9.0") {
		t.Fatalf("version still pinned: %s", raw)
	}
}

func TestUnhealthyRollsBack(t *testing.T) {
	d := &fakeDocker{unhealthyOnce: map[string]bool{"web": true}}
	u := newUpdater(t, d, nil)
	_, _ = u.Start(Request{Targets: map[string]string{"web": "0.9.0"}, Current: map[string]string{"web": "0.8.0"}})
	u.Wait()
	run := u.Snapshot().Run
	if run.Status != StatusRolledBack {
		t.Fatalf("run %+v", run)
	}
	if raw, _ := os.ReadFile(u.VersionsFile()); !strings.Contains(string(raw), "WEB_VERSION=0.8.0") {
		t.Fatalf("not restored: %s", raw)
	}
}

func TestRollbackWithoutKnownPreviousFails(t *testing.T) {
	d := &fakeDocker{unhealthyOnce: map[string]bool{"web": true}}
	u := newUpdater(t, d, nil)
	_, _ = u.Start(Request{Targets: map[string]string{"web": "0.9.0"}})
	u.Wait()
	if run := u.Snapshot().Run; run.Status != StatusRollbackFailed {
		t.Fatalf("run %+v", run)
	}
}

func TestBusy(t *testing.T) {
	d := &fakeDocker{health: map[string]string{"web": "starting"}}
	u := newUpdater(t, d, nil)
	u.opts.HealthTimeout = 200 * time.Millisecond
	if _, err := u.Start(Request{Targets: map[string]string{"web": "0.9.0"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Start(Request{Targets: map[string]string{"web": "0.9.1"}}); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected busy, got %v", err)
	}
	u.Wait()
}

func TestInterruptedRunIsMarkedFailed(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(dir+"/state.json", []byte(`{"versions":{},"run":{"id":"1","status":"running"}}`), 0o644)
	u, err := New(Options{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if run := u.Snapshot().Run; run.Status != StatusFailed {
		t.Fatalf("run %+v", run)
	}
}
