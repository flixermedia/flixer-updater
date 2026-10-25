// Package updater runs updates: back up, pin the new versions, recreate containers one by one, wait for them to be
// healthy, and put the previous versions back if anything fails.
package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Components maps each Flixer component to its compose service and the env variable that pins its image tag.
var Components = map[string]struct{ Service, EnvVar string }{
	"flixer":   {"api", "FLIXER_VERSION"},
	"web":      {"web", "WEB_VERSION"},
	"streamer": {"streamer", "STREAMER_VERSION"},
	"indexer":  {"indexer", "INDEXER_VERSION"},
}

// updateOrder keeps the API last: it is the service that asked for the update, and the dashboard needs it back.
var updateOrder = []string{"streamer", "indexer", "web", "flixer"}

var releasePattern = regexp.MustCompile(`^\d{1,4}\.\d{1,4}\.\d{1,4}$`)

// Status values of a run.
const (
	StatusRunning        = "running"
	StatusSucceeded      = "succeeded"
	StatusFailed         = "failed"
	StatusRolledBack     = "rolled_back"
	StatusRollbackFailed = "rollback_failed"
)

var (
	// ErrBusy is returned when an update is already running.
	ErrBusy = errors.New("an update is already in progress")
	// ErrInvalid wraps problems with the request itself.
	ErrInvalid = errors.New("invalid update request")
)

// Docker is the part of the docker CLI the updater uses.
type Docker interface {
	Compose(ctx context.Context, args ...string) (string, error)
	Health(ctx context.Context, service string) (string, error)
}

// Backuper snapshots the database before an update.
type Backuper interface {
	Backup(ctx context.Context, databaseURL string) (string, error)
}

// Request asks for components to be moved to specific releases.
type Request struct {
	// Targets maps component to the X.Y.Z release to install.
	Targets map[string]string `json:"targets"`
	// Current maps component to the release the caller sees running; used as the rollback target when the updater has
	// not pinned one itself yet.
	Current map[string]string `json:"current"`
	// DatabaseURL is the Postgres connection string to back up. It is used for the backup only and never stored or logged.
	DatabaseURL string `json:"databaseUrl"`
}

// Run is the record of one update.
type Run struct {
	ID         string            `json:"id"`
	Status     string            `json:"status"`
	Kind       string            `json:"kind,omitempty"`
	Encrypted  bool              `json:"encrypted"`
	Step       string            `json:"step"`
	StartedAt  time.Time         `json:"startedAt"`
	FinishedAt *time.Time        `json:"finishedAt"`
	Targets    map[string]string `json:"targets"`
	Previous   map[string]string `json:"previous"`
	Backup     string            `json:"backup"`
	Error      string            `json:"error"`
	Log        []string          `json:"log"`
	Steps      []RunStep         `json:"steps,omitempty"`
}

// RunStep records a named phase of an operation.
type RunStep struct {
	Name       string     `json:"name"`
	Message    string     `json:"message"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

type state struct {
	// Versions are the releases pinned by the updater, per component.
	Versions map[string]string `json:"versions"`
	Run      *Run              `json:"run"`
}

// Options configures an Updater.
type Options struct {
	StateDir        string
	HealthTimeout   time.Duration
	PollInterval    time.Duration
	RestartServices []string
	Docker          Docker
	Backup          Backuper
	Logger          *slog.Logger
}

// Updater serialises updates and remembers what it did.
type Updater struct {
	opts Options
	log  *slog.Logger

	mu    sync.Mutex
	state state
	wg    sync.WaitGroup
}

// New loads the saved state. A run that was still going when the updater last stopped is marked as failed.
func New(opts Options) (*Updater, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := os.MkdirAll(opts.StateDir, 0o755); err != nil {
		return nil, err
	}
	u := &Updater{opts: opts, log: opts.Logger, state: state{Versions: map[string]string{}}}
	if raw, err := os.ReadFile(u.statePath()); err == nil {
		if err := json.Unmarshal(raw, &u.state); err != nil {
			return nil, fmt.Errorf("read %s: %w", u.statePath(), err)
		}
		if u.state.Versions == nil {
			u.state.Versions = map[string]string{}
		}
	}
	if run := u.state.Run; run != nil && run.Status == StatusRunning {
		now := time.Now().UTC()
		run.Status, run.FinishedAt, run.Error = StatusFailed, &now, "The updater stopped while this update was running."
	}
	if err := u.writeVersionsFile(u.state.Versions); err != nil {
		return nil, err
	}
	return u, u.save()
}

// VersionsFile is the env file the compose project must read so pinned tags survive restarts.
func (u *Updater) VersionsFile() string { return filepath.Join(u.opts.StateDir, "versions.env") }

func (u *Updater) statePath() string { return filepath.Join(u.opts.StateDir, "state.json") }

// Snapshot is the externally visible state.
type Snapshot struct {
	Versions map[string]string `json:"versions"`
	Run      *Run              `json:"run"`
}

// Snapshot returns a copy of the current state.
func (u *Updater) Snapshot() Snapshot {
	u.mu.Lock()
	defer u.mu.Unlock()
	snapshot := Snapshot{Versions: map[string]string{}}
	for key, value := range u.state.Versions {
		snapshot.Versions[key] = value
	}
	if u.state.Run != nil {
		run := *u.state.Run
		run.Log = append([]string(nil), run.Log...)
		run.Steps = append([]RunStep(nil), run.Steps...)
		snapshot.Run = &run
	}
	return snapshot
}

// Wait blocks until the current run, if any, has finished.
func (u *Updater) Wait() { u.wg.Wait() }

// Validate checks a request and returns the targets in a safe update order.
func Validate(req Request) ([]string, error) {
	if len(req.Targets) == 0 {
		return nil, fmt.Errorf("%w: no components to update", ErrInvalid)
	}
	var ordered []string
	for _, component := range updateOrder {
		version, wanted := req.Targets[component]
		if !wanted {
			continue
		}
		if !releasePattern.MatchString(version) {
			return nil, fmt.Errorf("%w: %q is not a release version for %s", ErrInvalid, version, component)
		}
		ordered = append(ordered, component)
	}
	if len(ordered) != len(req.Targets) {
		for component := range req.Targets {
			if _, known := Components[component]; !known {
				return nil, fmt.Errorf("%w: unknown component %q", ErrInvalid, component)
			}
		}
	}
	return ordered, nil
}

// Start validates the request and begins an update in the background.
func (u *Updater) Start(req Request) (Run, error) {
	order, err := Validate(req)
	if err != nil {
		return Run{}, err
	}
	u.mu.Lock()
	if u.state.Run != nil && u.state.Run.Status == StatusRunning {
		u.mu.Unlock()
		return Run{}, ErrBusy
	}
	previous := map[string]string{}
	for _, component := range order {
		if pinned := u.state.Versions[component]; pinned != "" {
			previous[component] = pinned
		} else if current := req.Current[component]; releasePattern.MatchString(current) {
			previous[component] = current
		}
	}
	targets := map[string]string{}
	for _, component := range order {
		targets[component] = req.Targets[component]
	}
	run := &Run{ID: newID(), Status: StatusRunning, Kind: "update", Step: "starting", StartedAt: time.Now().UTC(), Targets: targets, Previous: previous}
	u.state.Run = run
	_ = u.saveLocked()
	snapshot := *run
	u.mu.Unlock()

	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		u.execute(order, targets, previous, req.DatabaseURL)
	}()
	return snapshot, nil
}

func (u *Updater) execute(order []string, targets, previous map[string]string, databaseURL string) {
	ctx := context.Background()
	pinnedBefore := u.Snapshot().Versions

	if u.opts.Backup != nil {
		u.step("backup", "Backing up the database")
		name, err := u.opts.Backup.Backup(ctx, databaseURL)
		if err != nil {
			u.finish(StatusFailed, "The database backup failed, so nothing was changed: "+err.Error())
			return
		}
		u.mutate(func(run *Run) { run.Backup = name })
		u.logf("Database backup written: %s", name)
	}

	pinned := copyMap(pinnedBefore)
	for component, version := range targets {
		pinned[component] = version
	}
	if err := u.setVersions(pinned); err != nil {
		u.finish(StatusFailed, "Could not save the new versions: "+err.Error())
		return
	}

	services := serviceNames(order)
	u.step("pull", "Downloading new images")
	if _, err := u.opts.Docker.Compose(ctx, append([]string{"pull"}, services...)...); err != nil {
		_ = u.setVersions(pinnedBefore)
		u.finish(StatusFailed, "Could not download the new images, so nothing was changed: "+err.Error())
		return
	}

	var touched []string
	for _, component := range order {
		service := Components[component].Service
		u.step("update:"+component, fmt.Sprintf("Updating %s to %s", component, targets[component]))
		touched = append(touched, component)
		if _, err := u.opts.Docker.Compose(ctx, "up", "--detach", "--no-deps", service); err != nil {
			u.rollback(ctx, touched, targets, previous, pinnedBefore, fmt.Sprintf("Could not start %s: %v", component, err))
			return
		}
		u.step("verify:"+component, fmt.Sprintf("Waiting for %s to be healthy", component))
		if err := u.waitHealthy(ctx, service); err != nil {
			u.rollback(ctx, touched, targets, previous, pinnedBefore, fmt.Sprintf("%s did not become healthy: %v", component, err))
			return
		}
	}

	u.restartProxies(ctx)
	u.finish(StatusSucceeded, "")
}

// Progress reports steps and logs for a non-update operation.
type Progress struct{ updater *Updater }

func (p Progress) Step(name, message string)       { p.updater.step(name, message) }
func (p Progress) Logf(format string, args ...any) { p.updater.logf(format, args...) }
func (p Progress) SetBackup(name string)           { p.updater.mutate(func(run *Run) { run.Backup = name }) }
func (p Progress) SetEncrypted(encrypted bool) {
	p.updater.mutate(func(run *Run) { run.Encrypted = encrypted })
}
func (p Progress) FailStep() {
	p.updater.mutate(func(run *Run) {
		if n := len(run.Steps); n > 0 && run.Steps[n-1].Status == "running" {
			now := time.Now().UTC()
			run.Steps[n-1].Status = "failed"
			run.Steps[n-1].FinishedAt = &now
		}
	})
}

// StartOperation runs a backup or restore under the same single-operation lock as updates.
func (u *Updater) StartOperation(kind, backupName string, encrypted bool, operation func(Progress) error) (Run, error) {
	if kind != "backup" && kind != "restore" {
		return Run{}, fmt.Errorf("%w: unsupported operation", ErrInvalid)
	}
	u.mu.Lock()
	if u.state.Run != nil && u.state.Run.Status == StatusRunning {
		u.mu.Unlock()
		return Run{}, ErrBusy
	}
	run := &Run{ID: newID(), Status: StatusRunning, Kind: kind, Encrypted: encrypted, Step: "starting", StartedAt: time.Now().UTC(), Backup: backupName}
	u.state.Run = run
	_ = u.saveLocked()
	snapshot := *run
	u.mu.Unlock()
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		err := operation(Progress{updater: u})
		if err != nil {
			u.finish(StatusFailed, err.Error())
		} else {
			u.finish(StatusSucceeded, "")
		}
	}()
	return snapshot, nil
}

// rollback pins the previous releases again and recreates every component that was already touched.
func (u *Updater) rollback(ctx context.Context, touched []string, targets, previous, pinnedBefore map[string]string, reason string) {
	u.logf("%s", reason)
	u.step("rollback", "Rolling back to the previous versions")

	restore := copyMap(pinnedBefore)
	var revert, stuck []string
	for _, component := range touched {
		if version := previous[component]; version != "" {
			restore[component] = version
			revert = append(revert, component)
		} else {
			stuck = append(stuck, component)
		}
	}
	if err := u.setVersions(restore); err != nil {
		u.finish(StatusRollbackFailed, reason+" Rolling back failed as well: "+err.Error())
		return
	}
	for _, component := range revert {
		service := Components[component].Service
		u.logf("Restoring %s to %s", component, previous[component])
		if _, err := u.opts.Docker.Compose(ctx, "up", "--detach", "--no-deps", service); err != nil {
			u.finish(StatusRollbackFailed, fmt.Sprintf("%s Restoring %s failed: %v", reason, component, err))
			return
		}
		if err := u.waitHealthy(ctx, service); err != nil {
			u.finish(StatusRollbackFailed, fmt.Sprintf("%s %s did not become healthy after restoring it: %v", reason, component, err))
			return
		}
	}
	u.restartProxies(ctx)
	message := reason + " The previous versions were restored."
	if len(stuck) > 0 {
		sort.Strings(stuck)
		message += fmt.Sprintf(" %s could not be restored because its previous release is unknown.", strings.Join(stuck, ", "))
		u.finish(StatusRollbackFailed, message)
		return
	}
	u.finish(StatusRolledBack, message)
}

func (u *Updater) restartProxies(ctx context.Context) {
	if len(u.opts.RestartServices) == 0 {
		return
	}
	u.step("proxy", "Reconnecting the reverse proxy")
	if _, err := u.opts.Docker.Compose(ctx, append([]string{"restart"}, u.opts.RestartServices...)...); err != nil {
		u.logf("Restarting %s failed: %v", strings.Join(u.opts.RestartServices, ", "), err)
	}
}

// WaitHealthy waits for a compose service using the updater's standard health policy.
func (u *Updater) WaitHealthy(ctx context.Context, service string) error {
	return u.waitHealthy(ctx, service)
}

func (u *Updater) waitHealthy(ctx context.Context, service string) error {
	deadline := time.Now().Add(u.opts.HealthTimeout)
	stable := 0
	for {
		state, err := u.opts.Docker.Health(ctx, service)
		switch {
		case err == nil && state == "healthy":
			return nil
		case err == nil && state == "running":
			// A container without a health check: accept it once it has stayed up for a few polls.
			if stable++; stable >= 3 {
				return nil
			}
		case err == nil && (state == "unhealthy" || state == "exited" || state == "dead" || state == "missing"):
			return fmt.Errorf("container is %s", state)
		default:
			stable = 0
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still %q after %s", state, u.opts.HealthTimeout)
		}
		time.Sleep(u.opts.PollInterval)
	}
}

func (u *Updater) step(step, message string) {
	u.mutate(func(run *Run) {
		now := time.Now().UTC()
		if n := len(run.Steps); n > 0 && run.Steps[n-1].Status == "running" {
			run.Steps[n-1].Status = "succeeded"
			run.Steps[n-1].FinishedAt = &now
		}
		run.Step = step
		run.Steps = append(run.Steps, RunStep{Name: step, Message: message, Status: "running", StartedAt: now})
	})
	u.logf("%s", message)
}

func (u *Updater) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	u.log.Info(line)
	u.mutate(func(run *Run) {
		run.Log = append(run.Log, time.Now().UTC().Format("15:04:05")+" "+line)
		if len(run.Log) > 100 {
			run.Log = run.Log[len(run.Log)-100:]
		}
	})
}

func (u *Updater) finish(status, message string) {
	u.mutate(func(run *Run) {
		now := time.Now().UTC()
		run.Status, run.FinishedAt, run.Error, run.Step = status, &now, message, "done"
		if n := len(run.Steps); n > 0 && run.Steps[n-1].Status == "running" {
			run.Steps[n-1].Status = "succeeded"
			if message != "" {
				run.Steps[n-1].Status = "failed"
			}
			run.Steps[n-1].FinishedAt = &now
		}
	})
	if message != "" {
		u.log.Error("operation finished", "status", status, "error", message)
	}
}

func (u *Updater) mutate(change func(*Run)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.state.Run != nil {
		change(u.state.Run)
		_ = u.saveLocked()
	}
}

func (u *Updater) setVersions(versions map[string]string) error {
	if err := u.writeVersionsFile(versions); err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.state.Versions = copyMap(versions)
	return u.saveLocked()
}

func (u *Updater) writeVersionsFile(versions map[string]string) error {
	var lines []string
	for component, info := range Components {
		if version := versions[component]; releasePattern.MatchString(version) {
			lines = append(lines, info.EnvVar+"="+version)
		}
	}
	sort.Strings(lines)
	return writeAtomic(u.VersionsFile(), []byte(strings.Join(lines, "\n")+"\n"))
}

func (u *Updater) save() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.saveLocked()
}

func (u *Updater) saveLocked() error {
	raw, err := json.MarshalIndent(u.state, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(u.statePath(), raw)
}

func writeAtomic(path string, data []byte) error {
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

func serviceNames(components []string) []string {
	services := make([]string, 0, len(components))
	for _, component := range components {
		services = append(services, Components[component].Service)
	}
	return services
}

func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func newID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
