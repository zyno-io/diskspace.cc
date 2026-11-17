// SPDX-License-Identifier: MIT

package scan

import "unicode/utf8"

type entryKind uint8

const (
	kindFile entryKind = iota // any non-directory (regular, symlink, special)
	kindDir
)

// rawEntry is one directory entry as produced by the platform reader.
// The pointer handed to the callback is only valid during the call.
type rawEntry struct {
	Name       string
	Kind       entryKind
	Alloc      uint64
	Apparent   uint64
	Dev, Ino   uint64
	Dedup      bool     // file participates in (dev,ino) dedup
	Flags      uint64   // proto.File* flags set by the platform (SYMLINK, SPARSE, CLOUD, SPECIAL, MULTILINK)
	ChildFlags uint64   // for dirs: platform-determined child flags (e.g. REPARSE)
	ID         Identity // directory identity (dirs only)
	Err        error    // stat failure for this entry
}

// selfInfo describes the directory that was opened.
type selfInfo struct {
	Dev, Ino        uint64
	ID              Identity
	Alloc, Apparent uint64
}

func badName(s string) bool { return !utf8.ValidString(s) }
