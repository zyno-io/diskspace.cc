// SPDX-License-Identifier: MIT

package elevate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestExecSudoAttachesTerminalWithPipedStdin(t *testing.T) {
	if os.Getenv("DISKSPACE_TEST_EXEC_SUDO") == "1" {
		t.Fatal(ExecSudo("/bin/sh", []string{"-c", "test -t 0"}))
	}

	script, err := exec.LookPath("script")
	if err != nil {
		t.Skip("script is required to allocate a terminal")
	}
	dir := t.TempDir()
	// Stand in for sudo without requiring root or a particular sudo build.
	sudo := []byte("#!/bin/sh\n[ \"$1\" = -- ] || exit 2\nshift\nexec \"$@\"\n")
	if err := os.WriteFile(filepath.Join(dir, "sudo"), sudo, 0700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISKSPACE_TEST_EXEC_SUDO", "1")
	t.Setenv("DISKSPACE_TEST_EXE", self)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, script, "-qec",
		`printf '' | "$DISKSPACE_TEST_EXE" -test.run '^TestExecSudoAttachesTerminalWithPipedStdin$'`, "/dev/null")
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sudo with piped stdin did not get a terminal: %v\n%s", err, out)
	}
}
