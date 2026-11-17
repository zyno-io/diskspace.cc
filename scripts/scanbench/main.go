// SPDX-License-Identifier: MIT

// Command scanbench times a scan of one directory and prints aggregate
// statistics only (no file names). Use it on synthetic trees from
// scripts/mktree or on a dedicated test machine.
//
//	go run ./scripts/scanbench [-cross] [-threads N] [-folds N] <dir>
//
// -folds N prints the N largest folded subtrees by path (opt-in, since it
// prints directory names).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/zyno-io/diskspace.cc/internal/proto"
	"github.com/zyno-io/diskspace.cc/internal/scan"
)

func main() {
	cross := flag.Bool("cross", false, "cross mount points")
	threads := flag.Int("threads", 0, "worker threads (0 = default)")
	folds := flag.Int("folds", 0, "print the N largest folded subtrees (prints paths)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: scanbench [-cross] [-threads N] <dir>")
		os.Exit(2)
	}
	root, err := filepath.Abs(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	s, err := scan.New(scan.Options{Root: root, CrossMounts: *cross, Threads: *threads})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var records int
	type node struct {
		parent uint64
		name   string
	}
	nodes := map[uint64]node{}
	foldDirs := map[uint64]uint64{}
	start := time.Now()
	st, err := s.Run(context.Background(), func(r proto.Record) {
		records++
		if *folds <= 0 {
			return
		}
		switch x := r.(type) {
		case *proto.DirRecord:
			for i, c := range x.Dirs {
				nodes[x.FirstChildID+uint64(i)] = node{x.ID, c.Name}
			}
		case *proto.DirMoreRecord:
			for i, c := range x.Dirs {
				nodes[x.FirstChildID+uint64(i)] = node{x.ID, c.Name}
			}
		case *proto.FoldRecord:
			foldDirs[x.TargetID] += x.Dirs
		}
	})
	el := time.Since(start)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scan:", err)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("elapsed      %v\n", el.Round(time.Millisecond))
	fmt.Printf("dirs         %d\nfiles        %d\nrecords      %d\n", st.Dirs, st.Files, records)
	fmt.Printf("allocated    %.2f GB\napparent     %.2f GB\n", float64(st.Alloc)/1e9, float64(st.Apparent)/1e9)
	fmt.Printf("denied       %d\nerrors       %d\nvanished     %d\nskippedMnts  %d\nhardlinkDup  %d\nfolded       %d\n",
		st.Denied, st.Errors, st.Vanished, st.SkippedMounts, st.HardlinksDeduped, st.Folded)
	if *folds > 0 {
		ids := make([]uint64, 0, len(foldDirs))
		for id := range foldDirs {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return foldDirs[ids[i]] > foldDirs[ids[j]] })
		for i, id := range ids {
			if i == *folds {
				break
			}
			p := ""
			for n := id; n != 0; n = nodes[n].parent {
				p = string(filepath.Separator) + nodes[n].name + p
			}
			fmt.Printf("fold %8d dirs  %s%s\n", foldDirs[id], root, p)
		}
	}
	fmt.Printf("entries/s    %.0f\nheapSys      %d MB\n", float64(st.Dirs+st.Files)/el.Seconds(), ms.HeapSys>>20)
}
