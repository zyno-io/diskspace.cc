// SPDX-License-Identifier: MIT

//go:build windows

package elevate

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shell32           = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteW = shell32.NewProc("ShellExecuteExW")
)

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	errorCancelled        = windows.Errno(1223)
)

// shellExecuteInfo mirrors SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         windows.Handle
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     windows.Handle
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    windows.Handle
	dwHotKey     uint32
	hIcon        windows.Handle
	hProcess     windows.Handle
}

type winChild struct {
	h   windows.Handle
	pid int
}

// RunAsAdmin starts self elevated via the UAC "runas" verb.
func RunAsAdmin(self string, args []string, show bool) (Child, error) {
	verb, _ := windows.UTF16PtrFromString("runas")
	file, err := windows.UTF16PtrFromString(self)
	if err != nil {
		return nil, err
	}
	params, err := windows.UTF16PtrFromString(BuildWindowsCmdLine(args))
	if err != nil {
		return nil, err
	}
	show32 := int32(windows.SW_HIDE)
	if show {
		show32 = windows.SW_SHOWNORMAL
	}
	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: params,
		nShow:        show32,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	r, _, callErr := procShellExecuteW.Call(uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		if errors.Is(callErr, errorCancelled) {
			return nil, ErrCancelled
		}
		return nil, fmt.Errorf("ShellExecuteExW: %w", callErr)
	}
	if info.hProcess == 0 {
		return nil, errors.New("ShellExecuteExW returned no process handle")
	}
	pid, _ := windows.GetProcessId(info.hProcess)
	return &winChild{h: info.hProcess, pid: int(pid)}, nil
}

func (c *winChild) Pid() int { return c.pid }

func (c *winChild) Wait() (int, error) {
	if _, err := windows.WaitForSingleObject(c.h, windows.INFINITE); err != nil {
		return -1, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(c.h, &code); err != nil {
		return -1, err
	}
	return int(code), nil
}

func (c *winChild) Kill() error { return windows.TerminateProcess(c.h, 1) }

// SudoValidate is Unix-only.
func SudoValidate() error { return ErrUnsupported }

// ExecSudo is Unix-only.
func ExecSudo(self string, args []string) error { return ErrUnsupported }
