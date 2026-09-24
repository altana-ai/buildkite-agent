//go:build !linux

package process

import "errors"

func (p *Process) setupCgroup() error {
	return errors.New("starting a process in a cgroup is only supported on Linux")
}
