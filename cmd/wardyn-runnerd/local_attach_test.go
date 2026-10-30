// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// openTestSocket opens a local attach socket whose pointer file lands under a private config dir.
func openTestSocket(t *testing.T, runnerID, org string, serve func(net.Conn)) *localSocket {
	t.Helper()
	l, err := newLocalSocket(true, runnerID, org, t.TempDir(), serve)
	if err != nil || l == nil {
		t.Fatalf("newLocalSocket = %v, %v", l, err)
	}
	l.pointerDir = filepath.Join(t.TempDir(), "local-attach")
	return l
}

func readPointer(t *testing.T, l *localSocket) localPointer {
	t.Helper()
	fi, err := os.Lstat(l.pointerPath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("pointer file mode %v", fi.Mode().Perm())
	}
	b, err := os.ReadFile(l.pointerPath())
	if err != nil {
		t.Fatal(err)
	}
	var p localPointer
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A connection from another uid is closed before runnerd reads a byte of it.
func TestLocalAttachSocketClosesAnotherUIDBeforeReading(t *testing.T) {
	served := make(chan struct{}, 1)
	l := openTestSocket(t, "11111111-1111-1111-1111-111111111111", "https://org.example", func(c net.Conn) { served <- struct{}{}; _ = c.Close() })
	l.peerUID = func(net.Conn) (uint32, error) { return uint32(os.Geteuid()) + 1, nil }
	l.Set(true)
	defer l.Close()
	c, err := net.Dial("unix", readPointer(t, l).Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("h"))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("another uid's connection read %d bytes, %v", n, err)
	}
	select {
	case <-served:
		t.Fatal("another uid's connection reached the protocol")
	default:
	}
}

// Two runners (two organisations) under one OS user each keep their own pointer file.
func TestLocalAttachPointerFilesCoexist(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "local-attach")
	a := openTestSocket(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "HTTPS://Org-A.example:443/wardyn/", func(c net.Conn) { _ = c.Close() })
	b := openTestSocket(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "https://org-b.example", func(c net.Conn) { _ = c.Close() })
	a.pointerDir, b.pointerDir = dir, dir
	a.Set(true)
	b.Set(true)
	defer b.Close()
	pa, pb := readPointer(t, a), readPointer(t, b)
	if pa.OrgURL != "https://org-a.example/wardyn" || pa.RunnerID != a.runnerID || pb.OrgURL != "https://org-b.example" || pa.Socket == pb.Socket {
		t.Fatalf("pointers = %+v, %+v", pa, pb)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("pointer directory = %v, %v", fi, err)
	}
	a.Set(false) // the organisation turned local attach off
	if _, err := os.Lstat(a.pointerPath()); !os.IsNotExist(err) {
		t.Fatalf("a closed socket left its pointer file: %v", err)
	}
	if _, err := os.Lstat(pa.Socket); !os.IsNotExist(err) {
		t.Fatalf("a closed socket left its socket file: %v", err)
	}
	if readPointer(t, b).RunnerID != b.runnerID {
		t.Fatal("closing one runner's socket touched the other's pointer")
	}
}

// --local-attach=off withdraws the socket whatever the organisation says.
func TestLocalAttachOffOpensNoSocket(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HOME", cfg)
	state := t.TempDir()
	l, err := newLocalSocket(false, "11111111-1111-1111-1111-111111111111", "https://org.example", state, func(c net.Conn) { _ = c.Close() })
	if err != nil {
		t.Fatal(err)
	}
	l.Set(true) // AUTH_OK says local_attach: true
	defer l.Close()
	for _, dir := range []string{state, cfg} {
		var found []string
		_ = filepath.Walk(dir, func(p string, _ os.FileInfo, _ error) error {
			if p != dir {
				found = append(found, p)
			}
			return nil
		})
		if len(found) != 0 {
			t.Fatalf("--local-attach=off wrote %v", found)
		}
	}
	for v, want := range map[string]bool{"on": true, "off": false} {
		if got, err := (serveOptions{localAttach: v}).localAttachEnabled(); err != nil || got != want {
			t.Fatalf("--local-attach=%s = %v, %v", v, got, err)
		}
	}
	if _, err := (serveOptions{localAttach: "maybe"}).localAttachEnabled(); err == nil {
		t.Fatal("--local-attach=maybe was accepted")
	}
	if f := newServeCommand(nil).Flags().Lookup("local-attach"); f == nil || f.DefValue != "on" {
		t.Fatalf("--local-attach flag = %+v", f)
	}
}
