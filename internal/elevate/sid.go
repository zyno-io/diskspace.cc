// SPDX-License-Identifier: MIT

package elevate

import (
	"os/user"
	"runtime"
)

// UserSID returns the current user's SID on Windows ("" elsewhere). A
// standard user passes it to an elevated agent, which may run as a different
// (administrator) account, so deletions go to the right Recycle Bin.
func UserSID() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	if u, err := user.Current(); err == nil {
		return u.Uid // the SID string on Windows
	}
	return ""
}
