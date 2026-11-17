// SPDX-License-Identifier: MIT

// Adapted from github.com/zyno-io/sp2p (MIT)

package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultGitHubRepo is the repository releases are resolved from.
const DefaultGitHubRepo = "zyno-io/diskspace.cc"

const (
	releaseCacheTTL    = 1 * time.Minute
	releaseHTTPTimeout = 10 * time.Second
)

type githubRelease struct {
	TagName    string        `json:"tag_name"`
	Prerelease bool          `json:"prerelease"`
	Draft      bool          `json:"draft"`
	Assets     []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type resolvedAsset struct {
	url     string
	version string // tag_name with "v" prefix stripped
}

// ReleaseResolver resolves asset download URLs by scanning the GitHub releases
// list (newest first; drafts and prereleases skipped). Results are cached for
// a minute; if a refresh fails the previous cache is served stale.
type ReleaseResolver struct {
	repo   string
	client *http.Client
	apiURL string // overridable for testing

	mu         sync.RWMutex
	cache      map[string]resolvedAsset
	fetchedAt  time.Time
	refreshing bool

	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewReleaseResolver creates a resolver for repo ("owner/name").
func NewReleaseResolver(repo string) *ReleaseResolver {
	return &ReleaseResolver{
		repo:   repo,
		client: &http.Client{Timeout: releaseHTTPTimeout},
		apiURL: "https://api.github.com/repos/" + repo + "/releases",
	}
}

// Start begins background polling; the first fetch happens immediately.
func (r *ReleaseResolver) Start() {
	r.stopCh = make(chan struct{})
	go r.poll()
}

// Stop cancels background polling. Safe to call multiple times or without Start.
func (r *ReleaseResolver) Stop() {
	r.stopOnce.Do(func() {
		if r.stopCh != nil {
			close(r.stopCh)
		}
	})
}

func (r *ReleaseResolver) poll() {
	ctx, cancel := context.WithTimeout(context.Background(), releaseHTTPTimeout)
	if err := r.refresh(ctx); err != nil {
		slog.Warn("release resolver: initial refresh failed", "error", err)
	}
	cancel()
	t := time.NewTicker(releaseCacheTTL)
	defer t.Stop()
	for {
		select {
		case <-r.stopCh:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), releaseHTTPTimeout)
			if err := r.refresh(ctx); err != nil {
				slog.Warn("release resolver: periodic refresh failed", "error", err)
			}
			cancel()
		}
	}
}

// ResolveAssetURL returns the download URL of the newest stable release asset
// named assetName. When the asset is unknown (or nothing was ever fetched) it
// returns the moving /releases/latest/download/ URL.
func (r *ReleaseResolver) ResolveAssetURL(ctx context.Context, assetName string) (string, error) {
	r.mu.RLock()
	if r.cache != nil && time.Since(r.fetchedAt) < releaseCacheTTL {
		a, ok := r.cache[assetName]
		r.mu.RUnlock()
		if ok {
			return a.url, nil
		}
		return r.fallbackURL(assetName), nil
	}
	r.mu.RUnlock()

	if err := r.refresh(ctx); err != nil {
		slog.Warn("release resolver: refresh failed", "error", err)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if a, ok := r.cache[assetName]; ok {
		return a.url, nil
	}
	return r.fallbackURL(assetName), nil
}

// LatestVersion returns the version (without "v") of the newest release that
// carries assetName, or "".
func (r *ReleaseResolver) LatestVersion(assetName string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cache[assetName].version
}

func (r *ReleaseResolver) refresh(ctx context.Context) error {
	r.mu.Lock()
	if r.refreshing {
		r.mu.Unlock()
		return nil
	}
	r.refreshing = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.refreshing = false
		r.mu.Unlock()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.apiURL+"?per_page=100", nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github API returned %d", resp.StatusCode)
	}
	var releases []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return fmt.Errorf("decode releases: %w", err)
	}
	cache := make(map[string]resolvedAsset)
	for _, rel := range releases { // newest first; first occurrence wins
		if rel.Draft || rel.Prerelease {
			continue
		}
		version := strings.TrimPrefix(rel.TagName, "v")
		for _, a := range rel.Assets {
			if _, ok := cache[a.Name]; !ok {
				cache[a.Name] = resolvedAsset{url: a.BrowserDownloadURL, version: version}
			}
		}
	}
	r.mu.Lock()
	r.cache = cache
	r.fetchedAt = time.Now()
	r.mu.Unlock()
	slog.Info("release resolver: cache refreshed", "assets", len(cache))
	return nil
}

func (r *ReleaseResolver) fallbackURL(assetName string) string {
	return fmt.Sprintf("https://github.com/%s/releases/latest/download/%s", r.repo, assetName)
}
