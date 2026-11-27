// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// ceilingDirs are the managed-file directories whose file bounds the agent: Claude Code's, Codex's and
// Chromium's (runner.ManagedFileDir, runner.CodexManagedFileDir and runner.ChromiumPolicyDir; a test pins
// them). The run's CA directory, a delivered secret's directory and every other path are not checked here.
var ceilingDirs = []string{"/etc/claude-code", "/etc/codex", "/etc/chromium/policies/managed"}

// managedLstat is how ceilingParents reads a directory; tests replace it.
var managedLstat = os.Lstat

// ceilingParents refuses a managed file in a ceiling directory whose chain the agent could re-point
// (rootOwnedParents); a file anywhere else is not its business.
func ceilingParents(dir string) error {
	if !slices.Contains(ceilingDirs, dir) {
		return nil
	}
	return rootOwnedParents(dir, managedLstat)
}

// rootOwnedParents refuses a directory chain the agent could re-point: every directory from / down to
// dir that already exists must be a real directory (not a link), owned by root and writable by nobody
// else. The first one missing ends the walk; writeFile creates it and the rest root-owned. This is the
// guest's half of the Docker driver's image check: a guest image is the operator's, and nothing but the
// guest agent runs before the agent does.
func rootOwnedParents(dir string, lstat func(string) (fs.FileInfo, error)) error {
	p := "/"
	for _, part := range append([]string{""}, strings.Split(strings.TrimPrefix(dir, "/"), "/")...) {
		p = filepath.Join(p, part)
		fi, err := lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("managed file directory %s: %w", p, err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() || !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("managed file directory %s must be a directory owned by root and writable by nobody else", p)
		}
	}
	return nil
}
