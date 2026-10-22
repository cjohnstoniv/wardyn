// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/foldersync"
)

func TestSyncGateway_IgnoreFilesBothWays(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	putSyncFile(t, filepath.Join(root, ".gitignore"), "node_modules/\n*.log\n.env\n", 0o644)
	putSyncFile(t, filepath.Join(root, ".wardynignore"), "!keep.log\n", 0o644)
	putSyncFile(t, filepath.Join(root, "src/.gitignore"), "gen/\n", 0o644)
	putSyncFile(t, filepath.Join(root, "node_modules/pkg/index.js"), "dep", 0o644)
	putSyncFile(t, filepath.Join(root, "debug.log"), "noise", 0o644)
	putSyncFile(t, filepath.Join(root, "keep.log"), "wanted", 0o644)
	putSyncFile(t, filepath.Join(root, ".env"), "SECRET=1", 0o644)
	putSyncFile(t, filepath.Join(root, "src/gen/out.go"), "generated", 0o644)
	putSyncFile(t, filepath.Join(root, "src/main.go"), "package main", 0o644)
	// What the agent makes in the sandbox under an ignored path never comes back either.
	putSyncFile(t, filepath.Join(box, "node_modules/x/y.js"), "sandbox dep", 0o644)
	putSyncFile(t, filepath.Join(box, "build.log"), "sandbox log", 0o644)

	rep := mustPass(t, sy)
	want := []string{".gitignore", ".wardynignore", "keep.log", "src/.gitignore", "src/main.go"}
	if !slices.Equal(rep.Pushed, want) {
		t.Fatalf("pushed = %v, want %v", rep.Pushed, want)
	}
	for _, p := range []string{"node_modules/pkg/index.js", "debug.log", ".env", "src/gen/out.go"} {
		if _, err := os.Stat(filepath.Join(box, p)); err == nil {
			t.Errorf("%s is ignored but reached the sandbox", p)
		}
	}
	if len(rep.Pulled) != 0 {
		t.Errorf("pulled %v: ignored sandbox paths must never reach the laptop", rep.Pulled)
	}
	if rep.Ignored == 0 {
		t.Error("the report counts no ignored entries")
	}
}

func TestSyncGateway_OrganisationRulesCannotBeUndone(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	sy.Opts.OrgDeny = []string{".env*", "*.pem"}
	putSyncFile(t, filepath.Join(root, ".wardynignore"), "!.env.local\n!*.pem\n", 0o644)
	putSyncFile(t, filepath.Join(root, ".env.local"), "TOKEN=1", 0o644)
	putSyncFile(t, filepath.Join(root, "certs/site.pem"), "key", 0o644)
	putSyncFile(t, filepath.Join(box, "leak.pem"), "sandbox key", 0o644)

	rep := mustPass(t, sy)
	if !slices.Equal(rep.Pushed, []string{".wardynignore"}) || len(rep.Pulled) != 0 {
		t.Fatalf("pushed %v, pulled %v: an organisation rule must hold against every ignore file, both ways", rep.Pushed, rep.Pulled)
	}
	for _, p := range []string{".env.local", "certs/site.pem", "leak.pem"} {
		if !slices.ContainsFunc(rep.Refused, func(r foldersync.Refusal) bool { return r.Path == p && r.Reason == "organisation rule" }) {
			t.Errorf("%s is not refused as an organisation rule: %v", p, rep.Refused)
		}
	}
}

func TestSyncGateway_AnIgnoredFileIsNeverDeleted(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, false)
	putSyncFile(t, filepath.Join(root, "out/app.bin"), "built", 0o644)
	mustPass(t, sy)
	// The folder becomes ignored after it was synced: it is left alone in the sandbox, not deleted.
	putSyncFile(t, filepath.Join(root, ".gitignore"), "out/\n", 0o644)
	if rep := mustPass(t, sy); len(rep.DeletedRemote) != 0 {
		t.Errorf("deleted %v in the sandbox: an ignored path is not a deleted one", rep.DeletedRemote)
	}
	if _, err := os.Stat(filepath.Join(box, "out/app.bin")); err != nil {
		t.Errorf("the sandbox copy of an ignored file went: %v", err)
	}
}

func TestSyncGateway_TheSandboxNeverWritesTheLaptopsRules(t *testing.T) {
	g := newSyncRig(t)
	box := g.mkSandboxDir(syncRemote)
	sy, _, root := g.syncer(syncRemote, true)
	sy.Opts.Trash = t.TempDir()
	putSyncFile(t, filepath.Join(root, ".gitignore"), ".env\n", 0o644)
	putSyncFile(t, filepath.Join(root, ".env"), "SECRET=1", 0o644)
	putSyncFile(t, filepath.Join(root, "main.go"), "package main", 0o644)
	putSyncFile(t, filepath.Join(box, ".wardynignore"), "!.env\n", 0o644)
	putSyncFile(t, filepath.Join(box, "sub/.WardynIgnore"), "!*\n", 0o644)
	refusedIgnoreFile := func(rep *foldersync.Report, rel string) bool {
		return slices.Contains(rep.Refused, foldersync.Refusal{Path: rel, Reason: "ignore file"})
	}

	rep := mustPass(t, sy)
	if !refusedIgnoreFile(rep, ".wardynignore") || !refusedIgnoreFile(rep, "sub/.WardynIgnore") {
		t.Errorf("the sandbox's ignore files were not refused: %+v", rep.Refused)
	}
	for _, p := range []string{".wardynignore", "sub"} {
		if _, err := os.Stat(filepath.Join(root, p)); err == nil {
			t.Errorf("%s was pulled to the laptop", p)
		}
	}
	mustPass(t, sy)
	if _, err := os.Stat(filepath.Join(box, ".env")); err == nil {
		t.Fatal("a sandbox-written .wardynignore re-included the laptop's git-ignored .env")
	}

	// The sandbox rewrites the laptop's .gitignore, then deletes it under --pull-deletes.
	putSyncFile(t, filepath.Join(box, ".gitignore"), "", 0o644)
	later := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(box, ".gitignore"), later, later)
	if rep := mustPass(t, sy); !refusedIgnoreFile(rep, ".gitignore") || getSyncFile(t, filepath.Join(root, ".gitignore")) != ".env\n" {
		t.Errorf("the sandbox replaced the laptop's .gitignore: refused %+v", rep.Refused)
	}
	if err := os.Remove(filepath.Join(box, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	sy.Opts.PullDeletes, sy.Opts.MaxPullDeletes = true, 20
	if rep := mustPass(t, sy); !refusedIgnoreFile(rep, ".gitignore") || len(rep.DeletedLocal) != 0 {
		t.Errorf("a sandbox delete removed the laptop's .gitignore: deleted %v, refused %+v", rep.DeletedLocal, rep.Refused)
	}
	if _, err := os.Stat(filepath.Join(box, ".env")); err == nil {
		t.Error(".env reached the sandbox")
	}
}
