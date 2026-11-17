// SPDX-License-Identifier: MIT

package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

// Directories announced with child flags were never opened by the scanner and
// must not be reported as scanned (DIR_FILES refuses them).
func TestDirScannedFlagged(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "a"), 0o755)
	s, err := New(Options{Root: root, Threads: 2})
	if err != nil {
		t.Fatal(err)
	}
	var id uint64
	s.Run(context.Background(), func(r proto.Record) {
		if d, ok := r.(*proto.DirRecord); ok && len(d.Dirs) == 1 {
			id = d.FirstChildID
		}
	})
	if id == 0 || !s.DirScanned(0) || !s.DirScanned(id) {
		t.Fatalf("plain dir should be scanned (id=%d)", id)
	}
	e, _ := s.arena.get(id)
	s.arena.set(id, 0, e.name, e.ident, proto.ChildOtherDevice)
	if s.DirScanned(id) {
		t.Fatal("flagged dir reported as scanned")
	}
}
