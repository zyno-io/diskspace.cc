// SPDX-License-Identifier: MIT

package scan

import "testing"

func TestPathWithin(t *testing.T) {
	cases := []struct {
		root, p string
		want    bool
	}{
		{`\\?\C:\Data`, `\\?\C:\Data`, true},
		{`\\?\C:\Data`, `\\?\c:\data\sub\x`, true},
		{`\\?\C:\Data\`, `\\?\C:\Data\sub`, true},
		{`\\?\C:\Data`, `\\?\C:\Data2`, false},
		{`\\?\C:\Data`, `\\?\C:\Dat`, false},
		{`\\?\C:\Data`, `\\?\D:\Data\sub`, false},
		{"/r/root", "/r/root/a", true},
		{"/r/root", "/r/rootx", false},
	}
	for _, c := range cases {
		if got := pathWithin(c.root, c.p); got != c.want {
			t.Errorf("pathWithin(%q,%q)=%v want %v", c.root, c.p, got, c.want)
		}
	}
}
