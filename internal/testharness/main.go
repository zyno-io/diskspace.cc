// SPDX-License-Identifier: MIT

// Command testharness runs a development relay and an agent that auto-scans a
// directory, for end-to-end (Playwright) tests of the viewer.
//
//	go run ./internal/testharness -tree <dir> [-listen :8080]
//
// The first stdout line is the viewer URL (http://127.0.0.1:8080/s/<sid>#<K>).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	diskspace "github.com/zyno-io/diskspace.cc"
	"github.com/zyno-io/diskspace.cc/internal/relay"
	"github.com/zyno-io/diskspace.cc/internal/session"
)

func main() {
	tree := flag.String("tree", "", "directory to scan (required)")
	listen := flag.String("listen", "127.0.0.1:8080", "relay listen address")
	root := flag.String("root", "", "optional --root restriction")
	flag.Parse()
	if *tree == "" {
		fmt.Fprintln(os.Stderr, "testharness: -tree is required")
		os.Exit(2)
	}
	abs, err := filepath.Abs(*tree)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testharness:", err)
		os.Exit(2)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testharness:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	rs := relay.New(relay.Config{
		PublicURL: base, Dev: true, Static: diskspace.WebFS(), Version: "testharness", Logger: logger,
		Limits: relay.Limits{BytesPerSec: 1 << 30, UpgradesPerMinPerIP: 10000, SessionsPerIP: 1000, ViewerConnsPerMinSess: 10000},
	})
	srv := &http.Server{Handler: rs.Handler()}
	go func() { _ = srv.Serve(ln) }()

	linkCh := make(chan string, 4)
	cfg := session.Config{
		RelayURL: base, Root: *root, AutoScan: abs, Version: "testharness", Idle: 0,
		Cwd: &abs, Home: abs, Elevation: "none",
		Events: func(ev session.Event) {
			if ev.Kind == session.EvLinkReady && ev.URL != "" {
				select {
				case linkCh <- ev.URL:
				default:
				}
			}
		},
	}
	agent, err := session.NewAgent(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testharness:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()

	select {
	case u := <-linkCh:
		fmt.Println(u)
	case err := <-done:
		fmt.Fprintln(os.Stderr, "testharness: agent exited:", err)
		os.Exit(1)
	case <-time.After(15 * time.Second):
		fmt.Fprintln(os.Stderr, "testharness: agent did not register with the relay")
		os.Exit(1)
	}
	<-ctx.Done()
	<-done
	sctx, c := context.WithTimeout(context.Background(), 2*time.Second)
	defer c()
	_ = rs.Shutdown(sctx)
	_ = srv.Shutdown(sctx)
}
