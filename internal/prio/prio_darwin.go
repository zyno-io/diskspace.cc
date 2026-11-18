// SPDX-License-Identifier: MIT

package prio

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	sysIOPolicy = 322 // SYS_iopolicysys

	iopolCmdGet = 1 // XNU IOPOL_CMD_GET
	iopolCmdSet = 2 // XNU IOPOL_CMD_SET

	iopolTypeDisk        = 0
	iopolTypeVFSAtime    = 2
	iopolScopeProcess    = 0
	iopolUtility         = 4
	iopolAtimeUpdatesOff = 1
)

type iopolicyArgs struct {
	scope  int32 // struct _iopol_param_t: scope, iotype, policy
	iotype int32
	policy int32
}

func iopolicy(cmd int, a *iopolicyArgs) error {
	_, _, e := unix.Syscall(sysIOPolicy, uintptr(cmd), uintptr(unsafe.Pointer(a)), 0)
	if e != 0 {
		return e
	}
	return nil
}

// applyProcessLow: nice 10 plus utility I/O tier. PRIO_DARWIN_BG is avoided on
// purpose (pins to efficiency cores and throttles the network).
// Only ever lower: a user who already started us lower (nice -n 15, taskpolicy)
// keeps that.
func applyProcessLow() error {
	var err error
	if cur, e := unix.Getpriority(unix.PRIO_PROCESS, 0); e != nil || cur < 10 {
		err = unix.Setpriority(unix.PRIO_PROCESS, 0, 10)
	}
	switch p, e := diskPolicy(); {
	case e != nil, p == 0 /*DEFAULT*/, p == 1 /*IMPORTANT*/, p == 5 /*STANDARD*/ :
		if e := iopolicy(iopolCmdSet, &iopolicyArgs{iopolScopeProcess, iopolTypeDisk, iopolUtility}); e != nil && err == nil {
			err = e
		}
	}
	// Best effort: a scan must not dirty atimes.
	_ = iopolicy(iopolCmdSet, &iopolicyArgs{iopolScopeProcess, iopolTypeVFSAtime, iopolAtimeUpdatesOff})
	return err
}

func lockThreadLow() error { return nil }

// diskPolicy reads back the process disk I/O policy (tests).
func diskPolicy() (int32, error) {
	a := iopolicyArgs{iopolScopeProcess, iopolTypeDisk, 0}
	if err := iopolicy(iopolCmdGet, &a); err != nil {
		return 0, err
	}
	return a.policy, nil
}
