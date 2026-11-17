// SPDX-License-Identifier: MIT

//go:build linux

package scan

import (
	"bufio"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func devOf(st *unix.Stat_t) uint64 { return uint64(st.Dev) }

func platFileFlags(st *unix.Stat_t) uint64 { return 0 }

type platState struct {
	root    string
	rootDev uint64
	all     bool
	mounts  map[string]struct{}
}

func (p *platState) init(root string, rootDev uint64, cross bool) {
	p.root, p.rootDev, p.all = root, rootDev, cross
	p.mounts = parseMountpoints()
	delete(p.mounts, root)
}

func (p *platState) allowedDev(dev uint64) bool        { return p.all || dev == p.rootDev }
func (p *platState) excluded(parent, name string) bool { return false }

// bindMount reports whether path is a mountpoint on an allowed device
// (i.e. a bind mount or same-device submount).
func (p *platState) bindMount(path string) bool {
	if len(p.mounts) == 0 {
		return false
	}
	_, ok := p.mounts[path]
	return ok
}

func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			v := 0
			ok := true
			for _, c := range []byte(s[i+1 : i+4]) {
				if c < '0' || c > '7' {
					ok = false
					break
				}
				v = v*8 + int(c-'0')
			}
			if ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func parseMountpoints() map[string]struct{} {
	m := map[string]struct{}{}
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		m[unescapeMount(fields[4])] = struct{}{}
	}
	return m
}
