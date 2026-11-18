// SPDX-License-Identifier: MIT

package scan

// Pause stops workers from starting new directories. Directory reads already
// in flight finish; no file descriptors are held while paused. Safe to call at
// any time, from any goroutine, including before Run.
func (s *Scanner) Pause() {
	s.pauseMu.Lock()
	s.paused.Store(true)
	s.pauseMu.Unlock()
}

// Resume lets paused workers continue.
func (s *Scanner) Resume() {
	s.pauseMu.Lock()
	s.paused.Store(false)
	s.pauseCond.Broadcast()
	s.pauseMu.Unlock()
}

// Paused reports whether the scanner is paused.
func (s *Scanner) Paused() bool { return s.paused.Load() }

// gate blocks while the scanner is paused. It returns false when the scan was
// cancelled (the caller must drop its job without processing it). The common
// case is one atomic load.
func (s *Scanner) gate() bool {
	if !s.paused.Load() {
		return !s.cancelled.Load()
	}
	s.pauseMu.Lock()
	for s.paused.Load() && !s.cancelled.Load() {
		s.pauseCond.Wait()
	}
	s.pauseMu.Unlock()
	return !s.cancelled.Load()
}
