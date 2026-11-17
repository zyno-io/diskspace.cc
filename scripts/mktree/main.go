// SPDX-License-Identifier: MIT

// Command mktree creates a synthetic directory tree for scanner benchmarks:
//
//	go run ./scripts/mktree -dirs N -files M -seed S [-size B] [-sparse] <out>
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

func main() {
	dirs := flag.Int("dirs", 1000, "number of directories (excluding the root)")
	files := flag.Int("files", 10000, "number of files")
	seed := flag.Int64("seed", 1, "random seed")
	maxSize := flag.Int("size", 0, "max file size in bytes (0 = zero-length files; otherwise random 0..size)")
	sparse := flag.Bool("sparse", false, "make every 1000th file a 64 MiB sparse file")
	workers := flag.Int("j", 8, "parallel file creators")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: mktree [flags] <out>")
		os.Exit(2)
	}
	out := flag.Arg(0)
	rng := rand.New(rand.NewSource(*seed))

	paths := make([]string, 1, *dirs+1)
	paths[0] = out
	if err := os.MkdirAll(out, 0o755); err != nil {
		fatal(err)
	}
	for i := 0; i < *dirs; i++ {
		// Mostly random-recursive (shallow-ish, wide); sometimes extend the newest
		// dir to produce deep chains.
		var parent string
		if rng.Intn(20) == 0 {
			parent = paths[len(paths)-1]
		} else {
			parent = paths[rng.Intn(len(paths))]
		}
		p := filepath.Join(parent, "d"+strconv.Itoa(i))
		if err := os.Mkdir(p, 0o755); err != nil {
			fatal(err)
		}
		paths = append(paths, p)
	}

	type item struct {
		path string
		size int64
	}
	ch := make(chan item, 4096)
	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				f, err := os.OpenFile(it.path, os.O_CREATE|os.O_WRONLY, 0o644)
				if err != nil {
					fatal(err)
				}
				if it.size > 0 {
					if it.size > 1<<20 {
						_ = f.Truncate(it.size)
					} else {
						_, _ = f.Write(make([]byte, it.size))
					}
				}
				f.Close()
			}
		}()
	}
	for i := 0; i < *files; i++ {
		var size int64
		if *maxSize > 0 {
			size = int64(rng.Intn(*maxSize + 1))
		}
		if *sparse && i%1000 == 999 {
			size = 64 << 20
		}
		ch <- item{filepath.Join(paths[rng.Intn(len(paths))], "f"+strconv.Itoa(i)), size}
	}
	close(ch)
	wg.Wait()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "mktree:", err)
	os.Exit(1)
}
