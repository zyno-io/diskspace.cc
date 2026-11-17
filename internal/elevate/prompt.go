// SPDX-License-Identifier: MIT

package elevate

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Choice is the user's answer to the elevation prompt.
type Choice int

const (
	ChoiceContinue Choice = iota
	ChoiceElevate
)

// PromptOptions configures Prompt. In/Out nil means the controlling terminal.
type PromptOptions struct {
	User string
	OS   string // runtime.GOOS value
	FDA  FDAState
	In   io.Reader
	Out  io.Writer
}

// fdaOpener is injectable for tests.
var fdaOpener = OpenFDASettings

type answer int

const (
	ansInvalid answer = iota
	ansContinue
	ansElevate
	ansFDA
)

func parseAnswer(s, goos string) answer {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes":
		return ansElevate
	case "", "n", "no":
		return ansContinue
	case "f":
		if goos == "darwin" {
			return ansFDA
		}
	}
	return ansInvalid
}

func renderPrompt(o PromptOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "diskspace is running as %s (not administrator).\n", o.User)
	b.WriteString("Other users' folders and some system areas will show as \"no access\".\n")
	if o.OS == "darwin" {
		switch o.FDA {
		case FDAGranted:
			b.WriteString("Full Disk Access: granted for this terminal.\n")
		case FDADenied:
			b.WriteString("Full Disk Access: NOT granted for this terminal.\n")
			b.WriteString("Without it, macOS may show permission pop-ups for Desktop, Documents and Downloads.\n")
		}
	}
	switch o.OS {
	case "windows":
		b.WriteString("  y  Restart as administrator (Windows will ask for permission)\n")
	default:
		b.WriteString("  y  Restart as administrator (sudo will ask for your password)\n")
	}
	fmt.Fprintf(&b, "  N  Continue as %s\n", o.User)
	if o.OS == "darwin" {
		b.WriteString("  f  Open Full Disk Access settings (restart your terminal afterwards)\n")
	}
	b.WriteString("Elevate? [y/N] ")
	return b.String()
}

// Prompt asks whether to restart elevated. Enter defaults to continuing.
func Prompt(opts PromptOptions) (Choice, error) {
	in, out := opts.In, opts.Out
	if in == nil || out == nil {
		t, err := openTTY()
		if err != nil {
			return ChoiceContinue, ErrNoTTY
		}
		defer t.Close()
		if in == nil {
			in = t.In
		}
		if out == nil {
			out = t.Out
		}
	}
	r := bufio.NewReader(in)
	for {
		fmt.Fprint(out, renderPrompt(opts))
		line, err := r.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(out)
			return ChoiceContinue, nil // EOF: default
		}
		switch parseAnswer(line, opts.OS) {
		case ansElevate:
			return ChoiceElevate, nil
		case ansContinue:
			return ChoiceContinue, nil
		case ansFDA:
			if err := fdaOpener(); err != nil {
				fmt.Fprintf(out, "Could not open settings: %v\n", err)
			} else {
				fmt.Fprintln(out, "Opened System Settings. Grant access to your terminal, then restart it.")
			}
			if opts.FDA == FDADenied {
				fmt.Fprintln(out)
			}
		default:
			fmt.Fprintln(out, "Please answer y or n.")
		}
	}
}

// HasTTY reports whether a controlling terminal can be opened.
func HasTTY() bool {
	t, err := openTTY()
	if err != nil {
		return false
	}
	t.Close()
	return true
}
