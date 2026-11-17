// SPDX-License-Identifier: MIT

package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/zyno-io/diskspace.cc/internal/relay"
)

func (a *Agent) relayWS(path string, q url.Values) string {
	u, err := url.Parse(a.relayBase())
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "http", "ws":
		u.Scheme = "ws"
	default:
		u.Scheme = "wss"
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = q.Encode()
	return u.String()
}

func (g *generation) header() http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+g.tokB64)
	return h
}

// relayLoop keeps the control channel registered for the current generation,
// re-dialing with backoff (same sid+T) until the agent stops.
func (a *Agent) relayLoop() {
	backoff := 500 * time.Millisecond
	first := true
	for a.life.Err() == nil {
		g := a.curGen()
		state := "reconnecting"
		if first {
			state = "connecting"
		}
		first = false
		a.emit(Event{Kind: EvRelayState, State: state})
		registered, err := a.relaySession(g)
		if registered {
			backoff = 500 * time.Millisecond
		}
		if a.life.Err() != nil {
			return
		}
		if g.ctx.Err() != nil {
			continue // rotated: pick up the new generation immediately
		}
		if err != nil {
			a.emit(Event{Kind: EvRelayState, State: "reconnecting", Err: err})
		}
		select {
		case <-a.life.Done():
			return
		case <-g.ctx.Done():
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > 15*time.Second {
			backoff = 15 * time.Second
		}
	}
}

// relaySession runs one control-channel connection for g.
func (a *Agent) relaySession(g *generation) (registered bool, err error) {
	ctx, cancel := context.WithCancel(g.ctx)
	defer cancel()
	q := url.Values{"sid": {g.sec.SIDString()}}
	dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
	c, _, err := websocket.Dial(dctx, a.relayWS("/v1/agent", q), &websocket.DialOptions{
		HTTPHeader: g.header(), CompressionMode: websocket.CompressionDisabled,
	})
	dcancel()
	if err != nil {
		return false, err
	}
	c.SetReadLimit(4 << 10)
	// A clean 1000 close (shutdown / rotation) deletes the session at the
	// relay at once. Reading with an uncancellable context keeps coder from
	// tearing the socket down before the close handshake.
	stop := context.AfterFunc(g.ctx, func() {
		done := make(chan struct{})
		go func() { _ = c.Close(websocket.StatusNormalClosure, "bye"); close(done) }()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			_ = c.CloseNow()
		}
	})
	defer stop()
	defer c.CloseNow()

	var (
		mu    sync.Mutex
		pairs = map[string]context.CancelFunc{}
	)
	defer func() {
		mu.Lock()
		for _, cf := range pairs {
			cf()
		}
		mu.Unlock()
	}()

	for {
		_, data, rerr := c.Read(context.Background())
		if rerr != nil {
			if g.ctx.Err() != nil || a.life.Err() != nil {
				return registered, nil
			}
			switch st := websocket.CloseStatus(rerr); st {
			case relay.CloseSIDClaimed:
				a.emit(Event{Kind: EvError, Message: "relay session id is claimed by another agent; rotating link"})
				a.Rotate()
				return registered, nil
			case relay.CloseBadToken, relay.CloseTTL:
				a.emit(Event{Kind: EvError, Message: fmt.Sprintf("relay closed the session (code %d); rotating link", st)})
				a.Rotate()
				return registered, nil
			}
			return registered, rerr
		}
		var m relay.ControlMsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.T {
		case relay.MsgRegistered:
			if !registered {
				registered = true
				a.emit(Event{Kind: EvRelayState, State: "connected"})
				a.emit(Event{Kind: EvLinkReady, URL: linkFor(a.relayBase(), g), LocalURL: a.LocalURL()})
			}
		case relay.MsgViewer:
			vctx, vcancel := context.WithCancel(g.ctx)
			cid := m.CID
			mu.Lock()
			pairs[cid] = vcancel
			mu.Unlock()
			go func() {
				defer vcancel()
				a.dialData(vctx, g, cid)
				mu.Lock()
				delete(pairs, cid)
				mu.Unlock()
			}()
		case relay.MsgViewerGone:
			mu.Lock()
			if cf := pairs[m.CID]; cf != nil {
				cf()
			}
			mu.Unlock()
		}
	}
}

func (a *Agent) dialData(ctx context.Context, g *generation, cid string) {
	q := url.Values{"sid": {g.sec.SIDString()}, "cid": {cid}}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ws, _, err := websocket.Dial(dctx, a.relayWS("/v1/agent/data", q), &websocket.DialOptions{
		HTTPHeader: g.header(), CompressionMode: websocket.CompressionDisabled,
	})
	cancel()
	if err != nil {
		return
	}
	stop := context.AfterFunc(ctx, func() { _ = ws.CloseNow() })
	defer stop()
	a.serveViewer(ws, g)
}
