// SPDX-License-Identifier: MIT

//go:build darwin

package scan

import (
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

const (
	mntRdonly     = 0x1
	mntLocal      = 0x1000
	mntDontBrowse = 0x100000
)

func cstr8(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// Volumes lists browsable mounted filesystems.
func Volumes() ([]proto.Volume, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	buf := make([]unix.Statfs_t, n+16)
	n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	var vols []proto.Volume
	for i := 0; i < n && i < len(buf); i++ {
		fs := &buf[i]
		if fs.Flags&mntDontBrowse != 0 {
			continue
		}
		typ := cstr8(fs.Fstypename[:])
		switch typ {
		case "devfs", "autofs", "mtmfs", "fdesc", "volfs":
			continue
		}
		path := cstr8(fs.Mntonname[:])
		bs := uint64(fs.Bsize)
		total, free := fs.Blocks*bs, fs.Bavail*bs
		v := proto.Volume{
			Path:     path,
			FS:       typ,
			Total:    total,
			Free:     free,
			ReadOnly: fs.Flags&mntRdonly != 0,
		}
		if fs.Bfree*bs <= total {
			v.Used = total - fs.Bfree*bs
		}
		switch {
		case path == "/":
			v.Label, v.Kind = "Macintosh HD", "boot"
		case typ == "nfs" || typ == "smbfs" || typ == "afpfs" || typ == "webdav" || fs.Flags&mntLocal == 0:
			v.Label, v.Kind = filepath.Base(path), "network"
		case len(path) > 9 && path[:9] == "/Volumes/":
			v.Label, v.Kind = filepath.Base(path), "external"
		default:
			v.Label, v.Kind = filepath.Base(path), "internal"
		}
		vols = append(vols, v)
	}
	sort.SliceStable(vols, func(i, j int) bool { return vols[i].Kind == "boot" && vols[j].Kind != "boot" })
	return vols, nil
}
