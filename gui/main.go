// SPDX-License-Identifier: MIT

// Command Diskspace is the desktop app: a thin Wails window over the same
// agent the CLI runs. The binary embeds the whole CLI; `Diskspace agent …` is
// the headless agent the window starts as its child (optionally elevated).
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

func main() {
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		app.Main(os.Args, version, defaultRelay)
		return
	}
	if err := runGUI(); err != nil {
		fatal(err)
	}
}
