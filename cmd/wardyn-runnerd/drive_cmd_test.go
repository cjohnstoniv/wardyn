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
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
			_ = guestagent.WriteFrame(w, guestagent.FrameStderr, []byte(g.tarErr))
		}
		code = g.tarExit
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
	if err := driveOpenWork(t.Context(), j); err != nil {
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

// Ctrl-C before Return, and an input that ends, write nothing: no tar x, no delete. The copy stays, and the
// guest is still synced (opening the drive replayed its journal).
func TestDriveOpenWorkCtrlCWritesNothing(t *testing.T) {
	for _, name := range []string{"ctrl-c", "input ends"} {
		t.Run(name, func(t *testing.T) {
			j, g, dir, out := driveOpenJobIn(t, driveOpenEntry{name: "./a.txt", body: "alpha", mode: 0o644})
			j.deleteGone = true
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
			if err := driveOpenWork(ctx, j); err != nil {
				t.Fatal(err)
			}
			if g.call("/usr/bin/tar", "-xf") != nil || g.call("/bin/sh", "") != nil {
				t.Fatalf("the guest was written: %v", g.calls)
			}
			if g.call("/bin/sync", "") == nil || !strings.Contains(out.String(), "Discarded: nothing was written back.") {
				t.Fatalf("calls %v\n%s", g.calls, out)
			}
		})
	}
}

// A hostile listing, a tar that fails in the guest, and a copy with Ctrl-C part-way leave the folder empty.
func TestDriveOpenWorkLeavesNothingWhenTheCopyFails(t *testing.T) {
	for name, set := range map[string]func(*driveOpenGuest, *testing.T){
		"name leaves the folder": func(g *driveOpenGuest, t *testing.T) {
			g.listing = driveOpenStream(t, driveOpenEntry{name: "./ok", body: "1"}, driveOpenEntry{name: "../x", body: "2"})
		},
		"tar fails": func(g *driveOpenGuest, _ *testing.T) { g.tarExit, g.tarErr = 1, "tar: Permission denied" },
	} {
		t.Run(name, func(t *testing.T) {
			j, g, dir, _ := driveOpenJobIn(t, driveOpenEntry{name: "./ok", body: "1"})
			set(g, t)
			err := driveOpenWork(t.Context(), j)
			if err == nil {
				t.Fatal("the copy was kept")
			}
			if left, _ := os.ReadDir(dir); len(left) > 0 {
				t.Fatalf("left %v after %v", left, err)
			}
			if name == "tar fails" && !strings.Contains(err.Error(), "Permission denied") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// Deleting a copied file deletes it from the drive only with the flag, through one sh that reads the names
// from stdin.
func TestDriveOpenWorkDeletesOnlyWithTheFlag(t *testing.T) {
	for _, flag := range []bool{false, true} {
		j, g, dir, _ := driveOpenJobIn(t, driveOpenEntry{name: "./keep", body: "1"}, driveOpenEntry{name: "./gone", body: "2"})
		j.deleteGone = flag
		j.in = &driveOpenKeys{before: func() { _ = os.Remove(filepath.Join(dir, "gone")) }}
		if err := driveOpenWork(t.Context(), j); err != nil {
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
	if _, _, err := m.driveOpenFind("Other"); err == nil || !strings.Contains(err.Error(), "does not know a drive named \"Other\"") {
		t.Fatalf("unknown = %v", err)
	}
	put("wardyn-drive-aaaa", `{"label":"Wardyn-aaaaaaaa","name":"Notes"}`, true)
	if _, _, err := m.driveOpenFind("Notes"); err == nil || !strings.Contains(err.Error(), "More than one drive") {
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
	script := "#!/bin/sh\necho \"$@\" >>" + dr.args + "\n" + strings.NewReplacer("SOCK", sock, "ARGS", dr.args).Replace(helperScript)
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
