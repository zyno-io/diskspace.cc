// SPDX-License-Identifier: MIT

//go:build !windows

package elevate

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// SudoValidate runs `sudo -v` attached to the controlling terminal.
func SudoValidate() error {
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return fmt.Errorf("sudo not found: %w", err)
	}
	t, err := openTTY()
	if err != nil {
		return ErrNoTTY
	}
	defer t.Close()
	cmd := exec.Command(sudo, "-v")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = t.In, t.Out, t.Out
	return cmd.Run()
}

// ExecSudo replaces the process with `sudo -- self args...`. It only returns
// on error.
func ExecSudo(self string, args []string) error {
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return fmt.Errorf("sudo not found: %w", err)
	}
	argv := append([]string{"sudo", "--", self}, args...)

	// Some sudo implementations put the command in a background process
	// group on their PTY when stdin is a pipe (e.g. under curl | sh). Give
	// sudo the controlling terminal so the elevated CLI can read /dev/tty.
	t, err := openTTY()
	if err != nil {
		return syscall.Exec(sudo, argv, os.Environ())
	}
	defer t.Close()

	fd := int(t.In.Fd())
	if fd == unix.Stdin {
		// Opening the terminal filled a previously closed stdin.
		if _, err := unix.FcntlInt(t.In.Fd(), unix.F_SETFD, 0); err != nil {
			return fmt.Errorf("keep terminal stdin open for sudo: %w", err)
		}
	} else {
		saved, err := unix.FcntlInt(uintptr(unix.Stdin), unix.F_DUPFD_CLOEXEC, 3)
		if err != nil {
			return fmt.Errorf("save stdin for sudo: %w", err)
		}
		defer unix.Close(saved)
		if err := unix.Dup2(fd, unix.Stdin); err != nil {
			return fmt.Errorf("attach terminal stdin for sudo: %w", err)
		}
		defer unix.Dup2(saved, unix.Stdin) // exec failed; restore stdin
	}

	return syscall.Exec(sudo, argv, os.Environ())
}
