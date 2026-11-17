// SPDX-License-Identifier: MIT

//go:build windows

package elevate

import "os"

func openTTY() (*tty, error) {
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		in.Close()
		return nil, err
	}
	return &tty{In: in, Out: out}, nil
}
