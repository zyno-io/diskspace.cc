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

func lockThreadLow() error {
	runtime.LockOSThread() // intentionally never unlocked
	tid := unix.Gettid()
	err := unix.Setpriority(unix.PRIO_PROCESS, tid, 10)
	_, _, e := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(tid), uintptr(ioprioValue(ioprioClassBE, 7)))
	if e != 0 && err == nil {
		err = e
	}
	return err
}
