// SPDX-License-Identifier: MIT

// Adapted from github.com/zyno-io/sp2p (MIT)

package relay

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Limits are the relay's resource limits. Zero fields take the defaults.
type Limits struct {
	MaxMsgA2V int64 // agent->viewer message size
	MaxMsgV2A int64 // viewer->agent message size
	MaxMsgCtl int64 // control channel message size

	UpgradesPerMinPerIP   int
	SessionsPerIP         int
	ViewerConnsPerMinSess int
	MaxPendingViewers     int
	MaxSplicedPairs       int

	PendingTimeout time.Duration
	PingInterval   time.Duration
	Grace          time.Duration
	TTL            time.Duration
	Window         time.Duration // sliding window for the per-minute limits

	BytesPerSec int64 // per-session token bucket
}

// DefaultLimits returns the production limits.
func DefaultLimits() Limits {
	return Limits{
		MaxMsgA2V:             1<<20 + 4<<10,
		MaxMsgV2A:             64 << 10,
		MaxMsgCtl:             4 << 10,
		UpgradesPerMinPerIP:   60,
		SessionsPerIP:         8,
		ViewerConnsPerMinSess: 30,
		MaxPendingViewers:     4,
		MaxSplicedPairs:       2,
		PendingTimeout:        10 * time.Second,
		PingInterval:          20 * time.Second,
		Grace:                 60 * time.Second,
		TTL:                   24 * time.Hour,
		Window:                time.Minute,
		BytesPerSec:           50 << 20,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxMsgA2V > 0 {
		d.MaxMsgA2V = l.MaxMsgA2V
	}
	if l.MaxMsgV2A > 0 {
		d.MaxMsgV2A = l.MaxMsgV2A
	}
	if l.MaxMsgCtl > 0 {
		d.MaxMsgCtl = l.MaxMsgCtl
	}
	if l.UpgradesPerMinPerIP > 0 {
		d.UpgradesPerMinPerIP = l.UpgradesPerMinPerIP
	}
	if l.SessionsPerIP > 0 {
		d.SessionsPerIP = l.SessionsPerIP
	}
	if l.ViewerConnsPerMinSess > 0 {
		d.ViewerConnsPerMinSess = l.ViewerConnsPerMinSess
	}
	if l.MaxPendingViewers > 0 {
		d.MaxPendingViewers = l.MaxPendingViewers
	}
	if l.MaxSplicedPairs > 0 {
		d.MaxSplicedPairs = l.MaxSplicedPairs
	}
	if l.PendingTimeout > 0 {
		d.PendingTimeout = l.PendingTimeout
	}
	if l.PingInterval > 0 {
		d.PingInterval = l.PingInterval
	}
	if l.Grace > 0 {
		d.Grace = l.Grace
	}
	if l.TTL > 0 {
		d.TTL = l.TTL
	}
	if l.Window > 0 {
		d.Window = l.Window
	}
	if l.BytesPerSec > 0 {
		d.BytesPerSec = l.BytesPerSec
	}
	return d
}

// slidingWindow counts events per key within a window.
type slidingWindow struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	events map[string][]time.Time
}

func newSlidingWindow(n int, window time.Duration) *slidingWindow {
	return &slidingWindow{n: n, window: window, events: map[string][]time.Time{}}
}

func (w *slidingWindow) allow(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	ev := w.prune(key, now)
	if len(ev) >= w.n {
		w.events[key] = ev
		return false
	}
	w.events[key] = append(ev, now)
	return true
}

func (w *slidingWindow) prune(key string, now time.Time) []time.Time {
	ev := w.events[key]
	i := 0
	for i < len(ev) && now.Sub(ev[i]) >= w.window {
		i++
	}
	return ev[i:]
}

func (w *slidingWindow) gc() {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	for k := range w.events {
		if ev := w.prune(k, now); len(ev) == 0 {
			delete(w.events, k)
		} else {
			w.events[k] = ev
		}
	}
}

// bucket is a token bucket measured in bytes. Tokens may go negative, so a
// single message larger than the rate is allowed but delays later ones.
type bucket struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

func newBucket(rate int64) *bucket {
	return &bucket{rate: float64(rate), tokens: float64(rate), last: time.Now()}
}

func (b *bucket) wait(ctx context.Context, n int) error {
	b.mu.Lock()
	now := time.Now()
	b.tokens += now.Sub(b.last).Seconds() * b.rate
	if b.tokens > b.rate {
		b.tokens = b.rate
	}
	b.last = now
	b.tokens -= float64(n)
	var d time.Duration
	if b.tokens < 0 {
		d = time.Duration(-b.tokens / b.rate * float64(time.Second))
	}
	b.mu.Unlock()
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// clientIP extracts the client address. With trustProxy it prefers X-Real-IP,
// then the last X-Forwarded-For entry (the one appended by our own proxy).
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(v) != nil {
			return v
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			parts := strings.Split(v, ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(last) != nil {
				return last
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
