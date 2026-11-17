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
