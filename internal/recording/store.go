// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package recording provides storage and HTTP serving of asciicast session
// recordings produced by wardyn-rec. The Store interface is intentionally
// minimal so the fs-backed implementation can later be replaced by object
// storage without touching callers.
//
// Security constraints:
//   - All path construction goes through safeRunPath, which rejects any runID
//     containing path separators or dot-sequences (path-traversal prevention).
//   - Reads use os.OpenInRoot because shared-mount writers can create symlinks;
//     lexical validation alone cannot keep reads inside the recording directory.
//   - OpenCast returns (nil, ErrNotFound) for absent recordings so callers can
//     distinguish "never recorded" from storage errors.
package recording

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotFound is returned by OpenCast when no recording exists for the run.
var ErrNotFound = errors.New("recording: not found")

// Store is the recording persistence contract.
//
// SaveCast persists the asciicast bytestream from r under runID, replacing
// any prior recording; safe for concurrent saves of different runIDs.
//
// SaveCastNamed persists under a composite key "<runID>~<suffix>" (e.g. an
// interactive attach session id), so it does NOT clobber the batch run's
// cast (keyed by bare runID). The same path guardrails apply to both runID
// and suffix. Passing an empty suffix is equivalent to SaveCast.
//
// OpenCast returns a ReadCloser for the asciicast (caller closes it);
// ErrNotFound when no recording exists. The key is either a bare runID or a
// "<runID>~<suffix>" composite.
//
// StatAndTail answers "does this key have a recording, how big is it, and
// what are its last tailBytes" WITHOUT returning the whole payload.
// tailBytes is clamped to size when the cast is smaller. Returns ErrNotFound
// on the same terms as OpenCast.
type Store interface {
	SaveCast(ctx context.Context, runID string, r io.Reader) error
	SaveCastNamed(ctx context.Context, runID, suffix string, r io.Reader) error
	OpenCast(ctx context.Context, key string) (io.ReadCloser, error)
	StatAndTail(ctx context.Context, key string, tailBytes int64) (size int64, tail []byte, err error)
}

// castSep separates the run id from a session suffix in a composite cast key.
// Chosen as '~' because it is filesystem-safe and is not a path separator, and
// is rejected by safeRunPath's traversal checks like any other key character.
const castSep = "~"

// CastKey builds the composite cast key for a run + optional session suffix. An
// empty suffix yields the bare runID (the batch-run cast key).
func CastKey(runID, suffix string) string {
	if suffix == "" {
		return runID
	}
	return runID + castSep + suffix
}

// validSuffix rejects a session suffix that could misaddress a cast: one
// containing castSep would let a composite key collide with a DIFFERENT
// run/suffix pair. Shared by every Store implementation's SaveCastNamed so a
// suffix is accepted or rejected identically no matter which backend
// WARDYN_RECORDING_STORE selects.
func validSuffix(suffix string) error {
	if strings.ContainsAny(suffix, "/\\\x00"+castSep) || strings.Contains(suffix, "..") {
		return errors.New("recording: invalid session suffix")
	}
	return nil
}

// FSStore is a filesystem-backed Store. Each recording is stored as
// <root>/<runID>.cast. The root directory is created on first use.
type FSStore struct {
	root string
}

// NewFSStore returns an FSStore that persists casts under root. The directory
// is created with mode 0o750 if it does not exist.
func NewFSStore(root string) (*FSStore, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}
	return &FSStore{root: root}, nil
}

// SaveCastNamed writes the asciicast stream to <root>/<runID>~<suffix>.cast
// atomically. An empty suffix is equivalent to SaveCast (bare runID key). Both
// the runID and the composite key are checked by safeRunPath (fails closed on
// any path-traversal attempt in either component).
func (s *FSStore) SaveCastNamed(ctx context.Context, runID, suffix string, r io.Reader) error {
	// Reject a suffix carrying separators/traversal up front, before safeRunPath re-checks.
	if err := validSuffix(suffix); err != nil {
		return err
	}
	return s.SaveCast(ctx, CastKey(runID, suffix), r)
}

// SaveCast writes the asciicast stream to <root>/<runID>.cast atomically (write
// to a temp file then rename). Fails closed on any path-traversal attempt.
func (s *FSStore) SaveCast(_ context.Context, runID string, r io.Reader) error {
	dst, err := safeRunPath(s.root, runID)
	if err != nil {
		return err
	}

	// Write to a sibling temp file then rename for atomicity.
	tmp, err := os.CreateTemp(s.root, ".tmp-cast-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		// Without this unlink the store leaks its own .tmp-cast-* file.
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// Sweep unlinks every cast (and every orphaned atomic-write temp file)
// directly under root whose mtime is older than olderThan, returning how
// many files it removed. Deliberately NOT on the Store interface: retention
// is an fs-storage concern, so callers type-assert for it.
//
// Age is measured on ModTime, not birth time: the recordings directory is
// also mounted into agent containers for wardyn-rec's -out-dir fallback, so
// a cast may still be being appended to, and mtime advances on every write —
// do not "improve" this to birth time.
func (s *FSStore) Sweep(olderThan time.Duration) (int, error) {
	ents, err := os.ReadDir(s.root)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	removed := 0
	var errs []error
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".cast") && !strings.HasPrefix(name, ".tmp-cast-")) {
			continue
		}
		// A stat error means the entry vanished under us; nothing to remove.
		info, ierr := e.Info()
		if ierr != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if rerr := os.Remove(filepath.Join(s.root, name)); rerr != nil {
			errs = append(errs, rerr)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// OpenCast opens <root>/<runID>.cast for reading. Returns ErrNotFound when the
// file does not exist.
func (s *FSStore) OpenCast(_ context.Context, runID string) (io.ReadCloser, error) {
	path, err := safeRunPath(s.root, runID)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenInRoot(s.root, filepath.Base(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// StatAndTail reports the cast's size and reads its last tailBytes without
// opening (or copying) the rest of the file. tailBytes is clamped down to size
// when the cast is smaller.
func (s *FSStore) StatAndTail(_ context.Context, key string, tailBytes int64) (int64, []byte, error) {
	path, err := safeRunPath(s.root, key)
	if err != nil {
		return 0, nil, err
	}
	f, err := os.OpenInRoot(s.root, filepath.Base(path))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil, ErrNotFound
	}
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, nil, err
	}
	size := info.Size()
	start := size - tailBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return size, nil, err
	}
	// LimitReader, not a bare ReadAll: the file may still be written to
	// elsewhere between the Stat above and this read.
	tail, err := io.ReadAll(io.LimitReader(f, size-start))
	if err != nil {
		return size, nil, err
	}
	return size, tail, nil
}

// validKey rejects a cast key (a bare runID or a "<runID>~<suffix>" composite)
// that no Store should accept. Shared by EVERY implementation: a key one
// backend stores and another rejects means switching WARDYN_RECORDING_STORE
// silently changes which recordings exist.
func validKey(key string) error {
	if key == "" {
		return errors.New("recording: empty run id")
	}
	if strings.ContainsAny(key, "/\\\x00") {
		return errors.New("recording: invalid run id (path separator)")
	}
	if key == ".." || strings.HasPrefix(key, "../") || strings.HasSuffix(key, "/..") || strings.Contains(key, "/../") {
		return errors.New("recording: invalid run id (dot-dot)")
	}
	return nil
}

// safeRunPath builds the .cast file path for runID inside root. It rejects any
// runID that contains path separators, null bytes, or dot-dot sequences, which
// would allow directory traversal outside root.
func safeRunPath(root, runID string) (string, error) {
	if err := validKey(runID); err != nil {
		return "", err
	}
	// Extra guard: filepath.Clean must not escape root.
	joined := filepath.Join(root, runID+".cast")
	cleanRoot := filepath.Clean(root)
	if !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) &&
		joined != cleanRoot {
		return "", errors.New("recording: path traversal rejected")
	}
	return joined, nil
}
