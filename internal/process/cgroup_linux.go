//go:build linux

package process

import "syscall"

// setupCgroup must run after setupProcessGroup, which replaces SysProcAttr.
// StartPTY keeps these fields when it adds Setsid and Setctty.
func (p *Process) setupCgroup() error {
	if p.command.SysProcAttr == nil {
		p.command.SysProcAttr = &syscall.SysProcAttr{}
	}
	p.command.SysProcAttr.UseCgroupFD = true
	p.command.SysProcAttr.CgroupFD = p.conf.CgroupFD
	return nil
}
