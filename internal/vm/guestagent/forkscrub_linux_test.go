// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// rigOn is a fresh boot's agent (its own marker directory) on the disk whose root-only state is out.
func rigOn(t *testing.T, out string) *rig {
	t.Helper()
	r := newRig(t)
	r.srv.OutDir = out
	return r
}

// TestForkBootScrub: a fork's first Init removes every file its source's last Init wrote, by the record that
// Init kept on the disk, before writing its own; a fork with no record is refused before it writes anything;
// and a link on a recorded path is never followed.
func TestForkBootScrub(t *testing.T) {
	disk := t.TempDir() // the source's disk, which the fork's copy carries
	out := filepath.Join(disk, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(disk, "etc", "wardyn", "git-helper-secret")
	managed := filepath.Join(disk, "home", "agent", ".gitconfig")
	source := Init{Files: []File{{Path: helper, Mode: 0o600, Content: []byte("source secret")}, {Path: managed, Mode: 0o644, Content: []byte("[user]")}}}
	if _, err := rigOn(t, out).client(t).Init(ctxT(t), source); err != nil {
		t.Fatalf("source init = %v", err)
	}
	for _, p := range []string{helper, managed} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the source's file %s = %v", p, err)
		}
	}

	forkFile := filepath.Join(disk, "etc", "wardyn", "fork-helper-secret")
	fork := Init{Fork: true, Files: []File{{Path: forkFile, Mode: 0o600, Content: []byte("fork secret")}, {Path: managed, Mode: 0o644, Content: []byte("[fork]")}}}
	if _, err := rigOn(t, out).client(t).Init(ctxT(t), fork); err != nil {
		t.Fatalf("fork init = %v", err)
	}
	if _, err := os.Stat(helper); !os.IsNotExist(err) {
		t.Errorf("the source's git helper secret is still on the fork's disk: %v", err)
	}
	if b, _ := os.ReadFile(managed); string(b) != "[fork]" {
		t.Errorf("the fork's own managed file = %q", b)
	}
	if b, _ := os.ReadFile(forkFile); string(b) != "fork secret" {
		t.Errorf("the fork's own file = %q", b)
	}
	var rec initFiles
	b, err := os.ReadFile(filepath.Join(out, initFilesRecord))
	if err != nil || json.Unmarshal(b, &rec) != nil || !slices.Equal(rec.Files, []string{forkFile, managed}) {
		t.Fatalf("the record after the fork's init = %s, %v", b, err)
	}
	if st, err := os.Stat(filepath.Join(out, initFilesRecord)); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the record's mode = %v, %v", st, err)
	}

	// A fork whose disk has no record: the source's files cannot be found, so the fork never starts.
	bare := t.TempDir()
	never := filepath.Join(t.TempDir(), "never")
	if _, err := rigOn(t, bare).client(t).Init(ctxT(t), Init{Fork: true, Files: []File{{Path: never, Mode: 0o600}}}); err == nil || !strings.Contains(err.Error(), "no record") {
		t.Fatalf("fork init with no record = %v, want refused", err)
	}
	if _, err := os.Stat(never); !os.IsNotExist(err) {
		t.Errorf("a refused fork init wrote its file: %v", err)
	}

	// A plain boot with no record is a first boot: it records its files and goes on.
	if _, err := rigOn(t, t.TempDir()).client(t).Init(ctxT(t), Init{}); err != nil {
		t.Fatalf("first boot init = %v", err)
	}

	// The agent's user replaced a recorded file's folder with a link to another folder: the removal refuses
	// rather than follow it, and the file behind the link stays.
	victim := filepath.Join(disk, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "passwd"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(disk, "home", "agent", "cfg")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	planted, _ := json.Marshal(initFiles{Files: []string{filepath.Join(link, "passwd")}})
	if err := os.WriteFile(filepath.Join(out, initFilesRecord), planted, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rigOn(t, out).client(t).Init(ctxT(t), Init{Fork: true}); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("fork init through a planted link = %v, want refused", err)
	}
	if b, _ := os.ReadFile(filepath.Join(victim, "passwd")); string(b) != "keep" {
		t.Errorf("the file behind the planted link = %q, want it kept", b)
	}

	// A link two folders up the recorded path is refused the same way, and the file behind it stays.
	deep := filepath.Join(disk, "home", "agent", "deep")
	if err := os.Symlink(filepath.Dir(victim), deep); err != nil {
		t.Fatal(err)
	}
	planted, _ = json.Marshal(initFiles{Files: []string{filepath.Join(deep, filepath.Base(victim), "passwd")}})
	if err := os.WriteFile(filepath.Join(out, initFilesRecord), planted, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rigOn(t, out).client(t).Init(ctxT(t), Init{Fork: true}); err == nil || !strings.Contains(err.Error(), "link") {
		t.Fatalf("fork init through a link further up = %v, want refused", err)
	}
	if b, _ := os.ReadFile(filepath.Join(victim, "passwd")); string(b) != "keep" {
		t.Errorf("the file behind the link further up = %q, want it kept", b)
	}

	// A recorded file that is itself a link is removed as the link; the file it names stays.
	fileLink := filepath.Join(disk, "home", "agent", "passwd-link")
	if err := os.Symlink(filepath.Join(victim, "passwd"), fileLink); err != nil {
		t.Fatal(err)
	}
	planted, _ = json.Marshal(initFiles{Files: []string{fileLink}})
	if err := os.WriteFile(filepath.Join(out, initFilesRecord), planted, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rigOn(t, out).client(t).Init(ctxT(t), Init{Fork: true}); err != nil {
		t.Fatalf("fork init with a recorded link = %v", err)
	}
	if _, err := os.Lstat(fileLink); !os.IsNotExist(err) {
		t.Errorf("the recorded link is still there: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(victim, "passwd")); string(b) != "keep" {
		t.Errorf("the file a recorded link named = %q, want it kept", b)
	}
}
