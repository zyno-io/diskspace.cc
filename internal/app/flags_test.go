// SPDX-License-Identifier: MIT

package app

import (
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, args ...string) *Options {
	t.Helper()
	o, err := ParseArgs(args, "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestParseDefaults(t *testing.T) {
	o := parse(t)
	if o.Relay != FallbackRelay || o.Idle != 30*time.Minute || o.Path != "" || o.AgentMode {
		t.Fatalf("%+v", o)
	}
	o, _ = ParseArgs(nil, "https://relay.example", io.Discard)
	if o.Relay != "https://relay.example" {
		t.Fatal(o.Relay)
	}
}

func TestParseFlagsAroundPath(t *testing.T) {
	o := parse(t, "--local", "some/dir", "--no-elevate", "--idle", "5s", "--threads=3", "--root", "/r")
	if o.Path != "some/dir" || !o.Local || !o.NoElevate || o.Idle != 5*time.Second || o.Threads != 3 || o.Root != "/r" {
		t.Fatalf("%+v", o)
	}
}

func TestParseErrors(t *testing.T) {
	for _, args := range [][]string{
		{"a", "b"},
		{"--cwd", "/x", "--no-cwd"},
		{"--elevate", "--no-elevate"},
		{"--idle", "-1s"},
		{"--bogus"},
		{"agent"},
	} {
		if _, err := ParseArgs(args, "", io.Discard); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}
}

func TestParseAgent(t *testing.T) {
	o := parse(t, "agent", "--ipc", "127.0.0.1:1", "--ipc-token-file", "/t", "--no-cwd", "--idle", "0")
	if !o.AgentMode || o.IPCAddr != "127.0.0.1:1" || o.IPCToken != "/t" || !o.NoCwd || o.Idle != 0 {
		t.Fatalf("%+v", o)
	}
}

func TestResolveAbsolutizes(t *testing.T) {
	o := parse(t, "--root", "rel/root", "rel/path", "--no-relay")
	err := o.Resolve(func() (string, error) { return "/work/dir", nil }, func() string { return "/home/u" })
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(o.Path) || !filepath.IsAbs(o.Root) || !strings.HasSuffix(o.Path, filepath.Join("rel", "path")) {
		t.Fatalf("%+v", o)
	}
	if o.Cwd != "/work/dir" || o.Home != "/home/u" || !o.Local {
		t.Fatalf("%+v", o)
	}
	o = parse(t, "--no-cwd")
	_ = o.Resolve(func() (string, error) { return "/w", nil }, func() string { return "/h" })
	if o.Cwd != "" {
		t.Fatal("no-cwd")
	}
	o = parse(t)
	_ = o.Resolve(func() (string, error) { return "", errors.New("x") }, func() string { return "/h" })
	if o.Cwd != "" {
		t.Fatal("getwd failure should leave cwd empty")
	}
}

func TestElevatedArgs(t *testing.T) {
	o := parse(t, "--elevate", "--open", "--local", "--root", "/r", "--threads", "4", "/scan/me", "--idle", "10m")
	o.Cwd, o.Home = "/orig/cwd", "/orig/home"
	got := ElevatedArgs(o, 4242, "https://relay.example")
	want := []string{"--no-elevate", "--relay", "https://relay.example", "--parent-pid", "4242", "--local",
		"--cwd", "/orig/cwd", "--home", "/orig/home", "--root", "/r", "--idle", "10m0s", "--threads", "4", "--", "/scan/me"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	for _, a := range got {
		if a == "--elevate" || a == "--open" {
			t.Fatalf("leaked %s", a)
		}
	}
	// no cwd -> --no-cwd; no-relay preserved; round-trips through the parser
	o2 := parse(t, "--no-cwd", "--no-relay")
	args := AgentArgs(o2, 1, "https://r", "127.0.0.1:9", "/tok")
	if args[0] != "agent" || args[1] != "--ipc" || args[2] != "127.0.0.1:9" || args[4] != "/tok" {
		t.Fatal(args)
	}
	p, err := ParseArgs(args, "", io.Discard)
	if err != nil || !p.AgentMode || !p.NoCwd || !p.NoRelay || !p.NoElevate || p.ParentPID != 1 || p.Relay != "https://r" {
		t.Fatalf("%v %+v", err, p)
	}
}
