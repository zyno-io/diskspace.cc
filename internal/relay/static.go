// SPDX-License-Identifier: MIT

// Adapted from github.com/zyno-io/sp2p (MIT)

package relay

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// buildCSP returns the viewer-page Content-Security-Policy. Note: style-src is
// covered by default-src 'self'; the built SPA must not use inline <style> or
// style="" attributes (CSSOM element.style assignments are fine).
func buildCSP(publicURL string, dev bool) string {
	connect := "'self'"
	if u, err := url.Parse(publicURL); err == nil && u.Host != "" {
		scheme := "wss"
		if u.Scheme == "http" {
			scheme = "ws"
		}
		connect += " " + scheme + "://" + u.Host
	}
	if dev {
		connect += " ws://localhost:* ws://127.0.0.1:*"
	}
	return "default-src 'self'; connect-src " + connect +
		"; worker-src 'self'; img-src 'self' data: blob:; frame-ancestors 'none'; base-uri 'none'"
}

func (s *Server) setHTMLHeaders(h http.Header) {
	h.Set("Content-Security-Policy", s.csp)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
}

func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, name, cache string, html bool) {
	if s.cfg.Static == nil {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(s.cfg.Static, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	if html {
		s.setHTMLHeaders(h)
	} else {
		h.Set("Cache-Control", cache)
	}
	http.ServeContent(w, r, path.Base(name), time.Time{}, bytes.NewReader(data))
}

func validRelPath(p string) bool {
	return p != "" && fs.ValidPath(p) && !strings.Contains(p, "\\")
}

func (s *Server) handleViewerPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := parseSID(r.PathValue("sid")); !ok {
		http.NotFound(w, r)
		return
	}
	s.serveFile(w, r, "viewer.html", "", true)
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if !validRelPath(name) || strings.HasSuffix(name, "/") {
		http.NotFound(w, r)
		return
	}
	s.serveFile(w, r, name, "public, max-age=31536000, immutable", false)
}

// handleRoot serves "/" (index.html) and single-segment top-level files
// (favicon etc.); everything else is 404.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	switch {
	case p == "/":
		s.serveFile(w, r, "index.html", "", true)
	case p == "/launcher":
		s.serveFile(w, r, "launcher.html", "", true)
	default:
		name := strings.TrimPrefix(p, "/")
		if !validRelPath(name) || strings.Contains(name, "/") || strings.HasSuffix(name, ".html") {
			http.NotFound(w, r)
			return
		}
		s.serveFile(w, r, name, "public, max-age=3600", false)
	}
}
