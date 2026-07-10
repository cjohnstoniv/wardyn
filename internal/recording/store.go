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
)

// ErrNotFound is returned by OpenCast when no recording exists for the run.
var ErrNotFound = errors.New("recording: not found")

// Store is the recording persistence contract.
//
// SaveCast persists the asciicast bytestream from r under runID, replacing any
// prior recording for that run. It must be safe for concurrent saves of
// different runIDs.
//
// SaveCastNamed persists the asciicast bytestream from r under a composite key
// "<runID>~<suffix>" (e.g. an interactive attach session id), so an interactive
// session recording does NOT clobber the batch run's cast (keyed by bare runID)
// and concurrent/sequential attaches each get their own cast. The same path
// guardrails (no traversal) apply to both runID and suffix. The composite key is
// what OpenCast surfaces; the recording HTTP handler can serve it by that key.
// Passing an empty suffix is equivalent to SaveCast.
//
// OpenCast returns a ReadCloser for the asciicast. The caller is responsible
// for closing it. Returns ErrNotFound when no recording exists. The key is
// either a bare runID (batch cast) or a "<runID>~<suffix>" composite.
type Store interface {
	SaveCast(ctx context.Context, runID string, r io.Reader) error
	SaveCastNamed(ctx context.Context, runID, suffix string, r io.Reader) error
	OpenCast(ctx context.Context, key string) (io.ReadCloser, error)
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
	// Reject a suffix that could carry path separators / traversal up front, so
	// the composite key cannot escape root even though safeRunPath re-checks.
	if strings.ContainsAny(suffix, "/\\\x00"+castSep) || strings.Contains(suffix, "..") {
		return errors.New("recording: invalid session suffix")
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
	return os.Rename(tmpName, dst)
}

// OpenCast opens <root>/<runID>.cast for reading. Returns ErrNotFound when the
// file does not exist.
func (s *FSStore) OpenCast(_ context.Context, runID string) (io.ReadCloser, error) {
	path, err := safeRunPath(s.root, runID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// safeRunPath builds the .cast file path for runID inside root. It rejects any
// runID that contains path separators, null bytes, or dot-dot sequences, which
// would allow directory traversal outside root.
func safeRunPath(root, runID string) (string, error) {
	if runID == "" {
		return "", errors.New("recording: empty run id")
	}
	if strings.ContainsAny(runID, "/\\\x00") {
		return "", errors.New("recording: invalid run id (path separator)")
	}
	if runID == ".." || strings.HasPrefix(runID, "../") || strings.HasSuffix(runID, "/..") || strings.Contains(runID, "/../") {
		return "", errors.New("recording: invalid run id (dot-dot)")
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
