// SPDX-License-Identifier: MIT

// Package cli renders the agent's terminal UI: header, secure link (with a
// compact QR code on capable terminals), elevation status and live status
// lines. Nothing secret is written except the link itself, printed once.
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/mdp/qrterminal/v3"
	"golang.org/x/term"

	"github.com/zyno-io/diskspace.cc/internal/session"
)

// Options configures a Renderer.
type Options struct {
	Out, Err  io.Writer
	TTY       bool // Out is an interactive terminal
	NoColor   bool
	Quiet     bool // only the link
	Version   string
	Elevated  bool
	User      string // current user name (non-elevated line)
	FDADenied bool
	GOOS      string
	// TermSize returns terminal columns/rows (0,0 if unknown).
	TermSize func() (int, int)
}

// Renderer prints events. It is safe for concurrent use.
type Renderer struct {
	o           Options
	mu          sync.Mutex
	status      string // current in-place status line (TTY)
	live        bool   // a status line is currently drawn
	linked      bool
	relayWarned bool
	scanFiles   uint64
	lastLink    string // URL+local last printed; relay reconnects re-emit the same link
	held        int    // Hold depth: a prompt owns the terminal
	pending     []heldLine
}

type heldLine struct {
	w io.Writer
	s string
}

// New creates a Renderer.
func New(o Options) *Renderer {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Err == nil {
		o.Err = os.Stderr
	}
	if os.Getenv("NO_COLOR") != "" {
		o.NoColor = true
	}
	return &Renderer{o: o}
}

// IsTerminal reports whether f is an interactive terminal.
func IsTerminal(f *os.File) bool { return f != nil && term.IsTerminal(int(f.Fd())) }

// StdoutSize returns the stdout terminal size, or zeros.
func StdoutSize() (int, int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 0, 0
	}
	return w, h
}

func (r *Renderer) style(code, s string) string {
	if !r.o.TTY || r.o.NoColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Hold pauses all output (the status line is cleared and later lines are
// kept) until the returned release is called, so a prompt owns the terminal.
func (r *Renderer) Hold() (release func()) {
	r.mu.Lock()
	r.clearLocked()
	r.held++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.held--; r.held > 0 {
				return
			}
			p := r.pending
			r.pending = nil
			for _, l := range p {
				fmt.Fprintln(l.w, l.s)
			}
			if r.o.TTY {
				r.drawLocked()
			}
		})
	}
}

// printlnLocked writes a permanent line now, or after the hold ends.
func (r *Renderer) printlnLocked(w io.Writer, s string) {
	if r.held > 0 {
		r.pending = append(r.pending, heldLine{w, s})
		return
	}
	fmt.Fprintln(w, s)
}

func (r *Renderer) clearLocked() {
	if r.live {
		fmt.Fprint(r.o.Out, "\r\x1b[2K")
		r.live = false
	}
}

// line prints a permanent line, preserving the in-place status below it.
func (r *Renderer) lineLocked(w io.Writer, s string) {
	r.clearLocked()
	r.printlnLocked(w, s)
	if r.o.TTY && r.status != "" && w == r.o.Out {
		r.drawLocked()
	}
}

func (r *Renderer) drawLocked() {
	if r.status == "" || r.held > 0 {
		return
	}
	fmt.Fprint(r.o.Out, "\r\x1b[2K"+r.status)
	r.live = true
}

// setStatus shows a status: in place on a TTY, an appended line otherwise.
func (r *Renderer) setStatusLocked(s string, persist bool) {
	if r.o.Quiet {
		return
	}
	if !r.o.TTY {
		if persist {
			r.printlnLocked(r.o.Out, s)
		}
		return
	}
	r.status = s
	r.drawLocked()
}

// Header prints "diskspace.cc <version>".
func (r *Renderer) Header() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.o.Quiet {
		return
	}
	v := ""
	if r.o.Version != "" {
		v = " " + r.style("2", r.o.Version)
	}
	fmt.Fprintln(r.o.Out, r.style("1", "diskspace.cc")+v)
}

// Link prints the link block. url is the relay link (may be empty), local the
// 127.0.0.1 link (may be empty).
func (r *Renderer) Link(url, local string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearLocked()
	if r.o.Quiet {
		if url != "" {
			fmt.Fprintln(r.o.Out, url)
		} else {
			fmt.Fprintln(r.o.Out, local)
		}
		return
	}
	first := !r.linked
	r.linked = true
	w := r.o.Out
	fmt.Fprintln(w)
	if url != "" {
		if first {
			fmt.Fprintln(w, "Open this link in your browser:")
		} else {
			fmt.Fprintln(w, "New link:")
		}
		fmt.Fprintln(w, "  "+r.style("1;4", url))
		if r.o.TTY && r.qrFits() {
			fmt.Fprintln(w)
			qrterminal.GenerateHalfBlock(url, qrterminal.L, w)
		}
	}
	if local != "" {
		if url == "" {
			fmt.Fprintln(w, "Open this link in your browser (this computer only):")
			fmt.Fprintln(w, "  "+r.style("1;4", local))
		} else {
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Local link (this computer only):")
			fmt.Fprintln(w, "  "+local)
		}
	}
	fmt.Fprintln(w)
	if first {
		if r.o.Elevated {
			fmt.Fprintln(w, "Running as administrator — anyone with this link can see the names of all files on this computer.")
		} else {
			fmt.Fprintf(w, "Running as %s (standard user)\n", r.o.User)
		}
		if r.o.GOOS == "darwin" && r.o.FDADenied {
			fmt.Fprintln(w, r.style("2", "Full Disk Access is not granted to this terminal; some folders will show as \"no access\"."))
			fmt.Fprintln(w, r.style("2", "Grant it in System Settings > Privacy & Security > Full Disk Access, then restart the terminal."))
		}
		fmt.Fprintln(w)
	}
	if r.status == "" {
		r.setStatusLocked("Waiting for the browser to connect…  (Ctrl-C to quit)", true)
	} else {
		r.drawLocked()
	}
}

func (r *Renderer) qrFits() bool {
	if r.o.TermSize == nil {
		return false
	}
	w, h := r.o.TermSize()
	return w >= 60 && h >= 36
}

// Info prints a permanent informational line (stdout).
func (r *Renderer) Info(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.o.Quiet {
		return
	}
	r.lineLocked(r.o.Out, s)
}

// Warn prints a permanent line on stderr.
func (r *Renderer) Warn(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lineLocked(r.o.Err, s)
}

// Close finishes the live status line.
func (r *Renderer) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live {
		fmt.Fprintln(r.o.Out)
		r.live = false
	}
	r.status = ""
}

// FormatCount renders 1234567 as "1.2M".
func FormatCount(n uint64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fK", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

// FormatBytes renders a byte count with decimal units.
func FormatBytes(n uint64) string {
	const u = 1000.0
	f := float64(n)
	for _, s := range []string{"B", "KB", "MB", "GB", "TB"} {
		if f < u || s == "TB" {
			if s == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", f, s)
		}
		f /= u
	}
	return ""
}

// Event renders one session event.
func (r *Renderer) Event(ev session.Event) {
	switch ev.Kind {
	case session.EvLinkReady:
		r.mu.Lock()
		same := r.lastLink == ev.URL+"\x00"+ev.LocalURL
		r.lastLink = ev.URL + "\x00" + ev.LocalURL
		r.mu.Unlock()
		if same {
			r.Info("Reconnected to the relay.")
			return
		}
		r.Link(ev.URL, ev.LocalURL)
	case session.EvRotated:
		r.Info("Link rotated.")
	case session.EvViewerConnected:
		r.mu.Lock()
		r.setStatusLocked("Viewer connected · verification code "+ev.Code, true)
		r.mu.Unlock()
	case session.EvViewerDisconnected:
		r.mu.Lock()
		r.setStatusLocked("Viewer disconnected · waiting for the browser to connect…", true)
		r.mu.Unlock()
	case session.EvViewerSuperseded:
		r.Info("Another browser took over this session.")
	case session.EvScanStarted:
		r.mu.Lock()
		r.scanFiles = 0
		r.setStatusLocked("Scanning "+ev.Path+" …", true)
		r.mu.Unlock()
	case session.EvScanProgress:
		if ev.Totals != nil {
			r.mu.Lock()
			if r.o.TTY {
				r.setStatusLocked("Scanning "+ev.Path+" … "+FormatCount(ev.Totals.Files)+" files", false)
			}
			r.mu.Unlock()
		}
	case session.EvScanPaused:
		msg := "Paused · " + ev.Path
		if ev.Totals != nil {
			msg += " · " + FormatCount(ev.Totals.Files) + " files"
		}
		msg += " (resume in the browser)"
		r.mu.Lock()
		r.setStatusLocked(msg, true)
		r.mu.Unlock()
	case session.EvScanResumed:
		r.mu.Lock()
		r.setStatusLocked("Scanning "+ev.Path+" …", true)
		r.mu.Unlock()
	case session.EvScanDone:
		msg := fmt.Sprintf("Scan done in %.1f s", float64(ev.Ms)/1000)
		if ev.Totals != nil {
			msg += fmt.Sprintf(" · %s files · %s", FormatCount(ev.Totals.Files), FormatBytes(ev.Totals.Alloc))
		}
		r.mu.Lock()
		r.setStatusLocked(msg, true)
		r.mu.Unlock()
	case session.EvScanCanceled:
		r.mu.Lock()
		r.setStatusLocked("Scan canceled", true)
		r.mu.Unlock()
	case session.EvScanError:
		r.Warn("Scan failed: " + ev.Message)
	case session.EvRelayState:
		r.mu.Lock()
		switch {
		case ev.State == "reconnecting" && ev.Err != nil && !r.relayWarned:
			r.relayWarned = true
			r.lineLocked(r.o.Err, "Cannot reach the relay ("+briefErr(ev.Err)+"); retrying…")
		case ev.State == "connected" && r.relayWarned:
			r.relayWarned = false
			r.lineLocked(r.o.Out, "Relay connection restored.")
		}
		r.mu.Unlock()
	case session.EvDeleteStarted:
		r.mu.Lock()
		r.setStatusLocked(ev.Message, true)
		r.mu.Unlock()
	case session.EvDeleteItem:
		// the audit trail: one permanent line per requested item, even with --quiet
		r.mu.Lock()
		r.lineLocked(r.o.Out, "Deletion · "+ev.Path+" · "+ev.Message)
		r.mu.Unlock()
	case session.EvDeleteDone:
		r.mu.Lock()
		r.lineLocked(r.o.Out, ev.Message)
		r.mu.Unlock()
	case session.EvIdle:
		r.Info("No viewer connected for a while — exiting.")
	case session.EvError:
		msg := ev.Message
		if msg == "" && ev.Err != nil {
			msg = ev.Err.Error()
		}
		r.Warn("Error: " + msg)
	}
}

func briefErr(err error) string {
	s := err.Error()
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
