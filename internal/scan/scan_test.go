// SPDX-License-Identifier: MIT

package scan

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

// ---- tree generation ----

// genTree builds a random tree: nDirs dirs (some deep chains, one wide dir,
// some empty) and nFiles files. It returns the sum of file sizes created.
func genTree(t testing.TB, root string, seed int64, nDirs, nFiles int) (fileBytes uint64) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	dirs := []string{root}
	for i := 0; i < nDirs; i++ {
		var parent string
		switch {
		case i < 20: // deep chain
			parent = dirs[len(dirs)-1]
		case rng.Intn(10) == 0:
			parent = dirs[len(dirs)-1]
		default:
			parent = dirs[rng.Intn(len(dirs))]
		}
		p := filepath.Join(parent, fmt.Sprintf("d%d", i))
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, p)
	}
	for i := 0; i < nFiles; i++ {
		d := dirs[rng.Intn(len(dirs))]
		if i < 200 { // one wide dir
			d = dirs[len(dirs)/2]
		}
		var size int
		if rng.Intn(3) > 0 {
			size = rng.Intn(30000)
		}
		if err := os.WriteFile(filepath.Join(d, fmt.Sprintf("f%d", i)), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		fileBytes += uint64(size)
	}
	return fileBytes
}

// ---- reference single-threaded walker ----

type refTotals struct {
	dirs, files, alloc, apparent, fileApparent uint64
}

func refWalk(t testing.TB, root string) refTotals {
	t.Helper()
	var tot refTotals
	seen := map[fkey]bool{}
	var walk func(p string)
	walk = func(p string) {
		var sc readScratch
		var kids []string
		self, err := readDirEntries(p, Identity{}, &sc, func(e *rawEntry) bool {
			if e.Err != nil {
				t.Fatalf("ref stat %s: %v", e.Name, e.Err)
			}
			if e.Kind == kindDir {
				if e.ChildFlags == 0 {
					kids = append(kids, filepath.Join(p, e.Name))
				}
				return true
			}
			if e.Dedup {
				k := fkey{e.Dev, e.Ino}
				if seen[k] {
					return true
				}
				seen[k] = true
			}
			tot.files++
			tot.alloc += e.Alloc
			tot.apparent += e.Apparent
			tot.fileApparent += e.Apparent
			return true
		})
		if err != nil {
			t.Fatalf("ref read %s: %v", p, err)
		}
		tot.dirs++
		tot.alloc += self.Alloc
		tot.apparent += self.Apparent
		for _, k := range kids {
			walk(k)
		}
	}
	walk(root)
	return tot
}

// ---- run + verify ----

type result struct {
	recs  []proto.Record
	stats Stats
	s     *Scanner
	// derived
	dirRecs                                map[uint64]*proto.DirRecord
	dirs, files, alloc, apparent, foldDirs uint64
	foldTargets                            map[uint64]bool
	maxID                                  uint64
	depth                                  map[uint64]int
}

func runScan(t testing.TB, opts Options) *result {
	t.Helper()
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	r := &result{s: s}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { // exercise Progress concurrently (race detector)
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.Progress()
				runtime.Gosched()
			}
		}
	}()
	r.stats, err = s.Run(context.Background(), func(rec proto.Record) { r.recs = append(r.recs, rec) })
	close(stop)
	<-done
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return r
}

func (r *result) verify(t testing.TB, opts Options) {
	t.Helper()
	announced := map[uint64]bool{0: true}
	pending := map[uint64]bool{}
	r.dirRecs = map[uint64]*proto.DirRecord{}
	r.foldTargets = map[uint64]bool{}
	r.depth = map[uint64]int{0: 0}
	ended := map[uint64]bool{}
	foldSeen := map[uint64]bool{}
	for _, rec := range r.recs {
		switch x := rec.(type) {
		case *proto.DirRecord:
			if !announced[x.ID] {
				t.Fatalf("dir %d recorded before announcement", x.ID)
			}
			if _, dup := r.dirRecs[x.ID]; dup {
				t.Fatalf("duplicate dir record %d", x.ID)
			}
			r.dirRecs[x.ID] = x
			delete(pending, x.ID)
			if x.ID > r.maxID {
				r.maxID = x.ID
			}
			if x.Flags&proto.DirFoldedChildren != 0 {
				r.foldTargets[x.ID] = true
				if len(x.Dirs) != 0 {
					t.Fatalf("folded dir %d announces children", x.ID)
				}
			}
			for i, c := range x.Dirs {
				id := x.FirstChildID + uint64(i)
				if announced[id] {
					t.Fatalf("child id %d announced twice", id)
				}
				announced[id] = true
				r.depth[id] = r.depth[x.ID] + 1
				if c.Flags == 0 {
					pending[id] = true
				}
				if id > r.maxID {
					r.maxID = id
				}
			}
			r.dirs++
			r.files += x.OwnFiles
			r.alloc += x.OwnAlloc
			r.apparent += x.OwnApparent
		case *proto.FoldRecord:
			if r.dirRecs[x.TargetID] == nil || !r.foldTargets[x.TargetID] {
				t.Fatalf("FOLD for unknown/unfolded target %d", x.TargetID)
			}
			if ended[x.TargetID] {
				t.Fatalf("FOLD after FOLDEND for %d", x.TargetID)
			}
			foldSeen[x.TargetID] = true
			r.dirs += x.Dirs
			r.foldDirs += x.Dirs
			r.files += x.Files
			r.alloc += x.Alloc
			r.apparent += x.Apparent
		case *proto.FoldEndRecord:
			if !r.foldTargets[x.TargetID] || ended[x.TargetID] {
				t.Fatalf("bad FOLDEND for %d", x.TargetID)
			}
			ended[x.TargetID] = true
		}
	}
	if len(pending) != 0 {
		t.Fatalf("%d announced children never recorded", len(pending))
	}
	for id := range r.foldTargets {
		if !ended[id] {
			t.Fatalf("missing FOLDEND for %d", id)
		}
	}
	if r.stats.Dirs != r.dirs || r.stats.Files != r.files || r.stats.Alloc != r.alloc || r.stats.Apparent != r.apparent {
		t.Fatalf("stats %+v disagree with records dirs=%d files=%d alloc=%d app=%d", r.stats, r.dirs, r.files, r.alloc, r.apparent)
	}
	if opts.MaxDirs > 0 && r.maxID >= opts.MaxDirs {
		t.Fatalf("announced id %d >= MaxDirs %d", r.maxID, opts.MaxDirs)
	}
}

func (r *result) compare(t testing.TB, ref refTotals) {
	t.Helper()
	if r.dirs != ref.dirs || r.files != ref.files || r.alloc != ref.alloc || r.apparent != ref.apparent {
		t.Fatalf("totals mismatch: got dirs=%d files=%d alloc=%d app=%d; want %+v", r.dirs, r.files, r.alloc, r.apparent, ref)
	}
}

// ---- tests ----

func TestRandomTrees(t *testing.T) {
	for seed := int64(1); seed <= 6; seed++ {
		for _, threads := range []int{1, 8} {
			t.Run(fmt.Sprintf("seed%d/t%d", seed, threads), func(t *testing.T) {
				root := t.TempDir()
				nd, nf := int(50*seed), int(300*seed)
				fb := genTree(t, root, seed, nd, nf)
				ref := refWalk(t, root)
				if ref.fileApparent != fb || ref.files != uint64(nf) || ref.dirs != uint64(nd+1) {
					t.Fatalf("reference disagrees with generator: %+v bytes=%d", ref, fb)
				}
				opts := Options{Root: root, Threads: threads, MinFileBytes: 1}
				r := runScan(t, opts)
				r.verify(t, r.s.opts)
				r.compare(t, ref)
				if r.foldDirs != 0 || r.stats.Folded != 0 {
					t.Fatalf("unexpected folds")
				}
				// PathOf round-trips for all announced dirs
				for id, rec := range r.dirRecs {
					p, ok := r.s.PathOf(id)
					if !ok {
						t.Fatalf("PathOf(%d) missing", id)
					}
					for i, c := range rec.Dirs {
						cp, ok := r.s.PathOf(rec.FirstChildID + uint64(i))
						if !ok || cp != filepath.Join(p, c.Name) {
							t.Fatalf("PathOf child: %q vs %q", cp, filepath.Join(p, c.Name))
						}
					}
				}
				if _, ok := r.s.PathOf(1 << 40); ok {
					t.Fatal("PathOf bogus id ok")
				}
			})
		}
	}
}

func TestEmptyAndSingle(t *testing.T) {
	root := t.TempDir()
	r := runScan(t, Options{Root: root})
	r.verify(t, r.s.opts)
	if r.dirs != 1 || r.files != 0 {
		t.Fatalf("got %d dirs %d files", r.dirs, r.files)
	}
	if _, err := New(Options{Root: filepath.Join(root, "nope")}); err == nil {
		t.Fatal("expected error for missing root")
	}
	f := filepath.Join(root, "file")
	os.WriteFile(f, []byte("x"), 0o644)
	if _, err := New(Options{Root: f}); err == nil {
		t.Fatal("expected error for non-dir root")
	}
}

func TestTopFiles(t *testing.T) {
	root := t.TempDir()
	for i, sz := range []int{5000, 100, 9000, 7000, 0, 20000} {
		os.WriteFile(filepath.Join(root, fmt.Sprintf("f%d", i)), make([]byte, sz), 0o644)
	}
	r := runScan(t, Options{Root: root, TopFiles: 3, MinFileBytes: 4096})
	r.verify(t, r.s.opts)
	rec := r.dirRecs[0]
	if len(rec.Files) != 3 {
		t.Fatalf("want 3 top files, got %d", len(rec.Files))
	}
	for i := 1; i < len(rec.Files); i++ {
		if rec.Files[i-1].Alloc < rec.Files[i].Alloc {
			t.Fatal("top files not sorted desc")
		}
	}
	if rec.Files[0].Apparent != 20000 {
		t.Fatalf("largest apparent = %d", rec.Files[0].Apparent)
	}
	if rec.OwnFiles != 6 {
		t.Fatalf("OwnFiles=%d", rec.OwnFiles)
	}
}

func TestHardlinks(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	os.Mkdir(a, 0o755)
	os.Mkdir(b, 0o755)
	f := filepath.Join(a, "f")
	os.WriteFile(f, make([]byte, 50000), 0o644)
	if err := os.Link(f, filepath.Join(a, "l2")); err != nil {
		t.Skip("hardlinks unsupported:", err)
	}
	if err := os.Link(f, filepath.Join(b, "l1")); err != nil {
		t.Skip("hardlinks unsupported:", err)
	}
	for _, th := range []int{1, 8} {
		r := runScan(t, Options{Root: root, Threads: th})
		r.verify(t, r.s.opts)
		if r.stats.Files != 1 || r.stats.HardlinksDeduped != 2 {
			t.Fatalf("threads=%d files=%d deduped=%d", th, r.stats.Files, r.stats.HardlinksDeduped)
		}
		r.compare(t, refWalk(t, root))
	}
}

func TestSymlinkLoop(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "a", "b"), 0o755)
	if err := os.Symlink(root, filepath.Join(root, "a", "b", "loop")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	os.Symlink("a", filepath.Join(root, "dirlink"))
	r := runScan(t, Options{Root: root})
	r.verify(t, r.s.opts)
	if r.dirs != 3 {
		t.Fatalf("symlink followed? dirs=%d", r.dirs)
	}
	if r.files != 2 {
		t.Fatalf("symlinks should count as files, got %d", r.files)
	}
}

func TestDenied(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs non-root unix")
	}
	root := t.TempDir()
	d := filepath.Join(root, "locked")
	os.Mkdir(d, 0o755)
	os.WriteFile(filepath.Join(d, "x"), []byte("hello"), 0o644)
	os.Mkdir(filepath.Join(root, "ok"), 0o755)
	if err := os.Chmod(d, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(d, 0o755)
	r := runScan(t, Options{Root: root})
	r.verify(t, r.s.opts)
	if r.stats.Denied != 1 {
		t.Fatalf("Denied=%d", r.stats.Denied)
	}
	n := 0
	for _, rec := range r.dirRecs {
		if rec.Flags&proto.DirReadDenied != 0 {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("READ_DENIED records = %d", n)
	}
	res, err := ListDirFiles(d, 10)
	if err != nil || res.Flags&proto.DirReadDenied == 0 {
		t.Fatalf("ListDirFiles denied: %v %+v", err, res)
	}
}

func TestVanishedMidScan(t *testing.T) {
	root := t.TempDir()
	genTree(t, root, 7, 40, 100)
	victim := filepath.Join(root, "victim")
	os.MkdirAll(filepath.Join(victim, "sub"), 0o755)
	os.WriteFile(filepath.Join(victim, "f"), []byte("x"), 0o644)
	hookBeforeOpen = func(p string) {
		if p == victim {
			os.RemoveAll(victim)
		}
	}
	defer func() { hookBeforeOpen = nil }()
	r := runScan(t, Options{Root: root})
	hookBeforeOpen = nil
	r.verify(t, r.s.opts)
	if r.stats.Vanished != 1 {
		t.Fatalf("Vanished=%d", r.stats.Vanished)
	}
	found := false
	for _, rec := range r.dirRecs {
		if rec.Flags&proto.DirVanished != 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("no VANISHED record")
	}
}

func TestInvalidUTF8(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "bad\xff\xfename"), make([]byte, 2<<20), 0o644); err != nil {
		t.Skip("fs rejects invalid utf8:", err)
	}
	r := runScan(t, Options{Root: root})
	r.verify(t, r.s.opts)
	rec := r.dirRecs[0]
	if len(rec.Files) != 1 || rec.Files[0].Flags&proto.FileBadName == 0 {
		t.Fatalf("expected BAD_NAME top file: %+v", rec.Files)
	}
	res, err := ListDirFiles(root, 10)
	if err != nil || len(res.Files) != 1 || res.Files[0].Flags&proto.FileBadName == 0 {
		t.Fatalf("ListDirFiles: %v %+v", err, res)
	}
}

func TestSparse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix only")
	}
	root := t.TempDir()
	p := filepath.Join(root, "sparse")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(64 << 20)
	f.Close()
	os.WriteFile(filepath.Join(root, "dense"), make([]byte, 100000), 0o644)
	res, err := ListDirFiles(root, 10)
	if err != nil {
		t.Fatal(err)
	}
	var sp *proto.FileEntry
	for i := range res.Files {
		if res.Files[i].Name == "sparse" {
			sp = &res.Files[i]
		}
	}
	if sp == nil || sp.Apparent != 64<<20 {
		t.Fatalf("sparse missing: %+v", res.Files)
	}
	if sp.Alloc >= sp.Apparent {
		t.Skip("filesystem does not support sparse files")
	}
	if sp.Flags&proto.FileSparse == 0 {
		t.Fatalf("SPARSE flag missing: %+v", sp)
	}
	for _, f := range res.Files {
		if f.Name == "dense" && f.Flags&proto.FileSparse != 0 {
			t.Fatal("dense file flagged sparse")
		}
	}
	// limit → rest bucket
	res1, _ := ListDirFiles(root, 1)
	if len(res1.Files) != 1 || res1.RestCount != 1 {
		t.Fatalf("limit/rest: %+v", res1)
	}
}

func TestListSubdirs(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "b"), 0o755)
	os.Mkdir(filepath.Join(root, "a"), 0o755)
	os.WriteFile(filepath.Join(root, "file"), nil, 0o644)
	os.Symlink("a", filepath.Join(root, "link"))
	got, err := ListSubdirs(root)
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestFoldMaxDirs(t *testing.T) {
	root := t.TempDir()
	genTree(t, root, 11, 500, 1500)
	ref := refWalk(t, root)
	for _, th := range []int{1, 8} {
		opts := Options{Root: root, MaxDirs: 50, Threads: th}
		r := runScan(t, opts)
		r.verify(t, r.s.opts)
		r.compare(t, ref)
		if len(r.foldTargets) == 0 || r.foldDirs == 0 || r.stats.Folded != r.foldDirs {
			t.Fatalf("no folding happened: targets=%d folded=%d stat=%d", len(r.foldTargets), r.foldDirs, r.stats.Folded)
		}
	}
}

// A directory with so many subdirectories that announcing them would exceed
// the protocol's per-message limit must fold them instead of failing.
func TestFoldOversizeChildList(t *testing.T) {
	root := t.TempDir()
	wide := filepath.Join(root, "wide")
	name := strings.Repeat("n", 240)
	n := maxChildRecordBytes/(len(name)+8) + 200
	for i := 0; i < n; i++ {
		d := filepath.Join(wide, fmt.Sprintf("%s%06d", name, i))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if i%500 == 0 {
			if err := os.WriteFile(filepath.Join(d, "f"), make([]byte, 5000), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	ref := refWalk(t, root)
	opts := Options{Root: root, Threads: 8}
	r := runScan(t, opts)
	r.verify(t, r.s.opts)
	r.compare(t, ref)
	if len(r.foldTargets) != 1 || r.foldDirs != uint64(n) {
		t.Fatalf("want 1 fold target with %d dirs, got targets=%d dirs=%d", n, len(r.foldTargets), r.foldDirs)
	}
	for _, rec := range r.recs {
		if d, ok := rec.(*proto.DirRecord); ok {
			sz := 0
			for _, c := range d.Dirs {
				sz += len(c.Name) + 4
			}
			if sz > maxChildRecordBytes {
				t.Fatalf("dir %d announces %d bytes of children", d.ID, sz)
			}
		}
	}
}

func TestFoldMaxDepth(t *testing.T) {
	root := t.TempDir()
	genTree(t, root, 12, 500, 1500)
	ref := refWalk(t, root)
	for _, th := range []int{1, 8} {
		r := runScan(t, Options{Root: root, MaxDepth: 3, Threads: th})
		r.verify(t, r.s.opts)
		r.compare(t, ref)
		if len(r.foldTargets) == 0 {
			t.Fatal("no folding happened")
		}
		for id := range r.dirRecs {
			if d := r.depth[id]; d > 3 {
				t.Fatalf("dir %d announced at depth %d", id, d)
			}
		}
	}
}

func TestCancel(t *testing.T) {
	root := t.TempDir()
	genTree(t, root, 21, 3000, 3000)
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()
	s, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var n atomic.Int64
	var returned atomic.Bool
	var late atomic.Bool
	start := time.Now()
	_, err = s.Run(ctx, func(proto.Record) {
		if returned.Load() {
			late.Store(true)
		}
		if n.Add(1) == 20 {
			cancel()
		}
		time.Sleep(time.Millisecond)
	})
	returned.Store(true)
	if err != context.Canceled {
		t.Fatalf("err=%v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("cancel took %v", el)
	}
	if n.Load() > 200 {
		t.Fatalf("too many records after cancel: %d", n.Load())
	}
	time.Sleep(100 * time.Millisecond)
	if late.Load() {
		t.Fatal("emit after return")
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > base {
		t.Fatalf("goroutines %d > baseline %d", g, base)
	}
	// already-cancelled ctx
	s2, _ := New(Options{Root: root})
	if _, err := s2.Run(ctx, func(proto.Record) {}); err == nil {
		t.Fatal("expected error for cancelled ctx")
	}
}

func TestRunTwice(t *testing.T) {
	s, _ := New(Options{Root: t.TempDir()})
	if _, err := s.Run(context.Background(), func(proto.Record) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run(context.Background(), func(proto.Record) {}); err == nil {
		t.Fatal("second Run should fail")
	}
}

func TestVolumes(t *testing.T) {
	v, err := Volumes()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) == 0 {
		t.Fatal("no volumes")
	}
	for _, x := range v {
		if x.Path == "" || x.Total == 0 && x.Kind != "network" {
			t.Logf("odd volume: %+v", x)
		}
	}
}

func TestRootGuard(t *testing.T) {
	base := t.TempDir()
	b := filepath.Join(base, "a", "b")
	bc := filepath.Join(base, "a", "bc")
	os.MkdirAll(filepath.Join(b, "sub"), 0o755)
	os.MkdirAll(bc, 0o755)
	g, err := NewRootGuard(b)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		p    string
		want bool
	}{
		{b, true},
		{filepath.Join(b, "sub"), true},
		{bc, false},
		{filepath.Join(b, "..", "bc"), false},
		{filepath.Join(b, "sub", "..", ".."), false},
		{filepath.Join(b, "sub", "..", "sub"), true},
		{filepath.Join(b, "missing"), false},
		{base, false},
		{string(filepath.Separator), false},
	}
	for _, c := range cases {
		if got := g.Within(c.p); got != c.want {
			t.Errorf("Within(%q)=%v want %v", c.p, got, c.want)
		}
	}
	if err := os.Symlink(bc, filepath.Join(b, "esc")); err == nil {
		if g.Within(filepath.Join(b, "esc")) {
			t.Error("symlink escape accepted")
		}
		os.Symlink(filepath.Join(b, "sub"), filepath.Join(b, "in"))
		if !g.Within(filepath.Join(b, "in")) {
			t.Error("internal symlink rejected")
		}
	}
	var nilg *RootGuard
	if !nilg.Within("/anything") {
		t.Error("nil guard must allow")
	}
}

// ---- benchmarks ----

func benchScan(b *testing.B, root string, cross bool) {
	b.Helper()
	var st Stats
	for i := 0; i < b.N; i++ {
		s, err := New(Options{Root: root, CrossMounts: cross})
		if err != nil {
			b.Fatal(err)
		}
		st, err = s.Run(context.Background(), func(proto.Record) {})
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(st.Dirs), "dirs")
	b.ReportMetric(float64(st.Files), "files")
	b.ReportMetric(float64(st.Denied), "denied")
}

// BenchmarkScan scans an explicit directory only (never an implicit real
// location): DISKSPACE_BENCH_DIR=<tree built by scripts/mktree>.
func BenchmarkScan(b *testing.B) {
	dir := os.Getenv("DISKSPACE_BENCH_DIR")
	if dir == "" {
		b.Skip("set DISKSPACE_BENCH_DIR to a synthetic tree (go run ./scripts/mktree)")
	}
	benchScan(b, dir, false)
}
