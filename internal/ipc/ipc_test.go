// SPDX-License-Identifier: MIT

package ipc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

type acceptRes struct {
	c   *Conn
	err error
}

func startAccept(t *testing.T, l *Listener) chan acceptRes {
	ch := make(chan acceptRes, 1)
	go func() { c, err := l.Accept(ctx(t)); ch <- acceptRes{c, err} }()
	return ch
}

func TestAuthSuccessAndMessages(t *testing.T) {
	l, tf, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ch := startAccept(t, l)
	child, err := Dial(ctx(t), l.Addr(), tf)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.c.Close()

	want := Msg{T: TReady, URL: "https://x/y", LocalURL: "http://127.0.0.1:1", Code: "ab", Elevated: true, FDA: "granted", Elevation: "sudo"}
	if err := child.Send(want); err != nil {
		t.Fatal(err)
	}
	got, err := r.c.Recv()
	if err != nil || got != want {
		t.Fatalf("got %+v err %v", got, err)
	}
	if err := r.c.Send(Msg{T: TRotate}); err != nil {
		t.Fatal(err)
	}
	if m, err := child.Recv(); err != nil || m.T != TRotate {
		t.Fatalf("%+v %v", m, err)
	}
	// Listener no longer accepts.
	if c, err := net.DialTimeout("tcp", l.Addr(), time.Second); err == nil {
		c.Close()
		t.Error("listener should be closed after success")
	}
}

func TestWrongTokenRejected(t *testing.T) {
	l, tf, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ch := startAccept(t, l)

	bad, err := net.Dial("tcp", l.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	io.WriteString(bad, `{"t":"auth","token":"nope"}`+"\n")
	bad.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := bad.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected bad conn to be closed")
	}
	// Non-auth first message too.
	bad2, _ := net.Dial("tcp", l.Addr())
	io.WriteString(bad2, `{"t":"ready"}`+"\n")
	bad2.Close()

	select {
	case r := <-ch:
		t.Fatalf("accept returned early: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}

	child, err := Dial(ctx(t), l.Addr(), tf)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	r.c.Close()
}

func TestTokenFileDeletedAndPerms(t *testing.T) {
	l, tf, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(tf)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("file perm %v %v", fi, err)
		}
		di, err := os.Stat(filepath.Dir(tf))
		if err != nil || di.Mode().Perm() != 0o700 {
			t.Fatalf("dir perm %v %v", di, err)
		}
	}
	ch := startAccept(t, l)
	child, err := Dial(ctx(t), l.Addr(), tf)
	if err != nil {
		t.Fatal(err)
	}
	defer child.Close()
	if _, err := os.Stat(tf); !os.IsNotExist(err) {
		t.Errorf("token file should be gone: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(tf)); !os.IsNotExist(err) {
		t.Errorf("token dir should be gone: %v", err)
	}
	(<-ch).c.Close()
}

func TestListenerCloseRemovesDir(t *testing.T) {
	l, tf, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	l.Close()
	if _, err := os.Stat(filepath.Dir(tf)); !os.IsNotExist(err) {
		t.Error("dir should be removed")
	}
}

func TestAcceptContextCancel(t *testing.T) {
	l, _, _ := Listen()
	defer l.Close()
	c, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	go func() { _, err := l.Accept(c); ch <- err }()
	cancel()
	if err := <-ch; !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

func pair(t *testing.T) (*Conn, *Conn) {
	l, tf, err := Listen()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	ch := startAccept(t, l)
	child, err := Dial(ctx(t), l.Addr(), tf)
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { child.Close(); r.c.Close() })
	return r.c, child
}

func TestOversizeLine(t *testing.T) {
	parent, child := pair(t)
	if err := child.Send(Msg{T: TError, Message: strings.Repeat("x", MaxLine)}); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("Send err %v", err)
	}
	// Raw oversize line on the wire.
	go child.c.Write([]byte(strings.Repeat("a", MaxLine+10) + "\n"))
	if _, err := parent.Recv(); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("Recv err %v", err)
	}
}

func TestMaxLineBoundary(t *testing.T) {
	parent, child := pair(t)
	// Build a message whose encoding is just under MaxLine.
	m := Msg{T: TError, Message: strings.Repeat("y", MaxLine-100)}
	go child.Send(m)
	got, err := parent.Recv()
	if err != nil || got.Message != m.Message {
		t.Fatalf("err %v", err)
	}
}

func TestEOF(t *testing.T) {
	parent, child := pair(t)
	child.Close()
	if _, err := parent.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("err %v", err)
	}
}

func TestConcurrentSend(t *testing.T) {
	parent, child := pair(t)
	const n = 50
	for i := 0; i < n; i++ {
		go child.Send(Msg{T: TScan, State: "running", Path: strings.Repeat("p", 3000)})
	}
	for i := 0; i < n; i++ {
		m, err := parent.Recv()
		if err != nil || m.T != TScan || len(m.Path) != 3000 {
			t.Fatalf("i=%d %v", i, err)
		}
	}
}

func TestReadLineBufferFull(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader(strings.Repeat("z", 10000)+"\nnext\n"), 16)
	b, err := readLine(r)
	if err != nil || len(b) != 10000 {
		t.Fatalf("%d %v", len(b), err)
	}
}
