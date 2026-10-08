// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
)

// ManagedFile is one operator-authored file placed inside the sandbox that
// the AGENT CANNOT MODIFY. SECURITY: a driver delivers it root-owned, in a
// directory the agent can neither write nor replace, before the agent's main
// process can run — never materialised from inside the image (agent runs as
// uid 1000) or via a post-start root exec (races the main process). On
// Kubernetes it travels in the per-run Secret like SecretEnv.
//
// AgentOwned is the one other thing this type carries: a SECRET delivered to
// the agent as a file, which is the opposite contract — the agent must be able
// to read it and nobody else should — and shares only the delivery (same
// window, same per-run Secret).
type ManagedFile struct {
	// Path is the absolute in-sandbox path, already cleaned. See
	// ValidateManagedFiles for the shape both substrates can honour.
	Path string
	// Mode is the file's permission bits. Zero means DefaultManagedFileMode.
	// Group- and other-writable modes are refused: the file would be writable
	// by the agent's own supplementary groups, which defeats the field.
	Mode fs.FileMode
	// AgentOwned marks a delivered secret: readable by the agent's user and, as
	// far as the substrate can say it, by nobody else. Only under
	// ComponentSecretDir, only at ComponentSecretFileMode (ValidateManagedFiles).
	// Its owner can chmod and rewrite it, so it is NOT a ceiling — which is why
	// it is refused in ManagedFileDir. What each substrate delivers is stated on
	// its driver (docker: managedFilesTar; Kubernetes: managedFileVolumes).
	AgentOwned bool
	// Content is the exact file body. Delivered byte for byte; no trailing
	// newline is added.
	Content []byte
}

// DefaultManagedFileMode is the mode a ManagedFile with Mode == 0 is delivered
// with: readable by the agent, writable only by root.
const DefaultManagedFileMode fs.FileMode = 0o644

// ManagedFileDir is the one directory a managed file may be delivered into —
// where Claude Code reads its managed settings on Linux, the only consumer.
// It is an allowlist, not a path-shape rule, because the ceiling depends on
// where the file is: /etc is root-owned and unwritable by the agent, nothing
// loosens it after delivery, and no Wardyn image ships it. A second location
// joins only once checked against each of those.
const ManagedFileDir = "/etc/claude-code"

// ComponentSecretDir is the one directory a delivered secret (AgentOwned) lands
// in. Fixed, never authored: a file_secret grant names a FILE, not a path.
//
// It joined the allowlist against the three things ManagedFileDir's comment
// asks of a second location, read for what a secret needs rather than a
// ceiling: no mount can cover it (ValidateTarget admits nothing under /run, and
// neither driver mounts there), nothing either driver does after delivery
// loosens it, and no Wardyn image ships it — the docker driver refuses an image
// that does, and one whose /run is a link, so the value cannot be written
// anywhere else. Whether the agent could later replace the directory is not
// asked: the file is the agent's own secret, not a bound on it.
const ComponentSecretDir = "/run/wardyn/secrets"

// ComponentSecretFileMode is the only mode an AgentOwned file is delivered
// with: owner read. Docker owns the file by the agent's uid, so this is the
// whole of who can read it; Kubernetes cannot set a projected file's owner and
// says what it does instead (managedFileVolumes).
const ComponentSecretFileMode fs.FileMode = 0o400

// ManagedFilesMaxBytes caps the total content one spec may carry. The binding
// constraint is Kubernetes: every managed file rides the same per-run Secret
// (capped at 1 MiB) as the proxy config and SecretEnv, so this is refused on
// both substrates rather than diverging into docker-succeeds/k8s-413s.
const ManagedFilesMaxBytes = 256 << 10

// ValidateManagedFiles reports whether files can be delivered on EVERY
// substrate. Drivers call it before they create anything, so an impossible
// request is refused rather than half-applied. On Kubernetes, each directory
// becomes the mount point of a read-only Secret volume, not a subPath mount
// (the apiserver forbids that on the agent's ephemeral container).
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
		case path.Dir(f.Path) != ManagedFileDir && path.Dir(f.Path) != ComponentSecretDir:
			return fmt.Errorf("managed file %q: a managed file must sit directly in %s; anywhere else the agent could rename its directory aside, a mount could hide it, or delivery could write into a directory it did not create", f.Path, ManagedFileDir)
		case f.AgentOwned != (path.Dir(f.Path) == ComponentSecretDir):
			return fmt.Errorf("managed file %q: an agent-owned file is a delivered secret and sits in %s, and nothing else does; in %s it would be a ceiling its own subject can rewrite", f.Path, ComponentSecretDir, ManagedFileDir)
		case f.AgentOwned && f.FileMode() != ComponentSecretFileMode:
			return fmt.Errorf("managed file %q: an agent-owned file is delivered at mode %04o and no other, so only its owner can read it", f.Path, ComponentSecretFileMode.Perm())
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
