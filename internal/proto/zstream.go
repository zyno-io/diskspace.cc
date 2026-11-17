// SPDX-License-Identifier: MIT

package proto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"

	"github.com/klauspost/compress/zstd"
)

// CompressThreshold: bodies smaller than this are sent as codec 0.
const CompressThreshold = 512

var (
	ErrNotInitialized = errors.New("proto: InitFrame must precede compressed frames")
	ErrBadPlaintext   = errors.New("proto: bad plaintext frame")
)

// StreamCompressor builds the plaintext frames ([codec][body or compressed])
// of one a2v connection. In streaming mode a single zstd stream spans all
// codec-1 frames (shared window, flushed per message); otherwise every
// compressed message is an independent zstd frame. Not goroutine-safe.
type StreamCompressor struct {
	// Rand supplies STREAM_INIT's 32 bytes (default crypto/rand; tests override).
	Rand io.Reader

	stream bool
	buf    bytes.Buffer
	enc    *zstd.Encoder
	inited bool
}

func NewStreamCompressor(zstdStream bool) *StreamCompressor {
	c := &StreamCompressor{Rand: rand.Reader, stream: zstdStream}
	opts := []zstd.EOption{
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithWindowSize(4 << 20),
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderCRC(false),
	}
	if zstdStream {
		c.enc, _ = zstd.NewWriter(&c.buf, opts...)
	} else {
		c.enc, _ = zstd.NewWriter(nil, opts...)
	}
	return c
}

func (c *StreamCompressor) compress(body []byte) ([]byte, error) {
	if !c.stream {
		return c.enc.EncodeAll(body, nil), nil
	}
	c.buf.Reset()
	if _, err := c.enc.Write(body); err != nil {
		return nil, err
	}
	if err := c.enc.Flush(); err != nil {
		return nil, err
	}
	return append([]byte(nil), c.buf.Bytes()...), nil
}

// InitFrame returns the STREAM_INIT plaintext frame: codec 1 carrying
// [0x00 + 32 random bytes]. It must be the first codec-1 frame of the
// connection; it primes the viewer's streaming decoder (which buffers a
// frame's first ~18 bytes before emitting anything).
func (c *StreamCompressor) InitFrame() ([]byte, error) {
	body := make([]byte, 33)
	if _, err := io.ReadFull(c.Rand, body[1:]); err != nil {
		return nil, err
	}
	z, err := c.compress(body)
	if err != nil {
		return nil, err
	}
	c.inited = true
	return append([]byte{CodecZstd}, z...), nil
}

// Frame returns the plaintext frame for body (body[0] is the message type).
func (c *StreamCompressor) Frame(body []byte) ([]byte, error) {
	if len(body) > MaxBody {
		return nil, errors.New("proto: body too large")
	}
	if len(body) < CompressThreshold {
		return append([]byte{CodecRaw}, body...), nil
	}
	if !c.inited {
		return nil, ErrNotInitialized
	}
	z, err := c.compress(body)
	if err != nil {
		return nil, err
	}
	return append([]byte{CodecZstd}, z...), nil
}

// Close releases encoder resources.
func (c *StreamCompressor) Close() error { return c.enc.Close() }

// ParsePlaintext splits a plaintext frame into codec and rest.
func ParsePlaintext(pt []byte) (codec byte, rest []byte, err error) {
	if len(pt) < 1 || pt[0] > CodecZstd {
		return 0, nil, ErrBadPlaintext
	}
	return pt[0], pt[1:], nil
}

// ParseV2A parses a viewer->agent plaintext: codec must be 0, body 1..MaxBody.
func ParseV2A(pt []byte) (typ byte, payload []byte, err error) {
	codec, body, err := ParsePlaintext(pt)
	if err != nil || codec != CodecRaw || len(body) == 0 || len(body) > MaxBody {
		return 0, nil, ErrBadPlaintext
	}
	return body[0], body[1:], nil
}

// FrameV2A builds a viewer->agent plaintext (codec 0).
func FrameV2A(body []byte) []byte { return append([]byte{CodecRaw}, body...) }
