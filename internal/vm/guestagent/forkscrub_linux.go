// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// initFilesRecord, in OutDir (root's, on the guest's disk), names the files the last Init on this disk wrote.
// A fork's disk is a copy of another run's, so its first Init removes them before writing its own: the managed
// files of the run it was forked from, the git helper's secret among them, never reach the fork's agent.
const initFilesRecord = "wardyn-init-files.json"

type initFiles struct {
	Files []string `json:"files"`
}

// scrubLastInit removes the files the last Init on this disk recorded, then records in's. The record is written
// before in's files are, so an Init that fails part-way still leaves every file it may have written on record.
// A fork (in.Fork) with no record is refused: its source's files would stay.
func (s *Server) scrubLastInit(in Init) error {
	if s.OutDir == "" {
		if in.Fork {
			return errors.New("this agent keeps no record of the files an earlier sandbox was given")
		}
		return nil
	}
	rec := filepath.Join(s.OutDir, initFilesRecord)
	b, err := os.ReadFile(rec)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if in.Fork {
			return errors.New("the forked disk has no record of the files its last sandbox was given")
		}
	case err != nil:
		return err
	default:
		var last initFiles
		if err := json.Unmarshal(b, &last); err != nil {
			return fmt.Errorf("the record of the last sandbox's files: %w", err)
		}
		for _, p := range last.Files {
			if err := removeRecordedFile(p); err != nil {
				return fmt.Errorf("remove %s: %w", p, err)
			}
		}
	}
	next := initFiles{Files: []string{}}
	for _, f := range in.Files {
		next.Files = append(next.Files, f.Path)
	}
	b, _ = json.Marshal(next)
	tmp := rec + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, rec)
}

// removeRecordedFile removes one recorded file. It never follows a link: it walks from / through directory
// descriptors, each folder opened relative to the last with O_NOFOLLOW, and removes the file relative to its
// folder's descriptor. A link the agent's user plants on the path, even after it is looked at, cannot point the
// guest's root at another file; a folder on the path that is a link refuses the removal. The file itself, if a
// link, is removed as the link. A folder or file already gone leaves nothing to remove.
func removeRecordedFile(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.Contains(path, "..") || path == "/" {
		return errors.New("not a clean absolute path")
	}
	const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	dir, err := unix.Open("/", dirFlags, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dir) }()
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, c := range parts[:len(parts)-1] {
		fd, err := unix.Openat(dir, c, dirFlags, 0)
		switch {
		case errors.Is(err, unix.ENOENT):
			return nil
		case errors.Is(err, unix.ELOOP) || (errors.Is(err, unix.ENOTDIR) && isLinkAt(dir, c)):
			return fmt.Errorf("/%s is a link, which the removal never follows", strings.Join(parts[:i+1], "/"))
		case err != nil:
			return fmt.Errorf("/%s: %w", strings.Join(parts[:i+1], "/"), err)
		}
		_ = unix.Close(dir)
		dir = fd
	}
	if err := unix.Unlinkat(dir, parts[len(parts)-1], 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// isLinkAt reports whether name, in the folder dir, is a link.
func isLinkAt(dir int, name string) bool {
	var st unix.Stat_t
	return unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK
}
