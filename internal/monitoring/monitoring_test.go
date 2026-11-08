package monitoring

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

func TestFetchConfiguration(t *testing.T) {
	for _, test := range []struct {
		body  string
		valid bool
	}{
		{`{"enabled":false,"dsn":null}`, true},
		{`{"enabled":true,"dsn":"https://public@example.org/1"}`, true},
		{`{"enabled":true,"dsn":null}`, false},
		{`{}`, false},
		{`not json`, false},
	} {
		t.Run(test.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			_, err := fetch(context.Background(), server.Client(), server.URL)
			if (err == nil) != test.valid {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadRetriesUntilDisabledConfigurationAvailable(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sentry/config/updater" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if calls.Add(1) == 1 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"enabled":false,"dsn":null}`)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	Load(ctx, server.URL, "", "test", slog.New(slog.NewTextHandler(io.Discard, nil)), server.Client(), time.Millisecond)
	if calls.Load() < 2 {
		t.Fatal("configuration was not retried")
	}
}

func TestSanitizeRemovesSensitiveDetails(t *testing.T) {
	event := &sentry.Event{
		Message:     "secret message",
		Request:     &sentry.Request{URL: "https://example.org/private"},
		Contexts:    map[string]sentry.Context{"secret": {"value": "private"}},
		Breadcrumbs: []*sentry.Breadcrumb{{Message: "private"}},
		Exception:   []sentry.Exception{{Value: "sensitive"}},
	}
	clean := sanitizeEvent(event, nil)
	if clean.Message != "Flixer updater internal error" || clean.Request != nil || len(clean.Contexts) != 0 || len(clean.Breadcrumbs) != 0 || len(clean.Exception) != 0 {
		t.Fatalf("event not sanitized: %+v", clean)
	}
}

func TestHandlerKeepsOriginalLog(t *testing.T) {
	var output strings.Builder
	log := slog.New(WrapHandler(slog.NewTextHandler(&output, nil))).With("service", "updater")
	log.Error("operation failed")
	if !strings.Contains(output.String(), "operation failed") || !strings.Contains(output.String(), "service=updater") {
		t.Fatalf("original log was lost: %s", output.String())
	}
}

type memoryTransport struct{ events []*sentry.Event }

func (*memoryTransport) Configure(sentry.ClientOptions)        {}
func (m *memoryTransport) SendEvent(event *sentry.Event)       { m.events = append(m.events, event) }
func (*memoryTransport) Flush(time.Duration) bool              { return true }
func (*memoryTransport) FlushWithContext(context.Context) bool { return true }
func (*memoryTransport) Close()                                {}

func TestErrorLogsAndPanicsReportSanitizedEvents(t *testing.T) {
	transport := &memoryTransport{}
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn: "https://public@example.org/1", Transport: transport, BeforeSend: sanitizeEvent,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := sentry.CurrentHub()
	previous := hub.Client()
	hub.BindClient(client)
	defer hub.BindClient(previous)

	log := slog.New(WrapHandler(slog.NewTextHandler(io.Discard, nil))).WithGroup("test")
	log.Info("not an error")
	log.Error("sensitive log message")
	func() {
		defer func() {
			if recover() == nil {
				t.Error("middleware swallowed panic")
			}
		}()
		Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("sensitive panic")
		})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/private", nil))
	}()
	if len(transport.events) != 2 {
		t.Fatalf("got %d events, want 2", len(transport.events))
	}
	for _, event := range transport.events {
		if event.Message != "Flixer updater internal error" || event.Request != nil || len(event.Exception) != 0 {
			t.Fatalf("event not sanitized: %+v", event)
		}
	}
}
