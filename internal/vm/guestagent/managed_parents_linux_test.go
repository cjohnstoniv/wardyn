// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// parentInfo is a directory entry the fake lstat reports.
type parentInfo struct {
	mode fs.FileMode
	uid  uint32
}

func (p parentInfo) Name() string       { return "x" }
func (p parentInfo) Size() int64        { return 0 }
func (p parentInfo) Mode() fs.FileMode  { return p.mode }
func (p parentInfo) ModTime() time.Time { return time.Time{} }
func (p parentInfo) IsDir() bool        { return p.mode.IsDir() }
func (p parentInfo) Sys() any           { return &syscall.Stat_t{Uid: p.uid} }

var rootDir = parentInfo{mode: fs.ModeDir | 0o755}

// parentStat answers from entries, records every path it is asked for, and says a path it has no
// entry for does not exist.
func parentStat(entries map[string]parentInfo, asked *[]string) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) {
		*asked = append(*asked, p)
		if e, ok := entries[p]; ok {
			return e, nil
		}
		return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
	}
}

func TestRootOwnedParents(t *testing.T) {
	const dir = "/etc/chromium/policies/managed"
	all := func() map[string]parentInfo {
		return map[string]parentInfo{"/": rootDir, "/etc": rootDir, "/etc/chromium": rootDir,
			"/etc/chromium/policies": rootDir, dir: rootDir}
	}
	var asked []string
	if err := rootOwnedParents(dir, parentStat(all(), &asked)); err != nil {
		t.Fatalf("an all-root chain = %v, want nil", err)
	}
	if want := []string{"/", "/etc", "/etc/chromium", "/etc/chromium/policies", dir}; !slices.Equal(asked, want) {
		t.Fatalf("walked %v, want %v", asked, want)
	}

	for name, bad := range map[string]parentInfo{
		"a link":         {mode: fs.ModeSymlink | 0o777},
		"uid 1000":       {mode: fs.ModeDir | 0o755, uid: 1000},
		"mode 0775":      {mode: fs.ModeDir | 0o775},
		"mode 0757":      {mode: fs.ModeDir | 0o757},
		"a regular file": {mode: 0o644},
	} {
		entries := all()
		entries["/etc/chromium"] = bad
		asked = nil
		err := rootOwnedParents(dir, parentStat(entries, &asked))
		if err == nil || !strings.Contains(err.Error(), "/etc/chromium ") {
			t.Errorf("%s at /etc/chromium = %v, want an error naming it", name, err)
		}
	}

	// A missing component ends the walk: writeFile creates it and the rest root-owned.
	entries := all()
	delete(entries, "/etc/chromium")
	delete(entries, "/etc/chromium/policies")
	delete(entries, dir)
	asked = nil
	if err := rootOwnedParents(dir, parentStat(entries, &asked)); err != nil {
		t.Fatalf("a missing /etc/chromium = %v, want nil", err)
	}
	if want := []string{"/", "/etc", "/etc/chromium"}; !slices.Equal(asked, want) {
		t.Fatalf("walked %v past the first missing directory, want %v", asked, want)
	}
}

func TestCeilingParents_OnlyCeilingDirs(t *testing.T) {
	var asked []string
	everything := map[string]parentInfo{}
	for _, d := range ceilingDirs {
		p := "/"
		everything[p] = rootDir
		for _, part := range strings.Split(strings.TrimPrefix(d, "/"), "/") {
			p = filepath.Join(p, part)
			everything[p] = rootDir
		}
	}
	old := managedLstat
	managedLstat = parentStat(everything, &asked)
	t.Cleanup(func() { managedLstat = old })

	for _, d := range []string{"/tmp/wardyn", "/tmp", t.TempDir(), "/run/wardyn/secrets", "/etc", "/etc/chromium/policies"} {
		asked = nil
		if err := ceilingParents(d); err != nil || len(asked) != 0 {
			t.Errorf("ceilingParents(%s) = %v after lstat of %v, want nil with no lstat", d, err, asked)
		}
	}
	for _, d := range ceilingDirs {
		asked = nil
		if err := ceilingParents(d); err != nil {
			t.Fatalf("ceilingParents(%s) = %v on an all-root chain", d, err)
		}
		want := []string{"/"}
		p := "/"
		for _, part := range strings.Split(strings.TrimPrefix(d, "/"), "/") {
			p = filepath.Join(p, part)
			want = append(want, p)
		}
		if !slices.Equal(asked, want) {
			t.Errorf("ceilingParents(%s) read %v, want every directory from / down: %v", d, asked, want)
		}
	}
}

func TestWriteFile_RefusesAnAgentOwnedCeilingParent(t *testing.T) {
	var asked []string
	old := managedLstat
	managedLstat = parentStat(map[string]parentInfo{"/": rootDir, "/etc": {mode: fs.ModeDir | 0o755, uid: 1000}}, &asked)
	t.Cleanup(func() { managedLstat = old })

	err := (&Server{}).writeFile(File{Path: runner.ChromiumPolicyFile, Mode: 0o644, Content: []byte(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "managed file directory /etc must be a directory owned by root") {
		t.Fatalf("writeFile under an agent-owned /etc = %v, want the parent refusal naming /etc (before any write)", err)
	}
	if want := []string{"/", "/etc"}; !slices.Equal(asked, want) {
		t.Fatalf("lstat read %v, want %v", asked, want)
	}

	p := filepath.Join(t.TempDir(), "managed.json")
	if err := (&Server{}).writeFile(File{Path: p, Mode: 0o644, Content: []byte(`{"x":1}`)}); err != nil {
		t.Fatalf("writeFile outside the ceiling dirs = %v, want written", err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != `{"x":1}` {
		t.Fatalf("the file outside the ceiling dirs = %q, %v", b, err)
	}
}

func TestCeilingDirsMatchTheRunner(t *testing.T) {
	want := []string{runner.ManagedFileDir, runner.CodexManagedFileDir, runner.ChromiumPolicyDir}
	if !slices.Equal(ceilingDirs, want) {
		t.Fatalf("ceilingDirs = %v, want the runner's ceiling directories %v", ceilingDirs, want)
	}
}
