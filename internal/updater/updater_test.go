package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeDocker struct {
	mu              sync.Mutex
	calls           []string
	health          map[string]string
	healthSequence  map[string][]string
	failPull        bool
	failRestartOnce bool
	failRestart     bool
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
	if f.failRestartOnce && args[0] == "restart" {
		f.failRestartOnce = false
		return "", errors.New("restart failed")
	}
	if f.failRestart && args[0] == "restart" {
		return "", errors.New("restart failed")
	}
	return "", nil
}

func (f *fakeDocker) Health(_ context.Context, service string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if states := f.healthSequence[service]; len(states) > 0 {
		f.healthSequence[service] = states[1:]
		return states[0], nil
	}
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

func TestProxyRestartFailureRollsBack(t *testing.T) {
	d := &fakeDocker{failRestartOnce: true}
	u := newUpdater(t, d, nil)
	_, _ = u.Start(Request{Targets: map[string]string{"web": "0.9.0"}, Current: map[string]string{"web": "0.8.0"}})
	u.Wait()
	if run := u.Snapshot().Run; run.Status != StatusRolledBack || !strings.Contains(run.Error, "restart failed") {
		t.Fatalf("run %+v", run)
	}
	if raw, _ := os.ReadFile(u.VersionsFile()); !strings.Contains(string(raw), "WEB_VERSION=0.8.0") {
		t.Fatalf("not restored: %s", raw)
	}
}

func TestProxyRestartFailureDuringRollbackIsReported(t *testing.T) {
	d := &fakeDocker{failRestart: true}
	u := newUpdater(t, d, nil)
	_, _ = u.Start(Request{Targets: map[string]string{"web": "0.9.0"}, Current: map[string]string{"web": "0.8.0"}})
	u.Wait()
	if run := u.Snapshot().Run; run.Status != StatusRollbackFailed || !strings.Contains(run.Error, "reverse proxy could not be reconnected") {
		t.Fatalf("run %+v", run)
	}
}

func TestFinalHealthFailureRollsBackAllUpdatedComponents(t *testing.T) {
	d := &fakeDocker{healthSequence: map[string][]string{"web": {"healthy", "unhealthy", "healthy"}}}
	u := newUpdater(t, d, nil)
	_, _ = u.Start(Request{
		Targets: map[string]string{"web": "0.9.0", "flixer": "1.1.0"},
		Current: map[string]string{"web": "0.8.0", "flixer": "1.0.0"},
	})
	u.Wait()
	if run := u.Snapshot().Run; run.Status != StatusRolledBack || !strings.Contains(run.Error, "did not remain healthy") {
		t.Fatalf("run %+v", run)
	}
	if raw, _ := os.ReadFile(u.VersionsFile()); !strings.Contains(string(raw), "WEB_VERSION=0.8.0") || !strings.Contains(string(raw), "FLIXER_VERSION=1.0.0") {
		t.Fatalf("versions not restored: %s", raw)
	}
	for _, call := range []string{"up --detach --no-deps web", "up --detach --no-deps api"} {
		if strings.Count(strings.Join(d.calls, "\n"), call) != 2 {
			t.Fatalf("expected update and restore for %s: %v", call, d.calls)
		}
	}
}

func TestFinalRollbackHealthFailureIsReported(t *testing.T) {
	d := &fakeDocker{healthSequence: map[string][]string{"web": {"healthy", "unhealthy", "healthy", "unhealthy"}}}
	u := newUpdater(t, d, nil)
	_, _ = u.Start(Request{Targets: map[string]string{"web": "0.9.0"}, Current: map[string]string{"web": "0.8.0"}})
	u.Wait()
	if run := u.Snapshot().Run; run.Status != StatusRollbackFailed || !strings.Contains(run.Error, "did not remain healthy after rollback") {
		t.Fatalf("run %+v", run)
	}
}

func TestUnhealthyProxyRollsBack(t *testing.T) {
	d := &fakeDocker{healthSequence: map[string][]string{"proxy": {"unhealthy", "healthy"}}}
	u := newUpdater(t, d, nil)
	_, _ = u.Start(Request{Targets: map[string]string{"web": "0.9.0"}, Current: map[string]string{"web": "0.8.0"}})
	u.Wait()
	if run := u.Snapshot().Run; run.Status != StatusRolledBack || !strings.Contains(run.Error, "proxy did not become healthy") {
		t.Fatalf("run %+v", run)
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
	var output bytes.Buffer
	u, err := New(Options{StateDir: dir, Logger: slog.New(slog.NewJSONHandler(&output, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if run := u.Snapshot().Run; run.Status != StatusFailed {
		t.Fatalf("run %+v", run)
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil || record["task_id"] != "1" || record["msg"] != "operation interrupted by updater restart" {
		t.Fatalf("interrupted operation log: %s (%v)", output.String(), err)
	}
}

func TestOperationLogsHaveCorrelationAndPreserveRunDiagnostics(t *testing.T) {
	var output bytes.Buffer
	u, err := New(Options{
		StateDir: t.TempDir(), Logger: slog.New(slog.NewJSONHandler(&output, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := u.StartOperation("backup", "", false, func(progress Progress) error {
		progress.Step("snapshot", "Saving backup")
		progress.Logf("Backup completed")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	u.Wait()
	snapshot := u.Snapshot().Run
	if snapshot.Status != StatusSucceeded || len(snapshot.Log) != 2 || len(snapshot.Steps) != 1 {
		t.Fatalf("operation diagnostics changed: %+v", snapshot)
	}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["task_id"] != run.ID || record["operation_id"] != run.ID || record["operation_kind"] != "backup" {
			t.Fatalf("uncorrelated record: %v", record)
		}
	}
	if !strings.Contains(output.String(), `"status":"succeeded"`) {
		t.Fatalf("missing success lifecycle record: %s", output.String())
	}
}
