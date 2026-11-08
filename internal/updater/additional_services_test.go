package updater

import (
	"os"
	"strings"
	"testing"
)

func TestAIAndLoggerUpdateAndRollback(t *testing.T) {
	docker := &fakeDocker{unhealthyOnce: map[string]bool{"ai": true}}
	u := newUpdater(t, docker, fakeBackup{})
	_, err := u.Start(Request{
		Targets: map[string]string{"ai": "1.2.0", "logger": "2.1.0"},
		Current: map[string]string{"ai": "1.1.0", "logger": "2.0.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	u.Wait()
	if run := u.Snapshot().Run; run.Status != StatusRolledBack {
		t.Fatalf("run = %+v", run)
	}
	raw, err := os.ReadFile(u.VersionsFile())
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"AI_VERSION=1.1.0", "LOGGER_VERSION=2.0.0"} {
		if !strings.Contains(string(raw), line) {
			t.Fatalf("missing %s in %s", line, raw)
		}
	}
}
