package fullbackup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	cryptorand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncryptedRoundTripMultiChunk(t *testing.T) {
	dir := t.TempDir()
	metadata := filepath.Join(dir, "metadata")
	if err := os.MkdirAll(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, chunkSize*3+123)
	if _, err := cryptorand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metadata, "large.bin"), data, 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "test.flxbak")
	h, err := WriteArchive(file, []string{"metadata"}, map[string]string{"metadata": metadata}, map[string]string{"flixer": "1.2.3"}, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !h.Encrypted {
		t.Fatal("expected encryption")
	}
	stage := filepath.Join(dir, "stage")
	_, _, err = Extract(file, stage, "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(stage, "metadata", "large.bin"))
	if err != nil || !bytes.Equal(data, actual) {
		t.Fatalf("roundtrip mismatch: %v", err)
	}
	if _, _, err = Extract(file, filepath.Join(dir, "wrong"), "incorrect horse"); !errors.Is(err, ErrBadPassphrase) {
		t.Fatalf("wrong passphrase error = %v", err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	headerLen := binary.BigEndian.Uint32(raw[len(magic) : len(magic)+4])
	body := len(magic) + 4 + int(headerLen)
	framePos, frames := body, 0
	for {
		if framePos+5 > len(raw) {
			t.Fatal("missing encrypted frame")
		}
		flag := raw[framePos]
		size := int(binary.BigEndian.Uint32(raw[framePos+1 : framePos+5]))
		framePos += 5 + size
		frames++
		if framePos > len(raw) {
			t.Fatal("truncated encrypted frame")
		}
		if flag == 1 {
			break
		}
	}
	if frames < 2 {
		t.Fatalf("expected multiple encrypted chunks, got %d", frames)
	}
	tampered := append([]byte(nil), raw...)
	tampered[body+5] ^= 0x40
	bad := filepath.Join(dir, "tampered.flxbak")
	if err = os.WriteFile(bad, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = Extract(bad, filepath.Join(dir, "tamper-out"), "correct horse"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("tamper error = %v", err)
	}
	truncated := filepath.Join(dir, "truncated.flxbak")
	if err = os.WriteFile(truncated, raw[:len(raw)-3], 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = Extract(truncated, filepath.Join(dir, "truncated-out"), "correct horse"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncation error = %v", err)
	}
}

func TestExtractRejectsTraversalAndSymlink(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  byte
		entry string
	}{{"traversal", tar.TypeReg, "../escape"}, {"absolute", tar.TypeReg, "/escape"}, {"symlink", tar.TypeSymlink, "metadata/link"}, {"hardlink", tar.TypeLink, "metadata/link"}} {
		t.Run(tc.name, func(t *testing.T) {
			file := writeUnsafeArchive(t, tc.entry, tc.kind)
			if _, _, err := Extract(file, t.TempDir(), ""); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func writeUnsafeArchive(t *testing.T, name string, kind byte) string {
	t.Helper()
	dir := t.TempDir()
	created := "2026-01-01T00:00:00Z"
	h := Header{CreatedAt: created, Components: []string{"metadata"}, Versions: map[string]string{}}
	hb, _ := json.Marshal(h)
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	tw := tar.NewWriter(gz)
	manifest, _ := json.Marshal(Manifest{Version: 1, CreatedAt: created, Components: []string{"metadata"}, Versions: map[string]string{}})
	_ = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(manifest))})
	_, _ = tw.Write(manifest)
	header := &tar.Header{Name: name, Mode: 0600, Size: 1, Typeflag: kind}
	if kind == tar.TypeSymlink {
		header.Linkname = "../../etc/passwd"
	}
	if kind == tar.TypeLink {
		header.Linkname = "metadata/target"
	}
	_ = tw.WriteHeader(header)
	if kind == tar.TypeReg {
		_, _ = tw.Write([]byte("x"))
	}
	_ = tw.Close()
	_ = gz.Close()
	file := filepath.Join(dir, "bad.flxbak")
	out, _ := os.Create(file)
	_, _ = out.Write([]byte(magic))
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(hb)))
	_, _ = out.Write(n[:])
	_, _ = out.Write(hb)
	_, _ = out.Write(body.Bytes())
	_ = out.Close()
	return file
}

func TestNameValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid bool
	}{{"flixer-20261024T232900Z.flxbak", true}, {"flixer-2026T23-29-00Z.flxbak", true}, {"../flixer-20261024T232900Z.flxbak", false}, {"flixer-a.flxbak", false}, {"flixer-20261024.dump", false}, {"flixer-20261024T232900Z.flxbak/../x", false}} {
		t.Run(tc.name, func(t *testing.T) {
			if NamePattern.MatchString(tc.name) != tc.valid {
				t.Fatalf("pattern result mismatch")
			}
		})
	}
}

func TestArchivePlainHeader(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "config")
	_ = os.Mkdir(src, 0700)
	_ = os.WriteFile(filepath.Join(src, "key"), []byte("private-key"), 0600)
	file := filepath.Join(dir, "plain.flxbak")
	h, err := WriteArchive(file, []string{"config"}, map[string]string{"config": src}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if h.Encrypted {
		t.Fatal("unexpected encrypted archive")
	}
	raw, _ := os.ReadFile(file)
	if strings.Contains(string(raw), "secret-passphrase") {
		t.Fatal("passphrase leaked into archive")
	}
}

// A Flixer-written archive: trailing-slash directories, a PAX global header, and a manifest whose
// createdAt/versions differ in formatting from the header.
func TestExtractAcceptsFlixerLayout(t *testing.T) {
	dir := t.TempDir()
	comps := []string{"database", "config", "secrets", "metadata"}
	hb, _ := json.Marshal(Header{CreatedAt: "2026-01-01T00:00:00.000Z", Components: comps})
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "x"}})
	manifest, _ := json.Marshal(map[string]any{"version": 1, "createdAt": "2026-01-01T00:00:00Z", "components": comps})
	files := []struct {
		name, data string
		dir        bool
	}{
		{"manifest.json", string(manifest), false},
		{"database.dump", "dump", false},
		{"config/", "", true},
		{"config/flixer-config.db", "sqlite", false},
		{"config/key", "k", false},
		{"secrets/", "", true},
		{"secrets/token", "t", false},
		{"metadata/", "", true},
		{"metadata/sub/", "", true},
		{"metadata/sub/a.jpg", "img", false},
	}
	for _, f := range files {
		h := &tar.Header{Name: f.name, Mode: 0644, Size: int64(len(f.data)), Typeflag: tar.TypeReg}
		if f.dir {
			h.Typeflag, h.Mode, h.Size = tar.TypeDir, 0755, 0
		}
		_ = tw.WriteHeader(h)
		_, _ = tw.Write([]byte(f.data))
	}
	_ = tw.Close()
	_ = gz.Close()
	file := filepath.Join(dir, "flixer.flxbak")
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(hb)))
	raw := append(append(append([]byte(magic), n[:]...), hb...), body.Bytes()...)
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(dir, "stage")
	if _, _, err := Extract(file, stage, ""); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"database.dump", "config/flixer-config.db", "config/key", "secrets/token", "metadata/sub/a.jpg"} {
		if _, err := os.Stat(filepath.Join(stage, p)); err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
	}
}
