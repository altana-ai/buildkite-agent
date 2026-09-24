//go:build linux

package jobcontainers

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// testImage is any small image with sleep that the test host already has.
// The test never pulls it.
var testImage = cmp.Or(os.Getenv("JOBCONTAINERS_TEST_IMAGE"), "debian:trixie-slim")

// realDocker skips the test unless a Docker daemon and testImage are
// available.
func realDocker(t *testing.T) CLI {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("no docker command: %v", err)
	}
	if out, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", testImage).CombinedOutput(); err != nil {
		t.Skipf("no Docker daemon or no %s image: %v: %s", testImage, err, out)
	}
	return CLI{}
}

// runContainer starts a detached container that sleeps, and removes it when
// the test ends if the sweeper did not.
func runContainer(t *testing.T, args ...string) string {
	t.Helper()

	name := fmt.Sprintf("jobcontainers-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	argv := append([]string{"run", "--detach", "--pull", "never", "--name", name}, args...)
	argv = append(argv, testImage, "sleep", "300")
	var stderr strings.Builder
	cmd := exec.Command("docker", argv...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker %s: %v: %s", strings.Join(argv, " "), err, stderr.String())
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		exec.Command("docker", "rm", "--force", id).Run() //nolint:errcheck // Best-effort test cleanup.
	})
	// An image for another architecture starts and exits at once.
	state, err := exec.Command("docker", "container", "inspect", "--format", "{{.State.Running}}", id).Output()
	if err != nil || strings.TrimSpace(string(state)) != "true" {
		t.Fatalf("container %s from %s is not running (%q, %v)", id, testImage, state, err)
	}
	return id
}

// The test runs only the shared-host sweep, because on a host with one agent
// the sweep takes every container started during the job, which on a
// developer's machine includes containers the test did not start.
func TestSweeper_RealDockerOnASharedHost(t *testing.T) {
	t.Parallel()

	c := realDocker(t)
	jobID := fmt.Sprintf("jobcontainers-test-job-%d", time.Now().UnixNano())
	buildDir := t.TempDir()

	labelled := runContainer(t, "--label", "com.buildkite.job-id="+jobID)
	mounted := runContainer(t, "--volume", buildDir+":/src")
	// SIGTERM does not stop sleep as PID 1, so this also shows the kill
	// after the stop timeout.
	unrelated := runContainer(t, "--label", "com.buildkite.job-id=another-job")

	s := New(c, true)
	ctx := t.Context()
	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatalf("s.Snapshot() error = %v", err)
	}
	leftovers, err := s.Leftovers(ctx, snap, Job{ID: jobID, BuildDir: buildDir})
	if err != nil {
		t.Fatalf("s.Leftovers() error = %v", err)
	}
	got := map[string]Reason{}
	for _, l := range leftovers {
		got[l.ID] = l.Reason
		if l.Image != testImage || !strings.HasPrefix(l.Name, "jobcontainers-test-") {
			t.Errorf("leftover %+v, want image %q and the test's name", l, testImage)
		}
	}
	if want := map[string]Reason{labelled: ReasonJobLabel, mounted: ReasonBindMount}; !maps.Equal(got, want) {
		t.Fatalf("s.Leftovers() = %v, want %v", got, want)
	}

	if err := s.Remove(ctx, leftovers); err != nil {
		t.Fatalf("s.Remove() error = %v", err)
	}
	running, err := c.RunningContainers(ctx)
	if err != nil {
		t.Fatalf("c.RunningContainers() error = %v", err)
	}
	for _, id := range []string{labelled, mounted} {
		if slices.Contains(running, id) {
			t.Errorf("container %s is still running after s.Remove()", id)
		}
	}
	if !slices.Contains(running, unrelated) {
		t.Errorf("container %s from another job was removed, want it left running", unrelated)
	}
}
