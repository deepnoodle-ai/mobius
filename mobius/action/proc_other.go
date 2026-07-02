//go:build !unix

package action

import (
	"os/exec"
	"time"
)

// configureProcessGroup has no process-group support here; the stdlib default
// (kill the direct child on context cancellation) applies. WaitDelay still
// unblocks Wait when an orphaned grandchild holds the output pipes open.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.WaitDelay = 10 * time.Second
}
