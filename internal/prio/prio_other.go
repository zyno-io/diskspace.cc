// SPDX-License-Identifier: MIT

//go:build !darwin && !linux && !windows

package prio

func applyProcessLow() error { return nil }
func lockThreadLow() error   { return nil }
