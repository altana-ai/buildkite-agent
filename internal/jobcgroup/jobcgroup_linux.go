//go:build linux

package jobcgroup

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/buildkite/agent/v3/logger"
	"golang.org/x/sys/unix"
)

const cgroupFS = "/sys/fs/cgroup"

// ownerGroupPrefix names the subtree each agent process creates in its own
// cgroup, suffixed with its pid. Several agents can start in one unit or
// scope, so each must find its own job groups apart from the others'.
const ownerGroupPrefix = "agent-"

// agentGroupName is the leaf the agent moves itself into, inside its
// subtree. cgroup v2 forbids a group that has processes of its own from
// enabling controllers for its children, so the agent must leave the groups
// above its jobs for those groups to be able to limit jobs later.
const agentGroupName = "agent"

const jobGroupPrefix = "job-"

// Setup prepares the agent's own cgroup for job groups: it moves the agent
// into a subtree of its own, checks that a process can be started directly
// into a new group, and kills the job groups of agents that have exited. It
// returns nil when mode is off, and also, after logging one warning, when the
// host cannot support job groups.
//
// In enforce mode, the returned Manager is already tainted if an exited
// agent's processes survive being killed.
func Setup(l logger.Logger, mode Mode) *Manager {
	if mode == ModeOff {
		return nil
	}
	m, err := setup(mode)
	if err != nil {
		l.Warnf("job-cgroup=%s is unavailable, so jobs will run as if it were off: %v", mode, err)
		return nil
	}
	l.Infof("Each job will run in its own cgroup under %s (job-cgroup=%s)", m.root, mode)

	// A unit with KillMode=process leaves a stopped agent's job groups
	// running, so a restarted agent must not assume its cgroup is clean.
	if err := m.killExited(filepath.Dir(m.root), DrainTimeout); err != nil {
		l.Errorf("Couldn't kill every job cgroup left by an agent that has exited: %v", err)
	}
	return m
}

func setup(mode Mode) (*Manager, error) {
	own, err := cgroupOf("self")
	if err != nil {
		return nil, err
	}
	parent := filepath.Join(cgroupFS, own)

	// cgroup.kill arrived in Linux 5.14. Without it, killing a group means
	// racing its forks with per-process signals.
	if _, err := os.Stat(filepath.Join(parent, "cgroup.kill")); err != nil {
		return nil, fmt.Errorf("cgroup v2 with cgroup.kill (Linux 5.14 or later) is required: %w", err)
	}

	pid := os.Getpid()
	root := filepath.Join(parent, ownerGroupPrefix+strconv.Itoa(pid))
	agentGroup := filepath.Join(root, agentGroupName)
	for _, dir := range []string{root, agentGroup} {
		if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("creating %s, so the agent's cgroup is probably not delegated to it (systemd Delegate=pids): %w", dir, err)
		}
	}
	if err := writeFile(filepath.Join(agentGroup, "cgroup.procs"), strconv.Itoa(pid)); err != nil {
		return nil, fmt.Errorf("moving the agent into %s: %w", agentGroup, err)
	}

	m := NewManager(mode, root)
	if err := m.probe(); err != nil {
		return nil, fmt.Errorf("starting a process directly into a new cgroup: %w", err)
	}
	return m, nil
}

// cgroupOf returns a process's cgroup v2 path, relative to cgroupFS. pid is a
// number, or "self".
func cgroupOf(pid string) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", pid, "cgroup"))
	if err != nil {
		return "", err
	}
	// On the unified hierarchy the only line is "0::<path>".
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path, nil
		}
	}
	return "", errors.New("the agent is not on a cgroup v2 unified hierarchy")
}

// probe starts the agent's own binary into a throwaway group, because a
// seccomp policy or an old kernel can reject clone3 even where cgroup v2 works.
func (m *Manager) probe() error {
	g, err := m.create("probe")
	if err != nil {
		return err
	}
	defer g.Close() //nolint:errcheck // Best-effort cleanup; Kill reports what matters.

	// Only starting the process is under test, so its exit status is not.
	cmd := exec.Command("/proc/self/exe", "--version")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: g.FD()}
	var exitErr *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &exitErr) {
		return errors.Join(err, g.Kill(DrainTimeout))
	}
	return g.Kill(DrainTimeout)
}

// KillAll kills and removes every job group this agent process has created,
// waiting up to timeout for them to empty. Other agents' groups are left
// alone. The agent calls it as it exits.
func (m *Manager) KillAll(timeout time.Duration) error {
	paths, err := m.jobGroups()
	if err != nil {
		return err
	}
	errs := killGroups(paths, timeout)
	return joinErrors(paths, errs)
}

// jobGroups returns the paths of the groups m has created for jobs and for
// its probe.
func (m *Manager) jobGroups() ([]string, error) {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() && (strings.HasPrefix(e.Name(), jobGroupPrefix) || e.Name() == "probe") {
			paths = append(paths, filepath.Join(m.root, e.Name()))
		}
	}
	return paths, nil
}

// killExited kills and removes the subtree of every agent under parent whose
// process has exited, and any job groups already in m's own subtree. A group
// whose processes survive is kept, and in enforce mode taints m, because a
// job's processes have survived SIGKILL on this host just as if one of m's
// own jobs had left them.
func (m *Manager) killExited(parent string, timeout time.Duration) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	var paths []string
	for _, e := range entries {
		path := filepath.Join(parent, e.Name())
		pid, ok := ownerPID(e)
		switch {
		case !ok:
		case path == m.root:
			// m has run no job yet, so these were left by an earlier agent
			// with the same pid, as an agent that is always the same pid in
			// its container would be.
			own, err := m.jobGroups()
			if err != nil {
				return err
			}
			paths = append(paths, own...)
		case !ownerRunning(parent, pid):
			paths = append(paths, path)
		}
	}

	errs := killGroups(paths, timeout)
	for _, err := range errs {
		if errors.Is(err, ErrNotEmpty) && m.mode == ModeEnforce {
			m.Taint()
		}
	}
	return joinErrors(paths, errs)
}

// killGroups kills every group at once, then waits for them against one
// deadline, so that several stuck groups cost one timeout between them
// rather than one each. It returns the error for each group it could not
// kill and remove.
func killGroups(paths []string, timeout time.Duration) map[string]error {
	deadline := time.Now().Add(timeout)
	errs := make(map[string]error)
	var killed []string
	for _, path := range paths {
		err := writeFile(filepath.Join(path, "cgroup.kill"), "1")
		switch {
		case err == nil:
			killed = append(killed, path)
		case errors.Is(err, fs.ErrNotExist):
			// Another caller has already removed the group.
		default:
			if populated, perr := isPopulated(path); perr == nil && !populated {
				err = removeTree(path)
			} else {
				err = errors.Join(ErrNotEmpty, err)
			}
			if err != nil {
				errs[path] = err
			}
		}
	}
	for _, path := range killed {
		if err := waitEmpty(path, time.Until(deadline)); err != nil {
			errs[path] = err
		} else if err := removeTree(path); err != nil {
			errs[path] = err
		}
	}
	return errs
}

// joinErrors joins the errors killGroups returned, each prefixed with its
// group's path, in the order of paths.
func joinErrors(paths []string, errs map[string]error) error {
	var joined []error
	for _, path := range paths {
		if err := errs[path]; err != nil {
			joined = append(joined, fmt.Errorf("%s: %w", path, err))
		}
	}
	return errors.Join(joined...)
}

// ownerPID returns the pid of the agent that created the subtree e.
func ownerPID(e fs.DirEntry) (int, bool) {
	suffix, ok := strings.CutPrefix(e.Name(), ownerGroupPrefix)
	if !e.IsDir() || !ok {
		return 0, false
	}
	pid, err := strconv.Atoi(suffix)
	return pid, err == nil && pid > 0
}

// ownerRunning reports whether the agent with pid may still be running.
// Killing a running agent's subtree kills its jobs, so any doubt counts as
// running.
func ownerRunning(parent string, pid int) bool {
	agentGroup := filepath.Join(parent, ownerGroupPrefix+strconv.Itoa(pid), agentGroupName)

	// A process in the agent group may be the owner even where /proc does
	// not show it, as when it is in another pid namespace.
	populated, err := isPopulated(agentGroup)
	if populated || err != nil && !errors.Is(err, fs.ErrNotExist) {
		return true
	}

	// Otherwise the owner can only be in parent, about to move. A later
	// process that reuses the pid is elsewhere.
	own, err := cgroupOf(strconv.Itoa(pid))
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	path := filepath.Join(cgroupFS, own)
	return path == parent || path == agentGroup
}

// Create creates the group for a job and opens it, ready to be passed to
// process.Config as a cgroup file descriptor.
func (m *Manager) Create(jobID string) (*Group, error) {
	if jobID == "" || strings.ContainsAny(jobID, "/\x00") {
		return nil, fmt.Errorf("job ID %q cannot name a cgroup", jobID)
	}
	return m.create(jobGroupPrefix + jobID)
}

func (m *Manager) create(name string) (*Group, error) {
	path := filepath.Join(m.root, name)
	// A group that already exists belongs to an earlier run of the same job
	// whose processes could not be killed. Reusing it means this run's
	// teardown tries again.
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &Group{path: path, dir: dir}, nil
}

// FD returns the group's directory file descriptor. It stays valid until
// Close.
func (g *Group) FD() int { return int(g.dir.Fd()) }

// Close closes the group's directory. The group itself stays until Kill.
func (g *Group) Close() error {
	if g.dir == nil {
		return nil
	}
	return g.dir.Close()
}

// Processes lists every process in the group and its descendant groups.
func (g *Group) Processes() ([]Process, error) {
	var procs []Process
	err := filepath.WalkDir(g.path, func(path string, d fs.DirEntry, err error) error {
		// The job can remove a group of its own while the walk is in it,
		// and then that group has nothing left to report.
		gone := func(err error) bool { return path != g.path && errors.Is(err, fs.ErrNotExist) }
		if err != nil {
			if gone(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
		if gone(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for field := range strings.FieldsSeq(string(data)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				continue
			}
			name, ppid := stat(pid)
			procs = append(procs, Process{PID: pid, PPID: ppid, Name: name})
		}
		return nil
	})
	return procs, err
}

// stat returns a process's name and its parent's pid from /proc/<pid>/stat,
// or "?" and 0 if the process has already gone.
func stat(pid int) (name string, ppid int) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "?", 0
	}
	// The name is parenthesised and can itself contain ")", so it ends at
	// the last one. The state and then the parent's pid follow it.
	start, end := bytes.IndexByte(data, '('), bytes.LastIndexByte(data, ')')
	if start < 0 || end < start {
		return "?", 0
	}
	if fields := strings.Fields(string(data[end+1:])); len(fields) >= 2 {
		ppid, _ = strconv.Atoi(fields[1])
	}
	return string(data[start+1 : end]), ppid
}

// Kill sends SIGKILL to every process in the group and its descendant groups,
// waits up to timeout for the group to empty, then removes it. It returns
// ErrNotEmpty if the group is still populated at the deadline, or cannot be
// killed or shown to be empty, and leaves the group in place so that a later
// Kill can try again.
func (g *Group) Kill(timeout time.Duration) error {
	return killGroups([]string{g.path}, timeout)[g.path]
}

func waitEmpty(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	interval := time.Millisecond
	for {
		populated, err := isPopulated(path)
		if err != nil {
			if _, serr := os.Stat(path); errors.Is(serr, fs.ErrNotExist) {
				// Another caller has already removed the group.
				return nil
			}
			// A group whose processes cannot be shown to be gone counts
			// as not empty, so that enforce mode fails closed.
			return errors.Join(ErrNotEmpty, err)
		}
		if !populated {
			return nil
		}
		if time.Now().After(deadline) {
			return ErrNotEmpty
		}
		time.Sleep(interval)
		interval = min(2*interval, 100*time.Millisecond)
	}
}

// isPopulated reads cgroup.events, whose "populated" key covers descendant
// groups too.
func isPopulated(path string) (bool, error) {
	f, err := os.Open(filepath.Join(path, "cgroup.events"))
	if err != nil {
		return false, err
	}
	defer f.Close() //nolint:errcheck // Read-only.

	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "populated "); ok {
			return v != "0", nil
		}
	}
	if err := s.Err(); err != nil {
		return false, err
	}
	return false, errors.New("cgroup.events has no populated key")
}

// removeTree removes a group and its descendants, deepest first. cgroupfs
// rejects unlinking its interface files, so os.RemoveAll cannot be used.
// A group that is already gone, removed by another caller, is not an error.
func removeTree(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := removeTree(filepath.Join(path, e.Name())); err != nil {
				return err
			}
		}
	}
	if err := unix.Rmdir(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// writeFile writes to an existing cgroup interface file. os.WriteFile would
// try to create or truncate it.
func writeFile(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(value)
	return errors.Join(werr, f.Close())
}
