// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// ManagedFile is one operator-authored file placed inside the sandbox that the
// AGENT CANNOT MODIFY. That immutability is the whole point: a ceiling the
// agent can rewrite is not a ceiling, so a driver delivers this file owned by
// root inside a directory the agent can neither write nor replace, and it does
// so BEFORE the agent's main process can run.
//
// Two ways of producing such a file are deliberately NOT how this is delivered,
// because both look identical in a test and fail in production:
//
//   - Materialising it from inside the image. Every Wardyn agent image runs as
//     uid 1000 (image contract §3), so a file it writes is a file it can rewrite.
//   - A one-shot root exec after the container starts. That races the main
//     process, and the agent can observe — or act in — the window before the
//     file exists.
//
// Content is deliberately NOT serialised anywhere: a managed file may carry
// operator policy the agent is not meant to be able to tamper with, and on the
// Kubernetes substrate it travels in the per-run Secret for the same reason
// SecretEnv does.
type ManagedFile struct {
	// Path is the absolute in-sandbox path, already cleaned. See
	// ValidateManagedFiles for the shape both substrates can honour.
	Path string
	// Mode is the file's permission bits. Zero means DefaultManagedFileMode.
	// Group- and other-writable modes are refused: the file would be writable
	// by the agent's own supplementary groups, which defeats the field.
	Mode fs.FileMode
	// Content is the exact file body. Delivered byte for byte; no trailing
	// newline is added.
	Content []byte
}

// DefaultManagedFileMode is the mode a ManagedFile with Mode == 0 is delivered
// with: readable by the agent, writable only by root.
const DefaultManagedFileMode fs.FileMode = 0o644

// ManagedFilesMaxBytes caps the total content one spec may carry. The binding
// constraint is the Kubernetes substrate: every managed file rides the same
// per-run Secret as the proxy config and each SecretEnv value, and a Secret is
// capped at 1 MiB. Refusing here — in the contract, on both substrates — turns
// what would otherwise be a docker-succeeds/k8s-413s divergence into one
// refusal with the same words everywhere.
const ManagedFilesMaxBytes = 256 << 10

// ValidateManagedFiles reports whether files can be delivered on EVERY
// substrate. Drivers call it before they create anything, so an impossible
// request is refused rather than half-applied.
//
// The path shape is the Kubernetes substrate's constraint made explicit rather
// than left to be discovered. There, a managed file's PARENT DIRECTORY is the
// mount point of a read-only Secret volume — it cannot be a subPath mount,
// because the apiserver forbids subPath on the ephemeral container the agent
// actually runs in (see internal/runner/k8s/exec.go). Mounting over a
// top-level directory would therefore hide the image's own /etc (or /usr, or
// /bin) and the sandbox would not come up at all, so a managed path must be at
// least two directories deep.
func ValidateManagedFiles(files []ManagedFile) error {
	total := 0
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		switch {
		case f.Path == "":
			return fmt.Errorf("managed file: empty path")
		case !path.IsAbs(f.Path):
			return fmt.Errorf("managed file %q: path must be absolute", f.Path)
		case path.Clean(f.Path) != f.Path:
			return fmt.Errorf("managed file %q: path must be clean (no %q, %q or trailing separator)", f.Path, "..", "//")
		case strings.Count(f.Path, "/") < 3:
			return fmt.Errorf("managed file %q: path must be at least two directories deep (e.g. /etc/wardyn/file); its parent directory becomes a read-only mount point on the Kubernetes substrate, and mounting over a top-level directory would hide the image's own contents there", f.Path)
		case seen[f.Path]:
			return fmt.Errorf("managed file %q: duplicate path", f.Path)
		}
		if m := f.FileMode(); m&^fs.ModePerm != 0 {
			return fmt.Errorf("managed file %q: mode %v sets bits outside the permission bits", f.Path, m)
		} else if m&0o022 != 0 {
			return fmt.Errorf("managed file %q: mode %04o is group- or other-writable; a managed file the agent can write is not a ceiling", f.Path, m.Perm())
		}
		seen[f.Path] = true
		total += len(f.Content)
	}
	if total > ManagedFilesMaxBytes {
		return fmt.Errorf("managed files: %d bytes of content exceeds the %d-byte ceiling both substrates hold to", total, ManagedFilesMaxBytes)
	}
	return nil
}

// FileMode is Mode with the zero value resolved to DefaultManagedFileMode.
// Drivers use THIS, never the field, so "unset" means the same thing on both.
func (f ManagedFile) FileMode() fs.FileMode {
	if f.Mode == 0 {
		return DefaultManagedFileMode
	}
	return f.Mode
}

// ManagedFileDirs returns every distinct parent directory across files, sorted,
// so a driver that materialises directories (docker's archive) or mount points
// (the k8s Secret volumes) walks them deterministically.
func ManagedFileDirs(files []ManagedFile) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, f := range files {
		d := path.Dir(f.Path)
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	sort.Strings(dirs)
	return dirs
}
