// SPDX-License-Identifier: MIT

//go:build !windows

package cli

import (
	"os/signal"
	"syscall"
)

// ignoreBackgroundRead makes a terminal read from a background job fail
// (EIO) instead of stopping the whole agent with SIGTTIN.
func ignoreBackgroundRead() { signal.Ignore(syscall.SIGTTIN) }
