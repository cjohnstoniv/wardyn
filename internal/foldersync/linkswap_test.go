// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pkg/sftp"
)

// barrier runs fn the first time the sandbox's sftp server reaches it: a point inside a pass where the
// test changes the laptop, with the pass waiting on the sandbox's answer.
type barrier struct {
	once sync.Once
	fn   func()
}

func (b *barrier) reach() {
	if b != nil {
		b.once.Do(b.fn)
	}
}

// listBarrier reaches its barrier at the pass's first sandbox listing: after the laptop's walk, before
// any file is sent.
type listBarrier struct {
	sftp.FileLister
	b *barrier
}

func (l listBarrier) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	if r.Method == "List" {
		l.b.reach()
	}
	return l.FileLister.Filelist(r)
}

// readBarrier reaches its barrier when a sandbox file is opened to be brought back: after the laptop's
// checks of its path, before the laptop's temp file is made.
type readBarrier struct {
	sftp.FileReader
	b *barrier
}

func (r readBarrier) Fileread(req *sftp.Request) (io.ReaderAt, error) {
	r.b.reach()
	return r.FileReader.Fileread(req)
}

// memClient is a client of an sftp server over h.
func memClient(t *testing.T, h sftp.Handlers) *sftp.Client {
	t.Helper()
	toServer, fromClient := io.Pipe()
	toClient, fromServer := io.Pipe()
	srv := sftp.NewRequestServer(struct {
		io.Reader
		io.WriteCloser
	}{toServer, fromServer}, h)
	go func() { _ = srv.Serve() }()
	c, err := sftp.NewClientPipe(toClient, fromClient)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fromServer.Close(); _ = fromClient.Close(); _ = c.Close() })
	return c
}

// barrierSyncer is a --pull Syncer from root to an in-memory sandbox /box holding box (path to body),
// whose server reaches list at its first listing and read at its first file read. .aws is a credential
// folder, as the folder-only runner names it.
func barrierSyncer(t *testing.T, root string, box map[string]string, list, read *barrier) *Syncer {
	t.Helper()
	mem := sftp.InMemHandler()
	setup := memClient(t, mem)
	if err := setup.MkdirAll("/box"); err != nil {
		t.Fatal(err)
	}
	for p, body := range box {
		if err := setup.MkdirAll(path.Dir(p)); err != nil {
			t.Fatal(err)
		}
		f, err := setup.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(body))
		_ = f.Close()
	}
	hooked := mem
	hooked.FileList = listBarrier{mem.FileList, list}
	hooked.FileGet = readBarrier{mem.FileGet, read}
	lroot, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lroot.Close() })
	stateDir := t.TempDir()
	st, err := LoadState(filepath.Join(stateDir, "state.json"), root, "/box")
	if err != nil {
		t.Fatal(err)
	}
	credential := func(rel string) bool { return rel == ".aws" || strings.HasPrefix(rel, ".aws/") }
	return &Syncer{Client: memClient(t, hooked), Root: lroot, State: st, StateFile: filepath.Join(stateDir, "state.json"), StateRoot: stateDir,
		Opts: Options{Remote: "/box", Pull: true, MaxPullBytes: DefaultMaxPullBytes, Credential: credential}}
}

// linkSub replaces root's sub with a link to its .aws, keeping the real sub as sub.old.
func linkSub(t *testing.T, root string) func() {
	return func() {
		if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "sub.old")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(".aws", filepath.Join(root, "sub")); err != nil {
			t.Error(err)
		}
	}
}

// A directory the walk listed, replaced by a link to a credential folder inside the chosen one before
// its files are sent: what the pass sends is never read through the link.
func TestPass_ADirectorySwappedForALinkAfterTheWalkIsNeverReadThrough(t *testing.T) {
	root := t.TempDir()
	putSyncFile(t, filepath.Join(root, "sub", "credentials"), strings.Repeat("listed by the walk ", 4), 0o644)
	putSyncFile(t, filepath.Join(root, ".aws", "credentials"), "fixture: not a real key", 0o600)
	sy := barrierSyncer(t, root, nil, &barrier{fn: linkSub(t, root)}, nil)
	rep, err := sy.Pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if f, err := sy.Client.Open("/box/sub/credentials"); err == nil {
		b, _ := io.ReadAll(f)
		_ = f.Close()
		t.Fatalf("the sandbox got %q at sub/credentials", b)
	}
	if rep.BytesOut != 0 || !slices.ContainsFunc(rep.Refused, func(r Refusal) bool { return r.Path == "sub/credentials" }) {
		t.Errorf("report = %+v, want nothing sent and sub/credentials refused", rep)
	}
}

// The same swap before a sandbox file is brought back: nothing is written into the credential folder.
func TestPass_ADirectorySwappedForALinkAfterItsCheckIsNeverWrittenThrough(t *testing.T) {
	root := t.TempDir()
	putSyncFile(t, filepath.Join(root, "sub", "keep.txt"), "the laptop's", 0o644)
	putSyncFile(t, filepath.Join(root, ".aws", "credentials"), "fixture: not a real key", 0o600)
	sy := barrierSyncer(t, root, map[string]string{"/box/sub/new.txt": "from the sandbox, longer"}, nil, &barrier{fn: linkSub(t, root)})
	rep, err := sy.Pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(rep.Refused, func(r Refusal) bool { return r.Path == "sub/new.txt" }) {
		t.Errorf("report = %+v, want sub/new.txt refused", rep)
	}
	ents, err := os.ReadDir(filepath.Join(root, ".aws"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"credentials"}) {
		t.Fatalf("the credential folder holds %v after the pass, want credentials alone", names)
	}
}

// openTestRoot is dir opened as an os.Root, closed when the test ends.
func openTestRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// A directory replaced by a link to a credential folder between openChild's look at it and its open:
// the open is refused, never made through the link.
func TestOpenChild_ADirectorySwappedForALinkAfterItsLookIsRefused(t *testing.T) {
	dir := t.TempDir()
	putSyncFile(t, filepath.Join(dir, "sub", "keep.txt"), "the laptop's", 0o644)
	putSyncFile(t, filepath.Join(dir, ".aws", "credentials"), "fixture: not a real key", 0o600)
	var once sync.Once
	old := childLooked
	childLooked = func() { once.Do(linkSub(t, dir)) }
	t.Cleanup(func() { childLooked = old })
	child, err := openChild(openTestRoot(t, dir), "sub")
	if err == nil {
		_ = child.Close()
		t.Fatal("openChild opened sub through the link swapped in after its look")
	}
	if !errors.Is(err, ErrLinkInPath) {
		t.Fatalf("openChild = %v, want ErrLinkInPath", err)
	}
}

// A link at a path's last name, to a credential file inside the same folder: openIn refuses it rather
// than return the file the link names.
func TestOpenIn_ALinkAtTheLastNameIsRefused(t *testing.T) {
	dir := t.TempDir()
	putSyncFile(t, filepath.Join(dir, ".aws", "credentials"), "fixture: not a real key", 0o600)
	if err := os.Symlink(filepath.Join(".aws", "credentials"), filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	f, err := openIn(openTestRoot(t, dir), "notes.txt", os.O_RDONLY, 0)
	if err == nil {
		b, _ := io.ReadAll(f)
		_ = f.Close()
		t.Fatalf("openIn read %q through the link at notes.txt", b)
	}
	if !errors.Is(err, ErrLinkInPath) {
		t.Fatalf("openIn = %v, want ErrLinkInPath", err)
	}
}

// Every laptop path the engine touches goes through nofollow.go's helpers: no other file calls an
// os.Root method by a path, which would follow a link inside the folder. The scan reads every
// system's files, so a Windows-only call is caught on Linux too.
func TestEngine_EveryRootCallGoesThroughTheNoFollowHelpers(t *testing.T) {
	direct := regexp.MustCompile(`(sy\.Root|\broot)\.(Open|OpenFile|OpenRoot|Lstat|Stat|Remove|RemoveAll|Rename|Link|Chtimes|Chmod|Mkdir|MkdirAll|ReadFile|WriteFile|Create|FS|Symlink|Readlink)\(`)
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no Go files found: %v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "nofollow.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if direct.MatchString(line) {
				t.Errorf("%s:%d calls os.Root by a path: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
