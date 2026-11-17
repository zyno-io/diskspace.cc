// SPDX-License-Identifier: MIT

// Package crypto implements the diskspace.cc end-to-end handshake and frame
// AEAD (docs/protocol.md §2). The relay is untrusted: everything here
// authenticates with the link secret K carried in the URL fragment.
//
// Adapted from github.com/zyno-io/sp2p (MIT) (exchange.go key schedule shape).
package crypto

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// LabelPrefix is prepended to every domain-separation label.
	LabelPrefix = "diskspace.cc v1 "

	Version    = 1
	TypeM1     = 0xD1
	TypeM2     = 0xD2
	HelloLen   = 82
	macOff     = 50
	SIDLen     = 16
	KLen       = 32
	KeyLen     = 32
	CodeModulo = 1_000_000
)

// Sentinel errors. The caller maps them to WS close codes:
// ErrMalformed/ErrBadMAC -> 4401, ErrVersion -> 4400.
var (
	ErrMalformed = errors.New("crypto: malformed handshake message")
	ErrBadMAC    = errors.New("crypto: handshake authentication failed")
	ErrVersion   = errors.New("crypto: unsupported handshake version")
)

// L returns the label "diskspace.cc v1 " + x.
func L(x string) []byte { return []byte(LabelPrefix + x) }

// Secret is the per-session link secret: sid (URL path) and K (URL fragment).
type Secret struct {
	SID [SIDLen]byte
	K   [KLen]byte
}

// NewSecret generates a fresh random Secret.
func NewSecret() (*Secret, error) {
	s := new(Secret)
	if _, err := io.ReadFull(rand.Reader, s.SID[:]); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(rand.Reader, s.K[:]); err != nil {
		return nil, err
	}
	return s, nil
}

// SIDString is the base64url (no padding) session id, 22 chars.
func (s *Secret) SIDString() string { return base64.RawURLEncoding.EncodeToString(s.SID[:]) }

// KString is the base64url (no padding) fragment secret, 43 chars.
func (s *Secret) KString() string { return base64.RawURLEncoding.EncodeToString(s.K[:]) }

// ParseSecret decodes the base64url forms produced by SIDString/KString.
func ParseSecret(sidB64, kB64 string) (*Secret, error) {
	sid, err := base64.RawURLEncoding.DecodeString(sidB64)
	if err != nil || len(sid) != SIDLen {
		return nil, errors.New("crypto: invalid session id")
	}
	k, err := base64.RawURLEncoding.DecodeString(kB64)
	if err != nil || len(k) != KLen {
		return nil, errors.New("crypto: invalid secret")
	}
	s := new(Secret)
	copy(s.SID[:], sid)
	copy(s.K[:], k)
	return s, nil
}

// Keys is the result of a completed handshake.
type Keys struct {
	A2V  [KeyLen]byte // agent -> viewer AEAD key
	V2A  [KeyLen]byte // viewer -> agent AEAD key
	TH   [32]byte     // transcript hash
	Code string       // 6-digit verification code, "123 456"
}

func (s *Secret) kAuth() ([]byte, error) {
	return hkdf.Key(sha256.New, s.K[:], L("auth"), string(s.SID[:]), 32)
}

func mac(key, label, sid []byte, parts ...[]byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(label)
	h.Write(sid)
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// newEphemeral reads 32 bytes from rnd and builds an X25519 key from them.
// (crypto/ecdh's GenerateKey may ignore the supplied reader; reading the
// scalar ourselves keeps deterministic test hooks working.)
func newEphemeral(rnd io.Reader) (*ecdh.PrivateKey, error) {
	var b [32]byte
	if _, err := io.ReadFull(rnd, b[:]); err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPrivateKey(b[:])
}

func deriveKeys(sec *Secret, own *ecdh.PrivateKey, peerPub []byte, m1, m2 []byte) (*Keys, error) {
	pk, err := ecdh.X25519().NewPublicKey(peerPub)
	if err != nil {
		return nil, ErrMalformed
	}
	dh, err := own.ECDH(pk) // errors on all-zero output
	if err != nil {
		return nil, ErrMalformed
	}
	h := sha256.New()
	h.Write(L("th"))
	h.Write(sec.SID[:])
	h.Write(m1)
	h.Write(m2)
	th := h.Sum(nil)

	info := append(L("keys"), th...)
	kk, err := hkdf.Key(sha256.New, dh, sec.K[:], string(info), 64)
	if err != nil {
		return nil, err
	}
	info = append(L("code"), th...)
	cb, err := hkdf.Key(sha256.New, dh, sec.K[:], string(info), 4)
	if err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(cb) % CodeModulo
	k := &Keys{Code: fmt.Sprintf("%03d %03d", n/1000, n%1000)}
	copy(k.A2V[:], kk[:32])
	copy(k.V2A[:], kk[32:])
	copy(k.TH[:], th)
	return k, nil
}

// AgentRespond validates VIEWER_HELLO m1 and, if authentic, returns the
// AGENT_HELLO m2 and the derived keys. No ephemeral key is generated and no
// randomness is consumed unless m1 authenticates. rnd supplies 32 bytes of
// X25519 scalar then 16 bytes of nonce (use crypto/rand.Reader in production).
func AgentRespond(sec *Secret, m1 []byte, rnd io.Reader) (m2 []byte, keys *Keys, err error) {
	if len(m1) != HelloLen || m1[0] != TypeM1 {
		return nil, nil, ErrMalformed
	}
	if m1[1] != Version {
		return nil, nil, ErrVersion
	}
	ka, err := sec.kAuth()
	if err != nil {
		return nil, nil, err
	}
	want := mac(ka, L("vh"), sec.SID[:], m1[:macOff])
	if subtle.ConstantTimeCompare(want, m1[macOff:]) != 1 {
		return nil, nil, ErrBadMAC
	}
	priv, err := newEphemeral(rnd)
	if err != nil {
		return nil, nil, err
	}
	m2 = make([]byte, HelloLen)
	m2[0], m2[1] = TypeM2, Version
	copy(m2[2:34], priv.PublicKey().Bytes())
	if _, err := io.ReadFull(rnd, m2[34:50]); err != nil {
		return nil, nil, err
	}
	copy(m2[macOff:], mac(ka, L("ah"), sec.SID[:], m1, m2[:macOff]))
	keys, err = deriveKeys(sec, priv, m1[2:34], m1, m2)
	if err != nil {
		return nil, nil, err
	}
	return m2, keys, nil
}

// ViewerState carries viewer-side handshake state between hello and finish.
type ViewerState struct {
	sec  *Secret
	priv *ecdh.PrivateKey
	m1   []byte
}

// ViewerHello builds VIEWER_HELLO. rnd supplies 32 bytes of X25519 scalar then
// 16 bytes of nonce.
func ViewerHello(sec *Secret, rnd io.Reader) (m1 []byte, st *ViewerState, err error) {
	priv, err := newEphemeral(rnd)
	if err != nil {
		return nil, nil, err
	}
	ka, err := sec.kAuth()
	if err != nil {
		return nil, nil, err
	}
	m1 = make([]byte, HelloLen)
	m1[0], m1[1] = TypeM1, Version
	copy(m1[2:34], priv.PublicKey().Bytes())
	if _, err := io.ReadFull(rnd, m1[34:50]); err != nil {
		return nil, nil, err
	}
	copy(m1[macOff:], mac(ka, L("vh"), sec.SID[:], m1[:macOff]))
	return m1, &ViewerState{sec: sec, priv: priv, m1: m1}, nil
}

// Finish verifies AGENT_HELLO m2 and derives the keys.
func (st *ViewerState) Finish(m2 []byte) (*Keys, error) {
	if len(m2) != HelloLen || m2[0] != TypeM2 {
		return nil, ErrMalformed
	}
	if m2[1] != Version {
		return nil, ErrVersion
	}
	ka, err := st.sec.kAuth()
	if err != nil {
		return nil, err
	}
	want := mac(ka, L("ah"), st.sec.SID[:], st.m1, m2[:macOff])
	if subtle.ConstantTimeCompare(want, m2[macOff:]) != 1 {
		return nil, ErrBadMAC
	}
	return deriveKeys(st.sec, st.priv, m2[2:34], st.m1, m2)
}
