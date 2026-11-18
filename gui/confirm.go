// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"fmt"
	goruntime "runtime"
	"strings"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/zyno-io/diskspace.cc/internal/cli"
	"github.com/zyno-io/diskspace.cc/internal/ipc"
	"github.com/zyno-io/diskspace.cc/internal/session"
)

// Deletion approval for the desktop app (agent --confirm-via app). Every
// deletion asked for by any browser, the app's own "Open here" view
// included, is approved in a native dialog: web content can't click it.
// Nothing here is reachable from the page (no bound method approves).

// dialogFunc shows a native message dialog and returns the chosen button.
type dialogFunc func(ctx context.Context, o wruntime.MessageDialogOptions) (string, error)

// wailsDialog brings the window forward, then shows the dialog.
func wailsDialog(ctx context.Context, o wruntime.MessageDialogOptions) (string, error) {
	wruntime.WindowUnminimise(ctx)
	wruntime.WindowShow(ctx)
	return wruntime.MessageDialog(ctx, o)
}

// askOptions builds the dialog: Yes/No on Windows (MessageBox can't name its
// buttons) and Linux, named buttons on macOS. Cancel / No is the default.
func askOptions(goos, title, message, yes string) (wruntime.MessageDialogOptions, func(string) bool) {
	if goos == "darwin" {
		return wruntime.MessageDialogOptions{
			Type: wruntime.QuestionDialog, Title: title, Message: message,
			Buttons: []string{yes, "Cancel"}, DefaultButton: "Cancel", CancelButton: "Cancel",
		}, func(r string) bool {
			return r == yes
		}
	}
	return wruntime.MessageDialogOptions{
		Type: wruntime.QuestionDialog, Title: title,
		Message:       message + "\n\nChoose Yes to " + strings.ToLower(yes) + ", or No to decline.",
		DefaultButton: "No",
	}, func(r string) bool {
		return strings.EqualFold(r, "yes")
	}
}

// confirmText is the dialog's message. Paths arrive display-safe.
func confirmText(req session.ConfirmRequest) string {
	var b strings.Builder
	n := req.Count
	items := "1 item"
	if n != 1 {
		items = fmt.Sprintf("%d items", n)
	}
	if req.Mode == "permanent" {
		fmt.Fprintf(&b, "A browser asks to delete %s permanently. This can't be undone.\n\n", items)
	} else {
		fmt.Fprintf(&b, "A browser asks to move %s to the %s.\n\n", items, req.Trash)
	}
	for _, it := range req.Items {
		size := cli.FormatBytes(it.Alloc)
		if it.AtLeast {
			size = "≥" + size
		}
		fmt.Fprintf(&b, "%s  (%s)\n", it.Path, size)
	}
	if req.More > 0 {
		fmt.Fprintf(&b, "… and %d more\n", req.More)
	}
	at := ""
	if req.AtLeast {
		at = "at least "
	}
	fmt.Fprintf(&b, "\nTotal: %s%s in %s files and %s folders.\n", at, cli.FormatBytes(req.Alloc), cli.FormatCount(req.Files), cli.FormatCount(req.Dirs))
	for _, w := range req.Warnings {
		b.WriteString("• " + w + "\n")
	}
	via := "on this computer"
	if req.ViaRelay {
		via = "through the relay"
	}
	fmt.Fprintf(&b, "\nRequested by the browser showing the code %s, connected %s. If that isn't you, or you didn't ask for this, decline.", req.Code, via)
	return b.String()
}

// askDelete asks for approval, with a second "can't be undone" dialog for a
// large permanent deletion.
func (a *App) askDelete(ctx context.Context, req session.ConfirmRequest) bool {
	title, yes := "Move to "+req.Trash+"?", "Move to "+req.Trash
	if req.Mode == "permanent" {
		title, yes = "Delete permanently?", "Delete"
	}
	o, approved := askOptions(goruntime.GOOS, title, confirmText(req), yes)
	r, err := a.dialog(ctx, o)
	if err != nil || !approved(r) {
		return false
	}
	if !req.Large {
		return true
	}
	at := ""
	if req.AtLeast {
		at = "at least "
	}
	msg := fmt.Sprintf("This permanently deletes %s%s in %s files. It can't be undone, and nothing goes to the %s.",
		at, cli.FormatBytes(req.Alloc), cli.FormatCount(req.Files), req.Trash)
	o, approved = askOptions(goruntime.GOOS, "Delete permanently? This can't be undone", msg, "Delete permanently")
	o.Type = wruntime.WarningDialog
	if goruntime.GOOS != "darwin" {
		o.Type = wruntime.QuestionDialog // Windows' warning dialog has only OK
	}
	r, err = a.dialog(ctx, o)
	return err == nil && approved(r)
}

// confirm answers one TConfirm from the agent (in its own goroutine: the IPC
// read loop keeps running while the dialog is up).
func (a *App) confirm(p *proc, m ipc.Msg) {
	a.dialogMu.Lock() // one dialog at a time
	defer a.dialogMu.Unlock()
	reply := ipc.Msg{T: ipc.TConfirmReply, ID: m.ID, State: "declined"}
	var req session.ConfirmRequest
	ctx := a.wailsCtx()
	switch {
	case json.Unmarshal(m.Confirm, &req) != nil:
		reply.State, reply.Message = "error", "bad request"
	case ctx == nil:
		reply.State, reply.Message = "error", "the window isn't ready"
	default:
		if a.askDelete(ctx, req) {
			reply.OK, reply.State = true, "approved"
		}
	}
	if p.withdrawn(m.ID) {
		if reply.OK {
			a.notice(p, "That deletion request was withdrawn or timed out before you answered. Nothing was deleted.")
		}
		return
	}
	_ = p.conn.Send(reply)
}

// notice shows a message in the launcher while p is the current agent.
func (a *App) notice(p *proc, msg string) {
	a.updateWhen(func() bool { return a.cur == p }, func(s *Snapshot) { s.Notice = msg })
}
