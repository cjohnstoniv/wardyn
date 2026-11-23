// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package movepack_test

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
)

// Each test here pins one rule that the hostile-entry rows reach only through another rule, so removing the
// rule alone turns its test red.

const w = "home/agent/work"

// refusedNaming reads stream and wants ErrArchiveRefused whose text holds why.
func refusedNaming(t *testing.T, stream []byte, why string) {
	t.Helper()
	_, err := readAll(bytes.NewReader(stream), movepack.Options{})
	if !errors.Is(err, movepack.ErrArchiveRefused) || !strings.Contains(err.Error(), why) {
		t.Fatalf("read = %v; want ErrArchiveRefused naming %q", err, why)
	}
}

// TestMovePack_RefusesGNUSparsePAXRecords: a file whose PAX header carries a GNU.sparse record is refused as
// sparse, though archive/tar reads it as a plain file.
func TestMovePack_RefusesGNUSparsePAXRecords(t *testing.T) {
	h := regH(w + "/a")
	// archive/tar writes no GNU.sparse record, so one of the same length is renamed in the PAX data, which
	// no header checksum covers.
	h.PAXRecords = map[string]string{"XNU.sparse.comment": "x"}
	b := bytes.Replace(raw(t, nil, rawEntry{h: dirH(w)}, rawEntry{h: h, content: "a"}), []byte("XNU.sparse."), []byte("GNU.sparse."), 1)
	refusedNaming(t, b, "a GNU sparse entry")
}

// TestMovePack_RefusesGNUSparseType: an old-GNU sparse entry that archive/tar reads cleanly, as type 'S', is
// refused as sparse.
func TestMovePack_RefusesGNUSparseType(t *testing.T) {
	b := raw(t, nil, rawEntry{h: tar.Header{Name: w + "/s", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644, Format: tar.FormatGNU}, content: "x"})
	copy(b[483:495], fmt.Sprintf("%011o\x00", 1)) // the real size; the sparse map stays empty
	setTypeflag(b[:512], tar.TypeGNUSparse)
	if th, err := tar.NewReader(bytes.NewReader(b)).Next(); err != nil || th.Typeflag != tar.TypeGNUSparse {
		t.Fatalf("archive/tar reads the entry as %v; the test needs it read as a sparse entry", err)
	}
	refusedNaming(t, b, "a GNU sparse entry")
}

// TestMovePack_RefusesAnAbsolutePath: an absolute path is refused as one, not only through its empty first
// component. Writer.Skip applies the path rules alone.
func TestMovePack_RefusesAnAbsolutePath(t *testing.T) {
	wr, err := movepack.NewWriter(io.Discard, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := wr.Skip("/" + w + "/sock"); !errors.Is(err, movepack.ErrArchiveRefused) || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("skip of an absolute path = %v; want it refused as absolute", err)
	}
}

// TestMovePack_RefusesASizeOnANonFile: a directory or symlink header with a size, which archive/tar reads
// without its bytes, is refused.
func TestMovePack_RefusesASizeOnANonFile(t *testing.T) {
	d := dirH(w + "/d")
	d.Size = 5
	refusedNaming(t, raw(t, nil, rawEntry{h: dirH(w)}, rawEntry{h: d}), "size 5 for a dir")
	l := tar.Header{Name: w + "/l", Typeflag: tar.TypeSymlink, Linkname: "x", Size: 5, Format: tar.FormatPAX}
	refusedNaming(t, raw(t, nil, rawEntry{h: dirH(w)}, rawEntry{h: l}), "size 5 for a symlink")
}

// TestMoveScan_RefusesADeltaOverTheObjectBound: a delta declaring more than one object may hold is refused as
// too large before it is read, not as unreadable once it gives less.
func TestMoveScan_RefusesADeltaOverTheObjectBound(t *testing.T) {
	const o = w + "/.git/objects"
	stream := gitTree(t, dir(o+"/pack"), reg(o+"/pack/pack-1.pack", packFile(t, 1, packObj{kind: 6, declared: 4096, body: delta("abc")})))
	_, err := readAll(bytes.NewReader(stream), scanning(movepack.Limits{MaxObjectBytes: 1024}))
	if !errors.Is(err, movepack.ErrCarriedTooLarge) {
		t.Fatalf("read = %v; want ErrCarriedTooLarge", err)
	}
}

// TestMovePack_CopyRefusesADifferingDigest: Copy refuses when what it re-emitted is not what it read.
func TestMovePack_CopyRefusesADifferingDigest(t *testing.T) {
	stream, _ := pack(t, reg(w+"/a", "a"))
	r, err := movepack.NewReader(bytes.NewReader(stream), movepack.Options{Set: set})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := movepack.NewWriter(io.Discard, set)
	if err != nil {
		t.Fatal(err)
	}
	// An entry the read stream does not hold, so the re-emitted stream differs from it.
	if err := out.Add(movepack.Header{Path: w, Type: movepack.TypeDir, Mode: 0o755}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := movepack.Copy(out, r); !errors.Is(err, movepack.ErrDigestMismatch) {
		t.Fatalf("copy = %v; want ErrDigestMismatch", err)
	}
}

// TestMovePack_WriterRefusesOutOfOrder: the writer refuses an entry that does not sort after the one before.
func TestMovePack_WriterRefusesOutOfOrder(t *testing.T) {
	wr, err := movepack.NewWriter(io.Discard, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := wr.Add(reg(w+"/b", "b").h, strings.NewReader("b")); err != nil {
		t.Fatal(err)
	}
	if err := wr.Add(reg(w+"/a", "a").h, strings.NewReader("a")); !errors.Is(err, movepack.ErrArchiveRefused) {
		t.Fatalf("add out of order = %v; want ErrArchiveRefused", err)
	}
}

// TestMovePack_WriterMasksModeBits: the writer clears setuid, setgid and sticky bits, so what it writes reads.
func TestMovePack_WriterMasksModeBits(t *testing.T) {
	f := reg(w+"/a", "a")
	f.h.Mode = 0o6755
	stream, _ := pack(t, f)
	th, err := tar.NewReader(bytes.NewReader(stream)).Next()
	if err != nil || th.Mode != 0o755 {
		t.Fatalf("written mode = %o, %v; want 0755", th.Mode, err)
	}
	if _, err := readAll(bytes.NewReader(stream), movepack.Options{}); err != nil {
		t.Fatalf("read = %v; want nil", err)
	}
}

// TestMovePack_RefusesASkippedPathOutsideTheRoots: a manifest listing a skipped file outside the roots is
// refused, though the skipped list is outside the digest and every count matches.
func TestMovePack_RefusesASkippedPathOutsideTheRoots(t *testing.T) {
	stream, m := pack(t, reg(w+"/a", "a"))
	m.Skipped = []string{"etc/passwd"}
	line, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	body := stream[:bytes.LastIndex(stream, []byte("wardyn-movepack-manifest-v1 "))]
	forged := append(append(append([]byte{}, body...), "wardyn-movepack-manifest-v1 "...), append(line, '\n')...)
	refusedNaming(t, forged, `skipped file "etc/passwd"`)
}
