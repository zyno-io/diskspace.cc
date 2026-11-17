// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/zyno-io/diskspace.cc/internal/elevate"
	"github.com/zyno-io/diskspace.cc/internal/ipc"
)

func TestEmbeddedURL(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"http://127.0.0.1:5123/s/abcDEF123_-xyz#KKKK-_k", "http://127.0.0.1:5123/s/abcDEF123_-xyz?embedded=1#KKKK-_k"},
		{"https://127.0.0.1:5123/s/abc#k", ""},
		{"http://example.com/s/abc#k", ""},
		{"http://127.0.0.1:5123/other#k", ""},
		{"", ""},
	} {
		got, ok := embeddedURL(c.in)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("embeddedURL(%q) = %q,%v want %q", c.in, got, ok, c.want)
		}
	}
}

func TestLauncherFS(t *testing.T) {
	fsys := launcherFS(fstest.MapFS{
		"index.html":    {Data: []byte("landing")},
		"launcher.html": {Data: []byte("launcher")},
	})
	f, err := fsys.Open("index.html")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	if string(b) != "launcher" {
		t.Fatalf("index.html served %q", b)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "diskspace.cc", "gui.json")
	if c := loadConfig(p); c.Choice != "" {
		t.Fatalf("missing file: %+v", c)
	}
	if err := saveConfig(p, guiConfig{Choice: choiceAdmin}); err != nil {
		t.Fatal(err)
	}
	if c := loadConfig(p); c.Choice != choiceAdmin {
		t.Fatalf("got %+v", c)
	}
	if err := saveConfig(p, guiConfig{Choice: "root"}); err != nil {
		t.Fatal(err)
	}
	if c := loadConfig(p); c.Choice != "" {
		t.Fatalf("invalid choice kept: %+v", c)
	}
}

// fakeChild plays the agent side of IPC from the args the App passes.
type fakeChild struct {
	t      *testing.T
	args   chan []string
	conn   chan *ipc.Conn
	exited chan struct{}
	killed chan struct{}
	once   sync.Once
	err    error // returned by launch instead of starting
}

func newFakeChild(t *testing.T) *fakeChild {
	return &fakeChild{t: t, args: make(chan []string, 1), conn: make(chan *ipc.Conn, 1), exited: make(chan struct{}), killed: make(chan struct{})}
}

func argAfter(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func (f *fakeChild) launch(_ context.Context, _ string, args []string, _ bool) (handle, error) {
	if f.err != nil {
		return handle{}, f.err
	}
	f.args <- args
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := ipc.Dial(ctx, argAfter(args, "--ipc"), argAfter(args, "--ipc-token-file"))
		if err != nil {
			f.t.Error(err)
			return
		}
		f.conn <- c
	}()
	return handle{exited: f.exited, kill: func() error {
		f.once.Do(func() { close(f.killed); close(f.exited) })
		return nil
	}}, nil
}

func (f *fakeChild) exit() { f.once.Do(func() { close(f.exited) }) }

func newTestApp(t *testing.T, f *fakeChild) (*App, chan Snapshot) {
	ch := make(chan Snapshot, 64)
	a := &App{
		version: "test", relay: "https://relay.test", self: "/bin/self", home: "/home/u",
		cfgPath: filepath.Join(t.TempDir(), "gui.json"),
		spawn:   f.launch,
		snap:    Snapshot{Phase: phaseIdle, OS: "test"},
	}
	a.emitFn = func(s Snapshot) { ch <- s }
	return a, ch
}

func waitFor(t *testing.T, ch chan Snapshot, ok func(Snapshot) bool) Snapshot {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case s := <-ch:
			if ok(s) {
				return s
			}
		case <-deadline:
			t.Fatal("timed out waiting for snapshot")
		}
	}
}

func TestStartReadyRotateShutdown(t *testing.T) {
	f := newFakeChild(t)
	a, ch := newTestApp(t, f)
	if err := a.Start(false, true); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(false, false); err == nil {
		t.Error("second Start should fail while starting")
	}
	args := <-f.args
	for _, want := range [][]string{{"agent"}, {"--no-cwd"}, {"--home", "/home/u"}, {"--idle", "0s"}, {"--local"}, {"--no-elevate"}, {"--relay", "https://relay.test"}} {
		if i := slices.Index(args, want[0]); i < 0 || (len(want) == 2 && args[i+1] != want[1]) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	if args[0] != "agent" {
		t.Errorf("args must start with agent: %q", args)
	}
	child := <-f.conn
	defer child.Close()
	waitFor(t, ch, func(s Snapshot) bool { return s.Phase == phaseRunning })
	if s := a.State(); s.SavedChoice != choiceUser {
		t.Errorf("remembered choice = %q", s.SavedChoice)
	}

	_ = child.Send(ipc.Msg{T: ipc.TReady, URL: "https://relay.test/s/x#k", LocalURL: "http://127.0.0.1:9/s/x#k", FDA: "denied"})
	s := waitFor(t, ch, func(s Snapshot) bool { return s.URL != "" })
	if s.LocalURL == "" || s.FDA != "denied" {
		t.Errorf("ready not applied: %+v", s)
	}
	_ = child.Send(ipc.Msg{T: ipc.TViewer, State: "connected", Code: "123 456"})
	waitFor(t, ch, func(s Snapshot) bool { return s.Code == "123 456" })
	_ = child.Send(ipc.Msg{T: ipc.TScan, State: "started", Path: "/tmp/x"})
	waitFor(t, ch, func(s Snapshot) bool { return s.Scan == "started" && s.ScanPath == "/tmp/x" })

	if err := a.Rotate(); err != nil {
		t.Fatal(err)
	}
	if m, err := child.Recv(); err != nil || m.T != ipc.TRotate {
		t.Fatalf("rotate: %+v %v", m, err)
	}
	waitFor(t, ch, func(s Snapshot) bool { return s.URL == "" && s.Code == "" })

	// Shutdown: child gets "shutdown" and exits promptly; no kill needed.
	go func() {
		if m, err := child.Recv(); err == nil && m.T == ipc.TShutdown {
			child.Close()
			f.exit()
		}
	}()
	done := make(chan struct{})
	go func() { a.shutdown(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown hung")
	}
	select {
	case <-f.killed:
		t.Error("child was killed despite exiting on shutdown")
	default:
	}
}

func TestShutdownKillsStuckChild(t *testing.T) {
	f := newFakeChild(t)
	a, ch := newTestApp(t, f)
	_ = a.Start(false, false)
	child := <-f.conn
	defer child.Close()
	waitFor(t, ch, func(s Snapshot) bool { return s.Phase == phaseRunning })
	start := time.Now()
	a.shutdown(context.Background()) // child ignores shutdown
	select {
	case <-f.killed:
	default:
		t.Fatal("stuck child not killed")
	}
	if d := time.Since(start); d < stopGrace || d > stopGrace+time.Second {
		t.Errorf("grace %v", d)
	}
}

func TestCancelledPrompt(t *testing.T) {
	f := newFakeChild(t)
	f.err = elevate.ErrCancelled
	a, ch := newTestApp(t, f)
	_ = a.Start(true, false)
	s := waitFor(t, ch, func(s Snapshot) bool { return s.Phase != phaseStarting })
	if s.Phase != phaseCancelled || !s.Admin {
		t.Fatalf("%+v", s)
	}
	a.Reset()
	waitFor(t, ch, func(s Snapshot) bool { return s.Phase == phaseIdle })
	if err := a.Start(false, false); err != nil {
		t.Fatalf("start after cancel: %v", err)
	}
}

func TestLaunchError(t *testing.T) {
	f := newFakeChild(t)
	f.err = errors.New("boom")
	a, ch := newTestApp(t, f)
	_ = a.Start(false, false)
	s := waitFor(t, ch, func(s Snapshot) bool { return s.Phase == phaseError })
	if !strings.Contains(s.Message, "boom") {
		t.Fatalf("%+v", s)
	}
}

func TestChildDiesUnexpectedly(t *testing.T) {
	f := newFakeChild(t)
	a, ch := newTestApp(t, f)
	_ = a.Start(false, false)
	child := <-f.conn
	waitFor(t, ch, func(s Snapshot) bool { return s.Phase == phaseRunning })
	_ = child.Send(ipc.Msg{T: ipc.TError, Message: "relay refused"})
	child.Close()
	f.exit()
	s := waitFor(t, ch, func(s Snapshot) bool { return s.Phase == phaseError })
	if !strings.Contains(s.Message, "relay refused") {
		t.Fatalf("%+v", s)
	}
	a.mu.Lock()
	cur := a.cur
	a.mu.Unlock()
	if cur != nil {
		t.Error("dead child still current")
	}
}

func TestChildExitsBeforeDialing(t *testing.T) {
	a, ch := newTestApp(t, nil)
	ex := make(chan struct{})
	close(ex)
	a.spawn = func(context.Context, string, []string, bool) (handle, error) {
		return handle{exited: ex, kill: func() error { return nil }}, nil
	}
	_ = a.Start(false, false)
	s := waitFor(t, ch, func(s Snapshot) bool { return s.Phase == phaseError })
	if s.Message == "" {
		t.Fatal("no message")
	}
}

func TestRestartSwitchesMode(t *testing.T) {
	f1 := newFakeChild(t)
	a, ch := newTestApp(t, f1)
	_ = a.Start(false, true)
	c1 := <-f1.conn
	defer c1.Close()
	waitFor(t, ch, func(s Snapshot) bool { return s.Phase == phaseRunning })
	go func() {
		if m, err := c1.Recv(); err == nil && m.T == ipc.TShutdown {
			c1.Close()
			f1.exit()
		}
	}()
	f2 := newFakeChild(t)
	var gotAdmin bool
	a.spawn = func(ctx context.Context, self string, args []string, admin bool) (handle, error) {
		gotAdmin = admin
		return f2.launch(ctx, self, args, admin)
	}
	if err := a.Restart(true); err != nil {
		t.Fatal(err)
	}
	c2 := <-f2.conn
	defer c2.Close()
	s := waitFor(t, ch, func(s Snapshot) bool { return s.Phase == phaseRunning })
	if !gotAdmin || !s.Admin || s.SavedChoice != choiceAdmin {
		t.Fatalf("admin=%v snap=%+v", gotAdmin, s)
	}
	go func() { _, _ = c2.Recv(); c2.Close(); f2.exit() }()
	a.shutdown(context.Background())
}

// Review finding: a second Restart click while the first is stopping the old
// agent must be rejected, and exactly one new agent must end up current.
func TestDoubleRestartRejected(t *testing.T) {
	f1 := newFakeChild(t)
	a, ch := newTestApp(t, f1)
	_ = a.Start(false, false)
	c1 := <-f1.conn
	defer c1.Close()
	waitFor(t, ch, func(s Snapshot) bool { return s.Phase == phaseRunning })
	go func() { // old agent exits 300 ms after shutdown
		if m, err := c1.Recv(); err == nil && m.T == ipc.TShutdown {
			time.Sleep(300 * time.Millisecond)
			c1.Close()
			f1.exit()
		}
	}()
	var mu sync.Mutex
	launches := 0
	f2 := newFakeChild(t)
	a.spawn = func(ctx context.Context, self string, args []string, admin bool) (handle, error) {
		mu.Lock()
		launches++
		mu.Unlock()
		return f2.launch(ctx, self, args, admin)
	}
	errc := make(chan error, 1)
	go func() { errc <- a.Restart(true) }()
	waitFor(t, ch, func(s Snapshot) bool { return s.Phase == phaseStarting })
	if err := a.Restart(true); err == nil {
		t.Error("second Restart accepted while the first is in progress")
	}
	if err := a.Start(true, false); err == nil {
		t.Error("Start accepted while restarting")
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	c2 := <-f2.conn
	defer c2.Close()
	waitFor(t, ch, func(s Snapshot) bool { return s.Phase == phaseRunning })
	mu.Lock()
	if launches != 1 {
		t.Errorf("launches = %d", launches)
	}
	mu.Unlock()
	go func() { _, _ = c2.Recv(); c2.Close(); f2.exit() }()
	a.shutdown(context.Background())
}

// Review finding: quitting while the admin prompt is up cancels the spawn.
func TestShutdownCancelsPendingAdminPrompt(t *testing.T) {
	a, ch := newTestApp(t, nil)
	cancelled := make(chan struct{})
	a.spawn = func(ctx context.Context, _ string, _ []string, _ bool) (handle, error) {
		<-ctx.Done() // a password dialog nobody answers
		close(cancelled)
		return handle{}, ctx.Err()
	}
	_ = a.Start(true, false)
	waitFor(t, ch, func(s Snapshot) bool { return s.Phase == phaseStarting })
	a.shutdown(context.Background())
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("pending admin prompt not cancelled on shutdown")
	}
	if err := a.Start(false, false); err == nil {
		t.Error("Start after shutdown accepted")
	}
}

func TestRotateClearsBeforeNewLink(t *testing.T) {
	f := newFakeChild(t)
	a, ch := newTestApp(t, f)
	_ = a.Start(false, false)
	c := <-f.conn
	defer c.Close()
	_ = c.Send(ipc.Msg{T: ipc.TReady, URL: "https://r/s/a#k", LocalURL: "http://127.0.0.1:9/s/a#k"})
	waitFor(t, ch, func(s Snapshot) bool { return s.URL != "" })
	_ = a.Rotate()
	if m, err := c.Recv(); err != nil || m.T != ipc.TRotate {
		t.Fatal(m, err)
	}
	_ = c.Send(ipc.Msg{T: ipc.TReady, URL: "https://r/s/b#k", LocalURL: "http://127.0.0.1:9/s/b#k"})
	waitFor(t, ch, func(s Snapshot) bool { return s.URL == "https://r/s/b#k" })
	if s := a.State(); s.URL != "https://r/s/b#k" {
		t.Fatalf("new link lost: %q", s.URL)
	}
	go func() { _, _ = c.Recv(); c.Close(); f.exit() }()
	a.shutdown(context.Background())
}
