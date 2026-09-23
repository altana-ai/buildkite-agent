//go:build !linux

package jobcgroup

import (
	"errors"
	"time"

	"github.com/buildkite/agent/v3/logger"
)

var errUnsupported = errors.New("job-cgroup is only supported on Linux")

// Setup logs one warning and returns nil unless mode is off: cgroups exist
// only on Linux.
func Setup(l logger.Logger, mode Mode) *Manager {
	if mode != ModeOff {
		l.Warnf("job-cgroup=%s is unavailable, so jobs will run as if it were off: %v", mode, errUnsupported)
	}
	return nil
}

func (m *Manager) KillAll(time.Duration) error   { return errUnsupported }
func (m *Manager) Create(string) (*Group, error) { return nil, errUnsupported }
func (g *Group) FD() int                         { return -1 }
func (g *Group) Close() error                    { return nil }
func (g *Group) Processes() ([]Process, error)   { return nil, errUnsupported }
func (g *Group) Kill(time.Duration) error        { return errUnsupported }
