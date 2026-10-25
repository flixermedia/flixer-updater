package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/flixermedia/flixer-updater/internal/auth"
	"github.com/flixermedia/flixer-updater/internal/backup"
	"github.com/flixermedia/flixer-updater/internal/config"
	"github.com/flixermedia/flixer-updater/internal/docker"
	"github.com/flixermedia/flixer-updater/internal/fullbackup"
	"github.com/flixermedia/flixer-updater/internal/server"
	"github.com/flixermedia/flixer-updater/internal/updater"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("flixer-updater stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.FromEnv(os.Getenv, version)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.ProjectName == "" || cfg.ProjectDir == "" || cfg.ComposeFile == "" {
		labels, err := docker.DetectProject(ctx, docker.ExecRunner, docker.Hostname())
		if err != nil {
			return errors.New("could not detect the compose project (is /var/run/docker.sock mounted?); set COMPOSE_PROJECT_NAME, COMPOSE_PROJECT_DIR and COMPOSE_FILE: " + err.Error())
		}
		cfg.ProjectName = firstNonEmpty(cfg.ProjectName, labels.Project)
		cfg.ProjectDir = firstNonEmpty(cfg.ProjectDir, labels.WorkingDir)
		cfg.ComposeFile = firstNonEmpty(cfg.ComposeFile, labels.ConfigFile)
	}
	if _, err := os.Stat(cfg.ComposeFile); err != nil {
		return errors.New("the compose file " + cfg.ComposeFile + " is not visible in the container; mount the project folder at the same path it has on the host")
	}

	// The project's own .env is listed explicitly because passing --env-file turns off compose's default of reading it.
	envFiles := []string{}
	if projectEnv := filepath.Join(cfg.ProjectDir, ".env"); fileExists(projectEnv) {
		envFiles = append(envFiles, projectEnv)
	}
	envFiles = append(envFiles, filepath.Join(cfg.StateDir, "versions.env"))

	opts := updater.Options{
		StateDir: cfg.StateDir, HealthTimeout: cfg.HealthTimeout, PollInterval: cfg.PollInterval, RestartServices: cfg.RestartServices, Logger: logger,
		Docker: docker.New(cfg.ProjectName, cfg.ProjectDir, cfg.ComposeFile, envFiles, docker.ExecRunner),
	}
	opts.Backup = backup.Postgres{DatabaseURL: cfg.DatabaseURL, Dir: cfg.BackupDir, Keep: cfg.BackupsKept}
	manager, err := updater.New(opts)
	if err != nil {
		return err
	}

	backupService := &fullbackup.Service{Dir: cfg.BackupDir, PassphraseFile: cfg.PassphraseFile, ConfigDir: cfg.ConfigDir, SecretsDir: cfg.SecretsDir, MetadataDir: cfg.MetadataDir, DatabaseURL: cfg.DatabaseURL, Docker: opts.Docker, HealthWaiter: manager.WaitHealthy, HealthTimeout: cfg.HealthTimeout, PollInterval: cfg.PollInterval, Versions: func() map[string]string { return manager.Snapshot().Versions }}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(auth.Resolver(cfg.Token, cfg.SecretFile), cfg.Version, manager, backupService),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	logger.Info("flixer-updater listening", "addr", cfg.Addr, "version", cfg.Version, "project", cfg.ProjectName, "composeFile", cfg.ComposeFile)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	manager.Wait()
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
