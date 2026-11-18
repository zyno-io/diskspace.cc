// SPDX-License-Identifier: MIT

package session

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zyno-io/diskspace.cc/internal/proto"
	"github.com/zyno-io/diskspace.cc/internal/session/crypto"
)

func closeStatus(err error) int { return int(websocket.CloseStatus(err)) }

func checkTotals(t *testing.T, r *scanResult, tree string) {
	t.Helper()
	if r.Done == nil {
		t.Fatalf("no SCAN_DONE (ended=%+v)", r.Ended)
	}
	got := r.sums()
	if got != r.Done.Totals {
		t.Fatalf("record sums %+v != SCAN_DONE totals %+v", got, r.Done.Totals)
	}
	if ref := refAlloc(t, tree); ref >= 0 && uint64(ref) != r.Done.Totals.Alloc {
		t.Fatalf("SCAN_DONE alloc %d != WalkDir alloc %d", r.Done.Totals.Alloc, ref)
	}
}

func TestE2EScanAndQueries(t *testing.T) {
	tree := t.TempDir()
	mkTree(t, tree, 40, 6)
	cwd := tree
	e := newEnv(t, Config{Cwd: &cwd, Home: "/home/tester", FDA: "denied", Elevation: "none"})

	v := e.viewer()
	info := v.handshakeMsgs()
	if info.Proto != 1 || info.Cwd == nil || info.Cwd.Path != tree || info.Home != "/home/tester" ||
		info.Volumes == nil || info.Elevation != "none" || info.FDA == nil || *info.FDA != "denied" ||
		info.Root != nil || info.Scan != nil || info.Hostname == "" {
		t.Fatalf("unexpected AGENT_INFO %+v", info)
	}
	if len(info.Volumes) == 0 {
		t.Log("warning: no volumes reported")
	}
	if typ, _ := v.must(5 * time.Second); typ != proto.MsgReplayDone {
		t.Fatalf("want REPLAY_DONE, got %#x", typ)
	}

	v.send(proto.MsgPing, nil)
	v.expect(proto.MsgPong, nil)

	v.send(proto.MsgScanStart, proto.ScanStart{Path: tree})
	r := v.collect()
	checkTotals(t, r, tree)
	if r.Started.ScanID != 1 || r.Started.Path != tree {
		t.Fatalf("SCAN_STARTED %+v", r.Started)
	}

	// DIR_FILES for the first big-file directory.
	var target uint64
	var dirName string
	for _, rec := range r.Recs {
		if d, ok := rec.(*proto.DirRecord); ok && len(d.Files) > 0 {
			target = d.ID
			dirName = d.Files[0].Name
			break
		}
	}
	if dirName == "" {
		t.Fatal("no directory with listed files")
	}
	v.send(proto.MsgDirFilesReq, proto.DirFilesReq{ReqID: 7, ScanID: 1, DirID: target, Limit: 100})
	body := v.expect(proto.MsgDirFiles, nil)
	req, sid, did, res, err := proto.DecodeDirFiles(body)
	if err != nil || req != 7 || sid != 1 || did != target || len(res.Files) == 0 {
		t.Fatalf("DIR_FILES: %v %d %d %d %+v", err, req, sid, did, res)
	}
	v.send(proto.MsgDirFilesReq, proto.DirFilesReq{ReqID: 8, ScanID: 99, DirID: 0, Limit: 10})
	var em proto.Error
	v.expect(proto.MsgError, &em)
	if em.Code != "unknown_scan" {
		t.Fatalf("got error %+v", em)
	}

	// BROWSE + GET_ROOTS
	v.send(proto.MsgBrowse, proto.Browse{ReqID: 3, Path: tree})
	var br proto.BrowseResult
	v.expect(proto.MsgBrowseResult, &br)
	if br.ReqID != 3 || len(br.Dirs) != 40 || br.Parent == nil || br.Error != "" {
		t.Fatalf("BROWSE %+v", br)
	}
	v.send(proto.MsgGetRoots, nil)
	v.expect(proto.MsgRoots, nil)

	// Reconnect: replay of the finished scan gives identical totals.
	v2 := e.viewer()
	v2.handshakeMsgs()
	r2 := v2.collect()
	checkTotals(t, r2, tree)
	v2.finishReplay(r2) // the scan had ended at attach: REPLAY_DONE follows SCAN_DONE
	if r2.Done.Totals != r.Done.Totals {
		t.Fatalf("replay totals differ: %+v vs %+v", r2.Done.Totals, r.Done.Totals)
	}
	if !r2.ReplayDone {
		t.Fatal("no REPLAY_DONE after the replayed SCAN_DONE")
	}
	// v was superseded.
	var sawBye bool
	for i := 0; i < 20 && !sawBye; i++ {
		typ, body, err := v.recv(5 * time.Second)
		if err != nil {
			break
		}
		if typ == proto.MsgBye && strings.Contains(string(body), "superseded") {
			sawBye = true
		}
	}
	if !sawBye {
		t.Fatal("first viewer did not get BYE superseded")
	}
	e.rec.wait(t, EvViewerSuperseded, 5*time.Second)
}

func TestE2EReconnectMidScan(t *testing.T) {
	tree := t.TempDir()
	mkTree(t, tree, 60, 6)
	e := newEnv(t, Config{})
	v := e.viewer()
	v.handshakeMsgs()
	v.send(proto.MsgScanStart, proto.ScanStart{Path: tree})
	// read until the first NODES, then drop the connection
	v.expect(proto.MsgNodes, nil)
	v.ws.CloseNow()

	v2 := e.viewer()
	v2.handshakeMsgs()
	r := v2.collect()
	checkTotals(t, r, tree)
	v2.finishReplay(r) // before SCAN_DONE if the scan was still running at attach, else right after it
	// A second scan (cancel + restart) gets a new monotonic id.
	v2.send(proto.MsgScanStart, proto.ScanStart{Path: tree})
	r2 := v2.collect()
	if r2.Started.ScanID != 2 {
		t.Fatalf("scanId %d", r2.Started.ScanID)
	}
	checkTotals(t, r2, tree)
}

func TestE2ECancel(t *testing.T) {
	tree := t.TempDir()
	mkTree(t, tree, 80, 5)
	e := newEnv(t, Config{})
	v := e.viewer()
	v.handshakeMsgs()
	v.send(proto.MsgScanStart, proto.ScanStart{Path: tree})
	v.send(proto.MsgScanCancel, proto.ScanCancel{ScanID: 1})
	r := v.collect()
	if r.Done == nil && (r.Ended == nil || r.Ended.Reason != "canceled") {
		t.Fatalf("unexpected end %+v %+v", r.Done, r.Ended)
	}
}

func TestE2EWrongKey(t *testing.T) {
	e := newEnv(t, Config{})
	bad := &crypto.Secret{SID: e.sec.SID}
	if _, err := rand.Read(bad.K[:]); err != nil {
		t.Fatal(err)
	}
	ctx := ctx10(t)
	ws, err := dialRaw(ctx, e.wsURL+"/v1/view?sid="+e.sec.SIDString())
	if err != nil {
		t.Fatal(err)
	}
	m1, _, _ := crypto.ViewerHello(bad, rand.Reader)
	if err := ws.Write(ctx, websocket.MessageBinary, m1); err != nil {
		t.Fatal(err)
	}
	_, _, err = ws.Read(ctx)
	if closeStatus(err) != 4401 {
		t.Fatalf("want close 4401, got %v", err)
	}
}

func TestE2EBadVersion(t *testing.T) {
	e := newEnv(t, Config{})
	ctx := ctx10(t)
	ws, err := dialRaw(ctx, e.wsURL+"/v1/view?sid="+e.sec.SIDString())
	if err != nil {
		t.Fatal(err)
	}
	m1, _, _ := crypto.ViewerHello(e.sec, rand.Reader)
	m1[1] = 2
	_ = ws.Write(ctx, websocket.MessageBinary, m1)
	_, _, err = ws.Read(ctx)
	if closeStatus(err) != 4400 {
		t.Fatalf("want close 4400, got %v", err)
	}
}

func TestE2ETamperedFrame(t *testing.T) {
	e := newEnv(t, Config{})
	v := e.viewer()
	v.handshakeMsgs()
	// A frame sealed correctly but then bit-flipped.
	pt := proto.FrameV2A([]byte{proto.MsgPing})
	ct, _ := v.ciph.Seal(pt)
	ct[len(ct)-1] ^= 1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := v.ws.Write(ctx, websocket.MessageBinary, ct); err != nil {
		t.Fatal(err)
	}
	for {
		_, _, err := v.recv(5 * time.Second)
		if err == nil {
			continue
		}
		if closeStatus(err) != 4402 {
			t.Fatalf("want close 4402, got %v", err)
		}
		return
	}
}

func TestE2EBadHelloFrame(t *testing.T) {
	e := newEnv(t, Config{})
	ctx := ctx10(t)
	ws, err := dialRaw(ctx, e.wsURL+"/v1/view?sid="+e.sec.SIDString())
	if err != nil {
		t.Fatal(err)
	}
	ciph, _, err := handshake(ctx, ws, e.sec)
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := ciph.Seal(proto.FrameV2A([]byte{proto.MsgClientHello, '{'}))
	_ = ws.Write(ctx, websocket.MessageBinary, ct)
	_, _, err = ws.Read(ctx)
	if closeStatus(err) != 4402 {
		t.Fatalf("want close 4402, got %v", err)
	}
}

func TestE2EClientHelloProtoMismatch(t *testing.T) {
	e := newEnv(t, Config{})
	ctx := ctx10(t)
	ws, err := dialRaw(ctx, e.wsURL+"/v1/view?sid="+e.sec.SIDString())
	if err != nil {
		t.Fatal(err)
	}
	ciph, _, err := handshake(ctx, ws, e.sec)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(proto.ClientHello{Proto: proto.ProtoVersion + 1, Caps: proto.ClientCaps{ZstdStream: true}})
	ct, _ := ciph.Seal(proto.FrameV2A(append([]byte{proto.MsgClientHello}, body...)))
	_ = ws.Write(ctx, websocket.MessageBinary, ct)
	_, _, err = ws.Read(ctx)
	if closeStatus(err) != 4400 {
		t.Fatalf("want close 4400, got %v", err)
	}
}

func TestE2EOversizeV2A(t *testing.T) {
	e := newEnv(t, Config{})
	v := e.viewer()
	v.handshakeMsgs()
	big := append([]byte{proto.MsgBrowse}, make([]byte, 70<<10)...)
	ct, _ := v.ciph.Seal(proto.FrameV2A(big))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = v.ws.Write(ctx, websocket.MessageBinary, ct)
	for {
		_, _, err := v.recv(5 * time.Second)
		if err != nil {
			if c := closeStatus(err); c == -1 {
				t.Fatalf("expected a close code, got %v", err)
			}
			return
		}
	}
}

func TestE2ECreditStallDoesNotBlockScanner(t *testing.T) {
	old := initialCredit
	initialCredit = 64 << 10
	t.Cleanup(func() { initialCredit = old })

	tree := t.TempDir()
	mkWideTree(t, tree, 60, 70)
	e := newEnv(t, Config{})
	v := e.viewer()
	v.handshakeMsgs()
	v.send(proto.MsgScanStart, proto.ScanStart{Path: tree})

	// The agent-side scan must finish although the viewer never sends CREDIT.
	e.rec.wait(t, EvScanDone, 20*time.Second)

	// Drain what the stalled agent sent: only about one window of NODES.
	var gotDone bool
	for {
		typ, _, err := v.recv(700 * time.Millisecond)
		if err != nil {
			break
		}
		if typ == proto.MsgScanDone {
			gotDone = true
		}
	}
	if gotDone {
		t.Fatal("SCAN_DONE delivered although NODES were stalled")
	}
	if v.nodesCT == 0 {
		t.Fatal("no NODES delivered before the stall")
	}
	// Returning credit lets the rest flow.
	returned := 0
	for i := 0; i < 100; i++ {
		v.send(proto.MsgCredit, proto.Credit{N: uint64(min(v.nodesCT-returned, int(initialCredit)))})
		returned = v.nodesCT
		for {
			typ, _, err := v.recv(300 * time.Millisecond)
			if err != nil {
				break
			}
			if typ == proto.MsgScanDone {
				return
			}
		}
	}
	t.Fatal("SCAN_DONE never arrived after credit was returned")
}

func TestE2ERootGuard(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	other := filepath.Join(parent, "other")
	for _, d := range []string{root, other} {
		if err := os.MkdirAll(filepath.Join(d, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// symlink escape inside root
	if err := os.Symlink(other, filepath.Join(root, "escape")); err != nil {
		t.Log("symlink unsupported:", err)
	}
	cwd := parent // outside root -> must be dropped
	e := newEnv(t, Config{Root: root, Cwd: &cwd})
	v := e.viewer()
	info := v.handshakeMsgs()
	if info.Root == nil || info.Cwd != nil {
		t.Fatalf("root/cwd: %+v %+v", info.Root, info.Cwd)
	}
	v.send(proto.MsgBrowse, proto.Browse{ReqID: 1, Path: parent})
	var br proto.BrowseResult
	v.expect(proto.MsgBrowseResult, &br)
	if br.Error == "" || len(br.Dirs) != 0 {
		t.Fatalf("browse outside root allowed: %+v", br)
	}
	v.send(proto.MsgBrowse, proto.Browse{ReqID: 2, Path: filepath.Join(root, "escape")})
	br = proto.BrowseResult{}
	v.expect(proto.MsgBrowseResult, &br)
	if _, err := os.Lstat(filepath.Join(root, "escape")); err == nil && br.Error == "" {
		t.Fatalf("symlink escape allowed: %+v", br)
	}
	v.send(proto.MsgBrowse, proto.Browse{ReqID: 3, Path: root})
	br = proto.BrowseResult{}
	v.expect(proto.MsgBrowseResult, &br)
	if br.Error != "" || br.Parent != nil {
		t.Fatalf("browse root: %+v", br)
	}
	v.send(proto.MsgScanStart, proto.ScanStart{Path: parent})
	var em proto.Error
	v.expect(proto.MsgError, &em)
	if em.Code != "denied" {
		t.Fatalf("scan outside root: %+v", em)
	}
	v.send(proto.MsgScanStart, proto.ScanStart{Path: "relative/path"})
	v.expect(proto.MsgError, &em)
	if em.Code != "bad_path" {
		t.Fatalf("relative path: %+v", em)
	}
}

func TestE2EAutoScan(t *testing.T) {
	tree := t.TempDir()
	mkTree(t, tree, 10, 3)
	e := newEnv(t, Config{AutoScan: tree})
	v := e.viewer()
	info := v.handshakeMsgs()
	if info.AutoScan == nil || *info.AutoScan != tree || info.Scan == nil {
		t.Fatalf("info %+v", info)
	}
	r := v.collect()
	checkTotals(t, r, tree)
}

func TestE2ERotate(t *testing.T) {
	e := newEnv(t, Config{})
	oldURL := e.agent.URL()
	v := e.viewer()
	v.handshakeMsgs()
	e.agent.Rotate()
	for i := 0; ; i++ {
		if e.rec.count(EvLinkReady) >= 2 {
			break
		}
		if i > 200 {
			t.Fatal("no second LinkReady")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if e.agent.URL() == oldURL {
		t.Fatal("URL unchanged")
	}
	// old viewer dropped
	for {
		if _, _, err := v.recv(5 * time.Second); err != nil {
			break
		}
	}
	// old sid is gone at the relay
	ctx := ctx10(t)
	ws, err := dialRaw(ctx, e.wsURL+"/v1/view?sid="+e.sec.SIDString())
	if err == nil {
		_, _, err = ws.Read(ctx)
	}
	if closeStatus(err) != 4404 {
		t.Fatalf("old sid: want 4404, got %v", err)
	}
	// new link works
	ev, _ := e.rec.find(EvLinkReady)
	_ = ev
	e.rec.mu.Lock()
	var newURL string
	for _, x := range e.rec.evs {
		if x.Kind == EvLinkReady {
			newURL = x.URL
		}
	}
	e.rec.mu.Unlock()
	nv := connectViewer(t, e.wsURL, parseLink(t, newURL))
	nv.handshakeMsgs()
}

func TestLocalServer(t *testing.T) {
	rec := newRecorder()
	tree := t.TempDir()
	mkTree(t, tree, 5, 2)
	a := startAgent(t, Config{Local: true, Version: "test", Events: rec.on})
	ev := rec.wait(t, EvLinkReady, 5*time.Second)
	if ev.URL != "" || !strings.HasPrefix(ev.LocalURL, "http://127.0.0.1:") {
		t.Fatalf("event %+v", ev)
	}
	sec := parseLink(t, ev.LocalURL)
	hostport := strings.TrimPrefix(ev.LocalURL[:strings.Index(ev.LocalURL, "/s/")], "http://")
	wsBase := "ws://" + hostport

	v := connectViewer(t, wsBase, sec)
	v.handshakeMsgs()
	v.send(proto.MsgScanStart, proto.ScanStart{Path: tree})
	checkTotals(t, v.collect(), tree)

	// wrong sid -> 4404
	ctx := ctx10(t)
	bad := &crypto.Secret{}
	_, _ = rand.Read(bad.SID[:])
	ws, err := dialRaw(ctx, wsBase+"/v1/view?sid="+bad.SIDString())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = ws.Read(ctx); closeStatus(err) != 4404 {
		t.Fatalf("wrong sid: %v", err)
	}
	// wrong Origin rejected before upgrade
	_, resp, err := websocket.Dial(ctx, wsBase+"/v1/view?sid="+sec.SIDString(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"http://evil.example"}},
	})
	if err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("evil origin: err=%v resp=%v", err, resp)
	}
	// matching Origin accepted
	ws2, _, err := websocket.Dial(ctx, wsBase+"/v1/view?sid="+sec.SIDString(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"http://" + hostport}},
	})
	if err != nil {
		t.Fatalf("good origin: %v", err)
	}
	ws2.CloseNow()
	// wrong Host rejected
	req, _ := http.NewRequest("GET", "http://"+hostport+"/", nil)
	req.Host = "evil.example"
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 403 {
		t.Fatalf("wrong Host: %d", r.StatusCode)
	}
	// right Host: served (404 is fine without a built SPA) with no 403
	req2, _ := http.NewRequest("GET", "http://"+hostport+"/s/"+sec.SIDString(), nil)
	r2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode == 403 {
		t.Fatal("legit Host rejected")
	}
	if a.LocalURL() == "" {
		t.Fatal("LocalURL empty")
	}
}

func TestIdleExit(t *testing.T) {
	rec := newRecorder()
	a, err := NewAgent(Config{Local: true, Idle: 300 * time.Millisecond, Events: rec.on})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not exit when idle")
	}
	if _, ok := rec.find(EvIdle); !ok {
		t.Fatal("no idle event")
	}
}

func TestHandshakeRateLimit(t *testing.T) {
	e := newEnv(t, Config{})
	ctx := ctx10(t)
	var limited bool
	for i := 0; i < 25 && !limited; i++ {
		ws, err := dialRaw(ctx, e.wsURL+"/v1/view?sid="+e.sec.SIDString())
		if err != nil {
			t.Fatal(err)
		}
		m1 := make([]byte, 82)
		_ = ws.Write(ctx, websocket.MessageBinary, m1)
		_, _, err = ws.Read(ctx)
		if closeStatus(err) == 4429 {
			limited = true
		}
	}
	if !limited {
		t.Fatal("handshake attempts were never rate limited")
	}
}
