// SPDX-License-Identifier: MIT

package scan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

// The removal engine turns scanned entries into identity-pinned targets and
// removes them, to the Trash or permanently, without following links, leaving
// the scanned filesystem, or touching anything that is not the entry that was
// scanned:
//
//   - The parent is opened by path and verified against the identity the arena
//     recorded; containment in the scan root (and the --root guard) is proven
//     from that handle (Unix: walking ".." through the recorded ancestors;
//     Windows: the handle's final path).
//   - The target is opened relative to the parent handle without following
//     links and verified against the recorded identity. Mount points and
//     reparse points are refused as targets and never entered.
//   - Every recursive step re-verifies the identity of the directory it opens
//     and stays on the target's filesystem.
//
// Resolve* is the preflight; Remove repeats every check from scratch at the
// time of removal (approval can take minutes), so a Target holds no handles.

// RemoveMode selects where removed entries go.
type RemoveMode uint8

const (
	ModeTrash     RemoveMode = iota // move to the user's Trash / Recycle Bin
	ModePermanent                   // delete
)

// Codes carried by RemoveError and EntryError.
const (
	CodeChanged          = "changed"           // not the entry that was scanned (replaced, swapped for a link, moved)
	CodeProtected        = "protected"         // a protected folder, an ancestor of one, or the scan root
	CodeMountPoint       = "mount_point"       // a mount point, reparse point or other filesystem
	CodeNotFound         = "not_found"         // gone
	CodePermission       = "permission"        // access denied, read-only filesystem, immutable
	CodeInvalidName      = "invalid_name"      // see ValidName
	CodeOutsideRoot      = "outside_root"      // no longer inside the scan root / --root
	CodeNotScanned       = "not_scanned"       // unknown id, or announced with child flags and never scanned
	CodeTrashUnavailable = "trash_unavailable" // no usable Trash on the target's filesystem
	CodeCanceled         = "canceled"          // the context was canceled
	CodeFailed           = "failed"            // any other error
)

// RemoveError is a typed resolution or removal failure.
type RemoveError struct {
	Code string
	Path string
	Err  error // underlying error, may be nil
}

func (e *RemoveError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Path, e.Err)
	}
	return e.Code + ": " + e.Path
}

func (e *RemoveError) Unwrap() error { return e.Err }

func rmErr(code, path string, err error) *RemoveError {
	return &RemoveError{Code: code, Path: path, Err: err}
}

// ErrorCode returns the Code of a *RemoveError in err's chain, or CodeFailed.
func ErrorCode(err error) string {
	var re *RemoveError
	if errors.As(err, &re) {
		return re.Code
	}
	return CodeFailed
}

// maxNameBytes bounds an entry name.
const maxNameBytes = 4096

// ValidName reports whether name can address one entry inside a directory:
// not empty, not "." or "..", no separator or NUL (Windows: no "\" or ":",
// which would name an alternate data stream), at most 4096 bytes.
func ValidName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > maxNameBytes {
		return false
	}
	return !strings.ContainsAny(name, badNameChars)
}

// Bounds is what every target must lie within, plus the user whose folders
// are protected. Build it with NewBounds or Scanner.Bounds (which cache the
// protected set); a zero-cache Bounds recomputes it on every check.
type Bounds struct {
	Root   string     // scan root path, as scanned
	RootID Identity   // scan root identity (Scanner.DirIdentity(0))
	Guard  *RootGuard // --root guard, or nil
	Home   string     // the real user's home ("" = the current user's)

	prot *protection
}

// NewBounds builds Bounds and computes the protected set once.
func NewBounds(root string, rootID Identity, guard *RootGuard, home string) Bounds {
	b := Bounds{Root: root, RootID: rootID, Guard: guard, Home: home}
	b.prot = newProtection(home)
	return b
}

func (b *Bounds) protection() *protection {
	if b.prot != nil {
		return b.prot
	}
	return newProtection(b.Home)
}

// Loc is what the arena knows about an entry: its parent directory's path as
// scanned, the identities recorded on the way from that parent (Chain[0]) up
// toward the scan root, and the entry's name.
type Loc struct {
	Parent string
	Chain  []Identity
	Name   string
}

// Target is a resolved entry. It holds no handles: Remove re-resolves it and
// requires the same identity.
type Target struct {
	Loc
	Path     string   // Parent joined with Name (display, audit)
	Dir      bool     // a directory (else any non-directory, including a symlink)
	ID       Identity // directories: as scanned; files: as resolved
	Alloc    uint64   // files: allocated size as resolved
	Apparent uint64   // files: apparent size as resolved
	Links    uint64   // files: link count as resolved

	bounds Bounds
}

// ResolveDir pins the directory loc names, which must still be the one with
// identity want (as scanned), inside b, not a mount or reparse point, and not
// protected.
func ResolveDir(b Bounds, loc Loc, want Identity) (Target, error) {
	t := Target{Loc: loc, Path: joinPath(loc.Parent, loc.Name), Dir: true, ID: want, bounds: b}
	if want.IsZero() {
		return Target{}, rmErr(CodeChanged, t.Path, errors.New("no recorded identity"))
	}
	p, err := pin(&t)
	if err != nil {
		return Target{}, err
	}
	p.close()
	return t, nil
}

// ResolveFile pins the non-directory loc names (a symlink is fine: the link
// itself is the target) and records its identity and size.
func ResolveFile(b Bounds, loc Loc) (Target, error) {
	t := Target{Loc: loc, Path: joinPath(loc.Parent, loc.Name), bounds: b}
	p, err := pin(&t)
	if err != nil {
		return Target{}, err
	}
	p.close()
	return t, nil
}

// preflightLoc validates what every resolution needs before touching disk.
func preflightLoc(t *Target) error {
	if !ValidName(t.Name) {
		return rmErr(CodeInvalidName, t.Path, nil)
	}
	if len(t.Chain) == 0 || t.Chain[0].IsZero() || t.bounds.RootID.IsZero() {
		return rmErr(CodeChanged, t.Path, errors.New("no recorded parent identity"))
	}
	return nil
}

// ---- Scanner adapters ----

// Bounds returns removal bounds for this scan's root.
func (s *Scanner) Bounds(guard *RootGuard, home string) Bounds {
	return NewBounds(s.opts.Root, s.rootIdent, guard, home)
}

// chainOf returns the identities recorded from id up to and including the scan
// root.
func (s *Scanner) chainOf(id uint64) ([]Identity, bool) {
	var ids []Identity
	for n := 0; n <= s.opts.MaxDepth+2; n++ {
		if id == 0 {
			return append(ids, s.rootIdent), true
		}
		e, ok := s.arena.get(id)
		if !ok {
			return nil, false
		}
		ids = append(ids, e.ident)
		id = e.parent - 1
	}
	return nil, false
}

func (s *Scanner) locOf(parent uint64, name string) (Loc, bool) {
	pp, ok := s.PathOf(parent)
	if !ok {
		return Loc{}, false
	}
	chain, ok := s.chainOf(parent)
	if !ok {
		return Loc{}, false
	}
	return Loc{Parent: pp, Chain: chain, Name: name}, true
}

// ResolveDir resolves announced directory id. The scan root is refused
// (protected), as are directories announced with child flags: mount points,
// bind mounts and reparse points as mount_point, the rest as not_scanned.
func (s *Scanner) ResolveDir(b Bounds, id uint64) (Target, error) {
	if id == 0 {
		return Target{}, rmErr(CodeProtected, s.opts.Root, errors.New("scan root"))
	}
	e, ok := s.arena.get(id)
	if !ok {
		return Target{}, rmErr(CodeNotScanned, "", errors.New("unknown directory id"))
	}
	path, _ := s.PathOf(id)
	if e.flags&(proto.ChildOtherDevice|proto.ChildBindMount|proto.ChildReparse) != 0 {
		return Target{}, rmErr(CodeMountPoint, path, nil)
	}
	if e.flags != 0 {
		return Target{}, rmErr(CodeNotScanned, path, nil)
	}
	loc, ok := s.locOf(e.parent-1, e.name)
	if !ok {
		return Target{}, rmErr(CodeNotScanned, path, errors.New("unknown parent"))
	}
	return ResolveDir(b, loc, e.ident)
}

// ResolveFile resolves the non-directory name inside scanned directory
// parentID.
func (s *Scanner) ResolveFile(b Bounds, parentID uint64, name string) (Target, error) {
	if !s.DirScanned(parentID) {
		return Target{}, rmErr(CodeNotScanned, "", errors.New("parent directory was not scanned"))
	}
	loc, ok := s.locOf(parentID, name)
	if !ok {
		return Target{}, rmErr(CodeNotScanned, "", errors.New("unknown directory id"))
	}
	return ResolveFile(b, loc)
}

// ---- removal ----

// RemoveOptions configures Remove.
type RemoveOptions struct {
	Mode      RemoveMode
	Home      string    // the real user's home, whose Trash is used ("" = the current user's)
	SID       string    // Windows: the real user's SID for the Recycle Bin ("" = the process token's user)
	TrashSize uint64    // Windows: size recorded in the Recycle Bin for a directory (0 = unknown)
	MaxErrors int       // per-entry errors kept in Result.Errors (default 100)
	Live      *Counters // optional live totals, updated as entries are removed

	hooks *removeHooks
}

// Counters are live permanent-removal totals a caller may poll.
type Counters struct {
	FreedAlloc, Files, Dirs atomic.Uint64
}

// removeHooks are test hooks.
type removeHooks struct {
	beforeOpen func(path string)                    // before a directory inside a permanent removal is opened
	devOf      func(path string, dev uint64) uint64 // device of a subdirectory, as compared with its parent's
	recycleDir string                               // Windows: use this directory as the Recycle Bin
}

// EntryError is one entry that could not be removed.
type EntryError struct {
	Path, Code, Message string
}

// Result reports one Remove. Counts are valid even when Remove returns an
// error (a partial permanent removal).
type Result struct {
	OK      bool // the target is gone (permanent) or in the Trash
	Partial bool // permanent: some contents were removed but not the target

	// Permanent: what was removed. Bytes count only entries whose last link
	// went (hardlinked files elsewhere free nothing). Trash: a file reports
	// its own size and Files=1; a directory reports Dirs=1 and no sizes (the
	// caller knows the scanned totals).
	FreedAlloc, FreedApparent uint64
	Files, Dirs               uint64

	Skipped      []string // mount points / other filesystems left in place (capped)
	SkippedCount int
	Errors       []EntryError // capped at MaxErrors
	ErrorCount   int

	Where string // Trash: where the target now is
}

// maxRemoveDepth bounds recursion of a permanent removal.
const maxRemoveDepth = 4096

var errTooDeep = errors.New("directory nesting too deep")

type remover struct {
	ctx  context.Context
	opts *RemoveOptions
	res  Result
	max  int
}

func (r *remover) canceled() bool { return r.ctx.Err() != nil }

func (r *remover) fail(path, code string, err error) {
	r.res.ErrorCount++
	if len(r.res.Errors) < r.max {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		r.res.Errors = append(r.res.Errors, EntryError{Path: path, Code: code, Message: msg})
	}
}

func (r *remover) skip(path string) {
	r.res.SkippedCount++
	if len(r.res.Skipped) < r.max {
		r.res.Skipped = append(r.res.Skipped, path)
	}
}

// removed accounts for one removed entry.
func (r *remover) removed(dir bool, alloc, apparent uint64, lastLink bool) {
	if dir {
		r.res.Dirs++
	} else {
		r.res.Files++
	}
	if lastLink {
		r.res.FreedAlloc += alloc
		r.res.FreedApparent += apparent
	}
	if l := r.opts.Live; l != nil {
		if dir {
			l.Dirs.Add(1)
		} else {
			l.Files.Add(1)
		}
		if lastLink {
			l.FreedAlloc.Add(alloc)
		}
	}
}

func (r *remover) devOf(path string, dev uint64) uint64 {
	if h := r.opts.hooks; h != nil && h.devOf != nil {
		return h.devOf(path, dev)
	}
	return dev
}

func (r *remover) beforeOpen(path string) {
	if h := r.opts.hooks; h != nil && h.beforeOpen != nil {
		h.beforeOpen(path)
	}
}

// Remove re-resolves t (every check of Resolve* again, plus the same identity)
// and removes it. On success it returns OK and a nil error. Otherwise the
// error is a *RemoveError (resolution failures, trash_unavailable, or for a
// partial permanent removal the first entry error's code, mount_point when
// only mounts were skipped, or canceled) and Result still reports what was
// done.
func Remove(ctx context.Context, t Target, opts RemoveOptions) (Result, error) {
	r := &remover{ctx: ctx, opts: &opts, max: opts.MaxErrors}
	if r.max <= 0 {
		r.max = 100
	}
	if r.canceled() {
		return r.res, rmErr(CodeCanceled, t.Path, ctx.Err())
	}
	if t.ID.IsZero() {
		return r.res, rmErr(CodeChanged, t.Path, errors.New("unresolved target"))
	}
	p, err := pin(&t)
	if err != nil {
		return r.res, err
	}
	defer p.close()
	if opts.Mode == ModeTrash {
		where, err := r.trash(p, &t)
		r.res.Where = where
		if err != nil {
			return r.res, err
		}
		r.res.OK = true
		if t.Dir {
			r.res.Dirs = 1
		} else {
			r.res.Files = 1
			r.res.FreedAlloc, r.res.FreedApparent = t.Alloc, t.Apparent
		}
		return r.res, nil
	}
	var done bool
	if t.Dir {
		done = r.removeTree(p, &t)
	} else {
		done = r.removeFile(p, &t)
	}
	return r.finish(done, t.Path)
}

func (r *remover) finish(done bool, path string) (Result, error) {
	if done {
		r.res.OK = true
		return r.res, nil
	}
	r.res.Partial = r.res.Files+r.res.Dirs > 0
	code := CodeFailed
	switch {
	case r.canceled():
		code = CodeCanceled
	case len(r.res.Errors) > 0:
		code = r.res.Errors[0].Code
	case r.res.SkippedCount > 0:
		code = CodeMountPoint
	}
	return r.res, rmErr(code, path, nil)
}

// trashName returns the n-th candidate name in a Trash: name, then
// "name 2", "name 3"... with the suffix before a file's extension, shortened
// to at most max bytes.
func trashName(name string, n int, dir bool, max int) string {
	if n <= 1 && len(name) <= max {
		return name
	}
	base, ext := name, ""
	if !dir {
		if i := strings.LastIndexByte(name, '.'); i > 0 && len(name)-i <= 32 {
			base, ext = name[:i], name[i:]
		}
	}
	suffix := ""
	if n > 1 {
		suffix = fmt.Sprintf(" %d", n)
	}
	if over := len(base) + len(suffix) + len(ext) - max; over > 0 {
		cut := len(base) - over
		if cut < 1 {
			base, ext = name, ""
			cut = max - len(suffix)
		}
		base = truncUTF8(base, cut)
	}
	return base + suffix + ext
}

// truncUTF8 cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncUTF8(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// homeDir returns h, or the current user's home.
func homeDir(h string) (string, error) {
	if h != "" {
		return h, nil
	}
	return os.UserHomeDir()
}
