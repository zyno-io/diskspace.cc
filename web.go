// SPDX-License-Identifier: MIT

// Package diskspace exposes the built web assets (landing page, viewer, GUI
// launcher) so the relay, the agent's local mode and the GUI can serve them.
package diskspace

import (
	"embed"
	"io/fs"
)

//go:embed all:web/dist
var webDist embed.FS

// WebFS returns the built SPA rooted at web/dist.
func WebFS() fs.FS {
	sub, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		panic(err)
	}
	return sub
}
