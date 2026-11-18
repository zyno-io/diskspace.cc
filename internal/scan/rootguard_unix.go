// SPDX-License-Identifier: MIT

//go:build darwin || linux

package scan

import "golang.org/x/sys/unix"

// maxGuardWalk bounds the upward ".." walk.
const maxGuardWalk = 4096

func fdIdentity(fd int) (Identity, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return Identity{}, err
	}
	return Identity{Dev: devOf(&st), Ino: uint64(st.Ino)}, nil
}

func openatDir(dirfd int, name string) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == unix.EINTR {
			continue
		}
		return fd, err
	}
}

func (g *RootGuard) initRoot(real string) error {
	fd, err := openDir(real)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	g.id, err = fdIdentity(fd)
	return err
}

// contained opens resolved, takes its identity from the handle, and walks
// upward through ".." from that same handle until it meets the guard root
// (contained) or the filesystem root (not contained).
func (g *RootGuard) contained(resolved string) (Identity, bool) {
	fd, err := openDir(resolved)
	if err != nil {
		return Identity{}, false
	}
	defer unix.Close(fd)
	id, err := fdIdentity(fd)
	if err != nil || !walkUp(fd, g.id) {
		return Identity{}, false
	}
	return id, true
}

// walkUp walks upward through ".." from fd (left open), starting at fd's own
// directory, and reports whether every non-zero stop is met, in order, before
// the filesystem root. One level may meet several equal stops at once.
func walkUp(fd int, stops ...Identity) bool {
	cur, err := fdIdentity(fd)
	if err != nil {
		return false
	}
	at := fd // the directory at the current level; owned when != fd
	defer func() {
		if at != fd {
			unix.Close(at)
		}
	}()
	k := 0
	for i := 0; i < maxGuardWalk; i++ {
		for k < len(stops) && (stops[k].IsZero() || stops[k] == cur) {
			k++
		}
		if k == len(stops) {
			return true
		}
		pfd, err := openatDir(at, "..")
		if at != fd {
			unix.Close(at)
		}
		at = fd
		if err != nil {
			return false
		}
		at = pfd
		pid, err := fdIdentity(at)
		if err != nil || pid == cur {
			// error, or ".." of the filesystem root is itself: not met.
			return false
		}
		cur = pid
	}
	return false
}
