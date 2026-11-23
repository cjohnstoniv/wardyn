// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package movepack

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"path"
	"slices"
	"strings"
)

// Detector is the secrets check the scan writes one byte stream into: a file's stored bytes, one inflated
// git object, or one delta's joined inserts. Found reports a value in what was written.
// secretmask.Detector is one.
type Detector interface {
	io.Writer
	Found() bool
}

// Options is how a Reader reads.
type Options struct {
	// Set is what the move carries: an entry outside its roots, or one it leaves out, is refused.
	Set    Set
	Limits Limits
	// Detect, when set, scans the stream: it is called for each byte stream the scan reads, and the reader
	// also decodes the git directories in the stream and checks where they point. Nil reads without a scan.
	Detect func() Detector
}

// Reader reads one stream and applies every rule of it before a caller sees an entry's bytes. Next
// returns entries in order; its io.EOF comes only after the manifest has been read and matched.
type Reader struct {
	o     Options
	lim   Limits
	roots []string
	cr    *countingReader
	tr    *tar.Reader
	d     *digest
	index int
	last  string
	// lastLink is the previous entry when it was a symlink. In the stream's order every descendant of an
	// entry follows it at once, so only that entry can be a later path's ancestor: the reader keeps no
	// per-entry state, and its memory grows with the path's depth, never the entry count.
	lastLink string
	// dirs is the directory entries the previous entry is inside or is, outermost first: the only ones a
	// later entry's parent can be.
	dirs []string
	// gitOpen is the git directories (a directory holding HEAD) the current path is inside, outermost
	// first. It is kept only for a scan.
	gitOpen  []string
	cur      *entry
	inflated int64
	m        Manifest
	done     bool
	err      error
}

// NewReader starts reading a stream from src.
func NewReader(src io.Reader, o Options) (*Reader, error) {
	roots, err := o.Set.roots()
	if err != nil {
		return nil, err
	}
	lim := o.Limits.withDefaults()
	cr := &countingReader{r: src, max: lim.MaxBytes}
	return &Reader{o: o, lim: lim, roots: roots, cr: cr, tr: tar.NewReader(cr), d: newDigest()}, nil
}

// Next finishes the entry before it (its bytes are read to the end, through the digest and the scan) and
// returns the next one. A file's content is the returned reader, nil for the other types. Every error is
// final.
func (r *Reader) Next() (Header, io.Reader, error) {
	if r.err != nil {
		return Header{}, nil, r.err
	}
	if r.done {
		return Header{}, nil, io.EOF
	}
	if r.cur != nil {
		e := r.cur
		r.cur = nil
		if err := r.finish(e); err != nil {
			return Header{}, nil, r.fail(err)
		}
	}
	th, err := r.tr.Next()
	if err == io.EOF {
		if err := r.readManifest(); err != nil {
			return Header{}, nil, r.fail(err)
		}
		r.done = true
		return Header{}, nil, io.EOF
	}
	if err != nil {
		return Header{}, nil, r.fail(r.wrap(err, "reading the header after entry %d %q", r.index-1, r.last))
	}
	if r.index >= r.lim.MaxEntries {
		return Header{}, nil, r.fail(fmt.Errorf("%w: over %d entries", ErrCarriedTooLarge, r.lim.MaxEntries))
	}
	h, err := r.check(th)
	if err != nil {
		return Header{}, nil, r.fail(err)
	}
	r.index++
	r.last = h.Path
	r.trackDirs(h)
	r.lastLink = ""
	if h.Type == TypeSymlink {
		r.lastLink = h.Path
	}
	if r.o.Detect != nil {
		r.trackGit(h)
	}
	e := &entry{r: r, h: h, src: r.tr, hash: sha256.New()}
	if r.o.Detect != nil && h.Type == TypeFile {
		e.det = r.o.Detect()
		e.git = r.startGit(h.Path)
	}
	r.cur = e
	if h.Type != TypeFile {
		return h, nil, nil
	}
	return h, e, nil
}

// Close stops the current entry's git decoder, if any, for a caller that leaves a stream unfinished.
func (r *Reader) Close() {
	if r.cur != nil && r.cur.git != nil {
		_ = r.cur.git.pw.CloseWithError(fmt.Errorf("%w: the stream was left unfinished", ErrArchiveRefused))
	}
	_ = r.fail(fmt.Errorf("%w: the stream was closed", ErrArchiveRefused))
}

// Manifest is the stream's manifest, as read and matched; it is set once Next has returned io.EOF.
func (r *Reader) Manifest() Manifest { return r.m }

func (r *Reader) fail(err error) error {
	if r.err == nil {
		r.err = err
	}
	return r.err
}

// wrap names where a read failed, keeping a sentinel the error already carries.
func (r *Reader) wrap(err error, format string, args ...any) error {
	for _, s := range []error{ErrCarriedTooLarge, ErrArchiveRefused, ErrDigestMismatch, ErrScanUnreadable, ErrGitOutsideRoots, ErrValueFound} {
		if errors.Is(err, s) {
			return err
		}
	}
	return fmt.Errorf("%w: %s: %v", ErrArchiveRefused, fmt.Sprintf(format, args...), err)
}

// check applies the entry rules to one tar header.
func (r *Reader) check(th *tar.Header) (Header, error) {
	at := func(why string, args ...any) error {
		return fmt.Errorf("%w: entry %d %q: %s", ErrArchiveRefused, r.index, th.Name, fmt.Sprintf(why, args...))
	}
	pax := 0
	for k, v := range th.PAXRecords {
		pax += len(k) + len(v) + 32
		if strings.HasPrefix(k, "GNU.sparse.") {
			return Header{}, at("a GNU sparse entry")
		}
	}
	if pax > maxPAXBytes {
		return Header{}, at("a PAX extended header over %d bytes", maxPAXBytes)
	}
	h := Header{Path: th.Name, Mode: th.Mode & 0o777}
	switch th.Typeflag {
	case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // an old writer's regular file is still one
		h.Type, h.Size = TypeFile, th.Size
	case tar.TypeDir:
		h.Type, h.Path = TypeDir, strings.TrimSuffix(th.Name, "/")
	case tar.TypeSymlink:
		h.Type, h.Link = TypeSymlink, th.Linkname
	case tar.TypeLink:
		return Header{}, at("a hardlink")
	case tar.TypeChar, tar.TypeBlock:
		return Header{}, at("a device")
	case tar.TypeFifo:
		return Header{}, at("a fifo")
	case tar.TypeXGlobalHeader:
		return Header{}, at("a PAX global header")
	case tar.TypeGNUSparse:
		return Header{}, at("a GNU sparse entry")
	default:
		return Header{}, at("tar type %q", th.Typeflag)
	}
	if th.Mode&0o7000 != 0 {
		return Header{}, at("a setuid, setgid or sticky bit")
	}
	if err := checkPath(h.Path); err != nil {
		return Header{}, at("%v", err)
	}
	switch {
	case th.Size < 0 || (h.Type != TypeFile && th.Size != 0):
		return Header{}, at("size %d for a %s", th.Size, h.Type)
	case h.Type == TypeSymlink && (th.Linkname == "" || len(th.Linkname) > maxLinkBytes || strings.IndexByte(th.Linkname, 0) >= 0):
		return Header{}, at("an empty, NUL-bearing or over-long link target")
	case h.Type == TypeSymlink && isRoot(h.Path, r.roots):
		return Header{}, at("a carried root that is a symlink")
	case r.index > 0 && !before(r.last, h.Path):
		return Header{}, at("does not sort after %q", r.last)
	case !underAny(h.Path, r.roots):
		return Header{}, at("outside the carried roots")
	case Excluded(h.Path, r.o.Set.Exclude):
		return Header{}, at("a path the move leaves out")
	}
	if r.lastLink != "" && under(h.Path, r.lastLink) {
		return Header{}, at("passes through the symlink entry %q", r.lastLink)
	}
	// Every entry below a root sits in a directory the stream declared, so nothing already at the destination
	// decides where it lands.
	if parent := path.Dir(h.Path); !isRoot(h.Path, r.roots) && !isRoot(parent, r.roots) && !slices.Contains(r.dirs, parent) {
		return Header{}, at("its directory %q is not an earlier entry of the stream", parent)
	}
	return h, nil
}

// trackDirs leaves the directory entries h is not inside, and opens h when it is one.
func (r *Reader) trackDirs(h Header) {
	for n := len(r.dirs); n > 0 && !under(h.Path, r.dirs[n-1]); n = len(r.dirs) {
		r.dirs = r.dirs[:n-1]
	}
	if h.Type == TypeDir {
		r.dirs = append(r.dirs, h.Path)
	}
}

// trackGit leaves the git directories h is not inside, and opens the one whose HEAD h is.
func (r *Reader) trackGit(h Header) {
	for n := len(r.gitOpen); n > 0 && !under(h.Path, r.gitOpen[n-1]); n = len(r.gitOpen) {
		r.gitOpen = r.gitOpen[:n-1]
	}
	if h.Type == TypeFile && path.Base(h.Path) == "HEAD" {
		r.gitOpen = append(r.gitOpen, path.Dir(h.Path))
	}
}

// readManifest reads the line after the archive's end and matches it with what was read.
func (r *Reader) readManifest() error {
	b, err := io.ReadAll(io.LimitReader(r.cr, maxManifestBytes+1))
	if err != nil {
		return r.wrap(err, "reading the manifest")
	}
	line, ok := bytes.CutPrefix(b, []byte(manifestMagic))
	switch {
	case len(b) > maxManifestBytes:
		return fmt.Errorf("%w: the manifest is over %d bytes", ErrArchiveRefused, maxManifestBytes)
	case !ok || !bytes.HasSuffix(line, []byte("\n")) || bytes.Count(line, []byte("\n")) != 1:
		return fmt.Errorf("%w: no manifest line after the archive's end", ErrDigestMismatch)
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("%w: the manifest does not parse: %v", ErrDigestMismatch, err)
	}
	if len(m.Skipped) > maxSkipped {
		return fmt.Errorf("%w: over %d skipped files", ErrCarriedTooLarge, maxSkipped)
	}
	for _, p := range m.Skipped {
		if err := checkPath(p); err != nil || !underAny(p, r.roots) {
			return fmt.Errorf("%w: skipped file %q is not a carried path", ErrArchiveRefused, p)
		}
	}
	got := r.d.manifest(m.Skipped)
	if got.Entries != m.Entries || got.Bytes != m.Bytes || got.Digest != m.Digest {
		return fmt.Errorf("%w: read %d entries, %d bytes, digest %s; the manifest says %d, %d, %s",
			ErrDigestMismatch, got.Entries, got.Bytes, got.Digest, m.Entries, m.Bytes, m.Digest)
	}
	r.m = got
	return nil
}

// finish reads the rest of an entry through the digest and the scan, and records it.
func (r *Reader) finish(e *entry) error {
	if _, err := io.Copy(io.Discard, e); err != nil {
		return err
	}
	if e.n != e.h.Size {
		return fmt.Errorf("%w: entry %d %q gave %d of %d bytes", ErrArchiveRefused, r.index-1, e.h.Path, e.n, e.h.Size)
	}
	if e.git != nil {
		_ = e.git.pw.Close()
		if err := <-e.git.done; err != nil {
			return err
		}
	}
	if e.det != nil && e.det.Found() {
		return fmt.Errorf("%w: in file %q", ErrValueFound, e.h.Path)
	}
	r.d.add(e.h, e.hash.Sum(nil))
	return nil
}

// entry is one file's content as the caller reads it: every byte also goes to the digest, the scan and,
// for a git file, its decoder.
type entry struct {
	r    *Reader
	h    Header
	src  io.Reader
	hash hash.Hash
	det  Detector
	git  *gitFeed
	n    int64
}

func (e *entry) Read(p []byte) (int, error) {
	if e.r.err != nil {
		return 0, e.r.err
	}
	n, err := e.src.Read(p)
	if n > 0 {
		e.n += int64(n)
		e.hash.Write(p[:n])
		if e.det != nil {
			_, _ = e.det.Write(p[:n])
		}
		if e.git != nil {
			if _, werr := e.git.pw.Write(p[:n]); werr != nil {
				return n, e.r.fail(e.r.wrap(werr, "decoding git file %q", e.h.Path))
			}
		}
	}
	if err != nil && err != io.EOF {
		return n, e.r.fail(e.r.wrap(err, "reading entry %d %q", e.r.index-1, e.h.Path))
	}
	return n, err
}

// countingReader counts the stream's bytes as they are read and refuses past max.
type countingReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.n >= c.max {
		// Only a byte past the bound refuses: a stream of exactly max bytes ends cleanly.
		var one [1]byte
		n, err := c.r.Read(one[:])
		if n > 0 {
			return 0, fmt.Errorf("%w: over %d bytes read", ErrCarriedTooLarge, c.max)
		}
		return 0, err
	}
	if int64(len(p)) > c.max-c.n {
		p = p[:c.max-c.n]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Copy re-emits every entry r reads onto w, the relay's and an importer's pass, and returns the manifest
// both agree on.
func Copy(w *Writer, r *Reader) (Manifest, error) {
	for {
		h, content, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Manifest{}, err
		}
		if err := w.Add(h, content); err != nil {
			return Manifest{}, r.fail(err)
		}
	}
	for _, p := range r.Manifest().Skipped {
		if err := w.Skip(p); err != nil {
			return Manifest{}, err
		}
	}
	m, err := w.Close()
	if err != nil {
		return Manifest{}, err
	}
	if m.Digest != r.Manifest().Digest {
		return Manifest{}, fmt.Errorf("%w: re-emitted %s, read %s", ErrDigestMismatch, m.Digest, r.Manifest().Digest)
	}
	return m, nil
}
