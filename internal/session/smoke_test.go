// SPDX-License-Identifier: MIT

package session

import (
	"net/url"
	"os"
	"testing"
)

// TestSmokeURL drives a running testharness: DISKSPACE_SMOKE_URL is the link
// it printed (http://host/s/<sid>#<K>). The harness auto-scans, so this just
// connects and checks the scan streams to consistent totals.
func TestSmokeURL(t *testing.T) {
	link := os.Getenv("DISKSPACE_SMOKE_URL")
	if link == "" {
		t.Skip("DISKSPACE_SMOKE_URL not set")
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	sec := parseLink(t, link)
	v := connectViewer(t, "ws://"+u.Host, sec)
	info := v.handshakeMsgs()
	t.Logf("agent %s/%s autoScan=%v scan=%v", info.OS, info.Arch, info.AutoScan != nil, info.Scan)
	r := v.collect()
	if r.Done == nil {
		t.Fatalf("scan did not complete: %+v", r.Ended)
	}
	if got := r.sums(); got != r.Done.Totals {
		t.Fatalf("sums %+v != done %+v", got, r.Done.Totals)
	}
	t.Logf("smoke ok: %d NODES messages, %d dirs, %d files, %d bytes allocated, %d ms",
		r.Nodes, r.Done.Totals.Dirs, r.Done.Totals.Files, r.Done.Totals.Alloc, r.Done.Ms)
}
