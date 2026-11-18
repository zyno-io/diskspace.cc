// SPDX-License-Identifier: MIT

//go:build linux

package scan

var systemRoots = []string{
	"/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib32", "/lib64", "/libx32", "/media",
	"/mnt", "/opt", "/proc", "/root", "/run", "/sbin", "/snap", "/srv", "/sys", "/tmp", "/usr", "/var",
	"/var/tmp",
}

var homeFolders = []string{
	"Desktop", "Documents", "Downloads", "Music", "Pictures", "Public", "Templates", "Videos",
	".config", ".local", ".local/share", ".local/share/Trash",
}

func protectedPaths(home string) []string {
	homes := userHomes(home, []string{"/home"}, "/root")
	return withHomeFolders(append([]string(nil), systemRoots...), homes, homeFolders)
}

func mountRoots() []string {
	var out []string
	for m := range parseMountpoints() {
		out = append(out, m)
	}
	return out
}

func canonForms(canon string) []string { return []string{canon} }
