// Package jobcgroup runs each job's bootstrap in its own cgroup v2 group, so
// that when the job ends the agent can list and kill every process the job
// started, including ones that left the job's process group or session.
//
// It is intended for internal use by buildkite-agent only.
package jobcgroup

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Mode is the value of the job-cgroup agent setting.
type Mode string

const (
	// ModeOff runs jobs exactly as the agent always has.
	ModeOff Mode = "off"

	// ModeReport lists and kills leftover processes only after the job's
	// result has been reported, and never stops the agent.
	ModeReport Mode = "report"

	// ModeEnforce kills leftover processes before the job is marked finished,
	// and stops the agent accepting jobs if they cannot all be killed.
	ModeEnforce Mode = "enforce"
)

// DrainTimeout bounds how long a killed group may take to empty. It is well
// above the time the kernel needs to deliver SIGKILL, so exceeding it means a
// process is stuck, typically in uninterruptible sleep.
const DrainTimeout = 30 * time.Second

// ErrNotEmpty is returned by Group.Kill when processes remain in the group
// after DrainTimeout.
var ErrNotEmpty = errors.New("job cgroup still has processes after being killed")

// ParseMode parses the job-cgroup setting. The empty string means off.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case "":
		return ModeOff, nil
	case ModeOff, ModeReport, ModeEnforce:
		return m, nil
	default:
		return "", fmt.Errorf("invalid job-cgroup mode %q, must be one of %q, %q or %q", s, ModeOff, ModeReport, ModeEnforce)
	}
}

// Process is a process found in a job's group after its bootstrap exited.
type Process struct {
	PID     int
	Command string
}

// Group is one job's cgroup.
type Group struct {
	path string
	dir  *os.File
}

// Path returns the group's directory.
func (g *Group) Path() string { return g.path }

// Manager creates a group for each job under a root group that the agent
// owns. A nil *Manager means job-cgroup is off.
type Manager struct {
	mode Mode
	root string

	taintOnce sync.Once
	tainted   chan struct{}
}

// NewManager returns a Manager that creates job groups under root, which must
// be a cgroup v2 directory the agent can write to. Unlike Setup, it does not
// move the agent's own process.
func NewManager(mode Mode, root string) *Manager {
	return &Manager{mode: mode, root: root, tainted: make(chan struct{})}
}

// Mode returns the manager's mode.
func (m *Manager) Mode() Mode { return m.mode }

// Root returns the directory that job groups are created under.
func (m *Manager) Root() string { return m.root }

// Taint records that a job's processes could not all be killed. In enforce
// mode the agent stops accepting jobs once the manager is tainted.
func (m *Manager) Taint() {
	m.taintOnce.Do(func() { close(m.tainted) })
}

// Tainted returns a channel that is closed once Taint has been called.
func (m *Manager) Tainted() <-chan struct{} { return m.tainted }
