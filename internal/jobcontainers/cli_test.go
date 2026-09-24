//go:build !windows

package jobcontainers

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// fakeCLI returns a CLI whose docker command is script, run by sh after it
// appends its arguments to the returned file.
func fakeCLI(t *testing.T, script string) (CLI, string) {
	t.Helper()

	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	path := filepath.Join(dir, "docker")
	body := "#!/bin/sh\necho \"$*\" >> '" + args + "'\n" + script
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("os.WriteFile(%q) error = %v", path, err)
	}
	return CLI{Path: path}, args
}

func readArgs(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) error = %v", path, err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestCLI_Commands(t *testing.T) {
	t.Parallel()

	c, args := fakeCLI(t, `
case "$1" in
ps) printf 'aaa\nbbb\n' ;;
network) printf 'n1\n' ;;
esac
`)
	all, err := c.Containers(t.Context())
	if err != nil {
		t.Fatalf("c.Containers() error = %v", err)
	}
	if diff := cmp.Diff([]string{"aaa", "bbb"}, all); diff != "" {
		t.Errorf("c.Containers() diff (-want +got):\n%s", diff)
	}
	running, err := c.RunningContainers(t.Context())
	if err != nil {
		t.Fatalf("c.RunningContainers() error = %v", err)
	}
	if diff := cmp.Diff([]string{"aaa", "bbb"}, running); diff != "" {
		t.Errorf("c.RunningContainers() diff (-want +got):\n%s", diff)
	}
	networks, err := c.Networks(t.Context())
	if err != nil {
		t.Fatalf("c.Networks() error = %v", err)
	}
	if diff := cmp.Diff([]string{"n1"}, networks); diff != "" {
		t.Errorf("c.Networks() diff (-want +got):\n%s", diff)
	}
	if err := c.Stop(t.Context(), []string{"aaa", "bbb"}, 10*time.Second); err != nil {
		t.Errorf("c.Stop() error = %v", err)
	}
	if err := c.Remove(t.Context(), []string{"aaa", "bbb"}); err != nil {
		t.Errorf("c.Remove() error = %v", err)
	}
	if err := c.RemoveNetworks(t.Context(), []string{"n1"}); err != nil {
		t.Errorf("c.RemoveNetworks() error = %v", err)
	}

	want := []string{
		"ps --all --quiet --no-trunc",
		"ps --quiet --no-trunc",
		"network ls --quiet --no-trunc",
		"stop --time 10 aaa bbb",
		"rm --force --volumes aaa bbb",
		"network rm n1",
	}
	if diff := cmp.Diff(want, readArgs(t, args)); diff != "" {
		t.Errorf("docker args diff (-want +got):\n%s", diff)
	}
}

const inspectOutput = `[
  {
    "Id": "aaa",
    "Name": "/itest-db",
    "Config": {
      "Image": "postgres:16",
      "Labels": {"com.buildkite.job-id": "job-1"},
      "Env": ["SECRET=hunter2"],
      "Cmd": ["postgres", "--password=hunter2"]
    },
    "Mounts": [
      {"Type": "bind", "Source": "/builds/agent-1/org/p", "Destination": "/src"},
      {"Type": "volume", "Name": "pgdata", "Source": "/var/lib/docker/volumes/pgdata/_data", "Destination": "/data"}
    ]
  }
]`

func TestCLI_Inspect(t *testing.T) {
	t.Parallel()

	c, args := fakeCLI(t, "cat <<'EOF'\n"+inspectOutput+"\nEOF\n")
	got, err := c.Inspect(t.Context(), []string{"aaa"})
	if err != nil {
		t.Fatalf("c.Inspect() error = %v", err)
	}
	want := []Container{{
		ID:          "aaa",
		Name:        "itest-db",
		Image:       "postgres:16",
		Labels:      map[string]string{"com.buildkite.job-id": "job-1"},
		BindSources: []string{"/builds/agent-1/org/p"},
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("c.Inspect() diff (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"container inspect aaa"}, readArgs(t, args)); diff != "" {
		t.Errorf("docker args diff (-want +got):\n%s", diff)
	}
}

// Docker exits 1 when any container has gone, but still describes the rest.
func TestCLI_InspectToleratesContainersThatHaveGone(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		stdout  string
		gone    int
		wantIDs []string
	}{
		"some gone": {stdout: inspectOutput, gone: 1, wantIDs: []string{"aaa"}},
		"all gone":  {stdout: "[]", gone: 1, wantIDs: nil},
		// Enough to overflow maxStderr, as when a compose project exits.
		"many gone": {stdout: inspectOutput, gone: 40, wantIDs: []string{"aaa"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c, _ := fakeCLI(t, "cat <<'EOF'\n"+test.stdout+"\nEOF\n"+
				"i=0; while [ $i -lt "+strconv.Itoa(test.gone)+" ]; do "+
				"echo \"Error response from daemon: No such container: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd$i\" >&2; "+
				"i=$((i+1)); done\nexit 1\n")
			got, err := c.Inspect(t.Context(), []string{"aaa", "bbb"})
			if err != nil {
				t.Fatalf("c.Inspect() error = %v", err)
			}
			var ids []string
			for _, g := range got {
				ids = append(ids, g.ID)
			}
			if diff := cmp.Diff(test.wantIDs, ids); diff != "" {
				t.Errorf("c.Inspect() IDs diff (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCLI_Errors(t *testing.T) {
	t.Parallel()

	for name, script := range map[string]string{
		"daemon down": "echo 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock' >&2\nexit 1\n",
		"partial output from another failure": "cat <<'EOF'\n" + inspectOutput + "\nEOF\n" +
			"echo 'Error response from daemon: No such container: bbb' >&2\n" +
			"echo 'permission denied while trying to connect' >&2\nexit 1\n",
		"garbage":                   "echo not json\n",
		"failure without a message": "echo '[]'\nexit 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c, _ := fakeCLI(t, script)
			if got, err := c.Inspect(t.Context(), []string{"aaa", "bbb"}); err == nil {
				t.Errorf("c.Inspect() = %v, nil, want an error", got)
			}
		})
	}

	c, _ := fakeCLI(t, "echo 'Cannot connect to the Docker daemon' >&2\nexit 1\n")
	_, err := c.RunningContainers(t.Context())
	if err == nil || !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Errorf("c.RunningContainers() error = %v, want it to include docker's stderr", err)
	}
}

func TestCLI_MissingCommand(t *testing.T) {
	t.Parallel()

	c := CLI{Path: filepath.Join(t.TempDir(), "no-docker-here")}
	if _, err := c.RunningContainers(t.Context()); err == nil {
		t.Error("c.RunningContainers() error = nil, want an error for a missing docker command")
	}
}
