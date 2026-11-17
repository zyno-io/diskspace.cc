// SPDX-License-Identifier: MIT

//go:build !windows

package session

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

// Regression: a viewer that stops reading and keeps sending SCAN_START grows the control
// queue past ctrlHardEntries/ctrlHardBytes: attach() bypasses the caps.
func TestScanStartRespectsCtrlCap(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("p", 200)
	wide := filepath.Join(root, long, long, long)
	if err := os.MkdirAll(wide, 0o755); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(root, "big")
	rb := make([]byte, 60)
	for i := 0; i < 60; i++ {
		par := filepath.Join(big, fmt.Sprintf("p%03d", i))
		os.MkdirAll(par, 0o755)
		for j := 0; j < 500; j++ {
			crand.Read(rb)
			os.Mkdir(filepath.Join(par, hex.EncodeToString(rb)), 0o755)
		}
	}
	a := startAgent(t, Config{Local: true, Events: func(Event) {}})
	var addr string
	for i := 0; i < 100 && addr == ""; i++ {
		a.mu.Lock()
		addr = a.localAddr
		a.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	g := a.curGen()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	d := &net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) { syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096) })
	}}
	hc := &http.Client{Transport: &http.Transport{DialContext: d.DialContext}}
	ws, _, err := websocket.Dial(ctx, "ws://"+addr+"/v1/view?sid="+g.sec.SIDString(), &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ciph, _, err := handshake(ctx, ws, g.sec)
	if err != nil {
		t.Fatal(err)
	}
	send := func(typ byte, v any) error {
		b, _ := proto.EncodeMsg(typ, v)
		ct, _ := ciph.Seal(proto.FrameV2A(b))
		return ws.Write(ctx, websocket.MessageBinary, ct)
	}
	send(proto.MsgClientHello, proto.ClientHello{Proto: 1, Caps: proto.ClientCaps{ZstdStream: true}})
	time.Sleep(200 * time.Millisecond)
	a.mu.Lock()
	act := a.active
	a.mu.Unlock()
	snd := act.snd
	var maxN, maxB int
	stat := func(label string) {
		snd.mu.Lock()
		n, b, of, cr := len(snd.ctrl), snd.ctrlBytes, snd.overflowed, snd.credit
		snd.mu.Unlock()
		maxN, maxB = max(maxN, n), max(maxB, b)
		a.mu.Lock()
		seq := a.scanSeq
		a.mu.Unlock()
		t.Logf("%s: ctx=%v scans=%d credit=%d ctrl entries=%d bytes=%d KiB overflowed=%v (hard caps %d / %d KiB)", label, act.ctx.Err(), seq, cr, n, b>>10, of, ctrlHardEntries, ctrlHardBytes>>10)
	}
	send(proto.MsgScanStart, proto.ScanStart{Path: big})
	time.Sleep(4 * time.Second)
	stat("after big scan")
	t0 := time.Now()
	for i := 0; i < 2000 && time.Since(t0) < 10*time.Second; i++ {
		if err := send(proto.MsgScanStart, proto.ScanStart{Path: wide}); err != nil {
			t.Logf("write err at %d: %v", i, err)
			break
		}
		time.Sleep(5 * time.Millisecond)
		snd.mu.Lock()
		maxN, maxB = max(maxN, len(snd.ctrl)), max(maxB, snd.ctrlBytes)
		snd.mu.Unlock()
	}
	stat("end")
	if maxN > ctrlHardEntries || maxB > ctrlHardBytes {
		t.Fatalf("control queue exceeded hard caps: max entries=%d bytes=%d", maxN, maxB)
	}
}
