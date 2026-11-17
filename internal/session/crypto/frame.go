// SPDX-License-Identifier: MIT
// Adapted from github.com/zyno-io/sp2p (MIT): sequential-counter AES-GCM nonces.

package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
)

// MaxSeq is the first sequence number that may NOT be used; the connection
// must be closed and re-handshaken before reaching it.
const MaxSeq = uint64(1) << 32

var (
	ErrSeqExhausted = errors.New("crypto: frame sequence exhausted")
	ErrOpen         = errors.New("crypto: frame authentication failed")
)

// Cipher seals frames in one direction and opens frames in the other. Seal and
// Open keep independent state, so one goroutine may call Seal while another
// calls Open; concurrent calls to the same method are NOT safe.
//
// Sequence numbers are implicit and start at 0 per direction; the nonce is
// 4 zero bytes || u64be(seq) and the AAD is L("a2v") or L("v2a").
type Cipher struct {
	sealAEAD, openAEAD cipher.AEAD
	sealAAD, openAAD   []byte
	sealSeq, openSeq   uint64
}

func newAEAD(key *[KeyLen]byte) cipher.AEAD {
	b, err := aes.NewCipher(key[:])
	if err != nil {
		panic(err) // key length is fixed
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		panic(err)
	}
	return g
}

// NewAgentCipher seals with A2V and opens with V2A.
func NewAgentCipher(k *Keys) *Cipher {
	return &Cipher{
		sealAEAD: newAEAD(&k.A2V), openAEAD: newAEAD(&k.V2A),
		sealAAD: L("a2v"), openAAD: L("v2a"),
	}
}

// NewViewerCipher seals with V2A and opens with A2V.
func NewViewerCipher(k *Keys) *Cipher {
	return &Cipher{
		sealAEAD: newAEAD(&k.V2A), openAEAD: newAEAD(&k.A2V),
		sealAAD: L("v2a"), openAAD: L("a2v"),
	}
}

func nonce(seq uint64) (n [12]byte) {
	binary.BigEndian.PutUint64(n[4:], seq)
	return
}

// Seal encrypts plaintext as the next frame in this direction.
func (c *Cipher) Seal(plaintext []byte) ([]byte, error) {
	if c.sealSeq >= MaxSeq {
		return nil, ErrSeqExhausted
	}
	n := nonce(c.sealSeq)
	c.sealSeq++
	return c.sealAEAD.Seal(nil, n[:], plaintext, c.sealAAD), nil
}

// Open decrypts the next frame in the receive direction. Any failure
// (drop/dup/reorder/reflect/tamper) is fatal for the connection.
func (c *Cipher) Open(ct []byte) ([]byte, error) {
	if c.openSeq >= MaxSeq {
		return nil, ErrSeqExhausted
	}
	n := nonce(c.openSeq)
	pt, err := c.openAEAD.Open(nil, n[:], ct, c.openAAD)
	if err != nil {
		return nil, ErrOpen
	}
	c.openSeq++
	return pt, nil
}

// SetSeqs positions the counters (test hook for exercising 2^32 exhaustion).
func (c *Cipher) SetSeqs(seal, open uint64) { c.sealSeq, c.openSeq = seal, open }
