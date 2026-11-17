// SPDX-License-Identifier: MIT

package browser

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func capture(t *testing.T) *string {
	t.Helper()
	var got string
	old := launcher
	launcher = func(p string) error { got = p; return nil }
	t.Cleanup(func() { launcher = old; Cleanup() })
	return &got
}

func TestOpenWritesPrivateFile(t *testing.T) {
	got := capture(t)
	const u = "https://x.example/#sid=abc&k=SECRETKEY"
	if err := Open(u); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*got, "SECRETKEY") {
		t.Fatal("URL leaked into launcher argument")
	}
	b, err := os.ReadFile(*got)
	if err != nil || !strings.Contains(string(b), "location.replace(") {
		t.Fatalf("%v %s", err, b)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(*got)
		di, _ := os.Stat(filepath.Dir(*got))
		if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
			t.Fatalf("perms file=%v dir=%v", fi.Mode().Perm(), di.Mode().Perm())
		}
	}
	Cleanup()
	if _, err := os.Stat(*got); !os.IsNotExist(err) {
		t.Fatal("not cleaned up")
	}
}

func TestPageEscaping(t *testing.T) {
	evil := `https://a/"</script><script>alert(1)</script>'&x=1#k`
	p := page(evil)
	if strings.Count(p, "</script>") != 1 || strings.Count(p, "<script>") != 1 {
		t.Fatalf("breakout: %s", p)
	}
	if strings.Contains(p, `"</script>`) || strings.Contains(p, `content="0;url=https://a/"`) {
		t.Fatalf("breakout: %s", p)
	}
}

func TestOpenLauncherErrorCleansUp(t *testing.T) {
	var path string
	old := launcher
	launcher = func(p string) error { path = p; return errors.New("boom") }
	t.Cleanup(func() { launcher = old })
	if err := Open("https://x/#k"); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file left behind")
	}
	if Open("") == nil {
		t.Fatal("empty url must error")
	}
}
