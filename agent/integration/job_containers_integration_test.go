//go:build linux

package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buildkite/agent/v3/agent"
	"github.com/buildkite/agent/v3/internal/jobcgroup"
	"github.com/buildkite/agent/v3/internal/jobcontainers"
	"github.com/buildkite/agent/v3/logger"
)

// jobDocker is a Docker daemon on which the bootstrap "starts" a container by
// creating $DIR/container, and a network by creating $DIR/network.
type jobDocker struct {
	dir string

	// container is the container the job starts. The daemon also runs one
	// that was there before the job.
	container jobcontainers.Container

	snapshotErr error
	stuck       bool

	// listErr is returned by every listing after the snapshot.
	listErr error

	// onSnapshot runs while the snapshot is being taken.
	onSnapshot func()

	mu      sync.Mutex
	removed []string
	calls   int
}

const preexistingID = "preexisting-container"

func (d *jobDocker) started(name string) bool {
	_, err := os.Stat(filepath.Join(d.dir, name))
	return err == nil
}

func (d *jobDocker) RunningContainers(context.Context) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.snapshotErr != nil {
		return nil, d.snapshotErr
	}
	if d.listErr != nil && d.calls > 1 {
		return nil, d.listErr
	}
	ids := []string{preexistingID}
	if d.started("container") && (d.stuck || !slices.Contains(d.removed, d.container.ID)) {
		ids = append(ids, d.container.ID)
	}
	return ids, nil
}

func (d *jobDocker) Containers(ctx context.Context) ([]string, error) {
	if d.onSnapshot != nil {
		d.onSnapshot()
	}
	return d.RunningContainers(ctx)
}

func (d *jobDocker) Inspect(_ context.Context, ids []string) ([]jobcontainers.Container, error) {
	var found []jobcontainers.Container
	for _, id := range ids {
		switch id {
		case d.container.ID:
			found = append(found, d.container)
		case preexistingID:
			found = append(found, jobcontainers.Container{ID: preexistingID, Name: "host-service", Image: "datadog/agent"})
		}
	}
	return found, nil
}

func (d *jobDocker) Networks(context.Context) ([]string, error) {
	networks := []string{"bridge"}
	if d.started("network") && !slices.Contains(d.removed, "job-network") {
		networks = append(networks, "job-network")
	}
	return networks, nil
}

func (d *jobDocker) Stop(context.Context, []string, time.Duration) error { return nil }

func (d *jobDocker) Remove(_ context.Context, ids []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removed = append(d.removed, ids...)
	if d.stuck {
		return errors.New("removal is already in progress")
	}
	return nil
}

func (d *jobDocker) RemoveNetworks(_ context.Context, ids []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removed = append(d.removed, ids...)
	return nil
}

func (d *jobDocker) wasRemoved(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Contains(d.removed, id)
}

// The label value and mount path stand in for data a job controls, which the
// report must not repeat.
const (
	secretLabel = "label-s3cret"
	secretMount = "/mount-s3cret"
)

func leakedContainer() jobcontainers.Container {
	return jobcontainers.Container{
		ID:          "0123456789abcdef0123456789abcdef",
		Name:        "itest-db",
		Image:       "postgres:16",
		Labels:      map[string]string{"com.example.token": secretLabel},
		BindSources: []string{secretMount},
	}
}

const containerLeakingBootstrap = `touch "$DIR/container" "$DIR/network"` + "\n"

func TestJobCgroup_ContainersTheJobLeftRunning(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		mode        jobcgroup.Mode
		stuck       bool
		wantLog     string
		wantRemoved bool
		wantTainted bool
	}{
		"enforce removes them": {
			mode:        jobcgroup.ModeEnforce,
			wantLog:     "1 containers outlived the job and will be removed now",
			wantRemoved: true,
		},
		"report leaves them": {
			mode:    jobcgroup.ModeReport,
			wantLog: "1 containers outlived the job and are left running, since job-cgroup is report",
		},
		"enforce stops taking jobs when one survives": {
			mode:        jobcgroup.ModeEnforce,
			stuck:       true,
			wantLog:     "Containers left by this job could not all be removed",
			wantTainted: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			m := testJobCgroup(t, test.mode)
			e := createTestAgentEndpoint()
			server := e.server()
			defer server.Close()

			var agentLog lockedBuffer
			l := logger.NewConsoleLogger(logger.NewJSONPrinter(&agentLog), func(int) {})
			dir := t.TempDir()
			d := &jobDocker{dir: dir, container: leakedContainer(), stuck: test.stuck}
			jr := newLoggedScriptJobRunner(t, l, &lockedBuffer{}, server.URL, "container-job", dir, containerLeakingBootstrap, agent.AgentConfiguration{
				JobCgroup:     m,
				JobContainers: jobcontainers.New(d, false),
			})
			if err := jr.Run(t.Context(), nil); err != nil {
				t.Fatalf("jr.Run() error = %v", err)
			}

			logs := e.logsFor(t, "container-job")
			if !strings.Contains(logs, test.wantLog) {
				t.Errorf("job log = %q, want it to contain %q", logs, test.wantLog)
			}
			if want := `id=0123456789ab name="itest-db" image="postgres:16" matched="created during the job"`; !strings.Contains(logs, want) {
				t.Errorf("job log = %q, want it to contain %q", logs, want)
			}
			if want := "leftover_containers"; !strings.Contains(agentLog.String(), want) {
				t.Errorf("agent log = %q, want it to contain %q", agentLog.String(), want)
			}
			for name, log := range map[string]string{"job log": logs, "agent log": agentLog.String()} {
				for _, secret := range []string{secretLabel, secretMount, "host-service"} {
					if strings.Contains(log, secret) {
						t.Errorf("%s = %q, want no %q in it", name, log, secret)
					}
				}
			}

			if got := d.wasRemoved(d.container.ID); got != (test.wantRemoved || test.stuck) {
				t.Errorf("container removed = %t, want %t", got, test.wantRemoved || test.stuck)
			}
			if got := d.wasRemoved("job-network"); got != test.wantRemoved {
				t.Errorf("job's network removed = %t, want %t", got, test.wantRemoved)
			}
			if d.wasRemoved(preexistingID) {
				t.Error("the container that predates the job was removed, want it left running")
			}
			if got := m.IsTainted(); got != test.wantTainted {
				t.Errorf("m.IsTainted() = %t, want %t", got, test.wantTainted)
			}
			if got := ignoreInDispatches(t, e, "container-job"); (got != nil && *got) != test.wantTainted {
				t.Errorf("finish.IgnoreAgentInDispatches = %v, want %t", ptrString(got), test.wantTainted)
			}
		})
	}
}

func TestJobCgroup_ContainersOfThisJobOnASharedHost(t *testing.T) {
	t.Parallel()

	// The container mounts worker 2's checkout.
	for agentName, wantRemoved := range map[string]bool{"worker-2": true, "worker-1": false} {
		t.Run(agentName, func(t *testing.T) {
			t.Parallel()

			m := testJobCgroup(t, jobcgroup.ModeEnforce)
			e := createTestAgentEndpoint()
			server := e.server()
			defer server.Close()

			buildPath := t.TempDir()
			dir := t.TempDir()
			c := leakedContainer()
			c.BindSources = []string{filepath.Join(buildPath, "worker-2", "org", "pipeline")}
			d := &jobDocker{dir: dir, container: c}
			jr := newLoggedScriptJobRunner(t, logger.Discard, &lockedBuffer{}, server.URL, "shared-job", dir, containerLeakingBootstrap, agent.AgentConfiguration{
				BuildPath:     buildPath,
				JobCgroup:     m,
				JobContainers: jobcontainers.New(d, true),
			}, func(c *agent.JobRunnerConfig) { c.AgentName = agentName })
			if err := jr.Run(t.Context(), nil); err != nil {
				t.Fatalf("jr.Run() error = %v", err)
			}

			logs := e.logsFor(t, "shared-job")
			if want := `matched="bind mount in the agent's build dir"`; strings.Contains(logs, want) != wantRemoved {
				t.Errorf("job log = %q, want it to contain %q: %t", logs, want, wantRemoved)
			}
			if got := d.wasRemoved(c.ID); got != wantRemoved {
				t.Errorf("container removed = %t, want %t", got, wantRemoved)
			}
			// Another job may be using the network.
			if d.wasRemoved("job-network") || d.wasRemoved(preexistingID) {
				t.Errorf("removed = %q, want at most the job's container", d.removed)
			}
		})
	}
}

// Docker answered when the job started, so a failure to list its containers
// afterwards can't be taken to mean there are none.
func TestJobCgroup_ContainersThatCantBeListedTaintEnforce(t *testing.T) {
	t.Parallel()

	for mode, wantTainted := range map[jobcgroup.Mode]bool{jobcgroup.ModeEnforce: true, jobcgroup.ModeReport: false} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			m := testJobCgroup(t, mode)
			e := createTestAgentEndpoint()
			server := e.server()
			defer server.Close()

			dir := t.TempDir()
			d := &jobDocker{dir: dir, container: leakedContainer(), listErr: errors.New("context deadline exceeded")}
			jr := newScriptJobRunner(t, server.URL, "unlisted-job", dir, containerLeakingBootstrap, agent.AgentConfiguration{
				JobCgroup:     m,
				JobContainers: jobcontainers.New(d, false),
			})
			if err := jr.Run(t.Context(), nil); err != nil {
				t.Fatalf("jr.Run() error = %v", err)
			}
			if got := m.IsTainted(); got != wantTainted {
				t.Errorf("m.IsTainted() = %t, want %t", got, wantTainted)
			}
			if got := ignoreInDispatches(t, e, "unlisted-job"); (got != nil && *got) != wantTainted {
				t.Errorf("finish.IgnoreAgentInDispatches = %v, want %t", ptrString(got), wantTainted)
			}
		})
	}
}

func TestJobCgroup_ContainersAreLeftAloneWithoutDocker(t *testing.T) {
	t.Parallel()

	m := testJobCgroup(t, jobcgroup.ModeEnforce)
	e := createTestAgentEndpoint()
	server := e.server()
	defer server.Close()

	var agentLog lockedBuffer
	l := logger.NewConsoleLogger(logger.NewJSONPrinter(&agentLog), func(int) {})
	dir := t.TempDir()
	d := &jobDocker{dir: dir, container: leakedContainer(), snapshotErr: errors.New("Cannot connect to the Docker daemon")}
	jr := newLoggedScriptJobRunner(t, l, &lockedBuffer{}, server.URL, "dockerless-job", dir, containerLeakingBootstrap, agent.AgentConfiguration{
		JobCgroup:     m,
		JobContainers: jobcontainers.New(d, false),
	})
	if err := jr.Run(t.Context(), nil); err != nil {
		t.Fatalf("jr.Run() error = %v", err)
	}

	if want := "Docker is unavailable"; !strings.Contains(agentLog.String(), want) {
		t.Errorf("agent log = %q, want it to contain %q", agentLog.String(), want)
	}
	if m.IsTainted() {
		t.Error("m.IsTainted() = true, want Docker to be optional")
	}
	if d.calls != 1 {
		t.Errorf("docker was asked for its containers %d times, want only once, at the start", d.calls)
	}
}

func TestJobCgroup_OffLeavesContainersAlone(t *testing.T) {
	t.Parallel()

	e := createTestAgentEndpoint()
	server := e.server()
	defer server.Close()

	dir := t.TempDir()
	d := &jobDocker{dir: dir, container: leakedContainer()}
	jr := newScriptJobRunner(t, server.URL, "off-container-job", dir, containerLeakingBootstrap, agent.AgentConfiguration{
		JobContainers: jobcontainers.New(d, false),
	})
	if err := jr.Run(t.Context(), nil); err != nil {
		t.Fatalf("jr.Run() error = %v", err)
	}
	if d.calls != 0 || len(d.removed) != 0 {
		t.Errorf("docker calls = %d, removed = %q, want Docker untouched", d.calls, d.removed)
	}
}

// A cancel that arrives while Docker is slow to answer the snapshot finds no
// process to signal, so the bootstrap must not start after it.
func TestJobCgroup_CancelDuringTheContainerSnapshot(t *testing.T) {
	t.Parallel()

	m := testJobCgroup(t, jobcgroup.ModeEnforce)
	e := createTestAgentEndpoint()
	server := e.server()
	defer server.Close()

	dir := t.TempDir()
	d := &jobDocker{dir: dir, container: leakedContainer()}
	jr := newScriptJobRunner(t, server.URL, "cancelled-in-snapshot", dir, `touch "$DIR/ran"`+"\n", agent.AgentConfiguration{
		JobCgroup:         m,
		JobContainers:     jobcontainers.New(d, false),
		SignalGracePeriod: 100 * time.Millisecond,
	})
	d.onSnapshot = func() {
		if err := jr.Cancel(agent.CancelReasonJobState); err != nil {
			t.Errorf("jr.Cancel() error = %v", err)
		}
	}
	if err := jr.Run(t.Context(), nil); err != nil {
		t.Fatalf("jr.Run() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "ran")); err == nil {
		t.Error("the bootstrap ran after the job was cancelled")
	}
	finish := e.finishesFor(t, "cancelled-in-snapshot")[0]
	if got, want := finish.SignalReason, "cancel"; got != want {
		t.Errorf("finish.SignalReason = %q, want %q", got, want)
	}
	assertNoJobGroups(t, m)
}
