// SPDX-License-Identifier: MIT

package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/zyno-io/diskspace.cc/internal/proto"
	"github.com/zyno-io/diskspace.cc/internal/session"
)

func TestPlainRendering(t *testing.T) {
	var out, errb bytes.Buffer
	r := New(Options{Out: &out, Err: &errb, Version: "v1", User: "alex", GOOS: "linux", NoColor: true})
	r.Header()
	r.Event(session.Event{Kind: session.EvLinkReady, URL: "https://diskspace.cc/s/abc#key", LocalURL: "http://127.0.0.1:1/s/abc#key"})
	r.Event(session.Event{Kind: session.EvViewerConnected, Code: "123 456"})
	r.Event(session.Event{Kind: session.EvScanStarted, Path: "/x"})
	r.Event(session.Event{Kind: session.EvScanProgress, Path: "/x", Totals: &proto.ScanTotals{Files: 5}})
	r.Event(session.Event{Kind: session.EvScanDone, Ms: 14200, Totals: &proto.ScanTotals{Files: 1200000, Alloc: 5e9}})
	r.Close()
	s := out.String()
	for _, want := range []string{"diskspace.cc v1", "Open this link in your browser:", "https://diskspace.cc/s/abc#key",
		"Local link", "Running as alex (standard user)", "verification code 123 456", "Scanning /x", "Scan done in 14.2 s", "1.2M files"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "\x1b") || strings.Contains(s, "\r") {
		t.Errorf("escape codes in non-TTY output: %q", s)
	}
	if strings.Count(s, "https://diskspace.cc/s/abc#key") != 1 {
		t.Error("link must print once")
	}
}

func TestElevatedWarningAndTTY(t *testing.T) {
	var out bytes.Buffer
	r := New(Options{Out: &out, Err: &out, TTY: true, NoColor: true, Elevated: true})
	r.Event(session.Event{Kind: session.EvLinkReady, URL: "https://x/s/a#b"})
	if !strings.Contains(out.String(), "anyone with this link can see the names of all files") {
		t.Fatalf("no admin warning: %q", out.String())
	}
	if !strings.Contains(out.String(), "\x1b[2K") {
		t.Fatal("TTY mode should redraw in place")
	}
}

func TestQuiet(t *testing.T) {
	var out bytes.Buffer
	r := New(Options{Out: &out, Err: &out, Quiet: true})
	r.Header()
	r.Event(session.Event{Kind: session.EvLinkReady, URL: "https://x/s/a#b"})
	r.Event(session.Event{Kind: session.EvViewerConnected, Code: "1"})
	if out.String() != "https://x/s/a#b\n" {
		t.Fatalf("%q", out.String())
	}
}

func TestFormat(t *testing.T) {
	if FormatCount(1234567) != "1.2M" || FormatCount(999) != "999" || FormatBytes(1500) != "1.5 KB" {
		t.Fatal(FormatCount(1234567), FormatBytes(1500))
	}
}
