// Package config reads the updater's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Config is everything the updater needs to run.
type Config struct {
	Version string
	Addr    string
	Token   string

	// ComposeFile, ProjectDir and ProjectName identify the stack to update. They are detected from the updater's own
	// container labels when left empty.
	ComposeFile string
	ProjectDir  string
	ProjectName string

	StateDir    string
	BackupDir   string
	DatabaseURL string
	BackupsKept int

	HealthTimeout time.Duration
	PollInterval  time.Duration
	// RestartServices are restarted after an update because nginx resolves its upstreams only at start.
	RestartServices []string
}

// FromEnv builds a Config from an environment lookup function.
func FromEnv(getenv func(string) string, version string) (Config, error) {
	cfg := Config{
		Version:         version,
		Addr:            ":" + valueOr(getenv("PORT"), "8093"),
		Token:           strings.TrimSpace(getenv("UPDATER_TOKEN")),
		ComposeFile:     strings.TrimSpace(getenv("COMPOSE_FILE")),
		ProjectDir:      strings.TrimSpace(getenv("COMPOSE_PROJECT_DIR")),
		ProjectName:     strings.TrimSpace(getenv("COMPOSE_PROJECT_NAME")),
		StateDir:        valueOr(getenv("STATE_DIR"), "/state"),
		BackupDir:       valueOr(getenv("BACKUP_DIR"), "/backups"),
		DatabaseURL:     strings.TrimSpace(getenv("DATABASE_URL")),
		BackupsKept:     5,
		HealthTimeout:   3 * time.Minute,
		PollInterval:    3 * time.Second,
		RestartServices: splitList(valueOr(getenv("RESTART_SERVICES"), "proxy")),
	}
	if len(cfg.Token) < 16 {
		return Config{}, errors.New("UPDATER_TOKEN must be set to a random string of at least 16 characters")
	}
	if raw := strings.TrimSpace(getenv("HEALTH_TIMEOUT")); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil || timeout < 10*time.Second {
			return Config{}, fmt.Errorf("HEALTH_TIMEOUT must be a duration of at least 10s, got %q", raw)
		}
		cfg.HealthTimeout = timeout
	}
	return cfg, nil
}

func valueOr(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func splitList(value string) []string {
	var items []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" && item != "-" {
			items = append(items, item)
		}
	}
	return items
}
