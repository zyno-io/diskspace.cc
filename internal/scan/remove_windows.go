// SPDX-License-Identifier: MIT

//go:build windows

package scan

import (
	"encoding/binary"
	"errors"
	"runtime"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

const badNameChars = "/\\\x00:"

// NT information classes (NtSetInformationFile) and Win32 ones
// (GetFileInformationByHandleEx) used here.
const (
	ntFileBasicInformation        = 4
	ntFileRenameInformation       = 10
	ntFileDispositionInformation  = 13
	ntFileDispositionInformationE = 64 // FileDispositionInformationEx
	ntFileRenameInformationEx     = 65
	classAttributeTagInfo         = 9
	attrReadOnly                  = 0x1
	attrNormal                    = 0x80
	attrSettable                  = 0x2 | 0x4 | 0x20 | 0x2000 // hidden, system, archive, not content indexed
	reparseTagMountPoint          = 0xA0000003
)

// pinned is a target resolved for one operation: the verified parent handle
// and the target handle opened relative to it without following a reparse
// point.
type pinned struct {
	parent windows.Handle
	h      windows.Handle
	attrs  uint32
	canon  string // DOS path of the target, from its handle
}

func (p *pinned) close() {
	if p.h != 0 {
		windows.CloseHandle(p.h)
	}
	windows.CloseHandle(p.parent)
}

// errCode maps an NTSTATUS or Win32 error to a removal code.
func errCode(err error) string {
	var st windows.NTStatus
	if errors.As(err, &st) {
		switch st {
		case windows.STATUS_OBJECT_NAME_NOT_FOUND, windows.STATUS_OBJECT_PATH_NOT_FOUND,
			windows.STATUS_NO_SUCH_FILE, windows.STATUS_DELETE_PENDING, windows.STATUS_FILE_DELETED:
			return CodeNotFound
		case windows.STATUS_FILE_IS_A_DIRECTORY, windows.STATUS_NOT_A_DIRECTORY,
			windows.STATUS_REPARSE_POINT_ENCOUNTERED, windows.STATUS_STOPPED_ON_SYMLINK:
			return CodeChanged
		case windows.STATUS_ACCESS_DENIED, windows.STATUS_CANNOT_DELETE, windows.STATUS_MEDIA_WRITE_PROTECTED:
			return CodePermission
		case windows.STATUS_NOT_SAME_DEVICE:
			return CodeMountPoint
		case windows.STATUS_OBJECT_NAME_INVALID:
			return CodeInvalidName
		}
		err = st.Errno()
	}
	switch {
	case isGone(err):
		return CodeNotFound
	case isDenied(err), errors.Is(err, windows.ERROR_WRITE_PROTECT):
		return CodePermission
	case errors.Is(err, windows.ERROR_NOT_SAME_DEVICE):
		return CodeMountPoint
	case errors.Is(err, windows.ERROR_DIRECTORY):
		return CodeChanged
	}
	return CodeFailed
}

// unsupported reports whether an NtSetInformationFile failure means the
// information class or flags are not supported (older Windows, FAT, SMB).
func unsupported(err error) bool {
	var st windows.NTStatus
	if !errors.As(err, &st) {
		return false
	}
	switch st {
	case windows.STATUS_INVALID_PARAMETER, windows.STATUS_NOT_SUPPORTED, windows.STATUS_INVALID_INFO_CLASS,
		windows.STATUS_NOT_IMPLEMENTED, windows.STATUS_INVALID_DEVICE_REQUEST:
		return true
	}
	return false
}

// ntOpen opens name relative to directory dir with NtCreateFile, never
// following a reparse point (the link itself is opened).
func ntOpen(dir windows.Handle, name string, access, options, disposition uint32) (windows.Handle, error) {
	u := wtf8ToUTF16(name)
	n := len(u) - 1
	if n == 0 || n > 32766 {
		return 0, windows.STATUS_OBJECT_NAME_INVALID
	}
	us := windows.NTUnicodeString{Length: uint16(n * 2), MaximumLength: uint16(n*2 + 2), Buffer: &u[0]}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: dir, ObjectName: &us}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err := windows.NtCreateFile(&h, access|windows.SYNCHRONIZE, &oa, &iosb, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, disposition,
		options|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	runtime.KeepAlive(u)
	if err != nil {
		return 0, err
	}
	return h, nil
}

// openRel opens an existing entry for deletion, retrying without
// FILE_WRITE_ATTRIBUTES (only needed by the legacy read-only fallback) when
// that is denied.
func openRel(dir windows.Handle, name string, access, options uint32) (windows.Handle, error) {
	h, err := ntOpen(dir, name, access|windows.FILE_WRITE_ATTRIBUTES, options, windows.FILE_OPEN)
	if errors.Is(err, windows.STATUS_ACCESS_DENIED) {
		h, err = ntOpen(dir, name, access, options, windows.FILE_OPEN)
	}
	return h, err
}

// openDirPath opens a directory by path (intermediate reparse points are
// followed, the last component is not) for relative opens and identity.
func openDirPath(path string, access uint32) (windows.Handle, error) {
	p := wtf8ToUTF16(toLongPath(path))
	return windows.CreateFile(&p[0], access|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

// attrTag returns a handle's attributes and reparse tag.
func attrTag(h windows.Handle) (attrs, tag uint32, err error) {
	var ti struct{ Attrs, Tag uint32 }
	err = windows.GetFileInformationByHandleEx(h, classAttributeTagInfo, (*byte)(unsafe.Pointer(&ti)), uint32(unsafe.Sizeof(ti)))
	return ti.Attrs, ti.Tag, err
}

// isVolumeMount reports whether a mount-point reparse point mounts a volume
// (\??\Volume{GUID}\) rather than being a junction.
func isVolumeMount(h windows.Handle) bool {
	buf := make([]byte, windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var n uint32
	if windows.DeviceIoControl(h, windows.FSCTL_GET_REPARSE_POINT, nil, 0, &buf[0], uint32(len(buf)), &n, nil) != nil || n < 16 {
		return true // can't tell: treat as a mount, never remove it
	}
	if binary.LittleEndian.Uint32(buf) != reparseTagMountPoint {
		return false
	}
	off := int(binary.LittleEndian.Uint16(buf[8:]))
	ln := int(binary.LittleEndian.Uint16(buf[10:]))
	if 16+off+ln > int(n) {
		return true
	}
	u := make([]uint16, ln/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(buf[16+off+2*i:])
	}
	return strings.HasPrefix(strings.ToLower(string(utf16.Decode(u))), `\??\volume{`)
}

// dosPath turns a final path (\\?\C:\x, \\?\UNC\srv\share\x) into a DOS path.
func dosPath(p string) string {
	if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
		return `\\` + rest
	}
	return strings.TrimPrefix(p, `\\?\`)
}

// rootCanon opens the scan root, verifies its identity, and returns its final
// path.
func rootCanon(b *Bounds) (string, error) {
	h, err := openDirPath(b.Root, 0)
	if err != nil {
		return "", rmErr(CodeOutsideRoot, b.Root, err)
	}
	defer windows.CloseHandle(h)
	if id, ok := handleIdentity(h); !ok || id != b.RootID {
		return "", rmErr(CodeOutsideRoot, b.Root, errors.New("scan root changed"))
	}
	fp, err := finalPath(h)
	if err != nil {
		return "", rmErr(CodeOutsideRoot, b.Root, err)
	}
	return fp, nil
}

func (b *Bounds) within(fp, root string) bool {
	return pathWithin(root, fp) && (b.Guard == nil || pathWithin(b.Guard.canon, fp))
}

// pin opens t's parent by path and verifies it against the recorded identity
// and, by its final path, containment in the scan root (and the guard root).
// The target is then opened relative to the parent handle with
// FILE_OPEN_REPARSE_POINT, identity-checked, and checked again by final path.
func pin(t *Target) (*pinned, error) {
	if err := preflightLoc(t); err != nil {
		return nil, err
	}
	b := &t.bounds
	root, err := rootCanon(b)
	if err != nil {
		return nil, err
	}
	ph, err := openDirPath(t.Parent, windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE)
	if err != nil {
		return nil, rmErr(errCode(err), t.Parent, err)
	}
	p := &pinned{parent: ph}
	ok := false
	defer func() {
		if !ok {
			p.close()
		}
	}()
	pid, idOK := handleIdentity(ph)
	if !idOK || pid != t.Chain[0] {
		return nil, rmErr(CodeChanged, t.Parent, nil)
	}
	if fp, err := finalPath(ph); err != nil || !b.within(fp, root) {
		return nil, rmErr(CodeOutsideRoot, t.Parent, err)
	}
	access, opts := uint32(windows.DELETE|windows.FILE_READ_ATTRIBUTES), uint32(windows.FILE_NON_DIRECTORY_FILE)
	if t.Dir {
		access |= windows.FILE_LIST_DIRECTORY | windows.FILE_TRAVERSE
		opts = windows.FILE_DIRECTORY_FILE
	}
	h, err := openRel(ph, t.Name, access, opts)
	if err != nil {
		return nil, rmErr(errCode(err), t.Path, err)
	}
	p.h = h
	id, idOK := handleIdentity(h)
	if !idOK {
		return nil, rmErr(CodeFailed, t.Path, errors.New("cannot identify"))
	}
	if !t.ID.IsZero() && id != t.ID {
		return nil, rmErr(CodeChanged, t.Path, nil)
	}
	p.attrs, _, err = attrTag(h)
	if err != nil {
		return nil, rmErr(CodeFailed, t.Path, err)
	}
	if id.Dev != pid.Dev || t.Dir && p.attrs&attrReparse != 0 {
		return nil, rmErr(CodeMountPoint, t.Path, nil)
	}
	fp, err := finalPath(h)
	if err != nil || !b.within(fp, root) {
		return nil, rmErr(CodeOutsideRoot, t.Path, err)
	}
	p.canon = dosPath(fp)
	if t.Dir {
		if id == b.RootID {
			return nil, rmErr(CodeProtected, t.Path, errors.New("scan root"))
		}
		if b.protection().covers(p.canon, id) {
			return nil, rmErr(CodeProtected, t.Path, nil)
		}
	} else {
		var si fileStandardInfo
		if err := windows.GetFileInformationByHandleEx(h, classStandardInfo, (*byte)(unsafe.Pointer(&si)), uint32(unsafe.Sizeof(si))); err != nil {
			return nil, rmErr(CodeFailed, t.Path, err)
		}
		t.ID = id
		t.Alloc, t.Apparent, t.Links = uint64(si.AllocationSize), uint64(si.EndOfFile), uint64(si.NumberOfLinks)
	}
	ok = true
	return p, nil
}

// deleteHandle deletes the open file or (empty) directory: POSIX semantics
// (the name goes at once) ignoring the read-only attribute, falling back to
// the classic disposition after clearing read-only where Ex is unsupported.
func deleteHandle(h windows.Handle, attrs uint32) error {
	var iosb windows.IO_STATUS_BLOCK
	for _, flags := range []uint32{
		windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE,
		windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE,
	} {
		err := windows.NtSetInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&flags)), 4, ntFileDispositionInformationE)
		if err == nil || !unsupported(err) {
			return err
		}
	}
	if attrs&attrReadOnly != 0 {
		bi := struct {
			Times [4]int64
			Attrs uint32
			_     uint32
		}{Attrs: attrs & attrSettable}
		if bi.Attrs == 0 {
			bi.Attrs = attrNormal
		}
		windows.NtSetInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&bi)), uint32(unsafe.Sizeof(bi)), ntFileBasicInformation)
	}
	del := uint32(1) // FILE_DISPOSITION_INFORMATION{DeleteFile: TRUE}
	return windows.NtSetInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&del)), 1, ntFileDispositionInformation)
}

func (r *remover) removeFile(p *pinned, t *Target) bool {
	if err := deleteHandle(p.h, p.attrs); err != nil {
		r.fail(t.Path, errCode(err), err)
		return false
	}
	r.removed(false, t.Alloc, t.Apparent, t.Links <= 1)
	return true
}

func (r *remover) removeTree(p *pinned, t *Target) bool {
	return r.removeDir(p.h, p.attrs, t.Path, t.ID, 0)
}

// removeDir removes everything in the verified directory handle h (identity
// id) on the same volume, then the directory. Directory reparse points
// (junctions, symlinks) are removed as links and never entered; mounted
// volumes are skipped. It reports whether the directory itself was removed.
func (r *remover) removeDir(h windows.Handle, attrs uint32, path string, id Identity, depth int) bool {
	var (
		failed map[string]struct{} // left in place; never retried
		sc     = readScratch{buf: make([]uint64, 8<<10/8)}
		en     dirEnum
	)
	for {
		progress := false
		for restart := true; ; restart = false {
			end, err := en.batch(h, id.Dev, &sc, restart, func(e *rawEntry) bool {
				if r.canceled() {
					return false
				}
				if _, bad := failed[e.Name]; bad {
					return true
				}
				if r.removeEntry(h, e, joinPath(path, e.Name), id.Dev, depth) {
					progress = true
					return true
				}
				if failed == nil {
					failed = map[string]struct{}{}
				}
				failed[e.Name] = struct{}{}
				return true
			})
			if err != nil {
				r.fail(path, errCode(err), err)
				return false
			}
			if end {
				break
			}
		}
		if !progress || r.canceled() {
			break
		}
	}
	if r.canceled() || len(failed) > 0 {
		return false
	}
	var si fileStandardInfo
	windows.GetFileInformationByHandleEx(h, classStandardInfo, (*byte)(unsafe.Pointer(&si)), uint32(unsafe.Sizeof(si)))
	if err := deleteHandle(h, attrs); err != nil {
		r.fail(path, errCode(err), err)
		return false
	}
	r.removed(true, uint64(si.AllocationSize), uint64(si.EndOfFile), true)
	return true
}

// removeEntry removes one enumerated entry of directory dh (on volume dev).
// It reports whether the entry is gone.
func (r *remover) removeEntry(dh windows.Handle, e *rawEntry, path string, dev uint64, depth int) bool {
	if e.Kind == kindDir && e.ChildFlags&proto.ChildReparse == 0 {
		if r.devOf(path, e.ID.Dev) != dev {
			r.skip(path)
			return false
		}
		if depth+1 > maxRemoveDepth {
			r.fail(path, CodeFailed, errTooDeep)
			return false
		}
		r.beforeOpen(path)
		ch, err := openRel(dh, e.Name, windows.DELETE|windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES, windows.FILE_DIRECTORY_FILE)
		if err != nil {
			if errCode(err) == CodeNotFound {
				return true
			}
			r.fail(path, errCode(err), err)
			return false
		}
		defer windows.CloseHandle(ch)
		id, ok := handleIdentity(ch)
		attrs, _, aerr := attrTag(ch)
		if !ok || id != e.ID || aerr != nil || attrs&attrReparse != 0 {
			r.fail(path, CodeChanged, nil)
			return false
		}
		return r.removeDir(ch, attrs, path, id, depth+1)
	}
	// A file, or a directory reparse point (junction, symlink): removed as
	// itself, never entered.
	opts := uint32(windows.FILE_NON_DIRECTORY_FILE)
	if e.Kind == kindDir {
		opts = 0
	}
	ch, err := openRel(dh, e.Name, windows.DELETE|windows.FILE_READ_ATTRIBUTES, opts)
	if err != nil {
		if errCode(err) == CodeNotFound {
			return true
		}
		r.fail(path, errCode(err), err)
		return false
	}
	defer windows.CloseHandle(ch)
	attrs, tag, err := attrTag(ch)
	if err != nil {
		r.fail(path, CodeFailed, err)
		return false
	}
	if e.Kind == kindDir {
		if attrs&attrReparse == 0 {
			r.fail(path, CodeChanged, nil) // a real directory replaced the link
			return false
		}
		if tag == reparseTagMountPoint && isVolumeMount(ch) {
			r.skip(path)
			return false
		}
	}
	var si fileStandardInfo
	windows.GetFileInformationByHandleEx(ch, classStandardInfo, (*byte)(unsafe.Pointer(&si)), uint32(unsafe.Sizeof(si)))
	if err := deleteHandle(ch, attrs); err != nil {
		r.fail(path, errCode(err), err)
		return false
	}
	r.removed(e.Kind == kindDir, uint64(si.AllocationSize), uint64(si.EndOfFile), si.NumberOfLinks <= 1)
	return true
}
