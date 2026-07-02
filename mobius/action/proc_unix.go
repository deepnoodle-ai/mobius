//go:build unix

package action

import (
	"os/exec"
	"syscall"
	"time"
)

// configureProcessGroup runs cmd in its own process group and kills the whole
// group when the command's context is cancelled. exec.CommandContext's default
// cancel signals only the direct child, so grandchildren spawned by a
// timed-out `environment.bash` (or a git helper) would otherwise keep running
// in the sandbox. WaitDelay unblocks Wait when a leaked grandchild holds the
// stdout/stderr pipes open after the parent exits.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 10 * time.Second
}
