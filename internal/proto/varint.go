// SPDX-License-Identifier: MIT

package proto

import (
	"encoding/binary"
	"errors"
)

// ErrVarint is returned for truncated or overflowing unsigned LEB128 values.
var ErrVarint = errors.New("proto: bad varint")

// AppendUvarint appends v as unsigned LEB128.
func AppendUvarint(b []byte, v uint64) []byte { return binary.AppendUvarint(b, v) }

// Uvarint decodes an unsigned LEB128 value, returning it and the bytes consumed.
func Uvarint(b []byte) (uint64, int, error) {
	v, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, 0, ErrVarint
	}
	return v, n, nil
}

// UvarintLen is the encoded size of v.
func UvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}
