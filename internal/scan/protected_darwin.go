// SPDX-License-Identifier: MIT

//go:build darwin

package scan

import (
	"strings"

	"golang.org/x/sys/unix"
)

var systemRoots = []string{
	"/", "/System", "/System/Volumes", dataVolume, "/Library", "/Applications", "/Users", "/Users/Shared",
	"/usr", "/bin", "/sbin", "/etc", "/var", "/private", "/private/etc", "/private/var", "/private/tmp",
	"/tmp", "/opt", "/dev", "/cores", "/Volumes",
}

var homeFolders = []string{
	"Library", "Documents", "Desktop", "Downloads", "Pictures", "Movies", "Music", "Public",
	"Applications", ".Trash",
}

func protectedPaths(home string) []string {
	homes := userHomes(home, []string{"/Users"}, "/var/root")
	return withHomeFolders(append([]string(nil), systemRoots...), homes, homeFolders)
}

// mountRoots lists every mount point, including ones hidden from browsing.
func mountRoots() []string {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil
	}
	buf := make([]unix.Statfs_t, n+16)
	n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return nil
	}
	var out []string
	for i := 0; i < n && i < len(buf); i++ {
		out = append(out, cstr8(buf[i].Mntonname[:]))
	}
	return out
}

// canonForms adds the firmlinked spelling of a Data-volume path
// (/System/Volumes/Data/Users/x is /Users/x).
func canonForms(canon string) []string {
	if rest, ok := strings.CutPrefix(canon, dataVolume+"/"); ok {
		return []string{canon, "/" + rest}
	}
	return []string{canon}
}
