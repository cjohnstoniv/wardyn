// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// Run in an isolated root container; subprocesses really drop to the shipped
// daemon/agent UIDs. Ordinary unprivileged test runs cannot exercise setuid.
func TestFSStore_DifferentUIDs(t *testing.T) {
	if action := os.Getenv("WARDYN_RECORDING_TEST_ACTION"); action != "" {
		recordingUIDAction(t, action)
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires isolated root container for distinct-UID filesystem proof")
	}
	for _, mode := range []os.FileMode{0o777, 0o770, os.ModeSetgid | 0o770} {
		for _, first := range []string{"daemon", "agent"} {
			t.Run(mode.String()+"/"+first, func(t *testing.T) {
				root, err := os.MkdirTemp("/tmp", "wardyn-recording-uids-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(root) })
				if err := os.Chown(root, 65532, 2222); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(root, mode); err != nil {
					t.Fatal(err)
				}
				act := func(who, action, key, umask string) {
					t.Helper()
					runRecordingUID(t, root, who, action, key, umask)
				}
				act(first, "save", "run", "022")
				for _, who := range []string{"daemon", "agent"} {
					action := "denied"
					if who == first {
						action = "save"
					}
					act(who, action, "run", "022")
				}
				act("agent", "save", ".tmp-cast-collision", "077")
				act(first, "erase", "run", "077")
				act("agent", "erased", "run", "077")
				act("agent", "erased", "run~part-2", "077")
				act("agent", "save", "new", "022")
				act("agent", "erase", "new", "077")
				act("daemon", "erased", "new", "077")
				act("agent", "save", "public-mode", "022")
				act("agent", "save", "private-mode", "077")
				for name, want := range map[string]os.FileMode{"public-mode.cast": 0o644, "private-mode.cast": 0o600} {
					info, err := os.Stat(filepath.Join(root, name))
					if err != nil || info.Mode().Perm() != want {
						t.Fatalf("fallback mode %s = %v, %v", name, info, err)
					}
				}
				assertRecordingMetadataModes(t, root, mode)
			})
		}
	}
	t.Run("root_widened_after_daemon_read", func(t *testing.T) {
		root, err := os.MkdirTemp("/tmp", "wardyn-recording-uids-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
		s, _ := recording.NewFSStore(root)
		if _, err := s.OpenCast(t.Context(), "run"); !errors.Is(err, recording.ErrNotFound) {
			t.Fatal(err)
		}
		if _, err := s.DeleteRun(t.Context(), "erased"); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0o777); err != nil {
			t.Fatal(err)
		}
		runRecordingUID(t, root, "agent", "save", "run", "022")
		runRecordingUID(t, root, "agent", "erased", "erased", "077")
		runRecordingUID(t, root, "agent", "save", "new", "077")
	})
}

func runRecordingUID(t *testing.T, root, who, action, key, umask string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cred := &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{2222}}
	if who == "daemon" {
		cred.Uid, cred.Gid = 65532, 2222
	}
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestFSStore_DifferentUIDs$")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	cmd.Env = append(os.Environ(), "WARDYN_RECORDING_TEST_ACTION="+action,
		"WARDYN_RECORDING_TEST_ROOT="+root, "WARDYN_RECORDING_TEST_KEY="+key, "WARDYN_RECORDING_TEST_UMASK="+umask)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s %s: %v\n%s", who, action, key, err, out)
	}
}

func recordingUIDAction(t *testing.T, action string) {
	t.Helper()
	mask := 0o022
	if os.Getenv("WARDYN_RECORDING_TEST_UMASK") == "077" {
		mask = 0o077
	}
	syscall.Umask(mask)
	s, err := recording.NewFSStore(os.Getenv("WARDYN_RECORDING_TEST_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	key := os.Getenv("WARDYN_RECORDING_TEST_KEY")
	if action == "erase" {
		_, err = s.DeleteRun(t.Context(), key)
	} else {
		err = s.SaveRecordingFile(t.Context(), key+".cast", strings.NewReader("fallback"))
	}
	if action == "erased" {
		if !errors.Is(err, recording.ErrErased) {
			t.Fatalf("late cross-UID save = %v", err)
		}
	} else if action == "denied" {
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("cross-UID overwrite = %v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
}

func assertRecordingMetadataModes(t *testing.T, root string, mode os.FileMode) {
	t.Helper()
	s, _ := recording.NewFSStore(root)
	for _, name := range []string{"run.lock", ".tmp-cast-collision.lock"} {
		before, err := os.Stat(filepath.Join(root, name))
		if err != nil || before.Mode().Perm() != 0o444 || before.Size() != 0 {
			t.Fatalf("lock permissions: %v, %v", before, err)
		}
		if _, err := s.Sweep(-1); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(filepath.Join(root, name))
		if err != nil || !os.SameFile(before, after) {
			t.Fatalf("retention changed lock identity: %v", err)
		}
	}
	info, err := os.Stat(filepath.Join(root, ".erased"))
	if err != nil || info.Mode().Perm()&0o222 != mode.Perm()&0o222 || info.Mode()&os.ModeSetgid != mode&os.ModeSetgid {
		t.Fatalf("marker directory did not retain root write/setgid modes: %v, %v", info, err)
	}
}
