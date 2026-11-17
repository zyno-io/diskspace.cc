// SPDX-License-Identifier: MIT

//go:build !windows

package elevate

import "os"

func openTTY() (*tty, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &tty{In: f, Out: f}, nil
}
