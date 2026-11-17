// SPDX-License-Identifier: MIT

package proto

import (
	"errors"
	"fmt"
)

// NodesEncoder accumulates records into one NODES message body.
// Because the record count precedes the records, records are buffered and the
// header is prepended by Bytes(). Not goroutine-safe.
type NodesEncoder struct {
	scanID uint64
	count  uint64
	buf    []byte
	out    []byte
}

func NewNodesEncoder(scanID uint64) *NodesEncoder {
	return &NodesEncoder{scanID: scanID}
}

// Reset clears the encoder for a new message (buffers are reused).
func (e *NodesEncoder) Reset(scanID uint64) {
	e.scanID, e.count = scanID, 0
	e.buf = e.buf[:0]
}

func appendName(b []byte, s string) []byte {
	if len(s) > MaxNameLen {
		s = s[:MaxNameLen] // oversize names are truncated (viewer flags invalid UTF-8)
	}
	b = AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// Add appends one record (*DirRecord, *DirMoreRecord, *FoldRecord or *FoldEndRecord).
func (e *NodesEncoder) Add(rec Record) {
	b := e.buf
	switch r := rec.(type) {
	case *DirRecord:
		b = append(b, RecDir)
		b = AppendUvarint(b, r.ID)
		b = AppendUvarint(b, r.Flags)
		b = AppendUvarint(b, r.OwnAlloc)
		b = AppendUvarint(b, r.OwnApparent)
		b = AppendUvarint(b, r.OwnFiles)
		b = AppendUvarint(b, r.EntryErrors)
		b = AppendUvarint(b, r.TotalDirs)
		if r.TotalDirs > 0 {
			b = AppendUvarint(b, r.FirstChildID)
		}
		b = AppendUvarint(b, uint64(len(r.Dirs)))
		for _, d := range r.Dirs {
			b = appendName(b, d.Name)
			b = AppendUvarint(b, d.Flags)
		}
		b = AppendUvarint(b, uint64(len(r.Files)))
		for _, f := range r.Files {
			b = appendFile(b, f)
		}
	case *FoldRecord:
		b = append(b, RecFold)
		b = AppendUvarint(b, r.TargetID)
		b = AppendUvarint(b, r.Alloc)
		b = AppendUvarint(b, r.Apparent)
		b = AppendUvarint(b, r.Files)
		b = AppendUvarint(b, r.Dirs)
	case *DirMoreRecord:
		b = append(b, RecDirMore)
		b = AppendUvarint(b, r.ID)
		b = AppendUvarint(b, r.FirstChildID)
		b = AppendUvarint(b, uint64(len(r.Dirs)))
		for _, d := range r.Dirs {
			b = appendName(b, d.Name)
			b = AppendUvarint(b, d.Flags)
		}
	case *FoldEndRecord:
		b = append(b, RecFoldEnd)
		b = AppendUvarint(b, r.TargetID)
	default:
		panic(fmt.Sprintf("proto: unknown record type %T", rec))
	}
	e.buf = b
	e.count++
}

func appendFile(b []byte, f FileEntry) []byte {
	b = appendName(b, f.Name)
	b = AppendUvarint(b, f.Alloc)
	b = AppendUvarint(b, f.Apparent)
	return AppendUvarint(b, f.Flags)
}

// Len is the size in bytes of the full body Bytes() would return.
func (e *NodesEncoder) Len() int {
	return 1 + UvarintLen(e.scanID) + UvarintLen(e.count) + len(e.buf)
}

// Count is the number of records added.
func (e *NodesEncoder) Count() int { return int(e.count) }

// Bytes returns the full body: 0x11 scanId count records. The slice is valid
// until the next Reset/Bytes call.
func (e *NodesEncoder) Bytes() []byte {
	o := e.out[:0]
	o = append(o, MsgNodes)
	o = AppendUvarint(o, e.scanID)
	o = AppendUvarint(o, e.count)
	o = append(o, e.buf...)
	e.out = o
	return o
}

// ---- decoding (strict; for tests, fuzzing and the Go e2e client) ----

var ErrNodes = errors.New("proto: malformed NODES/DIR_FILES body")

type reader struct {
	b   []byte
	err error
}

func (r *reader) fail() { r.err = ErrNodes }

func (r *reader) uv() uint64 {
	if r.err != nil {
		return 0
	}
	v, n, err := Uvarint(r.b)
	if err != nil {
		r.fail()
		return 0
	}
	r.b = r.b[n:]
	return v
}

func (r *reader) byte() byte {
	if r.err != nil || len(r.b) == 0 {
		r.fail()
		return 0
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c
}

func (r *reader) name() string {
	n := r.uv()
	if r.err != nil {
		return ""
	}
	if n > MaxNameLen || n > uint64(len(r.b)) {
		r.fail()
		return ""
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	return s
}

// count reads an element count and checks it against remaining bytes given a
// minimum encoded size per element.
func (r *reader) count(minElem int) int {
	n := r.uv()
	if r.err != nil {
		return 0
	}
	if n > uint64(len(r.b)/minElem) {
		r.fail()
		return 0
	}
	return int(n)
}

func (r *reader) file() FileEntry {
	return FileEntry{Name: r.name(), Alloc: r.uv(), Apparent: r.uv(), Flags: r.uv()}
}

// DecodeNodes parses a NODES body (including the leading 0x11).
func DecodeNodes(body []byte) (scanID uint64, recs []Record, err error) {
	r := &reader{b: body}
	if r.byte() != MsgNodes {
		return 0, nil, ErrNodes
	}
	scanID = r.uv()
	n := r.count(2) // every record is at least kind + 1 varint
	for i := 0; i < n && r.err == nil; i++ {
		switch r.byte() {
		case RecDir:
			d := &DirRecord{ID: r.uv(), Flags: r.uv(), OwnAlloc: r.uv(), OwnApparent: r.uv(), OwnFiles: r.uv(), EntryErrors: r.uv()}
			d.TotalDirs = r.uv()
			if d.TotalDirs > MaxChildDirs {
				r.fail()
			}
			if d.TotalDirs > 0 {
				d.FirstChildID = r.uv()
			}
			nd := r.count(2)
			if uint64(nd) > d.TotalDirs {
				r.fail()
				nd = 0
			}
			if nd > 0 {
				d.Dirs = make([]ChildDir, 0, nd)
				for j := 0; j < nd && r.err == nil; j++ {
					d.Dirs = append(d.Dirs, ChildDir{Name: r.name(), Flags: r.uv()})
				}
			}
			nf := r.count(4)
			if nf > 0 {
				d.Files = make([]FileEntry, 0, nf)
				for j := 0; j < nf && r.err == nil; j++ {
					d.Files = append(d.Files, r.file())
				}
			}
			recs = append(recs, d)
		case RecFold:
			recs = append(recs, &FoldRecord{TargetID: r.uv(), Alloc: r.uv(), Apparent: r.uv(), Files: r.uv(), Dirs: r.uv()})
		case RecDirMore:
			m := &DirMoreRecord{ID: r.uv(), FirstChildID: r.uv()}
			nd := r.count(2)
			if nd == 0 {
				r.fail() // an empty continuation is never emitted
			}
			m.Dirs = make([]ChildDir, 0, nd)
			for j := 0; j < nd && r.err == nil; j++ {
				m.Dirs = append(m.Dirs, ChildDir{Name: r.name(), Flags: r.uv()})
			}
			recs = append(recs, m)
		case RecFoldEnd:
			recs = append(recs, &FoldEndRecord{TargetID: r.uv()})
		default:
			r.fail()
		}
	}
	if r.err != nil || len(r.b) != 0 {
		return 0, nil, ErrNodes
	}
	return scanID, recs, nil
}

// EncodeDirFiles builds a DIR_FILES body (including the leading 0x15).
func EncodeDirFiles(reqID, scanID, dirID uint64, res *DirFilesResult) []byte {
	b := []byte{MsgDirFiles}
	b = AppendUvarint(b, reqID)
	b = AppendUvarint(b, scanID)
	b = AppendUvarint(b, dirID)
	b = AppendUvarint(b, res.Flags)
	b = AppendUvarint(b, res.RestCount)
	b = AppendUvarint(b, res.RestAlloc)
	b = AppendUvarint(b, res.RestApparent)
	b = AppendUvarint(b, uint64(len(res.Files)))
	for _, f := range res.Files {
		b = appendFile(b, f)
	}
	return b
}

// DecodeDirFiles parses a DIR_FILES body.
func DecodeDirFiles(body []byte) (reqID, scanID, dirID uint64, res *DirFilesResult, err error) {
	r := &reader{b: body}
	if r.byte() != MsgDirFiles {
		return 0, 0, 0, nil, ErrNodes
	}
	reqID, scanID, dirID = r.uv(), r.uv(), r.uv()
	res = &DirFilesResult{Flags: r.uv(), RestCount: r.uv(), RestAlloc: r.uv(), RestApparent: r.uv()}
	n := r.count(4)
	if n > 0 {
		res.Files = make([]FileEntry, 0, n)
		for i := 0; i < n && r.err == nil; i++ {
			res.Files = append(res.Files, r.file())
		}
	}
	if r.err != nil || len(r.b) != 0 {
		return 0, 0, 0, nil, ErrNodes
	}
	return
}
