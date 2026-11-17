// SPDX-License-Identifier: MIT

//go:build !darwin

package elevate

// ProbeFDA is only meaningful on macOS.
func ProbeFDA() FDAState { return FDAUnknown }

// OpenFDASettings is only meaningful on macOS.
func OpenFDASettings() error { return ErrUnsupported }
