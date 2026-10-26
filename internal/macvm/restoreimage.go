// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// restoreImage returns a validated restore image in the cache: reused if
// already complete and of known origin, otherwise downloaded (resuming a partial
// file). An image that fails validation is deleted and downloaded again, once.
// With an operator-supplied IPSW it is that file's cache copy instead (givenImage).
func (p *prep) restoreImage(ctx context.Context) (RestoreImage, string, string, error) {
	if p.IPSW != "" {
		return p.givenImage(ctx)
	}
	u, err := p.Engine.LatestRestoreImageURL(ctx)
	if err != nil {
		return RestoreImage{}, "", "", fmt.Errorf("macvm: latest restore image: %w", err)
	}
	name, err := ipswName(u)
	if err != nil {
		return RestoreImage{}, "", "", err
	}
	final := filepath.Join(p.cache, name)
	for attempt := 0; ; attempt++ {
		var sha string
		if _, err := os.Stat(final); err == nil {
			// A file in the cache is used only if a previous prepare's download recorded it. Anything else
			// may have been placed there by hand or damaged, and may be the only copy the operator has, so it
			// is refused, never deleted.
			var mismatch bool
			if sha, mismatch, err = knownOrigin(final); err != nil {
				return RestoreImage{}, "", "", err
			}
			if sha == "" {
				return RestoreImage{}, "", "", &UnrecordedImageError{Path: final, Mismatch: mismatch}
			}
			// A crash after the rename leaves the marker of the download that made it.
			if _, err := os.Lstat(final + ".part"); err != nil {
				_ = os.Remove(final + ".part" + partMarkerSuffix)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			// Only a file shown absent is downloaded over: one that cannot be inspected is kept.
			return RestoreImage{}, "", "", fmt.Errorf("macvm: inspect cached restore image %s: %w", final, err)
		}
		if sha == "" {
			if sha, err = p.download(ctx, u, final); err != nil {
				return RestoreImage{}, "", "", err
			}
		}
		img, err := p.validate(ctx, final)
		if err == nil {
			return img, sha, final, nil
		}
		if ctx.Err() != nil {
			return RestoreImage{}, "", "", err
		}
		removeImage(final)
		if attempt >= 1 {
			return RestoreImage{}, "", "", fmt.Errorf("macvm: the restore image failed validation twice: %w", err)
		}
		p.Log.Warn("macvm: restore image failed validation; downloading again", "err", err.Error())
	}
}

// validate loads the cached image into the engine, which checks it against what this Mac supports.
func (p *prep) validate(ctx context.Context, final string) (RestoreImage, error) {
	if err := p.waitAC(ctx, PhaseValidate); err != nil {
		return RestoreImage{}, err
	}
	p.emit(PrepareEvent{Phase: PhaseValidate})
	img, err := p.Engine.LoadRestoreImage(final)
	if err != nil {
		return RestoreImage{}, err
	}
	// The first validate event is written before the load, so it cannot say what the image is.
	var size int64
	if fi, serr := os.Stat(final); serr == nil {
		size = fi.Size()
	}
	p.emit(PrepareEvent{Phase: PhaseValidate, Total: size, Version: img.OSVersion})
	return img, nil
}

// recordPath is the provenance record of the cached restore image at image: the SHA-256 of the bytes
// the download left, written once it completes.
func recordPath(image string) string { return image + ".sha256" }

// removeImage deletes a cached restore image, its provenance record and any copy of it in progress.
func removeImage(image string) {
	_ = os.Remove(image)
	_ = os.Remove(recordPath(image))
	_ = os.Remove(image + copySuffix)
}

// copySuffix names the file an --ipsw copy is made in before it is checked.
const copySuffix = ".copy"

// knownOrigin is the SHA-256 of the cached restore image at image when its record says the same hash. It
// is "" for a file with no record (mismatch false) or whose hash is not its record's (mismatch true).
func knownOrigin(image string) (sha string, mismatch bool, err error) {
	sha, err = fileSHA256(image)
	if err != nil {
		return "", false, err
	}
	rec, err := os.ReadFile(recordPath(image))
	if err != nil {
		return "", false, nil
	}
	if strings.TrimSpace(string(rec)) != sha {
		return "", true, nil
	}
	return sha, false, nil
}

// givenImage is restoreImage for an operator-supplied file (PrepareOptions.IPSW). It copies the file into
// the cache (never a link, never a move: the operator's file stays as it is), hashes the copy, and keeps
// the copy only when the hash is the one given. A mismatch is final, with no download to fall back to.
// The copy is loaded and installed from, and the success cleanup deletes it like a downloaded one.
func (p *prep) givenImage(ctx context.Context) (RestoreImage, string, string, error) {
	src := p.IPSW
	name := filepath.Base(src)
	if !strings.HasSuffix(name, ".ipsw") || strings.HasPrefix(name, ".") {
		return RestoreImage{}, "", "", fmt.Errorf("macvm: --ipsw %q is not an .ipsw file", src)
	}
	if fi, err := os.Stat(src); err != nil {
		return RestoreImage{}, "", "", fmt.Errorf("macvm: --ipsw: %w", err)
	} else if !fi.Mode().IsRegular() {
		return RestoreImage{}, "", "", fmt.Errorf("macvm: --ipsw %q is not a regular file", src)
	}
	// The cleanup after a build deletes what is in the cache, so a file that is, or links to, one there is refused.
	inCache := false
	if a, err := os.Stat(filepath.Dir(src)); err == nil {
		if b, err := os.Stat(p.cache); err == nil && os.SameFile(a, b) {
			inCache = true
		}
	}
	if a, err := os.Stat(src); err == nil {
		if b, err := os.Stat(filepath.Join(p.cache, name)); err == nil && os.SameFile(a, b) {
			inCache = true
		}
	}
	if inCache {
		return RestoreImage{}, "", "", fmt.Errorf("macvm: --ipsw %q is, or links to, a file in the download cache, which prepare empties; give a file outside it", src)
	}
	// The copy replaces a cached file of the same name only when a Wardyn download recorded that file: the
	// given file's hash says nothing about another one, which may be the operator's only copy of it.
	// Only a file shown absent may be skipped: one that cannot be inspected is kept, not replaced.
	final := filepath.Join(p.cache, name)
	if _, err := os.Stat(final); err == nil {
		known, mismatch, err := knownOrigin(final)
		if err != nil {
			return RestoreImage{}, "", "", err
		}
		if known == "" {
			return RestoreImage{}, "", "", &UnrecordedImageError{Path: final, Mismatch: mismatch}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return RestoreImage{}, "", "", fmt.Errorf("macvm: inspect cached restore image %s: %w", final, err)
	}
	if err := p.waitAC(ctx, PhaseValidate); err != nil {
		return RestoreImage{}, "", "", err
	}
	p.emit(PrepareEvent{Phase: PhaseValidate, Detail: "checking the supplied restore image"})
	tmp := final + copySuffix
	_ = os.Remove(tmp) // a stray one, or a link planted there: the copy is made in a file of its own
	if err := copyFile(ctx, src, tmp); err != nil {
		_ = os.Remove(tmp)
		return RestoreImage{}, "", "", fmt.Errorf("macvm: --ipsw: copy into the cache: %w", err)
	}
	sha, err := fileSHA256(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return RestoreImage{}, "", "", err
	}
	if sha != p.IPSWSHA256 {
		_ = os.Remove(tmp)
		return RestoreImage{}, "", "", &IPSWMismatchError{Path: src, Want: p.IPSWSHA256, Got: sha}
	}
	_ = os.Remove(final)
	_ = os.Remove(recordPath(final))
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return RestoreImage{}, "", "", err
	}
	if err := os.WriteFile(recordPath(final), []byte(sha+"\n"), 0o600); err != nil {
		removeImage(final)
		return RestoreImage{}, "", "", err
	}
	img, err := p.validate(ctx, final)
	if err != nil {
		removeImage(final) // the cache's copy; src stays
		return RestoreImage{}, "", "", fmt.Errorf("macvm: --ipsw %q failed validation: %w", src, err)
	}
	return img, sha, final, nil
}

// copyFile writes src to dst, which must not exist, stopping when ctx ends.
func copyFile(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	for ctx.Err() == nil {
		n, rerr := in.Read(buf)
		if _, werr := out.Write(buf[:n]); werr != nil {
			_ = out.Close()
			return werr
		}
		if rerr == io.EOF {
			if err := out.Sync(); err != nil {
				_ = out.Close()
				return err
			}
			return out.Close()
		}
		if rerr != nil {
			_ = out.Close()
			return rerr
		}
	}
	_ = out.Close()
	return ctx.Err()
}

// partMarkerSuffix names the marker beside a partial download. Wardyn creates the partial file before
// its marker and, when a download fails, removes the marker first, so a crash between the two leaves an
// unmarked partial file, which is removed, never a marker a partial file placed by hand could pair with.
// The one marker left alone, after the rename to the final name, the next load removes. A partial file
// with a marker is one this download made and may resume.
const partMarkerSuffix = ".pinned"

// startPart makes part ready for a download: an existing one with its marker is kept to be resumed; one
// without (placed by hand, or left by something else) is removed, so the download starts at offset 0 and
// no byte of the image is one Wardyn did not fetch.
func (p *prep) startPart(part string) error {
	marker := part + partMarkerSuffix
	if _, err := os.Lstat(part); err == nil {
		if _, merr := os.Lstat(marker); merr == nil {
			return nil
		}
		p.Log.Warn("macvm: partial restore image has no marker of a Wardyn download; removing it and downloading from the start", "part", part)
		if err := os.Remove(part); err != nil {
			return err
		}
	}
	_ = os.Remove(marker)
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	m, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	return m.Close()
}
