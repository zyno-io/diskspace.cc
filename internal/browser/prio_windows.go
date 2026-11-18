// SPDX-License-Identifier: MIT

package browser

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// normalPriority starts cmd at normal priority even when this process runs
// below normal (children inherit BELOW_NORMAL otherwise).
func normalPriority(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.NORMAL_PRIORITY_CLASS
}
