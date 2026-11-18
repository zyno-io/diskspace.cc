// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBodyCap(t *testing.T) {
	var got int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = len(b)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)
	front := httptest.NewServer(newHandler(u))
	defer front.Close()

	post := func(n int, chunked bool) int {
		body := io.Reader(strings.NewReader(strings.Repeat("a", n)))
		req, _ := http.NewRequest("POST", front.URL, body)
		if chunked {
			req.ContentLength = -1
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if c := post(4096, false); c != http.StatusAccepted || got != 4096 {
		t.Fatalf("4096 bytes: status %d, backend read %d", c, got)
	}
	if c := post(5000, false); c != http.StatusRequestEntityTooLarge {
		t.Fatalf("5000 bytes: status %d", c)
	}
	if c := post(5000, true); c != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked 5000 bytes: status %d", c)
	}
	if c := post(100, true); c != http.StatusAccepted || got != 100 {
		t.Fatalf("chunked 100 bytes: status %d, backend read %d", c, got)
	}
}

func TestConnLimit(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := newLimitListener(ln, 2)
	defer l.Close()
	accepted := make(chan net.Conn, 3)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	var clients []net.Conn
	for range 3 {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		clients = append(clients, c)
	}
	first := <-accepted
	<-accepted
	select {
	case <-accepted:
		t.Fatal("third connection accepted while two were open")
	case <-time.After(200 * time.Millisecond):
	}
	first.Close()
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("third connection not accepted after a slot freed")
	}
}
