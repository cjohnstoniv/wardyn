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
	"syscall"
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

// removeRecordedFile removes one recorded file. It never follows a link the agent's user could have made: a
// folder on the path that is a link not owned by root refuses the removal, so a link planted in the agent's
// home cannot point the guest's root at another file. The file itself, if a link, is removed as the link.
func removeRecordedFile(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.Contains(path, "..") {
		return errors.New("not a clean absolute path")
	}
	p := "/"
	for _, c := range append([]string{""}, strings.Split(strings.TrimPrefix(filepath.Dir(path), "/"), "/")...) {
		p = filepath.Join(p, c)
		fi, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); fi.Mode()&os.ModeSymlink != 0 && (!ok || st.Uid != 0) {
			return fmt.Errorf("%s is a link the guest's root does not own", p)
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
