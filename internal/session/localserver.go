// SPDX-License-Identifier: MIT

package session

import (
	"bytes"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/coder/websocket"

	diskspace "github.com/zyno-io/diskspace.cc"
)

// startLocal serves the embedded SPA and /v1/view on 127.0.0.1:<random port>.
// The Host header must equal 127.0.0.1:<port> and a present Origin must equal
// http://127.0.0.1:<port> (defeats DNS rebinding and cross-site sockets).
func (a *Agent) startLocal() (*http.Server, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	a.setLocalAddr(ln)
	addr := ln.Addr().String()
	static := diskspace.WebFS()
	csp := "default-src 'self'; connect-src 'self' ws://" + addr +
		"; worker-src 'self'; img-src 'self' data: blob:; frame-ancestors 'none'; base-uri 'none'"

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/view", func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+addr {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		g := a.curGen()
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		if r.URL.Query().Get("sid") != g.sec.SIDString() {
			_ = ws.Close(closeUnknownSID, "unknown session")
			return
		}
		a.serveViewer(ws, g, false)
	})
	serve := func(w http.ResponseWriter, r *http.Request, name string, html bool) {
		data, err := fs.ReadFile(static, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		if html {
			h.Set("Content-Security-Policy", csp)
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Cache-Control", "no-store")
		} else if strings.HasPrefix(name, "assets/") {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "public, max-age=3600")
		}
		http.ServeContent(w, r, path.Base(name), time.Time{}, bytes.NewReader(data))
	}
	mux.HandleFunc("GET /s/{sid}", func(w http.ResponseWriter, r *http.Request) {
		if len(r.PathValue("sid")) != 22 {
			http.NotFound(w, r)
			return
		}
		serve(w, r, "viewer.html", true)
	})
	mux.HandleFunc("GET /assets/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if !fs.ValidPath(name) || strings.HasSuffix(name, "/") {
			http.NotFound(w, r)
			return
		}
		serve(w, r, name, false)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := r.URL.Path
		switch {
		case p == "/":
			serve(w, r, "index.html", true)
		case p == "/launcher":
			serve(w, r, "launcher.html", true)
		default:
			name := strings.TrimPrefix(p, "/")
			if !fs.ValidPath(name) || strings.Contains(name, "/") || strings.HasSuffix(name, ".html") {
				http.NotFound(w, r)
				return
			}
			serve(w, r, name, false)
		}
	})
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != addr {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			mux.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	return srv, nil
}
