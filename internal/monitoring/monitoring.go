// Package monitoring reports updater failures without sending sensitive operation details.
package monitoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
)

type configuration struct {
	Enabled *bool   `json:"enabled"`
	DSN     *string `json:"dsn"`
}

// Load retries until the API is available, without blocking updater startup.
func Load(ctx context.Context, baseURL, environment, release string, log *slog.Logger, client *http.Client, interval time.Duration) {
	endpoint := strings.TrimRight(baseURL, "/") + "/api/sentry/config/updater"
	for {
		cfg, err := fetch(ctx, client, endpoint)
		if err == nil {
			if !*cfg.Enabled {
				log.Info("Sentry monitoring disabled")
				return
			}
			if environment == "" {
				environment = "production"
			}
			if err := sentry.Init(sentry.ClientOptions{
				Dsn: strings.TrimSpace(*cfg.DSN), Environment: environment, Release: release,
				BeforeSend: sanitizeEvent, AttachStacktrace: false,
			}); err != nil {
				log.Error("Could not initialize Sentry; error reporting is disabled")
			} else {
				log.Info("Sentry monitoring enabled")
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("Could not load Sentry configuration from Flixer; retrying with reporting disabled")
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func fetch(ctx context.Context, client *http.Client, endpoint string) (configuration, error) {
	var cfg configuration
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return cfg, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return cfg, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return cfg, fmt.Errorf("Flixer returned status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&cfg); err != nil {
		return cfg, err
	}
	if cfg.Enabled == nil || (*cfg.Enabled && (cfg.DSN == nil || strings.TrimSpace(*cfg.DSN) == "")) {
		return cfg, errors.New("invalid Sentry configuration")
	}
	return cfg, nil
}

func sanitizeEvent(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	return &sentry.Event{
		EventID: event.EventID, Timestamp: event.Timestamp, Platform: event.Platform,
		Level: sentry.LevelError, Environment: event.Environment, Release: event.Release,
		Message: "Flixer updater internal error",
	}
}

// Flush waits briefly for pending events.
func Flush() { sentry.Flush(2 * time.Second) }

// Recover reports a panic without exposing its value and preserves crash behavior.
func Recover() {
	if recovered := recover(); recovered != nil {
		sentry.CaptureMessage("Flixer updater internal error")
		Flush()
		panic(recovered)
	}
}

// Middleware reports request panics and leaves normal HTTP recovery intact.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				sentry.CaptureMessage("Flixer updater internal error")
				panic(recovered)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// WrapHandler preserves the existing handler and reports error-level records.
func WrapHandler(h slog.Handler) slog.Handler { return &handler{Handler: h} }

type handler struct{ slog.Handler }

func (h *handler) Handle(ctx context.Context, record slog.Record) error {
	if record.Level >= slog.LevelError {
		sentry.CaptureMessage("Flixer updater internal error")
	}
	return h.Handler.Handle(ctx, record)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{Handler: h.Handler.WithGroup(name)}
}
