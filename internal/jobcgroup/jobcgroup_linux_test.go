//go:build linux

package jobcgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testManager returns a Manager rooted in a fresh group under the test's own
// cgroup, or skips the test when that cgroup is not delegated to the user
// running it.
func testManager(t *testing.T, mode Mode) *Manager {
	t.Helper()

	own, err := ownCgroup()
	if err != nil {
		t.Skipf("no cgroup v2: %v", err)
	}
	parent := filepath.Join(cgroupFS, own)
	if _, err := os.Stat(filepath.Join(parent, "cgroup.kill")); err != nil {
		t.Skipf("no cgroup.kill: %v", err)
	}
	root := filepath.Join(parent, fmt.Sprintf("jobcgroup-test-%d-%d", os.Getpid(), time.Now().UnixNano()))
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Skipf("the test's cgroup is not delegated: %v", err)
	}

	m := NewManager(mode, root)
	t.Cleanup(func() {
		if err := m.KillAll(); err != nil {
			t.Errorf("KillAll() error = %v", err)
		}
		if err := removeTree(root); err != nil {
			t.Errorf("removeTree(%q) error = %v", root, err)
		}
	})
	return m
}

// startInGroup runs script with sh directly inside g and waits for sh to
// exit, leaving behind whatever it started.
func startInGroup(t *testing.T, g *Group, script string) {
	t.Helper()

	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: g.FD()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -c %q error = %v, output: %s", script, err, out)
	}
}

// awaitCommands waits for the group to hold exactly the given commands, since
// setsid and nohup exec their argument only after sh has returned.
func awaitCommands(t *testing.T, g *Group, want ...string) []Process {
	t.Helper()

	slices.Sort(want)
	var commands []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		procs, err := g.Processes()
		if err != nil {
			t.Fatalf("Processes() error = %v", err)
		}
		commands = commands[:0]
		for _, p := range procs {
			commands = append(commands, p.Command)
		}
		slices.Sort(commands)
		if slices.Equal(commands, want) {
			return procs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Processes() commands = %q, want %q", commands, want)
	return nil
}

// awaitDead fails the test unless pid exits within a few seconds. A zombie
// counts as dead: it runs nothing and holds no files.
func awaitDead(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		// The state follows the parenthesised command name.
		if i := strings.LastIndexByte(string(stat), ')'); i >= 0 && strings.HasPrefix(string(stat[i+1:]), " Z") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("process %d is still running", pid)
}

func TestKillRemovesEscapedProcesses(t *testing.T) {
	t.Parallel()

	m := testManager(t, ModeEnforce)
	g, err := m.Create("escapes")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer g.Close() //nolint:errcheck // Test cleanup.

	// One process starts a new session, one is double-forked so that its
	// parent is gone. Neither is in the shell's process group.
	startInGroup(t, g, `
		setsid nohup sleep 301 >/dev/null 2>&1 &
		( sleep 302 & ) >/dev/null 2>&1
	`)

	procs := awaitCommands(t, g, "sleep 301", "sleep 302")

	if err := g.Kill(DrainTimeout); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	for _, p := range procs {
		awaitDead(t, p.PID)
	}
	if _, err := os.Stat(g.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(%q) error = %v, want the group to be removed", g.Path(), err)
	}
}

func TestKillIncludesDescendantGroups(t *testing.T) {
	t.Parallel()

	m := testManager(t, ModeEnforce)
	g, err := m.Create("nested")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer g.Close() //nolint:errcheck // Test cleanup.

	child, err := m.create(filepath.Base(g.Path()) + "/child")
	if err != nil {
		t.Fatalf("create(child) error = %v", err)
	}
	defer child.Close() //nolint:errcheck // Test cleanup.
	startInGroup(t, child, `setsid nohup sleep 303 >/dev/null 2>&1 &`)

	procs := awaitCommands(t, g, "sleep 303")
	if err := g.Kill(DrainTimeout); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	awaitDead(t, procs[0].PID)
	if _, err := os.Stat(g.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(%q) error = %v, want the group and its child removed", g.Path(), err)
	}
}

func TestKillAllKillsEveryJobGroup(t *testing.T) {
	t.Parallel()

	m := testManager(t, ModeReport)
	var pids []int
	for _, id := range []string{"a", "b"} {
		g, err := m.Create(id)
		if err != nil {
			t.Fatalf("Create(%q) error = %v", id, err)
		}
		startInGroup(t, g, `setsid nohup sleep 304 >/dev/null 2>&1 &`)
		procs := awaitCommands(t, g, "sleep 304")
		pids = append(pids, procs[0].PID)
		g.Close() //nolint:errcheck // Test cleanup.
	}

	if err := m.KillAll(); err != nil {
		t.Fatalf("KillAll() error = %v", err)
	}
	for _, pid := range pids {
		awaitDead(t, pid)
	}
	entries, err := os.ReadDir(m.Root())
	if err != nil {
		t.Fatalf("os.ReadDir(%q) error = %v", m.Root(), err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("group %s remains after KillAll", e.Name())
		}
	}
}

func TestKillReportsGroupThatDoesNotEmpty(t *testing.T) {
	t.Parallel()

	// A plain directory stands in for a group whose process ignores
	// SIGKILL, which no real process can be made to do on demand.
	dir := t.TempDir()
	for name, content := range map[string]string{
		"cgroup.kill":   "",
		"cgroup.events": "populated 1\nfrozen 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("os.WriteFile(%q) error = %v", name, err)
		}
	}

	g := &Group{path: dir}
	if err := g.Kill(50 * time.Millisecond); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("Kill() error = %v, want %v", err, ErrNotEmpty)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("os.Stat(%q) error = %v, want the group kept for a later retry", dir, err)
	}
}

func TestCreateRejectsPathLikeJobIDs(t *testing.T) {
	t.Parallel()

	m := NewManager(ModeEnforce, t.TempDir())
	for _, id := range []string{"", "../a", "a/b", "a\x00b"} {
		if _, err := m.Create(id); err == nil {
			t.Errorf("Create(%q) error = nil, want an error", id)
		}
	}
}

const setupHelperEnv = "JOBCGROUP_TEST_SETUP_HELPER"

// TestSetup runs setup in a copy of the test binary started inside a group of
// its own, because setup moves the calling process.
func TestSetup(t *testing.T) {
	if os.Getenv(setupHelperEnv) != "" {
		m, err := setup(ModeEnforce)
		if err != nil {
			fmt.Println("setup error:", err)
			os.Exit(1)
		}
		own, err := ownCgroup()
		fmt.Printf("root=%s\nown=%s %v\n", m.Root(), filepath.Join(cgroupFS, own), err)
		os.Exit(0)
	}
	t.Parallel()

	m := testManager(t, ModeEnforce)
	unit, err := m.create("job-unit")
	if err != nil {
		t.Fatalf("create(unit) error = %v", err)
	}
	defer unit.Close() //nolint:errcheck // Test cleanup.

	stale, err := m.create("job-unit/job-stale")
	if err != nil {
		t.Fatalf("create(stale) error = %v", err)
	}
	startInGroup(t, stale, `setsid nohup sleep 305 >/dev/null 2>&1 &`)
	stalePID := awaitCommands(t, stale, "sleep 305")[0].PID
	stale.Close() //nolint:errcheck // Test cleanup.

	cmd := exec.Command(os.Args[0], "-test.run=^TestSetup$")
	cmd.Env = append(os.Environ(), setupHelperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: unit.FD()}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("setup helper error = %v, output: %s", err, out)
	}

	wantAgent := filepath.Join(unit.Path(), agentGroupName)
	for _, want := range []string{"root=" + unit.Path() + "\n", "own=" + wantAgent + " <nil>\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("setup helper output = %q, want it to contain %q", out, want)
		}
	}
	awaitDead(t, stalePID)
	if _, err := os.Stat(stale.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(%q) error = %v, want the stale group removed", stale.Path(), err)
	}
	if _, err := os.Stat(filepath.Join(unit.Path(), "probe")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("probe group remains after setup: %v", err)
	}
}
