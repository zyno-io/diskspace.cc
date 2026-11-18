// SPDX-License-Identifier: MIT

// Command limitproxy runs as PID 1 in the deploy sidecar. It starts the
// webhook server as a child on loopback and fronts it with a reverse proxy
// that caps request bodies, because webhook buffers whole bodies before
// verifying the HMAC and has no size limit.
package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

const (
	listenAddr   = ":9000"
	backendAddr  = "127.0.0.1:9001"
	maxBodyBytes = 4096
)

func newHandler(backend *url.URL) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(backend)
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
		ReadTimeout:       10 * time.Second,
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
	if err := newServer(newHandler(backend)).ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "limitproxy:", err)
		_ = child.Process.Kill()
		os.Exit(1)
	}
}
