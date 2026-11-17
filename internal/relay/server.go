// SPDX-License-Identifier: MIT

// Adapted from github.com/zyno-io/sp2p (MIT)

package relay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Config configures a relay Server.
type Config struct {
	Listen     string // informational; the caller owns the listener
	PublicURL  string // e.g. https://diskspace.cc; its wss origin goes into CSP connect-src
	Dev        bool
	TrustProxy bool // use X-Real-IP / X-Forwarded-For for the client IP
	Static     fs.FS
	// Extra mounts additional handlers by mux pattern path, e.g. "/run",
	// "/run.ps1", "/dl/".
	Extra   map[string]http.Handler
	Version string
	Limits  Limits
	Logger  *slog.Logger
}

// Server is the relay.
type Server struct {
	cfg Config
	lim Limits
	csp string
	log *slog.Logger
	mux *http.ServeMux

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	upgrades *slidingWindow

	mu         sync.Mutex
	closed     bool
	sessions   map[string]*session
	ipSessions map[string]int
}

// New creates a relay server.
func New(cfg Config) *Server {
	s := &Server{
		cfg:        cfg,
		lim:        cfg.Limits.withDefaults(),
		csp:        buildCSP(cfg.PublicURL, cfg.Dev),
		log:        cfg.Logger,
		mux:        http.NewServeMux(),
		sessions:   map[string]*session{},
		ipSessions: map[string]int{},
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.upgrades = newSlidingWindow(s.lim.UpgradesPerMinPerIP, s.lim.Window)

	s.mux.HandleFunc("/v1/agent", s.handleAgent)
	s.mux.HandleFunc("/v1/view", s.handleView)
	s.mux.HandleFunc("/v1/agent/data", s.handleAgentData)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})
	s.mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(cfg.Version + "\n"))
	})
	s.mux.HandleFunc("GET /s/{sid}", s.handleViewerPage)
	s.mux.HandleFunc("GET /assets/", s.handleAssets)
	for p, h := range cfg.Extra {
		s.mux.Handle(p, h)
	}
	s.mux.HandleFunc("/", s.handleRoot)

	s.wg.Add(1)
	go s.gcLoop()
	return s
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		s.mux.ServeHTTP(w, r)
	})
}

// Shutdown closes all sessions and waits for handlers to finish or ctx to end.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	var all []*session
	for _, sess := range s.sessions {
		all = append(all, sess)
	}
	s.mu.Unlock()
	for _, sess := range all {
		s.deleteSession(sess, websocket.StatusGoingAway, "server shutting down")
	}
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) gcLoop() {
	defer s.wg.Done()
	t := time.NewTicker(s.lim.Window)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.upgrades.gc()
		case <-s.ctx.Done():
			return
		}
	}
}

// track registers an in-flight handler; false means the server is closed.
func (s *Server) track() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.wg.Add(1)
	return true
}

// go runs f in a tracked goroutine.
func (s *Server) goTracked(f func()) {
	s.wg.Add(1)
	go func() { defer s.wg.Done(); f() }()
}

func (s *Server) acceptOpts() *websocket.AcceptOptions {
	o := &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled}
	if s.cfg.Dev {
		o.OriginPatterns = []string{"localhost:*", "127.0.0.1:*"}
	}
	if u, err := url.Parse(s.cfg.PublicURL); err == nil && u.Host != "" {
		o.OriginPatterns = append(o.OriginPatterns, u.Host)
	}
	return o
}

// accept upgrades the request; on failure the response is already written.
func (s *Server) accept(w http.ResponseWriter, r *http.Request) *websocket.Conn {
	c, err := websocket.Accept(w, r, s.acceptOpts())
	if err != nil {
		return nil
	}
	return c
}

func closeConn(c *websocket.Conn, code int, reason string) {
	_ = c.Close(websocket.StatusCode(code), reason)
}

// parseSID validates a 22-char base64url (16 byte) sid.
func parseSID(sid string) ([]byte, bool) {
	if len(sid) != 22 {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(sid)
	if err != nil || len(b) != 16 || base64.RawURLEncoding.EncodeToString(b) != sid {
		return nil, false
	}
	return b, true
}

func bearerHash(r *http.Request) ([32]byte, bool) {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return [32]byte{}, false
	}
	tok := strings.TrimSpace(h[len(p):])
	if tok == "" || len(tok) > 256 {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(tok)), true
}

func hashEqual(a, b [32]byte) bool { return subtle.ConstantTimeCompare(a[:], b[:]) == 1 }

func newCID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func short(sid string) string {
	if len(sid) > 4 {
		return sid[:4]
	}
	return sid
}
