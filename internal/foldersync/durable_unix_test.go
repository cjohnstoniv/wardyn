// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package foldersync

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// onLink makes every hard link fail with errno for one test.
func onLink(t *testing.T, errno syscall.Errno) {
	t.Helper()
	old := linkFile
	t.Cleanup(func() { linkFile = old })
	linkFile = func(_ *os.Root, from, to string) error {
		return &os.LinkError{Op: "link", Old: from, New: to, Err: errno}
	}
}

// On a filesystem without hard links the conflict copy is created and copied instead, with the
// mode and time a link would give it, and synced before the push may truncate the sandbox's file. A name
// already taken is still never replaced, and no temp file is left.
func TestInstall_WithoutHardLinksTheCopyIsCreatedAndSynced(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.EXDEV, syscall.ENOTSUP, syscall.EOPNOTSUPP} {
		t.Run(errno.Error(), func(t *testing.T) {
			sy, box, root := runnerSyncer(t, true)
			bothChanged(t, sy, box, root)
			putSyncFile(t, filepath.Join(root, copyName(1)), "someone else's", 0o644)
			want, err := os.Stat(filepath.Join(box, "x.txt"))
			if err != nil {
				t.Fatal(err)
			}
			sandbox := func() string { return getSyncFile(t, filepath.Join(box, "x.txt")) }
			onLink(t, errno)
			var events []string
			onSync(t, func(f *os.File) error {
				if strings.Contains(f.Name(), "sandbox conflict") {
					events = append(events, "copy synced, sandbox="+sandbox())
				}
				return nil
			}, nil)
			if _, err := sy.Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			if wantEv := []string{"copy synced, sandbox=sandbox edit!"}; !slices.Equal(events, wantEv) {
				t.Errorf("events = %q, want %q", events, wantEv)
			}
			if getSyncFile(t, filepath.Join(root, copyName(1))) != "someone else's" || getSyncFile(t, filepath.Join(root, copyName(2))) != "sandbox edit!" || sandbox() != "laptop edit" {
				t.Errorf("copy 1 = %q, copy 2 = %q, sandbox = %q", getSyncFile(t, filepath.Join(root, copyName(1))), getSyncFile(t, filepath.Join(root, copyName(2))), sandbox())
			}
			got, err := os.Stat(filepath.Join(root, copyName(2)))
			if err != nil || got.Mode().Perm() != 0o644 || !got.ModTime().Equal(want.ModTime()) {
				t.Errorf("copy mode %v time %v, want -rw-r--r-- %v (%v)", got.Mode().Perm(), got.ModTime(), want.ModTime(), err)
			}
			ents, _ := os.ReadDir(root)
			for _, e := range ents {
				if strings.Contains(e.Name(), ".wardyn-sync-") {
					t.Errorf("temp file left: %s", e.Name())
				}
			}
		})
	}
}

// A copy whose sync fails is removed, being the pass's own file, and the push is refused. A link that
// fails for another reason (EIO) is not tried as a copy and refuses the push too.
func TestInstall_AFailedCopyOrLinkRefusesThePush(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.EIO} {
		t.Run(errno.Error(), func(t *testing.T) {
			sy, box, root := runnerSyncer(t, true)
			bothChanged(t, sy, box, root)
			onLink(t, errno)
			onSync(t, func(f *os.File) error {
				if strings.Contains(f.Name(), "sandbox conflict") {
					return errFault
				}
				return nil
			}, nil)
			rep, err := sy.Pass(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := getSyncFile(t, filepath.Join(box, "x.txt")); got != "sandbox edit!" || !slices.Equal(rep.UnsyncedPaths, []string{"x.txt"}) {
				t.Errorf("sandbox = %q, report = %+v: want the push refused", got, rep)
			}
			if _, err := os.Stat(filepath.Join(root, copyName(1))); !os.IsNotExist(err) {
				t.Errorf("a copy that was not kept is on the laptop: %v", err)
			}
		})
	}
}
