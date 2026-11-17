// SPDX-License-Identifier: MIT

// Adapted from github.com/zyno-io/sp2p (MIT)

package relay

import (
	"context"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

type pumpResult struct {
	fromAgent bool
	err       error
	text      bool // received a non-binary frame
	writeFail bool // writing to the peer failed
}

func (r pumpResult) oversize() bool {
	return r.err != nil && !r.writeFail && strings.Contains(r.err.Error(), "read limited")
}

// peerCode maps a close status observed on one side to the code to forward.
func peerCode(err error, fromAgent bool) websocket.StatusCode {
	c := websocket.CloseStatus(err)
	if c >= 4000 && c <= 4999 {
		return c
	}
	if !fromAgent && (c == websocket.StatusNormalClosure || c == websocket.StatusGoingAway) {
		return websocket.StatusNormalClosure
	}
	return websocket.StatusInternalError
}

// splice copies binary messages verbatim between agent (data socket) and
// viewer until either side ends, then closes both. Blocks until done.
func splice(parent context.Context, b *bucket, agent, viewer *websocket.Conn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	res := make(chan pumpResult, 2)
	pump := func(src, dst *websocket.Conn, fromAgent bool) {
		for {
			typ, data, err := src.Read(ctx)
			if err != nil {
				res <- pumpResult{fromAgent: fromAgent, err: err}
				return
			}
			if typ != websocket.MessageBinary {
				res <- pumpResult{fromAgent: fromAgent, text: true}
				return
			}
			if err := b.wait(ctx, len(data)); err != nil {
				res <- pumpResult{fromAgent: fromAgent, err: err}
				return
			}
			if err := dst.Write(ctx, websocket.MessageBinary, data); err != nil {
				res <- pumpResult{fromAgent: fromAgent, err: err, writeFail: true}
				return
			}
		}
	}
	go pump(agent, viewer, true)
	go pump(viewer, agent, false)

	r := <-res
	var agentCode, viewerCode websocket.StatusCode
	switch {
	case r.text:
		agentCode, viewerCode = websocket.StatusUnsupportedData, websocket.StatusUnsupportedData
	case r.oversize():
		agentCode, viewerCode = websocket.StatusMessageTooBig, websocket.StatusMessageTooBig
	case parent.Err() != nil:
		agentCode, viewerCode = websocket.StatusGoingAway, websocket.StatusGoingAway
	case r.writeFail:
		agentCode, viewerCode = websocket.StatusInternalError, websocket.StatusInternalError
	case r.fromAgent:
		agentCode, viewerCode = websocket.StatusNormalClosure, peerCode(r.err, true)
	default:
		c := peerCode(r.err, false)
		agentCode, viewerCode = c, websocket.StatusNormalClosure
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = agent.Close(agentCode, "") }()
	go func() { defer wg.Done(); _ = viewer.Close(viewerCode, "") }()
	wg.Wait()
	cancel()
	<-res
}
