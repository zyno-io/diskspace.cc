// SPDX-License-Identifier: MIT

//go:build darwin || linux

package scan

import (
	"errors"

	"golang.org/x/sys/unix"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

// readScratch holds per-worker reusable buffers.
type readScratch struct {
	buf   []byte
	names []string
}

func openDir(path string) (int, error) {
	for {
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == unix.EINTR {
			continue
		}
		return fd, err
	}
}

func isDenied(err error) bool {
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)
}

func isGone(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR)
}

func rootID(path string) (dev, ino uint64, err error) {
	var st unix.Stat_t
	if err = unix.Stat(path, &st); err != nil {
		return 0, 0, err
	}
	return devOf(&st), uint64(st.Ino), nil
}

// readDirEntries opens path and calls fn for every entry (excluding . and
// ..) after lstat-ing it relative to the directory fd. fn returns false to
// stop early. A non-nil error with partial callbacks means a mid-read failure.
func readDirEntries(path string, sc *readScratch, fn func(*rawEntry) bool) (selfInfo, error) {
	fd, err := openDir(path)
	if err != nil {
		return selfInfo{}, err
	}
	defer unix.Close(fd)

	var self selfInfo
	var st unix.Stat_t
	if unix.Fstat(fd, &st) == nil {
		self = selfInfo{Dev: devOf(&st), Ino: uint64(st.Ino), Alloc: uint64(st.Blocks) * 512, Apparent: uint64(st.Size)}
	}
	if sc.buf == nil {
		sc.buf = make([]byte, 64<<10)
	}
	for {
		var n int
		for {
			n, err = unix.ReadDirent(fd, sc.buf)
			if err == unix.EINTR {
				continue
			}
			break
		}
		if err != nil {
			return self, err
		}
		if n <= 0 {
			return self, nil
		}
		_, _, sc.names = unix.ParseDirent(sc.buf[:n], -1, sc.names[:0])
		for _, name := range sc.names {
			var e rawEntry
			e.Name = name
			for {
				err = unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
				if err != unix.EINTR {
					break
				}
			}
			if err != nil {
				e.Err = err
			} else {
				fillEntry(&e, &st)
			}
			if !fn(&e) {
				return self, nil
			}
		}
	}
}

func fillEntry(e *rawEntry, st *unix.Stat_t) {
	e.Dev = devOf(st)
	e.Ino = uint64(st.Ino)
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFDIR:
		e.Kind = kindDir
		return
	case unix.S_IFREG:
		e.Alloc = uint64(st.Blocks) * 512
		e.Apparent = uint64(st.Size)
		if e.Alloc < e.Apparent {
			e.Flags |= proto.FileSparse
		}
		e.Flags |= platFileFlags(st)
	case unix.S_IFLNK:
		e.Alloc = uint64(st.Blocks) * 512
		e.Apparent = uint64(st.Size)
		e.Flags |= proto.FileSymlink
	default:
		e.Alloc = uint64(st.Blocks) * 512
		e.Apparent = uint64(st.Size)
		e.Flags |= proto.FileSpecial
	}
	if uint64(st.Nlink) > 1 {
		e.Dedup = true
		e.Flags |= proto.FileMultiLink
	}
}
