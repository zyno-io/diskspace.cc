// SPDX-License-Identifier: MIT

//go:build !windows

package browser

import "os/exec"

func normalPriority(*exec.Cmd) {}
