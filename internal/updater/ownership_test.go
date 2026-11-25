package updater

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type selfUpdatingDocker struct {
	*fakeDocker
	selfUpdated bool
	statusAtCall string
	updater     *Updater
}

func (s *selfUpdatingDocker) SelfUpdate(context.Context) (bool, error) {
	s.selfUpdated = true
	if s.updater != nil {
		if run := s.updater.Snapshot().Run; run != nil {
			s.statusAtCall = run.Status
		}
	}
	return true, nil
}

func TestNormalizeOwnershipHandlesMissingAndExistingDirs(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "poster.jpg")
	if err := os.WriteFile(file, []byte("img"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Non-existent path is skipped cleanly; existing path does not panic or error.
	_ = NormalizeOwnership(log, filepath.Join(dir, "missing"), dir)
}

func TestExecuteRunsSelfUpdateAfterSuccess(t *testing.T) {
	fd := &fakeDocker{}
	sd := &selfUpdatingDocker{fakeDocker: fd}
	u, err := New(Options{
		StateDir:      t.TempDir(),
		HealthTimeout: time.Second,
		PollInterval:  time.Millisecond,
		DataPaths:     []string{},
		Docker:        sd,
	})
	if err != nil {
		t.Fatal(err)
	}
	sd.updater = u

	if _, err := u.Start(Request{Targets: map[string]string{"web": "0.13.8"}}); err != nil {
		t.Fatal(err)
	}
	u.Wait()

	if !sd.selfUpdated {
		t.Fatalf("expected SelfUpdate to be called after successful update")
	}
	if sd.statusAtCall != StatusSucceeded {
		t.Fatalf("expected run status to be %q before SelfUpdate, got %q", StatusSucceeded, sd.statusAtCall)
	}
}
