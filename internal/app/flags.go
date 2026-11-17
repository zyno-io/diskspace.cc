// SPDX-License-Identifier: MIT

// Package app wires the CLI: flag parsing, the elevation flow, the agent
// session and terminal / IPC rendering.
package app

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"
)

// FallbackRelay is used when no relay is baked in with -X.
const FallbackRelay = "https://diskspace.cc"

// Options are the parsed command-line options. Path-valued fields are
// absolute after Resolve.
type Options struct {
	Path        string // positional path (autoScan)
	Relay       string
	Local       bool
	NoRelay     bool
	Root        string
	Elevate     bool
	NoElevate   bool
	Cwd         string
	NoCwd       bool
	Home        string
	Open        bool
	Idle        time.Duration
	Threads     int
	ParentPID   int
	ShowVer     bool
	Quiet       bool
	IPCAddr     string // agent subcommand
	IPCToken    string // agent subcommand
	GUIElevated bool   // agent: the desktop app elevated this process
	AgentMode   bool
	idleSet     bool
}

func newFlagSet(o *Options, defaultRelay string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("diskspace", flag.ContinueOnError)
	fs.SetOutput(out)
	if defaultRelay == "" {
		defaultRelay = FallbackRelay
	}
	fs.StringVar(&o.Relay, "relay", defaultRelay, "relay `URL`")
	fs.BoolVar(&o.Local, "local", false, "also print a 127.0.0.1 link served by this process")
	fs.BoolVar(&o.NoRelay, "no-relay", false, "do not use a relay; local link only")
	fs.StringVar(&o.Root, "root", "", "restrict scanning and browsing to this `directory`")
	fs.BoolVar(&o.Elevate, "elevate", false, "restart with administrator privileges without asking")
	fs.BoolVar(&o.NoElevate, "no-elevate", false, "never offer to elevate")
	fs.StringVar(&o.Cwd, "cwd", "", "offer this `directory` as the current folder")
	fs.BoolVar(&o.NoCwd, "no-cwd", false, "do not offer a current folder")
	fs.StringVar(&o.Home, "home", "", "home `directory` to offer")
	fs.BoolVar(&o.Open, "open", false, "open the link in a browser (ignored as administrator)")
	fs.DurationVar(&o.Idle, "idle", 30*time.Minute, "exit after this long without a viewer (0 disables)")
	fs.IntVar(&o.Threads, "threads", 0, "scanner worker `count` (default: automatic)")
	fs.IntVar(&o.ParentPID, "parent-pid", 0, "exit when this process exits")
	fs.BoolVar(&o.ShowVer, "version", false, "print version and exit")
	fs.BoolVar(&o.Quiet, "quiet", false, "print only the link")
	fs.StringVar(&o.IPCAddr, "ipc", "", "(agent) IPC address")
	fs.StringVar(&o.IPCToken, "ipc-token-file", "", "(agent) IPC token file")
	fs.BoolVar(&o.GUIElevated, "gui-elevated", false, "(agent) elevated by the desktop app")
	fs.Usage = func() {
		fmt.Fprintf(out, "Usage: diskspace [flags] [path]\n\nShows what is using your disk space in a browser, end-to-end encrypted.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	return fs
}

// ParseArgs parses args (without argv[0]); flags may appear before or after
// the optional positional path. A leading "agent" selects the headless agent.
func ParseArgs(args []string, defaultRelay string, out io.Writer) (*Options, error) {
	o := &Options{}
	if len(args) > 0 && args[0] == "agent" {
		o.AgentMode = true
		args = args[1:]
	}
	fs := newFlagSet(o, defaultRelay, out)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "idle" {
				o.idleSet = true
			}
		})
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) > 1 {
		return nil, fmt.Errorf("unexpected argument %q (only one path is accepted)", positional[1])
	}
	if len(positional) == 1 {
		o.Path = positional[0]
	}
	if o.Cwd != "" && o.NoCwd {
		return nil, errors.New("--cwd and --no-cwd are mutually exclusive")
	}
	if o.Elevate && o.NoElevate {
		return nil, errors.New("--elevate and --no-elevate are mutually exclusive")
	}
	if o.Idle < 0 {
		return nil, errors.New("--idle must not be negative")
	}
	if o.AgentMode && (o.IPCAddr == "" || o.IPCToken == "") {
		return nil, errors.New("agent mode requires --ipc and --ipc-token-file")
	}
	return o, nil
}

func abs(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if p == "~" || (len(p) > 1 && (p[:2] == "~/" || p[:2] == "~\\")) {
		if h, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(h, p[1:])
		}
	}
	return filepath.Abs(p)
}

// Resolve absolutizes paths and fills defaults (cwd, home). getwd/home are
// injectable for tests.
func (o *Options) Resolve(getwd func() (string, error), homeDir func() string) error {
	var err error
	if o.Path, err = abs(o.Path); err != nil {
		return err
	}
	if o.Root, err = abs(o.Root); err != nil {
		return err
	}
	switch {
	case o.NoCwd:
		o.Cwd = ""
	case o.Cwd != "":
		if o.Cwd, err = abs(o.Cwd); err != nil {
			return err
		}
	default:
		if wd, err := getwd(); err == nil {
			o.Cwd = wd
		}
	}
	if o.Home != "" {
		if o.Home, err = abs(o.Home); err != nil {
			return err
		}
	} else {
		o.Home = homeDir()
	}
	if o.NoRelay {
		o.Local = true
	}
	return nil
}

// invokingHome is the home of the real user when running under sudo.
func invokingHome() string {
	if su := os.Getenv("SUDO_USER"); su != "" && os.Geteuid() == 0 {
		if u, err := user.Lookup(su); err == nil && u.HomeDir != "" {
			return u.HomeDir
		}
	}
	h, _ := os.UserHomeDir()
	return h
}

// ElevatedArgs builds the argument list (without argv[0]) for the elevated
// re-launch: the original options minus --elevate, plus --no-elevate and the
// original cwd/home/parent pid and resolved relay. All paths are absolute. K
// is never passed; the child generates its own.
func ElevatedArgs(o *Options, parentPID int, relay string) []string {
	args := []string{"--no-elevate", "--relay", relay, "--parent-pid", strconv.Itoa(parentPID)}
	if o.NoRelay {
		args = append(args, "--no-relay")
	} else if o.Local {
		args = append(args, "--local")
	}
	if o.Cwd != "" {
		args = append(args, "--cwd", o.Cwd)
	} else {
		args = append(args, "--no-cwd")
	}
	if o.Home != "" {
		args = append(args, "--home", o.Home)
	}
	if o.Root != "" {
		args = append(args, "--root", o.Root)
	}
	args = append(args, "--idle", o.Idle.String())
	if o.Threads > 0 {
		args = append(args, "--threads", strconv.Itoa(o.Threads))
	}
	if o.Quiet {
		args = append(args, "--quiet")
	}
	// --open is intentionally dropped: it is ignored as administrator.
	if o.Path != "" {
		args = append(args, "--", o.Path)
	}
	return args
}

// AgentArgs is ElevatedArgs prefixed for the headless Windows child.
func AgentArgs(o *Options, parentPID int, relay, ipcAddr, tokenFile string) []string {
	return append([]string{"agent", "--ipc", ipcAddr, "--ipc-token-file", tokenFile},
		ElevatedArgs(o, parentPID, relay)...)
}
