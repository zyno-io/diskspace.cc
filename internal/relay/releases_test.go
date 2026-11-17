// SPDX-License-Identifier: MIT

package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const releasesJSON = `[
 {"tag_name":"v2.0.0-rc1","prerelease":true,"assets":[{"name":"diskspace_linux_amd64.tar.gz","browser_download_url":"https://github.com/zyno-io/diskspace.cc/releases/download/v2.0.0-rc1/diskspace_linux_amd64.tar.gz"}]},
 {"tag_name":"v1.9.0","draft":true,"assets":[{"name":"diskspace_linux_amd64.tar.gz","browser_download_url":"https://github.com/zyno-io/diskspace.cc/releases/download/v1.9.0/diskspace_linux_amd64.tar.gz"}]},
 {"tag_name":"v1.1.0","assets":[{"name":"diskspace_linux_amd64.tar.gz","browser_download_url":"https://github.com/zyno-io/diskspace.cc/releases/download/v1.1.0/diskspace_linux_amd64.tar.gz"}]},
 {"tag_name":"v1.0.0","assets":[
   {"name":"diskspace_linux_amd64.tar.gz","browser_download_url":"https://github.com/zyno-io/diskspace.cc/releases/download/v1.0.0/diskspace_linux_amd64.tar.gz"},
   {"name":"diskspace_linux_arm64.tar.gz","browser_download_url":"https://github.com/zyno-io/diskspace.cc/releases/download/v1.0.0/diskspace_linux_arm64.tar.gz"}]}
]`

func fakeGitHub(t *testing.T, hits *atomic.Int32, fail *atomic.Bool) *ReleaseResolver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(releasesJSON))
	}))
	t.Cleanup(srv.Close)
	r := NewReleaseResolver(DefaultGitHubRepo)
	r.apiURL = srv.URL
	return r
}

func TestResolverSkipsDraftAndPrerelease(t *testing.T) {
	var hits atomic.Int32
	var fail atomic.Bool
	r := fakeGitHub(t, &hits, &fail)
	ctx := context.Background()
	u, err := r.ResolveAssetURL(ctx, "diskspace_linux_amd64.tar.gz")
	if err != nil || u != "https://github.com/zyno-io/diskspace.cc/releases/download/v1.1.0/diskspace_linux_amd64.tar.gz" {
		t.Fatalf("got %q %v", u, err)
	}
	// arm64 only exists in an older release: still resolved from it.
	u, _ = r.ResolveAssetURL(ctx, "diskspace_linux_arm64.tar.gz")
	if u != "https://github.com/zyno-io/diskspace.cc/releases/download/v1.0.0/diskspace_linux_arm64.tar.gz" {
		t.Fatalf("got %q", u)
	}
	if v := r.LatestVersion("diskspace_linux_amd64.tar.gz"); v != "1.1.0" {
		t.Fatalf("version %q", v)
	}
	// Unknown asset falls back to the moving latest URL.
	u, _ = r.ResolveAssetURL(ctx, "diskspace_darwin_arm64.tar.gz")
	if u != "https://github.com/zyno-io/diskspace.cc/releases/latest/download/diskspace_darwin_arm64.tar.gz" {
		t.Fatalf("fallback %q", u)
	}
}

func TestResolverCacheAndStaleFallback(t *testing.T) {
	var hits atomic.Int32
	var fail atomic.Bool
	r := fakeGitHub(t, &hits, &fail)
	ctx := context.Background()
	const asset = "diskspace_linux_amd64.tar.gz"
	for range 3 {
		if _, err := r.ResolveAssetURL(ctx, asset); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 API hit within TTL, got %d", hits.Load())
	}
	// Expire the cache and make GitHub fail: stale data must still be served.
	r.mu.Lock()
	r.fetchedAt = time.Now().Add(-2 * releaseCacheTTL)
	r.mu.Unlock()
	fail.Store(true)
	u, err := r.ResolveAssetURL(ctx, asset)
	if err != nil || u != "https://github.com/zyno-io/diskspace.cc/releases/download/v1.1.0/"+asset {
		t.Fatalf("stale fallback failed: %q %v", u, err)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected refresh attempt, hits=%d", hits.Load())
	}
}

func TestResolverNoDataFallsBackToLatest(t *testing.T) {
	var hits atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	r := fakeGitHub(t, &hits, &fail)
	u, err := r.ResolveAssetURL(context.Background(), "diskspace_linux_amd64.tar.gz")
	if err != nil || u != "https://github.com/zyno-io/diskspace.cc/releases/latest/download/diskspace_linux_amd64.tar.gz" {
		t.Fatalf("got %q %v", u, err)
	}
}

func TestResolverStopWithoutStart(t *testing.T) {
	r := NewReleaseResolver(DefaultGitHubRepo)
	r.Stop()
	r.Stop()
}
