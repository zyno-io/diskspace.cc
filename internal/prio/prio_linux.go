// SPDX-License-Identifier: MIT

package prio

import (
	"runtime"

	"golang.org/x/sys/unix"
)

const (
	ioprioWhoProcess = 1
	ioprioClassBE    = 2
	ioprioShift      = 13
)

func ioprioValue(class, data int) int { return class<<ioprioShift | data }

func applyProcessLow() error { return nil }

// lockThreadLow lowers the calling scanner thread, never raising a priority the
// user already lowered (nice -n 15, ionice -c3).
func lockThreadLow() error {
	runtime.LockOSThread() // intentionally never unlocked
	tid := unix.Gettid()
	var err error
	// The raw syscall returns 20 - nice.
	if raw, e := unix.Getpriority(unix.PRIO_PROCESS, tid); e != nil || 20-raw < 10 {
		err = unix.Setpriority(unix.PRIO_PROCESS, tid, 10)
	}
	cur, _, e := unix.Syscall(unix.SYS_IOPRIO_GET, ioprioWhoProcess, uintptr(tid), 0)
	class, data := int(cur)>>ioprioShift, int(cur)&(1<<ioprioShift-1)
	if e != 0 || class == 0 /*NONE*/ || (class == ioprioClassBE && data < 7) {
		if _, _, e := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(tid), uintptr(ioprioValue(ioprioClassBE, 7))); e != 0 && err == nil {
			err = e
		}
	}
	return err
}
