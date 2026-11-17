// SPDX-License-Identifier: MIT

package crypto

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/vectors")

const vectorDir = "../../../testdata/vectors"

// detRand is a deterministic byte stream: sha256(seed || ctr).
type detRand struct {
	seed string
	ctr  uint64
	buf  []byte
}

func newDet(seed string) *detRand { return &detRand{seed: seed} }

func (d *detRand) Read(p []byte) (int, error) {
	for len(d.buf) < len(p) {
		var c [8]byte
		binary.BigEndian.PutUint64(c[:], d.ctr)
		d.ctr++
		h := sha256.Sum256(append([]byte(d.seed), c[:]...))
		d.buf = append(d.buf, h[:]...)
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}

func hx(b []byte) string { return hex.EncodeToString(b) }

func testSecret(t testing.TB, seed string) *Secret {
	t.Helper()
	var s Secret
	r := newDet(seed)
	r.Read(s.SID[:])
	r.Read(s.K[:])
	return &s
}

func handshake(t testing.TB, sec *Secret, seed string) (m1, m2 []byte, ak, vk *Keys) {
	t.Helper()
	m1, st, err := ViewerHello(sec, newDet(seed+"/viewer"))
	if err != nil {
		t.Fatal(err)
	}
	m2, ak, err = AgentRespond(sec, m1, newDet(seed+"/agent"))
	if err != nil {
		t.Fatal(err)
	}
	vk, err = st.Finish(m2)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestRoundTrip(t *testing.T) {
	sec, _ := NewSecret()
	if len(sec.SIDString()) != 22 || len(sec.KString()) != 43 {
		t.Fatal("encoded lengths")
	}
	p, err := ParseSecret(sec.SIDString(), sec.KString())
	if err != nil || *p != *sec {
		t.Fatal("ParseSecret", err)
	}
	if _, err := ParseSecret("x", sec.KString()); err == nil {
		t.Fatal("expected error")
	}
	m1, st, _ := ViewerHello(sec, newDet("a"))
	m2, ak, err := AgentRespond(sec, m1, newDet("b"))
	if err != nil {
		t.Fatal(err)
	}
	vk, err := st.Finish(m2)
	if err != nil {
		t.Fatal(err)
	}
	if *ak != *vk {
		t.Fatal("keys differ")
	}
	if len(ak.Code) != 7 || ak.Code[3] != ' ' {
		t.Fatalf("code %q", ak.Code)
	}
	if ak.A2V == ak.V2A {
		t.Fatal("directional keys equal")
	}
}

func TestHandshakeTamper(t *testing.T) {
	sec := testSecret(t, "tamper")
	m1, st, _ := ViewerHello(sec, newDet("v"))
	// Flip every bit of M1.
	for i := 0; i < len(m1)*8; i++ {
		b := bytes.Clone(m1)
		b[i/8] ^= 1 << (i % 8)
		_, _, err := AgentRespond(sec, b, newDet("a"))
		want := ErrBadMAC
		switch i / 8 {
		case 0:
			want = ErrMalformed
		case 1:
			want = ErrVersion
		}
		if !errors.Is(err, want) {
			t.Fatalf("m1 bit %d: got %v want %v", i, err, want)
		}
	}
	for _, l := range []int{0, 81, 83} {
		if _, _, err := AgentRespond(sec, make([]byte, l), newDet("a")); !errors.Is(err, ErrMalformed) {
			t.Fatalf("len %d: %v", l, err)
		}
	}
	// Substituted pv with otherwise intact MAC.
	b := bytes.Clone(m1)
	copy(b[2:34], bytes.Repeat([]byte{9}, 32))
	if _, _, err := AgentRespond(sec, b, newDet("a")); !errors.Is(err, ErrBadMAC) {
		t.Fatal(err)
	}
	// Wrong K / wrong sid.
	other := *sec
	other.K[0] ^= 1
	if _, _, err := AgentRespond(&other, m1, newDet("a")); !errors.Is(err, ErrBadMAC) {
		t.Fatal(err)
	}
	other = *sec
	other.SID[0] ^= 1
	if _, _, err := AgentRespond(&other, m1, newDet("a")); !errors.Is(err, ErrBadMAC) {
		t.Fatal(err)
	}
	// Version 2 (even with a valid MAC for it).
	v2 := bytes.Clone(m1)
	v2[1] = 2
	if _, _, err := AgentRespond(sec, v2, newDet("a")); !errors.Is(err, ErrVersion) {
		t.Fatal(err)
	}
	// M2 tamper: every bit.
	m2, _, err := AgentRespond(sec, m1, newDet("a"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(m2)*8; i++ {
		b := bytes.Clone(m2)
		b[i/8] ^= 1 << (i % 8)
		_, err := st.Finish(b)
		want := ErrBadMAC
		switch i / 8 {
		case 0:
			want = ErrMalformed
		case 1:
			want = ErrVersion
		}
		if !errors.Is(err, want) {
			t.Fatalf("m2 bit %d: got %v want %v", i, err, want)
		}
	}
	// Viewer with wrong K cannot verify M2.
	wrong := *sec
	wrong.K[5] ^= 1
	_, st2, _ := ViewerHello(&wrong, newDet("v"))
	if _, err := st2.Finish(m2); !errors.Is(err, ErrBadMAC) {
		t.Fatal(err)
	}
	// M2 from a different transcript (other viewer nonce) fails.
	m1b, _, _ := ViewerHello(sec, newDet("v2"))
	m2b, _, _ := AgentRespond(sec, m1b, newDet("a"))
	if _, err := st.Finish(m2b); !errors.Is(err, ErrBadMAC) {
		t.Fatal(err)
	}
}

func TestAllZeroPeerKey(t *testing.T) {
	// A MAC-valid M1 carrying a low-order point must not yield keys.
	sec := testSecret(t, "zero")
	m1, _, _ := ViewerHello(sec, newDet("v"))
	ka, _ := sec.kAuth()
	copy(m1[2:34], make([]byte, 32))
	copy(m1[50:], mac(ka, L("vh"), sec.SID[:], m1[:50]))
	if _, _, err := AgentRespond(sec, m1, newDet("a")); err == nil {
		t.Fatal("expected failure on all-zero pv")
	}
}

func FuzzAgentRespond(f *testing.F) {
	sec := testSecret(f, "fuzz")
	m1, _, _ := ViewerHello(sec, newDet("v"))
	f.Add(m1)
	f.Add([]byte{})
	f.Add(make([]byte, 82))
	f.Fuzz(func(t *testing.T, b []byte) {
		m2, k, err := AgentRespond(sec, b, newDet("a"))
		if err == nil && (m2 == nil || k == nil) {
			t.Fatal("nil result without error")
		}
		if err == nil && !bytes.Equal(b, m1) {
			t.Fatal("accepted a message that is not the authentic M1")
		}
	})
}

// ---- vectors ----

type hsVector struct {
	Name      string `json:"name"`
	SID       string `json:"sid"` // hex
	K         string `json:"k"`   // hex
	SIDB64    string `json:"sidB64"`
	KB64      string `json:"kB64"`
	ViewerRnd string `json:"viewerRnd"` // hex: 32B x25519 scalar || 16B nonce
	AgentRnd  string `json:"agentRnd"`
	M1        string `json:"m1"`
	M2        string `json:"m2"`
	TH        string `json:"th"`
	A2V       string `json:"a2v"`
	V2A       string `json:"v2a"`
	Code      string `json:"code"`
}

type frameItem struct {
	Dir        string `json:"dir"` // "a2v" or "v2a"
	Seq        int    `json:"seq"`
	Plaintext  string `json:"plaintext"`
	Ciphertext string `json:"ciphertext"`
}

type frameVector struct {
	Handshake string      `json:"handshake"` // name of handshake vector providing keys
	Frames    []frameItem `json:"frames"`
}

func rndBytes(seed string, n int) []byte {
	b := make([]byte, n)
	newDet(seed).Read(b)
	return b
}

func writeOrCompare(t *testing.T, name string, v any) {
	t.Helper()
	want, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.Join(vectorDir, name)
	if *update || os.Getenv("UPDATE_VECTORS") != "" {
		if err := os.MkdirAll(vectorDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from generated vectors (protocol change? run with -update)", name)
	}
}

func buildHandshakeVectors(t *testing.T) []hsVector {
	var out []hsVector
	for _, name := range []string{"v1-a", "v1-b"} {
		sec := testSecret(t, name)
		m1, m2, ak, _ := handshake(t, sec, name)
		out = append(out, hsVector{
			Name: name, SID: hx(sec.SID[:]), K: hx(sec.K[:]), SIDB64: sec.SIDString(), KB64: sec.KString(),
			ViewerRnd: hx(rndBytes(name+"/viewer", 48)), AgentRnd: hx(rndBytes(name+"/agent", 48)),
			M1: hx(m1), M2: hx(m2), TH: hx(ak.TH[:]), A2V: hx(ak.A2V[:]), V2A: hx(ak.V2A[:]), Code: ak.Code,
		})
	}
	return out
}

func TestVectors(t *testing.T) {
	hs := buildHandshakeVectors(t)
	writeOrCompare(t, "handshake.json", hs)

	sec := testSecret(t, "v1-a")
	_, _, ak, vk := handshake(t, sec, "v1-a")
	agent, viewer := NewAgentCipher(ak), NewViewerCipher(vk)
	fv := frameVector{Handshake: "v1-a"}
	pts := [][]byte{{0}, append([]byte{0, 0x01}, []byte(`{"proto":1}`)...), rndBytes("f/big", 3000), {1, 2, 3}}
	for i, pt := range pts {
		ct, _ := agent.Seal(pt)
		got, err := viewer.Open(ct)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatal("a2v roundtrip", err)
		}
		fv.Frames = append(fv.Frames, frameItem{"a2v", i, hx(pt), hx(ct)})
	}
	for i, pt := range pts[:3] {
		ct, _ := viewer.Seal(pt)
		got, err := agent.Open(ct)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatal("v2a roundtrip", err)
		}
		fv.Frames = append(fv.Frames, frameItem{"v2a", i, hx(pt), hx(ct)})
	}
	writeOrCompare(t, "frames.json", fv)
}
