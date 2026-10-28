// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MacShares mounts a run's folder shares in a macOS guest: Server.Mount there. It is plain file
// and command work, so it is tested off a Mac with a fake runner.
type MacShares struct {
	Run CmdRunner
	// Home is the agent's home: every target is strictly inside it.
	Home string
	// UID and GID are the agent user's, who owns each folder made for a target.
	UID, GID int
}

// Mount mounts share s at its target: the folders up to it are made first, as the agent's, and
// mount(8) gets nosuid, nodev and noowners, and rdonly when asked. noowners shows every file on the
// share as the accessing user's, since the host's owner ids mean nothing in the guest.
func (m MacShares) Mount(ctx context.Context, s Mount) error {
	if n, ok := strings.CutPrefix(s.Tag, "wardyn"); !ok || n == "" || strings.Trim(n, "0123456789") != "" {
		return fmt.Errorf("share tag %q is not a Wardyn share", s.Tag)
	}
	if err := checkMacShareTarget(m.Home, s.Target); err != nil {
		return err
	}
	if err := m.makeTarget(s.Target); err != nil {
		return err
	}
	_, err := m.Run.Run(ctx, "/sbin/mount", macShareMountArgs(s)...)
	return err
}

// checkMacShareTarget refuses a target that is not a clean path strictly inside home, or that is
// home's .wardyn folder or below it: the run's CA lands there (MacCABundlePath). The volume is case
// insensitive, so .WARDYN is the same folder.
func checkMacShareTarget(home, target string) error {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return fmt.Errorf("share target %q is not a clean absolute path", target)
	}
	rel, err := filepath.Rel(home, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("share target %q is not inside %s", target, home)
	}
	if first, _, _ := strings.Cut(rel, "/"); strings.EqualFold(first, ".wardyn") {
		return fmt.Errorf("share target %q is in the run's Wardyn folder", target)
	}
	return nil
}

// macShareMountArgs is mount(8)'s arguments for share s.
func macShareMountArgs(s Mount) []string {
	opts := "nosuid,nodev,noowners"
	if s.ReadOnly {
		opts += ",rdonly"
	}
	return []string{"-t", "virtiofs", "-o", opts, s.Tag, s.Target}
}

// makeTarget makes each missing folder from home down to target, owned by the agent. An existing
// one that is a link or not a folder is refused, so the mount lands only where the path says.
func (m MacShares) makeTarget(target string) error {
	rel, _ := filepath.Rel(m.Home, target)
	p := m.Home
	for _, name := range strings.Split(rel, "/") {
		p = filepath.Join(p, name)
		fi, err := os.Lstat(p)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(p, 0o755); err != nil {
				return err
			}
			if err := os.Lchown(p, m.UID, m.GID); err != nil {
				return err
			}
		case err != nil:
			return err
		case !fi.IsDir():
			return fmt.Errorf("share target %q: %s is not a folder", target, p)
		}
	}
	return nil
}
