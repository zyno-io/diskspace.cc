// SPDX-License-Identifier: MIT

package main

import (
	"io/fs"
	"net/url"
	"strings"
)

// launcherFS serves the built web app with launcher.html as the start page.
// Wails only injects its runtime (window.go / window.runtime) into "/" and
// "…/index.html", so the launcher has to be the index of the asset FS.
func launcherFS(web fs.FS) fs.FS { return launcherIndex{web} }

type launcherIndex struct{ fs.FS }

func (l launcherIndex) Open(name string) (fs.File, error) {
	if name == "index.html" {
		name = "launcher.html"
	}
	return l.FS.Open(name)
}

// embeddedURL adds embedded=1 to the agent's local viewer link (before the
// #key fragment) so the viewer shows its "← Launcher" button. Only loopback
// http links are accepted.
func embeddedURL(local string) (string, bool) {
	u, err := url.Parse(local)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || !strings.HasPrefix(u.Path, "/s/") {
		return "", false
	}
	q := u.Query()
	q.Set("embedded", "1")
	u.RawQuery = q.Encode()
	return u.String(), true
}
