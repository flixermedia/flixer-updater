// Package backup writes pg_dump snapshots of the Flixer database.
package backup

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var namePattern = regexp.MustCompile(`^flixer-.*\.dump$`)

// Postgres dumps a database and keeps only the newest snapshots.
type Postgres struct {
	DatabaseURL string
	Dir         string
	Keep        int
}

// Backup writes a custom-format dump and returns its file name.
// databaseURL comes from the update request; the configured DatabaseURL is only a fallback.
func (p Postgres) Backup(ctx context.Context, databaseURL string) (string, error) {
	if databaseURL == "" {
		databaseURL = p.DatabaseURL
	}
	if databaseURL == "" {
		return "", errors.New("no database URL was provided")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		return "", fmt.Errorf("invalid database URL: %w", err)
	}
	if err := os.MkdirAll(p.Dir, 0o755); err != nil {
		return "", err
	}
	name := "flixer-" + strings.NewReplacer(":", "-", ".", "-").Replace(time.Now().UTC().Format("2006-01-02T15-04-05.000Z")) + ".dump"
	file := filepath.Join(p.Dir, name)
	password, _ := parsed.User.Password()
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	cmd := exec.CommandContext(ctx, "pg_dump", "--format=custom", "--no-owner", "--file", file)
	cmd.Env = append(os.Environ(),
		"PGHOST="+parsed.Hostname(), "PGPORT="+port, "PGUSER="+parsed.User.Username(),
		"PGPASSWORD="+password, "PGDATABASE="+strings.TrimPrefix(parsed.Path, "/"),
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(file)
		message := strings.TrimSpace(string(output))
		if len(message) > 300 {
			message = message[:300]
		}
		return "", fmt.Errorf("pg_dump: %w: %s", err, message)
	}
	p.prune()
	return name, nil
}

func (p Postgres) prune() {
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		return
	}
	var names []string
	for _, entry := range entries {
		if namePattern.MatchString(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for len(names) > max(p.Keep, 1) {
		_ = os.Remove(filepath.Join(p.Dir, names[0]))
		names = names[1:]
	}
}
