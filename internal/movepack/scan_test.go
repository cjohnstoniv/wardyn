// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package movepack_test

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

const secret = "wdn-scan-value-7f3a9c2e1b"

func detect() movepack.Detector {
	return secretmask.NewDetector(secretmask.NewMasker([][]byte{[]byte(secret)}))
}

func scanning(l movepack.Limits) movepack.Options {
	return movepack.Options{Set: set, Limits: l, Detect: detect}
}

func zlibOf(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// looseObject is a loose object's file bytes: zlib of "<kind> <declared>\x00<content>".
func looseObject(t *testing.T, declared int, content []byte) string {
	return string(zlibOf(t, append([]byte("blob "+strconv.Itoa(declared)+"\x00"), content...)))
}

// packObj is one object of a test pack: kind 1-4 stores body whole; 6 is an OFS_DELTA whose body is its
// delta data.
type packObj struct {
	kind     int
	declared int // 0 is len(body)
	body     []byte
}

// packFile builds a version 2 pack of objs with a zero checksum.
func packFile(t *testing.T, count uint32, objs ...packObj) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("PACK")
	_ = binary.Write(&b, binary.BigEndian, uint32(2))
	_ = binary.Write(&b, binary.BigEndian, count)
	for _, o := range objs {
		size := o.declared
		if size == 0 {
			size = len(o.body)
		}
		c := byte(o.kind<<4) | byte(size&15)
		size >>= 4
		for size > 0 {
			b.WriteByte(c | 0x80)
			c = byte(size & 0x7f)
			size >>= 7
		}
		b.WriteByte(c)
		if o.kind == 6 {
			b.WriteByte(0x05) // the base is 5 bytes back; the scan never reads it
		}
		b.Write(zlibOf(t, o.body))
	}
	b.Write(make([]byte, 20))
	return b.String()
}

// delta builds delta data: the base and result sizes, then each instruction: a string is an insert of its
// bytes, and nil is a copy of 4 bytes from offset 0.
func delta(parts ...any) []byte {
	b := []byte{0x10, 0x20}
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			b = append(b, byte(len(v)))
			b = append(b, v...)
		case nil:
			b = append(b, 0x80|0x10, 4) // copy: size byte 0 present (4), offset 0
		}
	}
	return b
}

// gitTree is a stream holding a git directory with the given files under .git/objects.
func gitTree(t *testing.T, objects ...file) []byte {
	t.Helper()
	const g = "home/agent/work/.git"
	files := append([]file{dir("home/agent/work"), dir(g), reg(g+"/HEAD", "ref: refs/heads/main\n"), dir(g + "/objects")}, objects...)
	stream, _ := pack(t, files...)
	return stream
}

// TestMoveScan_InflateBombsRefused: an inflate counts what it writes. An object past its declared size or
// the object bound, a stream's objects past the inflated bound, and a pack over its object bound are each
// refused as too large, and the move is never carried unscanned.
func TestMoveScan_InflateBombsRefused(t *testing.T) {
	const o = "home/agent/work/.git/objects"
	bomb := bytes.Repeat([]byte{'z'}, 1<<20)
	id := strings.Repeat("ab", 19)
	rows := []struct {
		name   string
		stream []byte
		limits movepack.Limits
	}{
		{"a loose object past its declared size", gitTree(t, dir(o+"/ab"), reg(o+"/ab/"+id, looseObject(t, 10, bomb))), movepack.Limits{}},
		{"a loose object past the object bound", gitTree(t, dir(o+"/ab"), reg(o+"/ab/"+id, looseObject(t, len(bomb), bomb))), movepack.Limits{MaxObjectBytes: 64 << 10}},
		{"objects past the stream's inflated bound", gitTree(t, dir(o+"/ab"), reg(o+"/ab/"+id, looseObject(t, len(bomb), bomb)), reg(o+"/ab/"+strings.Repeat("cd", 19), looseObject(t, len(bomb), bomb))), movepack.Limits{MaxInflated: 3 << 19}},
		{"a packed object past its declared size", gitTree(t, dir(o+"/pack"), reg(o+"/pack/pack-1.pack", packFile(t, 1, packObj{kind: 3, declared: 16, body: bomb}))), movepack.Limits{}},
		{"a delta past its declared size", gitTree(t, dir(o+"/pack"), reg(o+"/pack/pack-1.pack", packFile(t, 1, packObj{kind: 6, declared: 8, body: delta(string(bomb[:100]))}))), movepack.Limits{}},
		{"a pack over its object bound", gitTree(t, dir(o+"/pack"), reg(o+"/pack/pack-1.pack", packFile(t, 6, packObj{kind: 3, body: []byte("a")}))), movepack.Limits{MaxPackObjects: 5}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := readAll(bytes.NewReader(row.stream), scanning(row.limits))
			if !errors.Is(err, movepack.ErrCarriedTooLarge) {
				t.Fatalf("read = %v; want ErrCarriedTooLarge", err)
			}
		})
	}
	// The same objects within their bounds scan clean.
	ok := gitTree(t, dir(o+"/ab"), reg(o+"/ab/"+id, looseObject(t, len(bomb), bomb)))
	if _, err := readAll(bytes.NewReader(ok), scanning(movepack.Limits{})); err != nil {
		t.Fatalf("a megabyte object within its bounds = %v; want nil", err)
	}
}

// TestMoveScan_FindsValuesAcrossChunks: a value split across tar blocks and relay chunks is found, from a
// stream delivered a byte at a time or in odd chunks, and the refusal never holds the value.
func TestMoveScan_FindsValuesAcrossChunks(t *testing.T) {
	const w = "home/agent/work"
	// The file's content starts in the block after its header; the value straddles the first 512-byte
	// boundary of that content.
	content := strings.Repeat("-", 512-len(secret)/2) + secret + strings.Repeat("-", 700)
	stream, _ := pack(t, dir(w), reg(w+"/.env", content))
	for name, src := range map[string]io.Reader{
		"one byte at a time": iotest.OneByteReader(bytes.NewReader(stream)),
		"half reads":         iotest.HalfReader(bytes.NewReader(stream)),
		"whole":              bytes.NewReader(stream),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readAll(src, scanning(movepack.Limits{}))
			if !errors.Is(err, movepack.ErrValueFound) {
				t.Fatalf("read = %v; want ErrValueFound", err)
			}
			if !strings.Contains(err.Error(), w+"/.env") || strings.Contains(err.Error(), secret) {
				t.Errorf("refusal %q must name the file and never the value", err)
			}
		})
	}
	// The detector alone: the value split at every point across two writes is found; a near miss is not,
	// and halves in two streams (Reset between them) are not joined.
	m := secretmask.NewMasker([][]byte{[]byte(secret)})
	for cut := 1; cut < len(secret); cut++ {
		d := secretmask.NewDetector(m)
		_, _ = d.Write([]byte("xx" + secret[:cut]))
		_, _ = d.Write([]byte(secret[cut:] + "yy"))
		if !d.Found() {
			t.Errorf("value split at %d across two writes was not found", cut)
		}
		d.Reset()
		_, _ = d.Write([]byte(secret[:cut]))
		d.Reset()
		_, _ = d.Write([]byte(secret[cut:]))
		if d.Found() {
			t.Errorf("halves split at %d in two streams were joined", cut)
		}
	}
	d := secretmask.NewDetector(m)
	_, _ = d.Write([]byte(secret[:len(secret)-1] + "!" + secret[1:]))
	if d.Found() {
		t.Error("a near miss was found")
	}
	// The same file without the value scans clean.
	clean, _ := pack(t, dir(w), reg(w+"/.env", strings.Repeat("-", 2000)))
	if _, err := readAll(iotest.OneByteReader(bytes.NewReader(clean)), scanning(movepack.Limits{})); err != nil {
		t.Fatalf("clean file = %v; want nil", err)
	}
}

// TestMoveScan_FindsValuesInGitObjects: a value in a loose object, a packed object stored whole, and a
// delta's inserts joined across consecutive inserts is found; one a copy splits is not (the stated limit).
// A git directory pointing outside the roots, and a git file that cannot be decoded, are refused.
func TestMoveScan_FindsValuesInGitObjects(t *testing.T) {
	const g = "home/agent/work/.git"
	const o = g + "/objects"
	id := strings.Repeat("ab", 19)
	half := len(secret) / 2
	// Compressible padding makes deflate code the value's bytes, so the stored bytes do not hold it.
	padded := strings.Repeat("a", 300) + secret
	rows := []struct {
		name   string
		stream []byte
		want   error
		names  string
	}{
		{"loose object", gitTree(t, dir(o+"/ab"), reg(o+"/ab/"+id, looseObject(t, len(padded), []byte(padded)))), movepack.ErrValueFound, "loose object ab" + id},
		{"packed object stored whole", gitTree(t, dir(o+"/pack"), reg(o+"/pack/pack-1.pack", packFile(t, 2, packObj{kind: 1, body: []byte("tree x")}, packObj{kind: 3, body: []byte("k: " + secret)}))), movepack.ErrValueFound, "object 1 of pack"},
		{"delta inserts joined", gitTree(t, dir(o+"/pack"), reg(o+"/pack/pack-1.pack", packFile(t, 1, packObj{kind: 6, body: delta(nil, secret[:half], secret[half:])}))), movepack.ErrValueFound, "object 0 of pack"},
		{"delta inserts a copy splits (stated limit)", gitTree(t, dir(o+"/pack"), reg(o+"/pack/pack-1.pack", packFile(t, 1, packObj{kind: 6, body: delta(secret[:half], nil, secret[half:])}))), nil, ""},
		{"alternates outside the roots", gitTree(t, dir(o+"/info"), reg(o+"/info/alternates", "/srv/shared/objects\n")), movepack.ErrGitOutsideRoots, "/srv/shared/objects"},
		{"alternates inside the roots", gitTree(t, dir(o+"/info"), reg(o+"/info/alternates", "../../other/.git/objects\n")), nil, ""},
		{"a .git pointer outside the roots", mustPack(t, dir("home/agent/work"), dir("home/agent/work/sub"), reg("home/agent/work/sub/.git", "gitdir: /home/agent/elsewhere/.git\n")), movepack.ErrGitOutsideRoots, "home/agent/work/sub/.git"},
		{"a .git pointer inside the roots", mustPack(t, dir("home/agent/work"), dir("home/agent/work/sub"), reg("home/agent/work/sub/.git", "gitdir: ../.git/modules/sub\n")), nil, ""},
		{"a loose object that is not zlib", gitTree(t, dir(o+"/ab"), reg(o+"/ab/"+id, "not zlib")), movepack.ErrScanUnreadable, "loose object ab" + id},
		{"a pack that is not a pack", gitTree(t, dir(o+"/pack"), reg(o+"/pack/pack-1.pack", "JUNKJUNKJUNKJUNK")), movepack.ErrScanUnreadable, "pack-1.pack"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := readAll(bytes.NewReader(row.stream), scanning(movepack.Limits{}))
			if row.want == nil {
				if err != nil {
					t.Fatalf("read = %v; want nil", err)
				}
				return
			}
			if !errors.Is(err, row.want) {
				t.Fatalf("read = %v; want %v", err, row.want)
			}
			if !strings.Contains(err.Error(), row.names) || strings.Contains(err.Error(), secret) {
				t.Errorf("refusal %q must name %q and never the value", err, row.names)
			}
		})
	}
	// Without a scan, the same compressed value passes: the scan, not the file bytes, found it.
	stream := gitTree(t, dir(o+"/ab"), reg(o+"/ab/"+id, looseObject(t, len(padded), []byte(padded))))
	if bytes.Contains(stream, []byte(secret)) {
		t.Fatal("the test value is visible in the stored bytes; the row proves nothing")
	}
	if _, err := readAll(bytes.NewReader(stream), movepack.Options{Set: set}); err != nil {
		t.Fatalf("read without a scan = %v; want nil", err)
	}
}

func mustPack(t *testing.T, files ...file) []byte {
	t.Helper()
	stream, _ := pack(t, files...)
	return stream
}

// TestMoveScan_PackEndsAtItsChecksum: a pack's last object is followed by its checksum, 20 or 32 bytes, and
// nothing else; bytes past it, or a pack cut before it, cannot be scanned and are refused.
func TestMoveScan_PackEndsAtItsChecksum(t *testing.T) {
	const o = "home/agent/work/.git/objects"
	whole := packFile(t, 1, packObj{kind: 3, body: []byte("a")})
	for _, row := range []struct {
		name string
		pack string
		ok   bool
	}{
		{"a SHA-1 checksum", whole, true},
		{"a SHA-256 checksum", whole + strings.Repeat("\x00", 12), true},
		{"bytes past the checksum", whole + strings.Repeat("x", 50), false},
		{"one byte past the checksum", whole + "x", false},
		{"no checksum", whole[:len(whole)-20], false},
	} {
		t.Run(row.name, func(t *testing.T) {
			stream := gitTree(t, dir(o+"/pack"), reg(o+"/pack/pack-1.pack", row.pack))
			_, err := readAll(bytes.NewReader(stream), scanning(movepack.Limits{}))
			if row.ok != (err == nil) || (!row.ok && !errors.Is(err, movepack.ErrScanUnreadable)) {
				t.Fatalf("read = %v; want ok %v, else ErrScanUnreadable", err, row.ok)
			}
		})
	}
}
