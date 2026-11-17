// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zyno-io/diskspace.cc/internal/app"
	"github.com/zyno-io/diskspace.cc/internal/elevate"
	"github.com/zyno-io/diskspace.cc/internal/ipc"
)

const (
	// acceptTimeout bounds the wait for the child to dial back after it was
	// started (the OS admin prompt has already been answered by then).
	acceptTimeout = 45 * time.Second
	stopGrace     = 2 * time.Second
)

// handle is a started child. exited is closed when the process is known to
// have exited; it is nil (and kill too) for the macOS admin child, which runs
// as root under osascript and can't be waited on or signalled by us. That
// child still exits within a second of the IPC connection closing, and polls
// --parent-pid.
type handle struct {
	exited <-chan struct{}
	kill   func() error
}

type launchFunc func(ctx context.Context, self string, args []string, admin bool) (handle, error)

// defaultLaunch starts `self agent …`:
//   - macOS admin: osascript "with administrator privileges" (args via argv);
//   - Windows admin: ShellExecuteExW runas, hidden (only the agent is
//     elevated; the window stays a normal process);
//   - otherwise (or when the app is already elevated): a plain child process.
func defaultLaunch(ctx context.Context, self string, args []string, admin bool) (handle, error) {
	if admin {
		switch goruntime.GOOS {
		case "darwin":
			return handle{}, spawnAdminDarwin(ctx, self, args)
		case "windows":
			c, err := elevate.RunAsAdmin(self, args, false)
			if err != nil {
				return handle{}, err
			}
			ex := make(chan struct{})
			go func() { _, _ = c.Wait(); close(ex) }()
			return handle{exited: ex, kill: c.Kill}, nil
		}
	}
	cmd := exec.Command(self, args...)
	if err := cmd.Start(); err != nil {
		return handle{}, err
	}
	ex := make(chan struct{})
	go func() { _ = cmd.Wait(); close(ex) }()
	return handle{exited: ex, kill: cmd.Process.Kill}, nil
}

// proc is a child agent with an authenticated IPC connection.
type proc struct {
	h        handle
	conn     *ipc.Conn
	closed   chan struct{} // closed when the IPC read loop ends
	stopping atomic.Bool
	once     sync.Once
}

func (p *proc) rotate() error { return p.conn.Send(ipc.Msg{T: ipc.TRotate}) }

// stop asks the agent to shut down, waits up to grace for it to go, then
// kills it (when we can) and closes the connection.
func (p *proc) stop(grace time.Duration) {
	p.once.Do(func() {
		p.stopping.Store(true)
		_ = p.conn.Send(ipc.Msg{T: ipc.TShutdown})
		done := p.h.exited
		if done == nil {
			done = p.closed
		}
		select {
		case <-done:
		case <-time.After(grace):
			if p.h.kill != nil {
				_ = p.h.kill()
			}
		}
		_ = p.conn.Close()
	})
}

// agentOptions are the GUI agent's flags: no current folder, the user's home,
// never idle out, and serve the local viewer for "Open here".
func (a *App) agentOptions() *app.Options {
	return &app.Options{NoCwd: true, Home: a.home, Local: true, Idle: 0, Priority: "low"}
}

// run starts one agent generation and pumps its IPC messages until it exits.
func (a *App) run(ctx context.Context, gen int, admin bool) {
	l, tok, err := ipc.Listen()
	if err != nil {
		a.fail(gen, "Couldn't prepare a private channel to the scanner: "+err.Error())
		return
	}
	defer l.Close()
	args := app.AgentArgs(a.agentOptions(), os.Getpid(), a.relay, l.Addr(), tok)
	if admin && !elevate.IsElevated() {
		// right after "agent", before any positional path
		args = append([]string{args[0], "--gui-elevated"}, args[1:]...)
	}

	h, err := a.spawn(ctx, a.self, args, admin)
	if err != nil {
		if errors.Is(err, elevate.ErrCancelled) {
			a.ifCurrent(gen, func(s *Snapshot) { resetRun(s); s.Phase = phaseCancelled })
			return
		}
		if ctx.Err() != nil {
			return // quit / restart while the admin prompt was up
		}
		a.fail(gen, "Couldn't start the scanner: "+err.Error())
		return
	}

	actx, cancel := context.WithTimeout(ctx, acceptTimeout)
	defer cancel()
	if h.exited != nil {
		go func() {
			select {
			case <-h.exited:
				cancel()
			case <-actx.Done():
			}
		}()
	}
	conn, err := l.Accept(actx)
	if err != nil {
		if h.kill != nil {
			_ = h.kill()
		}
		if ctx.Err() == nil {
			a.fail(gen, "The scanner didn't start. Try again, or restart Diskspace.")
		}
		return
	}
	p := &proc{h: h, conn: conn, closed: make(chan struct{})}

	current := a.updateWhen(func() bool {
		if a.gen != gen || a.quitting {
			return false
		}
		a.cur = p
		return true
	}, func(s *Snapshot) { s.Phase = phaseRunning })
	if !current {
		go p.readLoop(func(ipc.Msg) {})
		p.stop(stopGrace)
		return
	}

	var lastErr string
	p.readLoop(func(m ipc.Msg) {
		if m.T == ipc.TError {
			lastErr = m.Message
		}
		a.apply(p, m)
	})
	if p.stopping.Load() {
		return
	}
	// The agent went away on its own.
	if h.exited != nil {
		select {
		case <-h.exited:
		case <-time.After(time.Second):
		}
	}
	msg := "The scanner stopped unexpectedly."
	if lastErr != "" {
		msg = "The scanner stopped: " + lastErr
	}
	_ = conn.Close()
	a.updateWhen(func() bool {
		if a.cur != p {
			return false
		}
		a.cur = nil
		return true
	}, func(s *Snapshot) { resetRun(s); s.Phase = phaseError; s.Message = msg })
}

// spawnAdminDarwin is elevate.SpawnAdminDarwin with a context, so quitting
// the app while the macOS password dialog is up kills osascript (and with it
// the authorization request) instead of leaving the dialog behind. Arguments
// still travel via osascript argv only (elevate.BuildOsascriptArgs).
func spawnAdminDarwin(ctx context.Context, self string, args []string) error {
	out, err := exec.CommandContext(ctx, "osascript", elevate.BuildOsascriptArgs(self, args)...).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if strings.Contains(string(out), "(-128)") {
			return elevate.ErrCancelled
		}
		return fmt.Errorf("osascript: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (p *proc) readLoop(fn func(ipc.Msg)) {
	defer close(p.closed)
	for {
		m, err := p.conn.Recv()
		if err != nil {
			return
		}
		fn(m)
	}
}

// ifCurrent applies fn when gen is still the latest launch.
func (a *App) ifCurrent(gen int, fn func(s *Snapshot)) {
	a.updateWhen(func() bool { return a.gen == gen && !a.quitting }, fn)
}

func (a *App) fail(gen int, msg string) {
	a.ifCurrent(gen, func(s *Snapshot) { resetRun(s); s.Phase = phaseError; s.Message = msg })
}

// apply folds one IPC message from the current agent into the snapshot.
func (a *App) apply(p *proc, m ipc.Msg) {
	a.updateWhen(func() bool { return a.cur == p }, func(s *Snapshot) {
		switch m.T {
		case ipc.TReady:
			s.Phase = phaseRunning
			s.URL, s.LocalURL = m.URL, m.LocalURL
			s.Elevated = m.Elevated
			if m.FDA != "" {
				s.FDA = m.FDA
			}
			s.Notice = ""
		case ipc.TViewer:
			s.Viewer = m.State
			if m.State == "connected" {
				s.Code = m.Code
			} else {
				s.Code = ""
			}
		case ipc.TScan:
			s.Scan, s.ScanPath = m.State, m.Path
			if m.State == "resumed" {
				s.Scan = "started"
			}
		case ipc.TError:
			s.Notice = m.Message
		}
	})
}
