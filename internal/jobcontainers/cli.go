package jobcontainers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// commandTimeout bounds each docker command, so that a hung daemon delays
// the end of a job instead of blocking it forever.
const commandTimeout = 30 * time.Second

// CLI reaches Docker by running the docker command, so it needs no socket
// path or API version of its own.
type CLI struct {
	// Path is the docker command. Empty means "docker" on PATH.
	Path string
}

func (c CLI) RunningContainers(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, commandTimeout, "ps", "--quiet", "--no-trunc")
	return strings.Fields(string(out)), err
}

func (c CLI) Networks(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, commandTimeout, "network", "ls", "--quiet", "--no-trunc")
	return strings.Fields(string(out)), err
}

// inspected is the subset of `docker container inspect` that Container
// needs. Config.Env and the command are left out so they are never read.
type inspected struct {
	ID     string `json:"Id"`
	Name   string
	Config struct {
		Image  string
		Labels map[string]string
	}
	Mounts []struct {
		Type   string
		Source string
	}
}

func (c CLI) Inspect(ctx context.Context, ids []string) ([]Container, error) {
	out, runErr := c.run(ctx, commandTimeout, append([]string{"container", "inspect"}, ids...)...)

	// Inspect fails when any container has gone, but still describes the
	// others, and a container that has gone needs no removing.
	var found []inspected
	if err := json.Unmarshal(out, &found); err != nil {
		if runErr != nil {
			return nil, runErr
		}
		return nil, fmt.Errorf("parsing docker container inspect output: %w", err)
	}
	if runErr != nil && !allGone(runErr) {
		return nil, runErr
	}

	containers := make([]Container, 0, len(found))
	for _, f := range found {
		c := Container{
			ID:     f.ID,
			Name:   strings.TrimPrefix(f.Name, "/"),
			Image:  f.Config.Image,
			Labels: f.Config.Labels,
		}
		for _, m := range f.Mounts {
			if m.Type == "bind" {
				c.BindSources = append(c.BindSources, m.Source)
			}
		}
		containers = append(containers, c)
	}
	return containers, nil
}

// allGone reports whether err says only that containers don't exist.
func allGone(err error) bool {
	var cmdErr *commandError
	if !errors.As(err, &cmdErr) {
		return false
	}
	lines := strings.FieldsFunc(cmdErr.stderr, func(r rune) bool { return r == '\n' })
	for _, line := range lines {
		if !strings.Contains(line, "No such container") && !strings.Contains(line, "No such object") {
			return false
		}
	}
	return len(lines) > 0
}

func (c CLI) Stop(ctx context.Context, ids []string, timeout time.Duration) error {
	seconds := strconv.Itoa(int(timeout / time.Second))
	_, err := c.run(ctx, timeout+commandTimeout, append([]string{"stop", "--time", seconds}, ids...)...)
	return err
}

func (c CLI) Remove(ctx context.Context, ids []string) error {
	_, err := c.run(ctx, commandTimeout, append([]string{"rm", "--force", "--volumes"}, ids...)...)
	return err
}

func (c CLI) RemoveNetworks(ctx context.Context, ids []string) error {
	_, err := c.run(ctx, commandTimeout, append([]string{"network", "rm"}, ids...)...)
	return err
}

// commandError is a docker command that failed, with what it wrote to stderr.
type commandError struct {
	args   []string
	err    error
	stderr string
}

func (e *commandError) Error() string {
	return fmt.Sprintf("docker %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}

func (e *commandError) Unwrap() error { return e.err }

// maxStderr bounds how much of a failed command's stderr goes into its error.
const maxStderr = 1024

func (c CLI) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	path := c.Path
	if path == "" {
		path = "docker"
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > maxStderr {
			msg = msg[:maxStderr] + "…"
		}
		return stdout.Bytes(), &commandError{args: args[:min(len(args), 3)], err: err, stderr: msg}
	}
	return stdout.Bytes(), nil
}
