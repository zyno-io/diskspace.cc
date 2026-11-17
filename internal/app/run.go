// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/zyno-io/diskspace.cc/internal/browser"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/zyno-io/diskspace.cc/internal/cli"
	"github.com/zyno-io/diskspace.cc/internal/elevate"
	"github.com/zyno-io/diskspace.cc/internal/ipc"
	"github.com/zyno-io/diskspace.cc/internal/session"
)

// Main runs the program and exits with its status.
func Main(argv []string, version, defaultRelay string) {
	os.Exit(Run(argv[1:], version, defaultRelay, os.Stdout, os.Stderr))
}

// Run is the testable entry point; args exclude argv[0].
func Run(args []string, version, defaultRelay string, stdout, stderr io.Writer) int {
	o, err := ParseArgs(args, defaultRelay, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, "diskspace:", err)
		return 2
	}
	if o.ShowVer {
		fmt.Fprintln(stdout, "diskspace", version)
		return 0
	}
	if err := o.Resolve(os.Getwd, invokingHome); err != nil {
		fmt.Fprintln(stderr, "diskspace:", err)
		return 2
	}
	if o.AgentMode {
		return runAgentMode(o, version)
	}
	return runCLI(o, version, stdout, stderr)
}

func relayFor(o *Options) string {
	if o.NoRelay {
		return ""
	}
	return o.Relay
}

// elevationKind names how this process got its privileges.
func elevationKind() string {
	if !elevate.IsElevated() {
		return "none"
	}
	switch runtime.GOOS {
	case "windows":
		return "uac"
	default:
		if os.Getenv("SUDO_USER") != "" {
			return "sudo"
		}
	}
	return "native"
}

func sessionConfig(o *Options, version string) session.Config {
	cfg := session.Config{
		RelayURL:  relayFor(o),
		Local:     o.Local,
		Root:      o.Root,
		Home:      o.Home,
		AutoScan:  o.Path,
		Elevation: elevationKind(),
		FDA:       string(elevate.ProbeFDA()),
		Version:   version,
		Threads:   o.Threads,
		Idle:      o.Idle,
	}
	if o.Cwd != "" {
		c := o.Cwd
		cfg.Cwd = &c
	}
	return cfg
}

func runCLI(o *Options, version string, stdout, stderr io.Writer) int {
	self, _ := os.Executable()
	stdoutF, _ := stdout.(*os.File)
	tty := cli.IsTerminal(stdoutF)

	// Elevation, before any network or session activity.
	if !elevate.IsElevated() && !o.NoElevate {
		if code, done := maybeElevate(o, self, stderr); done {
			return code
		}
	}

	cfg := sessionConfig(o, version)
	rend := cli.New(cli.Options{
		Out: stdout, Err: stderr, TTY: tty, Quiet: o.Quiet, Version: version,
		Elevated: elevate.IsElevated(), User: elevate.CurrentUser(),
		FDADenied: cfg.FDA == "denied", GOOS: runtime.GOOS,
		TermSize: cli.StdoutSize,
	})
	return serve(o, cfg, rend, nil)
}

// serve runs the in-process agent, rendering events with rend.
func serve(o *Options, cfg session.Config, rend *cli.Renderer, onReady func(ev session.Event)) int {
	opened := false
	cfg.Events = func(ev session.Event) {
		rend.Event(ev)
		if ev.Kind == session.EvLinkReady && o.Open && !opened && !elevate.IsElevated() {
			opened = true
			u := ev.URL
			if u == "" {
				u = ev.LocalURL
			}
			go openBrowser(u)
		}
		if onReady != nil {
			onReady(ev)
		}
	}
	a, err := session.NewAgent(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "diskspace:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if o.ParentPID > 0 {
		go elevate.WatchParent(ctx, o.ParentPID, stop)
	}
	defer browser.Cleanup()
	rend.Header()
	err = a.Run(ctx)
	rend.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "diskspace:", err)
		return 1
	}
	return 0
}

// maybeElevate implements the elevation flow. done=true means the process
// should exit with code (Unix never returns on success: sudo replaces it).
func maybeElevate(o *Options, self string, stderr io.Writer) (code int, done bool) {
	want := o.Elevate
	if !want {
		if !elevate.HasTTY() {
			fmt.Fprintln(stderr, "diskspace: not running as administrator; run with sudo or --elevate to include system and other users' folders")
			return 0, false
		}
		ch, err := elevate.Prompt(elevate.PromptOptions{
			User: elevate.CurrentUser(), OS: runtime.GOOS, FDA: elevate.ProbeFDA(),
		})
		if err != nil || ch != elevate.ChoiceElevate {
			return 0, false
		}
		want = true
	}
	fail := func(msg string) (int, bool) {
		fmt.Fprintln(stderr, "diskspace:", msg)
		if o.Elevate {
			return 1, true
		}
		fmt.Fprintf(stderr, "Continuing as %s.\n", elevate.CurrentUser())
		return 0, false
	}
	if runtime.GOOS == "windows" {
		return elevateWindows(o, self, stderr, fail)
	}
	if err := elevate.SudoValidate(); err != nil && !errors.Is(err, elevate.ErrNoTTY) {
		return fail("could not get administrator access: " + err.Error())
	}
	args := ElevatedArgs(o, os.Getpid(), o.Relay)
	if err := elevate.ExecSudo(self, args); err != nil {
		return fail("could not start sudo: " + err.Error())
	}
	return 0, false // unreachable on success
}

// openBrowser opens url via a private redirect file so the link (which holds
// the session key) never appears in a process command line.
func openBrowser(url string) {
	if url == "" {
		return
	}
	if err := browser.Open(url); err != nil {
		fmt.Fprintln(os.Stderr, "diskspace: could not open browser:", err)
	}
}

// ipcEvent maps a child IPC message to a session event for rendering.
func ipcEvent(m ipc.Msg) (session.Event, bool) {
	switch m.T {
	case ipc.TReady:
		return session.Event{Kind: session.EvLinkReady, URL: m.URL, LocalURL: m.LocalURL}, true
	case ipc.TViewer:
		switch m.State {
		case "connected":
			return session.Event{Kind: session.EvViewerConnected, Code: m.Code}, true
		case "disconnected":
			return session.Event{Kind: session.EvViewerDisconnected, Code: m.Code}, true
		case "superseded":
			return session.Event{Kind: session.EvViewerSuperseded, Code: m.Code}, true
		}
	case ipc.TScan:
		switch m.State {
		case "started":
			return session.Event{Kind: session.EvScanStarted, Path: m.Path}, true
		case "done":
			return session.Event{Kind: session.EvScanDone, Path: m.Path}, true
		case "canceled":
			return session.Event{Kind: session.EvScanCanceled, Path: m.Path}, true
		}
	case ipc.TError:
		return session.Event{Kind: session.EvError, Message: m.Message}, true
	}
	return session.Event{}, false
}

// eventIPC maps a session event to an IPC message (child side).
func eventIPC(ev session.Event, elevated bool, fda string) (ipc.Msg, bool) {
	switch ev.Kind {
	case session.EvLinkReady:
		return ipc.Msg{T: ipc.TReady, URL: ev.URL, LocalURL: ev.LocalURL, Elevated: elevated, FDA: fda}, true
	case session.EvViewerConnected:
		return ipc.Msg{T: ipc.TViewer, State: "connected", Code: ev.Code}, true
	case session.EvViewerDisconnected:
		return ipc.Msg{T: ipc.TViewer, State: "disconnected", Code: ev.Code}, true
	case session.EvViewerSuperseded:
		return ipc.Msg{T: ipc.TViewer, State: "superseded", Code: ev.Code}, true
	case session.EvScanStarted:
		return ipc.Msg{T: ipc.TScan, State: "started", Path: ev.Path}, true
	case session.EvScanDone:
		return ipc.Msg{T: ipc.TScan, State: "done", Path: ev.Path}, true
	case session.EvScanCanceled:
		return ipc.Msg{T: ipc.TScan, State: "canceled", Path: ev.Path}, true
	case session.EvScanError:
		return ipc.Msg{T: ipc.TError, Message: "scan failed: " + ev.Message}, true
	case session.EvError:
		m := ev.Message
		if m == "" && ev.Err != nil {
			m = ev.Err.Error()
		}
		return ipc.Msg{T: ipc.TError, Message: m}, true
	}
	return ipc.Msg{}, false
}
