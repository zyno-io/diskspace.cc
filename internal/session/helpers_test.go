// SPDX-License-Identifier: MIT

package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/klauspost/compress/zstd"

	"github.com/zyno-io/diskspace.cc/internal/proto"
	"github.com/zyno-io/diskspace.cc/internal/relay"
	"github.com/zyno-io/diskspace.cc/internal/session/crypto"
)

// ---- event recorder ----

type recorder struct {
	mu  sync.Mutex
	evs []Event
	ch  chan struct{}
}

func newRecorder() *recorder { return &recorder{ch: make(chan struct{}, 1024)} }

func (r *recorder) on(e Event) {
	r.mu.Lock()
	r.evs = append(r.evs, e)
	r.mu.Unlock()
	select {
	case r.ch <- struct{}{}:
	default:
	}
}

func (r *recorder) find(kind EventKind) (Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.evs {
		if e.Kind == kind {
			return e, true
		}
	}
	return Event{}, false
}

func (r *recorder) count(kind EventKind) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.evs {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func (r *recorder) wait(t *testing.T, kind EventKind, d time.Duration) Event {
	t.Helper()
	deadline := time.After(d)
	for {
		if e, ok := r.find(kind); ok {
			return e
		}
		select {
		case <-r.ch:
		case <-deadline:
			t.Fatalf("timeout waiting for event %d", kind)
		}
	}
}

// ---- environment ----

type testEnv struct {
	t     *testing.T
	rs    *relay.Server
	ts    *httptest.Server
	agent *Agent
	rec   *recorder
	sec   *crypto.Secret
	wsURL string
}

func newRelay(t *testing.T) (*relay.Server, *httptest.Server) {
	t.Helper()
	rs := relay.New(relay.Config{
		PublicURL: "https://diskspace.cc", Dev: true, Version: "test",
		Static: fstest.MapFS{"viewer.html": {Data: []byte("v")}},
		Limits: relay.Limits{BytesPerSec: 1 << 30},
	})
	ts := httptest.NewServer(rs.Handler())
	t.Cleanup(func() {
		ts.Close()
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = rs.Shutdown(ctx)
	})
	return rs, ts
}

// newEnv starts a relay and an agent pointed at it (cfg.RelayURL is filled in).
func newEnv(t *testing.T, cfg Config) *testEnv {
	t.Helper()
	rs, ts := newRelay(t)
	e := &testEnv{t: t, rs: rs, ts: ts, rec: newRecorder(), wsURL: "ws" + strings.TrimPrefix(ts.URL, "http")}
	cfg.RelayURL = ts.URL
	cfg.Events = e.rec.on
	cfg.Version = "test"
	e.agent = startAgent(t, cfg)
	ev := e.rec.wait(t, EvLinkReady, 10*time.Second)
	e.sec = parseLink(t, ev.URL)
	return e
}

func startAgent(t *testing.T, cfg Config) *Agent {
	t.Helper()
	a, err := NewAgent(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("agent Run did not return")
		}
	})
	return a
}

func parseLink(t *testing.T, link string) *crypto.Secret {
	t.Helper()
	i := strings.Index(link, "/s/")
	h := strings.Index(link, "#")
	if i < 0 || h < 0 {
		t.Fatalf("bad link %q", link)
	}
	sec, err := crypto.ParseSecret(link[i+3:h], link[h+1:])
	if err != nil {
		t.Fatal(err)
	}
	return sec
}

// ---- viewer client ----

// blockingReader feeds one zstd payload at a time to a synchronous streaming
// decoder and reports when the decoder has consumed it and wants more.
type blockingReader struct {
	feed    chan []byte
	starved chan struct{}
	rem     []byte
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if len(b.rem) == 0 {
		b.starved <- struct{}{}
		var ok bool
		if b.rem, ok = <-b.feed; !ok {
			return 0, io.EOF
		}
	}
	n := copy(p, b.rem)
	b.rem = b.rem[n:]
	return n, nil
}

// zdec is the Go equivalent of the viewer's push-based fzstd decoder.
type zdec struct {
	in  *blockingReader
	dec *zstd.Decoder
	out chan []byte
}

func newZdec(t *testing.T) *zdec {
	in := &blockingReader{feed: make(chan []byte), starved: make(chan struct{}, 1)}
	dec, err := zstd.NewReader(in, zstd.WithDecoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	z := &zdec{in: in, dec: dec, out: make(chan []byte, 4096)}
	go func() {
		buf := make([]byte, 256<<10)
		for {
			n, err := dec.Read(buf)
			if n > 0 {
				z.out <- append([]byte(nil), buf[:n]...)
			}
			if err != nil {
				close(z.out)
				return
			}
		}
	}()
	<-in.starved // decoder waiting for its first payload
	return z
}

func (z *zdec) push(payload []byte) ([]byte, error) {
	select {
	case z.in.feed <- payload:
	case <-time.After(5 * time.Second):
		return nil, errors.New("zdec: decoder not waiting")
	}
	select {
	case <-z.in.starved:
	case <-time.After(5 * time.Second):
		return nil, errors.New("zdec: decoder did not ask for more")
	}
	var out []byte
	for {
		select {
		case c, ok := <-z.out:
			if !ok {
				return out, errors.New("zdec: decoder ended")
			}
			out = append(out, c...)
		default:
			return out, nil
		}
	}
}

type testViewer struct {
	t       *testing.T
	ws      *websocket.Conn
	ciph    *crypto.Cipher
	keys    *crypto.Keys
	zd      *zdec
	code    string
	nodesCT int // ciphertext bytes of NODES frames received
	frames  chan frameOrErr
}

type frameOrErr struct {
	ct  []byte
	err error
}

var errRecvTimeout = errors.New("recv timeout")

// dialRaw connects to the relay viewer socket.
func dialRaw(ctx context.Context, url string) (*websocket.Conn, error) {
	c, _, err := websocket.Dial(ctx, url, nil)
	if err == nil {
		c.SetReadLimit(2 << 20)
	}
	return c, err
}

func ctx10(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// handshake runs M1/M2 on ws and returns the cipher/keys.
func handshake(ctx context.Context, ws *websocket.Conn, sec *crypto.Secret) (*crypto.Cipher, *crypto.Keys, error) {
	m1, st, err := crypto.ViewerHello(sec, rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	if err := ws.Write(ctx, websocket.MessageBinary, m1); err != nil {
		return nil, nil, err
	}
	_, m2, err := ws.Read(ctx)
	if err != nil {
		return nil, nil, err
	}
	keys, err := st.Finish(m2)
	if err != nil {
		return nil, nil, err
	}
	return crypto.NewViewerCipher(keys), keys, nil
}

func (e *testEnv) viewer() *testViewer {
	e.t.Helper()
	return connectViewer(e.t, e.wsURL, e.sec)
}

func connectViewer(t *testing.T, wsBase string, sec *crypto.Secret) *testViewer {
	t.Helper()
	ctx := ctx10(t)
	ws, err := dialRaw(ctx, wsBase+"/v1/view?sid="+sec.SIDString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	ciph, keys, err := handshake(ctx, ws, sec)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	v := &testViewer{t: t, ws: ws, ciph: ciph, keys: keys, zd: newZdec(t), code: keys.Code}
	v.frames = make(chan frameOrErr, 4096)
	go func() {
		for {
			_, ct, err := ws.Read(context.Background())
			v.frames <- frameOrErr{ct, err}
			if err != nil {
				return
			}
		}
	}()
	v.send(proto.MsgClientHello, proto.ClientHello{Proto: proto.ProtoVersion, UA: "test", Caps: proto.ClientCaps{ZstdStream: true}})
	return v
}

func (v *testViewer) sendRaw(pt []byte) {
	ct, err := v.ciph.Seal(pt)
	if err != nil {
		v.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := v.ws.Write(ctx, websocket.MessageBinary, ct); err != nil {
		v.t.Fatalf("write: %v", err)
	}
}

func (v *testViewer) send(typ byte, payload any) {
	v.t.Helper()
	b, err := proto.EncodeMsg(typ, payload)
	if err != nil {
		v.t.Fatal(err)
	}
	v.sendRaw(proto.FrameV2A(b))
}

// recv returns the next message (type + whole body incl. type byte).
func (v *testViewer) recv(timeout time.Duration) (byte, []byte, error) {
	var ct []byte
	select {
	case f := <-v.frames:
		if f.err != nil {
			v.frames <- f // keep the error sticky
			return 0, nil, f.err
		}
		ct = f.ct
	case <-time.After(timeout):
		return 0, nil, errRecvTimeout
	}
	pt, err := v.ciph.Open(ct)
	if err != nil {
		return 0, nil, err
	}
	codec, rest, err := proto.ParsePlaintext(pt)
	if err != nil {
		return 0, nil, err
	}
	body := rest
	if codec == proto.CodecZstd {
		if body, err = v.zd.push(rest); err != nil {
			return 0, nil, err
		}
	}
	if len(body) == 0 {
		return 0, nil, errors.New("empty body")
	}
	if body[0] == proto.MsgNodes {
		v.nodesCT += len(ct)
	}
	return body[0], body, nil
}

func (v *testViewer) must(timeout time.Duration) (byte, []byte) {
	v.t.Helper()
	typ, body, err := v.recv(timeout)
	if err != nil {
		v.t.Fatalf("recv: %v", err)
	}
	return typ, body
}

// expect reads messages until one of type want arrives and decodes it into out.
func (v *testViewer) expect(want byte, out any) []byte {
	v.t.Helper()
	for {
		typ, body := v.must(15 * time.Second)
		if typ == want {
			if out != nil && len(body) > 1 {
				if err := json.Unmarshal(body[1:], out); err != nil {
					v.t.Fatalf("decode %#x: %v", typ, err)
				}
			}
			return body
		}
	}
}

// handshakeMsgs checks STREAM_INIT then AGENT_INFO and returns the info.
func (v *testViewer) handshakeMsgs() proto.AgentInfo {
	v.t.Helper()
	typ, _ := v.must(10 * time.Second)
	if typ != proto.MsgStreamInit {
		v.t.Fatalf("first message %#x, want STREAM_INIT", typ)
	}
	typ, body := v.must(10 * time.Second)
	if typ != proto.MsgAgentInfo {
		v.t.Fatalf("second message %#x, want AGENT_INFO", typ)
	}
	var info proto.AgentInfo
	if err := json.Unmarshal(body[1:], &info); err != nil {
		v.t.Fatal(err)
	}
	return info
}

type scanResult struct {
	Started    proto.ScanStarted
	Recs       []proto.Record
	Done       *proto.ScanDone
	Ended      *proto.ScanEnded
	ReplayDone bool
	Nodes      int
}

// collect reads until SCAN_DONE / SCAN_ENDED.
func (v *testViewer) collect() *scanResult {
	v.t.Helper()
	r := &scanResult{}
	for {
		typ, body := v.must(30 * time.Second)
		switch typ {
		case proto.MsgScanStarted:
			*r = scanResult{}
			_ = json.Unmarshal(body[1:], &r.Started)
		case proto.MsgNodes:
			id, recs, err := proto.DecodeNodes(body)
			if err != nil {
				v.t.Fatalf("DecodeNodes: %v", err)
			}
			if id != r.Started.ScanID {
				v.t.Fatalf("NODES scanId %d != %d", id, r.Started.ScanID)
			}
			r.Recs = append(r.Recs, recs...)
			r.Nodes++
		case proto.MsgReplayDone:
			r.ReplayDone = true
		case proto.MsgScanDone:
			r.Done = new(proto.ScanDone)
			_ = json.Unmarshal(body[1:], r.Done)
			return r
		case proto.MsgScanEnded:
			r.Ended = new(proto.ScanEnded)
			_ = json.Unmarshal(body[1:], r.Ended)
			return r
		}
	}
}

func (r *scanResult) sums() proto.ScanTotals {
	var t proto.ScanTotals
	for _, rec := range r.Recs {
		switch x := rec.(type) {
		case *proto.DirRecord:
			t.Alloc += x.OwnAlloc
			t.Apparent += x.OwnApparent
			t.Files += x.OwnFiles
			t.Dirs++
		case *proto.FoldRecord:
			t.Alloc += x.Alloc
			t.Apparent += x.Apparent
			t.Files += x.Files
			t.Dirs += x.Dirs
		}
	}
	return t
}

// ---- trees ----

func mkTree(t *testing.T, root string, dirs, filesPer int) {
	t.Helper()
	for i := 0; i < dirs; i++ {
		d := filepath.Join(root, fmt.Sprintf("d%03d", i), fmt.Sprintf("sub%d", i%7))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		for j := 0; j < filesPer; j++ {
			sz := 100 + (i*37+j*911)%9000
			if j == 0 && i%11 == 0 {
				sz = 1<<20 + 5000 // big enough to be listed individually
			}
			name := filepath.Join(d, fmt.Sprintf("file-%d-%s.dat", j, strings.Repeat("x", 40+j)))
			if err := os.WriteFile(name, make([]byte, sz), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// mkWideTree creates parents*kids directories with random 64-char names, each
// holding one small file, so NODES output is large and incompressible-ish.
func mkWideTree(t *testing.T, root string, parents, kids int) {
	t.Helper()
	for i := 0; i < parents; i++ {
		for j := 0; j < kids; j++ {
			var b [32]byte
			_, _ = rand.Read(b[:])
			d := filepath.Join(root, fmt.Sprintf("p%02d", i), hex.EncodeToString(b[:]))
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
}
