// SPDX-License-Identifier: MIT

package prio

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinApplyLow(t *testing.T) {
	if err := ApplyProcess(Low); err != nil {
		t.Fatalf("ApplyProcess: %v", err)
	}
	n, err := unix.Getpriority(unix.PRIO_PROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Errorf("priority = %d", n)
	}
	p, err := diskPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if p != iopolUtility {
		t.Errorf("disk iopolicy = %d, want %d", p, iopolUtility)
	}
}
