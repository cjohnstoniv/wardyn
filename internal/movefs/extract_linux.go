// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package movefs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
)

// Extract writes the stream src beneath o's mount roots and returns the manifest its own reader read. Every
// rule of the stream applies before an entry's bytes are written. Each entry is written beneath the one mount
// root it is at or under, reached from that root's descriptor a component at a time with O_NOFOLLOW, so no
// write passes through a link. A file is created with O_EXCL. A directory is made 0700 and given its own mode
// after the last entry, deepest first; one that exists is accepted only when it is a real directory. A mount
// root is never written or changed. Symlinks are made last, once the manifest has matched, and one whose path
// exists is refused.
func Extract(src io.Reader, o ExtractOptions) (movepack.Manifest, error) {
	if o.MaxBytes <= 0 {
		return movepack.Manifest{}, fmt.Errorf("movefs: import: the stream's bound must be set, got %d bytes", o.MaxBytes)
	}
	mounts, err := mountRoots(o.Mounts)
	if err != nil {
		return movepack.Manifest{}, err
	}
	rd, err := movepack.NewReader(src, movepack.Options{Set: o.Set, Limits: movepack.Limits{MaxBytes: o.MaxBytes}})
	if err != nil {
		return movepack.Manifest{}, err
	}
	defer rd.Close()
	x := &extractor{base: o.Base, mounts: mounts, roots: map[string]int{}, cacheFD: -1}
	defer x.close()
	for {
		h, content, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return movepack.Manifest{}, err
		}
		if err := x.entry(h, content); err != nil {
			return movepack.Manifest{}, err
		}
	}
	// The reader's EOF comes only after the manifest has matched.
	if err := x.makeLinks(); err != nil {
		return movepack.Manifest{}, err
	}
	if err := x.setModes(); err != nil {
		return movepack.Manifest{}, err
	}
	return rd.Manifest(), nil
}

// mountRoots turns the mount roots into stream paths, refusing one that is not absolute and clean.
func mountRoots(abs []string) ([]string, error) {
	if len(abs) == 0 {
		return nil, errors.New("movefs: import: no mount root named")
	}
	out := make([]string, 0, len(abs))
	for _, m := range abs {
		if !strings.HasPrefix(m, "/") || filepath.Clean(m) != m || m == "/" {
			return nil, fmt.Errorf("movefs: import: mount root %q is not an absolute, clean path below /", m)
		}
		out = append(out, m[1:])
	}
	return out, nil
}

type heldLink struct {
	mount, rel, link string
}

type heldMode struct {
	mount, rel string
	mode       uint32
}

type extractor struct {
	base   string
	mounts []string
	roots  map[string]int // mount root -> its open descriptor
	links  []heldLink
	held   int
	modes  []heldMode
	// The parent directory the last entry was written in: entries of one directory arrive together.
	cacheMount, cacheDir string
	cacheFD              int
}

func (x *extractor) close() {
	if x.cacheFD >= 0 {
		_ = unix.Close(x.cacheFD)
	}
	for _, fd := range x.roots {
		_ = unix.Close(fd)
	}
}

// mountOf is the longest mount root p is at or under.
func (x *extractor) mountOf(p string) (string, bool) {
	best, ok := "", false
	for _, m := range x.mounts {
		if under(p, m) && len(m) >= len(best) {
			best, ok = m, true
		}
	}
	return best, ok
}

func (x *extractor) entry(h movepack.Header, content io.Reader) error {
	m, ok := x.mountOf(h.Path)
	if !ok {
		return fmt.Errorf("%w: entry %q is under no mount root this import writes", movepack.ErrArchiveRefused, h.Path)
	}
	if h.Path == m {
		if h.Type != movepack.TypeDir {
			return fmt.Errorf("%w: mount root %q arrives as a %s", movepack.ErrArchiveRefused, h.Path, h.Type)
		}
		_, err := x.root(m)
		return err
	}
	rest := h.Path[len(m)+1:]
	dir, name := "", rest
	if i := strings.LastIndexByte(rest, '/'); i >= 0 {
		dir, name = rest[:i], rest[i+1:]
	}
	if h.Type == movepack.TypeSymlink {
		if x.held += len(h.Path) + len(h.Link) + heldLinkCost; x.held > maxHeldLinkBytes {
			return fmt.Errorf("%w: over %d bytes of symlinks", movepack.ErrCarriedTooLarge, maxHeldLinkBytes)
		}
		x.links = append(x.links, heldLink{mount: m, rel: rest, link: h.Link})
		return nil
	}
	pfd, err := x.parent(m, dir)
	if err != nil {
		return err
	}
	mode := uint32(h.Mode) & 0o777
	switch h.Type {
	case movepack.TypeDir:
		return x.mkdir(pfd, m, rest, name, mode)
	case movepack.TypeFile:
		return x.writeFile(pfd, h, name, mode, content)
	}
	return fmt.Errorf("%w: entry %q has type %q", movepack.ErrArchiveRefused, h.Path, h.Type)
}

func (x *extractor) mkdir(pfd int, m, rest, name string, mode uint32) error {
	err := unix.Mkdirat(pfd, name, 0o700)
	if errors.Is(err, unix.EEXIST) {
		// Accepted only as a real directory: O_NOFOLLOW refuses a link standing there.
		fd, oerr := openDir(pfd, name)
		if oerr != nil {
			return fmt.Errorf("%w: %q exists and is not a directory: %v", movepack.ErrArchiveRefused, m+"/"+rest, oerr)
		}
		_ = unix.Close(fd)
		err = nil
	}
	if err != nil {
		return fmt.Errorf("movefs: import: make %q: %w", m+"/"+rest, err)
	}
	x.modes = append(x.modes, heldMode{mount: m, rel: rest, mode: mode})
	return nil
}

func (x *extractor) writeFile(pfd int, h movepack.Header, name string, mode uint32, content io.Reader) error {
	fd, err := unix.Openat(pfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("%w: %q already exists", movepack.ErrArchiveRefused, h.Path)
	}
	if err != nil {
		return fmt.Errorf("movefs: import: create %q: %w", h.Path, err)
	}
	f := os.NewFile(uintptr(fd), h.Path)
	defer f.Close()
	if content != nil {
		if _, err := io.CopyN(f, content, h.Size); err != nil {
			return fmt.Errorf("movefs: import: write %q: %w", h.Path, err)
		}
	}
	// The mode is set on the descriptor, so the process umask takes nothing from it.
	if err := unix.Fchmod(fd, mode); err != nil {
		return fmt.Errorf("movefs: import: set the mode of %q: %w", h.Path, err)
	}
	return f.Close()
}

// root is the open descriptor of mount root m. Only its last component is opened with O_NOFOLLOW: the
// components above it are the helper's own filesystem and the kubelet's mount points, not the agent's.
func (x *extractor) root(m string) (int, error) {
	if fd, ok := x.roots[m]; ok {
		return fd, nil
	}
	fd, err := unix.Open(filepath.Join(x.base, "/"+m), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("movefs: import: open mount root /%s: %w", m, err)
	}
	x.roots[m] = fd
	return fd, nil
}

// parent is the open descriptor of dir beneath mount root m, reached a component at a time with O_NOFOLLOW.
// The caller does not close it.
func (x *extractor) parent(m, dir string) (int, error) {
	if dir == "" {
		return x.root(m)
	}
	if x.cacheFD >= 0 && x.cacheMount == m && x.cacheDir == dir {
		return x.cacheFD, nil
	}
	fd, err := x.walkTo(m, dir)
	if err != nil {
		return -1, err
	}
	if x.cacheFD >= 0 {
		_ = unix.Close(x.cacheFD)
	}
	x.cacheMount, x.cacheDir, x.cacheFD = m, dir, fd
	return fd, nil
}

// walkTo opens dir beneath mount root m a component at a time and returns a descriptor the caller closes.
func (x *extractor) walkTo(m, dir string) (int, error) {
	rfd, err := x.root(m)
	if err != nil {
		return -1, err
	}
	cur := rfd
	for c := range strings.SplitSeq(dir, "/") {
		next, err := openDir(cur, c)
		if cur != rfd {
			_ = unix.Close(cur)
		}
		if err != nil {
			return -1, fmt.Errorf("%w: %q passes through a link or a file: %v", movepack.ErrArchiveRefused, m+"/"+dir, err)
		}
		cur = next
	}
	if cur == rfd {
		return unix.Dup(rfd)
	}
	return cur, nil
}

func openDir(dirfd int, name string) (int, error) {
	return unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// makeLinks creates the held symlinks, after every other entry and the manifest.
func (x *extractor) makeLinks() error {
	for _, l := range x.links {
		dir, name := "", l.rel
		if i := strings.LastIndexByte(l.rel, '/'); i >= 0 {
			dir, name = l.rel[:i], l.rel[i+1:]
		}
		pfd, err := x.parent(l.mount, dir)
		if err != nil {
			return err
		}
		if err := unix.Symlinkat(l.link, pfd, name); err != nil {
			if errors.Is(err, unix.EEXIST) {
				return fmt.Errorf("%w: %q already exists", movepack.ErrArchiveRefused, l.mount+"/"+l.rel)
			}
			return fmt.Errorf("movefs: import: link %q: %w", l.mount+"/"+l.rel, err)
		}
	}
	return nil
}

// setModes gives each directory its own mode, deepest first, so a directory without write access is changed
// only after everything inside it is written. A set-group-ID bit the directory already has stays: the volume's
// group ownership set it, and new files below must keep that group.
func (x *extractor) setModes() error {
	for _, hm := range slices.Backward(x.modes) {
		fd, err := x.walkTo(hm.mount, hm.rel)
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err = unix.Fstat(fd, &st); err == nil {
			err = unix.Fchmod(fd, hm.mode|st.Mode&unix.S_ISGID)
		}
		_ = unix.Close(fd)
		if err != nil {
			return fmt.Errorf("movefs: import: set the mode of %q: %w", hm.mount+"/"+hm.rel, err)
		}
	}
	return nil
}
