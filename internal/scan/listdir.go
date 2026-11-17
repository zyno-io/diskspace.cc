// SPDX-License-Identifier: MIT

package scan

import (
	"slices"
	"strings"

	"github.com/zyno-io/diskspace.cc/internal/proto"
)

// ListDirFiles re-reads one directory and returns its non-directory entries
// sorted by Alloc descending (at most limit; limit<=0 means all). Entries
// beyond the limit are summed into the Rest* bucket. Hardlinks are deduped
// within the directory only.
func ListDirFiles(path string, limit int) (*proto.DirFilesResult, error) {
	var sc readScratch
	var files []proto.FileEntry
	var seen map[fkey]struct{}
	res := &proto.DirFilesResult{}
	_, err := readDirEntries(path, &sc, func(e *rawEntry) bool {
		if e.Err != nil || e.Kind == kindDir {
			return true
		}
		if e.Dedup {
			k := fkey{e.Dev, e.Ino}
			if _, dup := seen[k]; dup {
				return true
			}
			if seen == nil {
				seen = map[fkey]struct{}{}
			}
			seen[k] = struct{}{}
		}
		fl := e.Flags
		if badName(e.Name) {
			fl |= proto.FileBadName
		}
		files = append(files, proto.FileEntry{Name: e.Name, Alloc: e.Alloc, Apparent: e.Apparent, Flags: fl})
		return true
	})
	if err != nil {
		switch {
		case isDenied(err):
			res.Flags |= proto.DirReadDenied
		case isGone(err):
			res.Flags |= proto.DirVanished
		default:
			if len(files) == 0 {
				return nil, err
			}
			res.Flags |= proto.DirReadError
		}
	}
	slices.SortFunc(files, func(a, b proto.FileEntry) int {
		switch {
		case a.Alloc > b.Alloc:
			return -1
		case a.Alloc < b.Alloc:
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	if limit > 0 && len(files) > limit {
		for _, f := range files[limit:] {
			res.RestCount++
			res.RestAlloc += f.Alloc
			res.RestApparent += f.Apparent
		}
		files = files[:limit]
	}
	res.Files = files
	return res, nil
}

// ListSubdirs returns the sorted names of real subdirectories of path
// (symlinks are not included).
func ListSubdirs(path string) ([]string, error) {
	var sc readScratch
	var names []string
	_, err := readDirEntries(path, &sc, func(e *rawEntry) bool {
		if e.Err == nil && e.Kind == kindDir && e.ChildFlags&proto.ChildReparse == 0 {
			names = append(names, e.Name)
		}
		return true
	})
	if err != nil && len(names) == 0 {
		return nil, err
	}
	slices.Sort(names)
	return names, nil
}
