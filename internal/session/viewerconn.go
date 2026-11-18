// SPDX-License-Identifier: MIT

package session

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/zyno-io/diskspace.cc/internal/proto"
	"github.com/zyno-io/diskspace.cc/internal/scan"
	"github.com/zyno-io/diskspace.cc/internal/session/crypto"
)

// Close codes sent to viewers.
const (
	closeVersion    = 4400
	closeAuth       = 4401
	closeProtocol   = 4402
	closeRateLimit  = 4429
	closeUnknownSID = 4404

	handshakeTimeout = 10 * time.Second
	// m1Timeout bounds the wait for M1. It is short because an attacker who
	// only knows the session id can park connections here; the real viewer
	// sends M1 immediately after the socket opens.
	m1Timeout = 2 * time.Second
	// errCodeChanged is the ERROR code for a directory that no longer matches
	// what was scanned.
	errCodeChanged = "changed"
	maxV2A         = 64 << 10
	cmdQueueLen    = 64
)

type cmd struct {
	typ     byte
	payload []byte
}

// viewerConn is one viewer WebSocket (relay data socket or local socket).
type viewerConn struct {
	a    *Agent
	ws   *websocket.Conn
	code string // verification code once handshaken

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	snd        *sender // nil until activated
	cmds       chan cmd
	superseded bool
	activated  bool
	mu         sync.Mutex
}

// kill drops the connection immediately.
func (v *viewerConn) kill() {
	v.cancel()
	_ = v.ws.CloseNow()
}

// stop asks a handshaken viewer to leave with BYE{reason}; others are killed.
func (v *viewerConn) stop(reason string) {
	v.mu.Lock()
	snd := v.snd
	v.mu.Unlock()
	if snd == nil {
		v.kill()
		return
	}
	snd.bye(reason)
}

func (v *viewerConn) fail(code int, reason string) {
	_ = v.ws.Close(websocket.StatusCode(code), reason)
	v.cancel()
}

// serveViewer runs the whole lifetime of one viewer connection. g is the
// link generation the connection was accepted under.
func (a *Agent) serveViewer(ws *websocket.Conn, g *generation) {
	ctx, cancel := context.WithCancel(g.ctx)
	v := &viewerConn{a: a, ws: ws, ctx: ctx, cancel: cancel, done: make(chan struct{}), cmds: make(chan cmd, cmdQueueLen)}
	defer close(v.done)
	defer cancel()

	a.mu.Lock()
	if a.gen != g {
		a.mu.Unlock()
		_ = ws.CloseNow()
		return
	}
	a.conns[v] = struct{}{}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.conns, v)
		a.mu.Unlock()
	}()
	defer ws.CloseNow()

	ws.SetReadLimit(maxV2A + 64)

	// M1. Anyone who knows only the session id can open this socket, so the
	// handshake limiter is charged only for failures (timeout, malformed, bad
	// MAC); a hello whose MAC verifies proves knowledge of K and is never
	// refused.
	rctx, rcancel := context.WithTimeout(ctx, m1Timeout)
	mt, m1, err := ws.Read(rctx)
	rcancel()
	if err != nil || mt != websocket.MessageBinary {
		a.hsAllow() // charge the failure; the connection is dropped either way
		return
	}
	m2, keys, err := crypto.AgentRespond(g.sec, m1, rand.Reader)
	switch {
	case errors.Is(err, crypto.ErrVersion), errors.Is(err, crypto.ErrMalformed), errors.Is(err, crypto.ErrBadMAC):
		if !a.hsAllow() {
			v.fail(closeRateLimit, "too many attempts")
			return
		}
		if errors.Is(err, crypto.ErrVersion) {
			v.fail(closeVersion, "unsupported version")
		} else {
			v.fail(closeAuth, "authentication failed")
		}
		return
	case err != nil:
		v.fail(int(websocket.StatusInternalError), "internal error")
		return
	}
	wctx, wcancel := context.WithTimeout(ctx, handshakeTimeout)
	err = ws.Write(wctx, websocket.MessageBinary, m2)
	wcancel()
	if err != nil {
		return
	}
	ciph := crypto.NewAgentCipher(keys)

	// frame #0: CLIENT_HELLO
	rctx, rcancel = context.WithTimeout(ctx, handshakeTimeout)
	mt, ct, err := ws.Read(rctx)
	rcancel()
	if err != nil || mt != websocket.MessageBinary {
		return
	}
	pt, err := ciph.Open(ct)
	if err != nil {
		v.fail(closeProtocol, "protocol error")
		return
	}
	typ, payload, err := proto.ParseV2A(pt)
	var hello proto.ClientHello
	if err != nil || typ != proto.MsgClientHello || json.Unmarshal(payload, &hello) != nil {
		v.fail(closeProtocol, "protocol error")
		return
	}
	if hello.Proto != proto.ProtoVersion {
		v.fail(closeVersion, "unsupported version")
		return
	}
	v.code = keys.Code

	comp := proto.NewStreamCompressor(hello.Caps.ZstdStream)
	defer comp.Close()
	snd := newSender(ws, ciph, comp)
	snd.scanState = hello.Caps.ScanState
	snd.onOverflow = func() { v.fail(closeRateLimit, "control queue overflow") }
	if !a.activate(v, snd) {
		return
	}
	defer a.deactivate(v)

	go func() {
		if err := snd.run(ctx); err != nil && !errors.Is(err, errSenderClosed) {
			v.kill()
		} else if errors.Is(err, errSenderClosed) {
			v.cancel()
		}
	}()
	go v.cmdLoop()

	// reader loop
	for {
		mt, ct, err := ws.Read(ctx)
		if err != nil {
			return
		}
		if mt != websocket.MessageBinary {
			v.fail(closeProtocol, "protocol error")
			return
		}
		pt, err := ciph.Open(ct)
		if err != nil {
			v.fail(closeProtocol, "protocol error")
			return
		}
		typ, payload, err := proto.ParseV2A(pt)
		if err != nil || len(payload)+1 > maxV2A {
			v.fail(closeProtocol, "protocol error")
			return
		}
		switch typ {
		case proto.MsgPing:
			snd.pushPong()
		case proto.MsgCredit:
			var m proto.Credit
			if json.Unmarshal(payload, &m) != nil {
				v.fail(closeProtocol, "protocol error")
				return
			}
			snd.addCredit(m.N)
		case proto.MsgGetRoots, proto.MsgBrowse, proto.MsgScanStart, proto.MsgScanCancel, proto.MsgScanPause, proto.MsgDirFilesReq:
			select {
			case v.cmds <- cmd{typ, payload}:
			default:
				v.fail(closeRateLimit, "too many requests")
				return
			}
		default: // CLIENT_HELLO again, a2v types, unknown
			v.fail(closeProtocol, "protocol error")
			return
		}
	}
}

// activate marks v the active viewer, supersedes the previous one, and queues
// AGENT_INFO plus the scan replay. It returns false if v was dropped meanwhile.
func (a *Agent) activate(v *viewerConn, snd *sender) bool {
	// Auto-scan on the first connect.
	a.mu.Lock()
	auto := a.autoPending
	a.autoPending = false
	a.mu.Unlock()
	if auto {
		p := filepath.Clean(a.cfg.AutoScan)
		if _, id, ok := a.guard.WithinResolved(p); filepath.IsAbs(p) && ok {
			if _, err := a.startScan(scan.Options{Root: p, ExpectRoot: id}); err != nil {
				a.emit(Event{Kind: EvError, Err: err, Message: "auto-scan: " + err.Error()})
			}
		}
	}
	info := a.buildInfo()

	a.mu.Lock()
	if v.ctx.Err() != nil {
		a.mu.Unlock()
		return false
	}
	old := a.active
	a.active = v
	a.viewers[v] = struct{}{}
	v.mu.Lock()
	v.snd = snd
	v.activated = true
	v.mu.Unlock()
	run := a.cur
	if run != nil {
		info.Scan = &proto.ScanRef{ID: run.id, State: run.getState()}
		if paused, _ := run.pauseState(); paused && snd.scanState {
			info.Scan.Paused = true
		}
	}
	snd.pushJSON(proto.MsgAgentInfo, info)
	if run != nil {
		snd.attach(run, true)
	} else {
		snd.push([]byte{proto.MsgReplayDone})
	}
	a.mu.Unlock()

	if old != nil && old != v {
		old.mu.Lock()
		old.superseded = true
		osnd := old.snd
		old.mu.Unlock()
		if osnd != nil {
			osnd.bye(proto.ByeSuperseded)
		}
		a.emit(Event{Kind: EvViewerSuperseded, Code: old.code})
	}
	// A viewer that cannot show or end a pause would leave the scan stuck.
	if run != nil && !snd.scanState {
		a.resumeIfPaused(run)
	}
	a.emit(Event{Kind: EvViewerConnected, Code: v.code})
	return true
}

func (a *Agent) deactivate(v *viewerConn) {
	a.mu.Lock()
	delete(a.viewers, v)
	if a.active == v {
		a.active = nil
	}
	if len(a.viewers) == 0 {
		a.lastViewer = time.Now()
	}
	a.mu.Unlock()
	v.mu.Lock()
	sup := v.superseded
	v.mu.Unlock()
	if !sup {
		a.emit(Event{Kind: EvViewerDisconnected, Code: v.code})
	}
}

// cmdLoop runs the slower v2a requests in order, off the reader goroutine so
// CREDIT and PING stay responsive.
func (v *viewerConn) cmdLoop() {
	for {
		select {
		case <-v.ctx.Done():
			return
		case c := <-v.cmds:
			v.handle(c)
		}
	}
}

func (v *viewerConn) handle(c cmd) {
	a, snd := v.a, v.snd
	switch c.typ {
	case proto.MsgGetRoots:
		snd.pushJSONWait(v.ctx, proto.MsgRoots, struct {
			Volumes []proto.Volume `json:"volumes"`
		}{append([]proto.Volume{}, a.volumes()...)})

	case proto.MsgBrowse:
		var m proto.Browse
		if json.Unmarshal(c.payload, &m) != nil {
			v.fail(closeProtocol, "protocol error")
			return
		}
		snd.pushJSONWait(v.ctx, proto.MsgBrowseResult, a.browse(m))

	case proto.MsgScanStart:
		var m proto.ScanStart
		if json.Unmarshal(c.payload, &m) != nil {
			v.fail(closeProtocol, "protocol error")
			return
		}
		if !filepath.IsAbs(m.Path) {
			snd.pushErrorWait(v.ctx, "bad_path", "path must be absolute")
			return
		}
		p := filepath.Clean(m.Path)
		_, rootIdent, ok := a.guard.WithinResolved(p)
		if !ok {
			snd.pushErrorWait(v.ctx, "denied", "path is outside the allowed root")
			return
		}
		opts := scan.Options{Root: p, ExpectRoot: rootIdent, CrossMounts: m.CrossMounts, TopFiles: m.TopFiles, MinFileBytes: m.MinFileBytes}
		if opts.TopFiles > 64 {
			opts.TopFiles = 64
		}
		// Backpressure: do not start a scan (which attaches SCAN_STARTED) for a
		// viewer that is not draining its control queue.
		if !snd.waitRoom(v.ctx) {
			return
		}
		if _, err := a.startScan(opts); err != nil {
			snd.pushErrorWait(v.ctx, "scan_failed", errMessage(err))
		}

	case proto.MsgScanCancel:
		var m proto.ScanCancel
		if json.Unmarshal(c.payload, &m) != nil {
			v.fail(closeProtocol, "protocol error")
			return
		}
		a.cancelScan(m.ScanID)

	case proto.MsgScanPause:
		var m proto.ScanPause
		if json.Unmarshal(c.payload, &m) != nil {
			v.fail(closeProtocol, "protocol error")
			return
		}
		// A superseded connection (BYE queued, still draining) must not steer the scan.
		a.mu.Lock()
		active := a.active == v
		a.mu.Unlock()
		if active {
			a.setPaused(m.ScanID, m.Paused)
		}

	case proto.MsgDirFilesReq:
		var m proto.DirFilesReq
		if json.Unmarshal(c.payload, &m) != nil {
			v.fail(closeProtocol, "protocol error")
			return
		}
		v.dirFiles(m)
	}
}

func errMessage(err error) string {
	switch {
	case errors.Is(err, scan.ErrChanged):
		return "changed"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, fs.ErrNotExist):
		return "not found"
	case errors.Is(err, syscall.ENOTDIR):
		return "not a directory"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

func (a *Agent) browse(m proto.Browse) proto.BrowseResult {
	res := proto.BrowseResult{ReqID: m.ReqID, Path: m.Path}
	if !filepath.IsAbs(m.Path) {
		res.Error = "path must be absolute"
		return res
	}
	p := filepath.Clean(m.Path)
	res.Path = p
	resolved, ident, ok := a.guard.WithinResolved(p)
	if !ok {
		res.Error = "outside_root"
		return res
	}
	// List the path the guard resolved, verifying it is still the directory
	// the guard checked (closes the check/use window for swapped ancestors).
	dirs, err := scan.ListSubdirsExpect(resolved, ident)
	if err != nil {
		res.Error = errMessage(err)
		return res
	}
	res.Dirs = dirs
	if par := filepath.Dir(p); par != p && a.guard.Within(par) {
		res.Parent = &par
	}
	return res
}

func (v *viewerConn) dirFiles(m proto.DirFilesReq) {
	snd := v.snd
	run := v.a.currentRun()
	if run == nil || run.id != m.ScanID {
		snd.pushErrorWait(v.ctx, "unknown_scan", "no such scan")
		return
	}
	path, ok := run.scanner.PathOf(m.DirID)
	if !ok {
		snd.pushErrorWait(v.ctx, "unknown_dir", "unknown directory id")
		return
	}
	if !run.scanner.DirScanned(m.DirID) {
		// Announced with child flags (other device, bind mount, reparse point,
		// duplicate): never scanned, so its identity was never verified.
		snd.pushErrorWait(v.ctx, "not_scanned", "directory was not scanned")
		return
	}
	want, ok := run.scanner.DirIdentity(m.DirID)
	if !ok || want.IsZero() {
		snd.pushErrorWait(v.ctx, "unknown_dir", "unknown directory id")
		return
	}
	limit := m.Limit
	if limit <= 0 || limit > proto.MaxDirFilesLimit {
		limit = proto.MaxDirFilesLimit
	}
	for {
		res, err := scan.ListDirFilesExpect(path, want, limit)
		if errors.Is(err, scan.ErrChanged) {
			snd.pushErrorWait(v.ctx, errCodeChanged, "directory changed since it was scanned")
			return
		}
		if err != nil {
			snd.pushErrorWait(v.ctx, "dir_failed", errMessage(err))
			return
		}
		body := proto.EncodeDirFiles(m.ReqID, m.ScanID, m.DirID, res)
		if len(body) <= proto.MaxBody-1024 || limit <= 1 {
			snd.pushWait(v.ctx, body)
			return
		}
		limit /= 2
	}
}
