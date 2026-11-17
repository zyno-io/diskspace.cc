// SPDX-License-Identifier: MIT

package proto

// Scan output records. The scanner (internal/scan) produces these in
// parent-before-child order; the session layer encodes them into NODES
// messages (see docs/protocol.md §4 and EncodeNodes).

// Dir flags.
const (
	DirReadDenied     = 1 << 0
	DirReadError      = 1 << 1
	DirVanished       = 1 << 2
	DirFoldedChildren = 1 << 3
	DirHasEntryErrors = 1 << 4
)

// Child (subdirectory) flags. A flagged child never gets its own DirRecord.
const (
	ChildOtherDevice = 1 << 0
	ChildReparse     = 1 << 1
	ChildExcluded    = 1 << 2
	ChildDupDir      = 1 << 3
	ChildBindMount   = 1 << 4
)

// File flags.
const (
	FileMultiLink = 1 << 0
	FileSymlink   = 1 << 1
	FileSparse    = 1 << 2
	FileCloud     = 1 << 3
	FileSpecial   = 1 << 4
	FileBadName   = 1 << 5
)

// Record kinds inside a NODES message.
const (
	RecDir     = 0x01
	RecFold    = 0x02
	RecFoldEnd = 0x03
)

// ChildDir is a subdirectory announced by its parent's DirRecord.
// Its id is FirstChildID + index within DirRecord.Dirs.
type ChildDir struct {
	Name  string
	Flags uint64
}

// FileEntry is an individually listed file (top-N policy).
type FileEntry struct {
	Name     string
	Alloc    uint64
	Apparent uint64
	Flags    uint64
}

// DirRecord describes one fully-read directory.
// Own* include every non-directory entry directly inside (after hardlink
// dedup) plus the directory's own allocated blocks. Files lists only the
// top-N; the viewer derives the "other files" bucket as Own − Σ Files.
type DirRecord struct {
	ID           uint64
	Flags        uint64
	OwnAlloc     uint64
	OwnApparent  uint64
	OwnFiles     uint64
	EntryErrors  uint64
	FirstChildID uint64 // valid when len(Dirs) > 0
	Dirs         []ChildDir
	Files        []FileEntry
}

// FoldRecord adds the totals of an unannounced (folded) subtree directory
// to TargetID's own totals.
type FoldRecord struct {
	TargetID uint64
	Alloc    uint64
	Apparent uint64
	Files    uint64
	Dirs     uint64
}

// FoldEndRecord marks that all folded descendants of TargetID are reported.
type FoldEndRecord struct {
	TargetID uint64
}

// Record is one of *DirRecord, *FoldRecord, *FoldEndRecord.
type Record interface{ isRecord() }

func (*DirRecord) isRecord()     {}
func (*FoldRecord) isRecord()    {}
func (*FoldEndRecord) isRecord() {}
