// SPDX-License-Identifier: MIT

// Package ipc implements the parent<->elevated-child channel: newline
// delimited JSON over loopback TCP. The parent listens, the child dials.
package ipc

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// MaxLine is the maximum size of one message line, excluding the newline.
const MaxLine = 64 * 1024

const authTimeout = 30 * time.Second

var (
	ErrLineTooLong = errors.New("ipc: line too long")
	ErrAuth        = errors.New("ipc: authentication failed")
)

// Message types.
const (
	TAuth     = "auth"
	TReady    = "ready"
	TViewer   = "viewer"
	TScan     = "scan"
	TError    = "error"
	TRotate   = "rotate"
	TShutdown = "shutdown"

	// Deletion approval (agent --confirm-via app|console). The child asks
	// with TConfirm{ID, Confirm}; the parent answers TConfirmReply{ID, OK}.
	// TConfirmCancel{ID} tells the parent the request was withdrawn or timed
	// out (a reply after it is ignored). TDelete reports a deletion that ran:
	// State "started" | "item" (Path, Message: audit) | "done" (Message).
	TConfirm       = "confirm"
	TConfirmReply  = "confirmReply"
	TConfirmCancel = "confirmCancel"
	TDelete        = "delete"
)

// Msg is one IPC message. T selects which other fields are meaningful.
type Msg struct {
	T         string `json:"t"`
	Token     string `json:"token,omitempty"`
	URL       string `json:"url,omitempty"`
	LocalURL  string `json:"localUrl,omitempty"`
	Code      string `json:"code,omitempty"`
	Elevated  bool   `json:"elevated,omitempty"`
	FDA       string `json:"fda,omitempty"`
	State     string `json:"state,omitempty"`
	Path      string `json:"path,omitempty"`
	Message   string `json:"message,omitempty"`
	Elevation string `json:"elevation,omitempty"`
	ID        uint64 `json:"id,omitempty"`
	OK        bool   `json:"ok,omitempty"`
	// Confirm is a session.ConfirmRequest (JSON) for TConfirm.
	Confirm json.RawMessage `json:"confirm,omitempty"`
}

// Conn is an authenticated connection. Send is goroutine-safe; Recv must be
// called from one goroutine at a time.
type Conn struct {
	c  net.Conn
	r  *bufio.Reader
	mu sync.Mutex
}

func newConn(c net.Conn, r *bufio.Reader) *Conn {
	if r == nil {
		r = bufio.NewReaderSize(c, 4096)
	}
	return &Conn{c: c, r: r}
}

// Send writes one message.
func (c *Conn) Send(m Msg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > MaxLine {
		return ErrLineTooLong
	}
	b = append(b, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.c.Write(b)
	return err
}

// Recv reads one message. It returns io.EOF when the peer closed cleanly.
func (c *Conn) Recv() (Msg, error) {
	return readMsg(c.r)
}

// Close closes the connection.
func (c *Conn) Close() error { return c.c.Close() }

func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		if len(line)+len(frag) > MaxLine+1 { // +1 for the newline
			return nil, ErrLineTooLong
		}
		line = append(line, frag...)
		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err // includes io.EOF (partial line discarded)
		}
	}
}

func readMsg(r *bufio.Reader) (Msg, error) {
	line, err := readLine(r)
	if err != nil {
		return Msg{}, err
	}
	var m Msg
	if err := json.Unmarshal(line, &m); err != nil {
		return Msg{}, fmt.Errorf("ipc: bad message: %w", err)
	}
	return m, nil
}

// Listener accepts the single child connection.
type Listener struct {
	l     net.Listener
	token string
	dir   string
	once  sync.Once
}

// Listen starts a loopback listener and writes a fresh auth token to a 0600
// file inside a 0700 temp dir. The caller passes tokenFile to the child.
func Listen() (*Listener, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	token := base64.StdEncoding.EncodeToString(raw)
	dir, err := os.MkdirTemp("", "diskspace-ipc-")
	if err != nil {
		return nil, "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return nil, "", err
	}
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		os.RemoveAll(dir)
		return nil, "", err
	}
	nl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		os.RemoveAll(dir)
		return nil, "", err
	}
	return &Listener{l: nl, token: token, dir: dir}, file, nil
}

// Addr returns the host:port the child should dial.
func (l *Listener) Addr() string { return l.l.Addr().String() }

// Close closes the listener and removes the temp dir. Safe to call repeatedly.
func (l *Listener) Close() error {
	var err error
	l.once.Do(func() {
		err = l.l.Close()
		os.RemoveAll(l.dir)
	})
	return err
}

// Accept waits for exactly one connection whose first line is a valid auth
// message, rejecting all others. The listener stops accepting after success.
func (l *Listener) Accept(ctx context.Context) (*Conn, error) {
	stop := context.AfterFunc(ctx, func() { l.l.Close() })
	defer stop()

	var (
		mu      sync.Mutex
		done    bool
		winner  *Conn
		pending = map[net.Conn]struct{}{}
	)
	defer func() {
		mu.Lock()
		done = true
		for c := range pending {
			c.Close()
		}
		mu.Unlock()
	}()

	for {
		nc, err := l.l.Accept()
		if err != nil {
			mu.Lock()
			w := winner
			mu.Unlock()
			if w != nil {
				return w, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		mu.Lock()
		pending[nc] = struct{}{}
		mu.Unlock()
		go func() {
			br := bufio.NewReaderSize(nc, 4096)
			nc.SetReadDeadline(time.Now().Add(authTimeout))
			m, err := readMsg(br)
			ok := err == nil && m.T == TAuth &&
				subtle.ConstantTimeCompare([]byte(m.Token), []byte(l.token)) == 1
			mu.Lock()
			defer mu.Unlock()
			delete(pending, nc)
			if !ok || done || winner != nil {
				nc.Close()
				return
			}
			nc.SetReadDeadline(time.Time{})
			winner = newConn(nc, br)
			l.l.Close() // unblocks the accept loop
		}()
	}
}

// Dial connects to the parent, consumes (reads and deletes) the token file,
// and authenticates.
func Dial(ctx context.Context, addr, tokenFile string) (*Conn, error) {
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, err
	}
	os.Remove(tokenFile)
	os.Remove(filepath.Dir(tokenFile)) // only succeeds when empty
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp4", addr)
	if err != nil {
		return nil, err
	}
	c := newConn(nc, nil)
	if err := c.Send(Msg{T: TAuth, Token: strings.TrimSpace(string(b))}); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}
