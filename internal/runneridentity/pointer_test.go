// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package runneridentity

import (
	"encoding/json"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pointerFixture is runner r1's pointer file (0600 in a 0700 directory) naming a live socket.
func pointerFixture(t *testing.T) (dir, sock string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "local-attach")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("", "wp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock = filepath.Join(sockDir, "attach.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	writePointer(t, dir, "r1", Pointer{RunnerID: "r1", OrgURL: "https://org.example.com/", Socket: sock})
	return dir, sock
}

func writePointer(t *testing.T, dir, name string, p Pointer) {
	t.Helper()
	b, _ := json.Marshal(p)
	if err := os.WriteFile(filepath.Join(dir, name+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadPointer(t *testing.T) {
	dir, sock := pointerFixture(t)
	p, err := ReadPointer(dir, "r1")
	if err != nil || p.Socket != sock || p.RunnerID != "r1" {
		t.Fatalf("ReadPointer = %+v, %v", p, err)
	}
}

// TestReadPointerRefuses: each way a pointer file could lead this CLI to someone else's socket.
func TestReadPointerRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		break_     func(t *testing.T, dir, sock string)
	}{
		{"directory open to others", "open to other users", func(t *testing.T, dir, _ string) { _ = os.Chmod(dir, 0o755) }},
		{"file open to others", "open to other users", func(t *testing.T, dir, _ string) { _ = os.Chmod(filepath.Join(dir, "r1.json"), 0o640) }},
		{"file is a link", "not a regular file", func(t *testing.T, dir, _ string) {
			p := filepath.Join(dir, "r1.json")
			_ = os.Rename(p, p+".real")
			_ = os.Symlink(p+".real", p)
		}},
		{"names another runner", `names runner "r2"`, func(t *testing.T, dir, sock string) {
			writePointer(t, dir, "r1", Pointer{RunnerID: "r2", OrgURL: "https://org.example.com", Socket: sock})
		}},
		{"socket is a regular file", "not a socket", func(t *testing.T, dir, _ string) {
			writePointer(t, dir, "r1", Pointer{RunnerID: "r1", OrgURL: "https://org.example.com", Socket: filepath.Join(dir, "r1.json")})
		}},
		{"no socket", "names no socket", func(t *testing.T, dir, _ string) {
			writePointer(t, dir, "r1", Pointer{RunnerID: "r1", OrgURL: "https://org.example.com"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, sock := pointerFixture(t)
			tc.break_(t, dir, sock)
			if _, err := ReadPointer(dir, "r1"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ReadPointer = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestReadPointerRefusesAnotherUsersSocket: a socket, file or directory whose owner is not this user is
// refused (the test stands in for another user through the euid seam, as it cannot chown).
func TestReadPointerRefusesAnotherUsersSocket(t *testing.T) {
	dir, sock := pointerFixture(t)
	old := geteuid
	t.Cleanup(func() { geteuid = old })
	geteuid = func() int { return os.Geteuid() + 1 }
	if err := owned(sock, fs.ModeSocket); err == nil || !strings.Contains(err.Error(), "belongs to another user") {
		t.Fatalf("owned(socket) = %v, want another user's socket refused", err)
	}
	if _, err := ReadPointer(dir, "r1"); err == nil || !strings.Contains(err.Error(), "belongs to another user") {
		t.Fatalf("ReadPointer = %v, want refused", err)
	}
}

func TestPointerForOrg(t *testing.T) {
	p := Pointer{OrgURL: "https://org.example.com/"}
	for url, want := range map[string]bool{
		"https://org.example.com":     true,
		"https://org.example.com:443": true,
		"https://other.example.com":   false,
		"https://org.example.com/b":   false,
	} {
		if got := p.ForOrg(url); got != want {
			t.Errorf("ForOrg(%q) = %v, want %v", url, got, want)
		}
	}
}
