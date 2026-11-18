// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/zyno-io/diskspace.cc/internal/ipc"
	"github.com/zyno-io/diskspace.cc/internal/session"
)

// stubDialog records dialogs and answers them from a script.
type stubDialog struct {
	mu      sync.Mutex
	shown   []wruntime.MessageDialogOptions
	answers []bool // approve each dialog in turn; missing = decline
}

func (s *stubDialog) show(_ context.Context, o wruntime.MessageDialogOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.shown)
	s.shown = append(s.shown, o)
	if n < len(s.answers) && s.answers[n] {
		if len(o.Buttons) > 0 {
			return o.Buttons[0], nil
		}
		return "Yes", nil
	}
	if len(o.Buttons) > 0 {
		return o.CancelButton, nil
	}
	return "No", nil
}

func (s *stubDialog) at(i int) wruntime.MessageDialogOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shown[i]
}

func (s *stubDialog) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.shown)
}

// startWithChild starts the app's agent and returns the fake child's IPC side.
func startWithChild(t *testing.T, show dialogFunc) (*App, chan Snapshot, *ipc.Conn) {
	t.Helper()
	f := newFakeChild(t)
	a, ch := newTestApp(t, f)
	a.dialog = show
	a.ctx = context.Background()
	if err := a.Start(false, false); err != nil {
		t.Fatal(err)
	}
	args := <-f.args
	if argAfter(args, "--confirm-via") != "app" {
		t.Fatalf("agent args %q lack --confirm-via app", args)
	}
	c := <-f.conn
	t.Cleanup(func() { c.Close(); a.shutdown(context.Background()) })
	if err := c.Send(ipc.Msg{T: ipc.TReady, URL: "https://x/s/1#k"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, func(s Snapshot) bool { return s.URL != "" })
	return a, ch, c
}

func ask(t *testing.T, c *ipc.Conn, id uint64, req session.ConfirmRequest) {
	t.Helper()
	b, _ := json.Marshal(req)
	if err := c.Send(ipc.Msg{T: ipc.TConfirm, ID: id, Confirm: b}); err != nil {
		t.Fatal(err)
	}
}

func replyOf(t *testing.T, c *ipc.Conn) ipc.Msg {
	t.Helper()
	type res struct {
		m   ipc.Msg
		err error
	}
	ch := make(chan res, 1)
	go func() {
		m, err := c.Recv()
		ch <- res{m, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.m
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	return ipc.Msg{}
}

var guiReq = session.ConfirmRequest{
	Job: 1, Mode: "trash", Count: 1, Alloc: 2_000_000, Files: 3, Dirs: 1, Code: "123 456", ViaRelay: true, Trash: "Trash",
	Items: []session.ConfirmItem{{Path: `/Users/me/Caches/x\x0Ay`, Dir: true, Alloc: 2_000_000, Files: 3}},
}

func TestConfirmDialogApproveDecline(t *testing.T) {
	d := &stubDialog{answers: []bool{true, false}}
	_, _, c := startWithChild(t, d.show)

	ask(t, c, 1, guiReq)
	if r := replyOf(t, c); r.T != ipc.TConfirmReply || r.ID != 1 || !r.OK || r.State != "approved" {
		t.Fatalf("approve: %+v", r)
	}
	ask(t, c, 2, guiReq)
	if r := replyOf(t, c); r.ID != 2 || r.OK || r.State != "declined" {
		t.Fatalf("decline: %+v", r)
	}
	o := d.at(0)
	for _, want := range []string{`/Users/me/Caches/x\x0Ay`, "123 456", "through the relay", "move 1 item to the Trash"} {
		if !strings.Contains(o.Message, want) {
			t.Errorf("dialog lacks %q:\n%s", want, o.Message)
		}
	}
	if goruntime.GOOS == "darwin" {
		if o.DefaultButton != "Cancel" || o.CancelButton != "Cancel" || o.Buttons[0] != "Move to Trash" {
			t.Fatalf("buttons %+v", o)
		}
	} else if o.DefaultButton != "No" || !strings.Contains(o.Message, "Choose Yes") {
		t.Fatalf("Yes/No wording %+v", o)
	}
}

func TestConfirmDialogLargePermanentAsksTwice(t *testing.T) {
	big := guiReq
	big.Mode, big.Large, big.Alloc, big.Files = "permanent", true, 200e9, 300_000
	for _, tc := range []struct {
		answers []bool
		ok      bool
	}{{[]bool{true, true}, true}, {[]bool{true, false}, false}} {
		d := &stubDialog{answers: tc.answers}
		_, _, c := startWithChild(t, d.show)
		ask(t, c, 7, big)
		if r := replyOf(t, c); r.OK != tc.ok {
			t.Fatalf("answers %v: %+v", tc.answers, r)
		}
		if d.count() != 2 || !strings.Contains(d.at(1).Message, "can't be undone") {
			t.Fatalf("second dialog: %d dialogs", d.count())
		}
	}
	// small permanent: one dialog
	d := &stubDialog{answers: []bool{true}}
	_, _, c := startWithChild(t, d.show)
	small := big
	small.Large = false
	ask(t, c, 8, small)
	if r := replyOf(t, c); !r.OK || d.count() != 1 {
		t.Fatalf("small: %+v, %d dialogs", r, d.count())
	}
}

// An approval given right after the dialog appeared is asked again; three
// hasty ones decline.
func TestConfirmDialogHastyAnswerAskedAgain(t *testing.T) {
	d := &stubDialog{answers: []bool{true, true, true}}
	a, _, c := startWithChild(t, d.show)
	a.mu.Lock()
	a.dialogMinReact = time.Hour
	a.mu.Unlock()
	ask(t, c, 1, guiReq)
	if r := replyOf(t, c); r.OK || d.count() != 3 {
		t.Fatalf("hasty: %+v after %d dialogs", r, d.count())
	}
}

// A request withdrawn while an earlier dialog was up is never shown.
func TestConfirmDialogSkipsWithdrawn(t *testing.T) {
	release := make(chan struct{})
	d := &stubDialog{answers: []bool{false, true}}
	first := true
	var mu sync.Mutex
	show := func(ctx context.Context, o wruntime.MessageDialogOptions) (string, error) {
		mu.Lock()
		wait := first
		first = false
		mu.Unlock()
		if wait {
			<-release
		}
		return d.show(ctx, o)
	}
	_, _, c := startWithChild(t, show)
	ask(t, c, 1, guiReq) // up, unanswered
	time.Sleep(100 * time.Millisecond)
	ask(t, c, 2, guiReq) // waits behind it
	if err := c.Send(ipc.Msg{T: ipc.TConfirmCancel, ID: 2}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	if r := replyOf(t, c); r.ID != 1 || r.OK {
		t.Fatalf("first: %+v", r)
	}
	time.Sleep(200 * time.Millisecond)
	if n := d.count(); n != 1 {
		t.Fatalf("%d dialogs shown, want 1 (the withdrawn one skipped)", n)
	}
}

// An answer to a request the agent withdrew is not sent; the launcher says so.
func TestConfirmDialogWithdrawn(t *testing.T) {
	release := make(chan struct{})
	d := &stubDialog{answers: []bool{true}}
	// the dialog stays up until the request was withdrawn
	up := make(chan struct{}, 1)
	slow := func(ctx context.Context, o wruntime.MessageDialogOptions) (string, error) {
		up <- struct{}{}
		<-release
		return d.show(ctx, o)
	}
	_, ch, c := startWithChild(t, slow)
	ask(t, c, 3, guiReq)
	<-up // the dialog is showing
	if err := c.Send(ipc.Msg{T: ipc.TConfirmCancel, ID: 3}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	waitFor(t, ch, func(s Snapshot) bool { return strings.Contains(s.Notice, "withdrawn") })
	// a deletion result shows in the launcher
	if err := c.Send(ipc.Msg{T: ipc.TDelete, State: "done", Message: "Moved 1 of 1 item(s) to the Trash"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, func(s Snapshot) bool { return strings.HasPrefix(s.Notice, "Moved 1 of 1") })
}
