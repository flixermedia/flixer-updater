package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveIsStableAndSecretSpecific(t *testing.T) {
	if Derive("a") != Derive("a") || Derive("a") == Derive("b") {
		t.Fatal("derivation is not deterministic per secret")
	}
	// Same value Flixer computes with createHmac("sha256", "secret").update("flixer-updater-token").digest("hex").
	if got := Derive("secret"); got != "d171f4d65c41290afa1ba74b5aebaccc3ee1a6c7811ec0e9ddeaa6433180b65c" {
		t.Fatalf("unexpected token %q", got)
	}
}

func TestResolver(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secret")
	resolve := Resolver("", file)
	if resolve() != "" {
		t.Fatal("missing file must give an empty token")
	}
	_ = os.WriteFile(file, []byte("shared-secret"), 0o644)
	if resolve() != Derive("shared-secret") {
		t.Fatal("token not derived once the file exists")
	}
	if Resolver("override-token-123456", file)() != "override-token-123456" {
		t.Fatal("explicit token must win")
	}
}
