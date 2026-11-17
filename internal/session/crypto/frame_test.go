// SPDX-License-Identifier: MIT

package crypto

import (
	"errors"
	"sync"
	"testing"
)

func pair(t *testing.T) (*Cipher, *Cipher) {
	_, _, ak, vk := handshake(t, testSecret(t, "frames"), "frames")
	return NewAgentCipher(ak), NewViewerCipher(vk)
}

func TestFrameRoundTrip(t *testing.T) {
	a, v := pair(t)
	for i := 0; i < 5; i++ {
		ct, _ := a.Seal([]byte{byte(i)})
		pt, err := v.Open(ct)
		if err != nil || pt[0] != byte(i) {
			t.Fatal(err)
		}
		ct, _ = v.Seal([]byte{byte(i), 1})
		if _, err := a.Open(ct); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFrameAttacks(t *testing.T) {
	a, v := pair(t)
	c0, _ := a.Seal([]byte("zero"))
	c1, _ := a.Seal([]byte("one"))
	c2, _ := a.Seal([]byte("two"))
	// reorder: seq 1 first
	if _, err := v.Open(c1); !errors.Is(err, ErrOpen) {
		t.Fatal("reorder accepted")
	}
	if _, err := v.Open(c0); err != nil {
		t.Fatal(err)
	}
	// duplicate
	if _, err := v.Open(c0); err == nil {
		t.Fatal("dup accepted")
	}
	// drop (skip c1)
	if _, err := v.Open(c2); err == nil {
		t.Fatal("drop accepted")
	}
	// reflect: a's own a2v frame fed back to a
	a2, _ := pair(t)
	ct, _ := a2.Seal([]byte("x"))
	if _, err := a2.Open(ct); err == nil {
		t.Fatal("reflect accepted")
	}
	// bitflip
	a3, v3 := pair(t)
	ct, _ = a3.Seal([]byte("hello"))
	ct[0] ^= 1
	if _, err := v3.Open(ct); err == nil {
		t.Fatal("tamper accepted")
	}
	// truncated
	if _, err := v3.Open(ct[:3]); err == nil {
		t.Fatal("short accepted")
	}
}

func TestSeqExhaustion(t *testing.T) {
	a, v := pair(t)
	a.SetSeqs(MaxSeq-1, 0)
	if _, err := a.Seal(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Seal(nil); !errors.Is(err, ErrSeqExhausted) {
		t.Fatal(err)
	}
	v.SetSeqs(0, MaxSeq)
	if _, err := v.Open([]byte("x")); !errors.Is(err, ErrSeqExhausted) {
		t.Fatal(err)
	}
}

func TestConcurrentSealOpen(t *testing.T) {
	a, v := pair(t)
	var wg sync.WaitGroup
	wg.Add(2)
	// a seals a2v while a opens v2a produced by v, concurrently.
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			a.Seal([]byte("x"))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			ct, _ := v.Seal([]byte("y"))
			if _, err := a.Open(ct); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
}
