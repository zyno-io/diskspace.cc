// SPDX-License-Identifier: MIT

package session

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SafeText makes s safe to show in a terminal prompt or a native dialog:
// control characters (newlines, escape sequences), Unicode format characters
// (bidi overrides and isolates, zero-width characters, tags), line and
// paragraph separators and invalid UTF-8 are replaced by visible escapes, so
// a file name can't fake extra prompt lines, reorder what is shown, or hide
// part of a path. A result longer than max bytes keeps its start and end
// with "…" between them (max <= 0: no limit).
func SafeText(s string, max int) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n <= 1:
			fmt.Fprintf(&b, `\x%02X`, s[i])
		case unsafeRune(r):
			if r < 0x100 {
				fmt.Fprintf(&b, `\x%02X`, r)
			} else {
				fmt.Fprintf(&b, `\u{%X}`, r)
			}
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	out := b.String()
	if max > 0 && len(out) > max {
		keep := max - len("…")
		head := truncRunes(out, keep/2)
		tail := tailRunes(out, keep-len(head))
		out = head + "…" + tail
	}
	return out
}

func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) ||
		unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) ||
		r == 0x115F || r == 0x1160 || r == 0x3164 || r == 0xFFA0 // Hangul fillers render as blanks
}

// truncRunes returns the longest prefix of s of at most n bytes that ends on
// a rune boundary.
func truncRunes(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// tailRunes returns the longest suffix of s of at most n bytes that starts on
// a rune boundary.
func tailRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
