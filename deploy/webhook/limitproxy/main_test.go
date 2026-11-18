// SPDX-License-Identifier: MIT

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
	if c := post(5000, true); c == http.StatusAccepted {
		t.Fatalf("chunked 5000 bytes was accepted")
	}
}
