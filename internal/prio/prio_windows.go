// SPDX-License-Identifier: MIT

package prio

import "golang.org/x/sys/windows"

// applyProcessLow sets BELOW_NORMAL_PRIORITY_CLASS. PROCESS_MODE_BACKGROUND is
// avoided: it throttles I/O so hard that scans crawl.
// applyProcessLow never raises a class the user chose (start /low).
func applyProcessLow() error {
	switch c, err := windows.GetPriorityClass(windows.CurrentProcess()); {
	case err == nil && (c == windows.IDLE_PRIORITY_CLASS || c == windows.BELOW_NORMAL_PRIORITY_CLASS):
		return nil
	}
	return windows.SetPriorityClass(windows.CurrentProcess(), windows.BELOW_NORMAL_PRIORITY_CLASS)
}

func lockThreadLow() error { return nil }
