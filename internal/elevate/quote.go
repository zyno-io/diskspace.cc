// SPDX-License-Identifier: MIT

package elevate

import "strings"

// osascriptSource builds AppleScript source that depends only on the number
// of arguments, never on their content. Every value is read via argv and
// shell-quoted with "quoted form of".
func osascriptSource(n int) []string {
	parts := make([]string, 0, n+1)
	for i := 1; i <= n; i++ {
		parts = append(parts, "(quoted form of item "+itoa(i)+" of argv)")
	}
	expr := strings.Join(parts, ` & " " & `)
	return []string{
		"on run argv",
		`do shell script ` + expr + ` & " >/dev/null 2>&1 &" with administrator privileges`,
		"end run",
	}
}

// BuildOsascriptArgs returns the osascript argument vector (without the
// program name) that runs self+args as administrator in the background.
func BuildOsascriptArgs(self string, args []string) []string {
	argv := append([]string{self}, args...)
	src := osascriptSource(len(argv))
	out := make([]string, 0, len(src)*2+1+len(argv))
	for _, l := range src {
		out = append(out, "-e", l)
	}
	out = append(out, "--")
	return append(out, argv...)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// EscapeWindowsArg quotes one argument per the CommandLineToArgvW rules.
func EscapeWindowsArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\n\v\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	bs := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			bs++
		case '"':
			b.WriteString(strings.Repeat(`\`, bs*2+1))
			b.WriteByte('"')
			bs = 0
		default:
			b.WriteString(strings.Repeat(`\`, bs))
			bs = 0
			b.WriteByte(c)
		}
	}
	b.WriteString(strings.Repeat(`\`, bs*2))
	b.WriteByte('"')
	return b.String()
}

// BuildWindowsCmdLine joins args into a Windows command-line parameter string.
func BuildWindowsCmdLine(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = EscapeWindowsArg(a)
	}
	return strings.Join(q, " ")
}
