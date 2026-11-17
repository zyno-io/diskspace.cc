// SPDX-License-Identifier: MIT

//go:build !windows

package elevate

// RunAsAdmin is Windows-only.
func RunAsAdmin(self string, args []string, show bool) (Child, error) {
	return nil, ErrUnsupported
}
