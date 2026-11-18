// SPDX-License-Identifier: MIT

//go:build windows

package scan

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

var homeFolders = []string{
	"AppData", "Documents", "Desktop", "Downloads", "Pictures", "Videos", "Music", "OneDrive",
}

func sysDrive() string {
	if d := os.Getenv("SystemDrive"); len(d) == 2 && d[1] == ':' {
		return d
	}
	return "C:"
}

func protectedPaths(home string) []string {
	drive := sysDrive()
	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = drive + `\Windows`
	}
	out := []string{
		drive + `\`, sysRoot, drive + `\ProgramData`, drive + `\Users`,
		drive + `\$Recycle.Bin`, drive + `\System Volume Information`,
	}
	if m, err := filepath.Glob(drive + `\Program Files*`); err == nil {
		out = append(out, m...)
	}
	homes := userHomes(home, []string{drive + `\Users`}, os.Getenv("USERPROFILE"))
	out = withHomeFolders(out, homes, homeFolders)
	for _, h := range homes {
		if m, err := filepath.Glob(filepath.Join(h, "OneDrive*")); err == nil {
			out = append(out, m...) // "OneDrive - Company"
		}
	}
	// The current user's folders may be redirected (e.g. into OneDrive).
	for _, id := range []*windows.KNOWNFOLDERID{
		windows.FOLDERID_Profile, windows.FOLDERID_Documents, windows.FOLDERID_Desktop,
		windows.FOLDERID_Downloads, windows.FOLDERID_Pictures, windows.FOLDERID_Videos,
		windows.FOLDERID_Music, windows.FOLDERID_SkyDrive,
	} {
		if p, err := windows.KnownFolderPath(id, windows.KF_FLAG_DONT_VERIFY); err == nil && p != "" {
			out = append(out, p)
		}
	}
	return out
}

// mountRoots lists every drive root and its system folders. They are
// compared by path only (no I/O on removable or network drives).
func mountRoots() []string {
	buf := make([]uint16, 512)
	n, err := windows.GetLogicalDriveStrings(uint32(len(buf)), &buf[0])
	if err != nil {
		return nil
	}
	if int(n) > len(buf) {
		buf = make([]uint16, n)
		if n, err = windows.GetLogicalDriveStrings(uint32(len(buf)), &buf[0]); err != nil {
			return nil
		}
	}
	var out []string
	for _, root := range splitMulti(buf[:n]) {
		root = strings.TrimRight(root, `\`) + `\`
		out = append(out, root, root+`$Recycle.Bin`, root+`System Volume Information`)
	}
	return out
}

func canonForms(canon string) []string { return []string{canon} }

// pathIdentity returns the identity of the directory path names, following
// reparse points.
func pathIdentity(path string) (Identity, bool) {
	h, err := openFollow(path)
	if err != nil {
		return Identity{}, false
	}
	defer windows.CloseHandle(h)
	return handleIdentity(h)
}
