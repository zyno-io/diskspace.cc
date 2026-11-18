// SPDX-License-Identifier: MIT

package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/zyno-io/diskspace.cc/internal/ipc"
	"github.com/zyno-io/diskspace.cc/internal/session"
)

// Confirmation channels (session.Config.Confirm):
//   - terminal: the CLI asks in its own terminal (cli.Confirmer);
//   - app: an agent started by the desktop app asks the app over IPC, which
//     shows a native dialog;
//   - console: the elevated agent of the Windows CLI asks the unelevated
//     parent over IPC, which prompts in its console.
const (
	ConfirmTerminal = "terminal"
	ConfirmApp      = "app"
	ConfirmConsole  = "console"
)

// Reply states carried in TConfirmReply.State.
const (
	replyApproved = "approved"
	replyDeclined = "declined"
	replyTimeout  = "timeout"
	replyError    = "error"
)

// ipcConfirmer asks the parent process over IPC.
type ipcConfirmer struct {
	conn interface{ Send(ipc.Msg) error }

	mu    sync.Mutex
	next  uint64
	waits map[uint64]chan ipc.Msg
}

func newIPCConfirmer(conn interface{ Send(ipc.Msg) error }) *ipcConfirmer {
	return &ipcConfirmer{conn: conn, waits: map[uint64]chan ipc.Msg{}}
}

// encodeConfirm marshals req to fit one IPC line, listing fewer paths if
// needed (the smallest; counted in More). Folders are never dropped, so the
// folder of every item is still shown.
func encodeConfirm(req session.ConfirmRequest) ([]byte, error) {
	for {
		b, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		if len(b) <= ipc.MaxLine-4096 || len(req.Items) == 0 {
			return b, nil
		}
		n := len(req.Items) / 2
		req.More += len(req.Items) - n
		req.Items = req.Items[:n]
	}
}

// Confirm implements session.Config.Confirm.
func (c *ipcConfirmer) Confirm(ctx context.Context, req session.ConfirmRequest) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err // withdrawn before it was asked
	}
	b, err := encodeConfirm(req)
	if err != nil {
		return false, err
	}
	ch := make(chan ipc.Msg, 1)
	c.mu.Lock()
	c.next++
	id := c.next
	c.waits[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waits, id)
		c.mu.Unlock()
	}()
	if err := c.conn.Send(ipc.Msg{T: ipc.TConfirm, ID: id, Confirm: b}); err != nil {
		return false, err
	}
	select {
	case m := <-ch:
		switch {
		case m.OK && m.State != replyTimeout && m.State != replyError:
			return true, nil
		case m.State == replyTimeout:
			return false, session.ErrConfirmTimeout
		case m.State == replyError:
			msg := m.Message
			if msg == "" {
				msg = "could not ask"
			}
			return false, errors.New(msg)
		}
		return false, nil
	case <-ctx.Done():
		_ = c.conn.Send(ipc.Msg{T: ipc.TConfirmCancel, ID: id})
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return false, session.ErrConfirmTimeout
		}
		return false, ctx.Err()
	}
}

// reply delivers a TConfirmReply; replies to unknown or finished requests
// are ignored.
func (c *ipcConfirmer) reply(m ipc.Msg) {
	c.mu.Lock()
	ch := c.waits[m.ID]
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- m:
		default:
		}
	}
}

// confirmServer is the parent side for the console channel: it answers each
// TConfirm with ask (in its own goroutine, so the IPC loop keeps running)
// and aborts a prompt on TConfirmCancel.
type confirmServer struct {
	conn interface{ Send(ipc.Msg) error }
	ask  func(ctx context.Context, req session.ConfirmRequest) (bool, error)

	mu      sync.Mutex
	cancels map[uint64]context.CancelFunc
}

func newConfirmServer(conn interface{ Send(ipc.Msg) error }, ask func(context.Context, session.ConfirmRequest) (bool, error)) *confirmServer {
	return &confirmServer{conn: conn, ask: ask, cancels: map[uint64]context.CancelFunc{}}
}

// handle processes TConfirm / TConfirmCancel and reports whether m was one.
func (s *confirmServer) handle(ctx context.Context, m ipc.Msg) bool {
	switch m.T {
	case ipc.TConfirm:
		var req session.ConfirmRequest
		if err := json.Unmarshal(m.Confirm, &req); err != nil {
			_ = s.conn.Send(ipc.Msg{T: ipc.TConfirmReply, ID: m.ID, State: replyError, Message: "bad request"})
			return true
		}
		cctx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.cancels[m.ID] = cancel
		s.mu.Unlock()
		go func() {
			defer func() {
				s.mu.Lock()
				delete(s.cancels, m.ID)
				s.mu.Unlock()
				cancel()
			}()
			ok, err := s.ask(cctx, req)
			if cctx.Err() != nil {
				return // withdrawn: the child no longer waits
			}
			r := ipc.Msg{T: ipc.TConfirmReply, ID: m.ID, OK: ok && err == nil, State: replyDeclined}
			switch {
			case errors.Is(err, session.ErrConfirmTimeout):
				r.State = replyTimeout
			case err != nil:
				r.State, r.Message = replyError, err.Error()
			case ok:
				r.State = replyApproved
			}
			_ = s.conn.Send(r)
		}()
		return true
	case ipc.TConfirmCancel:
		s.mu.Lock()
		if c := s.cancels[m.ID]; c != nil {
			c()
		}
		s.mu.Unlock()
		return true
	}
	return false
}
