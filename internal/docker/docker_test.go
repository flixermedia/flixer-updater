package docker

import (
	"context"
	"strings"
	"testing"
)

func TestSelfUpdateAlreadyCurrent(t *testing.T) {
	var calls []string
	cli := New("flixer", "/srv/flixer", "/srv/flixer/docker-compose.yml", []string{"/state/versions.env"}, func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch {
		case args[0] == "inspect" && args[2] == "{{.Image}}":
			return "sha256:abc123", nil
		case args[0] == "inspect" && args[2] == "{{.Config.Image}}":
			return "flixermedia/flixer-updater:latest", nil
		case args[0] == "image":
			return "sha256:abc123", nil
		default:
			return "", nil
		}
	})

	updated, err := cli.SelfUpdate(context.Background())
	if err != nil {
		t.Fatalf("SelfUpdate failed: %v", err)
	}
	if updated {
		t.Fatalf("expected updated=false when image IDs match")
	}
	for _, call := range calls {
		if strings.Contains(call, " run ") {
			t.Fatalf("unexpected detached run when already up to date: %s", call)
		}
	}
}

func TestSelfUpdateSpawnsDetachedRecreate(t *testing.T) {
	var calls []string
	cli := New("flixer", "/srv/flixer", "/srv/flixer/docker-compose.yml", []string{"/state/versions.env"}, func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch {
		case args[0] == "inspect" && args[2] == "{{.Image}}":
			return "sha256:old", nil
		case args[0] == "inspect" && args[2] == "{{.Config.Image}}":
			return "flixermedia/flixer-updater:latest", nil
		case args[0] == "image":
			return "sha256:new", nil
		default:
			return "", nil
		}
	})

	updated, err := cli.SelfUpdate(context.Background())
	if err != nil {
		t.Fatalf("SelfUpdate failed: %v", err)
	}
	if !updated {
		t.Fatalf("expected updated=true when image IDs differ")
	}
	last := calls[len(calls)-1]
	if !strings.Contains(last, "run -d --rm --no-deps --entrypoint sh flixer-updater -c") {
		t.Fatalf("expected detached compose run, got: %s", last)
	}
	if !strings.Contains(last, "up' '--detach' '--no-deps' 'flixer-updater'") {
		t.Fatalf("expected recreate command inside helper, got: %s", last)
	}
}
