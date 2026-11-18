// SPDX-License-Identifier: MIT

//go:build windows

package scan

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// maxRecycleTries bounds random-name collisions in the Recycle Bin; access
// rights not named in x/sys.
const (
	maxRecycleTries = 32
	fileAddFile     = 0x2 // FILE_ADD_FILE
	fileAddSubdir   = 0x4 // FILE_ADD_SUBDIRECTORY
)

// trash moves the pinned target into the Recycle Bin of its volume, as
// Explorer does: <vol>\$Recycle.Bin\<SID>\$I<6><ext> (metadata) is written
// first, then the target handle is renamed to $R<same 6><ext> relative to the
// pinned Recycle Bin directory handle, never replacing anything. The $I file
// is removed if the move fails. Explorer lists and restores such entries.
func (r *remover) trash(p *pinned, t *Target) (string, error) {
	binPath, err := r.recycleDir(p)
	if err != nil {
		return "", rmErr(CodeTrashUnavailable, t.Path, err)
	}
	bin, err := openDirPath(binPath, fileAddFile|fileAddSubdir|windows.FILE_TRAVERSE)
	if err != nil {
		return "", rmErr(CodeTrashUnavailable, t.Path, err)
	}
	defer windows.CloseHandle(bin)
	bid, ok := handleIdentity(bin)
	attrs, _, aerr := attrTag(bin)
	if !ok || aerr != nil || attrs&attrDirectory == 0 || attrs&attrReparse != 0 || bid.Dev != t.ID.Dev {
		return "", rmErr(CodeTrashUnavailable, t.Path, errors.New("no usable Recycle Bin on this volume"))
	}
	ext := ""
	if !t.Dir {
		ext = filepath.Ext(t.Name)
	}
	size := t.Apparent
	if t.Dir {
		size = r.opts.TrashSize
	}
	info := recycleInfo(p.canon, size, time.Now())
	for i := 0; i < maxRecycleTries; i++ {
		if r.canceled() {
			return "", rmErr(CodeCanceled, t.Path, r.ctx.Err())
		}
		id := randRecycleID()
		ih, err := ntOpen(bin, "$I"+id+ext, windows.DELETE|windows.FILE_WRITE_DATA|windows.FILE_READ_ATTRIBUTES,
			windows.FILE_NON_DIRECTORY_FILE, windows.FILE_CREATE)
		if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
			continue
		}
		if err != nil {
			return "", rmErr(CodeTrashUnavailable, t.Path, err)
		}
		var n uint32
		if err := windows.WriteFile(ih, info, &n, nil); err != nil || int(n) != len(info) {
			deleteHandle(ih, 0)
			windows.CloseHandle(ih)
			return "", rmErr(CodeTrashUnavailable, t.Path, err)
		}
		err = renameInto(p.h, bin, "$R"+id+ext)
		if err != nil {
			deleteHandle(ih, 0)
			windows.CloseHandle(ih)
			if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
				continue
			}
			if errors.Is(err, windows.STATUS_NOT_SAME_DEVICE) {
				return "", rmErr(CodeTrashUnavailable, t.Path, err)
			}
			return "", rmErr(errCode(err), t.Path, err)
		}
		windows.CloseHandle(ih)
		return binPath + `\$R` + id + ext, nil
	}
	return "", rmErr(CodeTrashUnavailable, t.Path, errors.New("no free name in the Recycle Bin"))
}

// recycleDir returns <volume of the target>\$Recycle.Bin\<SID>.
func (r *remover) recycleDir(p *pinned) (string, error) {
	if h := r.opts.hooks; h != nil && h.recycleDir != "" {
		return h.recycleDir, nil
	}
	sid := r.opts.SID
	if sid == "" {
		u, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return "", err
		}
		sid = u.User.Sid.String()
	}
	if !strings.HasPrefix(sid, "S-") || !ValidName(sid) {
		return "", errors.New("invalid SID")
	}
	vol, err := volumeRoot(p.canon)
	if err != nil {
		return "", err
	}
	return vol + `$Recycle.Bin\` + sid, nil
}

// volumeRoot returns the root (with a trailing "\") of the volume holding the
// DOS path p. Network paths have no Recycle Bin.
func volumeRoot(p string) (string, error) {
	if strings.HasPrefix(p, `\\`) {
		return "", errors.New("network paths have no Recycle Bin")
	}
	u := wtf8ToUTF16(p)
	buf := make([]uint16, 1024)
	if windows.GetVolumePathName(&u[0], &buf[0], uint32(len(buf))) == nil {
		if v := windows.UTF16ToString(buf); v != "" {
			return strings.TrimRight(v, `\`) + `\`, nil
		}
	}
	if len(p) >= 3 && p[1] == ':' && p[2] == '\\' {
		return p[:3], nil
	}
	return "", errors.New("unknown volume")
}

// randRecycleID returns 6 random characters from [A-Z0-9].
func randRecycleID() string {
	const set = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var b [6]byte
	rand.Read(b[:])
	for i := range b {
		b[i] = set[int(b[i])%len(set)]
	}
	return string(b[:])
}

// recycleInfo encodes a version 2 $I file: int64 2, int64 size, int64
// FILETIME (UTC) of deletion, int32 path length in UTF-16 units including the
// NUL, then the UTF-16 path and NUL.
func recycleInfo(orig string, size uint64, when time.Time) []byte {
	u := wtf8ToUTF16(orig)
	b := make([]byte, 28+2*len(u))
	binary.LittleEndian.PutUint64(b[0:], 2)
	binary.LittleEndian.PutUint64(b[8:], size)
	ft := windows.NsecToFiletime(when.UnixNano())
	binary.LittleEndian.PutUint64(b[16:], uint64(ft.HighDateTime)<<32|uint64(ft.LowDateTime))
	binary.LittleEndian.PutUint32(b[24:], uint32(len(u)))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[28+2*i:], c)
	}
	return b
}

// renameInto renames the open handle h to name inside directory dir, failing
// if the name exists (FileRenameInformationEx without REPLACE_IF_EXISTS;
// FileRenameInformation with ReplaceIfExists FALSE where Ex is unsupported).
func renameInto(h, dir windows.Handle, name string) error {
	type renameInfo struct {
		Flags          uint32 // FileRenameInformation: ReplaceIfExists (BOOLEAN)
		RootDirectory  windows.Handle
		FileNameLength uint32
		FileName       [1]uint16
	}
	u := wtf8ToUTF16(name)
	u = u[:len(u)-1]
	off := unsafe.Offsetof(renameInfo{}.FileName)
	size := max(off+uintptr(len(u))*2, unsafe.Sizeof(renameInfo{}))
	buf := make([]uint64, (size+7)/8)
	ri := (*renameInfo)(unsafe.Pointer(&buf[0]))
	ri.RootDirectory = dir
	ri.FileNameLength = uint32(len(u) * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Add(unsafe.Pointer(ri), off)), len(u)), u)
	var iosb windows.IO_STATUS_BLOCK
	err := windows.NtSetInformationFile(h, &iosb, (*byte)(unsafe.Pointer(ri)), uint32(size), ntFileRenameInformationEx)
	if unsupported(err) {
		ri.Flags = 0
		err = windows.NtSetInformationFile(h, &iosb, (*byte)(unsafe.Pointer(ri)), uint32(size), ntFileRenameInformation)
	}
	return err
}
