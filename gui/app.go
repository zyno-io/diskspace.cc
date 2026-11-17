// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/zyno-io/diskspace.cc/internal/app"
	"github.com/zyno-io/diskspace.cc/internal/browser"
	"github.com/zyno-io/diskspace.cc/internal/elevate"
)

// Launcher phases.
const (
	phaseIdle      = "idle"      // choice screen
	phaseStarting  = "starting"  // spawning the agent / waiting for the OS admin prompt
	phaseRunning   = "running"   // agent connected over IPC (URL empty until it has a link)
	phaseCancelled = "cancelled" // the admin prompt was declined
	phaseError     = "error"     // the agent failed to start or stopped unexpectedly
)

// Snapshot is the launcher's whole view of the app. State() returns it and
// every change is pushed to the page as the "agent" event. Field names match
// web/src/launcher/bridge.ts.
type Snapshot struct {
	Seq         uint64 `json:"seq"`
	Phase       string `json:"phase"`
	Admin       bool   `json:"admin"` // admin was requested for the current / last start
	OS          string `json:"os"`
	Version     string `json:"version"`
	User        string `json:"user"`
	GUIElevated bool   `json:"guiElevated"` // the app itself runs as administrator
	Elevated    bool   `json:"elevated"`    // the running agent reported administrator rights
	FDA         string `json:"fda"`         // granted | denied | "" (macOS Full Disk Access)
	SavedChoice string `json:"savedChoice"` // "" | admin | user
	URL         string `json:"url"`
	LocalURL    string `json:"localUrl"`
	Viewer      string `json:"viewer"` // "" | connected | disconnected | superseded
	Code        string `json:"code"`
	Scan        string `json:"scan"` // "" | started | paused | done | canceled
	ScanPath    string `json:"scanPath"`
	Notice      string `json:"notice"`  // last non-fatal agent error
	Message     string `json:"message"` // phaseError detail
	Expanded    bool   `json:"expanded"`
}

// App is bound to the launcher page as window.go.main.App.
type App struct {
	version string
	relay   string
	self    string
	home    string
	cfgPath string
	spawn   launchFunc
	emitFn  func(Snapshot)

	guiElevated bool

	emitMu sync.Mutex // serialises emits so snapshots arrive in order

	mu           sync.Mutex
	ctx          context.Context
	snap         Snapshot
	cur          *proc
	gen          int
	cancelLaunch context.CancelFunc
	quitting     bool
	prevW, prevH int
}

func newApp(version, defaultRelay string) (*App, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if p, err := filepath.EvalSymlinks(self); err == nil {
		self = p
	}
	relay := defaultRelay
	if relay == "" {
		relay = app.FallbackRelay
	}
	home, _ := os.UserHomeDir()
	cfgPath, _ := configPath()
	a := &App{
		version: version,
		relay:   relay,
		self:    self,
		home:    home,
		cfgPath: cfgPath,
		spawn:   defaultLaunch,

		guiElevated: elevate.IsElevated(),
	}
	a.emitFn = a.wailsEmit
	a.snap = Snapshot{
		Phase:       phaseIdle,
		OS:          goruntime.GOOS,
		Version:     version,
		User:        elevate.CurrentUser(),
		GUIElevated: a.guiElevated,
		FDA:         string(elevate.ProbeFDA()),
	}
	if cfgPath != "" {
		a.snap.SavedChoice = loadConfig(cfgPath).Choice
	}
	return a, nil
}

func (a *App) startup(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()
}

// shutdown runs when the window closes or the app quits: stop the agent
// (IPC shutdown, ≤2 s grace, then kill) so no child outlives the app.
func (a *App) shutdown(context.Context) {
	a.mu.Lock()
	a.quitting = true
	p := a.cur
	a.cur = nil
	if a.cancelLaunch != nil {
		a.cancelLaunch()
	}
	a.mu.Unlock()
	if p != nil {
		p.stop(stopGrace)
	}
	browser.Cleanup()
}

func (a *App) wailsEmit(s Snapshot) {
	a.mu.Lock()
	ctx := a.ctx
	a.mu.Unlock()
	if ctx != nil {
		wruntime.EventsEmit(ctx, "agent", s)
	}
}

// update mutates the snapshot under the lock and pushes the result.
func (a *App) update(fn func(s *Snapshot)) { a.updateWhen(nil, fn) }

// updateWhen applies fn only if cond (evaluated under a.mu) holds, so a
// stale generation can never overwrite a newer one. It reports whether fn ran.
func (a *App) updateWhen(cond func() bool, fn func(s *Snapshot)) bool {
	a.emitMu.Lock()
	defer a.emitMu.Unlock()
	a.mu.Lock()
	if cond != nil && !cond() {
		a.mu.Unlock()
		return false
	}
	fn(&a.snap)
	a.snap.Seq++
	s := a.snap
	a.mu.Unlock()
	a.emitFn(s)
	return true
}

// ---- bound methods (window.go.main.App.*) ----------------------------------

// State returns the current snapshot.
func (a *App) State() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur == nil {
		a.snap.FDA = string(elevate.ProbeFDA())
	}
	return a.snap
}

var errBusy = errors.New("diskspace is already running")

// Start launches the agent, as administrator when admin is set (and the app
// isn't already elevated). remember stores the choice for the next launch.
// It returns immediately; progress arrives as "agent" events.
func (a *App) Start(admin, remember bool) error {
	return a.launch(admin, remember, func() error {
		if a.cur != nil || a.snap.Phase == phaseStarting || a.snap.Phase == phaseRunning {
			return errBusy
		}
		return nil
	})
}

// launch atomically checks allow (under a.mu), opens a new generation and
// moves to phaseStarting, then spawns the agent in the background.
func (a *App) launch(admin, remember bool, allow func() error) error {
	var (
		gen  int
		ctx  context.Context
		werr error
	)
	ok := a.updateWhen(func() bool {
		if a.quitting {
			werr = errors.New("quitting")
			return false
		}
		if werr = allow(); werr != nil {
			return false
		}
		a.gen++
		gen = a.gen
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(context.Background())
		a.cancelLaunch = cancel
		return true
	}, func(s *Snapshot) {
		resetRun(s)
		s.Phase = phaseStarting
		s.Admin = admin
	})
	if !ok {
		return werr
	}
	if remember {
		choice := choiceUser
		if admin {
			choice = choiceAdmin
		}
		if saveConfig(a.cfgPath, guiConfig{Choice: choice}) == nil {
			a.update(func(s *Snapshot) { s.SavedChoice = choice })
		}
	}
	go a.run(ctx, gen, admin && !a.guiElevated)
	return nil
}

// Restart stops the running agent and starts a new one with or without
// administrator rights. A remembered choice follows the new mode. Only valid
// while an agent is running; the UI switches to "starting" immediately, so a
// second click is rejected.
func (a *App) Restart(admin bool) error {
	var (
		p     *proc
		gen   int
		saved string
		werr  error
	)
	ok := a.updateWhen(func() bool {
		if a.quitting || a.cur == nil || a.snap.Phase != phaseRunning {
			werr = errors.New("diskspace isn't running")
			return false
		}
		p, a.cur = a.cur, nil
		if a.cancelLaunch != nil {
			a.cancelLaunch()
		}
		a.gen++ // the old run's late updates are now ignored
		gen = a.gen
		saved = a.snap.SavedChoice
		return true
	}, func(s *Snapshot) {
		resetRun(s)
		s.Phase = phaseStarting
		s.Admin = admin
	})
	if !ok {
		return werr
	}
	p.stop(stopGrace)
	return a.launch(admin, saved != "", func() error {
		if a.gen != gen { // quit, or something else started meanwhile
			return errBusy
		}
		return nil
	})
}

// Reset returns to the choice screen after an error or a declined prompt.
func (a *App) Reset() {
	a.updateWhen(func() bool {
		return a.cur == nil && (a.snap.Phase == phaseCancelled || a.snap.Phase == phaseError)
	}, func(s *Snapshot) { resetRun(s); s.Phase = phaseIdle })
}

// Forget clears the remembered choice.
func (a *App) Forget() error {
	if a.cfgPath != "" {
		if err := os.Remove(a.cfgPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	a.update(func(s *Snapshot) { s.SavedChoice = "" })
	return nil
}

// Rotate asks the agent for a new link; the old one stops working.
func (a *App) Rotate() error {
	a.mu.Lock()
	p := a.cur
	a.mu.Unlock()
	if p == nil {
		return errors.New("diskspace isn't running")
	}
	// Clear first: the new link's "ready" must not be wiped by this update.
	a.updateWhen(func() bool { return a.cur == p }, func(s *Snapshot) {
		s.URL, s.LocalURL = "", ""
		s.Viewer, s.Code = "", ""
	})
	return p.rotate()
}

// OpenInBrowser opens the secure link in the default browser. It goes
// through internal/browser (a private redirect file), never argv: the link's
// #key must not be visible to other local users. Never log the link.
func (a *App) OpenInBrowser(link string) error {
	if !strings.HasPrefix(link, "https://") && !strings.HasPrefix(link, "http://") {
		return errors.New("not a web link")
	}
	return browser.Open(link)
}

// OpenHere enlarges the window for the viewer and returns the local viewer
// link (with embedded=1) for the page to navigate to.
func (a *App) OpenHere() (string, error) {
	a.mu.Lock()
	local, ctx, expanded := a.snap.LocalURL, a.ctx, a.snap.Expanded
	a.mu.Unlock()
	u, ok := embeddedURL(local)
	if !ok {
		return "", errors.New("the in-app viewer isn't available yet")
	}
	if ctx != nil && !expanded {
		w, h := wruntime.WindowGetSize(ctx)
		tw, th := 1280, 840
		if screens, err := wruntime.ScreenGetAll(ctx); err == nil {
			for _, sc := range screens {
				if sc.IsCurrent && sc.Size.Width > 0 {
					tw = min(tw, sc.Size.Width-80)
					th = min(th, sc.Size.Height-80)
				}
			}
		}
		if tw > w || th > h {
			a.mu.Lock()
			a.prevW, a.prevH = w, h
			a.mu.Unlock()
			wruntime.WindowSetSize(ctx, max(tw, w), max(th, h))
			wruntime.WindowCenter(ctx)
			a.update(func(s *Snapshot) { s.Expanded = true })
		}
	}
	return u, nil
}

// LauncherShown restores the launcher's window size after the in-app viewer.
func (a *App) LauncherShown() {
	a.mu.Lock()
	ctx, expanded, w, h := a.ctx, a.snap.Expanded, a.prevW, a.prevH
	a.mu.Unlock()
	if !expanded || ctx == nil {
		return
	}
	if w <= 0 || h <= 0 {
		w, h = launcherW, launcherH
	}
	wruntime.WindowSetSize(ctx, w, h)
	wruntime.WindowCenter(ctx)
	a.update(func(s *Snapshot) { s.Expanded = false })
}

// OpenFDASettings opens System Settings › Privacy & Security › Full Disk Access.
func (a *App) OpenFDASettings() error { return elevate.OpenFDASettings() }

// Copy puts text on the clipboard.
func (a *App) Copy(text string) error {
	a.mu.Lock()
	ctx := a.ctx
	a.mu.Unlock()
	if ctx == nil {
		return errors.New("window not ready")
	}
	return wruntime.ClipboardSetText(ctx, text)
}

// Quit closes the app (the agent is stopped in shutdown).
func (a *App) Quit() {
	a.mu.Lock()
	ctx := a.ctx
	a.mu.Unlock()
	if ctx != nil {
		wruntime.Quit(ctx)
	}
}

func resetRun(s *Snapshot) {
	s.Elevated = false
	s.URL, s.LocalURL = "", ""
	s.Viewer, s.Code = "", ""
	s.Scan, s.ScanPath = "", ""
	s.Notice, s.Message = "", ""
}
