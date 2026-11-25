// Package docker drives the docker CLI. Arguments are always passed as an argv list, never through a shell.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Runner executes a command and returns its combined output.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner runs real processes.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if len(text) > 800 {
			text = "…" + text[len(text)-800:]
		}
		return text, fmt.Errorf("%s %s: %w: %s", name, firstArgs(args), err, text)
	}
	return text, nil
}

func firstArgs(args []string) string {
	for i, arg := range args {
		if !strings.HasPrefix(arg, "-") && i > 0 && !strings.HasPrefix(args[i-1], "-") {
			return arg
		}
	}
	return ""
}

// CLI runs docker compose commands against one project.
type CLI struct {
	base []string
	run  Runner
}

// New returns a CLI for the compose project. envFiles are read in order, later files winning.
func New(project, projectDir, composeFile string, envFiles []string, run Runner) *CLI {
	base := []string{"compose", "--project-name", project, "--project-directory", projectDir, "--file", composeFile}
	for _, file := range envFiles {
		base = append(base, "--env-file", file)
	}
	return &CLI{base: base, run: run}
}

// Compose runs `docker compose <args>` for the project.
func (c *CLI) Compose(ctx context.Context, args ...string) (string, error) {
	return c.run(ctx, "docker", append(append([]string{}, c.base...), args...)...)
}

// Health reports a service's container state: healthy, starting, unhealthy, running (no health check), exited, ...
func (c *CLI) Health(ctx context.Context, service string) (string, error) {
	id, err := c.Compose(ctx, "ps", "--all", "--quiet", service)
	if err != nil {
		return "", err
	}
	id = strings.TrimSpace(strings.SplitN(id, "\n", 2)[0])
	if id == "" {
		return "missing", nil
	}
	state, err := c.run(ctx, "docker", "inspect", "--format", "{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}", id)
	return strings.TrimSpace(state), err
}

// ProjectLabels are what compose records on every container it creates.
type ProjectLabels struct {
	Project    string
	WorkingDir string
	ConfigFile string
}

// DetectProject reads the compose labels of a container, normally the updater itself.
func DetectProject(ctx context.Context, run Runner, container string) (ProjectLabels, error) {
	output, err := run(ctx, "docker", "inspect", "--format", "{{json .Config.Labels}}", container)
	if err != nil {
		return ProjectLabels{}, err
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(output), &labels); err != nil {
		return ProjectLabels{}, fmt.Errorf("read container labels: %w", err)
	}
	found := ProjectLabels{
		Project:    labels["com.docker.compose.project"],
		WorkingDir: labels["com.docker.compose.project.working_dir"],
		ConfigFile: strings.Split(labels["com.docker.compose.project.config_files"], ",")[0],
	}
	if found.Project == "" {
		return found, fmt.Errorf("container %q was not started by docker compose", container)
	}
	return found, nil
}

// Hostname is the updater's own container id.
func Hostname() string {
	name, _ := os.Hostname()
	return name
}

// SelfUpdate pulls the flixer-updater image and, if it differs from the running container's image,
// spawns a detached one-off container to recreate flixer-updater without interrupting the caller.
func (c *CLI) SelfUpdate(ctx context.Context) (bool, error) {
	container := Hostname()
	if container == "" {
		return false, nil
	}
	currentID, err := c.run(ctx, "docker", "inspect", "--format", "{{.Image}}", container)
	if err != nil {
		return false, err
	}
	imageRef, err := c.run(ctx, "docker", "inspect", "--format", "{{.Config.Image}}", container)
	if err != nil {
		return false, err
	}
	imageRef = strings.TrimSpace(imageRef)
	if imageRef == "" {
		return false, nil
	}
	if _, err := c.Compose(ctx, "pull", "flixer-updater"); err != nil {
		return false, err
	}
	latestID, err := c.run(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", imageRef)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(latestID) == "" || strings.TrimSpace(latestID) == strings.TrimSpace(currentID) {
		return false, nil
	}
	recreateCmd := "sleep 1 && exec docker " + shellJoin(append(append([]string{}, c.base...), "up", "--detach", "--no-deps", "flixer-updater"))
	if _, err := c.Compose(ctx, "run", "-d", "--rm", "--no-deps", "--entrypoint", "sh", "flixer-updater", "-c", recreateCmd); err != nil {
		return false, err
	}
	return true, nil
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	return strings.Join(quoted, " ")
}
