// SPDX-License-Identifier: MIT

package scan

import "errors"

// Identity identifies a directory independent of its path: (dev, ino) on Unix,
// (volume serial, 128-bit file id) on Windows. The zero value means "unknown"
// and disables verification.
type Identity struct{ Dev, Ino, Ino2 uint64 }

// IsZero reports whether i is the unset identity.
func (i Identity) IsZero() bool { return i == Identity{} }

// ErrChanged is returned when a directory opened by path is not the directory
// that was recorded (it was replaced or an ancestor was swapped, e.g. for a
// symlink, after it was scanned).
var ErrChanged = errors.New("scan: directory changed since it was scanned")

// verifyOpened compares the identity of an opened directory handle with the
// expected one. An unset want accepts anything.
func verifyOpened(want, got Identity, gotOK bool) error {
	if want.IsZero() {
		return nil
	}
	if !gotOK || got != want {
		return ErrChanged
	}
	return nil
}
