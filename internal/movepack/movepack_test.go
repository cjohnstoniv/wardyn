// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package movepack_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
)

var set = movepack.Set{Roots: []string{"/home/agent/work"}, Exclude: []string{"**/.credentials.json"}}

// file is one entry a test stream carries.
type file struct {
	h       movepack.Header
	content string
}

func dir(p string) file {
	return file{h: movepack.Header{Path: p, Type: movepack.TypeDir, Mode: 0o755}}
}
func reg(p, content string) file {
	return file{h: movepack.Header{Path: p, Type: movepack.TypeFile, Mode: 0o644, Size: int64(len(content))}, content: content}
}
func link(p, to string) file {
	return file{h: movepack.Header{Path: p, Type: movepack.TypeSymlink, Mode: 0o777, Link: to}}
}

// pack writes files as a stream through movepack.Writer.
func pack(t *testing.T, files ...file) ([]byte, movepack.Manifest) {
	t.Helper()
	var buf bytes.Buffer
	w, err := movepack.NewWriter(&buf, set)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := w.Add(f.h, strings.NewReader(f.content)); err != nil {
			t.Fatalf("add %s: %v", f.h.Path, err)
		}
	}
	m, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), m
}

// readAll reads a stream to its end and returns the reader's manifest.
func readAll(src io.Reader, o movepack.Options) (movepack.Manifest, error) {
	if o.Set.Roots == nil {
		o.Set = set
	}
	r, err := movepack.NewReader(src, o)
	if err != nil {
		return movepack.Manifest{}, err
	}
	defer r.Close()
	for {
		_, content, err := r.Next()
		if err == io.EOF {
			return r.Manifest(), nil
		}
		if err != nil {
			return movepack.Manifest{}, err
		}
		if content != nil {
			if _, err := io.Copy(io.Discard, content); err != nil {
				return movepack.Manifest{}, err
			}
		}
	}
}

// rawEntry is a tar header written as is, for streams the Writer would refuse.
type rawEntry struct {
	h       tar.Header
	content string
}

// raw writes entries with archive/tar, then the manifest line (nil for none).
func raw(t *testing.T, line []byte, entries ...rawEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := e.h
		if h.Size == 0 && h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.content))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatalf("write header %q: %v", h.Name, err)
		}
		if _, err := io.WriteString(tw, e.content); err != nil {
			t.Fatalf("write %q: %v", h.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	buf.Write(line)
	return buf.Bytes()
}

// manifestLine is the manifest line a stream ends with.
func manifestLine(stream []byte) []byte {
	return stream[bytes.LastIndex(stream, []byte("wardyn-movepack-manifest-v1 ")):]
}

func regH(name string) tar.Header {
	return tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Format: tar.FormatPAX}
}

func dirH(name string) tar.Header {
	return tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755, Format: tar.FormatPAX}
}

// TestMovePack_RefusesHostileEntries: one row per rule of the hostile-archive rule. Each refusal is
// ErrArchiveRefused and names the entry.
func TestMovePack_RefusesHostileEntries(t *testing.T) {
	const w = "home/agent/work"
	long := strings.Repeat("a", 250) + "/"
	setuid := regH(w + "/s")
	setuid.Mode = 0o4755
	setgid := regH(w + "/s")
	setgid.Mode = 0o2755
	sticky := dirH(w + "/s")
	sticky.Mode = 0o1777
	paxBig := regH(w + "/p")
	paxBig.PAXRecords = map[string]string{"comment": strings.Repeat("x", 70<<10)}
	global := tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "g", PAXRecords: map[string]string{"comment": "x"}, Format: tar.FormatPAX}
	rows := []struct {
		name    string
		entries []rawEntry
		names   string
	}{
		{"out of order", []rawEntry{{h: dirH(w)}, {h: regH(w + "/b"), content: "b"}, {h: regH(w + "/a"), content: "a"}}, w + "/a"},
		{"repeated path", []rawEntry{{h: dirH(w)}, {h: regH(w + "/a"), content: "a"}, {h: regH(w + "/a"), content: "a"}}, w + "/a"},
		{"component order, not raw byte order", []rawEntry{{h: dirH(w)}, {h: regH(w + "/a-b"), content: "x"}, {h: dirH(w + "/a")}}, w + "/a"},
		{"absolute path", []rawEntry{{h: regH("/" + w + "/a"), content: "a"}}, "/" + w + "/a"},
		{"dot-dot component", []rawEntry{{h: dirH(w)}, {h: regH(w + "/../../../etc/passwd"), content: "x"}}, "etc/passwd"},
		{"path over 4096 bytes", []rawEntry{{h: dirH(w)}, {h: regH(w + "/" + strings.Repeat(long, 17) + "f"), content: "x"}}, "aaaa"},
		{"component over 255", []rawEntry{{h: dirH(w)}, {h: regH(w + "/" + strings.Repeat("c", 256)), content: "x"}}, "ccc"},
		{"outside the roots", []rawEntry{{h: regH("home/agent/.bashrc"), content: "x"}}, "home/agent/.bashrc"},
		{"hardlink", []rawEntry{{h: dirH(w)}, {h: regH(w + "/a"), content: "a"}, {h: tar.Header{Name: w + "/b", Typeflag: tar.TypeLink, Linkname: w + "/a", Format: tar.FormatPAX}}}, w + "/b"},
		{"character device", []rawEntry{{h: dirH(w)}, {h: tar.Header{Name: w + "/c", Typeflag: tar.TypeChar, Format: tar.FormatPAX}}}, w + "/c"},
		{"block device", []rawEntry{{h: dirH(w)}, {h: tar.Header{Name: w + "/d", Typeflag: tar.TypeBlock, Format: tar.FormatPAX}}}, w + "/d"},
		{"fifo", []rawEntry{{h: dirH(w)}, {h: tar.Header{Name: w + "/f", Typeflag: tar.TypeFifo, Format: tar.FormatPAX}}}, w + "/f"},
		{"PAX global header", []rawEntry{{h: global}}, "g"},
		{"setuid", []rawEntry{{h: dirH(w)}, {h: setuid, content: "x"}}, w + "/s"},
		{"setgid", []rawEntry{{h: dirH(w)}, {h: setgid, content: "x"}}, w + "/s"},
		{"sticky", []rawEntry{{h: dirH(w)}, {h: sticky}}, w + "/s"},
		{"PAX header over 64 KiB", []rawEntry{{h: dirH(w)}, {h: paxBig, content: "x"}}, w + "/p"},
		{"link target over 4096", []rawEntry{{h: dirH(w)}, {h: tar.Header{Name: w + "/l", Typeflag: tar.TypeSymlink, Linkname: strings.Repeat("t", 4097), Format: tar.FormatPAX}}}, w + "/l"},
		// The directory rule would refuse it too, so the row names the symlink rule's own reason.
		{"through an earlier symlink", []rawEntry{{h: dirH(w)}, {h: tar.Header{Name: w + "/l", Typeflag: tar.TypeSymlink, Linkname: "/etc", Format: tar.FormatPAX}}, {h: regH(w + "/l/shadow"), content: "x"}}, `passes through the symlink entry "` + w + `/l"`},
		{"a root that is a symlink", []rawEntry{{h: tar.Header{Name: w, Typeflag: tar.TypeSymlink, Linkname: "/etc", Format: tar.FormatPAX}}}, w},
		{"an undeclared parent directory", []rawEntry{{h: dirH(w)}, {h: regH(w + "/sub/deeper/x"), content: "x"}}, w + "/sub/deeper"},
		{"a parent that is a file", []rawEntry{{h: dirH(w)}, {h: regH(w + "/a"), content: "a"}, {h: regH(w + "/a/x"), content: "x"}}, `its directory "` + w + `/a" is not an earlier entry`},
		{"a parent declared only as a sibling's", []rawEntry{{h: dirH(w)}, {h: dirH(w + "/a")}, {h: regH(w + "/a/x"), content: "x"}, {h: regH(w + "/b/y"), content: "y"}}, w + "/b"},
		{"an excluded path", []rawEntry{{h: dirH(w)}, {h: regH(w + "/x/.credentials.json"), content: "x"}}, ".credentials.json"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			_, err := readAll(bytes.NewReader(raw(t, nil, row.entries...)), movepack.Options{})
			if !errors.Is(err, movepack.ErrArchiveRefused) {
				t.Fatalf("read = %v; want ErrArchiveRefused", err)
			}
			if !strings.Contains(err.Error(), row.names) {
				t.Errorf("refusal %q does not name %q", err, row.names)
			}
		})
	}
	// A NUL in a path cannot be written as tar (archive/tar refuses it, and its reader refuses a PAX path
	// holding one), so the rule is shown on the Writer, which shares it with the reader.
	t.Run("NUL in path", func(t *testing.T) {
		wr, _ := movepack.NewWriter(io.Discard, set)
		if err := wr.Add(movepack.Header{Path: w + "/a\x00b", Type: movepack.TypeDir}, nil); !errors.Is(err, movepack.ErrArchiveRefused) {
			t.Fatalf("add = %v; want ErrArchiveRefused", err)
		}
	})
	// GNU sparse: archive/tar writes none, so the typeflag of a plain header is set to 'S' by hand.
	t.Run("GNU sparse", func(t *testing.T) {
		b := raw(t, nil, rawEntry{h: tar.Header{Name: w + "/s", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644, Format: tar.FormatGNU}, content: "x"})
		setTypeflag(b[:512], tar.TypeGNUSparse)
		if _, err := readAll(bytes.NewReader(b), movepack.Options{}); !errors.Is(err, movepack.ErrArchiveRefused) {
			t.Fatalf("read = %v; want ErrArchiveRefused", err)
		}
	})
	// A size that does not match the bytes that follow: the stream ends inside the file.
	t.Run("size does not match the bytes", func(t *testing.T) {
		b := raw(t, nil, rawEntry{h: dirH(w)}, rawEntry{h: regH(w + "/a"), content: strings.Repeat("a", 2000)})
		cut := b[:512*3+100]
		if _, err := readAll(bytes.NewReader(cut), movepack.Options{}); !errors.Is(err, movepack.ErrArchiveRefused) {
			t.Fatalf("read = %v; want ErrArchiveRefused", err)
		}
	})
	// The control: the same shapes, well formed, read.
	stream, m := pack(t, dir(w), reg(w+"/a", "a"), dir(w+"/a.d"), link(w+"/l", "/etc"), reg(w+"/z", "z"))
	got, err := readAll(bytes.NewReader(stream), movepack.Options{})
	if err != nil || got.Digest != m.Digest || got.Entries != 5 {
		t.Fatalf("well-formed stream = %+v, %v; want digest %s, 5 entries", got, err, m.Digest)
	}
}

func setTypeflag(block []byte, flag byte) {
	block[156] = flag
	for i := 148; i < 156; i++ {
		block[i] = ' '
	}
	sum := 0
	for _, c := range block {
		sum += int(c)
	}
	copy(block[148:], []byte(strings.Repeat("0", 6-len(octal(sum)))+octal(sum)+"\x00 "))
}

func octal(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 8 {
		b = append([]byte{byte('0' + n%8)}, b...)
	}
	return string(b)
}

// TestMovePack_BoundsCountBytesRead: the bounds count entries and the bytes actually read; a declared
// size alone never trips them.
func TestMovePack_BoundsCountBytesRead(t *testing.T) {
	const w = "home/agent/work"
	stream, _ := pack(t, dir(w), reg(w+"/a", "a"), reg(w+"/b", "b"), reg(w+"/c", "c"))

	if _, err := readAll(bytes.NewReader(stream), movepack.Options{Limits: movepack.Limits{MaxEntries: 3}}); !errors.Is(err, movepack.ErrCarriedTooLarge) {
		t.Errorf("4 entries with a bound of 3 = %v; want ErrCarriedTooLarge", err)
	}
	if _, err := readAll(bytes.NewReader(stream), movepack.Options{Limits: movepack.Limits{MaxEntries: 4}}); err != nil {
		t.Errorf("4 entries with a bound of 4 = %v; want nil", err)
	}
	n := int64(len(stream))
	if _, err := readAll(bytes.NewReader(stream), movepack.Options{Limits: movepack.Limits{MaxBytes: n - 1}}); !errors.Is(err, movepack.ErrCarriedTooLarge) {
		t.Errorf("a stream of %d bytes with a bound of %d = %v; want ErrCarriedTooLarge", n, n-1, err)
	}
	if _, err := readAll(bytes.NewReader(stream), movepack.Options{Limits: movepack.Limits{MaxBytes: n}}); err != nil {
		t.Errorf("a stream of exactly its bound = %v; want nil", err)
	}

	// A header declaring a terabyte, followed by a few bytes: the bound is not tripped by the declaration,
	// and the stream is refused for its short entry.
	b := raw(t, nil, rawEntry{h: dirH(w)}, rawEntry{h: regH(w + "/big"), content: "few"})
	hdr := b[512 : 512*2]
	if bytes.Contains(b[512*2:], []byte("size=")) {
		t.Fatal("unexpected PAX size record")
	}
	copy(hdr[124:136], []byte("17777777777\x00"))
	setTypeflag(hdr, tar.TypeReg)
	_, err := readAll(bytes.NewReader(b), movepack.Options{Limits: movepack.Limits{MaxBytes: 1 << 20}})
	if !errors.Is(err, movepack.ErrArchiveRefused) || errors.Is(err, movepack.ErrCarriedTooLarge) {
		t.Errorf("a declared size past the bound with few bytes = %v; want ErrArchiveRefused, not a size bound", err)
	}

	// Many real bytes past the bound refuse, counted as read.
	big, _ := pack(t, dir(w), reg(w+"/big", strings.Repeat("x", 64<<10)))
	if _, err := readAll(bytes.NewReader(big), movepack.Options{Limits: movepack.Limits{MaxBytes: 32 << 10}}); !errors.Is(err, movepack.ErrCarriedTooLarge) {
		t.Errorf("64 KiB read with a 32 KiB bound = %v; want ErrCarriedTooLarge", err)
	}
}

// TestMovePack_DigestMismatchFailsClosed: a flipped byte, a truncated stream, a reordered entry and an
// extra file each fail, and the relay's re-emitted stream reads to the source's digest.
func TestMovePack_DigestMismatchFailsClosed(t *testing.T) {
	const w = "home/agent/work"
	files := []file{dir(w), reg(w+"/a", "alpha"), reg(w+"/b", "bravo")}
	stream, m := pack(t, files...)
	_ = m

	flipped := bytes.Clone(stream)
	i := bytes.Index(flipped, []byte("bravo"))
	flipped[i] ^= 0x01
	if _, err := readAll(bytes.NewReader(flipped), movepack.Options{}); !errors.Is(err, movepack.ErrDigestMismatch) {
		t.Errorf("flipped byte = %v; want ErrDigestMismatch", err)
	}

	for _, cut := range []int{len(stream) - 1, len(stream) - 40, bytes.Index(stream, []byte("wardyn-movepack")), len(stream) / 2, 700} {
		if _, err := readAll(bytes.NewReader(stream[:cut]), movepack.Options{}); err == nil {
			t.Errorf("stream cut at %d of %d read without an error", cut, len(stream))
		}
	}

	reordered := raw(t, manifestLine(stream), rawEntry{h: dirH(w)}, rawEntry{h: regH(w + "/b"), content: "bravo"}, rawEntry{h: regH(w + "/a"), content: "alpha"})
	if _, err := readAll(bytes.NewReader(reordered), movepack.Options{}); err == nil {
		t.Error("reordered entries read without an error")
	}

	extra := raw(t, manifestLine(stream), rawEntry{h: dirH(w)}, rawEntry{h: regH(w + "/a"), content: "alpha"}, rawEntry{h: regH(w + "/b"), content: "bravo"}, rawEntry{h: regH(w + "/c"), content: "extra"})
	if _, err := readAll(bytes.NewReader(extra), movepack.Options{}); !errors.Is(err, movepack.ErrDigestMismatch) {
		t.Errorf("extra file = %v; want ErrDigestMismatch", err)
	}
	same := raw(t, manifestLine(stream), rawEntry{h: dirH(w)}, rawEntry{h: regH(w + "/a"), content: "alpha"}, rawEntry{h: regH(w + "/b"), content: "bravo"})
	if _, err := readAll(bytes.NewReader(same), movepack.Options{}); err != nil {
		t.Errorf("the same entries under the same manifest = %v; want nil", err)
	}

	// The relay re-emits what it read; the destination reads the source's digest.
	r, err := movepack.NewReader(bytes.NewReader(stream), movepack.Options{Set: set})
	if err != nil {
		t.Fatal(err)
	}
	var relayed bytes.Buffer
	rw, _ := movepack.NewWriter(&relayed, set)
	rm, err := movepack.Copy(rw, r)
	if err != nil || rm.Digest != m.Digest {
		t.Fatalf("relay = %+v, %v; want digest %s", rm, err, m.Digest)
	}
	dm, err := readAll(&relayed, movepack.Options{})
	if err != nil || dm.Digest != m.Digest {
		t.Fatalf("destination = %+v, %v; want digest %s", dm, err, m.Digest)
	}
}

// TestMoveSet_RefusesMalformedExclude: a reader and a writer refuse a set whose exclude pattern is empty or one
// path.Match cannot read, instead of excluding nothing with it.
func TestMoveSet_RefusesMalformedExclude(t *testing.T) {
	for _, pat := range []string{"**/[", "/home/agent/work/a[", "", "**/"} {
		bad := movepack.Set{Roots: set.Roots, Exclude: []string{pat}}
		if _, err := movepack.NewReader(bytes.NewReader(nil), movepack.Options{Set: bad}); !errors.Is(err, movepack.ErrArchiveRefused) {
			t.Errorf("reader with exclude %q = %v; want ErrArchiveRefused", pat, err)
		}
		if _, err := movepack.NewWriter(io.Discard, bad); !errors.Is(err, movepack.ErrArchiveRefused) {
			t.Errorf("writer with exclude %q = %v; want ErrArchiveRefused", pat, err)
		}
	}
	if _, err := movepack.NewWriter(io.Discard, set); err != nil {
		t.Fatalf("the well-formed set = %v; want nil", err)
	}
}
