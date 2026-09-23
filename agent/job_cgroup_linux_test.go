//go:build linux

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/buildkite/agent/v3/api"
	"github.com/buildkite/agent/v3/internal/jobcgroup"
	"github.com/buildkite/agent/v3/logger"
	"github.com/buildkite/agent/v3/metrics"
	"github.com/google/uuid"
)

// testJobCgroup returns a Manager rooted in a fresh group under the test's
// own cgroup, or skips the test when that cgroup is not delegated to the user
// running it.
func testJobCgroup(t *testing.T) *jobcgroup.Manager {
	t.Helper()

	data, err := os.ReadFile("/proc/self/cgroup")
	own, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "0::")
	if err != nil || !ok {
		t.Skipf("no cgroup v2: %v %q", err, data)
	}
	parent := filepath.Join("/sys/fs/cgroup", own)
	if _, err := os.Stat(filepath.Join(parent, "cgroup.kill")); err != nil {
		t.Skipf("no cgroup.kill: %v", err)
	}
	root := filepath.Join(parent, fmt.Sprintf("jobcgroup-agent-test-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Skipf("the test's cgroup is not delegated: %v", err)
	}

	m := jobcgroup.NewManager(jobcgroup.ModeEnforce, root)
	t.Cleanup(func() {
		if err := m.KillAll(jobcgroup.DrainTimeout); err != nil {
			t.Errorf("KillAll() error = %v", err)
		}
		if err := syscall.Rmdir(root); err != nil {
			t.Errorf("rmdir %q: %v", root, err)
		}
	})
	return m
}

func TestAgentWorker_RunJobReleasesTheJobCgroupOfAJobItCannotRun(t *testing.T) {
	t.Parallel()

	m := testJobCgroup(t)
	server := NewFakeAPIServer()
	defer server.Close()

	l := logger.Discard
	worker := NewAgentWorker(
		l,
		&api.AgentRegisterResponse{UUID: uuid.New().String(), AccessToken: "alpacas", Endpoint: server.URL},
		metrics.NewCollector(l, metrics.CollectorConfig{}),
		api.NewClient(l, api.Config{Endpoint: server.URL, Token: "llamas"}),
		AgentWorkerConfig{AgentConfiguration: AgentConfiguration{BootstrapScript: "true", JobCgroup: m}},
	)
	worker.metrics = worker.metricsCollector.Scope(metrics.Tags{})
	worker.jobRunner.Store(&JobRunner{})

	err := worker.RunJob(t.Context(), &api.Job{ID: "second-job", ChunksMaxSizeBytes: 1024}, nil)
	if err == nil || !strings.Contains(err.Error(), "already has a job running") {
		t.Fatalf("worker.RunJob() error = %v, want the worker to refuse a second job", err)
	}

	entries, err := os.ReadDir(m.Root())
	if err != nil {
		t.Fatalf("os.ReadDir(%q) error = %v", m.Root(), err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("job group %s remains after the worker refused its job", e.Name())
		}
	}
	if fds := openFDsUnder(t, m.Root()); len(fds) > 0 {
		t.Errorf("descriptors %q remain open under %s", fds, m.Root())
	}
}

// openFDsUnder returns this process's open descriptors for paths under dir.
func openFDsUnder(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("os.ReadDir(/proc/self/fd) error = %v", err)
	}
	var fds []string
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err == nil && strings.HasPrefix(target, dir+"/") {
			fds = append(fds, target)
		}
	}
	return fds
}
