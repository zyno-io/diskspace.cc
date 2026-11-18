// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zyno-io/diskspace.cc/internal/session"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// waitFor waits until out contains sub (the prompt is up).
func (s *syncBuf) waitFor(t *testing.T, sub string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(s.String(), sub) {
		if time.Now().After(deadline) {
			t.Fatalf("output never contained %q:\n%s", sub, s.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type harness struct {
	c   *Confirmer
	in  *io.PipeWriter
	out *syncBuf
}

func newHarness(timeout time.Duration) *harness {
	r, w := io.Pipe()
	out := &syncBuf{}
	return &harness{c: &Confirmer{In: r, Out: out, Timeout: timeout, MinReact: -1}, in: w, out: out}
}

func (h *harness) ask(ctx context.Context, req session.ConfirmRequest) (chan bool, chan error) {
	okc, errc := make(chan bool, 1), make(chan error, 1)
	go func() {
		ok, err := h.c.Confirm(ctx, req)
		okc <- ok
		errc <- err
	}()
	return okc, errc
}

func (h *harness) typeLine(t *testing.T, waitFor, line string) {
	t.Helper()
	h.out.waitFor(t, waitFor)
	if _, err := io.WriteString(h.in, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

func result(t *testing.T, okc chan bool, errc chan error) (bool, error) {
	t.Helper()
	select {
	case ok := <-okc:
		return ok, <-errc
	case <-time.After(5 * time.Second):
		t.Fatal("Confirm did not return")
	}
	return false, nil
}

var trashReq = session.ConfirmRequest{
	Mode: "trash", Count: 2, Alloc: 12_300_000_000, Files: 1234, Dirs: 20, Code: "123 456", ViaRelay: true, Trash: "Trash",
	Items: []session.ConfirmItem{{Path: "/Users/me/Library/Caches/big", Dir: true, Alloc: 12_000_000_000, Files: 1233, Dirs: 19}, {Path: "/Users/me/old.dmg", Alloc: 300_000_000, Files: 1}},
}

func TestConfirmYes(t *testing.T) {
	h := newHarness(time.Minute)
	okc, errc := h.ask(context.Background(), trashReq)
	h.typeLine(t, "[y/N]", "y")
	if ok, err := result(t, okc, errc); !ok || err != nil {
		t.Fatalf("got %v %v", ok, err)
	}
	out := h.out.String()
	for _, want := range []string{"move 2 items to the Trash", "/Users/me/Library/Caches/big", "12.0 GB", "123 456", "through the relay", "Approved."} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt lacks %q:\n%s", want, out)
		}
	}
}

func TestConfirmDefaultNo(t *testing.T) {
	for _, answer := range []string{"", "n", "No", "  "} {
		h := newHarness(time.Minute)
		okc, errc := h.ask(context.Background(), trashReq)
		h.typeLine(t, "[y/N]", answer)
		if ok, err := result(t, okc, errc); ok || err != nil {
			t.Fatalf("%q: got %v %v", answer, ok, err)
		}
	}
}

func TestConfirmReasksOnGarbage(t *testing.T) {
	h := newHarness(time.Minute)
	okc, errc := h.ask(context.Background(), trashReq)
	h.typeLine(t, "[y/N]", "maybe")
	h.typeLine(t, "Please answer y or n", "yes")
	if ok, err := result(t, okc, errc); !ok || err != nil {
		t.Fatalf("got %v %v", ok, err)
	}
}

func TestConfirmTimeout(t *testing.T) {
	h := newHarness(100 * time.Millisecond)
	okc, errc := h.ask(context.Background(), trashReq)
	ok, err := result(t, okc, errc)
	if ok || !errors.Is(err, session.ErrConfirmTimeout) {
		t.Fatalf("got %v %v", ok, err)
	}
	if !strings.Contains(h.out.String(), "No answer in time") {
		t.Fatal(h.out.String())
	}
	// a late answer is not taken by the next prompt
	io.WriteString(h.in, "y\n")
	time.Sleep(50 * time.Millisecond)
	h.c.Timeout = time.Minute
	okc, errc = h.ask(context.Background(), trashReq)
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(h.out.String(), "[y/N]") < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	io.WriteString(h.in, "n\n")
	if ok, err := result(t, okc, errc); ok || err != nil {
		t.Fatalf("stale answer used: %v %v", ok, err)
	}
}

func TestConfirmWithdrawn(t *testing.T) {
	h := newHarness(time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	okc, errc := h.ask(ctx, trashReq)
	h.out.waitFor(t, "[y/N]")
	cancel()
	if ok, err := result(t, okc, errc); ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v %v", ok, err)
	}
	if !strings.Contains(h.out.String(), "withdrawn") {
		t.Fatal(h.out.String())
	}
}

func TestConfirmEOFDeclines(t *testing.T) {
	h := newHarness(time.Minute)
	okc, errc := h.ask(context.Background(), trashReq)
	h.out.waitFor(t, "[y/N]")
	h.in.Close()
	if ok, err := result(t, okc, errc); ok || err != nil {
		t.Fatalf("got %v %v", ok, err)
	}
}

func TestConfirmLargePermanentNeedsDelete(t *testing.T) {
	big := trashReq
	big.Mode, big.Large, big.Alloc, big.Files = "permanent", true, 150e9, 250_000
	for answer, want := range map[string]bool{"delete": true, "yes": false, "DELETE": false, "": false} {
		h := newHarness(time.Minute)
		okc, errc := h.ask(context.Background(), big)
		h.typeLine(t, "[y/N]", "y")
		h.typeLine(t, "Type delete to confirm", answer)
		if ok, err := result(t, okc, errc); ok != want || err != nil {
			t.Fatalf("%q: got %v %v", answer, ok, err)
		}
		if out := h.out.String(); !strings.Contains(out, "PERMANENTLY") || !strings.Contains(out, "150.0 GB") {
			t.Fatal(out)
		}
	}
	// small permanent: one question
	small := big
	small.Large = false
	h := newHarness(time.Minute)
	okc, errc := h.ask(context.Background(), small)
	h.typeLine(t, "[y/N]", "y")
	if ok, err := result(t, okc, errc); !ok || err != nil {
		t.Fatalf("got %v %v", ok, err)
	}
	if strings.Contains(h.out.String(), "Type delete") {
		t.Fatal("asked twice for a small deletion")
	}
}

// Lines typed before the prompt could be read are ignored.
func TestConfirmIgnoresTypeAhead(t *testing.T) {
	h := newHarness(time.Minute)
	h.c.MinReact = 200 * time.Millisecond
	okc, errc := h.ask(context.Background(), trashReq)
	h.typeLine(t, "[y/N]", "y") // immediately: ignored
	time.Sleep(300 * time.Millisecond)
	io.WriteString(h.in, "n\n")
	if ok, err := result(t, okc, errc); ok || err != nil {
		t.Fatalf("type-ahead approved: %v %v", ok, err)
	}
}

func TestConfirmHoldsStatusLine(t *testing.T) {
	out := &syncBuf{}
	rend := New(Options{Out: out, Err: out, TTY: true, NoColor: true})
	rend.Event(session.Event{Kind: session.EvScanStarted, Path: "/x"})
	h := newHarness(time.Minute)
	h.c.Hold = rend.Hold
	okc, errc := h.ask(context.Background(), trashReq)
	h.out.waitFor(t, "[y/N]")
	before := out.String()
	rend.Info("while asking")
	rend.Event(session.Event{Kind: session.EvScanDone, Path: "/x"})
	if out.String() != before {
		t.Fatalf("renderer wrote during the prompt: %q", strings.TrimPrefix(out.String(), before))
	}
	io.WriteString(h.in, "n\n")
	result(t, okc, errc)
	if !strings.Contains(out.String(), "while asking") {
		t.Fatal("held line lost")
	}
}

func TestRenderConfirmListsEveryItemAndFolders(t *testing.T) {
	r := trashReq
	r.Items = nil
	for i := 0; i < 20; i++ {
		r.Items = append(r.Items, session.ConfirmItem{Path: fmt.Sprintf("/Users/me/Caches/item%02d", i), Alloc: 1000})
	}
	r.Folders = []session.ConfirmFolder{{Path: "/Users/me/Caches", Count: 19, Alloc: 19000}, {Path: "/Users/me/Documents", Count: 1, Alloc: 1000}}
	s := RenderConfirm(r)
	for _, want := range []string{"item00", "item19", "In these folders:", "/Users/me/Documents  (1 item)"} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
}

func TestRenderConfirmShowsWarningsAndMore(t *testing.T) {
	r := trashReq
	r.More, r.AtLeast, r.ViaRelay = 4, true, false
	r.Warnings = []string{"Contains another volume or mount point, which is left alone."}
	s := RenderConfirm(r)
	for _, want := range []string{"… and 4 more", "at least", "! Contains another volume", "on this computer"} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q:\n%s", want, s)
		}
	}
}
