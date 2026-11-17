// SPDX-License-Identifier: MIT

//go:build !windows

package session

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zyno-io/diskspace.cc/internal/proto"
	"github.com/zyno-io/diskspace.cc/internal/relay"
)

// A viewer-less attacker who knows only the session id sends garbage M1s; the
// real viewer (valid MAC) must still get in.
func TestSidOnlyGarbageM1DoesNotLockOutViewer(t *testing.T) {
	e := newEnv(t, Config{})
	ctx := ctx10(t)
	for i := 0; i < 25; i++ {
		ws, err := dialRaw(ctx, e.wsURL+"/v1/view?sid="+e.sec.SIDString())
		if err != nil {
			t.Fatal(err)
		}
		_ = ws.Write(ctx, websocket.MessageBinary, make([]byte, 82))
		_, _, _ = ws.Read(ctx)
		ws.CloseNow()
	}
	ws, err := dialRaw(ctx, e.wsURL+"/v1/view?sid="+e.sec.SIDString())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if _, _, err := handshake(ctx, ws, e.sec); err != nil {
		t.Fatalf("legitimate viewer refused after garbage M1s: %v (status %d)", err, closeStatus(err))
	}
}

// A connection that never sends M1 is dropped quickly (and charged).
func TestM1Timeout(t *testing.T) {
	e := newEnv(t, Config{})
	ctx := ctx10(t)
	ws, err := dialRaw(ctx, e.wsURL+"/v1/view?sid="+e.sec.SIDString())
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	t0 := time.Now()
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("expected the connection to be closed")
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("idle pre-handshake socket held for %v, want ~2s", d)
	}
}

// DIR_FILES must not follow an ancestor swapped for a symlink after the scan.
func TestDirFilesRefusesSwappedAncestor(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	secret := filepath.Join(parent, "outside")
	os.MkdirAll(filepath.Join(root, "x", "sub"), 0o755)
	os.MkdirAll(filepath.Join(secret, "sub"), 0o755)
	os.WriteFile(filepath.Join(secret, "sub", "SECRET-outside-root.txt"), make([]byte, 5000), 0o600)
	e := newEnv(t, Config{Root: root})
	v := e.viewer()
	v.handshakeMsgs()
	v.send(proto.MsgScanStart, proto.ScanStart{Path: root})
	r := v.collect()
	var subID uint64
	var found bool
	for _, rec := range r.Recs {
		if d, ok := rec.(*proto.DirRecord); ok {
			for i, c := range d.Dirs {
				if c.Name == "sub" {
					subID, found = d.FirstChildID+uint64(i), true
				}
			}
		}
	}
	if !found {
		t.Fatal("sub not found")
	}

	// Sanity: before the swap the request works.
	v.send(proto.MsgDirFilesReq, proto.DirFilesReq{ReqID: 8, ScanID: r.Started.ScanID, DirID: subID, Limit: 100})
	v.expect(proto.MsgDirFiles, nil)

	os.RemoveAll(filepath.Join(root, "x"))
	if err := os.Symlink(secret, filepath.Join(root, "x")); err != nil {
		t.Fatal(err)
	}
	v.send(proto.MsgDirFilesReq, proto.DirFilesReq{ReqID: 9, ScanID: r.Started.ScanID, DirID: subID, Limit: 100})
	for {
		typ, body := v.must(15 * time.Second)
		if typ == proto.MsgDirFiles {
			t.Fatalf("DIR_FILES served from outside the root: %q", body)
		}
		if typ == proto.MsgError {
			if !strings.Contains(string(body), `"changed"`) {
				t.Fatalf("error %q, want code changed", body)
			}
			return
		}
	}
}

// BROWSE of a directory swapped to an outside symlink is rejected.
func TestBrowseSwappedDirRejected(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	secret := filepath.Join(parent, "outside")
	os.MkdirAll(filepath.Join(root, "x"), 0o755)
	os.MkdirAll(filepath.Join(secret, "leak"), 0o755)
	e := newEnv(t, Config{Root: root})
	v := e.viewer()
	v.handshakeMsgs()
	os.RemoveAll(filepath.Join(root, "x"))
	if err := os.Symlink(secret, filepath.Join(root, "x")); err != nil {
		t.Fatal(err)
	}
	v.send(proto.MsgBrowse, proto.Browse{ReqID: 1, Path: filepath.Join(root, "x")})
	var br proto.BrowseResult
	v.expect(proto.MsgBrowseResult, &br)
	if len(br.Dirs) != 0 || br.Error == "" {
		t.Fatalf("BROWSE through swapped dir: %+v", br)
	}
}

// With the sender stalled (viewer not reading), the command loop must stop
// answering once the control queue is full instead of queueing without bound.
func TestCmdLoopBackpressure(t *testing.T) {
	a := startAgent(t, Config{Local: true, Events: func(Event) {}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := &viewerConn{a: a, ctx: ctx, cancel: cancel, cmds: make(chan cmd, cmdQueueLen), snd: newSender(nil, nil, nil)}
	go v.cmdLoop()
	for i := 0; i < 120; i++ {
		select {
		case v.cmds <- cmd{typ: proto.MsgGetRoots}:
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n, b := ctrlStat(v.snd); n > 64 || b > 4<<20 {
		t.Fatalf("control queue grew to %d msgs / %d KiB with a stalled sender", n, b>>10)
	}
	// Draining the queue lets the loop resume.
	before, _ := ctrlStat(v.snd)
	for i := 0; i < 10; i++ {
		v.snd.pick()
	}
	time.Sleep(200 * time.Millisecond)
	if after, _ := ctrlStat(v.snd); after < before {
		t.Fatalf("loop did not resume after drain: %d -> %d", before, after)
	}
}

func ctrlStat(snd *sender) (n, bytes int) {
	snd.mu.Lock()
	defer snd.mu.Unlock()
	for _, b := range snd.ctrl {
		bytes += len(b)
	}
	return len(snd.ctrl), bytes
}

func TestPongCoalesced(t *testing.T) {
	s := newSender(nil, nil, nil)
	for i := 0; i < 10000; i++ {
		s.pushPong()
	}
	if n, _ := ctrlStat(s); n != 0 {
		t.Fatalf("%d PONGs queued in ctrl", n)
	}
	p := s.pick()
	if len(p.body) != 1 || p.body[0] != proto.MsgPong {
		t.Fatalf("first pick %+v, want one PONG", p)
	}
	if p := s.pick(); p.body != nil {
		t.Fatalf("second pick returned %v, PONGs were not coalesced", p.body)
	}
}

func TestCtrlOverflowFires(t *testing.T) {
	s := newSender(nil, nil, nil)
	fired := make(chan struct{}, 4)
	s.onOverflow = func() { fired <- struct{}{} }
	for i := 0; i < 200; i++ {
		s.push([]byte{proto.MsgPong})
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("overflow not reported")
	}
	if n, _ := ctrlStat(s); n > 64 {
		t.Fatalf("queued %d > 64", n)
	}
}

func TestPushWaitBlocksUntilRoom(t *testing.T) {
	s := newSender(nil, nil, nil)
	for i := 0; i < 32; i++ {
		if !s.pushWait(context.Background(), []byte{1}) {
			t.Fatal("unexpected failure")
		}
	}
	done := make(chan bool, 1)
	go func() { done <- s.pushWait(context.Background(), []byte{2}) }()
	select {
	case <-done:
		t.Fatal("pushWait did not block on a full queue")
	case <-time.After(100 * time.Millisecond):
	}
	s.pick() // frees one slot
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("pushWait failed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pushWait still blocked after room appeared")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for n, _ := ctrlStat(s); n < 32; n++ {
		s.pushWait(context.Background(), []byte{3})
	}
	if s.pushWait(ctx, []byte{4}) {
		t.Fatal("pushWait ignored a cancelled ctx")
	}
}

// A hostile relay announcing many viewers (and repeating cids) must not make
// the agent open more than maxDataSockets data connections at once.
func TestRelayViewerFlood(t *testing.T) {
	var (
		mu      sync.Mutex
		hits    = map[string]int{}
		cur     int
		maxCur  int
		release = make(chan struct{})
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		send := func(m relay.ControlMsg) {
			b := []byte(fmt.Sprintf(`{"t":%q,"cid":%q}`, m.T, m.CID))
			_ = c.Write(ctx, websocket.MessageText, b)
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"t":"registered","ttl":3600}`))
		for i := 0; i < 5; i++ {
			send(relay.Viewer("dup")) // same cid repeatedly
		}
		for i := 0; i < 30; i++ {
			send(relay.Viewer(fmt.Sprintf("c%d", i)))
		}
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/v1/agent/data", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		mu.Lock()
		hits[r.URL.Query().Get("cid")]++
		cur++
		maxCur = max(maxCur, cur)
		mu.Unlock()
		defer func() { mu.Lock(); cur--; mu.Unlock() }()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	defer close(release)

	startAgent(t, Config{RelayURL: ts.URL, Version: "test", Events: func(Event) {}})
	time.Sleep(1500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if maxCur > 4 {
		t.Fatalf("%d concurrent data sockets, want <= 4", maxCur)
	}
	if hits["dup"] != 1 {
		t.Fatalf("duplicate cid dialed %d times, want 1", hits["dup"])
	}
	if maxCur == 0 {
		t.Fatal("agent never dialed a data socket")
	}
}
