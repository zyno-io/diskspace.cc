// SPDX-License-Identifier: MIT

package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

var capsNew = proto.ClientCaps{ZstdStream: true, ScanState: true}
var capsOld = proto.ClientCaps{ZstdStream: true}

// startPaused makes every scan begin paused (the first directory blocks at the
// scanner's gate), so tests do not race the scan.
func startPaused(t *testing.T) {
	t.Helper()
	hookBeforeRun = func(r *scanRun) { r.setPause(true) }
	t.Cleanup(func() { hookBeforeRun = nil })
}

func (v *testViewer) expectState(want bool) proto.ScanState {
	v.t.Helper()
	var st proto.ScanState
	v.expect(proto.MsgScanState, &st)
	if st.Paused != want {
		v.t.Fatalf("SCAN_STATE paused=%v, want %v", st.Paused, want)
	}
	return st
}

// quiet asserts no NODES or end message arrives within d (a paused scan sends none).
func (v *testViewer) quiet(d time.Duration) {
	v.t.Helper()
	deadline := time.Now().Add(d)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return
		}
		typ, _, err := v.recv(left)
		if err == errRecvTimeout {
			return
		}
		if err != nil {
			v.t.Fatalf("recv: %v", err)
		}
		if typ == proto.MsgNodes || typ == proto.MsgScanDone || typ == proto.MsgScanEnded {
			v.t.Fatalf("message %#x arrived while paused", typ)
		}
	}
}

func TestE2EPauseResume(t *testing.T) {
	tree := t.TempDir()
	mkTree(t, tree, 40, 5)
	startPaused(t)
	e := newEnv(t, Config{})
	v := connectViewerCaps(t, e.wsURL, e.sec, capsNew)
	info := v.handshakeMsgs()
	if !info.Features.Pause {
		t.Fatal("AGENT_INFO.features.pause not set")
	}
	v.send(proto.MsgScanStart, proto.ScanStart{Path: tree})
	v.expect(proto.MsgScanStarted, nil)
	v.expectState(true) // part of the attach
	v.quiet(300 * time.Millisecond)
	e.rec.wait(t, EvScanStarted, 5*time.Second)

	// idempotent: pausing again replies with the same state
	v.send(proto.MsgScanPause, proto.ScanPause{ScanID: 1, Paused: true})
	v.expectState(true)

	v.send(proto.MsgScanPause, proto.ScanPause{ScanID: 1, Paused: false})
	v.expectState(false)
	e.rec.wait(t, EvScanResumed, 5*time.Second)
	r := v.collectScan(1)
	checkTotals(t, r, tree)
	if r.Done == nil || r.Done.Ms < 0 {
		t.Fatalf("done: %+v", r.Done)
	}
}

// A paused scan stays paused across a viewer reconnect, and the replay says so.
func TestE2EPauseReplay(t *testing.T) {
	tree := t.TempDir()
	mkTree(t, tree, 20, 4)
	startPaused(t)
	e := newEnv(t, Config{})
	v := connectViewerCaps(t, e.wsURL, e.sec, capsNew)
	v.handshakeMsgs()
	v.send(proto.MsgScanStart, proto.ScanStart{Path: tree})
	v.expect(proto.MsgScanStarted, nil)
	v.expectState(true)
	v.ws.CloseNow()

	v2 := connectViewerCaps(t, e.wsURL, e.sec, capsNew)
	info := v2.handshakeMsgs()
	if info.Scan == nil || info.Scan.State != "running" || !info.Scan.Paused {
		t.Fatalf("AGENT_INFO.scan=%+v, want running+paused", info.Scan)
	}
	v2.expect(proto.MsgScanStarted, nil)
	typ, _ := v2.must(5 * time.Second)
	if typ != proto.MsgScanState {
		t.Fatalf("message after SCAN_STARTED is %#x, want SCAN_STATE", typ)
	}
	v2.expect(proto.MsgReplayDone, nil)
	v2.send(proto.MsgScanPause, proto.ScanPause{ScanID: 1, Paused: false})
	v2.expectState(false)
	checkTotals(t, v2.collectScan(1), tree)
}

// A viewer without caps.scanState never receives SCAN_STATE, and attaching to
// a paused scan resumes it.
func TestE2ENoScanStateForOldViewer(t *testing.T) {
	tree := t.TempDir()
	mkTree(t, tree, 20, 4)
	startPaused(t)
	e := newEnv(t, Config{})
	nv := connectViewerCaps(t, e.wsURL, e.sec, capsNew)
	nv.handshakeMsgs()
	nv.send(proto.MsgScanStart, proto.ScanStart{Path: tree})
	nv.expect(proto.MsgScanStarted, nil)
	nv.expectState(true)

	ov := connectViewerCaps(t, e.wsURL, e.sec, capsOld)
	info := ov.handshakeMsgs()
	if info.Scan != nil && info.Scan.Paused {
		t.Fatal("old viewer was told the scan is paused")
	}
	e.rec.wait(t, EvScanResumed, 5*time.Second) // auto-resume
	done := false
	for !done {
		typ, body := ov.must(30 * time.Second)
		switch typ {
		case proto.MsgScanState:
			t.Fatal("old viewer received SCAN_STATE")
		case proto.MsgScanDone:
			var d proto.ScanDone
			_ = json.Unmarshal(body[1:], &d)
			done = true
		}
	}
}

// The extension rule does not loosen the strict parser: an unknown v2a type
// still closes the connection with 4402.
func TestE2EUnknownTypeStillFatal(t *testing.T) {
	e := newEnv(t, Config{})
	v := connectViewerCaps(t, e.wsURL, e.sec, capsNew)
	v.handshakeMsgs()
	v.sendRaw(proto.FrameV2A([]byte{0x8a}))
	for {
		_, _, err := v.recv(5 * time.Second)
		if err != nil {
			if c := closeStatus(err); c != 4402 {
				t.Fatalf("want close 4402, got %v", err)
			}
			return
		}
	}
}

// fakeRun installs a finished scan whose log holds nodes NODES entries.
func fakeRun(e *testEnv, nodes int, end []byte, done bool, state string) {
	log := newScanLog(1, 1<<20)
	for i := 0; i < nodes; i++ {
		log.append([]byte{proto.MsgNodes, 1, 0})
	}
	log.finish(logEnd{Body: end, Done: done})
	started, _ := proto.EncodeMsg(proto.MsgScanStarted, proto.ScanStarted{ScanID: 1, Path: "/x", RootName: "x"})
	r := &scanRun{a: e.agent, id: 1, path: "/x", log: log, startedBody: started, done: make(chan struct{}), started: time.Now(), state: state, cancel: func() {}}
	close(r.done)
	e.agent.mu.Lock()
	e.agent.cur = r
	e.agent.mu.Unlock()
}

func readTypes(v *testViewer, until byte) []byte {
	var got []byte
	for {
		typ, _ := v.must(10 * time.Second)
		got = append(got, typ)
		if typ == until {
			return got
		}
	}
}

func wantTypes(t *testing.T, got []byte, want ...byte) {
	t.Helper()
	if string(got) != string(want) {
		t.Fatalf("message order % x, want % x", got, want)
	}
}

// Reconnecting after Stop replays every logged NODES, then SCAN_ENDED.
func TestCanceledScanReplayIncludesNodes(t *testing.T) {
	e := newEnv(t, Config{})
	end, _ := proto.EncodeMsg(proto.MsgScanEnded, proto.ScanEnded{ScanID: 1, Reason: "canceled"})
	fakeRun(e, 3, end, false, "canceled")
	v := e.viewer()
	got := readTypes(v, proto.MsgReplayDone)
	wantTypes(t, got[2:], // skip STREAM_INIT, AGENT_INFO
		proto.MsgScanStarted, proto.MsgNodes, proto.MsgNodes, proto.MsgNodes, proto.MsgScanEnded, proto.MsgReplayDone)
}

// REPLAY_DONE comes after the end message.
func TestReplayDoneAfterEnd(t *testing.T) {
	e := newEnv(t, Config{})
	end, _ := proto.EncodeMsg(proto.MsgScanDone, proto.ScanDone{ScanID: 1})
	fakeRun(e, 2, end, true, "done")
	v := e.viewer()
	got := readTypes(v, proto.MsgReplayDone)
	wantTypes(t, got[2:],
		proto.MsgScanStarted, proto.MsgNodes, proto.MsgNodes, proto.MsgScanDone, proto.MsgReplayDone)
}

// A live viewer that stopped a scan gets SCAN_ENDED without waiting behind
// unsent NODES (a replay, tested above, delivers them first).
func TestLiveCancelEndsPromptly(t *testing.T) {
	log := newScanLog(1, 1<<20)
	for i := 0; i < 3; i++ {
		log.append([]byte{proto.MsgNodes, 1, 0})
	}
	end, _ := proto.EncodeMsg(proto.MsgScanEnded, proto.ScanEnded{ScanID: 1, Reason: "canceled"})
	log.finish(logEnd{Body: end})
	started, _ := proto.EncodeMsg(proto.MsgScanStarted, proto.ScanStarted{ScanID: 1})
	s := newSender(nil, nil, nil)
	s.attach(&scanRun{id: 1, log: log, startedBody: started, state: "canceled"}, false)
	if p := s.pick(); p.body[0] != proto.MsgScanStarted {
		t.Fatalf("first message %#x", p.body[0])
	}
	if p := s.pick(); p.body == nil || p.body[0] != proto.MsgScanEnded {
		t.Fatalf("second message %v, want SCAN_ENDED", p.body)
	}
}

// An ended scan has no pause state: a pause request is not answered with a
// "not paused" that a viewer could mistake for running.
func TestNoScanStateForEndedScan(t *testing.T) {
	e := newEnv(t, Config{})
	end, _ := proto.EncodeMsg(proto.MsgScanEnded, proto.ScanEnded{ScanID: 1, Reason: "canceled"})
	fakeRun(e, 1, end, false, "canceled")
	v := connectViewerCaps(t, e.wsURL, e.sec, capsNew)
	readTypes(v, proto.MsgReplayDone)
	v.send(proto.MsgScanPause, proto.ScanPause{ScanID: 1, Paused: true})
	v.send(proto.MsgScanPause, proto.ScanPause{ScanID: 1, Paused: false})
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		typ, _, err := v.recv(time.Until(deadline))
		if err == errRecvTimeout {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if typ == proto.MsgScanState {
			t.Fatal("SCAN_STATE sent for an ended scan")
		}
	}
}

// A connection that was superseded cannot pause or resume the scan.
func TestSupersededViewerCannotPause(t *testing.T) {
	tree := t.TempDir()
	mkTree(t, tree, 20, 4)
	startPaused(t)
	e := newEnv(t, Config{})
	v1 := connectViewerCaps(t, e.wsURL, e.sec, capsNew)
	v1.handshakeMsgs()
	v1.send(proto.MsgScanStart, proto.ScanStart{Path: tree})
	v1.expect(proto.MsgScanStarted, nil)
	v1.expectState(true)

	v2 := connectViewerCaps(t, e.wsURL, e.sec, capsNew)
	v2.handshakeMsgs()
	v2.expect(proto.MsgReplayDone, nil)

	// v1 is superseded; its socket may still be draining. Ignore write errors.
	b, _ := proto.EncodeMsg(proto.MsgScanPause, proto.ScanPause{ScanID: 1, Paused: false})
	if ct, err := v1.ciph.Seal(proto.FrameV2A(b)); err == nil {
		ctx := ctx10(t)
		_ = v1.ws.Write(ctx, websocket.MessageBinary, ct)
	}
	v2.quiet(400 * time.Millisecond) // a resumed scan would send NODES or SCAN_DONE
	if p, _ := e.agent.currentRun().pauseState(); !p {
		t.Fatal("superseded viewer resumed the scan")
	}
}
