// SPDX-License-Identifier: MIT

//go:build linux

package scan

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

var blockFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true,
	"f2fs": true, "vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "fuseblk": true,
	"bcachefs": true, "jfs": true, "reiserfs": true, "hfsplus": true, "apfs": true,
}
var netFS = map[string]bool{"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true}

// Volumes lists real block-backed and network filesystems.
func Volumes() ([]proto.Volume, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	seen := map[string]bool{}
	var vols []proto.Volume
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// id parent maj:min root mountpoint opts [optional...] - fstype source superopts
		dash := -1
		for i, fl := range fields {
			if fl == "-" {
				dash = i
				break
			}
		}
		if dash < 6 || dash+2 >= len(fields) {
			continue
		}
		majmin, root, mp, opts := fields[2], fields[3], unescapeMount(fields[4]), fields[5]
		fstype, source := fields[dash+1], fields[dash+2]
		isNet := netFS[fstype]
		if !blockFS[fstype] && !isNet {
			continue
		}
		if fstype == "squashfs" || strings.HasPrefix(mp, "/snap/") || strings.HasPrefix(source, "/dev/loop") {
			continue
		}
		if !isNet {
			if root != "/" || seen[majmin] {
				continue
			}
			seen[majmin] = true
		}
		var st unix.Statfs_t
		if unix.Statfs(mp, &st) != nil {
			continue
		}
		bs := uint64(st.Bsize)
		total := st.Blocks * bs
		v := proto.Volume{
			Path:     mp,
			FS:       fstype,
			Total:    total,
			Free:     st.Bavail * bs,
			Used:     (st.Blocks - st.Bfree) * bs,
			ReadOnly: strings.Contains(","+opts+",", ",ro,"),
			Label:    filepath.Base(mp),
		}
		switch {
		case isNet:
			v.Kind = "network"
		case mp == "/":
			v.Kind, v.Label = "boot", "/"
		case strings.HasPrefix(mp, "/media/") || strings.HasPrefix(mp, "/mnt/") || strings.HasPrefix(mp, "/run/media/"):
			v.Kind = "external"
		default:
			v.Kind = "internal"
		}
		vols = append(vols, v)
	}
	sort.SliceStable(vols, func(i, j int) bool { return vols[i].Kind == "boot" && vols[j].Kind != "boot" })
	return vols, sc.Err()
}
