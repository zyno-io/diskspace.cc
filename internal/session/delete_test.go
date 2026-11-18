// SPDX-License-Identifier: MIT

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

var capsDel = proto.ClientCaps{ZstdStream: true, ScanState: true, Del: true}

// fakeConfirm stands in for the terminal / app prompt.
type fakeConfirm struct {
	reqs  chan ConfirmRequest
	ans   chan bool
	ended chan error // ctx.Err() of a prompt that was withdrawn
}

func newFakeConfirm() *fakeConfirm {
	return &fakeConfirm{reqs: make(chan ConfirmRequest, 4), ans: make(chan bool, 4), ended: make(chan error, 4)}
}

func (f *fakeConfirm) Confirm(ctx context.Context, req ConfirmRequest) (bool, error) {
	f.reqs <- req
	select {
	case ok := <-f.ans:
		return ok, nil
	case <-ctx.Done():
		f.ended <- ctx.Err()
		return false, ctx.Err()
	}
}

func (f *fakeConfirm) next(t *testing.T) ConfirmRequest {
	t.Helper()
	select {
	case r := <-f.reqs:
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("no confirmation request")
	}
	return ConfirmRequest{}
}

// delTree: tree/{keep.txt, old.log, junk/{a.bin, sub/b.bin}, other/x.txt}
func delTree(t *testing.T) string {
	t.Helper()
	tree := t.TempDir()
	for _, d := range []string{"junk/sub", "other"} {
		if err := os.MkdirAll(filepath.Join(tree, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, size := range map[string]int{"keep.txt": 10, "old.log": 5000, "junk/a.bin": 70000, "junk/sub/b.bin": 9000, "other/x.txt": 3} {
		if err := os.WriteFile(filepath.Join(tree, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return tree
}

// dirIDs maps slash-separated paths relative to the scan root to dir ids.
func dirIDs(r *scanResult) map[string]uint64 {
	name := map[uint64]string{}
	parent := map[uint64]uint64{}
	add := func(p, first uint64, ds []proto.ChildDir) {
		for i, d := range ds {
			id := first + uint64(i)
			name[id], parent[id] = d.Name, p
		}
	}
	for _, rec := range r.Recs {
		switch x := rec.(type) {
		case *proto.DirRecord:
			add(x.ID, x.FirstChildID, x.Dirs)
		case *proto.DirMoreRecord:
			add(x.ID, x.FirstChildID, x.Dirs)
		}
	}
	out := map[string]uint64{"": 0}
	for id := range name {
		var parts []string
		for k := id; k != 0; k = parent[k] {
			parts = append([]string{name[k]}, parts...)
		}
		out[strings.Join(parts, "/")] = id
	}
	return out
}

func dirItem(id uint64) proto.DeleteItem { return proto.DeleteItem{Dir: &id} }

func fileItem(parent uint64, name string) proto.DeleteItem {
	return proto.DeleteItem{Parent: &parent, Name: &name}
}

// expectStatus reads until a DELETE_STATUS in state want arrives; a terminal
// status in another state fails the test. A DELETED seen on the way is kept
// in v.deleted.
func (v *testViewer) expectStatus(want string) proto.DeleteStatus {
	v.t.Helper()
	for {
		typ, body := v.must(15 * time.Second)
		if typ == proto.MsgDeleted {
			d := new(proto.Deleted)
			if err := json.Unmarshal(body[1:], d); err != nil {
				v.t.Fatal(err)
			}
			v.deleted = append(v.deleted, d)
			continue
		}
		if typ != proto.MsgDeleteStatus {
			continue
		}
		var st proto.DeleteStatus
		if err := json.Unmarshal(body[1:], &st); err != nil {
			v.t.Fatal(err)
		}
		if st.State == want {
			return st
		}
		if terminal(st.State) {
			v.t.Fatalf("DELETE_STATUS %+v, want state %s", st, want)
		}
	}
}

// expectDeleted returns the next DELETED (one already seen first).
func (v *testViewer) expectDeleted() *proto.Deleted {
	v.t.Helper()
	if len(v.deleted) > 0 {
		d := v.deleted[0]
		v.deleted = v.deleted[1:]
		return d
	}
	d := new(proto.Deleted)
	v.expect(proto.MsgDeleted, d)
	return d
}

// delSetup starts an agent with cfg, connects a del-capable viewer and scans tree.
func delSetup(t *testing.T, cfg Config, tree string) (*testEnv, *testViewer, *scanResult, map[string]uint64) {
	t.Helper()
	e := newEnv(t, cfg)
	v := connectViewerCaps(t, e.wsURL, e.sec, capsDel)
	v.handshakeMsgs()
	v.expect(proto.MsgReplayDone, nil)
	v.send(proto.MsgScanStart, proto.ScanStart{Path: tree})
	r := v.collect()
	if r.Done == nil {
		t.Fatalf("scan did not finish: %+v", r.Ended)
	}
	return e, v, r, dirIDs(r)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestDeleteApproved(t *testing.T) {
	tree := delTree(t)
	fc := newFakeConfirm()
	e, v, r, ids := delSetup(t, Config{Confirm: fc.Confirm, ConfirmKind: "terminal"}, tree)

	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 41, ScanID: r.Started.ScanID, Mode: "permanent", Items: []proto.DeleteItem{
		dirItem(ids["junk"]), fileItem(0, "old.log"), dirItem(ids["junk/sub"]), // the last is inside the first
	}})
	st := v.expectStatus(proto.DelPreparing)
	if st.ReqID != 41 || st.Job == 0 || st.Total != 3 {
		t.Fatalf("preparing: %+v", st)
	}
	v.expectStatus(proto.DelAwaiting)
	req := fc.next(t)
	if req.Mode != "permanent" || req.Count != 2 || req.Code != v.code || !req.ViaRelay || req.Skipped != 0 {
		t.Fatalf("confirm request %+v", req)
	}
	if len(req.Items) != 2 || req.Items[0].Path != filepath.Join(tree, "junk") || !req.Items[0].Dir || req.Items[0].AtLeast {
		t.Fatalf("items %+v", req.Items)
	}
	if req.Items[0].Files != 2 || req.Items[0].Dirs != 1 || req.Alloc == 0 || req.Files != 3 {
		t.Fatalf("measured %+v", req)
	}
	if len(req.Folders) != 1 || req.Folders[0].Count != 2 || req.Folders[0].Path != tree {
		t.Fatalf("folders %+v", req.Folders)
	}
	fc.ans <- true
	done := v.expectStatus(proto.DelDone)
	if done.Job != st.Job || len(done.Results) != 3 {
		t.Fatalf("done: %+v", done)
	}
	for _, res := range done.Results {
		if !res.OK {
			t.Fatalf("result %+v", res)
		}
	}
	if done.Results[0].Files != 2 || done.Results[1].FreedAlloc == 0 && done.Results[0].FreedAlloc == 0 {
		t.Fatalf("results %+v", done.Results)
	}
	del := v.expectDeleted()
	if del.ScanID != r.Started.ScanID || del.Job != st.Job || len(del.Items) != 2 ||
		del.Items[0].Dir == nil || *del.Items[0].Dir != ids["junk"] ||
		del.Items[1].Parent == nil || *del.Items[1].Parent != 0 || *del.Items[1].Name != "old.log" || del.Items[1].Dir != nil {
		b, _ := json.Marshal(del)
		t.Fatalf("DELETED %s", b)
	}
	if exists(filepath.Join(tree, "junk")) || exists(filepath.Join(tree, "old.log")) {
		t.Fatal("not deleted")
	}
	if !exists(filepath.Join(tree, "keep.txt")) || !exists(filepath.Join(tree, "other", "x.txt")) {
		t.Fatal("deleted too much")
	}
	e.rec.wait(t, EvDeleteDone, 5*time.Second)
	if n := e.rec.count(EvDeleteItem); n != 3 { // every requested item, the nested one too
		t.Fatalf("%d audit events, want 3", n)
	}
}

func TestDeleteRefusedTouchesNothing(t *testing.T) {
	tree := delTree(t)
	fc := newFakeConfirm()
	_, v, r, ids := delSetup(t, Config{Confirm: fc.Confirm}, tree)
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 1, ScanID: r.Started.ScanID, Mode: "permanent", Items: []proto.DeleteItem{dirItem(ids["junk"]), fileItem(0, "old.log")}})
	v.expectStatus(proto.DelAwaiting)
	fc.next(t)
	fc.ans <- false
	st := v.expectStatus(proto.DelRefused)
	if st.Code != "declined" {
		t.Fatalf("refused %+v", st)
	}
	for _, p := range []string{"junk/a.bin", "junk/sub/b.bin", "old.log"} {
		if !exists(filepath.Join(tree, p)) {
			t.Fatalf("%s was touched", p)
		}
	}
	// the cooldown follows a refusal
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 2, ScanID: r.Started.ScanID, Mode: "trash", Items: []proto.DeleteItem{dirItem(ids["junk"])}})
	if st := v.expectStatus(proto.DelRefused); st.Code != "cooldown" || st.ReqID != 2 {
		t.Fatalf("second request %+v", st)
	}
}

func TestDeleteCooldownAndDisable(t *testing.T) {
	old := refuseCooldown
	refuseCooldown = 100 * time.Millisecond
	t.Cleanup(func() { refuseCooldown = old })
	tree := delTree(t)
	fc := newFakeConfirm()
	_, v, r, ids := delSetup(t, Config{Confirm: fc.Confirm}, tree)
	req := func(id uint64) {
		v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: id, ScanID: r.Started.ScanID, Mode: "permanent", Items: []proto.DeleteItem{dirItem(ids["junk"])}})
	}
	for i := uint64(1); i <= 3; i++ {
		req(i)
		v.expectStatus(proto.DelAwaiting)
		fc.next(t)
		fc.ans <- false
		v.expectStatus(proto.DelRefused)
		req(10 + i) // within the cooldown; after the third refusal, off
		want := "cooldown"
		if i == 3 {
			want = "disabled"
		}
		if st := v.expectStatus(proto.DelRefused); st.Code != want {
			t.Fatalf("after refusal %d: %+v, want %s", i, st, want)
		}
		time.Sleep(150 * time.Millisecond)
	}
	req(99)
	if st := v.expectStatus(proto.DelRefused); st.Code != "disabled" {
		t.Fatalf("after 3 refusals: %+v", st)
	}
	if !exists(filepath.Join(tree, "junk", "a.bin")) {
		t.Fatal("touched")
	}
}

func TestDeleteNoConfirmer(t *testing.T) {
	tree := delTree(t)
	e := newEnv(t, Config{})
	v := connectViewerCaps(t, e.wsURL, e.sec, capsDel)
	info := v.handshakeMsgs()
	f := info.Features.Delete
	if f == nil || f.Available || f.Reason != "no_confirm" || info.Deleting != nil {
		t.Fatalf("features.delete %+v deleting %+v", f, info.Deleting)
	}
	v.expect(proto.MsgReplayDone, nil)
	v.send(proto.MsgScanStart, proto.ScanStart{Path: tree})
	r := v.collect()
	ids := dirIDs(r)
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 1, ScanID: r.Started.ScanID, Mode: "trash", Items: []proto.DeleteItem{dirItem(ids["junk"])}})
	if st := v.expectStatus(proto.DelRefused); st.Code != "no_confirm" {
		t.Fatalf("%+v", st)
	}
	if !exists(filepath.Join(tree, "junk")) {
		t.Fatal("touched")
	}
}

func TestDeleteFeatureAdvertised(t *testing.T) {
	fc := newFakeConfirm()
	e := newEnv(t, Config{Confirm: fc.Confirm, ConfirmKind: "app"})
	v := connectViewerCaps(t, e.wsURL, e.sec, capsDel)
	var raw map[string]json.RawMessage
	v.must(5 * time.Second) // STREAM_INIT
	_, body := v.must(5 * time.Second)
	if err := json.Unmarshal(body[1:], &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["deleting"]) != "null" {
		t.Fatalf("deleting = %s, want null", raw["deleting"])
	}
	var info proto.AgentInfo
	_ = json.Unmarshal(body[1:], &info)
	f := info.Features.Delete
	if f == nil || !f.Available || f.Confirm != "app" || !f.Trash || f.MaxItems != 500 || f.Reason != "" {
		t.Fatalf("features.delete %+v", f)
	}
}

func TestDeleteRefusedWhileScanning(t *testing.T) {
	tree := delTree(t)
	startPaused(t)
	fc := newFakeConfirm()
	e := newEnv(t, Config{Confirm: fc.Confirm})
	v := connectViewerCaps(t, e.wsURL, e.sec, capsDel)
	v.handshakeMsgs()
	v.send(proto.MsgScanStart, proto.ScanStart{Path: tree})
	v.expect(proto.MsgScanStarted, nil)
	v.expectState(true)
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 1, ScanID: 1, Mode: "trash", Items: []proto.DeleteItem{dirItem(1)}})
	if st := v.expectStatus(proto.DelRefused); st.Code != "scanning" {
		t.Fatalf("%+v", st)
	}
	select {
	case <-fc.reqs:
		t.Fatal("prompted while scanning")
	default:
	}
}

func TestDeleteBlocksScanStart(t *testing.T) {
	tree := delTree(t)
	fc := newFakeConfirm()
	_, v, r, ids := delSetup(t, Config{Confirm: fc.Confirm}, tree)
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 1, ScanID: r.Started.ScanID, Mode: "permanent", Items: []proto.DeleteItem{dirItem(ids["junk"])}})
	v.expectStatus(proto.DelAwaiting)
	fc.next(t)
	v.send(proto.MsgScanStart, proto.ScanStart{Path: tree})
	var em proto.Error
	v.expect(proto.MsgError, &em)
	if em.Code != "busy" {
		t.Fatalf("SCAN_START while deleting: %+v", em)
	}
	fc.ans <- false
	v.expectStatus(proto.DelRefused)
}

func TestDeleteValidation(t *testing.T) {
	tree := delTree(t)
	fc := newFakeConfirm()
	_, v, r, ids := delSetup(t, Config{Confirm: fc.Confirm}, tree)
	sid := r.Started.ScanID
	bad := map[string][]proto.DeleteItem{
		"root id":    {dirItem(0)},
		"dot-dot":    {fileItem(0, "../x")},
		"separator":  {fileItem(ids["junk"], "a/b")},
		"empty name": {fileItem(0, "")},
		"both forms": {{Dir: dirItem(ids["junk"]).Dir, Parent: fileItem(0, "x").Parent, Name: fileItem(0, "x").Name}},
		"neither":    {{}},
	}
	many := make([]proto.DeleteItem, proto.MaxDeleteItems+1)
	for i := range many {
		many[i] = dirItem(ids["junk"])
	}
	bad["too many"] = many
	n := uint64(0)
	for name, items := range bad {
		n++
		v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: n, ScanID: sid, Mode: "permanent", Items: items})
		st := v.expectStatus(proto.DelFailed)
		if st.Code != "invalid" || st.ReqID != n {
			t.Fatalf("%s: %+v", name, st)
		}
	}
	n++
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: n, ScanID: sid, Mode: "shred", Items: []proto.DeleteItem{dirItem(ids["junk"])}})
	if st := v.expectStatus(proto.DelFailed); st.Code != "invalid" {
		t.Fatalf("mode: %+v", st)
	}
	n++
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: n, ScanID: sid + 7, Mode: "trash", Items: []proto.DeleteItem{dirItem(ids["junk"])}})
	if st := v.expectStatus(proto.DelFailed); st.Code != "invalid" {
		t.Fatalf("stale scan: %+v", st)
	}
	// unknown id, missing file: nothing can be deleted, nobody is asked
	n++
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: n, ScanID: sid, Mode: "permanent", Items: []proto.DeleteItem{dirItem(987654), fileItem(0, "nope.txt")}})
	st := v.expectStatus(proto.DelFailed)
	if st.Code != "nothing_deletable" || len(st.Results) != 2 || st.Results[0].Code != "not_scanned" || st.Results[1].Code != "not_found" || st.Results[0].OK {
		t.Fatalf("unknown: %+v", st)
	}
	select {
	case <-fc.reqs:
		t.Fatal("prompted for an invalid request")
	default:
	}
	if !exists(filepath.Join(tree, "junk")) {
		t.Fatal("touched")
	}
	// a body over the v2a limit ends the connection
	long := strings.Repeat("n", 3000)
	var huge []proto.DeleteItem
	for i := 0; i < 30; i++ {
		huge = append(huge, fileItem(0, long))
	}
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 99, ScanID: sid, Mode: "trash", Items: huge})
	for {
		if _, _, err := v.recv(10 * time.Second); err != nil {
			if errors.Is(err, errRecvTimeout) {
				t.Fatal("connection stayed open after an oversize request")
			}
			break
		}
	}
}

// Items spread over more folders than a prompt can name are refused.
func TestDeleteTooSpread(t *testing.T) {
	tree := t.TempDir()
	for i := 0; i <= MaxConfirmFolders; i++ {
		d := filepath.Join(tree, fmt.Sprintf("p%02d", i))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fc := newFakeConfirm()
	_, v, r, ids := delSetup(t, Config{Confirm: fc.Confirm}, tree)
	var items []proto.DeleteItem
	for i := 0; i <= MaxConfirmFolders; i++ {
		items = append(items, fileItem(ids[fmt.Sprintf("p%02d", i)], "f"))
	}
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 1, ScanID: r.Started.ScanID, Mode: "trash", Items: items})
	if st := v.expectStatus(proto.DelFailed); st.Code != "too_spread" {
		t.Fatalf("%+v", st)
	}
	select {
	case <-fc.reqs:
		t.Fatal("prompted")
	default:
	}
	// one fewer folder is fine
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 2, ScanID: r.Started.ScanID, Mode: "permanent", Items: items[:MaxConfirmFolders]})
	v.expectStatus(proto.DelAwaiting)
	if req := fc.next(t); len(req.Folders) != MaxConfirmFolders || len(req.Items) != MaxConfirmFolders {
		t.Fatalf("folders %d items %d", len(req.Folders), len(req.Items))
	}
	fc.ans <- false
	v.expectStatus(proto.DelRefused)
	if !exists(filepath.Join(tree, "p00", "f")) {
		t.Fatal("touched")
	}
}

func TestDeleteWithdrawn(t *testing.T) {
	tree := delTree(t)
	fc := newFakeConfirm()
	_, v, r, ids := delSetup(t, Config{Confirm: fc.Confirm}, tree)
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 1, ScanID: r.Started.ScanID, Mode: "permanent", Items: []proto.DeleteItem{dirItem(ids["junk"])}})
	st := v.expectStatus(proto.DelAwaiting)
	fc.next(t)
	v.send(proto.MsgDeleteCancel, proto.DeleteCancel{Job: st.Job})
	select {
	case err := <-fc.ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("prompt ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("prompt not withdrawn")
	}
	if st := v.expectStatus(proto.DelRefused); st.Code != "canceled" {
		t.Fatalf("%+v", st)
	}
	if !exists(filepath.Join(tree, "junk")) {
		t.Fatal("touched")
	}
}

// Approved jobs outlive the requesting connection; DELETED is replayed after
// the end message, before REPLAY_DONE, and only to viewers with caps.del.
func TestDeleteSurvivesDisconnectAndReplays(t *testing.T) {
	tree := delTree(t)
	fc := newFakeConfirm()
	entered, release := make(chan string, 4), make(chan struct{})
	hookBeforeRemove = func(p string) {
		entered <- p
		<-release
	}
	t.Cleanup(func() { hookBeforeRemove = nil })
	e, v, r, ids := delSetup(t, Config{Confirm: fc.Confirm}, tree)
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 5, ScanID: r.Started.ScanID, Mode: "permanent", Items: []proto.DeleteItem{dirItem(ids["junk"]), fileItem(ids["other"], "x.txt")}})
	st := v.expectStatus(proto.DelAwaiting)
	fc.next(t)
	fc.ans <- true
	v.expectStatus(proto.DelRunning)
	<-entered
	v.ws.CloseNow()
	e.rec.wait(t, EvViewerDisconnected, 10*time.Second)

	// a new viewer (no del caps) sees the job in AGENT_INFO but no 0x18/0x19
	old := connectViewerCaps(t, e.wsURL, e.sec, capsNew)
	info := old.handshakeMsgs()
	if info.Deleting == nil || info.Deleting.Job != st.Job || info.Deleting.State != proto.DelRunning || info.Deleting.Total != 2 {
		t.Fatalf("AGENT_INFO.deleting %+v", info.Deleting)
	}
	release <- struct{}{} // first item
	<-entered
	close(release)
	e.rec.wait(t, EvDeleteDone, 10*time.Second)
	if exists(filepath.Join(tree, "junk")) || exists(filepath.Join(tree, "other", "x.txt")) || !exists(filepath.Join(tree, "other")) {
		t.Fatal("job did not finish after the disconnect")
	}
	var seenEnd bool
	for {
		typ, _, err := old.recv(time.Second)
		if errors.Is(err, errRecvTimeout) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch typ {
		case proto.MsgDeleteStatus, proto.MsgDeleted:
			t.Fatalf("message %#x sent to a viewer without caps.del", typ)
		case proto.MsgScanDone:
			seenEnd = true
		case proto.MsgReplayDone:
			if !seenEnd {
				t.Fatal("REPLAY_DONE before SCAN_DONE")
			}
		}
	}

	// replay to a del viewer: NODES…, SCAN_DONE, DELETED, REPLAY_DONE
	nv := connectViewerCaps(t, e.wsURL, e.sec, capsDel)
	info = nv.handshakeMsgs()
	if info.Deleting != nil {
		t.Fatalf("deleting %+v after the job", info.Deleting)
	}
	var order []byte
	for {
		typ, body := nv.must(15 * time.Second)
		if typ == proto.MsgNodes {
			if len(order) == 0 || order[len(order)-1] != proto.MsgNodes {
				order = append(order, typ)
			}
			continue
		}
		order = append(order, typ)
		if typ == proto.MsgDeleted {
			var d proto.Deleted
			_ = json.Unmarshal(body[1:], &d)
			if d.Job != st.Job || len(d.Items) != 2 {
				t.Fatalf("replayed DELETED %+v", d)
			}
		}
		if typ == proto.MsgReplayDone {
			break
		}
	}
	want := []byte{proto.MsgScanStarted, proto.MsgNodes, proto.MsgScanDone, proto.MsgDeleted, proto.MsgReplayDone}
	if string(order) != string(want) {
		t.Fatalf("replay order % x, want % x", order, want)
	}
}

// A viewer that takes over while a job runs follows it to the end.
func TestDeleteSupersedeFollowsJob(t *testing.T) {
	tree := delTree(t)
	fc := newFakeConfirm()
	entered, release := make(chan string, 4), make(chan struct{})
	hookBeforeRemove = func(p string) {
		entered <- p
		<-release
	}
	t.Cleanup(func() { hookBeforeRemove = nil })
	e, v, r, ids := delSetup(t, Config{Confirm: fc.Confirm}, tree)
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 5, ScanID: r.Started.ScanID, Mode: "permanent", Items: []proto.DeleteItem{dirItem(ids["junk"])}})
	st := v.expectStatus(proto.DelAwaiting)
	fc.next(t)
	fc.ans <- true
	<-entered

	v2 := connectViewerCaps(t, e.wsURL, e.sec, capsDel)
	info := v2.handshakeMsgs()
	if info.Deleting == nil || info.Deleting.Job != st.Job {
		t.Fatalf("deleting %+v", info.Deleting)
	}
	close(release)
	done := v2.expectStatus(proto.DelDone)
	if done.Job != st.Job || len(done.Results) != 1 || !done.Results[0].OK {
		t.Fatalf("done %+v", done)
	}
	v2.expectDeleted()
	if exists(filepath.Join(tree, "junk")) {
		t.Fatal("not deleted")
	}
}

// Before approval, a dropped requester withdraws the request.
func TestDeleteRequesterGoneBeforeApproval(t *testing.T) {
	tree := delTree(t)
	fc := newFakeConfirm()
	_, v, r, ids := delSetup(t, Config{Confirm: fc.Confirm}, tree)
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 1, ScanID: r.Started.ScanID, Mode: "permanent", Items: []proto.DeleteItem{dirItem(ids["junk"])}})
	v.expectStatus(proto.DelAwaiting)
	fc.next(t)
	v.ws.CloseNow()
	select {
	case <-fc.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("prompt not withdrawn after the requester left")
	}
	time.Sleep(100 * time.Millisecond)
	if !exists(filepath.Join(tree, "junk", "a.bin")) {
		t.Fatal("touched")
	}
}

// A viewer that did not advertise caps.del can't send DELETE_REQ.
func TestDeleteReqWithoutCaps(t *testing.T) {
	fc := newFakeConfirm()
	e := newEnv(t, Config{Confirm: fc.Confirm})
	v := connectViewerCaps(t, e.wsURL, e.sec, capsNew)
	v.handshakeMsgs()
	v.send(proto.MsgDeleteReq, proto.DeleteReq{ReqID: 1, ScanID: 1, Mode: "trash", Items: []proto.DeleteItem{dirItem(1)}})
	for {
		typ, _, err := v.recv(10 * time.Second)
		if err != nil {
			if errors.Is(err, errRecvTimeout) {
				t.Fatal("connection stayed open")
			}
			return
		}
		if typ == proto.MsgDeleteStatus {
			t.Fatal("DELETE_STATUS sent to a viewer without caps.del")
		}
	}
}

// A terminal status for one job doesn't drop another job's pending status.
func TestDeleteStatusSlotPerJob(t *testing.T) {
	s := newSender(nil, nil, nil)
	s.del = true
	s.setDeleteStatus(1, []byte("a"))
	s.pushDeleteFinal(2, []byte("b"))
	if string(s.delStatus) != "a" {
		t.Fatal("job 1's status was dropped by job 2's refusal")
	}
	s.pushDeleteFinal(1, []byte("c"))
	if s.delStatus != nil {
		t.Fatal("job 1's stale status survived its final one")
	}
	if len(s.ctrl) != 2 || string(s.ctrl[1]) != "c" {
		t.Fatalf("ctrl %q", s.ctrl)
	}
}

func TestSafeText(t *testing.T) {
	cases := map[string]string{
		"plain/path ü":              "plain/path ü",
		"a\nb":                      `a\x0Ab`,
		"x\r\x1b[2Ky":               `x\x0D\x1B[2Ky`,
		"evil\u202Etxt.exe":         `evil\u{202E}txt.exe`,
		"iso\u2066late\u2069":       `iso\u{2066}late\u{2069}`,
		"zero\u200bwidth":           `zero\u{200B}width`,
		"line\u2028sep":             `line\u{2028}sep`,
		"bad\xffutf8":               `bad\xFFutf8`,
		"del\x7f":                   `del\x7F`,
		"c1\u0085":                  `c1\x85`,
		`C:\Users\me\Downloads\a b`: `C:\Users\me\Downloads\a b`,
	}
	for in, want := range cases {
		if got := SafeText(in, 0); got != want {
			t.Errorf("SafeText(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("é", 300) + "/end.txt"
	got := SafeText(long, 100)
	if len(got) > 100 || !strings.HasSuffix(got, "/end.txt") || !strings.Contains(got, "…") || !strings.HasPrefix(got, "éé") {
		t.Fatalf("truncated %q (%d bytes)", got, len(got))
	}
}
