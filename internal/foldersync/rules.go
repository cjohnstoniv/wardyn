// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// The safety rules of `wardyn sync` (docs/design/0.9/PLAN.md §9.3). They run
// here, on the laptop: the gateway relays sftp bytes and cannot parse them.

const (
	syncRemoteRoot    = "/home/agent/"
	syncDefaultRemote = "/home/agent/work"
)

// syncDenied reports whether rel (slash-separated, relative to the synced
// root) is one of the paths that run code on the laptop when an editor, a
// shell or git next touches the tree: .git (hooks included), direnv's files,
// and the editor task/launch/run-configuration files. Matched at any depth and
// case- and normalisation-insensitively (syncFold), so a folding filesystem
// cannot reach one by another spelling. Both directions use it.
func syncDenied(rel string) bool {
	segs := strings.Split(syncFold(rel), "/")
	for i, seg := range segs {
		switch seg {
		case ".git", ".envrc", ".direnv":
			return true
		case ".vscode":
			if i+1 < len(segs) && (segs[i+1] == "tasks.json" || segs[i+1] == "launch.json") {
				return true
			}
		case ".idea":
			if i+1 < len(segs) && (segs[i+1] == "runconfigurations" || segs[i+1] == "workspace.xml") {
				return true
			}
		}
	}
	return false
}

// syncFold is the key two names are compared by: NFC, then Unicode case
// folding. APFS and HFS+ treat names equal under it, so two paths with one key
// are one file on a laptop.
func syncFold(s string) string {
	return cases.Fold().String(norm.NFC.String(s))
}

// syncInvisible reports a name holding a default-ignorable code point (zero
// width joiners, variation selectors, soft hyphen and the like). HFS+ ignores
// them when comparing, so ".g\u200cit" is ".git" there.
func syncInvisible(name string) bool {
	return strings.ContainsFunc(name, func(r rune) bool {
		return unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Variation_Selector, r) ||
			unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r)
	})
}

// RemoteDir validates --remote-dir the way the gateway does (an absolute,
// cleaned path under /home/agent/, no ".." segment, no control character, no
// '%'), so a bad value fails here with a plain message instead of as a refusal
// the gateway only reports after the channel opens. Empty is the gateway's own
// default.
func RemoteDir(v string) (string, error) {
	if v == "" {
		return syncDefaultRemote, nil
	}
	if !path.IsAbs(v) || strings.Contains(v, "%") || !utf8.ValidString(v) || strings.ContainsFunc(v, unicode.IsControl) {
		return "", errors.New("--remote-dir must be an absolute path without control characters or '%'")
	}
	if slices.Contains(strings.Split(v, "/"), "..") {
		return "", errors.New("--remote-dir must not contain a '..' segment")
	}
	clean := path.Clean(v)
	if !strings.HasPrefix(clean, syncRemoteRoot) {
		return "", fmt.Errorf("--remote-dir must be under %s", syncRemoteRoot)
	}
	return clean, nil
}

// syncEntryName refuses a directory-entry name that is not one plain path
// segment. The sandbox chooses remote names; one carrying a separator, NUL or
// ".." would otherwise be joined onto the local root.
func syncEntryName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return fmt.Errorf("entry name %q", name)
	case strings.ContainsAny(name, "/\\\x00") || !utf8.ValidString(name):
		return fmt.Errorf("entry name %q is not a plain file name", name)
	case strings.ContainsFunc(name, unicode.IsControl):
		return fmt.Errorf("entry name %q holds a control character", name)
	case syncInvisible(name):
		return fmt.Errorf("entry name %q holds an invisible character", name)
	}
	return nil
}

// Shown is p as the report prints it: unchanged when every character is
// printable, quoted otherwise, so no name can write control codes to the terminal.
func Shown(p string) string {
	if utf8.ValidString(p) && !strings.ContainsFunc(p, func(r rune) bool { return !unicode.IsGraphic(r) }) {
		return p
	}
	return strconv.QuoteToGraphic(p)
}

// syncCollisions returns the paths that differ only by case or Unicode
// normalisation from another path in rels: such a filesystem would write both
// to one file, so neither is synced.
func syncCollisions(rels []string) map[string]bool {
	byFold := map[string][]string{}
	for _, r := range rels {
		k := syncFold(r)
		if !slices.Contains(byFold[k], r) {
			byFold[k] = append(byFold[k], r)
		}
	}
	out := map[string]bool{}
	for _, group := range byFold {
		if len(group) < 2 {
			continue
		}
		for _, r := range group {
			out[r] = true
		}
	}
	return out
}

// syncCheckLocal fails when rel could leave the root or when any component of
// it that already exists is a symlink. A pulled file is the sandbox's choice of
// content and name; this keeps it from being written through a link on the
// laptop. A missing component is fine (the caller creates it, as a real
// directory). The root itself confines every later operation to its tree.
func syncCheckLocal(root *os.Root, rel string) error {
	if rel == "" || path.IsAbs(rel) || path.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("path %q escapes the local directory", rel)
	}
	cur := ""
	for _, seg := range strings.Split(rel, "/") {
		if err := syncEntryName(seg); err != nil {
			return err
		}
		cur = path.Join(cur, seg)
		fi, err := root.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", rel)
		}
	}
	return nil
}

// Side is what one side looked like the last time the file was synced.
type Side struct {
	Size  int64 `json:"size"`
	MTime int64 `json:"mtime_ns"`
}

// State is the per-run record of the last sync, kept outside the synced
// tree. Without an entry for a path nothing is known about its past, so a file
// present on both sides is a conflict under --pull.
type State struct {
	Local  string              `json:"local"`
	Remote string              `json:"remote"`
	Files  map[string]FileSeen `json:"files"`
	// GitSent is what --git last sent; --pull-commits brings back what came after it.
	GitSent *GitSent `json:"git_sent,omitempty"`
}

type FileSeen struct {
	Local  Side `json:"local"`
	Remote Side `json:"remote"`
}

func StatePath(runID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve ~: %w", err)
	}
	return filepath.Join(home, ".local", "state", "wardyn", "sync", runID+".json"), nil
}

// LoadState reads the state for one run and one local/remote pairing; a
// missing file, or one for another pairing, is an empty state.
func LoadState(file, local, remote string) (*State, error) {
	st := &State{Local: local, Remote: remote, Files: map[string]FileSeen{}}
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	var prev State
	if err := json.Unmarshal(b, &prev); err != nil {
		return nil, fmt.Errorf("sync state %s is unreadable (delete it to start over): %w", file, err)
	}
	if prev.Local == local && prev.Remote == remote && prev.Files != nil {
		return &prev, nil
	}
	return st, nil
}

func (st *State) Save(file string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// StateOutside refuses a local root that contains the state file: the
// record must never ride along in the synced tree.
func StateOutside(root, stateFile string) error {
	rel, err := filepath.Rel(root, stateFile)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("the sync state file %s is inside %s; pick a narrower directory", stateFile, root)
	}
	return nil
}
