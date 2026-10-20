// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pkg/sftp"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

func TestSyncAbortIsOnlyForAConnectionOrAnInterrupt(t *testing.T) {
	ctx := context.Background()
	if Abort(ctx, os.ErrPermission) || Abort(ctx, &os.PathError{Op: "open", Err: os.ErrExist}) {
		t.Error("a per-path error aborts the pass")
	}
	if !Abort(ctx, errors.New("connection lost")) {
		t.Error("an unknown error is not treated as the connection going away")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if !Abort(cancelled, os.ErrPermission) {
		t.Error("an interrupt does not abort")
	}
}

func TestSyncFoldAndInvisibleNames(t *testing.T) {
	if syncFold("cafe\u0301.txt") != syncFold("caf\u00e9.TXT") {
		t.Error("NFD and NFC spellings with different case have different keys")
	}
	if syncFold("ǅ") != syncFold("ǆ") || syncFold("Straße") != syncFold("STRASSE") {
		t.Error("full case folding is not applied")
	}
	for _, name := range []string{".g\u200cit", ".git\u200d", "a\u00adb", "x\ufe0f", "\u2060a", "a\u034fb"} {
		if !syncInvisible(name) || syncEntryName(name) == nil {
			t.Errorf("%q is not refused as invisible", name)
		}
	}
	for _, name := range []string{"plain", "caf\u00e9", "日本語", ".git"} {
		if syncInvisible(name) {
			t.Errorf("%q is refused as invisible", name)
		}
	}
	if !syncDenied(".GIT/config") || !syncDenied("a/.vscode/TASKS.json") {
		t.Error("folded denylist match failed")
	}
	got := syncCollisions([]string{"dir/cafe\u0301.txt", "dir/caf\u00e9.txt", "other"})
	if len(got) != 2 {
		t.Errorf("NFD/NFC spellings are not a collision: %v", got)
	}
}

func TestSync_PullRefusesALocalFileTheWalkNeverListed(t *testing.T) {
	sy, box, root := pipeSyncer(t, true)
	putSyncFile(t, filepath.Join(root, "x.txt"), "local", 0o644)
	putSyncFile(t, filepath.Join(box, "x.txt"), "remote", 0o644)
	fi, err := sy.Client.Stat(box + "/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	rep := NewReport()
	pulled := int64(0)
	if err := sy.pull(context.Background(), "x.txt", fi, nil, rep, &pulled); err != nil {
		t.Fatal(err)
	}
	if got := getSyncFile(t, filepath.Join(root, "x.txt")); got != "local" || len(rep.Pulled) != 0 || len(rep.Refused) != 1 {
		t.Errorf("file = %q, report = %+v", got, rep)
	}
}

func TestSync_PullDoesNotOverwriteAFileSavedAfterTheWalk(t *testing.T) {
	sy, box, root := pipeSyncer(t, true)
	putSyncFile(t, filepath.Join(root, "x.txt"), "saved after the walk", 0o644)
	putSyncFile(t, filepath.Join(box, "x.txt"), "remote", 0o644)
	fi, _ := sy.Client.Stat(box + "/x.txt")
	stale := &syncFile{side: Side{Size: 3, MTime: 1}, mode: 0o644}
	rep := NewReport()
	pulled := int64(0)
	if err := sy.pull(context.Background(), "x.txt", fi, stale, rep, &pulled); err != nil {
		t.Fatal(err)
	}
	if got := getSyncFile(t, filepath.Join(root, "x.txt")); got != "saved after the walk" || !slices.Equal(rep.Conflicts, []string{"x.txt"}) || len(rep.Pulled) != 0 {
		t.Errorf("file = %q, report = %+v", got, rep)
	}
	if left, _ := os.ReadDir(root); len(left) != 1 {
		t.Errorf("a temp file was left behind: %v", left)
	}
}

func TestSync_LocalWritesStayInsideTheRoot(t *testing.T) {
	sy, _, root := pipeSyncer(t, true)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "swapped")); err != nil {
		t.Fatal(err)
	}
	if _, f, err := sy.localTemp("swapped/x"); err == nil {
		_ = f.Close()
		t.Error("a temp file was created through a symlink that leaves the root")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("something was written outside the root: %v", ents)
	}
	if err := sy.Root.MkdirAll("swapped/deeper", 0o755); err == nil {
		t.Error("a directory was created through a symlink that leaves the root")
	}
}

// A pass with Credential set leaves a credential folder inside the synced root out both ways: the
// laptop's .ssh/id_x and .aws/credentials are never seeded, and the sandbox's .ssh/authorized_keys
// is never written to the laptop.
func TestSync_CredentialFoldersCrossNeitherWay(t *testing.T) {
	sy, box, root := pipeSyncer(t, true)
	sy.Opts.Credential = func(rel string) bool { return runner.DeniedUserSegment(rel) != "" }
	putSyncFile(t, filepath.Join(root, "main.go"), "package main\n", 0o644)
	putSyncFile(t, filepath.Join(root, ".ssh", "id_x"), "PRIVATE KEY", 0o600)
	putSyncFile(t, filepath.Join(root, "deploy", ".aws", "credentials"), "fixture: not a real key", 0o600)
	putSyncFile(t, filepath.Join(box, ".ssh", "authorized_keys"), "ssh-ed25519 AAAA attacker", 0o644)
	rep, err := sy.Pass(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(box, ".ssh", "id_x"), filepath.Join(box, "deploy", ".aws"), filepath.Join(root, ".ssh", "authorized_keys")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s crossed: report %+v", p, rep)
		}
	}
	if got := getSyncFile(t, filepath.Join(box, "main.go")); got != "package main\n" {
		t.Errorf("main.go in the sandbox = %q", got)
	}
	var credential int
	for _, r := range rep.Refused {
		if r.Reason == ReasonCredential {
			credential++
		}
	}
	if credential != 2 { // .ssh (one row for both sides) and deploy/.aws
		t.Errorf("credential refusals = %d, want 2: %+v", credential, rep.Refused)
	}
}

// pipeSyncer is a Syncer over an in-process sftp server whose sandbox
// directory is box, a temp directory: the engine with no gateway.
func pipeSyncer(t *testing.T, pull bool) (sy *Syncer, box, root string) {
	t.Helper()
	box, root = t.TempDir(), t.TempDir()
	toServer, fromClient := io.Pipe()
	toClient, fromServer := io.Pipe()
	srv, err := sftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{toServer, fromServer})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve() }()
	c, err := sftp.NewClientPipe(toClient, fromClient)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fromServer.Close(); _ = fromClient.Close(); _ = c.Close() })
	lroot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lroot.Close() })
	st, err := LoadState(filepath.Join(t.TempDir(), "state.json"), root, box)
	if err != nil {
		t.Fatal(err)
	}
	return &Syncer{Client: c, Root: lroot, State: st, StateFile: filepath.Join(t.TempDir(), "state.json"),
		Opts: Options{Remote: box, Pull: pull, MaxPullBytes: DefaultMaxPullBytes}}, box, root
}

func putSyncFile(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func getSyncFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
