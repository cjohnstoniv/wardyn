// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package folders

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/foldersync"
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

// A root swapped for a link to a credential folder after its check, before it is opened: the runner
// refuses to start rather than keep that folder open as its root.
func TestNew_ARootSwappedForALinkAfterItsCheckIsRefused(t *testing.T) {
	h := home(t)
	shared := filepath.Join(h, "shared")
	put(t, filepath.Join(shared, "sso", "notes.txt"), "n")
	put(t, filepath.Join(h, ".aws", "sso", "cache.json"), "fixture: not a real token")
	old := rootChecked
	rootChecked = func() { swapForLink(t, shared, ".aws") }
	t.Cleanup(func() { rootChecked = old })
	if s, err := New(context.Background(), []string{shared}, t.TempDir()); err == nil {
		s.closeRoots()
		t.Fatal("New kept a root that was swapped for a link after its check")
	}
}

// holdsOpen reports whether this process holds a descriptor on dir. It skips the test where
// /proc/self/fd is absent.
func holdsOpen(t *testing.T, dir string) bool {
	t.Helper()
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("needs Linux /proc/self/fd")
	}
	for _, fd := range names(t, "/proc/self/fd") {
		if open, _ := os.Readlink(filepath.Join("/proc/self/fd", fd)); open == dir {
			return true
		}
	}
	return false
}

// A root refused after an earlier root was opened: New closes the roots it opened before it.
func TestNew_ARefusedRootClosesTheRootsOpenedBeforeIt(t *testing.T) {
	h := home(t)
	for name, bad := range map[string]string{
		"not absolute":    "relative/root",
		"missing":         filepath.Join(h, "missing"),
		"not a directory": filepath.Join(h, "code", "notes.txt"),
	} {
		if _, err := New(context.Background(), []string{h, bad}, t.TempDir()); err == nil {
			t.Fatalf("%s: New accepted the root %q", name, bad)
		}
		if holdsOpen(t, h) {
			t.Fatalf("%s: New left the first root open after it refused the second", name)
		}
	}
}

// A root below a folder the runner's user can pass through but not read: the runner refuses to start,
// naming that folder and what to do.
func TestNew_ARootBelowAFolderItCannotReadNamesThatFolder(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix folder modes and a user they apply to")
	}
	h := home(t)
	locked := filepath.Join(h, "locked")
	r := filepath.Join(locked, "inner", "root")
	if err := os.MkdirAll(r, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o311); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	want := "the runner's user cannot read the folder " + locked + "; the runner opens each folder from the top of the drive down to a root, so make that folder readable, or choose another root"
	s, err := New(context.Background(), []string{r}, t.TempDir())
	if err == nil {
		s.closeRoots()
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("New = %v, want a refusal containing %q", err, want)
	}
}

// A folder's parent replaced by a link to a credential folder inside the root after the folder's path
// is checked, before it is opened: the browse is refused, never opened through the link.
func TestList_AFolderSwappedBeforeItIsOpenedIsRefused(t *testing.T) {
	h := home(t)
	put(t, filepath.Join(h, ".aws", "app", "sso", "cache.json"), "fixture: not a real token")
	old := folderChecked
	folderChecked = func() { swapForLink(t, filepath.Join(h, "code"), ".aws") }
	t.Cleanup(func() { folderChecked = old })
	got, err := newServer(t, h).list(context.Background(), runnerwire.ListFoldersArgs{Path: filepath.Join(h, "code", "app")})
	if !refused(err) {
		t.Fatalf("listing = %+v, %v; want the browse refused", got.Folders, err)
	}
}

// A folder that is a link by the time a browse counts it is not walked: count looks at the name and
// never follows it.
func TestCount_ALinkIsNeverWalked(t *testing.T) {
	h := home(t)
	if err := os.Symlink("app", filepath.Join(h, "code", "inside")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Join(h, "code"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if files, bytes, _ := count(root, "inside", time.Now().Add(time.Minute)); files != 0 || bytes != 0 {
		t.Fatalf("count walked the link: %d files, %d bytes", files, bytes)
	}
}

// A pass refused after its folder was opened closes that folder.
func TestStart_ARefusedPassClosesItsFolder(t *testing.T) {
	h := home(t)
	app := filepath.Join(h, "code", "app")
	_, err := newServer(t, h).start(context.Background(), nil, 0, runnerwire.SyncFolderArgs{RunID: uuid.New(), Path: app, RemoteDir: "relative"})
	if !refused(err) {
		t.Fatalf("start = %v, want a refusal of the remote folder", err)
	}
	if holdsOpen(t, app) {
		t.Fatal("a refused pass left its folder open")
	}
}

// deadStream is a pass stream on which nothing answers.
type deadStream struct{}

func (deadStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (deadStream) Write(b []byte) (int, error) { return len(b), nil }
func (deadStream) Close() error                { return nil }
func (deadStream) CloseWrite() error           { return nil }

// A pass that ends, here because the sandbox never answers, closes its folder.
func TestRun_APassThatEndsClosesItsFolder(t *testing.T) {
	h := home(t)
	app := filepath.Join(h, "code", "app")
	s := newServer(t, h)
	root, err := s.folder(app)
	if err != nil {
		t.Fatal(err)
	}
	res := s.run(deadStream{}, root, "/home/agent/work", &foldersync.State{}, filepath.Join(t.TempDir(), "state.json"), runnerwire.SyncFolderArgs{RunID: uuid.New()}, time.Time{})
	if res.Error == "" {
		t.Fatalf("pass = %+v, want it failed on the silent sandbox", res)
	}
	if holdsOpen(t, app) {
		t.Fatal("a pass that ended left its folder open")
	}
}
