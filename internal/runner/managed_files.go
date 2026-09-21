// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
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

// ManagedFileDir is the one directory a managed file may be delivered into,
// and every managed file sits directly in it. It is where Claude Code reads its
// managed settings on Linux, the only consumer. It is an allowlist rather than
// a rule about path shape because each property that makes a managed file a
// ceiling depends on where the file is:
//
//   - its parent, /etc, is root-owned and not writable by the agent, and the
//     agent is not root, so it cannot rename this directory aside and put its
//     own in its place (a rename within one parent needs write on the parent
//     only). The Docker driver refuses an image that breaks either; Kubernetes
//     runs the agent as uid 1000 on a read-only mount point whatever the image;
//   - nothing covers or loosens /etc after delivery: mount targets are confined
//     to allowedTargetPrefixes, the sandbox's tmpfs is /tmp, and recording
//     setup chmods only its own directories;
//   - no Wardyn image ships it, so the Docker driver creates it rather than
//     delivering into a directory it did not make.
//
// A second location joins only once it has been checked against each of those.
const ManagedFileDir = "/etc/claude-code"

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
// Every path must sit directly in ManagedFileDir; the refusal for any other
// says why. On the Kubernetes substrate that directory becomes the mount point
// of a read-only Secret volume — not a subPath mount, which the apiserver
// forbids on the ephemeral container the agent actually runs in (see
// internal/runner/k8s/exec.go).
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
		case path.Dir(f.Path) != ManagedFileDir:
			return fmt.Errorf("managed file %q: a managed file must sit directly in %s; anywhere else the agent could rename its directory aside, a mount could hide it, or delivery could write into a directory it did not create", f.Path, ManagedFileDir)
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
