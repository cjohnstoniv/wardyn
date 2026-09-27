// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// stagedConfigMode is the file mode StageProxyConfig leaves the destination
// at: owner-read-only. The destination lives on an in-memory emptyDir the
// k8s substrate's main proxy container mounts read-only and alone (T-28,
// issue #688) — no other principal shares that mount, so 0400 (not merely
// 0440/0400+group) is the tightest mode that still lets THIS container's own
// uid read it back.
const stagedConfigMode = 0o400

// StageProxyConfig is the k8s substrate's init-container step: it reads the
// proxy config JSON from src (a Secret-projected volume the init container
// alone mounts) and writes it to dst (a shared in-memory emptyDir the main
// proxy container also mounts, read-only) as an owner-only 0400 file, then
// the main container reads it back via -config instead of a secret-backed
// environment variable.
//
// Two things this buys over "the main container reads the Secret volume
// directly": (1) the destination's mode is exactly 0400 regardless of what a
// Secret volume's DefaultMode/Items[].Mode can express (a Secret volume mount
// is capped at readable-by-owning-uid-or-configured-group, never narrower
// than the projecting kubelet allows, and never independent of the source
// Secret's own permissions on that node), and (2) a single, always-run
// staging step is the same one CHECK proves for every substrate this binary
// ships to, rather than a per-substrate volume-projection contract nothing
// exercises directly.
//
// Fails closed: any read, parse, or write error leaves dst untouched (an
// invalid or missing config must stop the init container, not hand the main
// container a file it will only reject at ITS OWN boot) and returns a
// wrapped error the init container's exit code surfaces to `kubectl describe
// pod`.
//
// The write is atomic — a temp file in dst's own directory, chmodded BEFORE
// the rename, then renamed into place — so a concurrent reader (there should
// be none; the main container starts only after this init container
// succeeds) never observes a partially-written file, and a crash mid-write
// leaves no file at dst at all rather than a truncated one.
func StageProxyConfig(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("wardyn-proxy: stage config: read %s: %w", src, err)
	}
	// Validate before staging: an init container that hands the main
	// container garbage only pushes the failure one boot later, into a
	// process that has already dropped the privilege to read the Secret
	// volume this data came from.
	if _, err := proxy.LoadConfigBytes(data); err != nil {
		return fmt.Errorf("wardyn-proxy: stage config: invalid config JSON from %s: %w", src, err)
	}

	dstDir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dstDir, ".wardyn-proxy-config-*.tmp")
	if err != nil {
		return fmt.Errorf("wardyn-proxy: stage config: create temp file in %s: %w", dstDir, err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup: a no-op once the rename below succeeds, and the
	// only recourse if any step before it fails.
	defer os.Remove(tmpPath)

	if _, werr := tmp.Write(data); werr != nil {
		tmp.Close()
		return fmt.Errorf("wardyn-proxy: stage config: write %s: %w", tmpPath, werr)
	}
	if cerr := tmp.Close(); cerr != nil {
		return fmt.Errorf("wardyn-proxy: stage config: close %s: %w", tmpPath, cerr)
	}
	// Chmod BEFORE rename: renaming first would leave a window in which dst
	// exists at the temp file's (more permissive, umask-derived) mode.
	if cerr := os.Chmod(tmpPath, stagedConfigMode); cerr != nil {
		return fmt.Errorf("wardyn-proxy: stage config: chmod %s: %w", tmpPath, cerr)
	}
	if rerr := os.Rename(tmpPath, dst); rerr != nil {
		return fmt.Errorf("wardyn-proxy: stage config: rename %s to %s: %w", tmpPath, dst, rerr)
	}
	return nil
}
