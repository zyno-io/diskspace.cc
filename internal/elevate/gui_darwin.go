// SPDX-License-Identifier: MIT

//go:build darwin

package elevate

import (
	"fmt"
	"os/exec"
	"strings"
)

// SpawnAdminDarwin launches self+args as admin in the background through the
// macOS authorization dialog. Arguments travel via osascript argv only.
func SpawnAdminDarwin(self string, args []string) error {
	out, err := exec.Command("osascript", BuildOsascriptArgs(self, args)...).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "(-128)") {
			return ErrCancelled
		}
		return fmt.Errorf("osascript: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
