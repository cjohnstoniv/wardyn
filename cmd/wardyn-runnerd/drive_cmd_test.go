// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent/guestagenttest"
)

const (
	driveOpenTestObject = "wardyn-drive-0123456789abcdef0123456789abcdef-d-0123456789abcdef"
	// driveOpenTestRecord is the name file the driver's macDiskDrives writes for the drive named Notes; its test
	// pins the same bytes.
	driveOpenTestRecord = `{"label":"Wardyn-1a2b3c4d","name":"Notes"}`
)

// driveOpenGuestCall is one Exec a fake guest served.
type driveOpenGuestCall struct {
	argv  []string
	stdin []byte
}

// driveOpenGuest serves the guest agent's /v1/exec: tar -c answers listing, anything else reads its stdin to
// the end and exits 0, and every call is recorded in the order it finished.
type driveOpenGuest struct {
	mu      sync.Mutex
	calls   []driveOpenGuestCall
	listing []byte
	tarExit int
	tarErr  string
	during  func(argv []string) // called as a call starts
	// failing maps a program to the stderr it writes before it exits 1; tar -cf has tarExit and tarErr.
	failing map[string]string
}

func (g *driveOpenGuest) handle(w http.ResponseWriter, r *http.Request) {
	_, spec, err := guestagent.ReadFrame(r.Body)
	var req guestagent.ExecRequest
	if err != nil || json.Unmarshal(spec, &req) != nil {
		return
	}
	w.WriteHeader(http.StatusOK)
	flush := w.(http.Flusher).Flush
	flush()
	if g.during != nil {
		g.during(req.Argv)
	}
	call := driveOpenGuestCall{argv: req.Argv}
	code := 0
	if len(req.Argv) > 3 && req.Argv[3] == "-cf" {
		for rest := g.listing; len(rest) > 0; {
			n := min(len(rest), guestagent.MaxFrame)
			_ = guestagent.WriteFrame(w, guestagent.FrameStdout, rest[:n])
			rest = rest[n:]
		}
		if g.tarErr != "" {
			for b := []byte(g.tarErr); len(b) > 0; b = b[min(len(b), 16<<10):] {
				_ = guestagent.WriteFrame(w, guestagent.FrameStderr, b[:min(len(b), 16<<10)])
			}
		}
		code = g.tarExit
	} else if text, bad := g.failing[req.Argv[0]]; bad {
		_ = guestagent.WriteFrame(w, guestagent.FrameStderr, []byte(text))
		code = 1
	} else {
		for {
			typ, payload, err := guestagent.ReadFrame(r.Body)
			if err != nil || typ == guestagent.FrameStdinClose {
				break
			}
			call.stdin = append(call.stdin, payload...)
		}
	}
	g.mu.Lock()
	g.calls = append(g.calls, call)
	g.mu.Unlock()
	exit := make([]byte, 4)
	binary.BigEndian.PutUint32(exit, uint32(int32(code)))
	_ = guestagent.WriteFrame(w, guestagent.FrameExit, exit)
}

// serve runs the guest on ln.
func (g *driveOpenGuest) serve(t *testing.T, ln net.Listener) {
	t.Helper()
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	hs := &http.Server{Protocols: &protocols, Handler: http.HandlerFunc(g.handle)}
	go func() { _ = hs.Serve(guestagenttest.Listener(ln)) }()
	t.Cleanup(func() { _ = hs.Close() })
}

// call is the first recorded call whose argv[0] is prog (nil: none).
func (g *driveOpenGuest) call(prog string, flag string) *driveOpenGuestCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, c := range g.calls {
		if c.argv[0] == prog && (flag == "" || strings.Contains(strings.Join(c.argv, " "), flag)) {
			return &g.calls[i]
		}
	}
	return nil
}

// driveOpenAgent is a client of a fake guest on a loopback port.
func driveOpenAgent(t *testing.T, g *driveOpenGuest) *guestagent.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g.serve(t, ln)
	c := guestagent.NewClient(func(context.Context) (net.Conn, error) { return net.Dial("tcp", ln.Addr().String()) })
	t.Cleanup(c.Close)
	return c
}

// driveOpenKeys is the person's terminal: the first read runs before, then answers Return (or, with ctrlC,
// cancels and then waits for the test to end).
type driveOpenKeys struct {
	before func()
	ctrlC  context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (k *driveOpenKeys) Read(p []byte) (int, error) {
	first := false
	k.once.Do(func() { first = true })
	if !first {
		return 0, io.EOF
	}
	if k.before != nil {
		k.before()
	}
	if k.ctrlC != nil {
		k.ctrlC()
		<-k.done
		return 0, io.EOF
	}
	return copy(p, "\n"), nil
}

// driveOpenJobIn is a job in a fresh folder, on a fake guest that lists entries.
func driveOpenJobIn(t *testing.T, entries ...driveOpenEntry) (driveOpenJob, *driveOpenGuest, string, *bytes.Buffer) {
	t.Helper()
	root, dir, _ := driveOpenRoot(t)
	g := &driveOpenGuest{listing: driveOpenStream(t, entries...)}
	out := &bytes.Buffer{}
	return driveOpenJob{agent: driveOpenAgent(t, g), dest: &driveOpenDest{root: root, parent: root, shown: dir}, limits: driveOpenRoomy,
		mark: func(*os.File) error { return nil }, out: out}, g, dir, out
}

func driveOpenSentNames(t *testing.T, stdin []byte) map[string]string {
	t.Helper()
	sent := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(stdin))
	for {
		h, err := tr.Next()
		if err != nil {
			return sent
		}
		b, _ := io.ReadAll(tr)
		sent[h.Name] = string(b)
	}
}

// Return writes what changed back through Exec as one tar x, a link in the folder is skipped and said, and
// the guest is synced after the write, last.
func TestDriveOpenWorkWritesBackThroughExec(t *testing.T) {
	j, g, dir, out := driveOpenJobIn(t,
		driveOpenEntry{name: "./a.txt", body: "alpha", mode: 0o644},
		driveOpenEntry{name: "./b.txt", body: "beta", mode: 0o644},
		driveOpenEntry{name: "./l", typeflag: tar.TypeSymlink, link: "/etc/passwd"})
	j.in = &driveOpenKeys{before: func() {
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha two"), 0o644); err != nil {
			t.Error(err)
		}
		if err := os.Symlink("/etc/passwd", filepath.Join(dir, "host-link")); err != nil {
			t.Error(err)
		}
	}}
	if _, err := driveOpenWork(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	if sent := driveOpenSentNames(t, g.call("/usr/bin/tar", "-xf").stdin); len(sent) != 1 || sent["a.txt"] != "alpha two" {
		t.Fatalf("sent %v", sent)
	}
	if last := g.calls[len(g.calls)-1].argv[0]; last != "/bin/sync" || len(g.calls) != 3 {
		t.Fatalf("calls %v; sync must be last", g.calls)
	}
	for _, want := range []string{"Copied 2 files (9 B) to " + dir + ". Skipped 1: 1 link.", "Every file is marked as downloaded and has no execute permission.",
		"press Return to write them back, or press Ctrl-C to discard your changes.", "Wrote back 1 changed file. The next run that uses this drive sees them, owned by the agent. Skipped 1: 1 link."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

// driveOpenOwnDest makes the job's folder one this run created inside a parent, as drive open does, so that
// removing it can be seen.
func driveOpenOwnDest(t *testing.T, j *driveOpenJob, dir string) {
	t.Helper()
	parent, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Close() })
	j.dest.parent, j.dest.leaf, j.dest.created = parent, filepath.Base(dir), true
	j.dest.trash = filepath.Join(t.TempDir(), ".Trash")
}

// driveOpenTrashed is the files in the one folder the Trash holds, by name under it ("" for the Trash itself).
func driveOpenTrashed(t *testing.T, trash string) map[string]string {
	t.Helper()
	moved, err := os.ReadDir(trash)
	if err != nil || len(moved) != 1 {
		t.Fatalf("the Trash holds %v, %v", moved, err)
	}
	got := map[string]string{"": moved[0].Name()}
	_ = filepath.WalkDir(filepath.Join(trash, moved[0].Name()), func(p string, e fs.DirEntry, err error) error {
		if b, rerr := os.ReadFile(p); err == nil && !e.IsDir() && rerr == nil {
			got[strings.TrimPrefix(p, filepath.Join(trash, moved[0].Name())+"/")] = string(b)
		}
		return nil
	})
	return got
}

func driveOpenGone(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s is still there (%v)", dir, err)
	}
}

// Ctrl-C before Return, and an input that ends, write nothing: no tar x, no delete. The copy goes to the Trash, and
// the guest is still synced (opening the drive replayed its journal).
func TestDriveOpenWorkCtrlCWritesNothing(t *testing.T) {
	for _, name := range []string{"ctrl-c", "input ends"} {
		t.Run(name, func(t *testing.T) {
			j, g, dir, out := driveOpenJobIn(t, driveOpenEntry{name: "./a.txt", body: "alpha", mode: 0o644})
			j.deleteGone = true
			driveOpenOwnDest(t, &j, dir)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			keys := &driveOpenKeys{done: make(chan struct{}), before: func() { _ = os.Remove(filepath.Join(dir, "a.txt")) }}
			if name == "ctrl-c" {
				keys.ctrlC = cancel
				t.Cleanup(func() { close(keys.done) })
			} else {
				j.in = strings.NewReader("")
				keys.before = nil
			}
			if j.in == nil {
				j.in = keys
			}
			if _, err := driveOpenWork(ctx, j); err != nil {
				t.Fatal(err)
			}
			if g.call("/usr/bin/tar", "-xf") != nil || g.call("/bin/sh", "") != nil {
				t.Fatalf("the guest was written: %v", g.calls)
			}
			if g.call("/bin/sync", "") == nil || !strings.Contains(out.String(), "Discarded: nothing was written back.") {
				t.Fatalf("calls %v\n%s", g.calls, out)
			}
			driveOpenGone(t, dir)
			if got := driveOpenTrashed(t, j.dest.trash); !strings.HasPrefix(got[""], filepath.Base(dir)+" ") || got["a.txt"] != "alpha" && name == "input ends" {
				t.Fatalf("the Trash holds %v", got)
			}
			if want := "The folder " + dir + " was moved to the Trash.\n"; !strings.HasSuffix(out.String(), want) {
				t.Fatalf("the last line is not %q:\n%s", want, out)
			}
		})
	}
}

// A hostile listing leaves the folder empty, and a tar that cannot finish its stream does too.
func TestDriveOpenWorkLeavesNothingWhenTheCopyFails(t *testing.T) {
	for name, set := range map[string]func(*driveOpenGuest, *testing.T){
		"name leaves the folder": func(g *driveOpenGuest, t *testing.T) {
			g.listing = driveOpenStream(t, driveOpenEntry{name: "./ok", body: "1"}, driveOpenEntry{name: "../x", body: "2"})
		},
		"stream cut short": func(g *driveOpenGuest, _ *testing.T) { g.listing = g.listing[:512] },
	} {
		t.Run(name, func(t *testing.T) {
			j, g, dir, _ := driveOpenJobIn(t, driveOpenEntry{name: "./ok", body: "1"})
			set(g, t)
			result, err := driveOpenWork(t.Context(), j)
			if err == nil || result != driveOpenCopyFailed {
				t.Fatalf("the copy was kept: %q, %v", result, err)
			}
			if left, _ := os.ReadDir(dir); len(left) > 0 {
				t.Fatalf("left %v after %v", left, err)
			}
		})
	}
}

// One unreadable file (tar exits 1 but finishes its listing) does not discard the rest: the copy is kept, the
// person is told how many items are missing with tar's lines escaped, and write back leaves the missing
// names alone: a new file goes through tar -k, which never replaces, and --delete names only copied files.
func TestDriveOpenWorkKeepsAPartialCopy(t *testing.T) {
	j, g, dir, out := driveOpenJobIn(t, driveOpenEntry{name: "./a.txt", body: "alpha", mode: 0o644}, driveOpenEntry{name: "./b.txt", body: "beta", mode: 0o644})
	g.tarExit, g.tarErr = 1, "tar: ./secret: Cannot open: Permission denied\ntar: ./locked/\x1b[2Jx: Cannot stat\ntar: Error exit delayed from previous errors\n"
	j.deleteGone = true
	j.in = &driveOpenKeys{before: func() {
		_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha two"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "secret"), []byte("mine, not the drive's"), 0o644) // a name the copy never held
		_ = os.Remove(filepath.Join(dir, "b.txt"))
	}}
	result, err := driveOpenWork(t.Context(), j)
	if err != nil || result != driveOpenWrittenBack {
		t.Fatalf("%q, %v\n%s", result, err, out)
	}
	for _, want := range []string{"Copied 2 files", "2 items could not be read from the drive and are not in this copy.", "tar: ./secret: Cannot open: Permission denied",
		`tar: ./locked/\x1b[2Jx: Cannot stat`, "Write back will not replace, and --delete will not remove, anything this copy does not hold."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out.String(), "Error exit delayed") {
		t.Fatalf("tar's closing complaint was counted:\n%s", out)
	}
	driveOpenClean(t, "output", strings.ReplaceAll(out.String(), "\n", " "))
	known := driveOpenSentNames(t, g.call("/usr/bin/tar", "-xf").stdin)
	fresh := driveOpenSentNames(t, g.call("/usr/bin/tar", "-k").stdin)
	if len(known) != 1 || known["a.txt"] != "alpha two" || len(fresh) != 1 || fresh["secret"] == "" {
		t.Fatalf("known %v fresh %v", known, fresh)
	}
	if del := g.call("/bin/sh", ""); del == nil || string(del.stdin) != "./b.txt\x00" {
		t.Fatalf("delete = %+v", del)
	}
}

// A write back that fails says which changes did not go back.
func TestDriveOpenWorkSaysWhatDidNotGoBack(t *testing.T) {
	j, g, dir, _ := driveOpenJobIn(t, driveOpenEntry{name: "./a.txt", body: "alpha", mode: 0o644})
	g.failing = map[string]string{"/usr/bin/tar": "tar: disk full"} // the listing (-cf) is answered first; any other tar fails
	j.in = &driveOpenKeys{before: func() {
		_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0o644)
	}}
	result, err := driveOpenWork(t.Context(), j)
	if err == nil || result != driveOpenBackFailed || !strings.Contains(err.Error(), `These changes did not go back, or are not confirmed: "a.txt", "new.txt"`) {
		t.Fatalf("%q, %v", result, err)
	}
}

// Deleting a copied file deletes it from the drive only with the flag, through one sh that reads the names
// from stdin.
func TestDriveOpenWorkDeletesOnlyWithTheFlag(t *testing.T) {
	for _, flag := range []bool{false, true} {
		j, g, dir, _ := driveOpenJobIn(t, driveOpenEntry{name: "./keep", body: "1"}, driveOpenEntry{name: "./gone", body: "2"})
		j.deleteGone = flag
		j.in = &driveOpenKeys{before: func() { _ = os.Remove(filepath.Join(dir, "gone")) }}
		if _, err := driveOpenWork(t.Context(), j); err != nil {
			t.Fatal(err)
		}
		del := g.call("/bin/sh", "")
		if flag != (del != nil) || (flag && string(del.stdin) != "./gone\x00") {
			t.Fatalf("flag %v: calls %v", flag, g.calls)
		}
	}
}

// The name the person types finds the image and its volume label from the name file the driver wrote at attach.
func TestDriveOpenFindsTheDriveByItsName(t *testing.T) {
	m := newMacLedgerRig(t).open(t)
	drives := m.layout.DrivesDir()
	if err := os.MkdirAll(drives, 0o700); err != nil {
		t.Fatal(err)
	}
	put := func(object, record string, image bool) {
		t.Helper()
		if image {
			if err := os.WriteFile(filepath.Join(drives, object+".img"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(drives, object+driveOpenNameFile), []byte(record), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put(driveOpenTestObject, driveOpenTestRecord, true)
	if object, label, err := m.driveOpenFind("Notes"); err != nil || object != driveOpenTestObject || label != "Wardyn-1a2b3c4d" {
		t.Fatalf("Notes = %q %q %v", object, label, err)
	}
	if object, _, err := m.driveOpenFind(driveOpenTestObject); err != nil || object != driveOpenTestObject {
		t.Fatalf("by object name = %q %v", object, err)
	}
	if _, _, err := m.driveOpenFind("Other"); err == nil || err.Error() != `Wardyn does not know a drive named "Other" on this Mac. drive open opens only a drive that a run on this Mac has used.` {
		t.Fatalf("unknown = %v", err)
	}
	put("wardyn-drive-aaaa", `{"label":"Wardyn-aaaaaaaa","name":"Notes"}`, true)
	if _, _, err := m.driveOpenFind("Notes"); err == nil || err.Error() != `More than one drive on this Mac is named "Notes". Use one of these names instead: `+driveOpenTestObject+`, wardyn-drive-aaaa.` {
		t.Fatalf("twice = %v", err)
	}
	// A name file with no image beside it, a record with no label, and junk are not drives.
	put("wardyn-drive-bbbb", `{"label":"Wardyn-bbbbbbbb","name":"Lost"}`, false)
	put("wardyn-drive-cccc", `{"name":"NoLabel"}`, true)
	put("wardyn-drive-dddd", `not json`, true)
	for _, name := range []string{"Lost", "NoLabel"} {
		if _, _, err := m.driveOpenFind(name); err == nil {
			t.Fatalf("%s was found", name)
		}
	}
}

// The folder is ~/Wardyn Drives/<name>, made private, and a link at either step, a file, or a folder that
// holds anything is refused.
func TestDriveOpenDestination(t *testing.T) {
	m := newMacLedgerRig(t).open(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	elsewhere := t.TempDir()

	d, err := m.openDriveDest("Notes")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "Wardyn Drives", "Notes"); d.shown != want || !d.created {
		t.Fatalf("shown %q created %v", d.shown, d.created)
	}
	if fi, err := os.Stat(d.shown); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("folder %v %v", fi, err)
	}
	if err := d.root.WriteFile("x", []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.undo()
	if _, err := os.Lstat(d.shown); !os.IsNotExist(err) {
		t.Fatalf("undo left %v", err)
	}
	d.close()

	again, err := m.openDriveDest("Notes") // an empty folder that was there is used, and kept
	if err != nil {
		t.Fatal(err)
	}
	again.close()
	if again, err = m.openDriveDest("Notes"); err != nil || again.created {
		t.Fatalf("existing empty folder: %v", err)
	}
	if err := again.root.WriteFile("x", []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	again.close()
	if _, err := m.openDriveDest("Notes"); err == nil || !strings.Contains(err.Error(), "already holds files") {
		t.Fatalf("non-empty = %v", err)
	}
	for _, prepare := range []struct {
		name string
		do   func()
	}{
		{"leaf link", func() {
			_ = os.Symlink(elsewhere, filepath.Join(home, "Wardyn Drives", "Linked"))
		}},
		{"folder link", func() {
			_ = os.RemoveAll(filepath.Join(home, "Wardyn Drives"))
			_ = os.Symlink(elsewhere, filepath.Join(home, "Wardyn Drives"))
		}},
	} {
		prepare.do()
		if _, err := m.openDriveDest("Linked"); err == nil || !strings.Contains(err.Error(), "is a link") {
			t.Fatalf("%s: %v", prepare.name, err)
		}
	}
	if left, _ := os.ReadDir(elsewhere); len(left) > 0 {
		t.Fatalf("wrote through a link: %v", left)
	}
	for _, name := range []string{"", "..", strings.Repeat("n", 300)} {
		if _, err := m.openDriveDest(name); err == nil {
			t.Fatalf("name %q was accepted", name[:min(len(name), 5)])
		}
	}
	_ = os.Remove(filepath.Join(home, "Wardyn Drives"))
	if d, err := m.openDriveDest("a/b:c"); err != nil || filepath.Base(d.shown) != "a-b-c" {
		t.Fatalf("separators: %v", err)
	}
}

// driveOpenRig is a runner with a drive named Notes, a fake helper that prints its ready line for a fake guest
// on a socket and stops when its stdin ends, and a home folder.
type driveOpenRig struct {
	m    *macOSVMs
	g    *driveOpenGuest
	args string
	home string
	rig  *macLedgerRig
}

func newDriveOpenRig(t *testing.T, helperScript string, entries ...driveOpenEntry) *driveOpenRig {
	t.Helper()
	r := newMacLedgerRig(t)
	m := r.open(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	if err := os.MkdirAll(m.layout.DrivesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{driveOpenTestObject + ".img": "", driveOpenTestObject + driveOpenNameFile: driveOpenTestRecord} {
		if err := os.WriteFile(filepath.Join(m.layout.DrivesDir(), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", t.TempDir())
	short, err := os.MkdirTemp("", "dro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	sock := filepath.Join(short, "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	g := &driveOpenGuest{listing: driveOpenStream(t, entries...)}
	g.serve(t, ln)
	dr := &driveOpenRig{m: m, g: g, args: filepath.Join(short, "args"), home: os.Getenv("HOME"), rig: r}
	m.helper = filepath.Join(short, "wardyn-macvm")
	script := "#!/bin/sh\necho \"$@\" >>" + dr.args + "\n" + strings.NewReplacer("SOCK", sock, "ARGS", dr.args, "PIDFILE", dr.args+".pid").Replace(helperScript)
	if err := os.WriteFile(m.helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return dr
}

const driveOpenReadyHelper = `echo '{"agent_sock":"SOCK","target":"/Users/agent/drive"}'
cat >/dev/null
echo stopped >>ARGS
`

func (r *driveOpenRig) counted(t *testing.T) []admission.Entry {
	t.Helper()
	es, err := r.m.ledger.Counted()
	if err != nil {
		t.Fatal(err)
	}
	return es
}

// The whole command: it takes a ledger entry of its own kind, starts the helper with the drive and its label,
// copies the files to the home folder, writes back through Exec, ends the helper by closing its stdin, and
// releases the entry.
func TestDriveOpenRunsTheHelperAndReleasesItsEntry(t *testing.T) {
	r := newDriveOpenRig(t, driveOpenReadyHelper, driveOpenEntry{name: "./a.txt", body: "alpha", mode: 0o755})
	argsFile := r.args
	var out bytes.Buffer
	var during []admission.Entry
	dir := filepath.Join(r.home, "Wardyn Drives", "Notes")
	in := &driveOpenKeys{before: func() {
		during = r.counted(t)
		if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644); err != nil {
			t.Error(err)
		}
	}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := r.m.driveOpen(ctx, driveOpenArgs{name: "Notes", in: in, out: &out}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(during) != 1 || during[0].Kind != kindDriveOpen {
		t.Fatalf("entries while open: %+v", during)
	}
	if left := r.counted(t); len(left) != 0 {
		t.Fatalf("entries after: %+v", left)
	}
	b, _ := os.ReadFile(argsFile)
	for _, want := range []string{"drive-open --root " + r.m.layout.Root, "--drive " + driveOpenTestObject, "--drive-label Wardyn-1a2b3c4d", "--ledger-entry " + during[0].ID, "stopped"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("helper args lack %q:\n%s", want, b)
		}
	}
	if sent := driveOpenSentNames(t, r.g.call("/usr/bin/tar", "-xf").stdin); sent["a.txt"] != "changed" {
		t.Fatalf("sent %v", sent)
	}
	if !strings.HasPrefix(out.String(), `Starting a throwaway macOS VM with the drive "Notes". It has no internet and runs no agent. This takes about a minute.`) {
		t.Fatalf("output:\n%s", out.String())
	}
}

// A helper that refuses (a drive in use, say) answers its last stderr line, frees its entry, and leaves no folder.
func TestDriveOpenAnswersTheHelpersRefusal(t *testing.T) {
	r := newDriveOpenRig(t, "echo 'wardyn-macvm: macvm: the disk drive is attached to another run on this Mac' >&2\nexit 1\n")
	err := r.m.driveOpen(t.Context(), driveOpenArgs{name: "Notes", in: strings.NewReader(""), out: io.Discard})
	if err == nil || err.Error() != "macvm: the disk drive is attached to another run on this Mac" {
		t.Fatalf("err = %v", err)
	}
	if left := r.counted(t); len(left) != 0 {
		t.Fatalf("entries after a refusal: %+v", left)
	}
	if _, err := os.Lstat(filepath.Join(r.home, "Wardyn Drives", "Notes")); !os.IsNotExist(err) {
		t.Fatalf("a refused open left its folder: %v", err)
	}
}

// A drive open entry whose CLI died is this runner's to reclaim at the next start; a live one is not.
func TestDriveOpenReclaimsItsDeadEntries(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	admit := func(pid int, start int64) {
		t.Helper()
		if _, err := m.ledger.Admit(admission.Request{Kind: kindDriveOpen, GuestMemory: 4 * gib, DiskReserve: gib, OwnerPID: pid, OwnerStart: start}); err != nil {
			t.Fatal(err)
		}
	}
	admit(4242, 9) // not in the process table: its CLI is gone
	admit(os.Getpid(), 3)
	m.reconcileDriveOpen()
	es, err := m.ledger.Counted()
	if err != nil || len(es) != 1 || es[0].OwnerPID != os.Getpid() {
		t.Fatalf("entries = %+v, %v", es, err)
	}
}

// The Return key's wait does not block past a cancelled ctx.
func TestDriveOpenReturnStopsWithItsContext(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if driveOpenReturn(ctx, pr) {
		t.Fatal("a cancelled wait reported Return")
	}
}

// driveOpenHostile is what a compromised guest writes to try to rewrite this terminal.
const driveOpenHostile = "tar failed\n\x1b[2J\x1b[HRun this other command\r"

// driveOpenClean fails the test if text carries a byte that moves a terminal or starts a new line.
func driveOpenClean(t *testing.T, what, text string) {
	t.Helper()
	if strings.ContainsAny(text, "\x1b\r\x00\x07\x08") || strings.Contains(strings.TrimSuffix(text, "\n"), "\n") {
		t.Fatalf("%s carries a control byte or an extra line: %q", what, text)
	}
}

// Everything a guest, a stream's names or the helper can put into an error or a line is escaped: no ESC, CR
// or injected newline reaches the terminal.
func TestDriveOpenGuestTextIsEscaped(t *testing.T) {
	t.Run("tar stderr", func(t *testing.T) {
		r := newDriveOpenRig(t, driveOpenReadyHelper, driveOpenEntry{name: "./a", body: "1"})
		r.g.tarExit, r.g.tarErr = 1, driveOpenHostile
		var out bytes.Buffer
		if err := r.m.driveOpen(t.Context(), driveOpenArgs{name: "Notes", in: strings.NewReader(""), out: &out}); err != nil {
			t.Fatal(err)
		}
		// A listing that ended is a partial copy, and tar's lines are shown escaped, one line each.
		if !strings.Contains(out.String(), `  tar failed`) || !strings.Contains(out.String(), `  \x1b[2J\x1b[HRun this other command`) {
			t.Fatalf("output:\n%s", out.String())
		}
		for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
			driveOpenClean(t, "line", line)
		}
	})
	t.Run("protocol error", func(t *testing.T) {
		r := newDriveOpenRig(t, driveOpenReadyHelper, driveOpenEntry{name: "./a", body: "1"})
		r.g.listing = r.g.listing[:512]
		r.g.tarErr = driveOpenHostile
		err := r.m.driveOpen(t.Context(), driveOpenArgs{name: "Notes", in: strings.NewReader(""), out: io.Discard})
		if err == nil {
			t.Fatal("a cut listing was kept")
		}
		driveOpenClean(t, "error", err.Error())
	})
	t.Run("sync stderr", func(t *testing.T) {
		r := newDriveOpenRig(t, driveOpenReadyHelper, driveOpenEntry{name: "./a", body: "1"})
		r.g.failing = map[string]string{"/bin/sync": driveOpenHostile}
		var out bytes.Buffer
		if err := r.m.driveOpen(t.Context(), driveOpenArgs{name: "Notes", in: strings.NewReader(""), out: &out}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "did not confirm") {
			t.Fatalf("output:\n%s", out.String())
		}
		driveOpenClean(t, "output", strings.ReplaceAll(out.String(), "\n", " "))
		for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
			driveOpenClean(t, "line", line)
		}
	})
	t.Run("names in the stream", func(t *testing.T) {
		bad := "x\x1b[2Jy\nRun this"
		for name, entries := range map[string][]driveOpenEntry{
			"refused":  {{name: "../" + bad, body: "1"}},
			"repeated": {{name: "./" + bad, body: "1"}, {name: "./" + bad, body: "2"}},
		} {
			r := newDriveOpenRig(t, driveOpenReadyHelper, entries...)
			err := r.m.driveOpen(t.Context(), driveOpenArgs{name: "Notes", in: strings.NewReader(""), out: io.Discard})
			if err == nil {
				t.Fatalf("%s: copied", name)
			}
			driveOpenClean(t, name, err.Error())
		}
	})
	t.Run("helper refusal", func(t *testing.T) {
		r := newDriveOpenRig(t, "printf 'wardyn-macvm: the guest said \\033[2J\\033[Hhi\\r\\n' >&2\nexit 1\n")
		err := r.m.driveOpen(t.Context(), driveOpenArgs{name: "Notes", in: strings.NewReader(""), out: io.Discard})
		if err == nil || !strings.Contains(err.Error(), `the guest said \x1b[2J\x1b[Hhi`) {
			t.Fatalf("err = %q", err)
		}
		driveOpenClean(t, "error", err.Error())
	})
	if got := driveOpenSafe("ok text, \"quoted\" ünï \xff"); got != `ok text, "quoted" ünï \xff` {
		t.Fatalf("safe = %q", got)
	}
	if got := driveOpenSafe(strings.Repeat("a", 1000)); len(got) != 603 {
		t.Fatalf("not bounded: %d", len(got))
	}
}

// Two drive opens of one name cannot meet: the second is refused at the destination's lock, before it boots
// anything, and the first's files stay; the lock goes with the first.
func TestDriveOpenSecondOfTheSameNameIsRefusedAndTouchesNothing(t *testing.T) {
	r := newDriveOpenRig(t, driveOpenReadyHelper, driveOpenEntry{name: "./a", body: "1"})
	first, err := r.m.openDriveDest("Notes")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.root.WriteFile("a.txt", []byte("the first's copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = r.m.driveOpen(t.Context(), driveOpenArgs{name: "Notes", in: strings.NewReader(""), out: io.Discard, deleteGone: true})
	if err == nil || !strings.Contains(err.Error(), "Another drive open of \"Notes\" is running") {
		t.Fatalf("second = %v", err)
	}
	if b, err := first.root.ReadFile("a.txt"); err != nil || string(b) != "the first's copy" {
		t.Fatalf("the first's file: %q, %v", b, err)
	}
	if left := r.counted(t); len(left) != 0 || r.g.call("/usr/bin/tar", "") != nil {
		t.Fatalf("the second started something: %+v %v", left, r.g.calls)
	}
	first.close()
	// The lock is gone: the next one reaches the folder check, which refuses the first's files.
	if _, err := r.m.openDriveDest("Notes"); err == nil || !strings.Contains(err.Error(), "already holds files") {
		t.Fatalf("after the first closed: %v", err)
	}
}

// After the first Ctrl-C the helper's stop waits for the VM; a second Ctrl-C (hard) ends that wait, and the
// command returns while the helper, its stdin closed, stops on its own.
func TestDriveOpenSecondCtrlCEndsTheWaits(t *testing.T) {
	r := newDriveOpenRig(t, "echo $$ >PIDFILE\necho '{\"agent_sock\":\"SOCK\",\"target\":\"/Users/agent/drive\"}'\nexec sleep 60\n", driveOpenEntry{name: "./a", body: "1"})
	t.Cleanup(func() { // by the recorded pid only
		if b, err := os.ReadFile(r.args + ".pid"); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	soft, ctrlC := context.WithCancel(t.Context())
	hard, again := context.WithCancel(context.WithoutCancel(t.Context()))
	defer ctrlC()
	defer again()
	keys := &driveOpenKeys{ctrlC: ctrlC, done: make(chan struct{}), before: func() { time.AfterFunc(300*time.Millisecond, again) }}
	t.Cleanup(func() { close(keys.done) })
	start := time.Now()
	var out bytes.Buffer
	if err := r.m.driveOpen(soft, driveOpenArgs{name: "Notes", in: keys, out: &out, hard: hard}); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("took %v; the helper's two-minute stop wait was not ended", took)
	}
	if !strings.Contains(out.String(), "Discarded: nothing was written back.") || r.g.call("/usr/bin/tar", "-xf") != nil {
		t.Fatalf("output:\n%s", out.String())
	}
}

// The CLI logs one line when the drive opens and one when it closes, with the drive and the result, in the VM
// directory beside the helper's log.
func TestDriveOpenLogsOpenAndClose(t *testing.T) {
	r := newDriveOpenRig(t, driveOpenReadyHelper, driveOpenEntry{name: "./a", body: "1"})
	if err := r.m.driveOpen(t.Context(), driveOpenArgs{name: "Notes", in: strings.NewReader(""), out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(r.m.layout.Root, driveOpenCLILog))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"msg":"drive open: opened"`) || !strings.Contains(lines[0], `"drive":"Notes"`) ||
		!strings.Contains(lines[1], `"msg":"drive open: closed"`) || !strings.Contains(lines[1], `"result":"discarded"`) {
		t.Fatalf("log:\n%s", b)
	}
	// A refusal is logged as a failed close.
	r2 := newDriveOpenRig(t, "echo 'wardyn-macvm: busy' >&2\nexit 1\n")
	if err := r2.m.driveOpen(t.Context(), driveOpenArgs{name: "Notes", in: strings.NewReader(""), out: io.Discard}); err == nil {
		t.Fatal("no refusal")
	}
	b, _ = os.ReadFile(filepath.Join(r2.m.layout.Root, driveOpenCLILog))
	if !strings.Contains(string(b), `"result":"failed: busy"`) {
		t.Fatalf("log:\n%s", b)
	}
}

// serve's reconcile releases a drive open whose CLI was killed, so it cannot hold a VM slot until someone
// runs drive open again.
func TestDriveOpenServeReconcilesADeadOne(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	if _, err := m.ledger.Admit(admission.Request{Kind: kindDriveOpen, GuestMemory: 4 * gib, DiskReserve: gib, OwnerPID: 4242, OwnerStart: 9}); err != nil {
		t.Fatal(err)
	}
	m.reconcilePrepare()
	if es, err := m.ledger.Counted(); err != nil || len(es) != 0 {
		t.Fatalf("entries = %+v, %v", es, err)
	}
}

// A write back that sent everything removes the folder, and says so on the last line.
func TestDriveOpenWorkRemovesTheFolderAfterWriteBack(t *testing.T) {
	j, g, dir, out := driveOpenJobIn(t, driveOpenEntry{name: "./a.txt", body: "alpha", mode: 0o644})
	driveOpenOwnDest(t, &j, dir)
	j.in = &driveOpenKeys{before: func() { _ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha two"), 0o644) }}
	result, err := driveOpenWork(t.Context(), j)
	if err != nil || result != driveOpenWrittenBack {
		t.Fatalf("%q, %v\n%s", result, err, out)
	}
	if sent := driveOpenSentNames(t, g.call("/usr/bin/tar", "-xf").stdin); sent["a.txt"] != "alpha two" {
		t.Fatalf("sent %v", sent)
	}
	driveOpenGone(t, dir)
	if got := driveOpenTrashed(t, j.dest.trash); got["a.txt"] != "alpha two" || !strings.HasPrefix(got[""], filepath.Base(dir)+" ") {
		t.Fatalf("the Trash holds %v", got)
	}
	if want := "Wrote back 1 changed file."; !strings.Contains(out.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, out)
	}
	if want := "The folder " + dir + " was moved to the Trash.\n"; !strings.HasSuffix(out.String(), want) {
		t.Fatalf("the last line is not %q:\n%s", want, out)
	}
}

// A Trash that cannot be made or moved into keeps the folder with its files, and the line says to delete it.
func TestDriveOpenWorkKeepsTheFolderWhenTheTrashFails(t *testing.T) {
	j, _, dir, out := driveOpenJobIn(t, driveOpenEntry{name: "./a.txt", body: "alpha", mode: 0o644})
	driveOpenOwnDest(t, &j, dir)
	j.dest.trash = filepath.Join(dir, "no", "such", ".Trash")
	j.in = strings.NewReader("\n")
	result, err := driveOpenWork(t.Context(), j)
	if err != nil || result != driveOpenWrittenBack {
		t.Fatalf("%q, %v", result, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(b) != "alpha" {
		t.Fatalf("the file is gone: %q, %v", b, err)
	}
	if want := "The folder " + dir + " is kept; delete it when you're done.\n"; !strings.HasSuffix(out.String(), want) {
		t.Fatalf("the last line is not %q:\n%s", want, out)
	}
}

// A file the person's editor changed, and one it made, after the write back packed what it sent are not in the
// guest. The folder is kept where it is with every file, the new bytes among them, and the line says so.
func TestDriveOpenWorkKeepsWhatChangedAfterItWasSent(t *testing.T) {
	j, g, dir, out := driveOpenJobIn(t,
		driveOpenEntry{name: "./a.txt", body: "alpha", mode: 0o644},
		driveOpenEntry{name: "./b.txt", body: "beta", mode: 0o644},
		driveOpenEntry{name: "./sub/", typeflag: tar.TypeDir, mode: 0o755},
		driveOpenEntry{name: "./sub/c.txt", body: "gamma", mode: 0o644},
		driveOpenEntry{name: "./sub2/", typeflag: tar.TypeDir, mode: 0o755},
		driveOpenEntry{name: "./sub2/d.txt", body: "delta", mode: 0o644})
	driveOpenOwnDest(t, &j, dir)
	j.in = &driveOpenKeys{before: func() { _ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha two"), 0o644) }}
	var once sync.Once
	g.during = func(argv []string) {
		if argv[0] != "/usr/bin/tar" || argv[3] != "-xf" {
			return
		}
		once.Do(func() {
			if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("edited again"), 0o644); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "sub", "new.txt"), []byte("brand new"), 0o644); err != nil {
				t.Error(err)
			}
		})
	}
	result, err := driveOpenWork(t.Context(), j)
	if err != nil || result != driveOpenWrittenBack {
		t.Fatalf("%q, %v\n%s", result, err, out)
	}
	for name, want := range map[string]string{"a.txt": "edited again", "sub/new.txt": "brand new"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != want {
			t.Fatalf("%s = %q, %v; want %q", name, b, err, want)
		}
	}
	for name, want := range map[string]string{"b.txt": "beta", "sub/c.txt": "gamma", "sub2/d.txt": "delta"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != want {
			t.Fatalf("%s = %q, %v; want %q", name, b, err, want)
		}
	}
	if want := "The folder " + dir + " is kept: 2 items in it changed after it was sent and did not go back.\n"; !strings.HasSuffix(out.String(), want) {
		t.Fatalf("the last line is not %q:\n%s", want, out)
	}
}

// A folder that was there before the run is never moved: it is kept, and the line says to delete it.
func TestDriveOpenWorkKeepsAFolderItDidNotMake(t *testing.T) {
	j, _, dir, out := driveOpenJobIn(t, driveOpenEntry{name: "./a.txt", body: "alpha", mode: 0o644})
	j.dest.trash = filepath.Join(t.TempDir(), ".Trash")
	j.in = strings.NewReader("")
	if _, err := driveOpenWork(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if want := "The folder " + dir + " is kept; delete it when you're done.\n"; !strings.HasSuffix(out.String(), want) {
		t.Fatalf("the last line is not %q:\n%s", want, out)
	}
}

// A write back that failed keeps the folder and its files, and the last line names the folder.
func TestDriveOpenWorkKeepsTheFolderWhenWriteBackFails(t *testing.T) {
	j, g, dir, out := driveOpenJobIn(t, driveOpenEntry{name: "./a.txt", body: "alpha", mode: 0o644})
	driveOpenOwnDest(t, &j, dir)
	g.failing = map[string]string{"/usr/bin/tar": "tar: disk full"}
	j.in = &driveOpenKeys{before: func() { _ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644) }}
	result, err := driveOpenWork(t.Context(), j)
	if err == nil || result != driveOpenBackFailed {
		t.Fatalf("%q, %v", result, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "a.txt")); err != nil || string(b) != "changed" {
		t.Fatalf("the edit is gone: %q, %v", b, err)
	}
	if want := "The folder " + dir + " is kept: it holds the changes that did not go back.\n"; !strings.HasSuffix(out.String(), want) {
		t.Fatalf("the last line is not %q:\n%s", want, out)
	}
}

// A write back that succeeded but left out a link the person made keeps the folder: the link is in it only.
func TestDriveOpenWorkKeepsTheFolderWhenItemsWereNotSent(t *testing.T) {
	j, _, dir, out := driveOpenJobIn(t, driveOpenEntry{name: "./a.txt", body: "alpha", mode: 0o644})
	driveOpenOwnDest(t, &j, dir)
	j.in = &driveOpenKeys{before: func() { _ = os.Symlink("a.txt", filepath.Join(dir, "mine")) }}
	result, err := driveOpenWork(t.Context(), j)
	if err != nil || result != driveOpenWrittenBack {
		t.Fatalf("%q, %v", result, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "mine")); err != nil {
		t.Fatalf("the link is gone: %v", err)
	}
	if want := "The folder " + dir + " is kept: 1 item in it could not be written back.\n"; !strings.HasSuffix(out.String(), want) {
		t.Fatalf("the last line is not %q:\n%s", want, out)
	}
}

// The guest's tar leaves out extended attributes and the volume's own folders at the drive's root.
func TestDriveOpenListsWithoutVolumeFoldersOrXattrs(t *testing.T) {
	j, g, _, _ := driveOpenJobIn(t, driveOpenEntry{name: "./a.txt", body: "alpha", mode: 0o644})
	j.in = strings.NewReader("")
	if _, err := driveOpenWork(t.Context(), j); err != nil {
		t.Fatal(err)
	}
	call := g.call("/usr/bin/tar", "-cf")
	if call == nil {
		t.Fatalf("no listing: %v", g.calls)
	}
	want := "/usr/bin/tar -C " + macvm.DriveOpenTarget + " -cf - --no-xattrs --no-mac-metadata --exclude ./.Trashes --exclude ./.Spotlight-V100 --exclude ./.fseventsd --exclude ./.TemporaryItems ."
	if got := strings.Join(call.argv, " "); got != want {
		t.Fatalf("argv %q, want %q", got, want)
	}
}

// A tar that exits 1 having complained only about the volume's own folders and extended attributes lost no item
// of the drive: no warning. One that exits 1 with nothing on stderr still says it ended without saying why, and
// a real complaint among the others is counted alone.
func TestDriveOpenUnreadItemsLeaveOutVolumeNoise(t *testing.T) {
	noise := "tar: ./.Spotlight-V100: Cannot open: Operation not permitted\ntar: ./.Trashes/501: Cannot stat: Permission denied\n" +
		"tar: ./.fseventsd: Failed to get extended attributes\ntar: ./a.txt: Couldn't read xattr\ntar: ./a.txt: Couldn't read xattr: Operation not permitted\ntar: ./x: Couldn't list extended attributes: Permission denied\ntar: ./y: Could not pack extended attributes: Operation not permitted\ntar: ./z: Couldn't retrieve extended attributes: Permission denied\ntar: Error exit delayed from previous errors\n"
	for _, tc := range []struct {
		name, stderr string
		unread       string // "" for no warning
		line         string
	}{
		{"only the volume's own", noise, "", ""},
		{"nothing on stderr", "", "1 item could not be read", "tar ended with code 1 without saying why"},
		{"one real among the noise", noise + "tar: ./secret: Cannot open: Permission denied\n", "1 item could not be read", "tar: ./secret: Cannot open: Permission denied"},
		{"a nested folder of the same name is a real item", "tar: ./docs/.Trashes: Cannot open: Permission denied\n", "1 item could not be read", "tar: ./docs/.Trashes: Cannot open: Permission denied"},
		{"only noise, but more than was kept", strings.Repeat(noise, 300), "1 item could not be read", "tar said more than was kept"},
		{"a file named like an xattr complaint is a real item", noise + "tar: ./Couldn't read xattr.txt: Cannot open: Operation not permitted\n", "1 item could not be read", "tar: ./Couldn't read xattr.txt: Cannot open: Operation not permitted"},
		{"a file named for extended attributes is a real item", noise + "tar: ./reports/xattr-notes.txt: Cannot open: Permission denied\n", "1 item could not be read", "tar: ./reports/xattr-notes.txt: Cannot open: Permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j, g, _, out := driveOpenJobIn(t, driveOpenEntry{name: "./a.txt", body: "alpha", mode: 0o644})
			g.tarExit, g.tarErr = 1, tc.stderr
			j.in = strings.NewReader("")
			if result, err := driveOpenWork(t.Context(), j); err != nil || result != driveOpenDiscarded {
				t.Fatalf("%q, %v\n%s", result, err, out)
			}
			if got := strings.Contains(out.String(), "could not be read from the drive"); got != (tc.unread != "") {
				t.Fatalf("warning = %v, want %q:\n%s", got, tc.unread, out)
			}
			if tc.unread != "" && (!strings.Contains(out.String(), tc.unread) || !strings.Contains(out.String(), tc.line)) {
				t.Fatalf("output lacks %q and %q:\n%s", tc.unread, tc.line, out)
			}
			if strings.Contains(out.String(), "Spotlight") || strings.Contains(out.String(), "./a.txt: Couldn't read xattr") {
				t.Fatalf("noise shown:\n%s", out)
			}
		})
	}
}

// The volume's own folder names match as written: a name that only looks like one is a real item, and the
// real folder's complaint is still dropped.
func TestDriveOpenUnreadLinesMatchVolumeFoldersLiterally(t *testing.T) {
	keep := []string{"tar: ./xTrashes: Cannot open: Permission denied", "tar: ./xSpotlight-V100/data: Cannot open: Permission denied", "tar: ./.Trashesx: Cannot open: Permission denied"}
	if got := driveOpenUnreadLines(keep); !slices.Equal(got, keep) {
		t.Fatalf("kept %q, want all of %q", got, keep)
	}
	if got := driveOpenUnreadLines([]string{"tar: ./.Trashes: Cannot open: Permission denied", "tar: ./.Spotlight-V100/data: Cannot stat: Permission denied"}); len(got) != 0 {
		t.Fatalf("kept %q", got)
	}
}
