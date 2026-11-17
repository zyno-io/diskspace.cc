// SPDX-License-Identifier: MIT

// Package update checks whether a newer stable release exists, using the
// relay's GET /v1/latest. It only informs; nothing is ever downloaded.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Timeout bounds one check.
const Timeout = 3 * time.Second

// InstallURL is where the CLI's update instructions live.
const InstallURL = "https://diskspace.cc/#install"

// parse returns the numeric major.minor.patch of v ("v0.1.2", "0.1.2-rc1" and
// "0.1.2+build" all give 0, 1, 2). Anything else (including "dev") is not a
// release version.
func parse(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	var out [3]int
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Newer reports whether latest is a higher release than have. Dev builds and
// unparseable versions never count as outdated, and a prerelease of the latest
// version is not older than it.
func Newer(have, latest string) bool {
	h, ok := parse(have)
	if !ok {
		return false
	}
	l, ok := parse(latest)
	if !ok {
		return false
	}
	for i := range h {
		if l[i] != h[i] {
			return l[i] > h[i]
		}
	}
	return false
}

// Latest fetches the newest stable version (without "v") from the relay at
// base. An empty string means the relay does not know yet.
func Latest(ctx context.Context, client *http.Client, base string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/latest", nil)
	if err != nil {
		return "", err
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("update check: status %d", resp.StatusCode)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&v); err != nil {
		return "", err
	}
	return strings.TrimPrefix(v.Version, "v"), nil
}

// Check returns the latest version if it is newer than have, else "".
func Check(ctx context.Context, base, have string) string {
	if _, ok := parse(have); !ok || base == "" {
		return ""
	}
	l, err := Latest(ctx, nil, base)
	if err != nil || !Newer(have, l) {
		return ""
	}
	return l
}
