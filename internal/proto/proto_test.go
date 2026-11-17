// SPDX-License-Identifier: MIT

package proto

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/klauspost/compress/zstd"
)

var update = flag.Bool("update", false, "rewrite testdata/vectors")

const vectorDir = "../../testdata/vectors"

func hx(b []byte) string { return hex.EncodeToString(b) }

type detRand struct {
	seed string
	ctr  uint64
	buf  []byte
}

func (d *detRand) Read(p []byte) (int, error) {
	for len(d.buf) < len(p) {
		var c [8]byte
		binary.BigEndian.PutUint64(c[:], d.ctr)
		d.ctr++
		h := sha256.Sum256(append([]byte(d.seed), c[:]...))
		d.buf = append(d.buf, h[:]...)
	}
	n := copy(p, d.buf)
	d.buf = d.buf[n:]
	return n, nil
}

func writeOrCompare(t *testing.T, name string, v any) {
	t.Helper()
	want, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.Join(vectorDir, name)
	if *update || os.Getenv("UPDATE_VECTORS") != "" {
		if err := os.MkdirAll(vectorDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from generated vectors (run with -update if intended)", name)
	}
}

func TestVarint(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 16383, 16384, 1<<32 - 1, 1 << 32, 1<<53 - 1, 1<<64 - 1} {
		b := AppendUvarint(nil, v)
		if len(b) != UvarintLen(v) {
			t.Fatal("len", v)
		}
		got, n, err := Uvarint(b)
		if err != nil || got != v || n != len(b) {
			t.Fatal(v, got, err)
		}
	}
	if _, _, err := Uvarint([]byte{0x80}); err == nil {
		t.Fatal("truncated accepted")
	}
	if _, _, err := Uvarint(bytes.Repeat([]byte{0xff}, 11)); err == nil {
		t.Fatal("overflow accepted")
	}
}

func sampleRecords() []Record {
	return []Record{
		&DirRecord{ID: 0, Flags: DirHasEntryErrors, OwnAlloc: 4096, OwnApparent: 100, OwnFiles: 3, EntryErrors: 1, TotalDirs: 6, FirstChildID: 1,
			Dirs:  []ChildDir{{"usr", 0}, {"mnt", ChildOtherDevice}, {"héllo 世界", ChildReparse | ChildExcluded}},
			Files: []FileEntry{{"big.iso", 1 << 40, 1<<40 + 5, FileSparse}, {"a", 1, 1, 0}, {"bad\xff\xfename", 8192, 7, FileBadName}}},
		&DirRecord{ID: 1, OwnAlloc: 1<<53 - 1, OwnApparent: 1<<53 - 1, OwnFiles: 1 << 33},
		&DirRecord{ID: 2, Flags: DirReadDenied | DirReadError | DirVanished | DirFoldedChildren, TotalDirs: 2, FirstChildID: 1 << 33,
			Dirs: []ChildDir{{"x", 0}}},
		&DirMoreRecord{ID: 0, FirstChildID: 4, Dirs: []ChildDir{{"more", 0}, {"mnt2", ChildBindMount}, {"héllo", ChildDupDir}}},
		&DirMoreRecord{ID: 2, FirstChildID: 1<<33 + 1, Dirs: []ChildDir{{"y", 0}}},
		&FoldRecord{TargetID: 1, Alloc: 1 << 35, Apparent: 1 << 36, Files: 12345, Dirs: 99},
		&FoldEndRecord{TargetID: 1},
	}
}

func TestNodesRoundTrip(t *testing.T) {
	e := NewNodesEncoder(7)
	if e.Count() != 0 || e.Len() != 3 {
		t.Fatal("empty", e.Len())
	}
	recs := sampleRecords()
	for _, r := range recs {
		e.Add(r)
	}
	b := e.Bytes()
	if len(b) != e.Len() || e.Count() != len(recs) || b[0] != MsgNodes {
		t.Fatal("len/count")
	}
	id, got, err := DecodeNodes(b)
	if err != nil || id != 7 || !reflect.DeepEqual(got, recs) {
		t.Fatalf("roundtrip: %v\n%#v", err, got)
	}
	e.Reset(1<<40 + 3)
	e.Add(&FoldEndRecord{TargetID: 5})
	id, got, err = DecodeNodes(e.Bytes())
	if err != nil || id != 1<<40+3 || len(got) != 1 {
		t.Fatal(err)
	}
}

func TestNodesStrict(t *testing.T) {
	e := NewNodesEncoder(1)
	for _, r := range sampleRecords() {
		e.Add(r)
	}
	b := bytes.Clone(e.Bytes())
	// every truncation must fail, never panic
	for i := 0; i < len(b); i++ {
		if _, _, err := DecodeNodes(b[:i]); err == nil {
			t.Fatalf("truncation at %d accepted", i)
		}
	}
	if _, _, err := DecodeNodes(append(bytes.Clone(b), 0)); err == nil {
		t.Fatal("trailing byte accepted")
	}
	// oversize name
	long := &DirRecord{ID: 0, TotalDirs: 1, Dirs: []ChildDir{{string(make([]byte, 4097)), 0}}}
	raw := []byte{MsgNodes, 1, 1, RecDir, 0, 0, 0, 0, 0, 0, 1, 0, 1}
	raw = AppendUvarint(raw, 4097)
	raw = append(raw, make([]byte, 4097)...)
	raw = append(raw, 0, 0)
	if _, _, err := DecodeNodes(raw); err == nil {
		t.Fatal("4097-byte name accepted")
	}
	// nInline > nTotal rejected; absurd nTotal rejected
	if _, _, err := DecodeNodes([]byte{MsgNodes, 1, 1, RecDir, 0, 0, 0, 0, 0, 0, 0, 1, 1, 'a', 0, 0}); err == nil {
		t.Fatal("nInline > nTotal accepted")
	}
	raw = []byte{MsgNodes, 1, 1, RecDir, 0, 0, 0, 0, 0, 0}
	raw = AppendUvarint(raw, MaxChildDirs+1)
	raw = append(raw, 1, 0, 0)
	if _, _, err := DecodeNodes(raw); err == nil {
		t.Fatal("nTotal above cap accepted")
	}
	// encoder truncates instead
	e.Reset(1)
	e.Add(long)
	_, got, err := DecodeNodes(e.Bytes())
	if err != nil || len(got[0].(*DirRecord).Dirs[0].Name) != MaxNameLen {
		t.Fatal(err)
	}
	// DIRMORE: empty chunk and oversize name rejected
	if _, _, err := DecodeNodes([]byte{MsgNodes, 1, 1, RecDirMore, 0, 1, 0}); err == nil {
		t.Fatal("empty DIRMORE accepted")
	}
	raw = []byte{MsgNodes, 1, 1, RecDirMore, 0, 1, 1}
	raw = AppendUvarint(raw, 4097)
	raw = append(raw, make([]byte, 4097)...)
	raw = append(raw, 0)
	if _, _, err := DecodeNodes(raw); err == nil {
		t.Fatal("4097-byte DIRMORE name accepted")
	}
	// count larger than remaining bytes
	if _, _, err := DecodeNodes([]byte{MsgNodes, 1, 1, RecDirMore, 0, 1, 50, 1, 'a', 0}); err == nil {
		t.Fatal("bogus DIRMORE count accepted")
	}
	// huge count with no data
	if _, _, err := DecodeNodes([]byte{MsgNodes, 1, 0xff, 0xff, 0xff, 0xff, 0x0f}); err == nil {
		t.Fatal("bogus count accepted")
	}
}

func FuzzDecodeNodes(f *testing.F) {
	e := NewNodesEncoder(1)
	for _, r := range sampleRecords() {
		e.Add(r)
	}
	f.Add(bytes.Clone(e.Bytes()))
	f.Add([]byte{MsgNodes, 0, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		id, recs, err := DecodeNodes(b)
		if err != nil {
			return
		}
		// Anything accepted must re-encode to the identical bytes (canonical
		// varints aside, which the fuzzer can produce — so compare decoded forms).
		e := NewNodesEncoder(id)
		for _, r := range recs {
			e.Add(r)
		}
		id2, recs2, err := DecodeNodes(e.Bytes())
		if err != nil || id2 != id || !reflect.DeepEqual(recs, recs2) {
			t.Fatalf("re-encode mismatch: %v", err)
		}
	})
}

func TestDirFiles(t *testing.T) {
	res := &DirFilesResult{Flags: DirReadDenied, RestCount: 1 << 34, RestAlloc: 5, RestApparent: 6,
		Files: []FileEntry{{"b", 9, 8, 0}, {"bad\xc3(", 1, 1, FileBadName}}}
	b := EncodeDirFiles(9, 2, 1<<33, res)
	rq, sc, di, got, err := DecodeDirFiles(b)
	if err != nil || rq != 9 || sc != 2 || di != 1<<33 || !reflect.DeepEqual(got, res) {
		t.Fatal(err, got)
	}
	for i := 0; i < len(b); i++ {
		if _, _, _, _, err := DecodeDirFiles(b[:i]); err == nil {
			t.Fatal("truncation accepted", i)
		}
	}
}

func TestMsgJSON(t *testing.T) {
	b, _ := EncodeMsg(MsgAgentInfo, AgentInfo{Proto: 1})
	s := string(b[1:])
	for _, want := range []string{`"fda":null`, `"root":null`, `"cwd":null`, `"autoScan":null`, `"volumes":[]`, `"scan":null`} {
		if !bytes.Contains([]byte(s), []byte(want)) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
	b, _ = EncodeMsg(MsgScanStarted, ScanStarted{ScanID: 1})
	if !bytes.Contains(b, []byte(`"volume":null`)) {
		t.Fatal(string(b))
	}
	b, _ = EncodeMsg(MsgBrowseResult, BrowseResult{})
	if !bytes.Contains(b, []byte(`"parent":null`)) || !bytes.Contains(b, []byte(`"dirs":[]`)) {
		t.Fatal(string(b))
	}
	var ch ClientHello
	typ, err := DecodeMsg([]byte(append([]byte{MsgClientHello}, `{"proto":1,"ua":"x","caps":{"zstdStream":true}}`...)), &ch)
	if err != nil || typ != MsgClientHello || !ch.Caps.ZstdStream {
		t.Fatal(err)
	}
}

// ---- zstream ----

// synthBody generates a deterministic compressible body (first byte = type).
func synthBody(i, n int) []byte {
	b := make([]byte, n)
	b[0] = MsgNodes
	st := uint32(i*7919 + 1)
	var tab [97]byte
	for j := range tab {
		st = st*1664525 + 1013904223
		tab[j] = byte(st >> 24)
	}
	for j := 1; j < n; j++ {
		b[j] = tab[j%97] + byte(j>>12)
	}
	return b
}

func synthLen(i int) int {
	switch {
	case i == 0:
		return 1
	case i == 1:
		return 511
	case i == 2:
		return 512
	case i == 250:
		return MaxBody
	case i%97 == 5:
		return 65536
	}
	st := uint32(i)*2654435761 + 12345
	st = st*1664525 + 1013904223
	return 1 + int(st>>8)%3000
}

type zFrame struct {
	Len   int    `json:"len"`
	Codec int    `json:"codec"`
	Frame string `json:"frame"`
}

type zVector struct {
	Note   string   `json:"note"`
	Init   string   `json:"init"`
	Stream []zFrame `json:"stream"`
	Indep  []zFrame `json:"independent"`
}

func TestStreamCompressor(t *testing.T) {
	var vec zVector
	vec.Note = "body i = synthBody(i, len); stream: codec-1 frames form one zstd stream; init frame is first codec-1 frame"
	for _, stream := range []bool{true, false} {
		c := NewStreamCompressor(stream)
		c.Rand = &detRand{seed: "zinit"}
		if _, err := c.Frame(synthBody(1, 600)); err != ErrNotInitialized {
			t.Fatal("expected ErrNotInitialized", err)
		}
		init, err := c.InitFrame()
		if err != nil || init[0] != CodecZstd {
			t.Fatal(err)
		}
		if stream {
			vec.Init = hx(init)
		}
		var frames []zFrame
		var codec1 [][]byte
		codec1 = append(codec1, init[1:])
		count := 500
		if !stream {
			count = 20
		}
		for i := 0; i < count; i++ {
			body := synthBody(i, synthLen(i))
			f, err := c.Frame(body)
			if err != nil {
				t.Fatal(err)
			}
			wantCodec := CodecRaw
			if len(body) >= CompressThreshold {
				wantCodec = CodecZstd
			}
			if int(f[0]) != wantCodec {
				t.Fatalf("i=%d codec %d", i, f[0])
			}
			if f[0] == CodecRaw && !bytes.Equal(f[1:], body) {
				t.Fatal("raw body mismatch")
			}
			if f[0] == CodecZstd {
				codec1 = append(codec1, f[1:])
				if !stream {
					d, _ := zstd.NewReader(nil)
					out, err := d.DecodeAll(f[1:], nil)
					d.Close()
					if err != nil || !bytes.Equal(out, body) {
						t.Fatalf("independent frame %d: %v", i, err)
					}
				}
			}
			frames = append(frames, zFrame{len(body), int(f[0]), hx(f)})
		}
		if stream {
			// The concatenation of all codec-1 payloads is a single valid zstd
			// stream whose output is init body ‖ compressed bodies in order.
			var all bytes.Buffer
			for _, p := range codec1 {
				all.Write(p)
			}
			d, _ := zstd.NewReader(&all)
			out := new(bytes.Buffer)
			// Stream is not terminated (no final block); read until EOF/unexpected EOF.
			buf := make([]byte, 1<<16)
			for {
				n, err := d.Read(buf)
				out.Write(buf[:n])
				if err != nil {
					break
				}
			}
			d.Close()
			// skip init body (33 bytes)
			o := out.Bytes()
			if len(o) < 33 || o[0] != MsgStreamInit {
				t.Fatalf("stream output starts %v len %d", o[:min(len(o), 4)], len(o))
			}
			o = o[33:]
			for i := 0; i < count; i++ {
				body := synthBody(i, synthLen(i))
				if len(body) < CompressThreshold {
					continue
				}
				if len(o) < len(body) || !bytes.Equal(o[:len(body)], body) {
					t.Fatalf("stream body %d mismatch", i)
				}
				o = o[len(body):]
			}
			if len(o) != 0 {
				t.Fatal("extra output")
			}
			vec.Stream = frames
		} else {
			vec.Indep = frames
		}
		c.Close()
	}
	// zstd output is not guaranteed byte-stable across library versions, so
	// this file is only rewritten on -update; TestZstreamVectorDecodes verifies it.
	if *update || os.Getenv("UPDATE_VECTORS") != "" {
		j, _ := json.MarshalIndent(vec, "", " ")
		if err := os.WriteFile(filepath.Join(vectorDir, "zstream.json"), append(j, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestZstreamVectorDecodes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(vectorDir, "zstream.json"))
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	var vec zVector
	if err := json.Unmarshal(raw, &vec); err != nil {
		t.Fatal(err)
	}
	init, _ := hex.DecodeString(vec.Init)
	var all bytes.Buffer
	all.Write(init[1:])
	var want []byte
	want = append(want, 0)
	for i, f := range vec.Stream {
		fr, _ := hex.DecodeString(f.Frame)
		body := synthBody(i, f.Len)
		if f.Codec == CodecRaw {
			if !bytes.Equal(fr[1:], body) {
				t.Fatalf("raw %d", i)
			}
			continue
		}
		all.Write(fr[1:])
		want = append(want, body...)
	}
	d, _ := zstd.NewReader(&all)
	defer d.Close()
	out := new(bytes.Buffer)
	buf := make([]byte, 1<<16)
	for {
		n, err := d.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			break
		}
	}
	o := out.Bytes()
	// init body is 33 bytes: type byte + 32 random
	if len(o) < 33 || !bytes.Equal(append([]byte{0}, o[33:]...), want) {
		t.Fatal("stream vector does not decode to expected bodies")
	}
	for i, f := range vec.Indep {
		fr, _ := hex.DecodeString(f.Frame)
		if f.Codec == CodecRaw {
			continue
		}
		d2, _ := zstd.NewReader(nil)
		out, err := d2.DecodeAll(fr[1:], nil)
		d2.Close()
		if err != nil || !bytes.Equal(out, synthBody(i, f.Len)) {
			t.Fatalf("indep %d: %v", i, err)
		}
	}
}

type nodesCase struct {
	Name    string       `json:"name"`
	Body    string       `json:"body"`
	ScanID  uint64       `json:"scanId"`
	Records []jsonRecord `json:"records"`
}

type jsonName struct {
	Hex   string `json:"hex"`
	Flags uint64 `json:"flags"`
}

type jsonFile struct {
	Hex      string `json:"hex"`
	Alloc    uint64 `json:"alloc"`
	Apparent uint64 `json:"apparent"`
	Flags    uint64 `json:"flags"`
}

type jsonRecord struct {
	K            int        `json:"k"`
	ID           uint64     `json:"id,omitempty"`
	Flags        uint64     `json:"flags,omitempty"`
	OwnAlloc     uint64     `json:"ownAlloc,omitempty"`
	OwnApparent  uint64     `json:"ownApparent,omitempty"`
	OwnFiles     uint64     `json:"ownFiles,omitempty"`
	EntryErrors  uint64     `json:"entryErrors,omitempty"`
	TotalDirs    uint64     `json:"totalDirs,omitempty"`
	FirstChildID uint64     `json:"firstChildId,omitempty"`
	Dirs         []jsonName `json:"dirs,omitempty"`
	Files        []jsonFile `json:"files,omitempty"`
	TargetID     uint64     `json:"targetId,omitempty"`
	Alloc        uint64     `json:"alloc,omitempty"`
	Apparent     uint64     `json:"apparent,omitempty"`
	FilesN       uint64     `json:"filesN,omitempty"`
	DirsN        uint64     `json:"dirsN,omitempty"`
}

func toJSON(recs []Record) []jsonRecord {
	var out []jsonRecord
	for _, r := range recs {
		switch r := r.(type) {
		case *DirRecord:
			j := jsonRecord{K: 1, ID: r.ID, Flags: r.Flags, OwnAlloc: r.OwnAlloc, OwnApparent: r.OwnApparent, OwnFiles: r.OwnFiles, EntryErrors: r.EntryErrors, TotalDirs: r.TotalDirs, FirstChildID: r.FirstChildID}
			for _, d := range r.Dirs {
				j.Dirs = append(j.Dirs, jsonName{hx([]byte(d.Name)), d.Flags})
			}
			for _, f := range r.Files {
				j.Files = append(j.Files, jsonFile{hx([]byte(f.Name)), f.Alloc, f.Apparent, f.Flags})
			}
			out = append(out, j)
		case *DirMoreRecord:
			j := jsonRecord{K: 4, ID: r.ID, FirstChildID: r.FirstChildID}
			for _, d := range r.Dirs {
				j.Dirs = append(j.Dirs, jsonName{hx([]byte(d.Name)), d.Flags})
			}
			out = append(out, j)
		case *FoldRecord:
			out = append(out, jsonRecord{K: 2, TargetID: r.TargetID, Alloc: r.Alloc, Apparent: r.Apparent, FilesN: r.Files, DirsN: r.Dirs})
		case *FoldEndRecord:
			out = append(out, jsonRecord{K: 3, TargetID: r.TargetID})
		}
	}
	return out
}

type dirFilesCase struct {
	Body         string     `json:"body"`
	ReqID        uint64     `json:"reqId"`
	ScanID       uint64     `json:"scanId"`
	DirID        uint64     `json:"dirId"`
	Flags        uint64     `json:"flags"`
	RestCount    uint64     `json:"restCount"`
	RestAlloc    uint64     `json:"restAlloc"`
	RestApparent uint64     `json:"restApparent"`
	Files        []jsonFile `json:"files"`
}

type nodesVector struct {
	Nodes    []nodesCase  `json:"nodes"`
	DirFiles dirFilesCase `json:"dirFiles"`
}

func TestVectors(t *testing.T) {
	var v nodesVector
	add := func(name string, scan uint64, recs []Record) {
		e := NewNodesEncoder(scan)
		for _, r := range recs {
			e.Add(r)
		}
		v.Nodes = append(v.Nodes, nodesCase{name, hx(e.Bytes()), scan, toJSON(recs)})
	}
	add("empty", 1, nil)
	add("mixed", 3, sampleRecords())
	add("bigScanId", 1<<40+17, []Record{&FoldEndRecord{TargetID: 1<<53 - 1}})
	res := &DirFilesResult{Flags: DirReadDenied, RestCount: 1 << 34, RestAlloc: 5, RestApparent: 6,
		Files: []FileEntry{{"b", 9, 8, 0}, {"bad\xc3(", 1, 1, FileBadName}, {"日本語", 1 << 40, 1 << 41, FileCloud}}}
	v.DirFiles = dirFilesCase{Body: hx(EncodeDirFiles(9, 2, 1<<33, res)), ReqID: 9, ScanID: 2, DirID: 1 << 33, Flags: res.Flags,
		RestCount: res.RestCount, RestAlloc: res.RestAlloc, RestApparent: res.RestApparent}
	for _, f := range res.Files {
		v.DirFiles.Files = append(v.DirFiles.Files, jsonFile{hx([]byte(f.Name)), f.Alloc, f.Apparent, f.Flags})
	}
	writeOrCompare(t, "nodes.json", v)
}
