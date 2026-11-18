// SPDX-License-Identifier: MIT

package elevate

import "os"

// tty is an open console: In for reading, Out for writing.
type tty struct{ In, Out *os.File }

func (t *tty) Close() {
	t.In.Close()
	if t.Out != t.In {
		t.Out.Close()
	}
}

// OpenTTY opens the controlling terminal (/dev/tty) or the console (CONIN$ /
// CONOUT$ on Windows): input that only the person at this computer can
// type, even when stdin or stdout are redirected.
func OpenTTY() (in, out *os.File, err error) {
	t, err := openTTY()
	if err != nil {
		return nil, nil, err
	}
	return t.In, t.Out, nil
}
