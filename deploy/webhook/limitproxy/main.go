// SPDX-License-Identifier: MIT

// Command limitproxy fronts the deploy sidecar's webhook server. It starts
// webhook as a child on loopback and proxies to it with a cap on body size
// and on concurrent connections, because webhook buffers whole bodies before
// verifying the HMAC and has neither limit. Memory in the container is then
// bounded no matter how many clients connect, so no unauthenticated request
// can get the container OOM-killed in the middle of a deploy.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

const (
	listenAddr   = ":9000"
	backendAddr  = "127.0.0.1:9001"
	maxBodyBytes = 4096
	// A real deploy is one request per release; a few spare slots let one
	// through while some are held. Further connections wait in the kernel's
	// accept backlog, which costs this process nothing.
	maxConns = 16
)

// limitListener blocks Accept while maxConns connections are open.
type limitListener struct {
	net.Listener
	sem chan struct{}
}

func newLimitListener(l net.Listener, n int) net.Listener {
	return &limitListener{Listener: l, sem: make(chan struct{}, n)}
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.sem <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: sync.OnceFunc(func() { <-l.sem })}, nil
}

type limitConn struct {
	net.Conn
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}

func newHandler(backend *url.URL) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(backend)
	proxy.Transport = &http.Transport{
		MaxConnsPerHost:     maxConns,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	}
	// Answer over-long chunked bodies with 413 like declared ones, and do not
	// log per request: anyone can trigger these, and logs grow host disk.
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		case errors.Is(err, context.Canceled):
			// The client went away.
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBodyBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		// Also caps chunked bodies, which have no declared length. Exceeding
		// the cap aborts the upstream request, so webhook never receives a
		// complete oversized body.
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		proxy.ServeHTTP(w, r)
	})
}

func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Addr:              listenAddr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		log.Fatal("usage: limitproxy -- command [args...]")
	}

	child := exec.Command(args[0], args[1:]...)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		log.Fatalf("start %s: %v", args[0], err)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for s := range sigs {
			_ = child.Process.Signal(s)
		}
	}()

	go func() {
		err := child.Wait()
		// Any child exit is fatal so the restart policy recovers the pair.
		code := 1
		if err == nil {
			log.Printf("%s exited", args[0])
		} else {
			log.Printf("%s exited: %v", args[0], err)
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() > 0 {
				code = ee.ExitCode()
			}
		}
		os.Exit(code)
	}()

	backend := &url.URL{Scheme: "http", Host: backendAddr}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "limitproxy:", err)
		_ = child.Process.Kill()
		os.Exit(1)
	}
	if err := newServer(newHandler(backend)).Serve(newLimitListener(ln, maxConns)); err != nil {
		fmt.Fprintln(os.Stderr, "limitproxy:", err)
		_ = child.Process.Kill()
		os.Exit(1)
	}
}
