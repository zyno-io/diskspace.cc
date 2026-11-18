// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/zyno-io/diskspace.cc/internal/elevate"
	"github.com/zyno-io/diskspace.cc/internal/ipc"
	"github.com/zyno-io/diskspace.cc/internal/session"
)

// runAgentMode is `diskspace agent --ipc ...`: a headless agent that reports
// over IPC to its parent (the Windows CLI or the GUI) and obeys rotate /
// shutdown. It exits within about a second of the IPC connection closing.
func runAgentMode(o *Options, version string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	conn, err := ipc.Dial(ctx, o.IPCAddr, o.IPCToken)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "diskspace agent: ipc:", err)
		return 1
	}
	defer conn.Close()

	cfg := sessionConfig(o, version)
	cfg.Launcher = "app" // the GUI, or (Windows CLI flow) --launcher cli
	if o.Launcher == "cli" {
		cfg.Launcher = "cli"
	}
	elevated := elevate.IsElevated()
	if elevated && o.GUIElevated {
		cfg.Elevation = "gui-admin" // elevated by the desktop app (osascript / UAC)
	}
	fda := cfg.FDA
	var asker *ipcConfirmer
	if o.ConfirmVia != "" {
		// the parent (desktop app dialog / Windows console) approves deletions
		asker = newIPCConfirmer(conn)
		cfg.Confirm, cfg.ConfirmKind = asker.Confirm, o.ConfirmVia
	}
	cfg.Events = func(ev session.Event) {
		if m, ok := eventIPC(ev, elevated, fda); ok {
			_ = conn.Send(m)
		}
	}
	a, err := session.NewAgent(cfg)
	if err != nil {
		_ = conn.Send(ipc.Msg{T: ipc.TError, Message: err.Error()})
		return 1
	}
	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	if o.ParentPID > 0 {
		go elevate.WatchParent(runCtx, o.ParentPID, stop)
	}
	go func() {
		defer stop() // EOF or error: the parent is gone
		for {
			m, err := conn.Recv()
			if err != nil {
				return
			}
			switch m.T {
			case ipc.TRotate:
				a.Rotate()
			case ipc.TConfirmReply:
				if asker != nil {
					asker.reply(m)
				}
			case ipc.TShutdown:
				return
			}
		}
	}()
	if err := a.Run(runCtx); err != nil {
		_ = conn.Send(ipc.Msg{T: ipc.TError, Message: err.Error()})
		return 1
	}
	return 0
}
