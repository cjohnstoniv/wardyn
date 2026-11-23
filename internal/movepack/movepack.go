// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package movepack is the stream a move carries a paused run's files in, from the source runner through
// the organisation to the destination. The agent wrote every byte of it, so every reader treats it as
// hostile.
//
// A stream is a PAX tar archive whose entries are the carried files, in strictly ascending order of their
// paths compared component by component, followed by its manifest. Paths are relative to the sandbox's
// root ("home/agent/work/a.txt"). Only directories, regular files and symlinks are carried, with no
// setuid, setgid or sticky bit. The manifest is one JSON line after the archive's end: the entry count, the
// file bytes, the files the exporter skipped (sockets, fifos and devices) and the digest of the entry
// records. An entry's record is its path, type, mode, size, the SHA-256 of its bytes and its link target,
// in stream order. Every reader recomputes the records from the bytes it read, so a flipped byte, a cut, a
// reordered entry or an extra file fails at the digest or before it.
//
// The order rule makes a repeated path impossible, and lets a reader refuse a path through an earlier
// symlink entry by checking its ancestors. Bounds are on bytes actually read, never on declared sizes.
package movepack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"path"
	"strings"
)

// The sentinels below are what a move's failure reason is read from; every one names the entry index and
// path, or the git object, it refused, and never a value.
var (
	// ErrArchiveRefused: an entry breaks a rule of the stream (its type, mode, path, order, size or the
	// manifest's shape), or an excluded path arrived.
	ErrArchiveRefused = errors.New("movepack: carried entry refused")
	// ErrCarriedTooLarge: the stream passed a bound on bytes read, entries, inflated git bytes or pack
	// objects.
	ErrCarriedTooLarge = errors.New("movepack: carried files over a size bound")
	// ErrDigestMismatch: the manifest does not match what was read.
	ErrDigestMismatch = errors.New("movepack: carried files differ from their manifest")
	// ErrScanUnreadable: a git file the scan must read could not be decoded.
	ErrScanUnreadable = errors.New("movepack: git file could not be decoded")
	// ErrGitOutsideRoots: a git directory names objects outside what is carried, so its history would not
	// arrive whole.
	ErrGitOutsideRoots = errors.New("movepack: git directory points outside the carried roots")
	// ErrValueFound: the scan found one of the run's secret values.
	ErrValueFound = errors.New("movepack: a secret value is in the carried files")
)

// Set is what a move carries: the roots it exports, as absolute paths in the sandbox, and the patterns it
// leaves out. A pattern is an absolute path, which leaves out that path and everything beneath it, or
// "**/" followed by a path.Match pattern, which leaves out any path whose last components match it and
// everything beneath that.
type Set struct {
	Roots   []string `json:"roots"`
	Exclude []string `json:"exclude,omitempty"`
}

// Limits bounds what a reader takes. A zero field takes its default.
type Limits struct {
	// MaxBytes bounds the stream's bytes, counted as they are read.
	MaxBytes int64
	// MaxEntries bounds the entries.
	MaxEntries int
	// MaxObjectBytes bounds one git object's inflated bytes (as well as its own declared size).
	MaxObjectBytes int64
	// MaxInflated bounds the inflated bytes of every git object in one stream.
	MaxInflated int64
	// MaxPackObjects bounds the objects one pack declares.
	MaxPackObjects uint32
}

// The defaults (estimates): a stream of 20 GiB, 2,000,000 entries, a git object of 1 GiB, four times the
// stream in inflated git bytes, and 5,000,000 objects in a pack.
const (
	DefaultMaxBytes       int64  = 20 << 30
	DefaultMaxEntries     int    = 2_000_000
	DefaultMaxObjectBytes int64  = 1 << 30
	DefaultMaxPackObjects uint32 = 5_000_000
)

// The fixed rules of an entry.
const (
	maxPathBytes      = 4096
	maxComponentBytes = 255
	maxLinkBytes      = 4096
	maxPAXBytes       = 64 << 10
	maxSkipped        = 10_000
	maxManifestBytes  = 8 << 20
)

func (l Limits) withDefaults() Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultMaxBytes
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultMaxEntries
	}
	if l.MaxObjectBytes <= 0 {
		l.MaxObjectBytes = DefaultMaxObjectBytes
	}
	if l.MaxInflated <= 0 {
		l.MaxInflated = 4 * l.MaxBytes
	}
	if l.MaxPackObjects == 0 {
		l.MaxPackObjects = DefaultMaxPackObjects
	}
	return l
}

// Type is a carried entry's type.
type Type string

const (
	TypeDir     Type = "dir"
	TypeFile    Type = "file"
	TypeSymlink Type = "symlink"
)

// Header is one carried entry. Mode holds only permission bits (0777). Size is a file's byte count, and 0
// for the other types. Link is a symlink's target, as its text.
type Header struct {
	Path string `json:"path"`
	Type Type   `json:"type"`
	Mode int64  `json:"mode"`
	Size int64  `json:"size"`
	Link string `json:"link,omitempty"`
}

// Manifest is a stream's manifest: what a writer wrote and a reader read. Digest is the hex SHA-256 of the
// entry records in stream order, so two streams with equal digests carry the same entries.
type Manifest struct {
	Entries int      `json:"entries"`
	Bytes   int64    `json:"bytes"`
	Digest  string   `json:"digest"`
	Skipped []string `json:"skipped,omitempty"`
}

// manifestMagic opens the manifest line after the archive's end.
const manifestMagic = "wardyn-movepack-manifest-v1 "

// record is one entry's line in the digest.
type record struct {
	Path   string `json:"p"`
	Type   Type   `json:"t"`
	Mode   int64  `json:"m"`
	Size   int64  `json:"s"`
	SHA256 string `json:"h,omitempty"`
	Link   string `json:"l,omitempty"`
}

// digest accumulates the entry records of one stream.
type digest struct {
	h       hash.Hash
	entries int
	bytes   int64
}

func newDigest() *digest { return &digest{h: sha256.New()} }

func (d *digest) add(h Header, sum []byte) {
	r := record{Path: h.Path, Type: h.Type, Mode: h.Mode, Size: h.Size, Link: h.Link}
	if h.Type == TypeFile {
		r.SHA256 = hex.EncodeToString(sum)
	}
	b, _ := json.Marshal(r) // a struct of strings and integers always marshals
	d.h.Write(append(b, '\n'))
	d.entries++
	d.bytes += h.Size
}

func (d *digest) manifest(skipped []string) Manifest {
	return Manifest{Entries: d.entries, Bytes: d.bytes, Digest: hex.EncodeToString(d.h.Sum(nil)), Skipped: skipped}
}

// roots turns a Set's roots into the stream's relative form, refusing a root that is not absolute and
// clean, is the sandbox's root, or holds or sits under another root, and an exclude pattern that is empty
// or malformed.
func (s Set) roots() ([]string, error) {
	if len(s.Roots) == 0 {
		return nil, fmt.Errorf("%w: a carried set names no root", ErrArchiveRefused)
	}
	out := make([]string, 0, len(s.Roots))
	for _, r := range s.Roots {
		if !strings.HasPrefix(r, "/") || path.Clean(r) != r || r == "/" {
			return nil, fmt.Errorf("%w: carried root %q is not an absolute, clean path below /", ErrArchiveRefused, r)
		}
		rel := r[1:]
		if err := checkPath(rel); err != nil {
			return nil, fmt.Errorf("%w: carried root %q: %v", ErrArchiveRefused, r, err)
		}
		for _, o := range out {
			if under(rel, o) || under(o, rel) {
				return nil, fmt.Errorf("%w: carried roots %q and %q overlap", ErrArchiveRefused, "/"+o, r)
			}
		}
		out = append(out, rel)
	}
	// A pattern path.Match cannot read would exclude nothing, and its files would be carried.
	for _, pat := range s.Exclude {
		rest := strings.TrimPrefix(strings.TrimPrefix(pat, "**/"), "/")
		if _, err := path.Match(rest, ""); rest == "" || err != nil {
			return nil, fmt.Errorf("%w: exclude pattern %q is malformed", ErrArchiveRefused, pat)
		}
	}
	return out, nil
}

// checkPath refuses an entry path the stream never carries: empty, absolute, holding NUL, a "." or ".."
// component or an empty one, over 4096 bytes, or with a component over 255.
func checkPath(p string) error {
	switch {
	case p == "":
		return errors.New("empty path")
	case len(p) > maxPathBytes:
		return fmt.Errorf("path over %d bytes", maxPathBytes)
	case strings.HasPrefix(p, "/"):
		return errors.New("absolute path")
	case strings.IndexByte(p, 0) >= 0:
		return errors.New("path holds NUL")
	}
	for c := range strings.SplitSeq(p, "/") {
		switch {
		case c == "" || c == ".":
			return errors.New("empty or '.' component")
		case c == "..":
			return errors.New("'..' component")
		case len(c) > maxComponentBytes:
			return fmt.Errorf("component over %d bytes", maxComponentBytes)
		}
	}
	return nil
}

// under reports whether p is root or beneath it.
func under(p, root string) bool { return p == root || strings.HasPrefix(p, root+"/") }

// underAny reports whether p is one of roots or beneath one.
func underAny(p string, roots []string) bool {
	for _, r := range roots {
		if under(p, r) {
			return true
		}
	}
	return false
}

// before reports whether a sorts before b, comparing paths component by component in byte order: a path
// sorts before every path beneath it, and those before its next sibling.
func before(a, b string) bool {
	n := min(len(a), len(b))
	for i := range n {
		ca, cb := a[i], b[i]
		if ca == cb {
			continue
		}
		if ca == '/' {
			return true
		}
		if cb == '/' {
			return false
		}
		return ca < cb
	}
	return len(a) < len(b)
}

// Excluded reports whether p, a stream path, is left out by any of patterns: p or one of its ancestors
// matches one.
func Excluded(p string, patterns []string) bool {
	for _, pat := range patterns {
		anywhere := strings.HasPrefix(pat, "**/")
		rest := strings.TrimPrefix(strings.TrimPrefix(pat, "**/"), "/")
		for q := p; q != "."; q = path.Dir(q) {
			if matches(rest, q, anywhere) {
				return true
			}
		}
	}
	return false
}

// matches reports whether q matches pat, or, when anywhere, whether q's last components do.
func matches(pat, q string, anywhere bool) bool {
	if ok, _ := path.Match(pat, q); ok {
		return true
	}
	if !anywhere {
		return false
	}
	for i := strings.IndexByte(q, '/'); i >= 0; i = strings.IndexByte(q, '/') {
		q = q[i+1:]
		if ok, _ := path.Match(pat, q); ok {
			return true
		}
	}
	return false
}
