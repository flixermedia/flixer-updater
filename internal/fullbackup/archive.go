package fullbackup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	magic       = "FLXBAK1\n"
	headerLimit = 1 << 20
	chunkSize   = 64 << 10
	iterations  = 600000
)

var (
	ErrBadPassphrase = errors.New("backup passphrase is incorrect")
	ErrCorrupt       = errors.New("backup is corrupt or unsupported")
)

type Header struct {
	CreatedAt  string            `json:"createdAt"`
	Components []string          `json:"components"`
	Encrypted  bool              `json:"encrypted"`
	Versions   map[string]string `json:"versions"`
	KDF        *KDF              `json:"kdf,omitempty"`
}
type KDF struct {
	Name       string `json:"name"`
	Iterations int    `json:"iterations"`
	Salt       string `json:"salt"`
	Prefix     string `json:"noncePrefix"`
	Check      string `json:"check,omitempty"`
}
type Manifest struct {
	Version       int               `json:"version"`
	CreatedAt     string            `json:"createdAt"`
	Components    []string          `json:"components"`
	Versions      map[string]string `json:"versions"`
	DatabaseBytes int64             `json:"databaseBytes"`
	MetadataFiles int               `json:"metadataFiles"`
}

var knownComponents = map[string]bool{"database": true, "config": true, "secrets": true, "metadata": true}

func normalizeComponents(in []string) ([]string, error) {
	if len(in) == 0 {
		in = []string{"database", "config", "secrets", "metadata"}
	}
	seen := map[string]bool{}
	for _, c := range in {
		if !knownComponents[c] {
			return nil, fmt.Errorf("%w: unknown backup component %q", ErrInvalid, c)
		}
		if seen[c] {
			return nil, fmt.Errorf("%w: duplicate backup component %q", ErrInvalid, c)
		}
		seen[c] = true
	}
	out := make([]string, 0, len(seen))
	for _, c := range []string{"database", "config", "secrets", "metadata"} {
		if seen[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

func readHeader(r io.Reader) (Header, []byte, error) {
	magicBytes := make([]byte, len(magic))
	if _, err := io.ReadFull(r, magicBytes); err != nil || string(magicBytes) != magic {
		return Header{}, nil, ErrCorrupt
	}
	var length [4]byte
	if _, err := io.ReadFull(r, length[:]); err != nil {
		return Header{}, nil, ErrCorrupt
	}
	n := binary.BigEndian.Uint32(length[:])
	if n == 0 || n > headerLimit {
		return Header{}, nil, ErrCorrupt
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(r, raw); err != nil {
		return Header{}, nil, ErrCorrupt
	}
	var h Header
	if json.Unmarshal(raw, &h) != nil {
		return Header{}, nil, ErrCorrupt
	}
	components, err := normalizeComponents(h.Components)
	if err != nil || len(components) != len(h.Components) {
		return Header{}, nil, ErrCorrupt
	}
	h.Components = components
	if _, err := time.Parse(time.RFC3339Nano, h.CreatedAt); err != nil {
		return Header{}, nil, ErrCorrupt
	}
	if h.Versions == nil {
		h.Versions = map[string]string{}
	}
	if h.Encrypted {
		if h.KDF == nil || h.KDF.Name != "pbkdf2-sha256" || h.KDF.Iterations != iterations {
			return Header{}, nil, ErrCorrupt
		}
		salt, err := base64.StdEncoding.DecodeString(h.KDF.Salt)
		if err != nil || len(salt) != 16 {
			return Header{}, nil, ErrCorrupt
		}
		prefix, err := base64.StdEncoding.DecodeString(h.KDF.Prefix)
		if err != nil || len(prefix) != 4 {
			return Header{}, nil, ErrCorrupt
		}
		if h.KDF.Check != "" {
			check, err := base64.StdEncoding.DecodeString(h.KDF.Check)
			if err != nil || len(check) != sha256.Size {
				return Header{}, nil, ErrCorrupt
			}
		}
	}
	return h, raw, nil
}

func ReadHeaderFile(name string) (Header, int64, error) {
	f, err := os.Open(name)
	if err != nil {
		return Header{}, 0, err
	}
	defer f.Close()
	h, _, err := readHeader(f)
	if err != nil {
		return Header{}, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		return Header{}, 0, err
	}
	return h, st.Size(), nil
}

// WriteArchive streams tar.gz through an optional authenticated chunked AES-GCM envelope.
func WriteArchive(destination string, components []string, sources map[string]string, versions map[string]string, passphrase string) (Header, error) {
	components, err := normalizeComponents(components)
	if err != nil {
		return Header{}, err
	}
	created := time.Now().UTC().Format(time.RFC3339Nano)
	header := Header{CreatedAt: created, Components: components, Encrypted: passphrase != "", Versions: cloneMap(versions)}
	var key, prefix []byte
	if header.Encrypted {
		salt := make([]byte, 16)
		prefix = make([]byte, 4)
		if _, err = rand.Read(salt); err != nil {
			return Header{}, err
		}
		if _, err = rand.Read(prefix); err != nil {
			return Header{}, err
		}
		key, err = pbkdf2.Key(sha256.New, passphrase, salt, iterations, 32)
		if err != nil {
			return Header{}, err
		}
		header.KDF = &KDF{Name: "pbkdf2-sha256", Iterations: iterations, Salt: base64.StdEncoding.EncodeToString(salt), Prefix: base64.StdEncoding.EncodeToString(prefix)}
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte("flixer-backup-passphrase-check"))
		header.KDF.Check = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	headerBytes, err := json.Marshal(header)
	if err != nil {
		return Header{}, err
	}
	if len(headerBytes) > headerLimit {
		return Header{}, fmt.Errorf("backup header is too large")
	}
	if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return Header{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".flixer-backup-*.tmp")
	if err != nil {
		return Header{}, err
	}
	defer tmp.Close()
	temp := tmp.Name()
	defer os.Remove(temp)
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return Header{}, err
	}
	if _, err = tmp.Write([]byte(magic)); err != nil {
		tmp.Close()
		return Header{}, err
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(headerBytes)))
	if _, err = tmp.Write(n[:]); err != nil {
		tmp.Close()
		return Header{}, err
	}
	if _, err = tmp.Write(headerBytes); err != nil {
		tmp.Close()
		return Header{}, err
	}
	pr, pw := io.Pipe()
	archiveErr := make(chan error, 1)
	go func() {
		gz := gzip.NewWriter(pw)
		tw := tar.NewWriter(gz)
		manifest := Manifest{Version: 1, CreatedAt: created, Components: components, Versions: cloneMap(versions)}
		for _, c := range components {
			if c == "database" {
				if fi, e := os.Stat(sources[c]); e == nil {
					manifest.DatabaseBytes = fi.Size()
				}
			}
			if c == "metadata" {
				manifest.MetadataFiles = countFiles(sources[c])
			}
		}
		mb, _ := json.Marshal(manifest)
		e := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(mb)), ModTime: time.Now().UTC()})
		if e == nil {
			_, e = tw.Write(mb)
		}
		for _, component := range components {
			if e != nil {
				break
			}
			if component == "database" {
				e = addFile(tw, sources[component], "database.dump")
				continue
			}
			e = addDirectory(tw, sources[component], component)
		}
		if closeErr := tw.Close(); e == nil {
			e = closeErr
		}
		if closeErr := gz.Close(); e == nil {
			e = closeErr
		}
		_ = pw.CloseWithError(e)
		archiveErr <- e
	}()
	var writeErr error
	if header.Encrypted {
		writeErr = encryptChunks(tmp, pr, key, prefix, headerBytes)
	} else {
		_, writeErr = io.Copy(tmp, pr)
	}
	_ = pr.Close()
	buildErr := <-archiveErr
	if writeErr == nil {
		writeErr = buildErr
	}
	if writeErr == nil {
		writeErr = tmp.Sync()
	}
	closeErr := tmp.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return Header{}, writeErr
	}
	if err = os.Rename(temp, destination); err != nil {
		return Header{}, err
	}
	return header, nil
}

func encryptChunks(dst io.Writer, src io.Reader, key, prefix, aad []byte) error {
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	buf := make([]byte, chunkSize)
	var counter uint64
	for {
		n, e := io.ReadFull(src, buf)
		if e != nil && e != io.ErrUnexpectedEOF && e != io.EOF {
			return e
		}
		final := e == io.EOF || e == io.ErrUnexpectedEOF
		if n == 0 && !final {
			continue
		}
		nonce := make([]byte, gcm.NonceSize())
		copy(nonce, prefix)
		binary.BigEndian.PutUint64(nonce[4:], counter)
		counter++
		sealed := gcm.Seal(nil, nonce, buf[:n], aad)
		flag := byte(0)
		if final {
			flag = 1
		}
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(sealed)))
		if _, err = dst.Write([]byte{flag}); err != nil {
			return err
		}
		if _, err = dst.Write(size[:]); err != nil {
			return err
		}
		if _, err = dst.Write(sealed); err != nil {
			return err
		}
		if final {
			return nil
		}
	}
}

func decryptChunks(dst io.Writer, src io.Reader, h Header, headerBytes []byte, passphrase string) error {
	salt, _ := base64.StdEncoding.DecodeString(h.KDF.Salt)
	prefix, _ := base64.StdEncoding.DecodeString(h.KDF.Prefix)
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, iterations, 32)
	if err != nil {
		return err
	}
	// The check value is optional; without it a failed first chunk is reported as a wrong passphrase.
	if h.KDF.Check != "" {
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte("flixer-backup-passphrase-check"))
		expected, _ := base64.StdEncoding.DecodeString(h.KDF.Check)
		if !hmac.Equal(mac.Sum(nil), expected) {
			return ErrBadPassphrase
		}
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	var counter uint64
	for {
		var flag [1]byte
		if _, err = io.ReadFull(src, flag[:]); err != nil {
			return ErrCorrupt
		}
		var size [4]byte
		if _, err = io.ReadFull(src, size[:]); err != nil {
			return ErrCorrupt
		}
		n := binary.BigEndian.Uint32(size[:])
		if n < uint32(gcm.Overhead()) || n > chunkSize+uint32(gcm.Overhead()) {
			return ErrCorrupt
		}
		ciphertext := make([]byte, n)
		if _, err = io.ReadFull(src, ciphertext); err != nil {
			return ErrCorrupt
		}
		nonce := make([]byte, gcm.NonceSize())
		copy(nonce, prefix)
		binary.BigEndian.PutUint64(nonce[4:], counter)
		counter++
		plain, e := gcm.Open(nil, nonce, ciphertext, headerBytes)
		if e != nil {
			if counter == 1 && h.KDF.Check == "" {
				return ErrBadPassphrase
			}
			return ErrCorrupt
		}
		if _, e = dst.Write(plain); e != nil {
			return e
		}
		if flag[0] == 1 {
			var extra [1]byte
			if n, e := io.ReadFull(src, extra[:]); n != 0 || e != io.EOF {
				return ErrCorrupt
			}
			return nil
		}
		if flag[0] != 0 {
			return ErrCorrupt
		}
	}
}

// Extract validates the archive and safely writes its contents beneath destination.
func Extract(source, destination, passphrase string) (Header, Manifest, error) {
	f, err := os.Open(source)
	if err != nil {
		return Header{}, Manifest{}, err
	}
	defer f.Close()
	h, headerBytes, err := readHeader(f)
	if err != nil {
		return Header{}, Manifest{}, err
	}
	pr, pw := io.Pipe()
	decryptErr := make(chan error, 1)
	go func() {
		var e error
		if h.Encrypted {
			e = decryptChunks(pw, f, h, headerBytes, passphrase)
		} else {
			_, e = io.Copy(pw, f)
		}
		_ = pw.CloseWithError(e)
		decryptErr <- e
	}()
	gz, e := gzip.NewReader(pr)
	if e != nil {
		_ = pr.Close()
		de := <-decryptErr
		if de == ErrBadPassphrase {
			return Header{}, Manifest{}, de
		}
		return Header{}, Manifest{}, ErrCorrupt
	}
	tr := tar.NewReader(gz)
	if err = os.MkdirAll(destination, 0700); err != nil {
		_ = gz.Close()
		_ = pr.Close()
		<-decryptErr
		return Header{}, Manifest{}, err
	}
	allowed := map[string]bool{}
	for _, c := range h.Components {
		allowed[c] = true
	}
	var manifest Manifest
	seenManifest := false
	for {
		th, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			err = ErrCorrupt
			break
		}
		if th.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := th.Name
		if th.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		clean, e := safeName(name)
		if e != nil {
			err = ErrCorrupt
			break
		}
		if th.Typeflag != tar.TypeReg && th.Typeflag != tar.TypeRegA && th.Typeflag != tar.TypeDir {
			err = ErrCorrupt
			break
		}
		if clean == "manifest.json" {
			if seenManifest || th.Size > 1<<20 {
				err = ErrCorrupt
				break
			}
			data, e := io.ReadAll(io.LimitReader(tr, 1<<20+1))
			if e != nil {
				err = ErrCorrupt
				break
			}
			if json.Unmarshal(data, &manifest) != nil {
				err = ErrCorrupt
				break
			}
			seenManifest = true
			continue
		}
		parts := strings.Split(clean, "/")
		component := parts[0]
		if component == "database.dump" {
			component = "database"
		} else if len(parts) < 2 && th.Typeflag != tar.TypeDir {
			err = ErrCorrupt
			break
		}
		if !allowed[component] {
			err = ErrCorrupt
			break
		}
		target := filepath.Join(destination, filepath.FromSlash(clean))
		if !within(destination, target) {
			err = ErrCorrupt
			break
		}
		if th.Typeflag == tar.TypeDir {
			if e = os.MkdirAll(target, 0700); e != nil {
				err = e
				break
			}
			continue
		}
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			err = e
			break
		}
		mode := os.FileMode(th.Mode) & 0600
		if mode == 0 {
			mode = 0600
		}
		out, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if e != nil {
			err = ErrCorrupt
			break
		}
		_, copyErr := io.CopyN(out, tr, th.Size)
		closeErr := out.Close()
		if copyErr != nil || closeErr != nil {
			err = ErrCorrupt
			break
		}
	}
	_ = gz.Close()
	_ = pr.Close()
	decErr := <-decryptErr
	if err == nil && decErr != nil {
		if decErr == ErrBadPassphrase {
			err = decErr
		} else {
			err = ErrCorrupt
		}
	}
	if err != nil {
		return Header{}, Manifest{}, err
	}
	components, e := normalizeComponents(manifest.Components)
	if !seenManifest || e != nil || manifest.Version != 1 || !equalStrings(components, h.Components) {
		return Header{}, Manifest{}, ErrCorrupt
	}
	for _, c := range h.Components {
		if c == "database" {
			if _, e = os.Stat(filepath.Join(destination, "database.dump")); e != nil {
				return Header{}, Manifest{}, ErrCorrupt
			}
		}
	}
	return h, manifest, nil
}

func safeName(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) || (len(name) >= 2 && name[1] == ':') {
		return "", ErrCorrupt
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != name {
		return "", ErrCorrupt
	}
	return clean, nil
}
func within(root, target string) bool {
	r, e := filepath.Abs(root)
	if e != nil {
		return false
	}
	t, e := filepath.Abs(target)
	if e != nil {
		return false
	}
	rel, e := filepath.Rel(r, t)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func addFile(tw *tar.Writer, filename, name string) error {
	f, e := os.Open(filename)
	if e != nil {
		return e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return e
	}
	return addStream(tw, f, name, st.Size(), st.Mode())
}
func addStream(tw *tar.Writer, r io.Reader, name string, size int64, mode os.FileMode) error {
	if _, e := safeName(name); e != nil {
		return e
	}
	m := int64(mode.Perm())
	if mode.Perm()&0600 == 0 {
		m = 0600
	}
	if e := tw.WriteHeader(&tar.Header{Name: name, Mode: m, Size: size, Typeflag: tar.TypeReg, ModTime: time.Now().UTC()}); e != nil {
		return e
	}
	_, e := io.CopyN(tw, r, size)
	return e
}
func addDirectory(tw *tar.Writer, root, prefix string) error {
	if root == "" {
		return fmt.Errorf("source directory unavailable")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source directory unavailable")
	}
	return filepath.Walk(root, func(filename string, info os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		rel, e := filepath.Rel(root, filename)
		if e != nil {
			return e
		}
		if rel == "." {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("unsupported file type in %s", prefix)
		}
		name := path.Join(prefix, filepath.ToSlash(rel))
		if info.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: name, Mode: 0700, Typeflag: tar.TypeDir, ModTime: time.Now().UTC()})
		}
		return addFile(tw, filename, name)
	})
}
func countFiles(root string) int {
	n := 0
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, e error) error {
		if e == nil && info.Mode().IsRegular() {
			n++
		}
		return nil
	})
	return n
}
func cloneMap(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	return out
}
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
