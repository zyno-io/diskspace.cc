// SPDX-License-Identifier: MIT

package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zyno-io/diskspace.cc/internal/prio"
	"github.com/zyno-io/diskspace.cc/internal/proto"
	"github.com/zyno-io/diskspace.cc/internal/scan"
)

// Deleting things a viewer asked for (docs/protocol.md, "Deletion"). The
// viewer only names scanned entries by id; the agent resolves and measures
// them itself and asks the person at the scanned computer through
// Config.Confirm, a channel web content can't drive. Nothing is touched
// before that approval, and the removal engine re-checks every entry when
// it runs.

// Abuse limits and timings (vars for tests).
var (
	confirmTimeout   = 2 * time.Minute  // an unanswered prompt is refused
	refuseCooldown   = 30 * time.Second // after a refusal, before the next request
	maxRefusals      = 3                // refusals per agent run before deletion is off
	preflightBudget  = 2 * time.Second  // measuring the requested folders
	deleteStatusRate = 250 * time.Millisecond
)

const (
	maxConfirmFolders = 40  // parent folders listed in a prompt
	maxShownPath      = 512 // bytes of one path in a prompt
	// LargeBytes and LargeFiles: a permanent deletion above either needs a
	// second confirmation on the host.
	LargeBytes = 100e9
	LargeFiles = 100_000
	// deletedChunk bounds one DELETED message body.
	deletedChunk = 256 << 10
)

// hookBeforeRemove is a test hook called before each item is removed.
var hookBeforeRemove func(path string)

// Confirmation errors. A Confirm func returns (false, nil) when the person
// declined, ErrConfirmTimeout when nobody answered, and ctx.Err() when the
// request was withdrawn.
var (
	ErrConfirmTimeout = errors.New("session: confirmation timed out")

	errWithdrawn = errors.New("withdrawn by the viewer")
	errDeleting  = errors.New("a deletion is in progress")
)

// ConfirmRequest is what the person at the scanned computer is asked to
// approve. Every string is display-safe (SafeText) and every size was
// measured by the agent, never taken from the viewer.
type ConfirmRequest struct {
	Job   uint64        `json:"job"`
	Mode  string        `json:"mode"`  // trash | permanent
	Items []ConfirmItem `json:"items"` // every item, largest first
	More  int           `json:"more"`  // items left out of Items (to fit an IPC message)
	// Folders holds the parent folders of all items, largest first (at most
	// 40; MoreFolders counts the rest).
	Folders     []ConfirmFolder `json:"folders"`
	MoreFolders int             `json:"moreFolders"`
	Count       int             `json:"count"` // items that will be deleted
	Skipped     int             `json:"skipped"`
	Alloc       uint64          `json:"alloc"`
	Files       uint64          `json:"files"`
	Dirs        uint64          `json:"dirs"`
	AtLeast     bool            `json:"atLeast"` // measuring stopped early: totals are lower bounds
	Large       bool            `json:"large"`   // permanent and large: ask a second time
	Warnings    []string        `json:"warnings,omitempty"`
	Code        string          `json:"code"`     // the requesting viewer's verification code
	ViaRelay    bool            `json:"viaRelay"` // the viewer is connected through the relay
	Trash       string          `json:"trash"`    // "Trash" or "Recycle Bin"
}

// ConfirmFolder is a folder that holds Count of the items.
type ConfirmFolder struct {
	Path  string `json:"path"`
	Count int    `json:"count"`
	Alloc uint64 `json:"alloc"`
}

// ConfirmItem is one listed path.
type ConfirmItem struct {
	Path    string `json:"path"`
	Dir     bool   `json:"dir"`
	Alloc   uint64 `json:"alloc"`
	Files   uint64 `json:"files"`
	Dirs    uint64 `json:"dirs"`
	AtLeast bool   `json:"atLeast"`
}

// TrashName is the platform's name for the Trash.
func TrashName(goos string) string {
	if goos == "windows" {
		return "Recycle Bin"
	}
	return "Trash"
}

// ---- per-scan directory marks ----

// dirMarks records which announced directories got their DIR record and
// which of those could not be read or had vanished. The scanner's arena
// does not keep read flags. Written by the scan goroutine; read only after
// the scan has ended (deletion waits for r.done).
type dirMarks struct{ rec, bad []uint64 }

func setBit(b *[]uint64, id uint64) {
	i := id >> 6
	for uint64(len(*b)) <= i {
		*b = append(*b, 0)
	}
	(*b)[i] |= 1 << (id & 63)
}

func hasBit(b []uint64, id uint64) bool {
	i := id >> 6
	return i < uint64(len(b)) && b[i]&(1<<(id&63)) != 0
}

func (m *dirMarks) note(rec proto.Record) {
	if d, ok := rec.(*proto.DirRecord); ok && d.ID <= proto.MaxScanDirs {
		setBit(&m.rec, d.ID)
		if d.Flags&(proto.DirReadDenied|proto.DirReadError|proto.DirVanished) != 0 {
			setBit(&m.bad, d.ID)
		}
	}
}

// ---- jobs ----

type preflight struct {
	alloc, apparent, files, dirs uint64 // dirs: below the item
	atLeast, cloud               bool
	mounts, denied               uint64
}

type delItem struct {
	i      int    // index in DELETE_REQ.items
	dir    bool   // {dir: id}
	id     uint64 // the dir, or a file's parent
	name   string // file name
	cover  int    // index of the queued folder that contains this item, or -1
	target scan.Target
	ready  bool // resolved
	code   string
	msg    string
	pre    preflight
}

type delJob struct {
	a     *Agent
	id    uint64
	reqID uint64
	mode  scan.RemoveMode
	from  *viewerConn
	run   *scanRun
	items []*delItem

	pctx    context.Context // until approval: the requester's connection
	pcancel context.CancelCauseFunc
	stop    atomic.Bool // stop after the current item

	mu        sync.Mutex
	state     string
	done      int
	freed     uint64
	live      *scan.Counters
	current   string
	where     string
	code, msg string
	results   []proto.DeleteResult
}

func (j *delJob) modeName() string {
	if j.mode == scan.ModePermanent {
		return proto.DeleteModePermanent
	}
	return proto.DeleteModeTrash
}

func (j *delJob) statusLocked() proto.DeleteStatus {
	freed := j.freed
	if j.live != nil {
		freed += j.live.FreedAlloc.Load()
	}
	return proto.DeleteStatus{
		Job: j.id, ReqID: j.reqID, State: j.state, Where: j.where,
		Total: len(j.items), Done: j.done, FreedAlloc: freed, Current: j.current,
		Code: j.code, Message: j.msg, Results: j.results,
	}
}

func terminal(state string) bool {
	return state == proto.DelDone || state == proto.DelRefused || state == proto.DelFailed
}

// deleteFeature is AGENT_INFO.features.delete.
func (a *Agent) deleteFeature() *proto.DeleteFeature {
	f := &proto.DeleteFeature{Available: a.cfg.Confirm != nil, Trash: true, MaxItems: proto.MaxDeleteItems}
	if f.Available {
		f.Confirm = a.cfg.ConfirmKind
		if f.Confirm == "" {
			f.Confirm = "terminal"
		}
	} else {
		f.Reason = "no_confirm"
	}
	return f
}

// deletingLocked is AGENT_INFO.deleting; a.mu must be held.
func (a *Agent) deletingLocked() *proto.Deleting {
	j := a.del
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if terminal(j.state) {
		return nil
	}
	return &proto.Deleting{Job: j.id, State: j.state, Done: j.done, Total: len(j.items)}
}

func (a *Agent) home() string {
	if a.cfg.Home != "" {
		return a.cfg.Home
	}
	h, _ := os.UserHomeDir()
	return h
}

// sendDelStatus sends a job's status to the active viewer and, while it is
// still connected, to the requester. Non-terminal statuses coalesce; a
// terminal one is queued so it can't be lost.
func (a *Agent) sendDelStatus(j *delJob) {
	j.mu.Lock()
	st := j.statusLocked()
	j.mu.Unlock()
	a.sendStatusTo(j.from, st)
}

func (a *Agent) sendStatusTo(from *viewerConn, st proto.DeleteStatus) {
	body, err := proto.EncodeMsg(proto.MsgDeleteStatus, st)
	if err != nil {
		return
	}
	a.mu.Lock()
	var targets []*sender
	if act := a.active; act != nil && act.snd != nil {
		targets = append(targets, act.snd)
	}
	if from != nil && from != a.active && from.ctx.Err() == nil {
		if _, ok := a.viewers[from]; ok && from.snd != nil {
			targets = append(targets, from.snd)
		}
	}
	a.mu.Unlock()
	for _, s := range targets {
		if terminal(st.State) {
			s.pushDeleteFinal(st.Job, body)
		} else {
			s.setDeleteStatus(st.Job, body)
		}
	}
}

// requestDelete handles DELETE_REQ. The cheap checks run here; resolving,
// measuring, asking and deleting run in the job's goroutine, so the command
// loop never waits for an approval.
func (a *Agent) requestDelete(v *viewerConn, m proto.DeleteReq) {
	a.scanMu.Lock()
	defer a.scanMu.Unlock()

	a.mu.Lock()
	a.delSeq++
	id := a.delSeq
	busy := a.del != nil
	strikes, cool := a.delStrikes, a.delCooldown
	a.mu.Unlock()

	refuse := func(state, code, msg string) {
		a.sendStatusTo(v, proto.DeleteStatus{Job: id, ReqID: m.ReqID, State: state, Total: len(m.Items), Code: code, Message: msg})
	}
	var mode scan.RemoveMode
	switch m.Mode {
	case proto.DeleteModeTrash:
		mode = scan.ModeTrash
	case proto.DeleteModePermanent:
		mode = scan.ModePermanent
	default:
		refuse(proto.DelFailed, "invalid", "unknown mode")
		return
	}
	run := a.currentRun()
	switch {
	case a.cfg.Confirm == nil:
		refuse(proto.DelRefused, "no_confirm", "")
		return
	case busy:
		refuse(proto.DelFailed, "busy", "")
		return
	case strikes >= maxRefusals:
		refuse(proto.DelRefused, "disabled", "")
		return
	case time.Now().Before(cool):
		refuse(proto.DelRefused, "cooldown", "")
		return
	case len(m.Items) == 0:
		refuse(proto.DelFailed, "invalid", "no items")
		return
	case len(m.Items) > proto.MaxDeleteItems:
		refuse(proto.DelFailed, "invalid", fmt.Sprintf("at most %d items per request", proto.MaxDeleteItems))
		return
	case run == nil || run.id != m.ScanID:
		refuse(proto.DelFailed, "invalid", "not the current scan; scan again")
		return
	}
	switch run.getState() {
	case "done", "canceled":
	case "running":
		refuse(proto.DelRefused, "scanning", "")
		return
	default:
		refuse(proto.DelFailed, "invalid", "the scan failed; scan again")
		return
	}
	items := make([]*delItem, len(m.Items))
	for i, it := range m.Items {
		d, msg := parseItem(i, it)
		if d == nil {
			refuse(proto.DelFailed, "invalid", fmt.Sprintf("item %d: %s", i+1, msg))
			return
		}
		items[i] = d
	}

	j := &delJob{a: a, id: id, reqID: m.ReqID, mode: mode, from: v, run: run, items: items, state: proto.DelPreparing}
	j.pctx, j.pcancel = context.WithCancelCause(v.ctx)
	a.mu.Lock()
	if a.delClosed { // shutting down
		a.mu.Unlock()
		j.pcancel(nil)
		return
	}
	a.del = j
	a.delWG.Add(1) // under a.mu: never after waitDeletes started waiting
	a.mu.Unlock()
	a.sendDelStatus(j)
	go a.runJob(j)
}

// parseItem checks one item's form. It returns nil and why for a malformed
// item (the whole request is refused).
func parseItem(i int, it proto.DeleteItem) (*delItem, string) {
	switch {
	case it.Dir != nil && it.Parent == nil && it.Name == nil:
		if *it.Dir == 0 {
			return nil, "the scanned folder itself can't be deleted"
		}
		return &delItem{i: i, dir: true, id: *it.Dir, cover: -1}, ""
	case it.Dir == nil && it.Parent != nil && it.Name != nil:
		if !scan.ValidName(*it.Name) {
			return nil, "invalid name"
		}
		return &delItem{i: i, id: *it.Parent, name: *it.Name, cover: -1}, ""
	}
	return nil, "an item names either {dir} or {parent, name}"
}

// cancelDelete handles DELETE_CANCEL from the active viewer or the requester.
func (a *Agent) cancelDelete(v *viewerConn, job uint64) {
	a.mu.Lock()
	j := a.del
	active := a.active == v
	a.mu.Unlock()
	if j == nil || j.id != job || (!active && v != j.from) {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	switch j.state {
	case proto.DelRunning:
		j.stop.Store(true)
	case proto.DelPreparing, proto.DelAwaiting:
		j.pcancel(errWithdrawn)
	}
}

func (j *delJob) setState(state string) {
	j.mu.Lock()
	j.state = state
	j.mu.Unlock()
	j.a.sendDelStatus(j)
}

// finish ends the job with a terminal state and frees the slot.
func (a *Agent) finishJob(j *delJob, state, code, msg string) {
	j.mu.Lock()
	j.state, j.code, j.msg, j.current = state, code, msg, ""
	j.live = nil
	j.mu.Unlock()
	a.mu.Lock()
	if a.del == j {
		a.del = nil
	}
	a.mu.Unlock()
	a.sendDelStatus(j)
}

// refused records a prompt that ended without approval: every one starts the
// cooldown; a decline or a timeout also counts toward disabling deletion.
func (a *Agent) refused(strike bool) {
	a.mu.Lock()
	a.delCooldown = time.Now().Add(refuseCooldown)
	if strike {
		a.delStrikes++
	}
	a.mu.Unlock()
}

func (a *Agent) runJob(j *delJob) {
	defer a.delWG.Done()
	defer j.pcancel(nil)
	<-j.run.done // the log has its end message (the annex follows it)

	bounds := j.run.scanner.Bounds(a.guard, a.home())
	if !a.prepare(j, bounds) {
		a.finishJob(j, proto.DelRefused, "canceled", "")
		return
	}
	req := a.confirmRequest(j)
	if req.Count == 0 {
		setPrepResults(j)
		a.finishJob(j, proto.DelFailed, "nothing_deletable", "None of the items can be deleted.")
		return
	}

	j.setState(proto.DelAwaiting)
	cctx, ccancel := context.WithTimeout(j.pctx, confirmTimeout)
	ok, err := a.cfg.Confirm(cctx, req)
	timedOut := errors.Is(err, ErrConfirmTimeout) || (errors.Is(cctx.Err(), context.DeadlineExceeded) && j.pctx.Err() == nil)
	ccancel()

	j.mu.Lock()
	withdrawn := j.pctx.Err() != nil
	approved := ok && err == nil && !withdrawn && !timedOut
	if approved {
		j.state = proto.DelRunning // under j.mu: a cancel from now on stops after the current item
	}
	j.mu.Unlock()
	if !approved {
		switch {
		case withdrawn:
			a.refused(false)
			a.finishJob(j, proto.DelRefused, "canceled", "")
		case timedOut:
			a.refused(true)
			a.finishJob(j, proto.DelRefused, "timeout", "")
		case err != nil:
			a.finishJob(j, proto.DelRefused, "no_confirm", SafeText(err.Error(), 200))
		default:
			a.refused(true)
			a.finishJob(j, proto.DelRefused, "declined", "")
		}
		return
	}
	a.execute(j, req)
}

// prepare validates every item against the scan and resolves it (identity,
// containment, protected folders), then measures folders within the
// preflight budget. It returns false if the request was withdrawn meanwhile.
func (a *Agent) prepare(j *delJob, b scan.Bounds) bool {
	sc, marks := j.run.scanner, &j.run.marks
	// Duplicates go with their first occurrence.
	dirs := map[uint64]int{}
	files := map[string]int{}
	for _, it := range j.items {
		key := fmt.Sprintf("f%d/%s", it.id, it.name)
		if it.dir {
			key = fmt.Sprintf("d%d", it.id)
		}
		if k, dup := files[key]; dup {
			it.cover = k
		} else {
			files[key] = it.i
		}
	}
	for _, it := range j.items {
		if j.pctx.Err() != nil {
			return false
		}
		if it.cover >= 0 {
			continue
		}
		var err error
		switch {
		case !knownDir(sc, it.id):
			it.code, it.msg = "not_scanned", "unknown folder"
		case !sc.DirScanned(it.id):
			// announced with child flags (other volume, link, duplicate): never scanned
			it.code, it.msg = "not_scanned", "this folder was not scanned"
			if it.dir {
				if _, rerr := sc.ResolveDir(b, it.id); rerr != nil { // no disk access for flagged dirs
					it.code, it.msg = itemCode(scan.ErrorCode(rerr)), ""
				}
			}
		case hasBit(marks.bad, it.id):
			it.code, it.msg = "permission", "its folder could not be read during the scan"
		case !hasBit(marks.rec, it.id):
			it.code, it.msg = "not_scanned", "not scanned yet"
		case it.dir:
			it.target, err = sc.ResolveDir(b, it.id)
		default:
			it.target, err = sc.ResolveFile(b, it.id, it.name)
		}
		if err != nil {
			it.code, it.msg = itemCode(scan.ErrorCode(err)), errText(err)
			continue
		}
		if it.code == "" {
			it.ready = true
			if it.dir {
				dirs[it.id] = it.i
			}
		}
	}
	// Items inside another requested folder that can be deleted go with it.
	for _, it := range j.items {
		if it.cover >= 0 {
			continue
		}
		p, ok := it.id, !it.dir
		if it.dir {
			p, ok = sc.ParentOf(it.id)
		}
		for n := 0; ok && n <= 4096; n++ {
			if k, hit := dirs[p]; hit && k != it.i {
				it.cover = k
				break
			}
			p, ok = sc.ParentOf(p)
		}
	}
	// A covering item may itself be covered: point at the outermost.
	for _, it := range j.items {
		for n := 0; it.cover >= 0 && j.items[it.cover].cover >= 0 && n < len(j.items); n++ {
			it.cover = j.items[it.cover].cover
		}
	}
	deadline := time.Now().Add(preflightBudget)
	for _, it := range j.items {
		if j.pctx.Err() != nil {
			return false
		}
		switch {
		case it.cover >= 0 || !it.ready:
		case it.dir:
			it.pre = measure(j.pctx, it.target, deadline)
		default:
			it.pre = preflight{alloc: it.target.Alloc, apparent: it.target.Apparent, files: 1}
		}
	}
	return j.pctx.Err() == nil
}

// displayPath is an item's path for the audit trail (resolved when possible).
func (it *delItem) displayPath(sc *scan.Scanner) string {
	if it.ready {
		return SafeText(it.target.Path, 4096)
	}
	p, _ := sc.PathOf(it.id)
	if !it.dir {
		p = filepath.Join(p, it.name)
	}
	return SafeText(p, 4096)
}

func knownDir(sc *scan.Scanner, id uint64) bool {
	_, ok := sc.PathOf(id)
	return ok
}

// measure scans a resolved folder (identity-pinned, one filesystem, low
// priority) until the deadline: its true size, and what a removal would
// leave behind.
func measure(ctx context.Context, t scan.Target, deadline time.Time) preflight {
	pre := preflight{atLeast: true}
	if time.Until(deadline) <= 0 {
		return pre
	}
	sc, err := scan.New(scan.Options{Root: t.Path, ExpectRoot: t.ID, Threads: 4, Priority: prio.Low})
	if err != nil {
		return pre
	}
	mctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	st, err := sc.Run(mctx, func(rec proto.Record) {
		if d, ok := rec.(*proto.DirRecord); ok {
			for _, f := range d.Files {
				if f.Flags&proto.FileCloud != 0 {
					pre.cloud = true
				}
			}
			for _, c := range d.Dirs {
				if c.Flags&(proto.ChildOtherDevice|proto.ChildBindMount|proto.ChildReparse) != 0 {
					pre.mounts++
				}
			}
		}
	})
	pre.alloc, pre.apparent, pre.files = st.Alloc, st.Apparent, st.Files
	if st.Dirs > 0 {
		pre.dirs = st.Dirs - 1
	}
	pre.denied = st.Denied
	pre.atLeast = err != nil
	return pre
}

func (a *Agent) confirmRequest(j *delJob) ConfirmRequest {
	req := ConfirmRequest{
		Job: j.id, Mode: j.modeName(), Code: j.from.code, ViaRelay: j.from.viaRelay,
		Trash: TrashName(goos),
	}
	var mounts, denied uint64
	var cloud bool
	var folders []ConfirmFolder
	folderIdx := map[string]int{}
	for _, it := range j.items {
		switch {
		case it.cover >= 0:
			continue
		case !it.ready:
			req.Skipped++
			continue
		}
		req.Count++
		p := it.pre
		req.Alloc += p.alloc
		req.Files += p.files
		req.Dirs += p.dirs
		if it.dir {
			req.Dirs++
		}
		req.AtLeast = req.AtLeast || p.atLeast
		mounts += p.mounts
		denied += p.denied
		cloud = cloud || p.cloud
		// every item is listed (the terminal shows them all; a dialog shows
		// the largest and the folders below), so the request order can't
		// push one out of sight
		req.Items = append(req.Items, ConfirmItem{
			Path: SafeText(it.target.Path, maxShownPath), Dir: it.dir,
			Alloc: p.alloc, Files: p.files, Dirs: p.dirs, AtLeast: p.atLeast,
		})
		fp := filepath.Dir(it.target.Path)
		k, ok := folderIdx[fp]
		if !ok {
			k = len(folders)
			folderIdx[fp] = k
			folders = append(folders, ConfirmFolder{Path: SafeText(fp, maxShownPath)})
		}
		folders[k].Count++
		folders[k].Alloc += p.alloc
	}
	sort.SliceStable(req.Items, func(a, b int) bool { return req.Items[a].Alloc > req.Items[b].Alloc })
	sort.SliceStable(folders, func(a, b int) bool { return folders[a].Alloc > folders[b].Alloc })
	if len(folders) > maxConfirmFolders {
		req.MoreFolders = len(folders) - maxConfirmFolders
		folders = folders[:maxConfirmFolders]
		if j.mode == scan.ModePermanent {
			req.Large = true // too spread out to show: ask twice
		}
	}
	req.Folders = folders
	if j.mode == scan.ModePermanent {
		req.Large = req.Large || req.Alloc > LargeBytes || req.Files > LargeFiles || req.AtLeast
	}
	if req.Skipped > 0 {
		req.Warnings = append(req.Warnings, fmt.Sprintf("%d of the requested items can't be deleted and will be left alone.", req.Skipped))
	}
	if mounts > 0 {
		req.Warnings = append(req.Warnings, "Contains another volume or mount point, which is left alone.")
	}
	if denied > 0 && j.mode == scan.ModePermanent {
		req.Warnings = append(req.Warnings, fmt.Sprintf("%d folders inside can't be read; their contents may stay behind.", denied))
	}
	if cloud {
		svc := "iCloud"
		if goos == "windows" {
			svc = "OneDrive"
		}
		req.Warnings = append(req.Warnings, "Includes "+svc+" files: deleting them removes them on all your devices.")
	}
	return req
}

// execute runs an approved job. It is bound to the agent's lifetime, not to
// any viewer: it survives disconnects and supersedes; DELETE_CANCEL stops it
// after the current item and Ctrl-C after the current entry.
func (a *Agent) execute(j *delJob, req ConfirmRequest) {
	jctx, cancel := context.WithCancel(a.life)
	defer cancel()
	a.emit(Event{Kind: EvDeleteStarted, Message: startedText(req)})
	a.sendDelStatus(j)

	tickDone := make(chan struct{})
	stopTick := make(chan struct{})
	go func() {
		defer close(tickDone)
		t := time.NewTicker(deleteStatusRate)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				a.sendDelStatus(j)
			case <-stopTick:
				return
			}
		}
	}()

	results := make([]proto.DeleteResult, len(j.items))
	for i := range results {
		results[i] = proto.DeleteResult{I: i}
	}
	var deleted []proto.DeletedItem
	var freed uint64
	var okCount, ran, trashUnavail int
	j.mu.Lock()
	for _, it := range j.items {
		if it.cover >= 0 || !it.ready {
			j.done++ // counted as finished from the start, so progress reaches the total
		}
	}
	j.mu.Unlock()
	sid := a.cfg.SID
	for _, it := range j.items {
		r := &results[it.i]
		if it.cover >= 0 {
			continue
		}
		if !it.ready {
			r.Code, r.Message = it.code, it.msg
			a.emit(Event{Kind: EvDeleteItem, Path: it.displayPath(j.run.scanner), Message: auditText(j, r)})
			continue
		}
		if j.stop.Load() || jctx.Err() != nil {
			r.Code = "canceled"
			a.emit(Event{Kind: EvDeleteItem, Path: it.displayPath(j.run.scanner), Message: auditText(j, r)})
			continue
		}
		live := &scan.Counters{}
		j.mu.Lock()
		j.current, j.live = SafeText(it.target.Path, 4096), live
		j.mu.Unlock()
		opts := scan.RemoveOptions{Mode: j.mode, Home: a.cfg.Home, SID: sid, Live: live}
		if it.dir && j.mode == scan.ModeTrash && !it.pre.atLeast {
			opts.TrashSize = it.pre.apparent
		}
		if hookBeforeRemove != nil {
			hookBeforeRemove(it.target.Path)
		}
		res, err := scan.Remove(jctx, it.target, opts)
		ran++
		var d *proto.DeletedItem
		switch {
		case err == nil && res.OK:
			okCount++
			r.OK = true
			if j.mode == scan.ModeTrash && it.dir {
				// a moved folder reports no sizes: what the agent measured
				r.FreedAlloc, r.Files, r.Dirs = it.pre.alloc, it.pre.files, it.pre.dirs
				d = dirTomb(it.id, false, it.pre.alloc, it.pre.apparent, it.pre.files, it.pre.dirs)
			} else if it.dir {
				dirs := res.Dirs
				if dirs > 0 {
					dirs-- // the folder itself
				}
				r.FreedAlloc, r.Files, r.Dirs = res.FreedAlloc, res.Files, dirs
				d = dirTomb(it.id, false, res.FreedAlloc, res.FreedApparent, res.Files, dirs)
			} else {
				r.FreedAlloc, r.Files = res.FreedAlloc, 1
				d = fileTomb(it.id, it.name, res.FreedAlloc, res.FreedApparent)
			}
			if res.Where != "" {
				j.mu.Lock()
				if j.where == "" {
					j.where = SafeText(filepath.Dir(res.Where), 4096)
				}
				j.mu.Unlock()
			}
		case res.Partial && it.dir:
			r.Code, r.Message = "partial", errText(err)
			r.FreedAlloc, r.Files, r.Dirs = res.FreedAlloc, res.Files, res.Dirs
			d = dirTomb(it.id, true, res.FreedAlloc, res.FreedApparent, res.Files, res.Dirs)
		default:
			code := scan.ErrorCode(err)
			if code == scan.CodeTrashUnavailable {
				trashUnavail++
			}
			r.Code, r.Message = itemCode(code), errText(err)
			if res.Files+res.Dirs > 0 { // a file target is all or nothing; be safe anyway
				r.FreedAlloc, r.Files, r.Dirs = res.FreedAlloc, res.Files, res.Dirs
			}
		}
		freed += r.FreedAlloc
		if d != nil {
			deleted = append(deleted, *d)
		}
		a.emit(Event{Kind: EvDeleteItem, Path: SafeText(it.target.Path, 4096), Message: auditText(j, r)})
		j.mu.Lock()
		j.done++
		j.freed, j.live, j.current = freed, nil, ""
		j.mu.Unlock()
	}
	// items inside another requested folder share its outcome
	for _, it := range j.items {
		if it.cover >= 0 {
			c := results[it.cover]
			results[it.i] = proto.DeleteResult{I: it.i, OK: c.OK, Code: c.Code, Message: c.Message}
			a.emit(Event{Kind: EvDeleteItem, Path: it.displayPath(j.run.scanner),
				Message: "requested again or inside " + j.items[it.cover].displayPath(j.run.scanner) + " (same outcome)"})
		}
	}
	close(stopTick)
	<-tickDone

	if len(deleted) > 0 {
		a.appendDeleted(j, deleted)
	}
	j.mu.Lock()
	j.results = results
	j.done = len(j.items)
	j.mu.Unlock()
	state, code := proto.DelDone, ""
	if okCount == 0 && ran > 0 && trashUnavail == ran && j.mode == scan.ModeTrash {
		state, code = proto.DelFailed, "trash_unavailable"
	}
	a.finishJob(j, state, code, "")
	a.emit(Event{Kind: EvDeleteDone, Message: doneText(j, okCount, ran, freed)})
}

// setPrepResults records why each item can't be deleted (nothing ran).
func setPrepResults(j *delJob) {
	results := make([]proto.DeleteResult, len(j.items))
	for _, it := range j.items {
		results[it.i] = proto.DeleteResult{I: it.i, Code: it.code, Message: it.msg}
		if it.cover >= 0 {
			c := j.items[it.cover]
			results[it.i].Code, results[it.i].Message = c.code, c.msg
		}
	}
	j.mu.Lock()
	j.results = results
	j.mu.Unlock()
}

func u64(v uint64) *uint64 { return &v }

func dirTomb(id uint64, partial bool, alloc, apparent, files, dirs uint64) *proto.DeletedItem {
	return &proto.DeletedItem{Dir: u64(id), Partial: partial, FreedAlloc: alloc, FreedApparent: apparent, Files: u64(files), Dirs: u64(dirs)}
}

func fileTomb(parent uint64, name string, alloc, apparent uint64) *proto.DeletedItem {
	n := name
	return &proto.DeletedItem{Parent: u64(parent), Name: &n, FreedAlloc: alloc, FreedApparent: apparent}
}

// appendDeleted adds DELETED to the scan log's annex, chunked so that no
// message exceeds the body limit.
func (a *Agent) appendDeleted(j *delJob, items []proto.DeletedItem) {
	for len(items) > 0 {
		n, size := 0, 64
		for n < len(items) {
			sz := 160
			if items[n].Name != nil {
				sz += 6 * len(*items[n].Name) // worst-case JSON escaping
			}
			if n > 0 && size+sz > deletedChunk {
				break
			}
			size += sz
			n++
		}
		body, err := proto.EncodeMsg(proto.MsgDeleted, proto.Deleted{ScanID: j.run.id, Job: j.id, Items: items[:n]})
		if err == nil && len(body) <= proto.MaxBody {
			j.run.log.appendAnnex(body)
		}
		items = items[n:]
	}
}

// itemCode maps a removal engine code onto the viewer's item codes.
func itemCode(c string) string {
	switch c {
	case scan.CodeMountPoint:
		return "mount"
	case scan.CodeOutsideRoot:
		return "changed"
	}
	return c
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	var re *scan.RemoveError
	if errors.As(err, &re) && re.Err != nil {
		return SafeText(re.Err.Error(), 300)
	}
	return ""
}

// ---- texts for the terminal / app ----

func fmtBytes(n uint64) string {
	const u = 1000.0
	f := float64(n)
	for _, s := range []string{"B", "KB", "MB", "GB", "TB"} {
		if f < u || s == "TB" {
			if s == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, s)
		}
		f /= u
	}
	return ""
}

func startedText(req ConfirmRequest) string {
	if req.Mode == proto.DeleteModePermanent {
		return fmt.Sprintf("Deleting %d item(s) permanently…", req.Count)
	}
	return fmt.Sprintf("Moving %d item(s) to the %s…", req.Count, req.Trash)
}

func auditText(j *delJob, r *proto.DeleteResult) string {
	trash := TrashName(goos)
	switch {
	case r.OK && j.mode == scan.ModeTrash:
		return fmt.Sprintf("moved to the %s (%s)", trash, fmtBytes(r.FreedAlloc))
	case r.OK:
		return fmt.Sprintf("deleted permanently (%s)", fmtBytes(r.FreedAlloc))
	case r.Code == "partial":
		return fmt.Sprintf("partly deleted (%s freed; some contents were left)", fmtBytes(r.FreedAlloc))
	}
	s := "not deleted: " + strings.ReplaceAll(r.Code, "_", " ")
	if r.Message != "" {
		s += " (" + r.Message + ")"
	}
	return s
}

func doneText(j *delJob, ok, ran int, freed uint64) string {
	var s string
	if j.mode == scan.ModeTrash {
		trash := TrashName(goos)
		s = fmt.Sprintf("Moved %d of %d item(s) to the %s (%s). Empty the %s to free the space.", ok, ran, trash, fmtBytes(freed), trash)
	} else {
		s = fmt.Sprintf("Deleted %d of %d item(s) permanently (%s freed).", ok, ran, fmtBytes(freed))
	}
	return s
}

// waitDeletes waits (bounded) for a running deletion to stop after the agent
// was told to quit.
func (a *Agent) waitDeletes(d time.Duration) {
	a.mu.Lock()
	a.delClosed = true
	a.mu.Unlock()
	done := make(chan struct{})
	go func() { a.delWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}
