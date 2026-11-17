// SPDX-License-Identifier: MIT

package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
)

type env struct {
	t   *testing.T
	rs  *Server
	ts  *httptest.Server
	url string
	mu  sync.Mutex
	cs  []*websocket.Conn
}

func (e *env) closeClients() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.cs {
		c.CloseNow()
	}
}

func newEnv(t *testing.T, lim Limits) *env {
	t.Helper()
	rs := New(Config{
		PublicURL: "https://diskspace.cc",
		Version:   "test",
		Limits:    lim,
		Static: fstest.MapFS{
			"index.html":      {Data: []byte("<html>index</html>")},
			"viewer.html":     {Data: []byte("<html>viewer</html>")},
			"launcher.html":   {Data: []byte("<html>launcher</html>")},
			"assets/app-1.js": {Data: []byte("console.log(1)")},
			"favicon.svg":     {Data: []byte("<svg/>")},
		},
		Extra: map[string]http.Handler{"/run": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("run")) })},
	})
	ts := httptest.NewServer(rs.Handler())
	e := &env{t: t, rs: rs, ts: ts, url: "ws" + strings.TrimPrefix(ts.URL, "http")}
	t.Cleanup(func() {
		e.closeClients()
		ts.Close()
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = rs.Shutdown(ctx)
	})
	return e
}

func newSID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func ctxT() context.Context {
	ctx, c := context.WithTimeout(context.Background(), 10*time.Second)
	_ = c
	return ctx
}

func (e *env) dial(path, tok string) *websocket.Conn {
	e.t.Helper()
	c, err := e.dialErr(path, tok)
	if err != nil {
		e.t.Fatalf("dial %s: %v", path, err)
	}
	return c
}

func (e *env) dialErr(path, tok string) (*websocket.Conn, error) {
	h := http.Header{}
	if tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
	c, _, err := websocket.Dial(ctxT(), e.url+path, &websocket.DialOptions{HTTPHeader: h})
	if err == nil {
		c.SetReadLimit(4 << 20)
		e.mu.Lock()
		e.cs = append(e.cs, c)
		e.mu.Unlock()
	}
	return c, err
}

func (e *env) register(sid, tok string) (*websocket.Conn, ControlMsg) {
	c := e.dial("/v1/agent?sid="+sid, tok)
	return c, readCtl(e.t, c)
}

func readCtl(t *testing.T, c *websocket.Conn) ControlMsg {
	t.Helper()
	typ, b, err := c.Read(ctxT())
	if err != nil {
		t.Fatalf("read control: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("control frame not text")
	}
	var m ControlMsg
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func expectClose(t *testing.T, c *websocket.Conn, want int) {
	t.Helper()
	_, _, err := c.Read(ctxT())
	if got := int(websocket.CloseStatus(err)); got != want {
		t.Fatalf("close code = %d (err %v), want %d", got, err, want)
	}
}

// pair sets up agent control + viewer + agent data, returning all three.
func (e *env) pair(sid, tok string, ctl *websocket.Conn) (viewer, data *websocket.Conn, cid string) {
	e.t.Helper()
	viewer = e.dial("/v1/view?sid="+sid, "")
	m := readCtl(e.t, ctl)
	if m.T != MsgViewer || m.CID == "" {
		e.t.Fatalf("unexpected control msg %+v", m)
	}
	data = e.dial("/v1/agent/data?sid="+sid+"&cid="+m.CID, tok)
	return viewer, data, m.CID
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func TestPairingAndForwarding(t *testing.T) {
	e := newEnv(t, Limits{})
	sid, tok := newSID(), "tok1"
	ctl, reg := e.register(sid, tok)
	if reg.T != MsgRegistered || reg.TTL <= 0 {
		t.Fatalf("bad registered: %+v", reg)
	}
	viewer, data, cid := e.pair(sid, tok, ctl)
	ctx := ctxT()

	// viewer -> agent (sent possibly before/after pairing)
	for _, n := range []int{1, 100, 64 << 10} {
		msg := randBytes(n)
		if err := viewer.Write(ctx, websocket.MessageBinary, msg); err != nil {
			t.Fatal(err)
		}
		typ, got, err := data.Read(ctx)
		if err != nil || typ != websocket.MessageBinary || !bytes.Equal(got, msg) {
			t.Fatalf("v2a mismatch n=%d err=%v", n, err)
		}
	}
	// agent -> viewer, including 1 MiB and max size
	for _, n := range []int{1, 1 << 20, 1<<20 + 4<<10} {
		msg := randBytes(n)
		if err := data.Write(ctx, websocket.MessageBinary, msg); err != nil {
			t.Fatal(err)
		}
		typ, got, err := viewer.Read(ctx)
		if err != nil || typ != websocket.MessageBinary || !bytes.Equal(got, msg) {
			t.Fatalf("a2v mismatch n=%d err=%v", n, err)
		}
	}

	// viewer disconnect -> viewer_gone, data socket closed
	viewer.Close(websocket.StatusNormalClosure, "")
	m := readCtl(t, ctl)
	if m.T != MsgViewerGone || m.CID != cid {
		t.Fatalf("expected viewer_gone %s, got %+v", cid, m)
	}
	expectClose(t, data, 1000)
}

func TestViewerSendsBeforeAgentDials(t *testing.T) {
	e := newEnv(t, Limits{})
	sid, tok := newSID(), "tok"
	ctl, _ := e.register(sid, tok)
	viewer := e.dial("/v1/view?sid="+sid, "")
	m := readCtl(t, ctl)
	msg := randBytes(82)
	if err := viewer.Write(ctxT(), websocket.MessageBinary, msg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	data := e.dial("/v1/agent/data?sid="+sid+"&cid="+m.CID, tok)
	_, got, err := data.Read(ctxT())
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("early message lost: %v", err)
	}
}

func TestOversizeAndText(t *testing.T) {
	e := newEnv(t, Limits{})
	sid, tok := newSID(), "tok"
	ctl, _ := e.register(sid, tok)

	// v2a oversize
	viewer, data, _ := e.pair(sid, tok, ctl)
	_ = viewer.Write(ctxT(), websocket.MessageBinary, make([]byte, 64<<10+1))
	expectClose(t, viewer, 1009)
	expectClose(t, data, 1009)
	readCtl(t, ctl) // viewer_gone

	// a2v oversize
	viewer, data, _ = e.pair(sid, tok, ctl)
	_ = data.Write(ctxT(), websocket.MessageBinary, make([]byte, 1<<20+4<<10+1))
	expectClose(t, data, 1009)
	expectClose(t, viewer, 1009)
	readCtl(t, ctl)

	// text from viewer
	viewer, data, _ = e.pair(sid, tok, ctl)
	_ = viewer.Write(ctxT(), websocket.MessageText, []byte("hi"))
	expectClose(t, viewer, int(websocket.StatusUnsupportedData))
	expectClose(t, data, int(websocket.StatusUnsupportedData))
	readCtl(t, ctl)

	// text from agent
	viewer, data, _ = e.pair(sid, tok, ctl)
	_ = data.Write(ctxT(), websocket.MessageText, []byte("hi"))
	expectClose(t, data, int(websocket.StatusUnsupportedData))
	expectClose(t, viewer, int(websocket.StatusUnsupportedData))
}

func TestAgentCloseCodePropagation(t *testing.T) {
	e := newEnv(t, Limits{})
	sid, tok := newSID(), "tok"
	ctl, _ := e.register(sid, tok)
	viewer, data, _ := e.pair(sid, tok, ctl)
	data.Close(4402, "x")
	expectClose(t, viewer, 4402)
	readCtl(t, ctl)
	viewer, data, _ = e.pair(sid, tok, ctl)
	data.Close(websocket.StatusNormalClosure, "")
	expectClose(t, viewer, 1011)
}

func TestAuthErrors(t *testing.T) {
	e := newEnv(t, Limits{})
	sid := newSID()

	// agent without token
	c := e.dial("/v1/agent?sid="+sid, "")
	expectClose(t, c, CloseBadToken)

	// unknown sid viewer / data
	v := e.dial("/v1/view?sid="+newSID(), "")
	expectClose(t, v, CloseUnknownSID)
	d := e.dial("/v1/agent/data?sid="+newSID()+"&cid=x", "tok")
	expectClose(t, d, CloseUnknownSID)

	// wrong token on data
	ctl, _ := e.register(sid, "good")
	_ = e.dial("/v1/view?sid="+sid, "")
	m := readCtl(t, ctl)
	d = e.dial("/v1/agent/data?sid="+sid+"&cid="+m.CID, "bad")
	expectClose(t, d, CloseBadToken)
	d = e.dial("/v1/agent/data?sid="+sid+"&cid="+m.CID, "")
	expectClose(t, d, CloseBadToken)
	// unknown cid
	d = e.dial("/v1/agent/data?sid="+sid+"&cid=nope", "good")
	expectClose(t, d, CloseUnknownSID)

	// different token claiming sid
	c = e.dial("/v1/agent?sid="+sid, "other")
	expectClose(t, c, CloseSIDClaimed)

	// bad sid -> 400
	if _, err := e.dialErr("/v1/agent?sid=short", "x"); err == nil {
		t.Fatal("expected error for bad sid")
	}
}

func TestReRegisterWithinGrace(t *testing.T) {
	e := newEnv(t, Limits{})
	sid, tok := newSID(), "tok"
	ctl, _ := e.register(sid, tok)
	ctl.CloseNow() // abnormal drop
	time.Sleep(150 * time.Millisecond)

	// Different token during grace -> 4409.
	c := e.dial("/v1/agent?sid="+sid, "other")
	expectClose(t, c, CloseSIDClaimed)

	ctl2, reg := e.register(sid, tok)
	if reg.T != MsgRegistered {
		t.Fatal("not registered")
	}
	viewer, data, _ := e.pair(sid, tok, ctl2)
	_ = viewer.Write(ctxT(), websocket.MessageBinary, []byte("x"))
	if _, _, err := data.Read(ctxT()); err != nil {
		t.Fatal(err)
	}
}

func TestGraceExpiryAndCleanClose(t *testing.T) {
	e := newEnv(t, Limits{Grace: 200 * time.Millisecond})
	sid := newSID()
	ctl, _ := e.register(sid, "a")
	ctl.CloseNow()
	time.Sleep(500 * time.Millisecond)
	// session gone: different token can now claim
	ctl2, _ := e.register(sid, "b")
	// clean close deletes immediately
	ctl2.Close(websocket.StatusNormalClosure, "")
	time.Sleep(150 * time.Millisecond)
	v := e.dial("/v1/view?sid="+sid, "")
	expectClose(t, v, CloseUnknownSID)
}

func TestViewerWhileAgentOffline(t *testing.T) {
	e := newEnv(t, Limits{})
	sid := newSID()
	ctl, _ := e.register(sid, "a")
	ctl.CloseNow()
	time.Sleep(150 * time.Millisecond)
	v := e.dial("/v1/view?sid="+sid, "")
	expectClose(t, v, int(websocket.StatusTryAgainLater))
}

func TestPendingTimeout(t *testing.T) {
	e := newEnv(t, Limits{PendingTimeout: 200 * time.Millisecond})
	sid := newSID()
	ctl, _ := e.register(sid, "a")
	v := e.dial("/v1/view?sid="+sid, "")
	m := readCtl(t, ctl)
	expectClose(t, v, int(websocket.StatusTryAgainLater))
	g := readCtl(t, ctl)
	if g.T != MsgViewerGone || g.CID != m.CID {
		t.Fatalf("got %+v", g)
	}
	d := e.dial("/v1/agent/data?sid="+sid+"&cid="+m.CID, "a")
	expectClose(t, d, CloseUnknownSID)
}

func TestTwoViewersAndPairCap(t *testing.T) {
	e := newEnv(t, Limits{MaxSplicedPairs: 2})
	sid, tok := newSID(), "tok"
	ctl, _ := e.register(sid, tok)
	v1, d1, _ := e.pair(sid, tok, ctl)
	v2, d2, _ := e.pair(sid, tok, ctl)
	for i, p := range [][2]*websocket.Conn{{v1, d1}, {v2, d2}} {
		msg := randBytes(50)
		_ = p[0].Write(ctxT(), websocket.MessageBinary, msg)
		_, got, err := p[1].Read(ctxT())
		if err != nil || !bytes.Equal(got, msg) {
			t.Fatalf("pair %d broken: %v", i, err)
		}
	}
	// third pair exceeds cap
	v3, d3, _ := e.pair(sid, tok, ctl)
	expectClose(t, d3, CloseRateLimit)
	expectClose(t, v3, CloseRateLimit)
	// agent supersedes v1 by closing its data socket; v1 sees close
	d1.Close(4410, "superseded")
	expectClose(t, v1, 4410)
}

func TestPerIPLimits(t *testing.T) {
	e := newEnv(t, Limits{SessionsPerIP: 2, UpgradesPerMinPerIP: 100})
	e.register(newSID(), "a")
	e.register(newSID(), "a")
	c := e.dial("/v1/agent?sid="+newSID(), "a")
	expectClose(t, c, CloseRateLimit)

	e2 := newEnv(t, Limits{UpgradesPerMinPerIP: 3})
	for i := 0; i < 3; i++ {
		c := e2.dial("/v1/view?sid="+newSID(), "")
		expectClose(t, c, CloseUnknownSID)
	}
	c = e2.dial("/v1/view?sid="+newSID(), "")
	expectClose(t, c, CloseRateLimit)
}

func TestTrustProxyIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 1.2.3.4")
	if got := clientIP(r, false); got != "10.0.0.1" {
		t.Fatal(got)
	}
	if got := clientIP(r, true); got != "1.2.3.4" {
		t.Fatal(got)
	}
	// client-supplied X-Real-IP must be ignored
	r.Header.Set("X-Real-IP", "9.9.9.9")
	if got := clientIP(r, true); got != "1.2.3.4" {
		t.Fatal(got)
	}
}

func TestClientIPKeying(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 2001:db8:1:2:aaaa:bbbb:cccc:dddd")
	a := clientIP(r, true)
	r.Header.Set("X-Forwarded-For", "2001:db8:1:2:1111:2222:3333:4444")
	if b := clientIP(r, true); a != b {
		t.Fatalf("same /64 must share a key: %q vs %q", a, b)
	}
	r.Header.Set("X-Forwarded-For", "2001:db8:1:3::1")
	if b := clientIP(r, true); a == b {
		t.Fatalf("different /64 must differ: %q", b)
	}
	r.RemoteAddr = "[2001:db8:9:9:1::1]:99"
	c := clientIP(r, false)
	r.RemoteAddr = "[2001:db8:9:9:2::2]:99"
	if d := clientIP(r, false); c != d {
		t.Fatalf("remote addr /64: %q vs %q", c, d)
	}
}

func TestMaxSessionsCap(t *testing.T) {
	e := newEnv(t, Limits{MaxSessions: 2, SessionsPerIP: 100})
	e.register(newSID(), "a")
	e.register(newSID(), "a")
	c := e.dial("/v1/agent?sid="+newSID(), "a")
	expectClose(t, c, CloseRateLimit)
}

func TestDefaultLimits(t *testing.T) {
	l := Limits{}.withDefaults()
	if l.MaxSplicedPairs < 6 || l.MaxPendingViewers < 8 || l.MaxSessions != 10000 {
		t.Fatalf("%+v", l)
	}
}

// sid-only sockets (no handshake) must not lock out a real viewer.
func TestIdleViewersDoNotLockOut(t *testing.T) {
	e := newEnv(t, Limits{})
	sid, tok := newSID(), "tok"
	ctl, _ := e.register(sid, tok)
	for i := 0; i < 2; i++ {
		e.pair(sid, tok, ctl)
	}
	v, d, _ := e.pair(sid, tok, ctl)
	msg := randBytes(10)
	_ = v.Write(ctxT(), websocket.MessageBinary, msg)
	if _, got, err := d.Read(ctxT()); err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("third pair refused: %v", err)
	}
}

func TestViewerConnectRatePerSession(t *testing.T) {
	e := newEnv(t, Limits{ViewerConnsPerMinSess: 2, UpgradesPerMinPerIP: 100})
	sid := newSID()
	e.register(sid, "a")
	for i := 0; i < 2; i++ {
		e.dial("/v1/view?sid="+sid, "")
	}
	v := e.dial("/v1/view?sid="+sid, "")
	expectClose(t, v, CloseRateLimit)
}

func TestPendingCap(t *testing.T) {
	e := newEnv(t, Limits{MaxPendingViewers: 2})
	sid := newSID()
	e.register(sid, "a")
	e.dial("/v1/view?sid="+sid, "")
	e.dial("/v1/view?sid="+sid, "")
	v := e.dial("/v1/view?sid="+sid, "")
	expectClose(t, v, CloseRateLimit)
}

func TestSessionTTL(t *testing.T) {
	e := newEnv(t, Limits{TTL: 300 * time.Millisecond})
	sid := newSID()
	ctl, _ := e.register(sid, "a")
	expectClose(t, ctl, CloseTTL)
}

func TestHTTPRoutes(t *testing.T) {
	e := newEnv(t, Limits{})
	get := func(p string) *http.Response {
		resp, err := http.Get(e.ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	sid := newSID()
	for _, p := range []string{"/s/" + sid, "/", "/launcher"} {
		r := get(p)
		if r.StatusCode != 200 {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
		csp := r.Header.Get("Content-Security-Policy")
		want := "default-src 'self'; connect-src 'self' wss://diskspace.cc; worker-src 'self'; img-src 'self' data: blob:; frame-ancestors 'none'; base-uri 'none'"
		if csp != want {
			t.Fatalf("%s csp = %q", p, csp)
		}
		for k, v := range map[string]string{
			"Referrer-Policy": "no-referrer", "Cross-Origin-Opener-Policy": "same-origin",
			"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Cache-Control": "no-store",
		} {
			if r.Header.Get(k) != v {
				t.Fatalf("%s %s = %q", p, k, r.Header.Get(k))
			}
		}
	}
	if r := get("/s/bad"); r.StatusCode != 404 {
		t.Fatalf("/s/bad: %d", r.StatusCode)
	}
	if r := get("/s/" + strings.Repeat("!", 22)); r.StatusCode != 404 {
		t.Fatal("non-base64 sid")
	}
	r := get("/assets/app-1.js")
	if r.StatusCode != 200 || r.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("asset: %d %q", r.StatusCode, r.Header.Get("Cache-Control"))
	}
	if get("/assets/nope.js").StatusCode != 404 || get("/assets/../viewer.html").StatusCode != 404 {
		t.Fatal("asset 404")
	}
	if get("/viewer.html").StatusCode != 404 || get("/nope").StatusCode != 404 {
		t.Fatal("direct html should 404")
	}
	if get("/favicon.svg").StatusCode != 200 || get("/healthz").StatusCode != 200 || get("/run").StatusCode != 200 {
		t.Fatal("misc routes")
	}
	resp, _ := http.Get(e.ts.URL + "/version")
	b := make([]byte, 16)
	n, _ := resp.Body.Read(b)
	resp.Body.Close()
	if strings.TrimSpace(string(b[:n])) != "test" {
		t.Fatal("version")
	}
}

func TestCSPDevAndHTTPPublicURL(t *testing.T) {
	if got := buildCSP("http://localhost:8080", true); !strings.Contains(got, "connect-src 'self' ws://localhost:8080 ws://localhost:* ws://127.0.0.1:*;") {
		t.Fatal(got)
	}
}

func goroutines() int {
	for i := 0; i < 50; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}

func TestNoGoroutineLeak(t *testing.T) {
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	before := goroutines()
	func() {
		rs := New(Config{PublicURL: "https://diskspace.cc"})
		ts := httptest.NewServer(rs.Handler())
		e := &env{t: t, rs: rs, ts: ts, url: "ws" + strings.TrimPrefix(ts.URL, "http")}
		sid, tok := newSID(), "tok"
		ctl, _ := e.register(sid, tok)
		v1, d1, _ := e.pair(sid, tok, ctl)
		v2, _, _ := e.pair(sid, tok, ctl)
		_ = v1.Write(ctxT(), websocket.MessageBinary, []byte("a"))
		_, _, _ = d1.Read(ctxT())
		e.dial("/v1/view?sid="+sid, "") // left pending
		v2.Close(websocket.StatusNormalClosure, "")
		ctl.Close(websocket.StatusNormalClosure, "")
		time.Sleep(200 * time.Millisecond)
		e.closeClients()
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if err := rs.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		ts.Close()
	}()
	var after int
	for i := 0; i < 20; i++ {
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
		after = goroutines()
		if after <= before {
			return
		}
	}
	buf := make([]byte, 1<<16)
	n := runtime.Stack(buf, true)
	t.Fatalf("goroutines before=%d after=%d\n%s", before, after, buf[:n])
}
