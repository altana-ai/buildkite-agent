//go:build linux

package jobcgroup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/buildkite/agent/v3/logger"
)

// testManager returns a Manager rooted in a fresh group under the test's own
// cgroup, or skips the test when that cgroup is not delegated to the user
// running it.
func testManager(t *testing.T, mode Mode) *Manager {
	t.Helper()

	own, err := cgroupOf("self")
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
		if err := m.KillAll(DrainTimeout); err != nil {
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

// awaitNames waits for the group to hold processes with exactly the given
// names, since setsid and nohup exec their argument only after sh has
// returned.
func awaitNames(t *testing.T, g *Group, want ...string) []Process {
	t.Helper()

	slices.Sort(want)
	var names []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		procs, err := g.Processes()
		if err != nil {
			t.Fatalf("Processes() error = %v", err)
		}
		names = names[:0]
		for _, p := range procs {
			names = append(names, p.Name)
		}
		slices.Sort(names)
		if slices.Equal(names, want) {
			return procs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Processes() names = %q, want %q", names, want)
	return nil
}

// alive reports whether pid is running. A zombie is not: it runs nothing and
// holds no files.
func alive(pid int) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	// The state follows the parenthesised command name.
	i := strings.LastIndexByte(string(stat), ')')
	return i < 0 || !strings.HasPrefix(string(stat[i+1:]), " Z")
}

// awaitDead fails the test unless pid exits within a few seconds.
func awaitDead(t *testing.T, pid int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
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

	procs := awaitNames(t, g, "sleep", "sleep")

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

	procs := awaitNames(t, g, "sleep")
	if err := g.Kill(DrainTimeout); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	awaitDead(t, procs[0].PID)
	if _, err := os.Stat(g.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(%q) error = %v, want the group and its child removed", g.Path(), err)
	}
}

func TestProcessesReportsNamesAndParentsButNotArguments(t *testing.T) {
	t.Parallel()

	m := testManager(t, ModeReport)
	g, err := m.Create("secretive")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	defer g.Close() //nolint:errcheck // Test cleanup.

	// A hook can export a secret the agent never sees, and pass it on the
	// command line of a process the job leaves behind.
	const secret = "s3cret-from-a-hook"
	startInGroup(t, g, `setsid nohup sh -c 'sleep 308' leaky --token=`+secret+` >/dev/null 2>&1 &`)

	procs := awaitNames(t, g, "sh", "sleep")
	if got := fmt.Sprintf("%+v", procs); strings.Contains(got, secret) {
		t.Errorf("Processes() = %s, want no arguments in it", got)
	}
	byName := make(map[string]Process)
	for _, p := range procs {
		byName[p.Name] = p
	}
	if got, want := byName["sleep"].PPID, byName["sh"].PID; got != want {
		t.Errorf("sleep's PPID = %d, want sh's PID %d", got, want)
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
		procs := awaitNames(t, g, "sleep")
		pids = append(pids, procs[0].PID)
		g.Close() //nolint:errcheck // Test cleanup.
	}

	if err := m.KillAll(DrainTimeout); err != nil {
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

// fakeStuckGroup makes a plain directory that stands in for a group whose
// process ignores SIGKILL, which no real process can be made to do on demand.
func fakeStuckGroup(t *testing.T, dir string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("os.MkdirAll(%q) error = %v", dir, err)
	}
	for name, content := range map[string]string{
		"cgroup.kill":   "",
		"cgroup.events": "populated 1\nfrozen 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("os.WriteFile(%q) error = %v", name, err)
		}
	}
}

func TestKillReportsGroupThatDoesNotEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeStuckGroup(t, dir)

	g := &Group{path: dir}
	if err := g.Kill(50 * time.Millisecond); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("Kill() error = %v, want %v", err, ErrNotEmpty)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("os.Stat(%q) error = %v, want the group kept for a later retry", dir, err)
	}
}

func TestKillFailsClosedWhenTheGroupCannotBeKilled(t *testing.T) {
	t.Parallel()

	// cgroup.kill as a directory makes the write fail, as EACCES would.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "cgroup.kill"), 0o755); err != nil {
		t.Fatalf("os.Mkdir(cgroup.kill) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.events"), []byte("populated 1\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(cgroup.events) error = %v", err)
	}

	if err := (&Group{path: dir}).Kill(time.Second); !errors.Is(err, ErrNotEmpty) {
		t.Errorf("Kill() error = %v, want %v", err, ErrNotEmpty)
	}
}

func TestKillingAGroupAnotherCallerRemovedIsNotAnError(t *testing.T) {
	t.Parallel()

	gone := filepath.Join(t.TempDir(), "gone")
	if err := waitEmpty(gone, time.Second); err != nil {
		t.Errorf("waitEmpty() error = %v, want nil", err)
	}
	if err := removeTree(gone); err != nil {
		t.Errorf("removeTree() error = %v, want nil", err)
	}
	if err := (&Group{path: gone}).Kill(time.Second); err != nil {
		t.Errorf("Kill() error = %v, want nil", err)
	}
}

func TestKillExitedLeavesASubtreeWhoseAgentGroupHasProcesses(t *testing.T) {
	t.Parallel()

	// The owner is out of sight in /proc, as from another pid namespace,
	// but its agent group still has a process.
	parent := t.TempDir()
	m := NewManager(ModeEnforce, filepath.Join(parent, ownerGroupPrefix+strconv.Itoa(os.Getpid())))
	other := filepath.Join(parent, ownerGroupPrefix+strconv.Itoa(exitedAgentPID))
	fakeStuckGroup(t, other)
	fakeStuckGroup(t, filepath.Join(other, agentGroupName))

	if err := m.killExited(parent, 50*time.Millisecond); err != nil {
		t.Errorf("killExited() error = %v, want nil", err)
	}
	if kill, err := os.ReadFile(filepath.Join(other, "cgroup.kill")); err != nil || len(kill) > 0 {
		t.Errorf("cgroup.kill = %q, %v, want the running agent's subtree left alone", kill, err)
	}
}

// exitedAgentPID is above the kernel's largest pid, so no agent with it can
// be running.
const exitedAgentPID = 99999999

func TestKillExitedTaintsEnforceWhenAnExitedAgentsGroupDoesNotEmpty(t *testing.T) {
	t.Parallel()

	for mode, wantTainted := range map[Mode]bool{ModeEnforce: true, ModeReport: false} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			parent := t.TempDir()
			m := NewManager(mode, filepath.Join(parent, ownerGroupPrefix+strconv.Itoa(os.Getpid())))
			fakeStuckGroup(t, filepath.Join(parent, ownerGroupPrefix+strconv.Itoa(exitedAgentPID)))

			if err := m.killExited(parent, 50*time.Millisecond); !errors.Is(err, ErrNotEmpty) {
				t.Errorf("killExited() error = %v, want %v", err, ErrNotEmpty)
			}
			select {
			case <-m.Tainted():
				if !wantTainted {
					t.Error("the manager is tainted, want it untainted in report mode")
				}
			default:
				if wantTainted {
					t.Error("the manager is not tainted, want it tainted in enforce mode")
				}
			}
		})
	}
}

func TestKillExitedKillsJobGroupsLeftByAnAgentWithTheSamePID(t *testing.T) {
	t.Parallel()

	parent := testManager(t, ModeEnforce).Root()
	m := NewManager(ModeEnforce, filepath.Join(parent, ownerGroupPrefix+strconv.Itoa(os.Getpid())))
	if err := os.Mkdir(m.Root(), 0o755); err != nil {
		t.Fatalf("os.Mkdir(%q) error = %v", m.Root(), err)
	}
	t.Cleanup(func() {
		if err := removeTree(m.Root()); err != nil {
			t.Errorf("removeTree(%q) error = %v", m.Root(), err)
		}
	})
	old, err := m.Create("old")
	if err != nil {
		t.Fatalf("Create(old) error = %v", err)
	}
	startInGroup(t, old, `setsid nohup sleep 309 >/dev/null 2>&1 &`)
	pid := awaitNames(t, old, "sleep")[0].PID
	old.Close() //nolint:errcheck // Test cleanup.

	if err := m.killExited(parent, DrainTimeout); err != nil {
		t.Fatalf("killExited() error = %v", err)
	}
	awaitDead(t, pid)
	if _, err := os.Stat(old.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(%q) error = %v, want the earlier agent's job group removed", old.Path(), err)
	}
}

func TestKillAllWaitsForStuckGroupsTogether(t *testing.T) {
	t.Parallel()

	m := NewManager(ModeEnforce, t.TempDir())
	for _, id := range []string{"a", "b", "c"} {
		fakeStuckGroup(t, filepath.Join(m.Root(), jobGroupPrefix+id))
	}

	const timeout = time.Second
	start := time.Now()
	err := m.KillAll(timeout)
	if elapsed := time.Since(start); elapsed > 2*timeout {
		t.Errorf("KillAll() took %v, want about one %v timeout for all three groups", elapsed, timeout)
	}
	if !errors.Is(err, ErrNotEmpty) {
		t.Errorf("KillAll() error = %v, want %v", err, ErrNotEmpty)
	}
}

func TestProcessesSkipsAGroupRemovedDuringTheWalk(t *testing.T) {
	t.Parallel()

	// A plain directory stands in for the job's group. Its subdirectory has
	// no cgroup.procs, as a group that the job removed mid-walk would not.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(cgroup.procs) error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "gone"), 0o755); err != nil {
		t.Fatalf("os.Mkdir(gone) error = %v", err)
	}

	procs, err := (&Group{path: dir}).Processes()
	if err != nil {
		t.Fatalf("Processes() error = %v", err)
	}
	if len(procs) != 1 || procs[0].PID != os.Getpid() {
		t.Errorf("Processes() = %+v, want only this process", procs)
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

// helperEnv makes the test binary act as an agent process instead of running
// tests, because Setup moves the process that calls it. Its value is the part
// to play:
//   - "setup" calls Setup, prints the result, then kills its job groups as
//     the agent does when it exits.
//   - "running-job" calls Setup, leaves one job's process running, prints
//     "ready", and then waits for its stdin to close.
const helperEnv = "JOBCGROUP_TEST_HELPER"

func init() {
	role := os.Getenv(helperEnv)
	if role == "" {
		return
	}
	// Setup's probe runs this binary again, which must not play a part too.
	os.Unsetenv(helperEnv) //nolint:errcheck // It cannot fail for a valid name.

	switch role {
	case "setup":
		m := Setup(logger.Discard, ModeEnforce)
		if m == nil {
			fmt.Println("Setup returned nil")
			os.Exit(1)
		}
		own, err := cgroupOf("self")
		fmt.Printf("root=%s\nown=%s %v\n", m.Root(), filepath.Join(cgroupFS, own), err)
		if err := m.KillAll(DrainTimeout); err != nil {
			fmt.Println("KillAll error:", err)
			os.Exit(1)
		}

	case "running-job":
		m := Setup(logger.Discard, ModeEnforce)
		if m == nil {
			fmt.Println("Setup returned nil")
			os.Exit(1)
		}
		g, err := m.Create("running")
		if err != nil {
			fmt.Println("Create error:", err)
			os.Exit(1)
		}
		cmd := exec.Command("/bin/sh", "-c", `setsid nohup sleep 306 >/dev/null 2>&1 &`)
		cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: g.FD()}
		if err := cmd.Run(); err != nil {
			fmt.Println("starting the job error:", err)
			os.Exit(1)
		}
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	os.Exit(0)
}

// helper returns an agent process in the part named by role, started inside
// unit.
func helper(unit *Group, role string) *exec.Cmd {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"="+role)
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: unit.FD()}
	return cmd
}

// runSetup runs Setup in a new agent process inside unit, and returns what it
// printed and its pid.
func runSetup(t *testing.T, unit *Group) (string, int) {
	t.Helper()

	cmd := helper(unit, "setup")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("setup helper error = %v, output: %s", err, out)
	}
	return string(out), cmd.Process.Pid
}

// testUnit returns a group standing in for the systemd unit agents run in.
// Its name makes testManager's cleanup kill whatever is left in it.
func testUnit(t *testing.T) *Group {
	t.Helper()

	unit, err := testManager(t, ModeEnforce).Create("unit")
	if err != nil {
		t.Fatalf("Create(unit) error = %v", err)
	}
	t.Cleanup(func() { unit.Close() }) //nolint:errcheck // Test cleanup.
	return unit
}

func TestSetup(t *testing.T) {
	t.Parallel()

	unit := testUnit(t)
	exited := &Manager{root: filepath.Join(unit.Path(), ownerGroupPrefix+strconv.Itoa(exitedAgentPID))}
	if err := os.Mkdir(exited.root, 0o755); err != nil {
		t.Fatalf("os.Mkdir(%q) error = %v", exited.root, err)
	}
	stale, err := exited.Create("stale")
	if err != nil {
		t.Fatalf("Create(stale) error = %v", err)
	}
	startInGroup(t, stale, `setsid nohup sleep 305 >/dev/null 2>&1 &`)
	stalePID := awaitNames(t, stale, "sleep")[0].PID
	stale.Close() //nolint:errcheck // Test cleanup.

	out, pid := runSetup(t, unit)

	wantRoot := filepath.Join(unit.Path(), ownerGroupPrefix+strconv.Itoa(pid))
	wantAgent := filepath.Join(wantRoot, agentGroupName)
	for _, want := range []string{"root=" + wantRoot + "\n", "own=" + wantAgent + " <nil>\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("setup helper output = %q, want it to contain %q", out, want)
		}
	}
	awaitDead(t, stalePID)
	if _, err := os.Stat(filepath.Dir(stale.Path())); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(%q) error = %v, want the exited agent's groups removed", filepath.Dir(stale.Path()), err)
	}
	if _, err := os.Stat(filepath.Join(wantRoot, "probe")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("probe group remains after setup: %v", err)
	}
}

func TestSetupAndExitLeaveOtherRunningAgentsJobsAlone(t *testing.T) {
	t.Parallel()

	unit := testUnit(t)

	running := helper(unit, "running-job")
	stdin, err := running.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe() error = %v", err)
	}
	stdout, err := running.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	if err := running.Start(); err != nil {
		t.Fatalf("starting the running-job helper: %v", err)
	}
	t.Cleanup(func() {
		stdin.Close()  //nolint:errcheck // Test cleanup.
		running.Wait() //nolint:errcheck // Test cleanup.
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); line != "ready\n" {
		t.Fatalf("running-job helper printed %q, %v, want %q", line, err, "ready\n")
	}
	job := &Group{path: filepath.Join(unit.Path(), ownerGroupPrefix+strconv.Itoa(running.Process.Pid), jobGroupPrefix+"running")}
	jobPID := awaitNames(t, job, "sleep")[0].PID

	// A second agent in the same unit starts and then exits.
	_, otherPID := runSetup(t, unit)
	if !alive(jobPID) {
		t.Fatalf("process %d was killed by another agent, want a running agent's job left alone", jobPID)
	}

	// Once its agent has exited, the job's process is a leftover.
	stdin.Close()  //nolint:errcheck // Its error is the Wait's.
	running.Wait() //nolint:errcheck // Only its exit matters.
	_, lastPID := runSetup(t, unit)
	awaitDead(t, jobPID)

	for _, pid := range []int{running.Process.Pid, otherPID} {
		path := filepath.Join(unit.Path(), ownerGroupPrefix+strconv.Itoa(pid))
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("os.Stat(%q) error = %v, want the exited agent's groups removed", path, err)
		}
	}
	path := filepath.Join(unit.Path(), ownerGroupPrefix+strconv.Itoa(lastPID))
	if _, err := os.Stat(path); err != nil {
		t.Errorf("os.Stat(%q) error = %v, want the last agent's group kept until another agent starts", path, err)
	}
}
