// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zyno-io/diskspace.cc/internal/ipc"
	"github.com/zyno-io/diskspace.cc/internal/session"
)

// ipcPair connects a parent and a child over a real loopback IPC channel.
func ipcPair(t *testing.T) (parent, child *ipc.Conn) {
	t.Helper()
	l, tok, err := ipc.Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type res struct {
		c   *ipc.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := l.Accept(ctx)
		ch <- res{c, err}
	}()
	child, err = ipc.Dial(ctx, l.Addr(), tok)
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { child.Close(); r.c.Close() })
	return r.c, child
}

// pump runs fn for every message conn receives until it closes.
func pump(conn *ipc.Conn, fn func(ipc.Msg)) {
	go func() {
		for {
			m, err := conn.Recv()
			if err != nil {
				return
			}
			fn(m)
		}
	}()
}

var sampleReq = session.ConfirmRequest{
	Job: 3, Mode: "permanent", Count: 1, Alloc: 42, Files: 1, Code: "111 222", ViaRelay: true, Trash: "Trash",
	Items: []session.ConfirmItem{{Path: "/tmp/x", Alloc: 42, Files: 1}},
}

func TestIPCConfirmRoundTrip(t *testing.T) {
	parent, child := ipcPair(t)
	answers := make(chan error, 4) // what the parent's prompt returns: nil = approve
	got := make(chan session.ConfirmRequest, 4)
	srv := newConfirmServer(parent, func(ctx context.Context, req session.ConfirmRequest) (bool, error) {
		got <- req
		select {
		case err := <-answers:
			if errors.Is(err, io.EOF) {
				return false, nil // declined
			}
			return err == nil, err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	})
	pump(parent, func(m ipc.Msg) { srv.handle(context.Background(), m) })
	asker := newIPCConfirmer(child)
	pump(child, func(m ipc.Msg) {
		if m.T == ipc.TConfirmReply {
			asker.reply(m)
		}
	})

	for _, tc := range []struct {
		answer error
		ok     bool
		err    error
	}{
		{nil, true, nil},
		{io.EOF, false, nil},
		{session.ErrConfirmTimeout, false, session.ErrConfirmTimeout},
	} {
		answers <- tc.answer
		ok, err := asker.Confirm(context.Background(), sampleReq)
		if ok != tc.ok || !errors.Is(err, tc.err) {
			t.Fatalf("answer %v: got %v %v", tc.answer, ok, err)
		}
		if r := <-got; r.Items[0].Path != "/tmp/x" || r.Code != "111 222" || r.Mode != "permanent" {
			t.Fatalf("parent saw %+v", r)
		}
	}

	// withdrawn by the child: the parent's prompt is canceled
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := asker.Confirm(ctx, sampleReq)
		done <- err
	}()
	<-got
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("withdrawn: %v", err)
	}
	// the parent's prompt ends (its ctx is canceled), so the next answer isn't consumed by it
	time.Sleep(100 * time.Millisecond)
	answers <- nil
	if ok, err := asker.Confirm(context.Background(), sampleReq); !ok || err != nil {
		t.Fatalf("after a withdrawal: %v %v", ok, err)
	}
	<-got
}

// A request withdrawn before it was sent is never asked.
func TestIPCConfirmAlreadyWithdrawn(t *testing.T) {
	parent, child := ipcPair(t)
	got := make(chan ipc.Msg, 1)
	pump(parent, func(m ipc.Msg) { got <- m })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, err := newIPCConfirmer(child).Confirm(ctx, sampleReq); ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v %v", ok, err)
	}
	select {
	case m := <-got:
		t.Fatalf("parent was asked: %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}

// A deadline on the child side is reported as a timeout.
func TestIPCConfirmDeadline(t *testing.T) {
	parent, child := ipcPair(t)
	pump(parent, func(ipc.Msg) {}) // a parent that never answers
	asker := newIPCConfirmer(child)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if ok, err := asker.Confirm(ctx, sampleReq); ok || !errors.Is(err, session.ErrConfirmTimeout) {
		t.Fatalf("got %v %v", ok, err)
	}
	// a late reply for a finished request is ignored
	asker.reply(ipc.Msg{T: ipc.TConfirmReply, ID: 1, OK: true})
}

func TestEncodeConfirmFitsOneLine(t *testing.T) {
	r := sampleReq
	r.Items = nil
	for i := 0; i < 15; i++ {
		r.Items = append(r.Items, session.ConfirmItem{Path: strings.Repeat("\\u{202E}", 500)})
	}
	b, err := encodeConfirm(r)
	if err != nil || len(b) > ipc.MaxLine {
		t.Fatalf("%d bytes, %v", len(b), err)
	}
}

func TestDeleteEventsOverIPC(t *testing.T) {
	for _, ev := range []session.Event{
		{Kind: session.EvDeleteStarted, Message: "Moving 2 item(s) to the Trash…"},
		{Kind: session.EvDeleteItem, Path: "/tmp/x", Message: "moved to the Trash (1 B)"},
		{Kind: session.EvDeleteDone, Message: "Moved 2 of 2"},
	} {
		m, ok := eventIPC(ev, false, "")
		if !ok || m.T != ipc.TDelete {
			t.Fatalf("eventIPC(%+v) = %+v %v", ev, m, ok)
		}
		back, ok := ipcEvent(m)
		if !ok || back.Kind != ev.Kind || back.Path != ev.Path || back.Message != ev.Message {
			t.Fatalf("round trip %+v -> %+v", ev, back)
		}
	}
}

func TestConfirmViaFlags(t *testing.T) {
	if _, err := ParseArgs([]string{"--confirm-via", "browser"}, "", io.Discard); err == nil {
		t.Fatal("bad --confirm-via accepted")
	}
	o := parse(t, "--confirm-via", "app", "--user-sid", "S-1-5-21-1")
	if o.ConfirmVia != "app" || o.SID != "S-1-5-21-1" {
		t.Fatalf("%+v", o)
	}
	o.Home = "/home/me"
	args := AgentArgsFor(o, os.Getpid(), "https://r", "127.0.0.1:1", "/tok", "cli")
	i := slices.Index(args, "--confirm-via")
	j := slices.Index(args, "--user-sid")
	if i < 0 || args[i+1] != "app" || j < 0 || args[j+1] != "S-1-5-21-1" || !slices.Contains(args, "/home/me") {
		t.Fatalf("args %q", args)
	}
	// the sudo re-launch keeps the user's SID and home, but has its own terminal
	ea := ElevatedArgs(o, 1, "https://r")
	if slices.Contains(ea, "--confirm-via") || !slices.Contains(ea, "--user-sid") {
		t.Fatalf("elevated args %q", ea)
	}
	back := parse(t, append([]string{"agent"}, args[1:]...)...)
	if back.ConfirmVia != "app" || back.SID != "S-1-5-21-1" || back.Home != "/home/me" {
		t.Fatalf("parsed back %+v", back)
	}
}
