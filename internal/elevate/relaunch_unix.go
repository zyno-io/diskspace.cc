// SPDX-License-Identifier: MIT

//go:build !windows

package elevate

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
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
	return syscall.Exec(sudo, argv, os.Environ())
}
