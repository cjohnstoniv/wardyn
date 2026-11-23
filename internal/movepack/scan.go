// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package movepack

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// The scan reads what is cheap and certain beyond a file's stored bytes: every loose git object inflated,
// every packed object stored whole inflated, and the literal bytes a packed delta's inserts carry, joined
// across consecutive inserts. It does not resolve deltas, so a value a delta builds partly from its base
// is not found. A git directory is one whose HEAD and objects/ both appear in the stream; HEAD sorts first.
//
// Every inflate counts what it writes: one object at most its own declared size and Limits.MaxObjectBytes,
// all of a stream's objects at most Limits.MaxInflated, and a pack at most Limits.MaxPackObjects objects.

// gitFeed is one git file's decoder, fed the file's bytes through a pipe as the caller reads them.
type gitFeed struct {
	pw   *io.PipeWriter
	done chan error
}

// startGit starts the decoder p needs, or returns nil for a file that is no git file the scan reads.
func (r *Reader) startGit(p string) *gitFeed {
	var decode func(io.Reader) error
	if path.Base(p) == ".git" {
		decode = func(src io.Reader) error { return r.gitPointer(p, src) }
	} else if gd, rest, ok := r.inGitObjects(p); ok {
		dir, name := path.Split(rest)
		switch {
		case rest == "info/alternates":
			decode = func(src io.Reader) error { return r.alternates(gd, src) }
		case dir == "pack/" && strings.HasSuffix(name, ".pack"):
			decode = func(src io.Reader) error { return r.pack(p, src) }
		case len(dir) == 3 && isHex(dir[:2]) && (len(name) == 38 || len(name) == 62) && isHex(name):
			decode = func(src io.Reader) error { return r.loose(dir[:2]+name, src) }
		}
	}
	if decode == nil {
		return nil
	}
	pr, pw := io.Pipe()
	g := &gitFeed{pw: pw, done: make(chan error, 1)}
	go func() {
		err := decode(pr)
		if err == nil {
			// The rest of the file (a pack's checksum) is read and dropped.
			_, err = io.Copy(io.Discard, pr)
		}
		if err != nil {
			_ = pr.CloseWithError(err)
		}
		g.done <- err
	}()
	return g
}

// inGitObjects splits p, a path under an open git directory's objects/, into that directory and the rest.
// The outermost such directory wins.
func (r *Reader) inGitObjects(p string) (gitDir, rest string, ok bool) {
	for _, gd := range r.gitOpen {
		if rest, ok := strings.CutPrefix(p, gd+"/objects/"); ok {
			return gd, rest, true
		}
	}
	return "", "", false
}

// gitPointer reads a .git file, "gitdir: <path>", and refuses a path outside the carried roots.
func (r *Reader) gitPointer(p string, src io.Reader) error {
	b, err := io.ReadAll(io.LimitReader(src, maxPathBytes+64))
	if err != nil {
		return err
	}
	target, ok := strings.CutPrefix(strings.TrimRight(string(b), "\r\n"), "gitdir: ")
	if !ok || target == "" || strings.ContainsAny(target, "\n\x00") {
		return fmt.Errorf("%w: %q is not a gitdir pointer", ErrScanUnreadable, p)
	}
	if !r.insideRoots(path.Dir(p), target) {
		return fmt.Errorf("%w: %q points at %q", ErrGitOutsideRoots, p, target)
	}
	return nil
}

// alternates reads objects/info/alternates and refuses a line naming a path outside the carried roots.
func (r *Reader) alternates(gitDir string, src io.Reader) error {
	b, err := io.ReadAll(io.LimitReader(src, maxManifestBytes+1))
	if err != nil {
		return err
	}
	if len(b) > maxManifestBytes {
		return fmt.Errorf("%w: %s/objects/info/alternates is over %d bytes", ErrScanUnreadable, gitDir, maxManifestBytes)
	}
	for line := range strings.Lines(string(b)) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !r.insideRoots(gitDir+"/objects", line) {
			return fmt.Errorf("%w: %s/objects/info/alternates names %q", ErrGitOutsideRoots, gitDir, line)
		}
	}
	return nil
}

// insideRoots reports whether target, absolute or relative to dir (a stream path), is under a carried root.
func (r *Reader) insideRoots(dir, target string) bool {
	var p string
	if strings.HasPrefix(target, "/") {
		p = path.Clean(target)[1:]
	} else {
		p = path.Join(dir, target)
	}
	return p != "" && p != "." && p != ".." && !strings.HasPrefix(p, "../") && underAny(p, r.roots)
}

// loose scans one loose object: zlib of "<type> <size>\x00<content>".
func (r *Reader) loose(id string, src io.Reader) error {
	zr, err := zlib.NewReader(src)
	if err != nil {
		return fmt.Errorf("%w: loose object %s: %v", ErrScanUnreadable, id, err)
	}
	in := &inflated{r: r, src: zr, where: "loose object " + id}
	br := bufio.NewReaderSize(in, 64)
	head, err := br.ReadSlice(0)
	if err != nil {
		return r.unreadable(err, "loose object %s has no header", id)
	}
	kind, sz, ok := strings.Cut(string(head[:len(head)-1]), " ")
	size, perr := strconv.ParseInt(sz, 10, 64)
	if !ok || perr != nil || size < 0 || !objectKind(kind) {
		return fmt.Errorf("%w: loose object %s has header %q", ErrScanUnreadable, id, head)
	}
	return r.scanObject("loose object "+id, br, size)
}

// scanObject scans exactly size inflated bytes of one object from src and refuses a stream that is
// shorter, or that would inflate past size or the object bound.
func (r *Reader) scanObject(where string, src io.Reader, size int64) error {
	if size > r.lim.MaxObjectBytes {
		return fmt.Errorf("%w: %s declares %d bytes, over %d", ErrCarriedTooLarge, where, size, r.lim.MaxObjectBytes)
	}
	det := r.o.Detect()
	n, err := io.Copy(det, io.LimitReader(src, size))
	if err != nil {
		return r.unreadable(err, "%s", where)
	}
	if n < size {
		return fmt.Errorf("%w: %s gave %d of its %d bytes", ErrScanUnreadable, where, n, size)
	}
	if err := r.ended(src, where); err != nil {
		return err
	}
	if det.Found() {
		return fmt.Errorf("%w: in %s", ErrValueFound, where)
	}
	return nil
}

// ended checks that an object's inflate stops at its declared size and that its stream is whole.
func (r *Reader) ended(src io.Reader, where string) error {
	var one [1]byte
	n, err := io.ReadFull(src, one[:])
	switch {
	case n > 0:
		return fmt.Errorf("%w: %s inflates past its declared size", ErrCarriedTooLarge, where)
	case err == io.EOF:
		return nil
	}
	return r.unreadable(err, "%s", where)
}

// unreadable is a decode error, unless a bound refused first.
func (r *Reader) unreadable(err error, format string, args ...any) error {
	if errors.Is(err, ErrCarriedTooLarge) {
		return err
	}
	return fmt.Errorf("%w: %s: %v", ErrScanUnreadable, fmt.Sprintf(format, args...), err)
}

// The pack object types (gitformat-pack).
const (
	packCommit   = 1
	packTree     = 2
	packBlob     = 3
	packTag      = 4
	packOfsDelta = 6
	packRefDelta = 7
)

// pack scans one pack file in stream order: each object stored whole, inflated, and each delta's inserts.
// A REF_DELTA's base is read as a SHA-1 id; a SHA-256 repository's pack then fails to decode and refuses.
func (r *Reader) pack(p string, src io.Reader) error {
	br := bufio.NewReaderSize(src, 64<<10)
	var hdr [12]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return r.unreadable(err, "pack %q header", p)
	}
	version := binary.BigEndian.Uint32(hdr[4:8])
	count := binary.BigEndian.Uint32(hdr[8:12])
	switch {
	case !bytes.Equal(hdr[:4], []byte("PACK")) || (version != 2 && version != 3):
		return fmt.Errorf("%w: %q is not a version 2 or 3 pack", ErrScanUnreadable, p)
	case count > r.lim.MaxPackObjects:
		return fmt.Errorf("%w: pack %q holds %d objects, over %d", ErrCarriedTooLarge, p, count, r.lim.MaxPackObjects)
	}
	var zr io.ReadCloser
	for i := range count {
		where := fmt.Sprintf("object %d of pack %q", i, p)
		kind, size, err := packHeader(br)
		if err != nil {
			return r.unreadable(err, "%s", where)
		}
		switch kind {
		case packCommit, packTree, packBlob, packTag:
		case packOfsDelta:
			if _, err := varint(br, true); err != nil {
				return r.unreadable(err, "%s", where)
			}
		case packRefDelta:
			if _, err := io.ReadFull(br, make([]byte, 20)); err != nil {
				return r.unreadable(err, "%s", where)
			}
		default:
			return fmt.Errorf("%w: %s has type %d", ErrScanUnreadable, where, kind)
		}
		if zr == nil {
			zr, err = zlib.NewReader(br)
		} else {
			err = zr.(zlib.Resetter).Reset(br, nil)
		}
		if err != nil {
			return r.unreadable(err, "%s", where)
		}
		in := &inflated{r: r, src: zr, where: where}
		if kind == packOfsDelta || kind == packRefDelta {
			err = r.scanDelta(where, in, size)
		} else {
			err = r.scanObject(where, in, size)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// scanDelta reads one delta's instructions and scans the bytes its inserts carry, each run of
// consecutive inserts as one stream.
func (r *Reader) scanDelta(where string, src io.Reader, size int64) error {
	if size > r.lim.MaxObjectBytes {
		return fmt.Errorf("%w: %s declares %d bytes, over %d", ErrCarriedTooLarge, where, size, r.lim.MaxObjectBytes)
	}
	lr := &io.LimitedReader{R: src, N: size}
	br := bufio.NewReader(lr)
	// A delta cut at its declared size mid-instruction is too large if more inflates, else unreadable.
	cut := func(err error) error {
		if lr.N == 0 {
			if e := r.ended(src, where); e != nil {
				return e
			}
		}
		return r.unreadable(err, "%s", where)
	}
	for range 2 { // the base's and the result's sizes
		if _, err := varint(br, false); err != nil {
			return cut(err)
		}
	}
	var det Detector
	found := false
	for {
		op, err := br.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return cut(err)
		}
		switch {
		case op&0x80 != 0: // a copy from the base: its offset and size bytes are skipped
			n := 0
			for bit := range 7 {
				if op&(1<<bit) != 0 {
					n++
				}
			}
			if _, err := br.Discard(n); err != nil {
				return cut(err)
			}
			if det != nil {
				found = found || det.Found()
				det = nil
			}
		case op == 0:
			return fmt.Errorf("%w: %s has a reserved delta instruction", ErrScanUnreadable, where)
		default: // an insert of op literal bytes
			if det == nil {
				det = r.o.Detect()
			}
			if _, err := io.CopyN(det, br, int64(op)); err != nil {
				return cut(err)
			}
		}
	}
	if det != nil {
		found = found || det.Found()
	}
	if lr.N > 0 {
		return fmt.Errorf("%w: %s gave %d of its %d bytes", ErrScanUnreadable, where, size-lr.N, size)
	}
	if err := r.ended(src, where); err != nil {
		return err
	}
	if found {
		return fmt.Errorf("%w: in %s", ErrValueFound, where)
	}
	return nil
}

// packHeader reads an object's type and declared size.
func packHeader(br *bufio.Reader) (kind int, size int64, err error) {
	c, err := br.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	kind, size = int(c>>4)&7, int64(c&15)
	for shift := 4; c&0x80 != 0; shift += 7 {
		if shift > 56 {
			return 0, 0, errors.New("object size overflows")
		}
		if c, err = br.ReadByte(); err != nil {
			return 0, 0, err
		}
		size |= int64(c&0x7f) << shift
	}
	return kind, size, nil
}

// varint reads a delta size (little-endian base 128) or, with offset, an OFS_DELTA offset (git's
// big-endian form with its +1 per continued byte).
func varint(br *bufio.Reader, offset bool) (uint64, error) {
	var v uint64
	for i := 0; ; i++ {
		if i > 9 {
			return 0, errors.New("varint overflows")
		}
		c, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		if offset {
			if i > 0 {
				v++
			}
			v = v<<7 | uint64(c&0x7f)
		} else {
			v |= uint64(c&0x7f) << (7 * i)
		}
		if c&0x80 == 0 {
			return v, nil
		}
	}
}

// inflated counts a git object's inflated bytes against the stream's bound.
type inflated struct {
	r     *Reader
	src   io.Reader
	where string
}

func (in *inflated) Read(p []byte) (int, error) {
	n, err := in.src.Read(p)
	in.r.inflated += int64(n)
	if in.r.inflated > in.r.lim.MaxInflated {
		return n, fmt.Errorf("%w: git objects inflate past %d bytes at %s", ErrCarriedTooLarge, in.r.lim.MaxInflated, in.where)
	}
	return n, err
}

func objectKind(k string) bool { return k == "blob" || k == "tree" || k == "commit" || k == "tag" }

func isHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return s != ""
}
