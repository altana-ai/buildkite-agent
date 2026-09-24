// Package jobcontainers finds the Docker containers a job left running, and
// removes them. A job's cgroup cannot contain them, because dockerd starts
// them, not the job.
//
// It is intended for internal use by buildkite-agent only.
package jobcontainers

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/agent/v3/logger"
)

// StopTimeout is how long a container on a shared host gets to exit after
// SIGTERM before it is killed.
const StopTimeout = 10 * time.Second

// ErrNotRemoved is returned by Sweeper.Remove when containers are still
// running after being removed.
var ErrNotRemoved = errors.New("job containers still running after being removed")

// Reason says why a container was taken to be a job's.
type Reason string

const (
	ReasonCreatedDuringJob Reason = "created during the job"
	ReasonJobLabel         Reason = "job label"
	ReasonBindMount        Reason = "bind mount in the agent's build dir"
	ReasonComposeDir       Reason = "compose working dir in the agent's build dir"
)

// jobIDLabels are the labels the docker and docker-compose plugins give a
// job's containers.
var jobIDLabels = []string{"com.buildkite.job_id", "com.buildkite.job-id"}

const composeWorkingDirLabel = "com.docker.compose.project.working_dir"

// Container is what the sweeper learns about a running container. It has no
// environment or command, which can hold secrets the agent never learns.
type Container struct {
	ID    string
	Name  string
	Image string

	Labels      map[string]string
	BindSources []string
}

// Leftover is a job's container that outlived the job.
type Leftover struct {
	ID     string
	Name   string
	Image  string
	Reason Reason
}

// Docker is the part of the Docker API that the sweeper uses.
type Docker interface {
	// Containers returns the IDs of every container, running or not.
	Containers(ctx context.Context) ([]string, error)

	// RunningContainers returns the IDs of every running container.
	RunningContainers(ctx context.Context) ([]string, error)

	// Inspect describes the containers among ids that still exist.
	Inspect(ctx context.Context, ids []string) ([]Container, error)

	Networks(ctx context.Context) ([]string, error)

	// Stop sends each container SIGTERM, and SIGKILL after timeout.
	Stop(ctx context.Context, ids []string, timeout time.Duration) error

	// Remove force-removes containers and their anonymous volumes.
	Remove(ctx context.Context, ids []string) error

	RemoveNetworks(ctx context.Context, ids []string) error
}

// Job identifies the job whose containers are swept.
type Job struct {
	ID string

	// BuildDir is the agent's own directory under its build path, where
	// the job's checkout is unless a hook moved it.
	BuildDir string
}

// Sweeper finds and removes a job's leftover containers.
type Sweeper struct {
	docker Docker
	shared bool

	warnOnce sync.Once
}

// New returns a Sweeper. shared is whether other workers run jobs on this
// host at the same time, which decides how it tells the job's containers.
func New(docker Docker, shared bool) *Sweeper {
	return &Sweeper{docker: docker, shared: shared}
}

// Snapshot is the host's Docker state when a job started.
type Snapshot struct {
	containers map[string]bool
	networks   map[string]bool
}

// Snapshot records every container and network, so that containers the job
// did not create are never taken for its own, even if they start during it.
func (s *Sweeper) Snapshot(ctx context.Context) (*Snapshot, error) {
	containers, err := s.docker.Containers(ctx)
	if err != nil {
		return nil, err
	}
	networks, err := s.docker.Networks(ctx)
	if err != nil {
		return nil, err
	}
	return &Snapshot{containers: set(containers), networks: set(networks)}, nil
}

// WarnUnavailable logs that Docker could not be reached, as a warning the
// first time and at debug level after that, since Docker is optional.
func (s *Sweeper) WarnUnavailable(l logger.Logger, err error) {
	warned := true
	s.warnOnce.Do(func() {
		warned = false
		l.Warnf("Docker is unavailable, so job-cgroup won't remove the containers jobs leave running: %v", err)
	})
	if warned {
		l.Debugf("Docker is unavailable, so job-cgroup won't remove this job's containers: %v", err)
	}
}

// Leftovers returns the running containers created since snap that are the
// job's. With one agent on the host, all of them are. On a shared host, only
// those tied to this job by a label or a path in its build dir are.
func (s *Sweeper) Leftovers(ctx context.Context, snap *Snapshot, job Job) ([]Leftover, error) {
	running, err := s.docker.RunningContainers(ctx)
	if err != nil {
		return nil, err
	}
	running = slices.DeleteFunc(running, func(id string) bool { return snap.containers[id] })
	if len(running) == 0 {
		return nil, nil
	}

	containers, err := s.docker.Inspect(ctx, running)
	if err != nil {
		return nil, err
	}
	var leftovers []Leftover
	for _, c := range containers {
		reason := ReasonCreatedDuringJob
		if s.shared {
			var ok bool
			if reason, ok = jobsOwn(c, job); !ok {
				continue
			}
		}
		leftovers = append(leftovers, Leftover{ID: c.ID, Name: c.Name, Image: c.Image, Reason: reason})
	}
	return leftovers, nil
}

// jobsOwn reports whether c belongs to job, and why.
func jobsOwn(c Container, job Job) (Reason, bool) {
	for _, label := range jobIDLabels {
		if job.ID != "" && c.Labels[label] == job.ID {
			return ReasonJobLabel, true
		}
	}
	if slices.ContainsFunc(c.BindSources, func(src string) bool { return within(job.BuildDir, src) }) {
		return ReasonBindMount, true
	}
	if within(job.BuildDir, c.Labels[composeWorkingDirLabel]) {
		return ReasonComposeDir, true
	}
	return "", false
}

// within reports whether path is dir or inside it.
func within(dir, path string) bool {
	if dir == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Remove removes the leftovers, and returns an error wrapping ErrNotRemoved
// if any of them is still running afterwards.
func (s *Sweeper) Remove(ctx context.Context, leftovers []Leftover) error {
	if len(leftovers) == 0 {
		return nil
	}
	ids := make([]string, 0, len(leftovers))
	for _, c := range leftovers {
		ids = append(ids, c.ID)
	}

	// Another job may share a volume or compose project with these, so they
	// get the chance to exit cleanly first.
	var stopErr error
	if s.shared {
		stopErr = s.docker.Stop(ctx, ids, StopTimeout)
	}
	removeErr := s.docker.Remove(ctx, ids)

	running, err := s.docker.RunningContainers(ctx)
	if err != nil {
		return fmt.Errorf("%w: couldn't list running containers: %w", ErrNotRemoved, errors.Join(err, stopErr, removeErr))
	}
	isRunning := set(running)
	survivors := slices.DeleteFunc(ids, func(id string) bool { return !isRunning[id] })
	if len(survivors) > 0 {
		return fmt.Errorf("%w: %s: %w", ErrNotRemoved, strings.Join(survivors, ", "), errors.Join(stopErr, removeErr))
	}
	return nil
}

// RemoveNetworks removes the networks created since snap on a host with one
// agent. On a shared host they may be another job's, so it does nothing.
func (s *Sweeper) RemoveNetworks(ctx context.Context, snap *Snapshot) error {
	if s.shared {
		return nil
	}
	networks, err := s.docker.Networks(ctx)
	if err != nil {
		return err
	}
	networks = slices.DeleteFunc(networks, func(id string) bool { return snap.networks[id] })
	if len(networks) == 0 {
		return nil
	}
	return s.docker.RemoveNetworks(ctx, networks)
}

func set(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}
