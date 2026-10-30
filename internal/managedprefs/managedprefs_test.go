// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package managedprefs

import (
	"path/filepath"
	"testing"
)

func TestBool(t *testing.T) {
	for in, want := range map[string]struct{ val, set, bad bool }{
		"":      {},
		"true":  {val: true, set: true},
		"True":  {val: true, set: true},
		"1":     {val: true, set: true},
		"false": {set: true},
		"0":     {set: true},
		"maybe": {set: true, bad: true},
	} {
		val, set, err := Bool(in)
		if val != want.val || set != want.set || (err != nil) != want.bad {
			t.Errorf("Bool(%q) = %v, %v, %v; want %+v", in, val, set, err, want)
		}
	}
}

// Files lists the device-wide profile last, and Read takes the first file that sets the key.
func TestFilesOrderIsReadsOrder(t *testing.T) {
	files := Files("/root", Domain)
	if got, want := files[len(files)-1], filepath.Join("/root", Domain+".plist"); got != want {
		t.Fatalf("last file = %q, want the device-wide %q", got, want)
	}
	seen := map[string]bool{}
	v, err := Read("darwin", "/root", Domain, "K", func(file, key string) (string, error) {
		seen[file] = true
		return map[bool]string{true: "v"}[file == files[len(files)-1]], nil
	})
	if err != nil || v != "v" || len(seen) != len(files) {
		t.Fatalf("Read = %q, %v after %d files, want v after %d", v, err, len(seen), len(files))
	}
}
