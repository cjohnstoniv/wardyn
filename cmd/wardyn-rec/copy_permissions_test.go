// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCopyToDir_PreservesExistingInodeAndMode(t *testing.T) {
	for _, ext := range []string{".cast", ".log"} {
		for _, mode := range []os.FileMode{0o600, 0o640, 0o660} {
			t.Run(fmt.Sprintf("%s/%o", ext, mode), func(t *testing.T) {
				srcDir, dstDir := t.TempDir(), t.TempDir()
				src, dst := filepath.Join(srcDir, "run"+ext), filepath.Join(dstDir, "run"+ext)
				writeCopyFixture(t, src, "new", 0o600)
				writeCopyFixture(t, dst, "old and longer", mode)
				before, err := os.Stat(dst)
				if err != nil {
					t.Fatal(err)
				}
				if err := copyToDir(src, dstDir); err != nil {
					t.Fatal(err)
				}
				assertCopyDestination(t, dst, before, "new")
			})
		}
	}
}

// Run this binary in an isolated root container so the filesystem, rather
// than simulated mode bits, decides which daemon/agent UID may overwrite.
func TestCopyToDir_DifferentUIDPermissions(t *testing.T) {
	if action := os.Getenv("WARDYN_COPY_TEST_ACTION"); action != "" {
		copyUIDAction(t, action)
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires isolated root container for distinct-UID fallback proof")
	}
	for _, owner := range []uint32{1000, 65532} {
		for _, ext := range []string{".cast", ".log"} {
			for _, access := range []string{"private", "read-only", "owner-read-only", "group-write", "acl-write"} {
				t.Run(fmt.Sprintf("%d/%s/%s", owner, ext, access), func(t *testing.T) {
					copyUIDAccess(t, owner, ext, access)
				})
			}
			t.Run(fmt.Sprintf("%d/%s/private-staging", owner, ext), func(t *testing.T) {
				copyUIDPrivateStaging(t, owner, ext)
			})
			for _, mask := range []string{"022", "077"} {
				t.Run(fmt.Sprintf("%d/%s/new-umask-%s", owner, ext, mask), func(t *testing.T) {
					root := copyUIDRoot(t)
					src, dst := filepath.Join(root, "src"+ext), filepath.Join(root, "out", "src"+ext)
					writeCopyFixture(t, src, "new", 0o444)
					runCopyUID(t, copyUIDCommand(t, owner, "new", src, filepath.Dir(dst), mask))
					info, err := os.Stat(dst)
					want := os.FileMode(0o644)
					if mask == "077" {
						want = 0o600
					}
					if err != nil || info.Mode().Perm() != want || info.Sys().(*syscall.Stat_t).Uid != owner {
						t.Fatalf("new fallback mode/owner: %v, %v", info, err)
					}
				})
			}
		}
	}
}

func copyUIDAccess(t *testing.T, owner uint32, ext, access string) {
	t.Helper()
	root := copyUIDRoot(t)
	src, dst := filepath.Join(root, "src"+ext), filepath.Join(root, "out", "src"+ext)
	writeCopyFixture(t, src, "replacement", 0o444)
	writeCopyFixture(t, dst, "private original bytes", 0o600)
	if err := os.Chown(dst, int(owner), 2222); err != nil {
		t.Fatal(err)
	}
	writer, action := otherCopyUID(owner), "deny"
	switch access {
	case "read-only", "owner-read-only":
		if err := os.Chmod(dst, 0o444); err != nil {
			t.Fatal(err)
		}
		if access == "owner-read-only" {
			writer = owner
		}
	case "group-write":
		if err := os.Chmod(dst, 0o660); err != nil {
			t.Fatal(err)
		}
		action = "allow"
	case "acl-write":
		installCopyACL(t, dst, writer)
		action = "allow"
	}
	before, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	acl := readCopyACL(t, dst)
	runCopyUID(t, copyUIDCommand(t, writer, action, src, filepath.Dir(dst), "022"))
	want := "private original bytes"
	if action == "allow" {
		want = "replacement"
	}
	assertCopyDestination(t, dst, before, want)
	if !bytes.Equal(acl, readCopyACL(t, dst)) {
		t.Fatal("fallback changed destination ACL")
	}
}

func copyUIDAction(t *testing.T, action string) {
	t.Helper()
	mask := 0o022
	if os.Getenv("WARDYN_COPY_TEST_UMASK") == "077" {
		mask = 0o077
	}
	syscall.Umask(mask)
	src, dstDir := os.Getenv("WARDYN_COPY_TEST_SRC"), os.Getenv("WARDYN_COPY_TEST_DST")
	if action == "private" {
		if _, err := os.ReadFile(src); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("peer read staging bytes: %v", err)
		}
		return
	}
	if action == "allow" || action == "deny" {
		f, err := os.OpenFile(filepath.Join(dstDir, filepath.Base(src)), os.O_WRONLY, 0)
		if f != nil {
			_ = f.Close()
		}
		if (err == nil) != (action == "allow") {
			t.Fatalf("kernel write-access precondition: %v", err)
		}
	}
	err := copyToDir(src, dstDir)
	if action == "deny" {
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("unauthorized fallback overwrite: %v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
}

func copyUIDCommand(t *testing.T, uid uint32, action, src, dst, mask string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestCopyToDir_DifferentUIDPermissions$", "-test.v")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid, Groups: []uint32{2222}}}
	cmd.Env = append(os.Environ(), "WARDYN_COPY_TEST_ACTION="+action, "WARDYN_COPY_TEST_SRC="+src,
		"WARDYN_COPY_TEST_DST="+dst, "WARDYN_COPY_TEST_UMASK="+mask)
	return cmd
}

func runCopyUID(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("UID subprocess: %v\n%s", err, out)
	}
}

func otherCopyUID(uid uint32) uint32 {
	if uid == 1000 {
		return 65532
	}
	return 1000
}

func copyUIDRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "wardyn-copy-uids-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "out"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "out"), 0o777); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeCopyFixture(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func assertCopyDestination(t *testing.T, path string, before os.FileInfo, want string) {
	t.Helper()
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if !os.SameFile(before, after) || before.Mode() != after.Mode() || a.Uid != b.Uid || a.Gid != b.Gid {
		t.Fatalf("changed inode, permissions or ownership: before=%+v after=%+v", a, b)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != want {
		t.Fatalf("destination bytes = %q, %v", data, err)
	}
}

func installCopyACL(t *testing.T, path string, uid uint32) {
	t.Helper()
	// Linux's posix_acl_xattr header/entries: owner rw, named UID rw, group
	// and other none, mask rw. This grants access independently of the group.
	acl := binary.LittleEndian.AppendUint32(nil, 2)
	for _, entry := range [][3]uint32{{1, 6, ^uint32(0)}, {2, 6, uid}, {4, 0, ^uint32(0)}, {16, 6, ^uint32(0)}, {32, 0, ^uint32(0)}} {
		acl = binary.LittleEndian.AppendUint16(acl, uint16(entry[0]))
		acl = binary.LittleEndian.AppendUint16(acl, uint16(entry[1]))
		acl = binary.LittleEndian.AppendUint32(acl, entry[2])
	}
	if err := unix.Setxattr(path, "system.posix_acl_access", acl, 0); errors.Is(err, unix.ENOTSUP) {
		t.Skip("filesystem does not support POSIX ACLs")
	} else if err != nil {
		t.Fatal(err)
	}
}

func readCopyACL(t *testing.T, path string) []byte {
	t.Helper()
	buf := make([]byte, 4096)
	n, err := unix.Getxattr(path, "system.posix_acl_access", buf)
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf[:n]
}
