//go:build linux

package jobcontainers

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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

	s := New(c, true)
	ctx := t.Context()
	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatalf("s.Snapshot() error = %v", err)
	}

	labelled := runContainer(t, "--label", "com.buildkite.job-id="+jobID)
	mounted := runContainer(t, "--volume", buildDir+":/src")
	// SIGTERM does not stop sleep as PID 1, so this also shows the kill
	// after the stop timeout.
	unrelated := runContainer(t, "--label", "com.buildkite.job-id=another-job")

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

// isolatedDocker returns a CLI for the daemon at
// $JOBCONTAINERS_TEST_ISOLATED_DOCKER_HOST, such as a docker:dind container,
// and a function that runs docker commands against it. It skips the test
// otherwise, because the one-agent sweep removes every container that
// starts while it runs.
func isolatedDocker(t *testing.T) (CLI, func(args ...string) string) {
	t.Helper()

	host := os.Getenv("JOBCONTAINERS_TEST_ISOLATED_DOCKER_HOST")
	if host == "" {
		t.Skip("JOBCONTAINERS_TEST_ISOLATED_DOCKER_HOST is not set to a disposable Docker daemon")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("no docker command: %v", err)
	}
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec docker -H '"+host+"' \"$@\"\n"), 0o755); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
	run := func(args ...string) string {
		t.Helper()
		var stderr strings.Builder
		cmd := exec.Command(path, args...)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, stderr.String())
		}
		return strings.TrimSpace(string(out))
	}
	return CLI{Path: path}, run
}

func TestSweeper_IsolatedDockerWithOneAgent(t *testing.T) {
	c, docker := isolatedDocker(t)
	sleeper := func(args ...string) string {
		return docker(append(append([]string{"run", "--detach", "--pull", "never"}, args...), testImage, "sleep", "300")...)
	}
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)

	preNetwork := docker("network", "create", "pre-net-"+suffix)
	preVolume := "pre-vol-" + suffix
	docker("volume", "create", preVolume)
	pre := sleeper("--network", preNetwork, "--volume", preVolume+":/v")
	stoppedBefore := sleeper()
	docker("stop", "--time", "0", stoppedBefore)
	jobVolume := "job-vol-" + suffix
	t.Cleanup(func() {
		docker("rm", "--force", pre, stoppedBefore)
		docker("network", "rm", preNetwork)
		docker("volume", "rm", preVolume, jobVolume)
	})

	s := New(c, false)
	ctx := t.Context()
	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatalf("s.Snapshot() error = %v", err)
	}

	// The job restarting a container it did not create does not make it the
	// job's.
	docker("start", stoppedBefore)
	jobNetwork := docker("network", "create", "job-net-"+suffix)
	withNamedVolume := sleeper("--network", jobNetwork, "--volume", jobVolume+":/v")
	withAnonymousVolume := sleeper("--volume", "/data")
	anonymousVolume := docker("container", "inspect", "--format", "{{range .Mounts}}{{.Name}}{{end}}", withAnonymousVolume)

	leftovers, err := s.Leftovers(ctx, snap, Job{ID: "job-" + suffix, BuildDir: t.TempDir()})
	if err != nil {
		t.Fatalf("s.Leftovers() error = %v", err)
	}
	got := map[string]Reason{}
	for _, l := range leftovers {
		got[l.ID] = l.Reason
	}
	want := map[string]Reason{withNamedVolume: ReasonCreatedDuringJob, withAnonymousVolume: ReasonCreatedDuringJob}
	if !maps.Equal(got, want) {
		t.Fatalf("s.Leftovers() = %v, want %v", got, want)
	}
	if err := s.Remove(ctx, leftovers); err != nil {
		t.Fatalf("s.Remove() error = %v", err)
	}
	if err := s.RemoveNetworks(ctx, snap); err != nil {
		t.Fatalf("s.RemoveNetworks() error = %v", err)
	}

	got = map[string]Reason{}
	for _, id := range strings.Fields(docker("ps", "--all", "--quiet", "--no-trunc")) {
		got[id] = ""
	}
	if want := map[string]Reason{pre: "", stoppedBefore: ""}; !maps.Equal(got, want) {
		t.Errorf("containers = %v, want only those from before the job, %v", got, want)
	}
	networks := strings.Fields(docker("network", "ls", "--quiet", "--no-trunc"))
	if !slices.Contains(networks, preNetwork) {
		t.Errorf("network %s from before the job was removed", preNetwork)
	}
	if slices.Contains(networks, jobNetwork) {
		t.Errorf("network %s the job created is still there", jobNetwork)
	}
	volumes := strings.Fields(docker("volume", "ls", "--quiet"))
	for _, v := range []string{preVolume, jobVolume} {
		if !slices.Contains(volumes, v) {
			t.Errorf("named volume %s was removed, want every named volume kept", v)
		}
	}
	if slices.Contains(volumes, anonymousVolume) {
		t.Errorf("anonymous volume %s of a removed container is still there", anonymousVolume)
	}
}
