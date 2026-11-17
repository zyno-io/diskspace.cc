// SPDX-License-Identifier: MIT

package prio

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxThreadLow(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := LockThreadLow(); err != nil {
			t.Errorf("LockThreadLow: %v", err)
			return
		}
		tid := unix.Gettid()
		n, err := unix.Getpriority(unix.PRIO_PROCESS, tid)
		if err != nil {
			t.Error(err)
			return
		}
		if n != 10 {
			t.Errorf("priority = %d", n)
		}
		v, _, e := unix.Syscall(unix.SYS_IOPRIO_GET, ioprioWhoProcess, uintptr(tid), 0)
		if e != 0 {
			t.Error(e)
			return
		}
		if int(v) != ioprioValue(ioprioClassBE, 7) {
			t.Errorf("ioprio = %#x", v)
		}
	}()
	<-done
}
