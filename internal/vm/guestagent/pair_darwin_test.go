// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// The pairing key as stage 1 leaves it (install -o root -g wheel -m 600, then dd) is root:wheel 0600, so
// the guest agent (root) reads it and a standard user such as agent cannot. The test makes its own key
// file that way through sudo, as the hosted macOS runner allows.
func TestDarwinPairKeyIsReadableByRootOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runs as root, which reads any file: the refusal needs a standard user")
	}
	if err := exec.Command("/usr/bin/sudo", "-n", "true").Run(); err != nil {
		t.Skipf("no non-interactive sudo to make a root-owned key file: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, testPairKey, 0o600); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "pair.key")
	sudo := func(argv ...string) []byte {
		t.Helper()
		out, err := exec.Command("/usr/bin/sudo", append([]string{"-n"}, argv...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("sudo %q: %v\n%s", argv, err, out)
		}
		return out
	}
	t.Cleanup(func() { _ = exec.Command("/usr/bin/sudo", "-n", "/bin/rm", "-f", key).Run() })
	sudo("/usr/bin/install", "-o", "root", "-g", "wheel", "-m", "600", src, key)

	fi, err := os.Lstat(key)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 || st.Uid != 0 || st.Gid != 0 {
		t.Fatalf("key file %v uid %d gid %d, want a regular root:wheel 0600 file", fi.Mode(), st.Uid, st.Gid)
	}
	if _, err := os.ReadFile(key); !errors.Is(err, fs.ErrPermission) || !errors.Is(err, syscall.EACCES) {
		t.Fatalf("a standard user's read of the key: %v, want EACCES", err)
	}
	if _, err := ReadPairKey(key); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("ReadPairKey as a standard user: %v, want EACCES", err)
	}
	if got := sudo("/bin/cat", key); !bytes.Equal(got, testPairKey) {
		t.Fatalf("root reads %d bytes, not the key", len(got))
	}
}
