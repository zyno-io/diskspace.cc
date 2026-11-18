// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"time"

	"github.com/zyno-io/diskspace.cc/internal/cli"
	"github.com/zyno-io/diskspace.cc/internal/elevate"
	"github.com/zyno-io/diskspace.cc/internal/ipc"
)

// elevateWindows starts an elevated headless agent via UAC and renders its
// IPC events in this console. It returns done=false to continue unelevated
// in-process (UAC declined, child died before it was ready).
func elevateWindows(o *Options, version, self string, stderr io.Writer, fail func(string) (int, bool)) (int, bool) {
	l, tokFile, err := ipc.Listen()
	if err != nil {
		return fail("ipc: " + err.Error())
	}
	defer l.Close()
	co := *o
	if elevate.HasTTY() {
		co.ConfirmVia = ConfirmConsole // deletions are approved in this console
	}
	child, err := elevate.RunAsAdmin(self, AgentArgsFor(&co, os.Getpid(), o.Relay, l.Addr(), tokFile, "cli"), false)
	if err != nil {
		if errors.Is(err, elevate.ErrCancelled) {
			fmt.Fprintf(stderr, "diskspace: administrator permission was declined; continuing as %s.\n", elevate.CurrentUser())
			if o.Elevate {
				return 1, true
			}
			return 0, false
		}
		return fail("could not start elevated agent: " + err.Error())
	}

	exited := make(chan int, 1)
	go func() {
		code, _ := child.Wait()
		exited <- code
	}()
	actx, acancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-exited:
			acancel()
		case <-actx.Done():
		}
	}()
	conn, err := l.Accept(actx)
	if err != nil {
		acancel()
		_ = child.Kill()
		return fail("the elevated agent did not start")
	}
	acancel()
	defer conn.Close()

	stdoutF := os.Stdout
	rend := cli.New(cli.Options{
		Out: stdoutF, Err: stderr, TTY: cli.IsTerminal(stdoutF), Quiet: o.Quiet, Version: "",
		Elevated: true, User: elevate.CurrentUser(), GOOS: runtime.GOOS, TermSize: cli.StdoutSize,
	})
	rend.Header()
	defer rend.Close()
	cctx, ccancel := context.WithCancel(context.Background())
	defer ccancel()
	asker := &cli.Confirmer{Hold: rend.Hold}
	confirms := newConfirmServer(conn, asker.Confirm)

	msgs := make(chan ipc.Msg)
	recvErr := make(chan error, 1)
	go func() {
		for {
			m, err := conn.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			msgs <- m
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)

	ready := false
	var checked sync.Once
	shutdown := func() int {
		_ = conn.Send(ipc.Msg{T: ipc.TShutdown})
		select {
		case code := <-exited:
			return code
		case <-time.After(3 * time.Second):
			_ = child.Kill()
			return 0
		}
	}
	for {
		select {
		case m := <-msgs:
			if confirms.handle(cctx, m) {
				continue
			}
			if m.T == ipc.TReady {
				ready = true
				if m.URL != "" {
					checked.Do(func() { go announceUpdate(rend, relayFor(o), version) })
				}
			}
			if ev, ok := ipcEvent(m); ok {
				rend.Event(ev)
			}
		case <-sig:
			return shutdown(), true
		case <-recvErr:
			select {
			case code := <-exited:
				if !ready {
					rend.Warn("The elevated agent stopped before it was ready; continuing without administrator access.")
					return 0, false
				}
				return code, true
			case <-time.After(3 * time.Second):
				_ = child.Kill()
				return 0, true
			}
		case code := <-exited:
			if !ready {
				return 0, false
			}
			return code, true
		}
	}
}
