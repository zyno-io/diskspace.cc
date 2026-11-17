// SPDX-License-Identifier: MIT

package prio

import "golang.org/x/sys/windows"

// applyProcessLow sets BELOW_NORMAL_PRIORITY_CLASS. PROCESS_MODE_BACKGROUND is
// avoided: it throttles I/O so hard that scans crawl.
func applyProcessLow() error {
	return windows.SetPriorityClass(windows.CurrentProcess(), windows.BELOW_NORMAL_PRIORITY_CLASS)
}

func lockThreadLow() error { return nil }
