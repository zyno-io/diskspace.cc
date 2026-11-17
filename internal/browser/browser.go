// SPDX-License-Identifier: MIT

// Package browser opens a URL in the user's default browser without putting
// the URL on any process command line. The viewer link carries the session key
// in its fragment; argv is readable by other local users (for example through
// /proc/<pid>/cmdline), so instead of passing the link to xdg-open/open/rundll32
// we write it into a private 0600 file inside a 0700 temp dir and open that
// file, which redirects to the URL.
package browser

import (
	"encoding/json"
	"errors"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// CleanupDelay is how long the redirect file survives before it is removed.
const CleanupDelay = 60 * time.Second

var (
	mu       sync.Mutex
	dirs     = map[string]*time.Timer{}
	launcher = launch // replaced in tests
)

// Open opens url in the default browser via a temporary redirect page. The temp
// dir is removed after CleanupDelay (best effort); call Cleanup on exit to
// remove any that remain. The URL is never logged or placed in argv.
func Open(url string) error {
	if url == "" {
		return errors.New("browser: empty url")
	}
	dir, err := os.MkdirTemp("", "diskspace-open-")
	if err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	file := filepath.Join(dir, "open.html")
	if err := os.WriteFile(file, []byte(page(url)), 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	mu.Lock()
	dirs[dir] = time.AfterFunc(CleanupDelay, func() { remove(dir) })
	mu.Unlock()
	if err := launcher(file); err != nil {
		remove(dir)
		return err
	}
	return nil
}

// Cleanup removes all outstanding redirect files immediately.
func Cleanup() {
	mu.Lock()
	all := make([]string, 0, len(dirs))
	for d := range dirs {
		all = append(all, d)
	}
	mu.Unlock()
	for _, d := range all {
		remove(d)
	}
}

func remove(dir string) {
	mu.Lock()
	t, ok := dirs[dir]
	delete(dirs, dir)
	mu.Unlock()
	if ok {
		t.Stop()
	}
	_ = os.RemoveAll(dir)
}

// page renders the redirect document. The URL is embedded as a JSON string
// (with <, >, & and U+2028/9 escaped by encoding/json) inside the script, and
// HTML-escaped in the meta refresh attribute, so it cannot break out of either.
func page(url string) string {
	j, _ := json.Marshal(url) // string marshal cannot fail; escapes <, >, &
	js := strings.ReplaceAll(string(j), "/", `\/`)
	return `<!doctype html><html><head><meta charset="utf-8"><title>diskspace.cc</title>` +
		`<meta name="referrer" content="no-referrer">` +
		`<meta http-equiv="refresh" content="0;url=` + html.EscapeString(url) + `">` +
		`</head><body><script>location.replace(` + js + `);</script></body></html>`
}

func launch(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
