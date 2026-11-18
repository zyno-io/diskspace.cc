// SPDX-License-Identifier: MIT

package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/zyno-io/diskspace.cc/internal/elevate"
	"github.com/zyno-io/diskspace.cc/internal/session"
)

// Confirmer asks the person at this computer to approve a deletion, in the
// terminal diskspace runs in (/dev/tty, or CONIN$/CONOUT$ on Windows): input
// only someone at this computer can type. Enter, n, end of input and no
// answer within the timeout all decline.
type Confirmer struct {
	In  io.Reader // nil: the controlling terminal
	Out io.Writer // nil: the controlling terminal
	// Hold pauses the status line while the prompt is up (optional).
	Hold func() (release func())
	// Timeout declines an unanswered prompt (default 2 minutes).
	Timeout time.Duration
	// MinReact ignores lines that arrive sooner than this after the prompt
	// appeared: they were typed before it could be read (default 300 ms).
	MinReact time.Duration
	Now      func() time.Time

	once    sync.Once
	openErr error
	out     io.Writer
	lines   chan ttyLine
	mu      sync.Mutex // one prompt at a time
}

type ttyLine struct {
	s   string
	at  time.Time
	eof bool
}

// ErrNoTerminal: no terminal to ask in.
var ErrNoTerminal = errors.New("cli: no terminal to ask in")

func (c *Confirmer) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// start opens the terminal once and starts the single reader goroutine. A
// blocked terminal read can't be interrupted portably, so one reader lives
// for the whole run and each prompt takes the lines typed while it is up.
func (c *Confirmer) start() error {
	c.once.Do(func() {
		in, out := c.In, c.Out
		injected := in != nil
		if in == nil || out == nil {
			ti, to, err := elevate.OpenTTY()
			if err != nil {
				c.openErr = ErrNoTerminal
				return
			}
			if in == nil {
				in = ti
				ignoreBackgroundRead()
			} else if ti != to {
				ti.Close()
			}
			if out == nil {
				out = to
			}
		}
		c.out = out
		c.lines = make(chan ttyLine, 16)
		go c.readLoop(in, injected)
	})
	return c.openErr
}

func (c *Confirmer) readLoop(in io.Reader, injected bool) {
	r := bufio.NewReaderSize(in, 4096)
	for {
		s, err := r.ReadString('\n')
		at := c.now()
		if err == nil || s != "" {
			c.offer(ttyLine{s: s, at: at})
		}
		switch {
		case err == nil:
		case injected:
			c.offer(ttyLine{eof: true, at: at})
			close(c.lines)
			return
		case errors.Is(err, io.EOF):
			c.offer(ttyLine{eof: true, at: at}) // Ctrl-D: decline
		default:
			// e.g. EIO while running in the background: nobody can answer;
			// the prompt times out.
			time.Sleep(500 * time.Millisecond)
		}
	}
}

func (c *Confirmer) offer(l ttyLine) {
	select {
	case c.lines <- l:
	default: // nobody is asking and the buffer is full: drop it
	}
}

// drain drops lines typed before the prompt.
func (c *Confirmer) drain() {
	for {
		select {
		case _, ok := <-c.lines:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

// Available reports whether there is a terminal to ask in. The terminal is
// only read once the first prompt is up.
func (c *Confirmer) Available() bool {
	if c.In != nil && c.Out != nil {
		return true
	}
	return elevate.HasTTY()
}

type reply int

const (
	replyNone reply = iota
	replyLine
	replyEOF
	replyTimeout
	replyCanceled
)

// Confirm implements session.Config.Confirm.
func (c *Confirmer) Confirm(ctx context.Context, req session.ConfirmRequest) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.start(); err != nil {
		return false, err
	}
	if c.Hold != nil {
		release := c.Hold()
		defer release()
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	minReact := c.MinReact
	if minReact == 0 {
		minReact = 300 * time.Millisecond
	} else if minReact < 0 {
		minReact = 0
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	w := c.out

	c.drain()
	fmt.Fprint(w, RenderConfirm(req))
	read := func(prompt string) (reply, string) {
		fmt.Fprint(w, prompt)
		shown := c.now()
		for {
			select {
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return replyTimeout, ""
				}
				return replyCanceled, ""
			case <-timer.C:
				return replyTimeout, ""
			case l, ok := <-c.lines:
				if !ok || l.eof {
					return replyEOF, ""
				}
				if l.at.Sub(shown) < minReact {
					continue // typed ahead, before the question could be read
				}
				return replyLine, strings.TrimSpace(strings.TrimRight(l.s, "\r\n"))
			}
		}
	}
	fail := func(r reply) (bool, error) {
		switch r {
		case replyTimeout:
			fmt.Fprintln(w, "\nNo answer in time. Nothing was deleted.")
			return false, session.ErrConfirmTimeout
		case replyCanceled:
			fmt.Fprintln(w, "\nThe request was withdrawn. Nothing was deleted.")
			return false, ctx.Err()
		}
		fmt.Fprintln(w, "\nDeclined. Nothing was deleted.")
		return false, nil
	}

	prompt := "Approve? Type y and Enter, or just Enter to decline [y/N]: "
	for {
		r, s := read(prompt)
		if r != replyLine {
			return fail(r)
		}
		switch strings.ToLower(s) {
		case "y", "yes":
		case "", "n", "no":
			fmt.Fprintln(w, "Declined. Nothing was deleted.")
			return false, nil
		default:
			prompt = "Please answer y or n [y/N]: "
			continue
		}
		break
	}
	if req.Large {
		fmt.Fprintf(w, "\nThis permanently deletes %s%s in %s files. It can't be undone.\n",
			atLeast(req.AtLeast), FormatBytes(req.Alloc), FormatCount(req.Files))
		r, s := read("Type delete to confirm, anything else cancels: ")
		if r != replyLine {
			return fail(r)
		}
		if s != "delete" {
			fmt.Fprintln(w, "Canceled. Nothing was deleted.")
			return false, nil
		}
	}
	fmt.Fprintln(w, "Approved.")
	return true, nil
}

func atLeast(b bool) string {
	if b {
		return "at least "
	}
	return ""
}

func plural(n uint64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return FormatCount(n) + " " + many
}

// RenderConfirm is the prompt's text (everything but the question line).
// Paths and warnings arrive display-safe from the session.
func RenderConfirm(req session.ConfirmRequest) string {
	var b strings.Builder
	n := uint64(req.Count)
	b.WriteString("\n──── Deletion request ────\n")
	if req.Mode == "permanent" {
		fmt.Fprintf(&b, "A browser asks to DELETE %s PERMANENTLY (this can't be undone):\n", plural(n, "item", "items"))
	} else {
		fmt.Fprintf(&b, "A browser asks to move %s to the %s:\n", plural(n, "item", "items"), req.Trash)
	}
	for _, it := range req.Items {
		size := atLeastMark(it.AtLeast) + FormatBytes(it.Alloc)
		detail := ""
		if it.Dir {
			detail = "  (folder"
			if it.Files > 0 || !it.AtLeast {
				detail += ", " + plural(it.Files, "file", "files")
			}
			detail += ")"
		}
		fmt.Fprintf(&b, "  %10s  %s%s\n", size, it.Path, detail)
	}
	if req.More > 0 {
		fmt.Fprintf(&b, "  … and %d more\n", req.More)
	}
	fmt.Fprintf(&b, "Total: %s%s in %s and %s",
		atLeast(req.AtLeast), FormatBytes(req.Alloc), plural(req.Files, "file", "files"), plural(req.Dirs, "folder", "folders"))
	if req.AtLeast {
		b.WriteString(" (measuring stopped early)")
	}
	b.WriteString("\n")
	for _, w := range req.Warnings {
		b.WriteString("! " + w + "\n")
	}
	via := "on this computer (local link)"
	if req.ViaRelay {
		via = "through the relay"
	}
	fmt.Fprintf(&b, "Requested by the browser showing the code %s, connected %s.\n", req.Code, via)
	b.WriteString("If that isn't you, or you didn't ask for this, decline.\n")
	return b.String()
}

func atLeastMark(b bool) string {
	if b {
		return "≥"
	}
	return ""
}
