// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package movepack

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Writer writes one stream. Add takes entries in stream order; Close writes the manifest.
type Writer struct {
	tw      *tar.Writer
	out     io.Writer
	roots   []string
	exclude []string
	last    string
	d       *digest
	skipped []string
	closed  bool
}

// NewWriter starts a stream of set's files on w.
func NewWriter(w io.Writer, set Set) (*Writer, error) {
	roots, err := set.roots()
	if err != nil {
		return nil, err
	}
	return &Writer{tw: tar.NewWriter(w), out: w, roots: roots, exclude: set.Exclude, d: newDigest()}, nil
}

// Add writes one entry. A file's content gives exactly h.Size bytes. Mode bits other than 0777 are
// cleared. It refuses an entry out of order, outside the roots, left out by the set, of another type, or
// whose content is short.
func (w *Writer) Add(h Header, content io.Reader) error {
	if w.closed {
		return fmt.Errorf("%w: stream already closed", ErrArchiveRefused)
	}
	if err := checkPath(h.Path); err != nil {
		return fmt.Errorf("%w: entry %d %q: %v", ErrArchiveRefused, w.d.entries, h.Path, err)
	}
	switch {
	case w.d.entries > 0 && !before(w.last, h.Path):
		return fmt.Errorf("%w: entry %d %q does not sort after %q", ErrArchiveRefused, w.d.entries, h.Path, w.last)
	case !underAny(h.Path, w.roots):
		return fmt.Errorf("%w: entry %d %q is outside the carried roots", ErrArchiveRefused, w.d.entries, h.Path)
	case Excluded(h.Path, w.exclude):
		return fmt.Errorf("%w: entry %d %q is left out of the move", ErrArchiveRefused, w.d.entries, h.Path)
	}
	h.Mode &= 0o777
	th := &tar.Header{Name: h.Path, Mode: h.Mode, Format: tar.FormatPAX}
	switch h.Type {
	case TypeDir:
		th.Typeflag, th.Name, h.Size, h.Link = tar.TypeDir, h.Path+"/", 0, ""
	case TypeFile:
		if h.Size < 0 {
			return fmt.Errorf("%w: entry %d %q has a negative size", ErrArchiveRefused, w.d.entries, h.Path)
		}
		th.Typeflag, th.Size, h.Link = tar.TypeReg, h.Size, ""
	case TypeSymlink:
		if h.Link == "" || len(h.Link) > maxLinkBytes || strings.IndexByte(h.Link, 0) >= 0 {
			return fmt.Errorf("%w: entry %d %q has an empty, NUL-bearing or over-long link target", ErrArchiveRefused, w.d.entries, h.Path)
		}
		if isRoot(h.Path, w.roots) {
			return fmt.Errorf("%w: carried root %q is a symlink", ErrArchiveRefused, "/"+h.Path)
		}
		th.Typeflag, th.Linkname, h.Size = tar.TypeSymlink, h.Link, 0
	default:
		return fmt.Errorf("%w: entry %d %q has type %q", ErrArchiveRefused, w.d.entries, h.Path, h.Type)
	}
	if err := w.tw.WriteHeader(th); err != nil {
		return err
	}
	var sum []byte
	if h.Type == TypeFile {
		hs := sha256.New()
		n, err := io.CopyN(io.MultiWriter(w.tw, hs), content, h.Size)
		if err != nil {
			return fmt.Errorf("%w: entry %d %q gave %d of %d bytes: %v", ErrArchiveRefused, w.d.entries, h.Path, n, h.Size, err)
		}
		sum = hs.Sum(nil)
	}
	w.last = h.Path
	w.d.add(h, sum)
	return nil
}

// Skip records a file the exporter did not carry (a socket, fifo or device) in the manifest.
func (w *Writer) Skip(p string) error {
	if err := checkPath(p); err != nil {
		return fmt.Errorf("%w: skipped %q: %v", ErrArchiveRefused, p, err)
	}
	if len(w.skipped) >= maxSkipped {
		return fmt.Errorf("%w: over %d skipped files", ErrCarriedTooLarge, maxSkipped)
	}
	w.skipped = append(w.skipped, p)
	return nil
}

// Close ends the archive and writes the manifest. It does not close the underlying writer.
func (w *Writer) Close() (Manifest, error) {
	if w.closed {
		return Manifest{}, fmt.Errorf("%w: stream already closed", ErrArchiveRefused)
	}
	w.closed = true
	if err := w.tw.Close(); err != nil {
		return Manifest{}, err
	}
	m := w.d.manifest(w.skipped)
	b, err := json.Marshal(m)
	if err != nil {
		return Manifest{}, err
	}
	if _, err := io.WriteString(w.out, manifestMagic+string(b)+"\n"); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func isRoot(p string, roots []string) bool {
	for _, r := range roots {
		if p == r {
			return true
		}
	}
	return false
}

// FromTar adds a substrate's tar of one root to w: the archive a container engine or a helper pod gives
// for that root, whose names start with base (the root's last component). Its order must already be the
// stream's (a directory walk in name order is). Excluded entries are passed over. A hardlink is carried as
// a file with its target's content, which flatten opens by the target's absolute path. Fifos and devices
// are skipped and listed. Any other entry type is refused.
func FromTar(w *Writer, tr *tar.Reader, root, base string, flatten func(abs string) (io.ReadCloser, int64, error)) error {
	rel := strings.TrimPrefix(root, "/")
	for {
		th, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: reading the substrate's archive of %s: %v", ErrArchiveRefused, root, err)
		}
		name := strings.TrimSuffix(th.Name, "/")
		var p string
		switch {
		case name == base:
			p = rel
		case strings.HasPrefix(name, base+"/"):
			p = rel + name[len(base):]
		default:
			return fmt.Errorf("%w: the substrate's archive of %s holds %q", ErrArchiveRefused, root, th.Name)
		}
		if Excluded(p, w.exclude) {
			continue
		}
		h := Header{Path: p, Mode: th.Mode}
		switch th.Typeflag {
		case tar.TypeDir:
			h.Type = TypeDir
			err = w.Add(h, nil)
		case tar.TypeReg:
			h.Type, h.Size = TypeFile, th.Size
			err = w.Add(h, tr)
		case tar.TypeSymlink:
			h.Type, h.Link = TypeSymlink, th.Linkname
			err = w.Add(h, nil)
		case tar.TypeLink:
			err = addFlattened(w, h, root, base, th.Linkname, flatten)
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			err = w.Skip(p)
		default:
			err = fmt.Errorf("%w: %q has tar type %q", ErrArchiveRefused, th.Name, th.Typeflag)
		}
		if err != nil {
			return err
		}
	}
}

// addFlattened carries a hardlink as a file holding its target's content.
func addFlattened(w *Writer, h Header, root, base, target string, flatten func(string) (io.ReadCloser, int64, error)) error {
	target = strings.TrimSuffix(target, "/")
	var abs string
	switch {
	case target == base:
		abs = root
	case strings.HasPrefix(target, base+"/"):
		abs = root + target[len(base):]
	default:
		return fmt.Errorf("%w: hardlink %q names %q outside its root", ErrArchiveRefused, h.Path, target)
	}
	if flatten == nil {
		return fmt.Errorf("%w: hardlink %q cannot be flattened here", ErrArchiveRefused, h.Path)
	}
	rc, size, err := flatten(abs)
	if err != nil {
		return fmt.Errorf("%w: hardlink %q: reading %s: %v", ErrArchiveRefused, h.Path, abs, err)
	}
	defer rc.Close()
	h.Type, h.Size = TypeFile, size
	return w.Add(h, rc)
}
