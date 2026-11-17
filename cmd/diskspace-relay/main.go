// SPDX-License-Identifier: MIT

// Command diskspace-relay runs the diskspace.cc rendezvous relay.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	diskspace "github.com/zyno-io/diskspace.cc"
	"github.com/zyno-io/diskspace.cc/internal/relay"
)

var version = "dev"

func main() {
	listen := flag.String("listen", ":8080", "listen address")
	publicURL := flag.String("public-url", "https://diskspace.cc", "public base URL (used for CSP)")
	dev := flag.Bool("dev", false, "development mode (allow ws://localhost in CSP)")
	trustProxy := flag.Bool("trust-proxy", false, "trust X-Real-IP / X-Forwarded-For")
	githubRepo := flag.String("github-repo", relay.DefaultGitHubRepo, "GitHub owner/name that release assets are resolved from")
	devBinaryDir := flag.String("dev-binary-dir", "", "dev only: serve local release assets (diskspace_<os>_<arch>.tar.gz, ...) from this directory instead of GitHub")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)

	bs, err := relay.NewBootstrap(relay.BootstrapConfig{
		BaseURL: *publicURL, Repo: *githubRepo, Dev: *dev, DevBinaryDir: *devBinaryDir,
	})
	if err != nil {
		log.Error("bootstrap", "err", err)
		os.Exit(1)
	}
	bs.Start()
	defer bs.Stop()

	srv := relay.New(relay.Config{
		Listen: *listen, PublicURL: *publicURL, Dev: *dev, TrustProxy: *trustProxy,
		Extra:  bs.Routes(),
		Static: diskspace.WebFS(), Version: version, Logger: log,
	})
	hs := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("relay listening", "addr", *listen, "version", version)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			os.Exit(1)
		}
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	_ = srv.Shutdown(sctx)
}
