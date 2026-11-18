// SPDX-License-Identifier: MIT

package prio

import "testing"

func TestParse(t *testing.T) {
	for in, want := range map[string]Level{"": Low, "low": Low, "LOW": Low, "normal": Normal} {
		if got, ok := Parse(in); !ok || got != want {
			t.Errorf("Parse(%q) = %v,%v", in, got, ok)
		}
	}
	if _, ok := Parse("high"); ok {
		t.Error("high accepted")
	}
}

func TestNormalNoop(t *testing.T) {
	if err := ApplyProcess(Normal); err != nil {
		t.Fatal(err)
	}
}
