// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestMacSharesMountsWithItsOptionsAndMakesTheTarget(t *testing.T) {
	r := &fakeRunner{}
	home := t.TempDir()
	m := MacShares{Run: r, Home: home, UID: os.Getuid(), GID: os.Getgid()}
	target := filepath.Join(home, "work", "proj")
	if err := m.Mount(context.Background(), Mount{Tag: "wardyn0", Target: target, ReadOnly: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Mount(context.Background(), Mount{Tag: "wardyn1", Target: filepath.Join(home, "out")}); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"/sbin/mount", "-t", "virtiofs", "-o", "nosuid,nodev,noowners,rdonly", "wardyn0", target},
		{"/sbin/mount", "-t", "virtiofs", "-o", "nosuid,nodev,noowners", "wardyn1", filepath.Join(home, "out")},
	}
	if !slices.EqualFunc(r.calls, want, slices.Equal) {
		t.Fatalf("mount calls = %q, want %q", r.calls, want)
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		t.Fatalf("target not made: %v", err)
	}
}

func TestMacSharesRefusals(t *testing.T) {
	home := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Mount{
		"the home itself":     {Tag: "wardyn0", Target: home},
		"outside the home":    {Tag: "wardyn0", Target: "/tmp/x"},
		"a sibling prefix":    {Tag: "wardyn0", Target: home + "2/x"},
		"the Wardyn folder":   {Tag: "wardyn0", Target: filepath.Join(home, ".wardyn")},
		"under it":            {Tag: "wardyn0", Target: filepath.Join(home, ".wardyn", "x")},
		"under it, any case":  {Tag: "wardyn0", Target: filepath.Join(home, ".WARDYN", "x")},
		"uncleaned":           {Tag: "wardyn0", Target: home + "/a/../b"},
		"relative":            {Tag: "wardyn0", Target: "a/b"},
		"through a link":      {Tag: "wardyn0", Target: filepath.Join(home, "link", "x")},
		"through a file":      {Tag: "wardyn0", Target: filepath.Join(home, "file", "x")},
		"another tag":         {Tag: "My Shared Files", Target: filepath.Join(home, "x")},
		"a bare prefix":       {Tag: "wardyn", Target: filepath.Join(home, "x")},
		"a tag with a suffix": {Tag: "wardyn0x", Target: filepath.Join(home, "x")},
	} {
		r := &fakeRunner{}
		m := MacShares{Run: r, Home: home, UID: os.Getuid(), GID: os.Getgid()}
		if err := m.Mount(context.Background(), s); err == nil {
			t.Errorf("%s: mounted", name)
		}
		if len(r.calls) != 0 {
			t.Errorf("%s: ran %q", name, r.calls)
		}
	}
	r := &fakeRunner{out: func([]string) ([]byte, error) { return nil, errors.New("mount_virtiofs: bad option") }}
	if err := (MacShares{Run: r, Home: home, UID: os.Getuid(), GID: os.Getgid()}).Mount(context.Background(),
		Mount{Tag: "wardyn0", Target: filepath.Join(home, "x")}); err == nil {
		t.Error("a failed mount(8) was not an error")
	}
}
