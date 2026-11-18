// SPDX-License-Identifier: MIT

// Package session is the agent side of a diskspace.cc session: it owns the
// link secret, the single active scan and its replay log, the relay control
// channel, the optional local HTTP server, and one encrypted connection per
// viewer (docs/protocol.md).
package session

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/zyno-io/diskspace.cc/internal/elevate"
	"github.com/zyno-io/diskspace.cc/internal/prio"
	"github.com/zyno-io/diskspace.cc/internal/proto"
	"github.com/zyno-io/diskspace.cc/internal/scan"
	"github.com/zyno-io/diskspace.cc/internal/session/crypto"
)

// EventKind classifies an Event.
type EventKind int

const (
	EvLinkReady EventKind = iota // URL/LocalURL are valid
	EvRelayState
	EvViewerConnected
	EvViewerDisconnected
	EvViewerSuperseded
	EvScanStarted
	EvScanDone
	EvScanCanceled
	EvScanError
	EvRotated
	EvIdle
	EvError
	EvScanProgress  // Path, Totals{Files,Dirs,Alloc} running counters (~4 Hz)
	EvScanPaused    // Path, Totals, Ms (elapsed excluding paused time)
	EvScanResumed   // Path, Totals, Ms
	EvDeleteStarted // Message: an approved deletion started
	EvDeleteItem    // Path, Message: one top-level item's outcome (audit line)
	EvDeleteDone    // Message: summary of a deletion that ran
)

// Event reports session activity to the CLI / IPC layer. Events are delivered
// serially; the callback must not block.
type Event struct {
	Kind     EventKind
	URL      string // EvLinkReady: relay link (empty without relay)
	LocalURL string // EvLinkReady: 127.0.0.1 link (empty unless local mode)
	Code     string // viewer verification code "123 456"
	Path     string // scan root
	Totals   *proto.ScanTotals
	Ms       int64
	State    string // EvRelayState: connecting | connected | reconnecting
	Err      error
	Message  string
}

// Config configures an Agent.
type Config struct {
	RelayURL  string // https base of the relay; empty = no relay (local only)
	Local     bool   // also serve on 127.0.0.1 (implied when RelayURL is empty)
	Root      string // restrict scanning/browsing to this subtree ("" = anywhere)
	Cwd       *string
	Home      string
	AutoScan  string
	Elevation string // none | sudo | uac | gui-admin | native
	Launcher  string // cli | app; empty means cli
	FDA       string // granted | denied | "" (unknown / not macOS)
	Version   string
	Threads   int
	Priority  prio.Level    // scanner CPU/IO priority (zero = normal)
	Idle      time.Duration // exit Run after this long with no viewer; 0 disables
	LogCap    int64         // scan log cap in compressed bytes (default 256 MiB)
	Logger    *slog.Logger
	Events    func(Event)

	// Confirm asks the person at this computer to approve a deletion through
	// a channel web content can't drive (terminal prompt, native dialog). It
	// returns (false, nil) when declined, ErrConfirmTimeout when unanswered
	// and ctx.Err() when ctx ends (request withdrawn, 2-minute limit). nil:
	// deletion is unavailable ("no_confirm"). It may be called from any
	// goroutine, one request at a time.
	Confirm func(ctx context.Context, req ConfirmRequest) (bool, error)
	// ConfirmKind is where Confirm asks: terminal | app | console.
	ConfirmKind string
	// SID is the real user's SID (Windows), whose Recycle Bin is used ("" =
	// this process's user).
	SID string
}

// goos is runtime.GOOS (a var for tests of platform wording).
var goos = runtime.GOOS

type generation struct {
	sec    *crypto.Secret
	tok    []byte
	tokB64 string
	ctx    context.Context
	cancel context.CancelFunc
}

// Agent is one diskspace.cc agent session.
type Agent struct {
	cfg   Config
	log   *slog.Logger
	guard *scan.RootGuard

	life       context.Context
	lifeCancel context.CancelFunc

	emitMu sync.Mutex

	mu          sync.Mutex
	gen         *generation
	conns       map[*viewerConn]struct{} // every live connection (pre- and post-handshake)
	viewers     map[*viewerConn]struct{} // activated
	active      *viewerConn
	cur         *scanRun
	scanSeq     uint64
	autoPending bool
	lastViewer  time.Time
	localAddr   string // 127.0.0.1:port once listening

	scanMu sync.Mutex // serializes scan start/cancel

	hsMu sync.Mutex
	hs   []time.Time

	// deletion (a.mu; the job slot is changed under scanMu too)
	del         *delJob // preparing, awaiting or running
	delSeq      uint64
	delStrikes  int       // refused prompts this run
	delCooldown time.Time // no new request before this
	delWG       sync.WaitGroup
}

// NewAgent validates cfg and creates the link secret.
func NewAgent(cfg Config) (*Agent, error) {
	a := &Agent{
		cfg:         cfg,
		log:         cfg.Logger,
		conns:       map[*viewerConn]struct{}{},
		viewers:     map[*viewerConn]struct{}{},
		autoPending: cfg.AutoScan != "",
		lastViewer:  time.Now(),
	}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	if cfg.Root != "" {
		g, err := scan.NewRootGuard(cfg.Root)
		if err != nil {
			return nil, err
		}
		a.guard = g
	}
	a.life, a.lifeCancel = context.WithCancel(context.Background())
	g, err := a.newGeneration()
	if err != nil {
		return nil, err
	}
	a.gen = g
	return a, nil
}

func (a *Agent) newGeneration() (*generation, error) {
	sec, err := crypto.NewSecret()
	if err != nil {
		return nil, err
	}
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return nil, err
	}
	g := &generation{sec: sec, tok: tok, tokB64: base64.RawURLEncoding.EncodeToString(tok)}
	g.ctx, g.cancel = context.WithCancel(a.life)
	return g, nil
}

func (a *Agent) curGen() *generation {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gen
}

func (a *Agent) emit(e Event) {
	if a.cfg.Events == nil {
		return
	}
	a.emitMu.Lock()
	defer a.emitMu.Unlock()
	a.cfg.Events(e)
}

func (a *Agent) relayBase() string { return strings.TrimRight(a.cfg.RelayURL, "/") }

func linkFor(base string, g *generation) string {
	return base + "/s/" + g.sec.SIDString() + "#" + g.sec.KString()
}

// URL is the secure relay link ("" without a relay).
func (a *Agent) URL() string {
	if a.cfg.RelayURL == "" {
		return ""
	}
	return linkFor(a.relayBase(), a.curGen())
}

// LocalURL is the 127.0.0.1 link ("" until the local server is listening).
func (a *Agent) LocalURL() string {
	a.mu.Lock()
	addr, g := a.localAddr, a.gen
	a.mu.Unlock()
	if addr == "" {
		return ""
	}
	return linkFor("http://"+addr, g)
}

func (a *Agent) localEnabled() bool { return a.cfg.Local || a.cfg.RelayURL == "" }

// Rotate replaces sid, K and the relay token. Connected viewers are dropped.
func (a *Agent) Rotate() {
	ng, err := a.newGeneration()
	if err != nil {
		a.emit(Event{Kind: EvError, Err: err})
		return
	}
	a.mu.Lock()
	old := a.gen
	a.gen = ng
	var cs []*viewerConn
	for c := range a.conns {
		cs = append(cs, c)
	}
	a.mu.Unlock()
	old.cancel()
	for _, c := range cs {
		c.kill()
	}
	a.emit(Event{Kind: EvRotated})
	if a.cfg.RelayURL == "" {
		a.emit(Event{Kind: EvLinkReady, LocalURL: a.LocalURL()})
	}
}

// hsAllow enforces <=20 handshake attempts per minute per agent.
func (a *Agent) hsAllow() bool {
	a.hsMu.Lock()
	defer a.hsMu.Unlock()
	now := time.Now()
	cut := now.Add(-time.Minute)
	i := 0
	for i < len(a.hs) && a.hs[i].Before(cut) {
		i++
	}
	a.hs = a.hs[i:]
	if len(a.hs) >= 20 {
		return false
	}
	a.hs = append(a.hs, now)
	return true
}

// Run serves until ctx is done (returns nil) or the idle timeout elapses.
func (a *Agent) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	var srv *http.Server
	if a.localEnabled() {
		var err error
		if srv, err = a.startLocal(); err != nil {
			return err
		}
	}
	if a.cfg.RelayURL != "" {
		wg.Add(1)
		go func() { defer wg.Done(); a.relayLoop() }()
		// With a relay the link is announced once it registers.
	} else {
		a.emit(Event{Kind: EvLinkReady, LocalURL: a.LocalURL()})
	}

	a.mu.Lock()
	a.lastViewer = time.Now()
	a.mu.Unlock()
	var tickC <-chan time.Time
	if a.cfg.Idle > 0 {
		d := min(max(a.cfg.Idle/10, 20*time.Millisecond), time.Second)
		t := time.NewTicker(d)
		defer t.Stop()
		tickC = t.C
	}
	var result error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-tickC:
			a.mu.Lock()
			idle := len(a.viewers) == 0 && time.Since(a.lastViewer) >= a.cfg.Idle
			a.mu.Unlock()
			if idle {
				a.emit(Event{Kind: EvIdle})
				break loop
			}
		}
	}
	a.shutdown(srv)
	a.lifeCancel()
	a.waitDeletes(5 * time.Second) // an approved deletion stops after the current entry
	wg.Wait()
	return result
}

func (a *Agent) shutdown(srv *http.Server) {
	a.mu.Lock()
	var cs []*viewerConn
	for c := range a.conns {
		cs = append(cs, c)
	}
	a.mu.Unlock()
	for _, c := range cs {
		c.stop(proto.ByeShutdown)
	}
	deadline := time.After(250 * time.Millisecond)
	for _, c := range cs {
		select {
		case <-c.done:
		case <-deadline:
		}
	}
	for _, c := range cs {
		c.kill()
	}
	a.scanMu.Lock()
	if r := a.currentRun(); r != nil {
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(250 * time.Millisecond):
		}
	}
	a.scanMu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_ = srv.Shutdown(ctx)
		cancel()
		_ = srv.Close()
	}
}

func (a *Agent) currentRun() *scanRun {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cur
}

// ---- AGENT_INFO ----

func (a *Agent) volumes() []proto.Volume {
	vs, _ := scan.Volumes()
	if a.guard == nil {
		return vs
	}
	out := vs[:0:0]
	for _, v := range vs {
		if a.guard.Within(v.Path) {
			out = append(out, v)
		}
	}
	return out
}

func (a *Agent) buildInfo() proto.AgentInfo {
	host, _ := os.Hostname()
	home := a.cfg.Home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	info := proto.AgentInfo{
		Proto:        proto.ProtoVersion,
		Version:      a.cfg.Version,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		Hostname:     host,
		User:         elevate.CurrentUser(),
		InvokingUser: elevate.InvokingUser(),
		Elevated:     elevate.IsElevated(),
		Elevation:    a.cfg.Elevation,
		Launcher:     a.cfg.Launcher,
		Home:         home,
		Volumes:      a.volumes(),
		Features:     proto.Features{Pause: true, Delete: a.deleteFeature()},
	}
	if info.Launcher == "" {
		info.Launcher = "cli"
	}
	if info.Elevation == "" {
		info.Elevation = "none"
		if info.Elevated {
			info.Elevation = "native"
		}
	}
	if a.cfg.FDA != "" {
		f := a.cfg.FDA
		info.FDA = &f
	}
	if a.cfg.Root != "" {
		r := a.guard.Root()
		info.Root = &r
	}
	if a.cfg.Cwd != nil && a.guard.Within(*a.cfg.Cwd) {
		info.Cwd = &proto.Cwd{Path: *a.cfg.Cwd}
	}
	if a.cfg.AutoScan != "" {
		s := a.cfg.AutoScan
		info.AutoScan = &s
	}
	return info
}

// ---- scans ----

// scanRun is one scan: scanner + replay log.
type scanRun struct {
	a           *Agent
	id          uint64
	path        string
	scanner     *scan.Scanner
	log         *scanLog
	startedBody []byte
	cancel      context.CancelFunc
	ctx         context.Context
	done        chan struct{} // closed after the log is finished
	started     time.Time

	tickMu      sync.Mutex // serializes the progress ticker with pause/resume
	mu          sync.Mutex
	state       string // running | done | canceled | error
	marks       dirMarks
	paused      bool
	pausedAt    time.Time
	pausedTotal time.Duration // completed pauses
}

// elapsedLocked is the scan's running time excluding paused time.
func (r *scanRun) elapsedLocked(now time.Time) time.Duration {
	d := now.Sub(r.started) - r.pausedTotal
	if r.paused {
		d -= now.Sub(r.pausedAt)
	}
	return max(d, 0)
}

func (r *scanRun) elapsedMs() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.elapsedLocked(time.Now()).Milliseconds()
}

// live reports whether the scan is still running and not being canceled.
func (r *scanRun) live() bool {
	r.mu.Lock()
	st := r.state
	r.mu.Unlock()
	return st == "running" && (r.ctx == nil || r.ctx.Err() == nil)
}

// pauseState reports whether a running scan is paused, and its elapsed ms.
func (r *scanRun) pauseState() (paused bool, elapsedMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused && r.state == "running", r.elapsedLocked(time.Now()).Milliseconds()
}

// setPause applies the state to a running scan and reports whether it changed.
func (r *scanRun) setPause(p bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != "running" || (r.ctx != nil && r.ctx.Err() != nil) || r.paused == p {
		return false
	}
	now := time.Now()
	if p {
		r.paused, r.pausedAt = true, now
		r.scanner.Pause()
	} else {
		r.pausedTotal += now.Sub(r.pausedAt)
		r.paused = false
		r.scanner.Resume()
	}
	return true
}

func (r *scanRun) setState(s string) {
	r.mu.Lock()
	r.state = s
	r.mu.Unlock()
}

func (r *scanRun) getState() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func rootName(p string) string {
	b := filepath.Base(p)
	if b == "." || b == string(filepath.Separator) || b == "" {
		return p
	}
	return b
}

func (a *Agent) volumeFor(path string) *proto.VolumeUsage {
	vs, _ := scan.Volumes()
	for _, v := range vs {
		if filepath.Clean(v.Path) == filepath.Clean(path) {
			return &proto.VolumeUsage{Total: v.Total, Free: v.Free, Used: v.Used}
		}
	}
	return nil
}

// hookBeforeRun is a test hook called with each new scan before it is attached
// or started (tests use it to start a scan paused).
var hookBeforeRun func(*scanRun)

// startScan cancels and drains any running scan, then starts a new one and
// attaches it to the active viewer.
func (a *Agent) startScan(opts scan.Options) (*scanRun, error) {
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	a.mu.Lock()
	deleting := a.del != nil
	a.mu.Unlock()
	if deleting {
		return nil, errDeleting
	}
	if opts.Threads == 0 {
		opts.Threads = a.cfg.Threads
	}
	if opts.Priority == prio.Normal {
		opts.Priority = a.cfg.Priority
	}
	if a.cfg.Root != "" {
		opts.CrossMounts = false
	}
	sc, err := scan.New(opts)
	if err != nil {
		return nil, err
	}
	if prev := a.currentRun(); prev != nil {
		prev.cancel()
		<-prev.done
	}
	a.mu.Lock()
	a.scanSeq++
	id := a.scanSeq
	a.mu.Unlock()

	path := filepath.Clean(opts.Root)
	started, _ := proto.EncodeMsg(proto.MsgScanStarted, proto.ScanStarted{
		ScanID:   id,
		Path:     path,
		RootName: rootName(path),
		Volume:   a.volumeFor(path),
		Opts: proto.ScanOptions{
			CrossMounts: opts.CrossMounts, TopFiles: opts.TopFiles, MinFileBytes: opts.MinFileBytes,
		},
	})
	r := &scanRun{
		a: a, id: id, path: path, scanner: sc,
		log:         newScanLog(id, a.cfg.LogCap),
		startedBody: started,
		done:        make(chan struct{}),
		started:     time.Now(),
		state:       "running",
	}
	r.ctx, r.cancel = context.WithCancel(a.life)
	if hookBeforeRun != nil {
		hookBeforeRun(r)
	}

	a.mu.Lock()
	a.cur = r
	act := a.active
	if act != nil && act.snd != nil {
		act.snd.attach(r, false)
	}
	a.mu.Unlock()
	a.emit(Event{Kind: EvScanStarted, Path: path})
	go r.run()
	return r, nil
}

func (r *scanRun) progressBody() []byte {
	st := r.scanner.Progress()
	b, _ := proto.EncodeMsg(proto.MsgProgress, proto.Progress{
		ScanID: r.id, Dirs: st.Dirs, Files: st.Files, Alloc: st.Alloc, Apparent: st.Apparent,
		Errors: st.Errors + st.Denied, ElapsedMs: r.elapsedMs(), CurrentPath: st.CurrentPath,
	})
	return b
}

func (r *scanRun) run() {
	a := r.a
	defer close(r.done)
	recs := make(chan proto.Record, 8192)
	batchDone := make(chan struct{})
	var tooBig bool
	var tbMu sync.Mutex
	go func() {
		defer close(batchDone)
		r.log.runBatcher(recs, func() {
			tbMu.Lock()
			tooBig = true
			tbMu.Unlock()
			r.cancel()
		})
	}()

	stopTick := make(chan struct{})
	tickDone := make(chan struct{})
	go func() {
		defer close(tickDone)
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				// tickMu: a pause that lands now waits, so no progress event
				// follows EvScanPaused
				r.tickMu.Lock()
				if paused, _ := r.pauseState(); !paused {
					a.pushProgress(r, r.progressBody())
					if a.cfg.Events != nil {
						p := r.scanner.Progress()
						a.emit(Event{Kind: EvScanProgress, Path: r.path, Ms: r.elapsedMs(),
							Totals: &proto.ScanTotals{Alloc: p.Alloc, Apparent: p.Apparent, Files: p.Files, Dirs: p.Dirs}})
					}
				}
				r.tickMu.Unlock()
			case <-stopTick:
				return
			}
		}
	}()

	st, err := r.scanner.Run(r.ctx, func(rec proto.Record) {
		r.marks.note(rec)
		recs <- rec
	})
	close(stopTick)
	<-tickDone
	close(recs)
	<-batchDone

	tbMu.Lock()
	big := tooBig
	tbMu.Unlock()
	ms := r.elapsedMs()
	switch {
	case err == nil && !big:
		body, _ := proto.EncodeMsg(proto.MsgScanDone, proto.ScanDone{
			ScanID:           r.id,
			Totals:           proto.ScanTotals{Alloc: st.Alloc, Apparent: st.Apparent, Files: st.Files, Dirs: st.Dirs},
			Denied:           st.Denied,
			Errors:           st.Errors,
			Vanished:         st.Vanished,
			SkippedMounts:    st.SkippedMounts,
			HardlinksDeduped: st.HardlinksDeduped,
			Folded:           st.Folded,
			Ms:               ms,
		})
		r.setState("done")
		r.log.finish(logEnd{Body: body, Done: true})
		a.emit(Event{Kind: EvScanDone, Path: r.path, Ms: ms,
			Totals: &proto.ScanTotals{Alloc: st.Alloc, Apparent: st.Apparent, Files: st.Files, Dirs: st.Dirs}})
	case big:
		r.setState("error")
		body, _ := proto.EncodeMsg(proto.MsgScanEnded, proto.ScanEnded{ScanID: r.id, Reason: "error", Message: errBodyTooLarge})
		r.log.finish(logEnd{Body: body})
		a.emit(Event{Kind: EvScanError, Path: r.path, Message: errBodyTooLarge})
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		r.setState("canceled")
		body, _ := proto.EncodeMsg(proto.MsgScanEnded, proto.ScanEnded{ScanID: r.id, Reason: "canceled"})
		r.log.finish(logEnd{Body: body})
		a.emit(Event{Kind: EvScanCanceled, Path: r.path})
	default:
		r.setState("error")
		body, _ := proto.EncodeMsg(proto.MsgScanEnded, proto.ScanEnded{ScanID: r.id, Reason: "error", Message: err.Error()})
		r.log.finish(logEnd{Body: body})
		a.emit(Event{Kind: EvScanError, Path: r.path, Err: err, Message: err.Error()})
	}
}

func (a *Agent) pushProgress(r *scanRun, body []byte) {
	a.mu.Lock()
	act := a.active
	a.mu.Unlock()
	if act != nil && act.snd != nil {
		act.snd.setProgress(r.log, body)
	}
}

// cancelScan cancels the running scan with the given id.
func (a *Agent) cancelScan(id uint64) {
	if r := a.currentRun(); r != nil && r.id == id {
		r.cancel()
	}
}

// setPaused pauses or resumes the scan with the given id (idempotent). A
// request for a scan that is not the current running one changes nothing. The
// active viewer is always told the resulting state.
// setPaused applies a viewer's SCAN_PAUSE. Only the active viewer may steer
// the scan: a superseded connection (BYE queued, still draining) is ignored.
// The check runs under scanMu so it can't interleave with the new viewer's
// activate → resumeIfPaused.
func (a *Agent) setPaused(from *viewerConn, id uint64, paused bool) {
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	a.mu.Lock()
	active := a.active == from
	a.mu.Unlock()
	if !active {
		return
	}
	r := a.currentRun()
	if r == nil {
		return
	}
	if r.id != id { // stale request: change nothing, report the current state
		a.sendScanState(r)
		return
	}
	r.tickMu.Lock()
	defer r.tickMu.Unlock()
	changed := r.setPause(paused)
	a.sendScanState(r)
	if changed {
		a.emitPause(r, paused)
	}
}

// resumeIfPaused resumes r if it is paused (an attaching viewer cannot resume it).
func (a *Agent) resumeIfPaused(r *scanRun) {
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	if a.currentRun() != r {
		return
	}
	r.tickMu.Lock()
	defer r.tickMu.Unlock()
	if r.setPause(false) {
		a.sendScanState(r)
		a.emitPause(r, false)
	}
}

func (a *Agent) emitPause(r *scanRun, paused bool) {
	kind := EvScanResumed
	if paused {
		kind = EvScanPaused
	}
	p := r.scanner.Progress()
	a.emit(Event{Kind: kind, Path: r.path, Ms: r.elapsedMs(),
		Totals: &proto.ScanTotals{Alloc: p.Alloc, Apparent: p.Apparent, Files: p.Files, Dirs: p.Dirs}})
}

// sendScanState queues SCAN_STATE for the active viewer if it understands it.
func (a *Agent) sendScanState(r *scanRun) {
	if !r.live() {
		return // an ended or ending scan has no pause state; never answer "not paused" for it
	}
	paused, ms := r.pauseState()
	a.mu.Lock()
	defer a.mu.Unlock()
	if act := a.active; act != nil && act.snd != nil && act.snd.scanState {
		act.snd.pushJSON(proto.MsgScanState, proto.ScanState{ScanID: r.id, Paused: paused, ElapsedMs: ms})
	}
}

// ---- local server listener helpers ----

func (a *Agent) setLocalAddr(l net.Listener) {
	a.mu.Lock()
	a.localAddr = l.Addr().String()
	a.mu.Unlock()
}
