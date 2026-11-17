// SPDX-License-Identifier: MIT

//go:build darwin

package scan

import (
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

const (
	dataVolume = "/System/Volumes/Data"
	sfDataless = 0x40000000 // SF_DATALESS
)

func devOf(st *unix.Stat_t) uint64 { return uint64(uint32(st.Dev)) }

func platFileFlags(st *unix.Stat_t) uint64 {
	if st.Flags&sfDataless != 0 {
		return proto.FileCloud
	}
	return 0
}

type platState struct {
	root    string
	all     bool
	allowed []uint64
	excl    map[string]struct{}
}

func statDev(path string) (uint64, bool) {
	var st unix.Stat_t
	if unix.Stat(path, &st) != nil {
		return 0, false
	}
	return devOf(&st), true
}

func (p *platState) init(root string, rootDev uint64, cross bool) {
	p.root, p.all = root, cross
	p.allowed = []uint64{rootDev}
	rd, ok1 := statDev("/")
	dd, ok2 := statDev(dataVolume)
	if ok1 && ok2 && (rootDev == rd || rootDev == dd) {
		p.allowed = []uint64{rd, dd}
	}
	// Firmlinks: when walking "/" the Data volume is reachable both through
	// the firmlinked paths and directly; skip the direct duplicates.
	p.excl = map[string]struct{}{}
	if root == "/" {
		if b, err := os.ReadFile("/usr/share/firmlinks"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				f := strings.Fields(line)
				if len(f) < 2 || strings.HasPrefix(f[0], "#") {
					continue
				}
				rhs := strings.TrimPrefix(f[1], "/")
				if i := strings.IndexByte(rhs, '/'); i >= 0 {
					rhs = rhs[:i]
				}
				if rhs != "" {
					p.excl[rhs] = struct{}{}
				}
			}
		}
	}
}

func (p *platState) allowedDev(dev uint64) bool {
	if p.all {
		return true
	}
	for _, d := range p.allowed {
		if d == dev {
			return true
		}
	}
	return false
}

func (p *platState) excluded(parent, name string) bool {
	if parent != dataVolume || len(p.excl) == 0 {
		return false
	}
	_, ok := p.excl[name]
	return ok
}

func (p *platState) bindMount(path string) bool { return false }
