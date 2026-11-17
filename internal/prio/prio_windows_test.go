// SPDX-License-Identifier: MIT

package prio

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsApplyLow(t *testing.T) {
	if err := ApplyProcess(Low); err != nil {
		t.Fatal(err)
	}
	c, err := windows.GetPriorityClass(windows.CurrentProcess())
	if err != nil {
		t.Fatal(err)
	}
	if c != windows.BELOW_NORMAL_PRIORITY_CLASS {
		t.Errorf("class = %#x", c)
	}
}
