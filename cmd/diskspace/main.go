// SPDX-License-Identifier: MIT

// Command diskspace shows what is using your disk space in a browser,
// end-to-end encrypted through a relay that cannot read it.
package main

import (
	"os"

	"github.com/zyno-io/diskspace.cc/internal/app"
)

// Set at link time: -X main.version=... -X main.defaultRelay=...
var (
	version      = "dev"
	defaultRelay = ""
)

func main() { app.Main(os.Args, version, defaultRelay) }
