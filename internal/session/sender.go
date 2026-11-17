// SPDX-License-Identifier: MIT

package session

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/zyno-io/diskspace.cc/internal/proto"
	"github.com/zyno-io/diskspace.cc/internal/session/crypto"
)

const (
	writeTimeout      = 30 * time.Second
	progressStarveMax = time.Second
)

// initialCredit is the credit window in NODES ciphertext bytes (var for tests).
var initialCredit = int64(proto.InitialCredit)

var errSenderClosed = errors.New("session: sender closed")

// sender is the single writer of one viewer connection. Priority:
//
//	control queue (JSON replies, BYE, ERROR, SCAN_* lifecycle)
//	  > scan-log entries at this connection's cursor (only with credit)
//	  > latest PROGRESS (coalesced, dropped when out of credit)
//
// It is the only user of the StreamCompressor and Cipher.Seal for the
// connection. Nothing here is ever awaited by the scanner.
type sender struct {
	ws   *websocket.Conn
	ciph *crypto.Cipher
	comp *proto.StreamCompressor

	wake chan struct{}

	mu            sync.Mutex
	ctrl          [][]byte
	closeAfter    bool
	closeCode     websocket.StatusCode
	closeReason   string
	progress      []byte
	credit        int64
	rd            *logReader
	replayPending bool
	replayUntil   int
	endSent       bool
	lastProgress  time.Time
}

func newSender(ws *websocket.Conn, c *crypto.Cipher, comp *proto.StreamCompressor) *sender {
	return &sender{ws: ws, ciph: c, comp: comp, wake: make(chan struct{}, 1), credit: initialCredit}
}

func (s *sender) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// push queues a control message body.
func (s *sender) push(body []byte) {
	s.mu.Lock()
	s.ctrl = append(s.ctrl, body)
	s.mu.Unlock()
	s.poke()
}

func (s *sender) pushJSON(typ byte, v any) {
	if b, err := proto.EncodeMsg(typ, v); err == nil {
		s.push(b)
	}
}

func (s *sender) pushError(code, msg string) {
	s.pushJSON(proto.MsgError, proto.Error{Code: code, Message: msg})
}

// bye queues BYE{reason} and then closes the socket with code 1000.
func (s *sender) bye(reason string) {
	b, _ := proto.EncodeMsg(proto.MsgBye, proto.Bye{Reason: reason})
	s.mu.Lock()
	s.ctrl = append(s.ctrl, b)
	s.closeAfter, s.closeCode, s.closeReason = true, websocket.StatusNormalClosure, reason
	s.mu.Unlock()
	s.poke()
}

// addCredit applies CREDIT{n}. The window never exceeds the initial size.
func (s *sender) addCredit(n uint64) {
	s.mu.Lock()
	if n > uint64(initialCredit) {
		n = uint64(initialCredit)
	}
	s.credit += int64(n)
	if s.credit > initialCredit {
		s.credit = initialCredit
	}
	s.mu.Unlock()
	s.poke()
}

// setProgress offers a PROGRESS body for the scan whose log is l.
func (s *sender) setProgress(l *scanLog, body []byte) {
	s.mu.Lock()
	if s.rd != nil && s.rd.l == l {
		s.progress = body
	}
	s.mu.Unlock()
	s.poke()
}

// attach points the sender at a scan: SCAN_STARTED is queued, and the log is
// read from the start. replay marks a reconnect replay (REPLAY_DONE follows
// the replayed portion).
func (s *sender) attach(run *scanRun, replay bool) {
	rd, err := run.log.newReader()
	s.mu.Lock()
	old := s.rd
	s.rd, s.endSent, s.progress, s.replayPending = rd, false, nil, false
	if err != nil {
		b, _ := proto.EncodeMsg(proto.MsgError, proto.Error{Code: proto.ErrCodeReplayUnavailable, Message: "scan history is no longer available; start a new scan"})
		s.ctrl = append(s.ctrl, b)
		if replay {
			s.ctrl = append(s.ctrl, []byte{proto.MsgReplayDone})
		}
	} else {
		s.ctrl = append(s.ctrl, run.startedBody)
		if replay {
			s.replayPending, s.replayUntil = true, run.log.countNow()
		}
	}
	s.mu.Unlock()
	if old != nil {
		old.close()
	}
	s.poke()
}

func (s *sender) release() {
	s.mu.Lock()
	rd := s.rd
	s.rd = nil
	s.mu.Unlock()
	if rd != nil {
		rd.close()
	}
}

type pick struct {
	body    []byte
	nodes   bool
	closing bool
	wait    <-chan struct{} // additional channel to wake on (log notify)
	delay   time.Duration   // wake after (progress pacing)
}

func (s *sender) pick() pick {
	s.mu.Lock()
	if len(s.ctrl) > 0 {
		b := s.ctrl[0]
		s.ctrl[0] = nil
		s.ctrl = s.ctrl[1:]
		s.mu.Unlock()
		return pick{body: b}
	}
	if s.closeAfter {
		s.mu.Unlock()
		return pick{closing: true}
	}
	rd, credit, replayPending, replayUntil, endSent := s.rd, s.credit, s.replayPending, s.replayUntil, s.endSent
	prog := s.progress
	if credit <= 0 || endSent {
		s.progress, prog = nil, nil // out of credit (droppable) or scan over
	}
	last := s.lastProgress
	s.mu.Unlock()

	var p pick
	if rd != nil {
		if e := rd.l.endNow(); e != nil && !e.Done && !endSent {
			// canceled / failed scan: deliver SCAN_ENDED promptly, skip the rest.
			rd.skipToEnd()
			s.mu.Lock()
			s.endSent = true
			s.mu.Unlock()
			return pick{body: e.Body}
		}
		if replayPending && rd.pos() >= replayUntil {
			s.mu.Lock()
			s.replayPending = false
			s.mu.Unlock()
			return pick{body: []byte{proto.MsgReplayDone}}
		}
		// A progress update that has waited too long goes first so a long
		// replay does not freeze the counters.
		if prog != nil && credit > 0 && time.Since(last) >= progressStarveMax {
			return s.takeProgress()
		}
		body, wait, end := rd.poll()
		switch {
		case body != nil:
			if credit > 0 {
				rd.advance()
				return pick{body: body, nodes: true}
			}
			// out of credit: wait for CREDIT (wake)
		case end != nil && !endSent:
			s.mu.Lock()
			s.endSent = true
			s.mu.Unlock()
			if len(end.Body) > 0 {
				return pick{body: end.Body}
			}
		default:
			p.wait = wait
		}
	}
	if prog != nil && credit > 0 {
		return s.takeProgress()
	}
	return p
}

func (s *sender) takeProgress() pick {
	s.mu.Lock()
	b := s.progress
	s.progress = nil
	s.lastProgress = time.Now()
	s.mu.Unlock()
	if b == nil {
		return pick{}
	}
	return pick{body: b}
}

func (s *sender) write(ctx context.Context, body []byte, nodes bool) error {
	pt, err := s.comp.Frame(body)
	if err != nil {
		return err
	}
	ct, err := s.ciph.Seal(pt)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := s.ws.Write(wctx, websocket.MessageBinary, ct); err != nil {
		return err
	}
	if nodes {
		s.mu.Lock()
		s.credit -= int64(len(ct))
		s.mu.Unlock()
	}
	return nil
}

// run writes frames until ctx ends, a write fails, or a closing BYE was sent.
func (s *sender) run(ctx context.Context) error {
	defer s.release()
	init, err := s.comp.InitFrame()
	if err != nil {
		return err
	}
	ct, err := s.ciph.Seal(init)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	err = s.ws.Write(wctx, websocket.MessageBinary, ct)
	cancel()
	if err != nil {
		return err
	}
	for {
		p := s.pick()
		switch {
		case p.closing:
			s.mu.Lock()
			code, reason := s.closeCode, s.closeReason
			s.mu.Unlock()
			_ = s.ws.Close(code, reason)
			return errSenderClosed
		case p.body != nil:
			if err := s.write(ctx, p.body, p.nodes); err != nil {
				return err
			}
			continue
		}
		var timer <-chan time.Time
		var t *time.Timer
		if p.delay > 0 {
			t = time.NewTimer(p.delay)
			timer = t.C
		}
		select {
		case <-ctx.Done():
			if t != nil {
				t.Stop()
			}
			return ctx.Err()
		case <-s.wake:
		case <-p.wait:
		case <-timer:
		}
		if t != nil {
			t.Stop()
		}
	}
}
