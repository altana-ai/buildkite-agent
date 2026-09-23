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

// agentGroupName is the leaf the agent moves itself into. cgroup v2 forbids a
// group that has processes of its own from enabling controllers for its
// children, so the agent must leave its unit's group for that group to be
// able to limit jobs later.
const agentGroupName = "agent"

const jobGroupPrefix = "job-"

// Setup prepares the agent's own cgroup for job groups: it moves the agent
// into a leaf group, kills any job groups left by a previous agent process,
// and checks that a process can be started directly into a new group. It
// returns nil when mode is off, and also, after logging one warning, when the
// host cannot support job groups.
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
	return m
}

func setup(mode Mode) (*Manager, error) {
	own, err := ownCgroup()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(cgroupFS, own)

	// cgroup.kill arrived in Linux 5.14. Without it, killing a group means
	// racing its forks with per-process signals.
	if _, err := os.Stat(filepath.Join(root, "cgroup.kill")); err != nil {
		return nil, fmt.Errorf("cgroup v2 with cgroup.kill (Linux 5.14 or later) is required: %w", err)
	}

	agentGroup := filepath.Join(root, agentGroupName)
	if err := os.Mkdir(agentGroup, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("creating %s, so the agent's cgroup is probably not delegated to it (systemd Delegate=yes): %w", agentGroup, err)
	}
	if err := writeFile(filepath.Join(agentGroup, "cgroup.procs"), strconv.Itoa(os.Getpid())); err != nil {
		return nil, fmt.Errorf("moving the agent into %s: %w", agentGroup, err)
	}

	m := NewManager(mode, root)

	// A unit with KillMode=process leaves a stopped agent's job groups
	// running, so a restarted agent must not assume its subtree is empty.
	if err := m.KillAll(); err != nil {
		return nil, fmt.Errorf("killing job groups left by a previous agent: %w", err)
	}

	if err := m.probe(); err != nil {
		return nil, fmt.Errorf("starting a process directly into a new cgroup: %w", err)
	}
	return m, nil
}

// ownCgroup returns this process's cgroup v2 path, relative to cgroupFS.
func ownCgroup() (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
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

// KillAll kills and removes every job group under the manager's root. The
// agent calls it at start-up and again as it exits.
func (m *Manager) KillAll() error {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || e.Name() == agentGroupName {
			continue
		}
		if !strings.HasPrefix(e.Name(), jobGroupPrefix) && e.Name() != "probe" {
			continue
		}
		g := &Group{path: filepath.Join(m.root, e.Name())}
		if err := g.Kill(DrainTimeout); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", g.path, err))
		}
	}
	return errors.Join(errs...)
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
		if err != nil || !d.IsDir() {
			return err
		}
		data, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
		if err != nil {
			return err
		}
		for field := range strings.FieldsSeq(string(data)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				continue
			}
			procs = append(procs, Process{PID: pid, Command: commandLine(pid)})
		}
		return nil
	})
	return procs, err
}

// commandLine returns a process's argv joined by spaces, or its bracketed
// name when argv is unavailable, as ps does.
func commandLine(pid int) string {
	proc := filepath.Join("/proc", strconv.Itoa(pid))
	if cmdline, err := os.ReadFile(filepath.Join(proc, "cmdline")); err == nil && len(cmdline) > 0 {
		return string(bytes.TrimRight(bytes.ReplaceAll(cmdline, []byte{0}, []byte{' '}), " "))
	}
	if comm, err := os.ReadFile(filepath.Join(proc, "comm")); err == nil {
		return "[" + strings.TrimSpace(string(comm)) + "]"
	}
	return "?"
}

// Kill sends SIGKILL to every process in the group and its descendant groups,
// waits up to timeout for the group to empty, then removes it. It returns
// ErrNotEmpty if the group is still populated at the deadline, and leaves the
// group in place so that a later Kill can try again.
func (g *Group) Kill(timeout time.Duration) error {
	if err := writeFile(filepath.Join(g.path, "cgroup.kill"), "1"); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := waitEmpty(g.path, timeout); err != nil {
		return err
	}
	return removeTree(g.path)
}

func waitEmpty(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	interval := time.Millisecond
	for {
		populated, err := isPopulated(path)
		if err != nil {
			return err
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
func removeTree(path string) error {
	entries, err := os.ReadDir(path)
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
	return unix.Rmdir(path)
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
