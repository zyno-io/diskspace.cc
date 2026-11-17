// SPDX-License-Identifier: MIT

//go:build windows

package scan

import (
	"errors"
	"strings"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

// FILE_INFO_BY_HANDLE_CLASS values used here.
const (
	classStandardInfo         = 1
	classIDBothDirInfo        = 10
	classIDBothDirRestartInfo = 11
	classIDExtdDirInfo        = 19
	classIDExtdDirRestartInfo = 20
	attrDirectory             = 0x10
	attrSparse                = 0x200
	attrReparse               = 0x400
	attrOffline               = 0x1000
	attrRecallOnOpen          = 0x40000
	attrRecallOnDataAccess    = 0x400000
	reparseNameSurrogate      = 0x20000000
	reparseTagSymlink         = 0xA000000C
)

// readScratch holds a 64 KiB, 8-byte aligned enumeration buffer.
type readScratch struct {
	buf []uint64
}

type platState struct{}

func (p *platState) init(root string, rootDev uint64, cross bool) {}
func (p *platState) allowedDev(dev uint64) bool                   { return true }
func (p *platState) excluded(parent, name string) bool            { return false }
func (p *platState) bindMount(path string) bool                   { return false }

func isDenied(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD)
}

func isGone(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_INVALID_NAME)
}

func toLongPath(p string) string {
	switch {
	case strings.HasPrefix(p, `\\?\`):
		return p
	case strings.HasPrefix(p, `\\`):
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}

// wtf8ToUTF16 converts a (possibly WTF-8) string to a NUL-terminated UTF-16 slice.
func wtf8ToUTF16(s string) []uint16 {
	out := make([]uint16, 0, len(s)+1)
	for i := 0; i < len(s); {
		if s[i] == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2]&0xC0 == 0x80 {
			out = append(out, uint16(0xD000|uint16(s[i+1]&0x3F)<<6|uint16(s[i+2]&0x3F)))
			i += 3
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		if r >= 0x10000 {
			r -= 0x10000
			out = append(out, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
		} else {
			out = append(out, uint16(r))
		}
	}
	return append(out, 0)
}

// utf16ToWTF8 converts UTF-16 to WTF-8 (lone surrogates preserved).
func utf16ToWTF8(u []uint16) string {
	b := make([]byte, 0, len(u)+8)
	for i := 0; i < len(u); i++ {
		c := rune(u[i])
		switch {
		case c >= 0xD800 && c < 0xDC00 && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] < 0xE000:
			c = 0x10000 + (c-0xD800)<<10 + rune(u[i+1]) - 0xDC00
			i++
			b = utf8.AppendRune(b, c)
		case c >= 0xD800 && c < 0xE000:
			b = append(b, 0xE0|byte(c>>12), 0x80|byte(c>>6)&0x3F, 0x80|byte(c)&0x3F)
		default:
			b = utf8.AppendRune(b, c)
		}
	}
	return string(b)
}

func openDir(path string) (windows.Handle, error) {
	p := wtf8ToUTF16(toLongPath(path))
	return windows.CreateFile(&p[0], windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

func rootID(path string) (dev, ino uint64, err error) {
	h, err := openDir(path)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(h)
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return 0, 0, err
	}
	return uint64(fi.VolumeSerialNumber), uint64(fi.FileIndexHigh)<<32 | uint64(fi.FileIndexLow), nil
}

type fileStandardInfo struct {
	AllocationSize int64
	EndOfFile      int64
	NumberOfLinks  uint32
	DeletePending  bool
	Directory      bool
}

func mixID(lo, hi uint64) uint64 {
	x := lo*0x9E3779B97F4A7C15 ^ (hi+0x7F4A7C15)*0xC2B2AE3D27D4EB4F
	x ^= x >> 29
	return x
}

func readDirEntries(path string, sc *readScratch, fn func(*rawEntry) bool) (selfInfo, error) {
	h, err := openDir(path)
	if err != nil {
		return selfInfo{}, err
	}
	defer windows.CloseHandle(h)

	var self selfInfo
	var fi windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(h, &fi) == nil {
		self.Dev = uint64(fi.VolumeSerialNumber)
		self.Ino = uint64(fi.FileIndexHigh)<<32 | uint64(fi.FileIndexLow)
	}
	var si fileStandardInfo
	if windows.GetFileInformationByHandleEx(h, classStandardInfo, (*byte)(unsafe.Pointer(&si)), uint32(unsafe.Sizeof(si))) == nil {
		self.Alloc, self.Apparent = uint64(si.AllocationSize), uint64(si.EndOfFile)
	}

	if sc.buf == nil {
		sc.buf = make([]uint64, 64<<10/8)
	}
	bufp := (*byte)(unsafe.Pointer(&sc.buf[0]))
	bufLen := uint32(len(sc.buf) * 8)
	class := uint32(classIDExtdDirRestartInfo)
	fallback, first := false, true
	for {
		err := windows.GetFileInformationByHandleEx(h, class, bufp, bufLen)
		if err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return self, nil
			}
			if first && !fallback && errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				fallback = true
				class = classIDBothDirRestartInfo
				continue
			}
			return self, err
		}
		first = false
		if fallback {
			class = classIDBothDirInfo
		} else {
			class = classIDExtdDirInfo
		}
		base := unsafe.Pointer(bufp)
		var off uintptr
		for {
			p := unsafe.Add(base, off)
			next := *(*uint32)(p)
			var (
				eof     = *(*int64)(unsafe.Add(p, 40))
				alloc   = *(*int64)(unsafe.Add(p, 48))
				attrs   = *(*uint32)(unsafe.Add(p, 56))
				nameLen = *(*uint32)(unsafe.Add(p, 60))
				tag     = *(*uint32)(unsafe.Add(p, 64)) // EaSize: reparse tag for reparse points (fallback)
				idLo    uint64
				idHi    uint64
				nameOff uintptr
			)
			if fallback {
				idLo = *(*uint64)(unsafe.Add(p, 96))
				nameOff = 104
			} else {
				tag = *(*uint32)(unsafe.Add(p, 68))
				idLo = *(*uint64)(unsafe.Add(p, 72))
				idHi = *(*uint64)(unsafe.Add(p, 80))
				nameOff = 88
			}
			name := unsafe.Slice((*uint16)(unsafe.Add(p, nameOff)), nameLen/2)
			if !(len(name) == 1 && name[0] == '.') && !(len(name) == 2 && name[0] == '.' && name[1] == '.') {
				var e rawEntry
				e.Name = utf16ToWTF8(name)
				e.Dev = self.Dev
				reparse := attrs&attrReparse != 0
				if attrs&attrDirectory != 0 {
					e.Kind = kindDir
					e.Ino = mixID(idLo, idHi)
					if reparse && tag&reparseNameSurrogate != 0 {
						e.ChildFlags = proto.ChildReparse
					}
				} else {
					e.Alloc, e.Apparent = uint64(alloc), uint64(eof)
					e.Ino = mixID(idLo, idHi)
					e.Dedup = idLo != 0 || idHi != 0
					if reparse && tag == reparseTagSymlink {
						e.Flags |= proto.FileSymlink
					}
					if attrs&attrSparse != 0 {
						e.Flags |= proto.FileSparse
					}
					if attrs&(attrOffline|attrRecallOnOpen|attrRecallOnDataAccess) != 0 {
						e.Flags |= proto.FileCloud
					}
				}
				if !fn(&e) {
					return self, nil
				}
			}
			if next == 0 {
				break
			}
			off += uintptr(next)
		}
	}
}
