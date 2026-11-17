// SPDX-License-Identifier: MIT

package session

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

const (
	defaultLogCap    = 256 << 20
	nodesMaxBody     = 64 << 10
	nodesMaxAge      = 100 * time.Millisecond
	nodesFirstAge    = 30 * time.Millisecond
	errBodyTooLarge  = "a directory record exceeds the 1 MiB message limit"
	keepRawSegments  = 2
	segCompressLevel = zstd.SpeedFastest
)

// segmentSize is the raw size at which a log segment is sealed (var for tests).
var segmentSize = 4 << 20

var (
	logEnc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(segCompressLevel), zstd.WithEncoderConcurrency(2))
	logDec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(2), zstd.WithDecoderMaxMemory(64<<20))

	errReplayUnavailable = errors.New("session: replay unavailable")
)

type segment struct {
	first      int      // absolute index of the first entry
	ents       [][]byte // raw entries; nil once compressed
	sizes      []int    // entry lengths (kept after compression)
	rawSize    int
	sealed     bool
	compressed bool // comp valid
	compacting bool
	comp       []byte
}

func (s *segment) retained() int64 {
	if s.compressed {
		return int64(len(s.comp))
	}
	return int64(s.rawSize)
}

// logEnd is the terminal message of a scan log.
type logEnd struct {
	Body []byte // SCAN_DONE or SCAN_ENDED body
	Done bool   // SCAN_DONE (all NODES must be delivered first)
}

// scanLog is the append-only per-scan record of NODES bodies. Viewers read it
// through logReaders, so replay and the live tail are the same code path.
type scanLog struct {
	scanID uint64
	cap    int64

	mu      sync.Mutex
	segs    []*segment
	count   int // total entries ever appended
	first   int // absolute index of the first retained entry
	notify  chan struct{}
	end     *logEnd
	readers map[*logReader]struct{}
}

func newScanLog(scanID uint64, capBytes int64) *scanLog {
	if capBytes <= 0 {
		capBytes = defaultLogCap
	}
	return &scanLog{scanID: scanID, cap: capBytes, notify: make(chan struct{}), readers: map[*logReader]struct{}{}}
}

func (l *scanLog) broadcastLocked() {
	close(l.notify)
	l.notify = make(chan struct{})
}

// append stores one NODES body (the slice is retained, callers must not reuse it).
func (l *scanLog) append(body []byte) {
	l.mu.Lock()
	var seg *segment
	if n := len(l.segs); n > 0 {
		seg = l.segs[n-1]
	}
	if seg == nil || (seg.rawSize > 0 && seg.rawSize+len(body) > segmentSize) {
		if seg != nil {
			seg.sealed = true
		}
		seg = &segment{first: l.count}
		l.segs = append(l.segs, seg)
		l.compactLocked()
	}
	seg.ents = append(seg.ents, body)
	seg.sizes = append(seg.sizes, len(body))
	seg.rawSize += len(body)
	l.count++
	l.trimLocked()
	l.broadcastLocked()
	l.mu.Unlock()
}

// compactLocked schedules background compression of every sealed segment
// except the newest keepRawSegments.
func (l *scanLog) compactLocked() {
	for i := 0; i < len(l.segs)-keepRawSegments; i++ {
		s := l.segs[i]
		if !s.sealed || s.compressed || s.compacting {
			continue
		}
		s.compacting = true
		ents := s.ents
		go func() {
			total := 0
			for _, e := range ents {
				total += len(e)
			}
			cat := make([]byte, 0, total)
			for _, e := range ents {
				cat = append(cat, e...)
			}
			comp := logEnc.EncodeAll(cat, nil)
			l.mu.Lock()
			s.comp, s.compressed, s.ents = comp, true, nil
			l.trimLocked()
			l.mu.Unlock()
		}()
	}
}

func (l *scanLog) retainedLocked() int64 {
	var t int64
	for _, s := range l.segs {
		t += s.retained()
	}
	return t
}

// trimLocked enforces the cap: past it, sealed segments are dropped once every
// registered reader has moved beyond them.
func (l *scanLog) trimLocked() {
	if len(l.segs) < 2 || l.retainedLocked() <= l.cap {
		return
	}
	min := l.count
	for r := range l.readers {
		if c := int(r.cursor.Load()); c < min {
			min = c
		}
	}
	for len(l.segs) > 1 && l.retainedLocked() > l.cap {
		s := l.segs[0]
		if !s.sealed || s.first+len(s.sizes) > min {
			break
		}
		l.segs = l.segs[1:]
		l.first = l.segs[0].first
	}
}

// finish records the terminal message. Call only after every NODES body has
// been appended.
func (l *scanLog) finish(e logEnd) {
	l.mu.Lock()
	if l.end == nil {
		l.end = &logEnd{Body: e.Body, Done: e.Done}
		l.broadcastLocked()
	}
	l.mu.Unlock()
}

func (l *scanLog) countNow() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}

// logReader is one viewer's cursor into a scanLog. Not goroutine-safe except
// close.
type logReader struct {
	l      *scanLog
	cursor atomic.Int64
	seg    *segment
	dec    []byte // decompressed contents of seg (when seg is compressed)
	decSeg *segment
	closed bool
}

// newReader registers a reader at entry 0. It fails when the log has already
// dropped its head (cap exceeded).
func (l *scanLog) newReader() (*logReader, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.first > 0 {
		return nil, errReplayUnavailable
	}
	r := &logReader{l: l}
	l.readers[r] = struct{}{}
	return r, nil
}

func (r *logReader) close() {
	r.l.mu.Lock()
	delete(r.l.readers, r)
	r.l.trimLocked()
	r.l.mu.Unlock()
}

func (r *logReader) pos() int { return int(r.cursor.Load()) }

// poll returns the entry at the cursor, or (nil, wait, end): wait is closed on
// the next append/finish; end is non-nil when the log is finished and the
// reader has consumed everything.
func (r *logReader) poll() (body []byte, wait <-chan struct{}, end *logEnd) {
	l := r.l
	l.mu.Lock()
	wait = l.notify
	cur := int(r.cursor.Load())
	if cur >= l.count {
		end = l.end
		l.mu.Unlock()
		return nil, wait, end
	}
	if cur < l.first {
		// Cannot happen while registered; fail closed.
		l.mu.Unlock()
		return nil, wait, &logEnd{}
	}
	seg := r.seg
	if seg == nil || cur < seg.first || cur >= seg.first+len(seg.sizes) {
		seg = nil
		for _, s := range l.segs {
			if cur >= s.first && cur < s.first+len(s.sizes) {
				seg = s
				break
			}
		}
		r.seg = seg
	}
	if seg == nil {
		l.mu.Unlock()
		return nil, wait, &logEnd{}
	}
	idx := cur - seg.first
	if !seg.compressed {
		b := seg.ents[idx]
		l.mu.Unlock()
		return b, nil, nil
	}
	comp, sizes := seg.comp, seg.sizes
	l.mu.Unlock()
	if r.decSeg != seg {
		d, err := logDec.DecodeAll(comp, nil)
		if err != nil {
			return nil, wait, &logEnd{}
		}
		r.dec, r.decSeg = d, seg
	}
	off := 0
	for i := 0; i < idx; i++ {
		off += sizes[i]
	}
	return r.dec[off : off+sizes[idx]], nil, nil
}

// advance moves past the entry last returned by poll.
func (r *logReader) advance() {
	r.l.mu.Lock()
	c := r.cursor.Add(1)
	if r.seg != nil && int(c) >= r.seg.first+len(r.seg.sizes) {
		r.l.trimLocked()
	}
	r.l.mu.Unlock()
}

// ---- batcher ----

// runBatcher consumes scanner records, packs them into NODES bodies (64 KiB or
// 100 ms; first message 30 ms) and appends them to the log. It never waits on
// network I/O. It returns when recs is closed, after the final flush. tooBig
// is called if a single record cannot fit in one message.
func (l *scanLog) runBatcher(recs <-chan proto.Record, tooBig func()) {
	enc := proto.NewNodesEncoder(l.scanID)
	first := true
	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer, timerC = nil, nil
		}
	}
	flush := func() {
		stopTimer()
		if enc.Count() == 0 {
			return
		}
		body := append([]byte(nil), enc.Bytes()...)
		enc.Reset(l.scanID)
		first = false
		if len(body) > proto.MaxBody {
			tooBig()
			return
		}
		l.append(body)
	}
	for {
		select {
		case rec, ok := <-recs:
			if !ok {
				flush()
				return
			}
			enc.Add(rec)
			if enc.Len() >= nodesMaxBody {
				flush()
			} else if enc.Count() == 1 {
				d := nodesMaxAge
				if first {
					d = nodesFirstAge
				}
				timer = time.NewTimer(d)
				timerC = timer.C
			}
		case <-timerC:
			timer, timerC = nil, nil
			flush()
		}
	}
}

// snapshot returns the entry count and whether the log has finished, atomically.
func (l *scanLog) snapshot() (count int, ended bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count, l.end != nil
}

// endNow returns the terminal message if the log is finished.
func (l *scanLog) endNow() *logEnd {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.end
}
