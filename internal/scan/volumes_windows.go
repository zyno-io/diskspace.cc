// SPDX-License-Identifier: MIT

//go:build windows

package scan

import (
	"os"
	"sort"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

const fileReadOnlyVolume = 0x00080000

// Volumes lists logical drives.
func Volumes() ([]proto.Volume, error) {
	buf := make([]uint16, 512)
	n, err := windows.GetLogicalDriveStrings(uint32(len(buf)), &buf[0])
	if err != nil {
		return nil, err
	}
	if int(n) > len(buf) {
		buf = make([]uint16, n)
		if n, err = windows.GetLogicalDriveStrings(uint32(len(buf)), &buf[0]); err != nil {
			return nil, err
		}
	}
	sysDrive := strings.ToUpper(os.Getenv("SystemDrive"))
	if sysDrive == "" {
		sysDrive = "C:"
	}
	var vols []proto.Volume
	for _, root := range splitMulti(buf[:n]) {
		rp, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		dt := windows.GetDriveType(rp)
		var kind string
		switch dt {
		case windows.DRIVE_FIXED:
			kind = "internal"
		case windows.DRIVE_REMOVABLE:
			kind = "external"
		case windows.DRIVE_REMOTE:
			kind = "network"
		default:
			continue
		}
		var avail, total, free uint64
		if windows.GetDiskFreeSpaceEx(rp, &avail, &total, &free) != nil {
			continue
		}
		name := make([]uint16, 261)
		fsn := make([]uint16, 261)
		var flags uint32
		label := ""
		fs := ""
		if windows.GetVolumeInformation(rp, &name[0], uint32(len(name)), nil, nil, &flags, &fsn[0], uint32(len(fsn))) == nil {
			label = windows.UTF16ToString(name)
			fs = windows.UTF16ToString(fsn)
		}
		if strings.HasPrefix(strings.ToUpper(root), sysDrive) {
			kind = "boot"
		}
		if label == "" {
			label = "Local Disk"
		}
		vols = append(vols, proto.Volume{
			Path: root, Label: label + " (" + strings.TrimRight(root, `\`) + ")", FS: fs,
			Total: total, Free: avail, Used: total - free, Kind: kind,
			ReadOnly: flags&fileReadOnlyVolume != 0,
		})
	}
	sort.SliceStable(vols, func(i, j int) bool { return vols[i].Kind == "boot" && vols[j].Kind != "boot" })
	return vols, nil
}

func splitMulti(b []uint16) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			if i > start {
				out = append(out, windows.UTF16ToString(b[start:i]))
			}
			start = i + 1
		}
	}
	return out
}
