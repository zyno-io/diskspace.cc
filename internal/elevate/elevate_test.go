// SPDX-License-Identifier: MIT

package elevate

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseAnswer(t *testing.T) {
	cases := []struct {
		in, os string
		want   answer
	}{
		{"y", "linux", ansElevate}, {"Y\n", "linux", ansElevate}, {"yes", "linux", ansElevate}, {"YES", "windows", ansElevate},
		{"", "linux", ansContinue}, {"\n", "linux", ansContinue}, {"n", "linux", ansContinue}, {"N", "linux", ansContinue}, {"no", "linux", ansContinue},
		{"f", "darwin", ansFDA}, {"f", "linux", ansInvalid}, {"maybe", "linux", ansInvalid},
	}
	for _, c := range cases {
		if got := parseAnswer(c.in, c.os); got != c.want {
			t.Errorf("parseAnswer(%q,%q)=%v want %v", c.in, c.os, got, c.want)
		}
	}
}

func prompt(t *testing.T, in, goos string, fda FDAState) (Choice, string) {
	t.Helper()
	var out bytes.Buffer
	c, err := Prompt(PromptOptions{User: "alex", OS: goos, FDA: fda, In: strings.NewReader(in), Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	return c, out.String()
}

func TestPrompt(t *testing.T) {
	if c, _ := prompt(t, "y\n", "linux", ""); c != ChoiceElevate {
		t.Fatal("y should elevate")
	}
	if c, _ := prompt(t, "\n", "linux", ""); c != ChoiceContinue {
		t.Fatal("enter should continue")
	}
	if c, _ := prompt(t, "", "linux", ""); c != ChoiceContinue {
		t.Fatal("EOF should continue")
	}
	c, out := prompt(t, "zzz\nno\n", "linux", "")
	if c != ChoiceContinue || !strings.Contains(out, "Please answer") {
		t.Fatalf("invalid then no: %v %q", c, out)
	}
	_, out = prompt(t, "\n", "linux", "")
	for _, s := range []string{"running as alex (not administrator)", "Restart as administrator (sudo will ask", "Continue as alex", "Elevate? [y/N]"} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in %q", s, out)
		}
	}
	if _, out = prompt(t, "\n", "windows", ""); !strings.Contains(out, "Windows will ask") {
		t.Error("windows wording")
	}
	if _, out = prompt(t, "\n", "linux", ""); strings.Contains(out, "Full Disk") {
		t.Error("FDA shown on linux")
	}
}

func TestPromptDarwinFDA(t *testing.T) {
	calls := 0
	old := fdaOpener
	fdaOpener = func() error { calls++; return nil }
	defer func() { fdaOpener = old }()
	c, out := prompt(t, "f\ny\n", "darwin", FDADenied)
	if c != ChoiceElevate || calls != 1 {
		t.Fatalf("choice=%v calls=%d", c, calls)
	}
	if !strings.Contains(out, "NOT granted") || strings.Count(out, "Elevate? [y/N]") != 2 {
		t.Fatalf("output: %q", out)
	}
}

func TestOsascriptNoInterpolation(t *testing.T) {
	evil := []string{`"; do shell script "rm -rf /"`, `a'b`, `$(x)`, "-e", `\`}
	args := BuildOsascriptArgs("/path/with space/self", evil)
	var src []string
	i := 0
	for ; args[i] != "--"; i += 2 {
		if args[i] != "-e" {
			t.Fatalf("expected -e at %d", i)
		}
		src = append(src, args[i+1])
	}
	script := strings.Join(src, "\n")
	for _, s := range append(evil, "/path/with space/self", "space") {
		if strings.Contains(script, s) {
			t.Errorf("script contains user string %q: %s", s, script)
		}
	}
	for n := 1; n <= 6; n++ {
		if !strings.Contains(script, "quoted form of item "+itoa(n)+" of argv") {
			t.Errorf("missing item %d", n)
		}
	}
	if !strings.Contains(script, "with administrator privileges") || !strings.Contains(script, ">/dev/null 2>&1 &") {
		t.Error("script shape")
	}
	rest := args[i+1:]
	if len(rest) != 1+len(evil) || rest[0] != "/path/with space/self" || rest[3] != "$(x)" {
		t.Errorf("argv: %q", rest)
	}
}

func TestEscapeWindowsArg(t *testing.T) {
	cases := map[string]string{
		``:                    `""`,
		`abc`:                 `abc`,
		`a b`:                 `"a b"`,
		`a"b`:                 `"a\"b"`,
		`a\b`:                 `a\b`,
		`C:\Program Files\x\`: `"C:\Program Files\x\\"`,
		`a\"b`:                `"a\\\"b"`,
		`a\\"b c`:             `"a\\\\\"b c"`,
		"a\tb":                "\"a\tb\"",
	}
	for in, want := range cases {
		if got := EscapeWindowsArg(in); got != want {
			t.Errorf("EscapeWindowsArg(%q)=%q want %q", in, got, want)
		}
	}
	got := BuildWindowsCmdLine([]string{"agent", "--cwd", `C:\My Dir\`, ""})
	if want := `agent --cwd "C:\My Dir\\" ""`; got != want {
		t.Errorf("cmdline %q want %q", got, want)
	}
}

func TestParentAlive(t *testing.T) {
	if !ParentAlive(os.Getpid()) {
		t.Error("self should be alive")
	}
	if ParentAlive(0x7ffffff0) {
		t.Error("huge pid should be dead")
	}
}

func TestWatchParent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { WatchParent(ctx, os.Getpid(), func() { t.Error("unexpected onGone") }); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("WatchParent did not stop")
	}
}

func TestInvokingUserNonEmpty(t *testing.T) {
	if CurrentUser() == "" || InvokingUser() == "" {
		t.Error("empty user")
	}
}
