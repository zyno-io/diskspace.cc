// SPDX-License-Identifier: MIT

// Adapted from github.com/zyno-io/sp2p (MIT)

package relay

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type pendingViewer struct {
	conn    *websocket.Conn
	claimed chan struct{} // closed when the agent data socket claims it
	done    chan struct{} // closed when the splice ends
}

type session struct {
	sid     string
	hash    [32]byte
	ip      string
	created time.Time

	ctx    context.Context
	cancel context.CancelFunc

	viewerRate *slidingWindow
	bucket     *bucket

	mu      sync.Mutex
	dead    bool
	control *websocket.Conn
	grace   *time.Timer
	ttl     *time.Timer
	pending map[string]*pendingViewer
	conns   map[*websocket.Conn]struct{}
	pairs   int
}

func (s *Server) rejectWS(w http.ResponseWriter, r *http.Request, code int, reason string) {
	if c := s.accept(w, r); c != nil {
		closeConn(c, code, reason)
	}
}

func (s *Server) getSession(sid string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[sid]
}

// deleteLocked removes sess; caller holds s.mu and sess.mu. Returns conns to close.
func (s *Server) deleteLocked(sess *session) []*websocket.Conn {
	if sess.dead {
		return nil
	}
	sess.dead = true
	if s.sessions[sess.sid] == sess {
		delete(s.sessions, sess.sid)
		if s.ipSessions[sess.ip]--; s.ipSessions[sess.ip] <= 0 {
			delete(s.ipSessions, sess.ip)
		}
	}
	if sess.grace != nil {
		sess.grace.Stop()
	}
	if sess.ttl != nil {
		sess.ttl.Stop()
	}
	var cs []*websocket.Conn
	if sess.control != nil {
		cs = append(cs, sess.control)
		sess.control = nil
	}
	for _, p := range sess.pending {
		cs = append(cs, p.conn)
	}
	sess.pending = map[string]*pendingViewer{}
	for c := range sess.conns {
		cs = append(cs, c)
	}
	sess.cancel()
	return cs
}

func (s *Server) closeAll(cs []*websocket.Conn, code websocket.StatusCode, reason string) {
	for _, c := range cs {
		s.goTracked(func() { _ = c.Close(code, reason) })
	}
}

func (s *Server) deleteSession(sess *session, code websocket.StatusCode, reason string) {
	s.mu.Lock()
	sess.mu.Lock()
	cs := s.deleteLocked(sess)
	sess.mu.Unlock()
	s.mu.Unlock()
	s.closeAll(cs, code, reason)
}

func (sess *session) sendControl(ctx context.Context, m ControlMsg) error {
	sess.mu.Lock()
	c := sess.control
	sess.mu.Unlock()
	if c == nil {
		return context.Canceled
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.Write(ctx, websocket.MessageText, m.bytes())
}

func (s *Server) allowUpgrade(r *http.Request) (string, bool) {
	ip := clientIP(r, s.cfg.TrustProxy)
	return ip, s.upgrades.allow(ip)
}

// handleAgent is the agent control channel.
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	if !s.track() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	defer s.wg.Done()
	sid := r.URL.Query().Get("sid")
	if _, ok := parseSID(sid); !ok {
		http.Error(w, "bad sid", http.StatusBadRequest)
		return
	}
	ip, ok := s.allowUpgrade(r)
	if !ok {
		s.rejectWS(w, r, CloseRateLimit, "rate limited")
		return
	}
	hash, ok := bearerHash(r)
	if !ok {
		s.rejectWS(w, r, CloseBadToken, "bad token")
		return
	}
	conn := s.accept(w, r)
	if conn == nil {
		return
	}
	conn.SetReadLimit(s.lim.MaxMsgCtl)

	// Find or create the session.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		closeConn(conn, int(websocket.StatusGoingAway), "shutting down")
		return
	}
	sess := s.sessions[sid]
	var old *websocket.Conn
	if sess != nil {
		if !hashEqual(sess.hash, hash) {
			s.mu.Unlock()
			closeConn(conn, CloseSIDClaimed, "sid claimed")
			return
		}
		sess.mu.Lock()
		old = sess.control
		sess.control = conn
		if sess.grace != nil {
			sess.grace.Stop()
			sess.grace = nil
		}
		sess.mu.Unlock()
	} else {
		if s.ipSessions[ip] >= s.lim.SessionsPerIP {
			s.mu.Unlock()
			closeConn(conn, CloseRateLimit, "too many sessions")
			return
		}
		ctx, cancel := context.WithCancel(s.ctx)
		sess = &session{
			sid: sid, hash: hash, ip: ip, created: time.Now(),
			ctx: ctx, cancel: cancel,
			viewerRate: newSlidingWindow(s.lim.ViewerConnsPerMinSess, s.lim.Window),
			bucket:     newBucket(s.lim.BytesPerSec),
			control:    conn,
			pending:    map[string]*pendingViewer{},
			conns:      map[*websocket.Conn]struct{}{},
		}
		sess.ttl = time.AfterFunc(s.lim.TTL, func() {
			s.deleteSession(sess, CloseTTL, "session expired")
		})
		s.sessions[sid] = sess
		s.ipSessions[ip]++
	}
	s.mu.Unlock()
	if old != nil {
		s.goTracked(func() { _ = old.Close(websocket.StatusPolicyViolation, "replaced") })
	}
	s.log.Info("agent registered", "sid", short(sid))

	ttl := int((s.lim.TTL - time.Since(sess.created) + time.Second - 1) / time.Second)
	if ttl < 0 {
		ttl = 0
	}
	wctx, wcancel := context.WithTimeout(sess.ctx, 5*time.Second)
	err := conn.Write(wctx, websocket.MessageText, Registered(ttl).bytes())
	wcancel()
	if err != nil {
		conn.CloseNow()
	}

	// Pinger.
	pctx, pcancel := context.WithCancel(sess.ctx)
	pdone := make(chan struct{})
	go func() {
		defer close(pdone)
		t := time.NewTicker(s.lim.PingInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				c, cc := context.WithTimeout(pctx, s.lim.PingInterval)
				err := conn.Ping(c)
				cc()
				if err != nil {
					conn.CloseNow()
					return
				}
			case <-pctx.Done():
				return
			}
		}
	}()

	// Read loop: agent sends nothing meaningful, but we must read for pings/close.
	var rerr error
	for {
		if _, _, rerr = conn.Read(s.ctx); rerr != nil {
			break
		}
	}
	pcancel()
	<-pdone
	conn.CloseNow()

	clean := websocket.CloseStatus(rerr) == websocket.StatusNormalClosure
	s.mu.Lock()
	sess.mu.Lock()
	if sess.control != conn || sess.dead || s.closed {
		sess.mu.Unlock()
		s.mu.Unlock()
		return
	}
	if clean {
		cs := s.deleteLocked(sess)
		sess.mu.Unlock()
		s.mu.Unlock()
		s.closeAll(cs, websocket.StatusGoingAway, "agent closed")
		s.log.Info("session closed", "sid", short(sid))
		return
	}
	sess.control = nil
	sess.grace = time.AfterFunc(s.lim.Grace, func() {
		s.mu.Lock()
		sess.mu.Lock()
		if sess.control != nil || sess.dead {
			sess.mu.Unlock()
			s.mu.Unlock()
			return
		}
		cs := s.deleteLocked(sess)
		sess.mu.Unlock()
		s.mu.Unlock()
		s.closeAll(cs, CloseUnknownSID, "agent gone")
		s.log.Info("session expired after grace", "sid", short(sid))
	})
	sess.mu.Unlock()
	s.mu.Unlock()
}

// handleView is the browser viewer socket.
func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	if !s.track() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	defer s.wg.Done()
	sid := r.URL.Query().Get("sid")
	if _, ok := parseSID(sid); !ok {
		http.Error(w, "bad sid", http.StatusBadRequest)
		return
	}
	if _, ok := s.allowUpgrade(r); !ok {
		s.rejectWS(w, r, CloseRateLimit, "rate limited")
		return
	}
	conn := s.accept(w, r)
	if conn == nil {
		return
	}
	conn.SetReadLimit(s.lim.MaxMsgV2A)
	sess := s.getSession(sid)
	if sess == nil {
		closeConn(conn, CloseUnknownSID, "unknown session")
		return
	}
	if !sess.viewerRate.allow("") {
		closeConn(conn, CloseRateLimit, "too many viewer connects")
		return
	}
	cid := newCID()
	pv := &pendingViewer{conn: conn, claimed: make(chan struct{}), done: make(chan struct{})}
	sess.mu.Lock()
	switch {
	case sess.dead:
		sess.mu.Unlock()
		closeConn(conn, CloseUnknownSID, "unknown session")
		return
	case sess.control == nil:
		sess.mu.Unlock()
		closeConn(conn, int(websocket.StatusTryAgainLater), "agent offline")
		return
	case len(sess.pending) >= s.lim.MaxPendingViewers:
		sess.mu.Unlock()
		closeConn(conn, CloseRateLimit, "too many pending viewers")
		return
	}
	sess.pending[cid] = pv
	sess.mu.Unlock()

	if err := sess.sendControl(sess.ctx, Viewer(cid)); err != nil {
		s.dropPending(sess, cid)
		closeConn(conn, int(websocket.StatusTryAgainLater), "agent offline")
		return
	}

	t := time.NewTimer(s.lim.PendingTimeout)
	defer t.Stop()
	select {
	case <-pv.claimed:
		select {
		case <-pv.done:
		case <-sess.ctx.Done():
		}
	case <-t.C:
		sess.mu.Lock()
		_, still := sess.pending[cid]
		delete(sess.pending, cid)
		sess.mu.Unlock()
		if still {
			_ = sess.sendControl(s.ctx, ViewerGone(cid))
			closeConn(conn, int(websocket.StatusTryAgainLater), "agent did not answer")
			return
		}
		select {
		case <-pv.done:
		case <-sess.ctx.Done():
		}
	case <-sess.ctx.Done():
	}
}

func (s *Server) dropPending(sess *session, cid string) {
	sess.mu.Lock()
	delete(sess.pending, cid)
	sess.mu.Unlock()
}

// handleAgentData is the per-viewer agent data socket.
func (s *Server) handleAgentData(w http.ResponseWriter, r *http.Request) {
	if !s.track() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	defer s.wg.Done()
	q := r.URL.Query()
	sid, cid := q.Get("sid"), q.Get("cid")
	if _, ok := parseSID(sid); !ok || cid == "" || len(cid) > 64 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if _, ok := s.allowUpgrade(r); !ok {
		s.rejectWS(w, r, CloseRateLimit, "rate limited")
		return
	}
	hash, hasTok := bearerHash(r)
	conn := s.accept(w, r)
	if conn == nil {
		return
	}
	conn.SetReadLimit(s.lim.MaxMsgA2V)
	sess := s.getSession(sid)
	if sess == nil {
		closeConn(conn, CloseUnknownSID, "unknown session")
		return
	}
	if !hasTok || !hashEqual(sess.hash, hash) {
		closeConn(conn, CloseBadToken, "bad token")
		return
	}
	sess.mu.Lock()
	pv := sess.pending[cid]
	if sess.dead || pv == nil {
		sess.mu.Unlock()
		closeConn(conn, CloseUnknownSID, "unknown viewer")
		return
	}
	delete(sess.pending, cid)
	if sess.pairs >= s.lim.MaxSplicedPairs {
		sess.mu.Unlock()
		close(pv.claimed)
		close(pv.done)
		s.goTracked(func() { _ = pv.conn.Close(CloseRateLimit, "too many active viewers") })
		closeConn(conn, CloseRateLimit, "too many active viewers")
		return
	}
	sess.pairs++
	sess.conns[conn] = struct{}{}
	sess.conns[pv.conn] = struct{}{}
	sess.mu.Unlock()
	close(pv.claimed)

	s.log.Info("viewer paired", "sid", short(sid))
	splice(s.ctx, sess.bucket, conn, pv.conn)

	sess.mu.Lock()
	sess.pairs--
	delete(sess.conns, conn)
	delete(sess.conns, pv.conn)
	sess.mu.Unlock()
	close(pv.done)
	_ = sess.sendControl(s.ctx, ViewerGone(cid))
}
