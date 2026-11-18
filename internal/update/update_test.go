// SPDX-License-Identifier: MIT

package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		have, latest string
		want         bool
	}{
		{"0.1.1", "0.1.2", true},
		{"v0.1.1", "v0.1.2", true},
		{"0.1.2", "0.1.2", false},
		{"0.1.3", "0.1.2", false},
		{"0.9.0", "0.10.0", true},
		{"1.0.0", "0.99.9", false},
		{"dev", "0.1.2", false},
		{"", "0.1.2", false},
		{"0.1.2-rc1", "0.1.2", false},
		{"0.1.1-rc1", "0.1.2", true},
		{"0.1.1", "", false},
		{"0.1.1", "garbage", false},
	}
	for _, c := range cases {
		if got := Newer(c.have, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.have, c.latest, got, c.want)
		}
	}
}

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/latest" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"version":"0.1.2"}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	if got := Check(ctx, srv.URL, "0.1.1"); got != "0.1.2" {
		t.Fatalf("Check = %q", got)
	}
	if got := Check(ctx, srv.URL, "0.1.2"); got != "" {
		t.Fatalf("up to date: Check = %q", got)
	}
	if got := Check(ctx, srv.URL, "dev"); got != "" {
		t.Fatalf("dev build: Check = %q", got)
	}
	if got := Check(ctx, "http://127.0.0.1:1", "0.1.1"); got != "" {
		t.Fatalf("unreachable relay: Check = %q", got)
	}
}
