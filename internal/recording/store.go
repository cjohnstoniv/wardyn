// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package recording provides storage and HTTP serving of asciicast session
// recordings produced by wardyn-rec. Store is intentionally minimal so the
// fs-backed implementation can later be replaced by object storage without
// touching callers.
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

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// ErrNotFound is returned by OpenCast when no recording exists for the run.
var ErrNotFound = errors.New("recording: not found")

// ErrErased refuses access to a run whose recordings were durably erased.
var ErrErased = errors.New("recording: erased")

// Store is the recording persistence contract.
//
// SaveCast persists the asciicast bytestream from r under runID, replacing
// any prior recording; safe for concurrent saves of different runIDs.
//
// SaveCastNamed persists under a composite key "<runID>~<suffix>" (e.g. an
// interactive attach session id) so it does not clobber the batch run's cast
// (keyed by bare runID). Same path guardrails as runID; an empty suffix is
// equivalent to SaveCast.
//
// OpenCast returns a ReadCloser (caller closes it) or ErrNotFound. The key is
// either a bare runID or a "<runID>~<suffix>" composite.
//
// StatAndTail reports a key's size and its last tailBytes without returning
// the whole payload; tailBytes clamps to size when the cast is smaller.
// ErrNotFound on the same terms as OpenCast.
//
// Save, open and stat return ErrErased after a run is erased. A key belongs to
// the run before its first "~", including keys passed directly to SaveCast.
// Already-open readers may retain bytes; erasure does not revoke them.
type Store interface {
	SaveCast(ctx context.Context, runID string, r io.Reader) error
	SaveCastNamed(ctx context.Context, runID, suffix string, r io.Reader) error
	OpenCast(ctx context.Context, key string) (io.ReadCloser, error)
	StatAndTail(ctx context.Context, key string, tailBytes int64) (size int64, tail []byte, err error)
}

// castSep separates the run id from a session suffix. Chosen as '~': it is
// filesystem-safe, not a path separator, and rejected by safeRunPath's
// traversal checks like any other key character.
const castSep = "~"

// CastKey builds the composite cast key for a run + optional session suffix.
// An empty suffix yields the bare runID (the batch-run cast key).
func CastKey(runID, suffix string) string {
	if suffix == "" {
		return runID
	}
	return runID + castSep + suffix
}

// validSuffix rejects a suffix that could misaddress a cast: one containing
// castSep would let a composite key collide with a different run/suffix pair.
// Shared by every Store's SaveCastNamed so acceptance is identical no matter
// which backend WARDYN_RECORDING_STORE selects.
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
// atomically. An empty suffix is equivalent to SaveCast. Both runID and the
// composite key are checked by safeRunPath, failing closed on any
// path-traversal attempt in either component.
func (s *FSStore) SaveCastNamed(ctx context.Context, runID, suffix string, r io.Reader) error {
	if err := validSuffix(suffix); err != nil {
		return err
	}
	return s.SaveCast(ctx, CastKey(runID, suffix), r)
}

// SaveCast writes the asciicast stream to <root>/<runID>.cast atomically (write
// to a temp file then rename). Fails closed on any path-traversal attempt.
func (s *FSStore) SaveCast(ctx context.Context, key string, r io.Reader) error {
	return s.saveFile(ctx, key, ".cast", r, false)
}

// SaveRecordingFile preserves wardyn-rec's shared-volume .cast/.log filename
// and existing-file permissions/ownership while applying the same run fence as
// SaveCast. Its final local copy retains the fallback's in-place overwrite.
func (s *FSStore) SaveRecordingFile(ctx context.Context, name string, r io.Reader) error {
	ext := filepath.Ext(name)
	if ext != ".cast" && ext != ".log" {
		return errors.New("recording: expected a .cast or .log filename")
	}
	return s.saveFile(ctx, strings.TrimSuffix(name, ext), ext, r, true)
}

func (s *FSStore) saveFile(ctx context.Context, key, ext string, r io.Reader, inPlace bool) error {
	if _, err := safeRunPath(s.root, key); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return err
	}
	defer root.Close()
	tmpName := ".tmp-cast-" + uuid.NewString()
	tmp, err := root.OpenFile(tmpName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(tmpName)
	defer tmp.Close()
	if _, err := io.Copy(tmp, contextReader{ctx, r}); err != nil {
		return err
	}
	lock, err := lockFSRun(ctx, root, key)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := checkFSErased(root, key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if inPlace {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return copyRecordingFile(ctx, root, key+ext, tmp)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return root.Rename(tmpName, key+ext)
}

func copyRecordingFile(ctx context.Context, root *os.Root, name string, src *os.File) error {
	// Replacing the inode would bypass write access and change owner/mode/ACL.
	// Only the prepared private file is copied while holding the erasure lock.
	dst, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o666)
	if err != nil {
		return err
	}
	defer dst.Close()
	info, err := dst.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(err, errors.New("recording: not a regular file"))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := dst.Truncate(0); err != nil {
		return err
	}
	_, err = io.Copy(dst, contextReader{ctx, src})
	return errors.Join(err, dst.Close())
}

// Sweep unlinks every cast (and orphaned atomic-write temp file) directly
// under root whose mtime is older than olderThan, returning the count
// removed. Deliberately not on the Store interface: retention is an
// fs-storage concern, so callers type-assert for it.
//
// Age is measured on ModTime, not birth time: the recordings directory is
// also mounted into agent containers for wardyn-rec's -out-dir fallback, so a
// cast may still be appended to and mtime keeps advancing — do not "improve"
// this to birth time.
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
		if e.IsDir() || strings.HasSuffix(name, ".lock") || (!strings.HasSuffix(name, ".cast") && !strings.HasPrefix(name, ".tmp-cast-")) {
			continue
		}
		// A stat error means the entry vanished under us.
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

// RunDeleter durably fences a run before deleting all of its recordings.
// A successful erase prevents later saves, opens and stat calls across restart.
// A composite runID erases that exact key and its "~" descendants only.
type RunDeleter interface {
	DeleteRun(ctx context.Context, runID string) (int, error)
}

var _ RunDeleter = (*FSStore)(nil)

// DeleteRun installs a durable fence and removes the run's bare and suffixed
// casts, plus wardyn-rec's shared-volume .log fallback. Repeating it is safe.
func (s *FSStore) DeleteRun(ctx context.Context, key string) (int, error) {
	if _, err := safeRunPath(s.root, key); err != nil {
		return 0, err
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	lock, err := lockFSRun(ctx, root, key)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	if err := markFSErased(ctx, root, key); err != nil {
		return 0, err
	}
	dir, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	defer dir.Close()
	ents, err := dir.ReadDir(-1)
	if err != nil {
		return 0, err
	}
	removed := 0
	var errs []error
	for _, e := range ents {
		ext := filepath.Ext(e.Name())
		if e.IsDir() || (ext != ".cast" && ext != ".log") || !withinRun(strings.TrimSuffix(e.Name(), ext), key) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if err := root.Remove(e.Name()); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		removed++
	}
	return removed, errors.Join(append(errs, dir.Sync())...)
}

// OpenCast opens <root>/<runID>.cast for reading. Returns ErrNotFound when the
// file does not exist.
func (s *FSStore) OpenCast(ctx context.Context, key string) (io.ReadCloser, error) {
	f, lock, err := s.openCast(ctx, key)
	if lock != nil {
		_ = lock.Close()
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s *FSStore) openCast(ctx context.Context, key string) (*os.File, *os.File, error) {
	if _, err := safeRunPath(s.root, key); err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	lock, err := lockFSRun(ctx, root, key)
	if err != nil {
		return nil, nil, err
	}
	if err := checkFSErased(root, key); err != nil {
		return nil, lock, err
	}
	if err := ctx.Err(); err != nil {
		return nil, lock, err
	}
	// A shared-mount FIFO must not pin the run lock while erasure waits.
	f, err := root.OpenFile(key+".cast", os.O_RDONLY|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, lock, ErrNotFound
	}
	if err != nil {
		return nil, lock, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, lock, errors.Join(err, errors.New("recording: not a regular file"))
	}
	return f, lock, nil
}

// StatAndTail reports the cast's size and reads its last tailBytes without
// copying the rest of the file. tailBytes clamps down to size when the cast
// is smaller.
func (s *FSStore) StatAndTail(ctx context.Context, key string, tailBytes int64) (int64, []byte, error) {
	f, lock, err := s.openCast(ctx, key)
	if lock != nil {
		defer lock.Close()
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

// validKey rejects a cast key (bare runID or "<runID>~<suffix>" composite)
// that no Store should accept. Shared by every implementation: divergence
// here means switching WARDYN_RECORDING_STORE silently changes which
// recordings exist.
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

// safeRunPath builds the .cast file path for runID inside root, rejecting any
// runID with path separators, null bytes, or dot-dot sequences that would
// allow directory traversal outside root.
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

func castRun(key string) string {
	run, _, _ := strings.Cut(key, castSep)
	return run
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func withinRun(key, run string) bool { return key == run || strings.HasPrefix(key, run+castSep) }

// Composite erasures retain their original narrow scope. A descendant checks
// each ancestor while sharing the first-prefix lock with every erase of it.
func fenceKeys(key string) []string {
	keys := []string{key}
	for i, c := range key {
		if c == '~' {
			keys = append(keys, key[:i])
		}
	}
	return keys
}
