// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package folders

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// peerPair is a runner peer whose org side accepts every stream the runner opens and hands it over.
func peerPair(t *testing.T) (*runnerwire.Peer, <-chan *runnerwire.Stream) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	org, run := runnerwire.Loopback()
	streams := make(chan *runnerwire.Stream, 1)
	orgPeer := runnerwire.NewPeer(org, runnerwire.PeerConfig{Org: true, OnOpen: func(s *runnerwire.Stream, _ runnerwire.Open) error {
		streams <- s
		return nil
	}})
	runPeer := runnerwire.NewPeer(run, runnerwire.PeerConfig{})
	go func() { _ = orgPeer.Run(ctx) }()
	go func() { _ = runPeer.Run(ctx) }()
	t.Cleanup(func() { orgPeer.Close(); runPeer.Close() })
	return runPeer, streams
}

// swapForLink moves dir to dir.approved and puts a link to target in its place.
func swapForLink(t *testing.T, dir, target string) {
	t.Helper()
	if err := os.Rename(dir, dir+".approved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// The chosen folder is swapped for a link to a folder outside the roots after its check, while the
// pass waits for the sandbox's sftp handshake: the pass reads and writes the folder that was checked,
// and nothing outside.
func TestStart_AFolderSwappedBeforeTheSandboxAnswersIsNeverFollowed(t *testing.T) {
	h := home(t)
	app := filepath.Join(h, "code", "app")
	outside := t.TempDir()
	put(t, filepath.Join(outside, "outside.txt"), "outside bytes")
	s := newServer(t, h)
	peer, streams := peerPair(t)
	sb := newSandbox(t)
	f, err := sb.client(t, "/").Create("/home/agent/work/back.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte("from the sandbox"))
	_ = f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := s.start(ctx, peer, 0, runnerwire.SyncFolderArgs{RunID: uuid.New(), Path: app, RemoteDir: "/home/agent/work", Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	stream := <-streams
	// The pass now waits on the handshake: nothing has answered on the stream yet.
	swapForLink(t, app, outside)
	srv := sb.serve(stream, "/home/agent/work")
	defer srv.Close()
	pass, err := s.wait(ctx, res.(runnerwire.StreamResult).Stream)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sb.client(t, "/").Stat("/home/agent/work/outside.txt"); err == nil {
		t.Error("a file from outside the roots reached the sandbox")
	}
	if got := names(t, outside); !slices.Equal(got, []string{"outside.txt"}) {
		t.Errorf("outside the roots now holds %v, want outside.txt alone", got)
	}
	if b, err := os.ReadFile(filepath.Join(app+".approved", "back.txt")); err != nil || string(b) != "from the sandbox" || pass.Error != "" || pass.Pushed != 2 {
		t.Errorf("pass = %+v, back.txt in the checked folder = %q, %v; want it pulled there and main.go and README.md sent", pass, b, err)
	}
}

// The folder browsed is swapped for a link to a folder outside the roots after its check: the listing
// and survey are of the folder that was checked.
func TestList_AFolderSwappedAfterItsCheckIsNeverFollowed(t *testing.T) {
	h := home(t)
	app := filepath.Join(h, "code", "app")
	outside := t.TempDir()
	put(t, filepath.Join(outside, "stolen", "a.txt"), "a")
	put(t, filepath.Join(outside, "stolen", "b.txt"), "b")
	put(t, filepath.Join(outside, "outside.txt"), "outside bytes")
	old := listChecked
	listChecked = func() { swapForLink(t, app, outside) }
	t.Cleanup(func() { listChecked = old })
	got, err := newServer(t, h).list(context.Background(), runnerwire.ListFoldersArgs{Path: app, Survey: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Folders) != 0 || got.Survey == nil || got.Survey.Files != 2 {
		t.Fatalf("listing = %+v, survey %+v; want the checked folder's two files and no folder from outside", got.Folders, got.Survey)
	}
}
