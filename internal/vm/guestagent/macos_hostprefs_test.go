// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package guestagent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	baseID   = "AAAAAAAA-1111-2222-3333-444444444444"
	thisID   = "BBBBBBBB-1111-2222-3333-444444444444"
	otherID  = "CCCCCCCC-1111-2222-3333-444444444444"
	fourthID = "DDDDDDDD-1111-2222-3333-444444444444"

	// carriedLine is the line a child prints when it copied two files.
	carriedLine = "host_prefs=carried files=2 kept=0 skipped=0 failed=0"
	// ioregOut is the part of `ioreg -rd1 -c IOPlatformExpertDevice` the carry reads.
	ioregOut = "+-o Root  <class IORegistryEntry, id 0x100000100, retain 17>\n" +
		"  +-o VMA2MACOSAP  <class IOPlatformExpertDevice, id 0x10000020f, registered, matched, active, busy 0 (22 ms), retain 40>\n" +
		"    {\n" +
		"      \"IOPlatformSerialNumber\" = \"ZZZZZZZZZZ\"\n" +
		"      \"IOPlatformUUID\" = \"" + thisID + "\"\n" +
		"    }\n"
)

// carryRig is a HostPrefsCarry whose switch is on, its console is the login window and ioreg answers
// ioregOut, with a child that records that it ran.
type carryRig struct {
	HostPrefsCarry
	run     *fakeRunner
	console *seqConsole
	ran     string // the file the child touches
	dir     string
}

func newCarryRig(t *testing.T, childScript string) *carryRig {
	t.Helper()
	dir := t.TempDir()
	sw := filepath.Join(dir, "setup-assistant-carry")
	if err := os.WriteFile(sw, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &carryRig{run: &fakeRunner{}, console: &seqConsole{answers: []fakeConsole{{err: ErrNoConsoleUser}}}, ran: filepath.Join(dir, "ran"), dir: dir}
	r.run.out = func(argv []string) ([]byte, error) {
		if argv[0] != "/usr/sbin/ioreg" || strings.Join(argv[1:], " ") != "-rd1 -c IOPlatformExpertDevice" {
			t.Errorf("unexpected command %q", argv)
		}
		return []byte(ioregOut), nil
	}
	r.HostPrefsCarry = HostPrefsCarry{Run: r.run, Console: r.console, MarkerDir: filepath.Join(dir, "marker"), Switch: sw,
		ByHost: filepath.Join(dir, "ByHost"), bound: 10 * time.Second,
		argv: []string{"/bin/sh", "-c", "touch " + r.ran + "; " + childScript, "sh"}}
	if err := os.Mkdir(r.MarkerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

// assertNothingRan fails when the carry read the console, called ioreg or started the child.
func (r *carryRig) assertNothingRan(t *testing.T, why string) {
	t.Helper()
	if r.console.n != 0 && why != "console" {
		t.Errorf("%s: console read %d times, want 0", why, r.console.n)
	}
	if len(r.run.calls) != 0 {
		t.Errorf("%s: ioreg calls = %q, want none", why, r.run.calls)
	}
	if _, err := os.Lstat(r.ran); err == nil {
		t.Errorf("%s: the child ran", why)
	}
}

func hasPrefix(t *testing.T, got, want string) {
	t.Helper()
	if !strings.HasPrefix(got, want) {
		t.Errorf("line = %q, want it to start %q", got, want)
	}
	if want != "host_prefs=off" {
		at := got[strings.LastIndex(got, " at=")+len(" at="):]
		if n, err := strconv.ParseInt(at, 10, 64); err != nil || n < time.Now().Add(-time.Minute).Unix() || n > time.Now().Add(time.Minute).Unix() {
			t.Errorf("line = %q: at= is %q, want a Unix time of now", got, at)
		}
	}
}

// Without the switch file, the default, the carry reads nothing and starts nothing. Catches a carry that
// runs without the opt-in.
func TestCarryHostPrefsOffWithoutTheSwitch(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(secret, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, setup := range map[string]func(sw string){
		"no file":     func(string) {},
		"a directory": func(sw string) { _ = os.Mkdir(sw, 0o755) },
		"a link":      func(sw string) { _ = os.Symlink(secret, sw) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newCarryRig(t, "echo '"+carriedLine+"'")
			if err := os.Remove(r.Switch); err != nil {
				t.Fatal(err)
			}
			setup(r.Switch)
			if got := r.Carry(t.Context()); got != "host_prefs=off" {
				t.Fatalf("line = %q, want host_prefs=off", got)
			}
			r.assertNothingRan(t, name)
		})
	}
	if MacHostPrefsSwitchPath != "/Library/Wardyn/setup-assistant-carry" {
		t.Fatalf("switch path = %q, which the docs do not name", MacHostPrefsSwitchPath)
	}
}

// A marker in MarkerDir means Init has happened: this is a restart inside a live run, and the carry
// does nothing. Catches a copy on a service restart.
func TestCarryHostPrefsSkipsAfterInit(t *testing.T) {
	r := newCarryRig(t, "echo '"+carriedLine+"'")
	if err := os.WriteFile(filepath.Join(r.MarkerDir, initMarkerFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := r.Carry(t.Context())
	hasPrefix(t, got, "host_prefs=skipped reason=not_first_start at=")
	r.assertNothingRan(t, "after init")
}

// Someone at the console, or a console that cannot be read, means no copy. Catches a copy under a
// signed-in session.
func TestCarryHostPrefsSkipsWithSomeoneAtTheConsole(t *testing.T) {
	for _, c := range []struct {
		name string
		at   fakeConsole
		want string
	}{
		{"uid 501", fakeConsole{uid: 501}, "host_prefs=skipped reason=agent_signed_in console=501 at="},
		{"read error", fakeConsole{err: errors.New("stat /dev/console: boom")}, "host_prefs=skipped reason=console_unread at="},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newCarryRig(t, "echo '"+carriedLine+"'")
			r.console.answers = []fakeConsole{c.at}
			got := r.Carry(t.Context())
			hasPrefix(t, got, c.want)
			r.assertNothingRan(t, "console")
			if r.console.n != 1 {
				t.Errorf("console read %d times, want 1", r.console.n)
			}
		})
	}
}

// ioreg output with no UUID, a lower-case one or an error gives uuid_unread, and no child.
func TestCarryHostPrefsUnreadUUID(t *testing.T) {
	uuidLine := func(id string) string { return "    \"IOPlatformUUID\" = \"" + id + "\"\n" }
	for name, out := range map[string]func([]string) ([]byte, error){
		"no UUID":    func([]string) ([]byte, error) { return []byte("+-o Root\n"), nil },
		"lower case": func([]string) ([]byte, error) { return []byte(uuidLine(strings.ToLower(thisID))), nil },
		"too short":  func([]string) ([]byte, error) { return []byte(uuidLine("AAAAAAAA-1111")), nil },
		"an error":   func([]string) ([]byte, error) { return nil, errors.New("exit status 1") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newCarryRig(t, "echo '"+carriedLine+"'")
			r.run.out = out
			got := r.Carry(t.Context())
			hasPrefix(t, got, "host_prefs=skipped reason=uuid_unread console=none at=")
			if _, err := os.Lstat(r.ran); err == nil {
				t.Error("the child ran")
			}
		})
	}
}

// The child's answer is the status line only when it is exactly one line of counts or a reason; the
// bound kills its whole group; a child that fails or cannot start is a failure. Catches a status line
// that could carry a name or contents.
func TestCarryHostPrefsChildBoundAndOutput(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	r := newCarryRig(t, "echo \"$1|$2|$PATH|$(pwd)\" >"+argsFile+"; echo '"+carriedLine+"'")
	got := r.Carry(t.Context())
	hasPrefix(t, got, carriedLine+" console=none at=")
	if b, _ := os.ReadFile(argsFile); string(b) != r.ByHost+"|"+thisID+"|/usr/bin:/bin|/\n" {
		t.Errorf("the child saw %q, want the folder, this boot's id, a bare PATH and /", b)
	}

	for name, c := range map[string]struct{ script, want string }{
		"a counts line":      {"echo 'host_prefs=carried files=0 kept=3 skipped=0 failed=1'", "host_prefs=carried files=0 kept=3 skipped=0 failed=1 console=none at="},
		"a skip reason":      {"echo 'host_prefs=skipped reason=no_byhost'", "host_prefs=skipped reason=no_byhost console=none at="},
		"a file name":        {"echo 'com.apple.loginwindow.AAAAAAAA-1111-2222-3333-444444444444.plist'", "host_prefs=failed reason=bad_output console=none at="},
		"a name in a line":   {"echo 'host_prefs=skipped reason=com.apple.x'", "host_prefs=failed reason=bad_output console=none at="},
		"two lines":          {"echo '" + carriedLine + "'; echo '" + carriedLine + "'", "host_prefs=failed reason=bad_output console=none at="},
		"nothing":            {"true", "host_prefs=failed reason=bad_output console=none at="},
		"too much":           {"head -c 5000 /dev/zero | tr '\\0' a", "host_prefs=failed reason=bad_output console=none at="},
		"a non-zero exit":    {"echo '" + carriedLine + "'; exit 1", "host_prefs=failed reason=child_failed console=none at="},
		"killed by a signal": {"kill -9 $$", "host_prefs=failed reason=child_failed console=none at="},
	} {
		t.Run(name, func(t *testing.T) {
			r := newCarryRig(t, c.script)
			hasPrefix(t, r.Carry(t.Context()), c.want)
		})
	}

	t.Run("a child that cannot start", func(t *testing.T) {
		r := newCarryRig(t, "true")
		r.argv = []string{filepath.Join(t.TempDir(), "missing")}
		hasPrefix(t, r.Carry(t.Context()), "host_prefs=failed reason=child_failed console=none at=")
	})

	t.Run("a child that hangs", func(t *testing.T) {
		// The shell writes its pid, which Setsid made its group's id, and leaves a child behind, so only
		// a kill of the whole group ends both.
		pidFile := filepath.Join(t.TempDir(), "pid")
		r := newCarryRig(t, "echo $$ >"+pidFile+"; sleep 60 & wait")
		r.bound = 500 * time.Millisecond
		start := time.Now()
		hasPrefix(t, r.Carry(t.Context()), "host_prefs=failed reason=timeout console=none at=")
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("the carry took %s on a 500ms bound", d)
		}
		b, err := os.ReadFile(pidFile)
		if err != nil {
			t.Fatal(err)
		}
		pgid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(5 * time.Second); unix.Kill(-pgid, 0) == nil; time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the child's process group outlived its bound")
			}
		}
	})
}

// The child exits 2 and prints nothing on any args but the folder and a well-formed id. Catches args[0]
// read from a bare verb.
func TestHostPrefsCarryMainArgs(t *testing.T) {
	for name, args := range map[string][]string{
		"none":              nil,
		"one":               {t.TempDir()},
		"three":             {t.TempDir(), thisID, "extra"},
		"a lower-case id":   {t.TempDir(), strings.ToLower(thisID)},
		"a short id":        {t.TempDir(), "BBBBBBBB-1111"},
		"an id with a tail": {t.TempDir(), thisID + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if code := HostPrefsCarryMain(args, &out); code != 2 || out.Len() != 0 {
				t.Fatalf("exit %d, stdout %q; want 2 and nothing", code, out.String())
			}
		})
	}
	var out bytes.Buffer
	if code := HostPrefsCarryMain([]string{t.TempDir(), thisID}, &out); code != 0 || out.String() != "host_prefs=skipped reason=no_other_identity\n" {
		t.Fatalf("two args: exit %d, stdout %q", code, out.String())
	}
}

// byHostFile is one ByHost file name.
func byHostFile(domain, id string) string { return domain + "." + id + ".plist" }

// newByHost is a ByHost folder of the test's own, with the files written at 0644.
func newByHost(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ByHost")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// listing is the folder's names, sorted, with a .wardyn- temp file failing the test.
func listing(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		if strings.Contains(e.Name(), ".wardyn-") {
			t.Errorf("temp file %s left in %s", e.Name(), dir)
		}
		names = append(names, e.Name())
	}
	return names
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func wantLine(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

func me() int { return os.Getuid() }

// Two files of the base's id give two files of this boot's id with the same bytes at 0600, and the
// sources stay. Catches a move instead of a copy, and the wrong destination id.
func TestCarryByHostCopiesTheBaseIdentity(t *testing.T) {
	dir := newByHost(t, map[string]string{
		byHostFile(".GlobalPreferences", baseID):       "global",
		byHostFile("com.apple.SetupAssistant", baseID): "pane",
	})
	wantLine(t, carryByHost(dir, thisID, me()), "host_prefs=carried files=2 kept=0 skipped=0 failed=0")
	for domain, body := range map[string]string{".GlobalPreferences": "global", "com.apple.SetupAssistant": "pane"} {
		for _, id := range []string{baseID, thisID} {
			p := filepath.Join(dir, byHostFile(domain, id))
			if got := readFile(t, p); got != body {
				t.Errorf("%s = %q, want %q", filepath.Base(p), got, body)
			}
		}
		fi, err := os.Lstat(filepath.Join(dir, byHostFile(domain, thisID)))
		if err != nil || fi.Mode() != 0o600 {
			t.Errorf("the copy of %s = %v, %v; want a regular file at 0600", domain, fi, err)
		}
	}
	if got := len(listing(t, dir)); got != 4 {
		t.Errorf("%d files, want 4", got)
	}
}

// A destination that exists keeps its bytes, and so does one that appears after the presence check
// (Linkat's EEXIST). Catches a replace instead of a link.
func TestCarryByHostNeverOverwrites(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		dir := newByHost(t, map[string]string{byHostFile("a", baseID): "base", byHostFile("a", thisID): "mine"})
		wantLine(t, carryByHost(dir, thisID, me()), "host_prefs=carried files=0 kept=1 skipped=0 failed=0")
		if got := readFile(t, filepath.Join(dir, byHostFile("a", thisID))); got != "mine" {
			t.Errorf("destination = %q, want it kept", got)
		}
		listing(t, dir)
	})
	t.Run("appears after the check", func(t *testing.T) {
		dir := newByHost(t, map[string]string{byHostFile("a", baseID): "base"})
		setVar(t, &hostPrefsBeforeOpen, func(string) {
			if err := os.WriteFile(filepath.Join(dir, byHostFile("a", thisID)), []byte("raced"), 0o644); err != nil {
				t.Error(err)
			}
		})
		wantLine(t, carryByHost(dir, thisID, me()), "host_prefs=carried files=0 kept=1 skipped=0 failed=0")
		if got := readFile(t, filepath.Join(dir, byHostFile("a", thisID))); got != "raced" {
			t.Errorf("destination = %q, want the one that appeared", got)
		}
		listing(t, dir)
	})
}

// Only the one other identity is copied: none gives no_other_identity, several give several_identities,
// and neither writes anything.
func TestCarryByHostOneOtherIdentity(t *testing.T) {
	for name, c := range map[string]struct {
		files map[string]string
		want  string
	}{
		"empty":            {nil, "host_prefs=skipped reason=no_other_identity"},
		"only this boot's": {map[string]string{byHostFile("a", thisID): "x"}, "host_prefs=skipped reason=no_other_identity"},
		"names of no id":   {map[string]string{"a.plist": "x", "a." + baseID: "x", "a.lower-case.plist": "x"}, "host_prefs=skipped reason=no_other_identity"},
		"two others": {map[string]string{byHostFile("a", baseID): "x", byHostFile("a", otherID): "x"},
			"host_prefs=skipped reason=several_identities"},
		"three others": {map[string]string{byHostFile("a", baseID): "x", byHostFile("a", otherID): "x", byHostFile("a", fourthID): "x"},
			"host_prefs=skipped reason=several_identities"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := newByHost(t, c.files)
			before := listing(t, dir)
			wantLine(t, carryByHost(dir, thisID, me()), c.want)
			if after := listing(t, dir); strings.Join(after, "|") != strings.Join(before, "|") {
				t.Errorf("folder = %q, want it unchanged %q", after, before)
			}
		})
	}
}

// No link is followed: a linked folder is refused, a linked source is not copied or read, and a link at the
// destination name is kept with its target unchanged.
func TestCarryByHostFollowsNoLink(t *testing.T) {
	t.Run("the folder", func(t *testing.T) {
		real := newByHost(t, map[string]string{byHostFile("a", baseID): "base"})
		link := filepath.Join(t.TempDir(), "ByHost")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		wantLine(t, carryByHost(link, thisID, me()), "host_prefs=skipped reason=no_byhost")
		if got := listing(t, real); len(got) != 1 {
			t.Errorf("real folder = %q, want it unchanged", got)
		}
	})
	t.Run("a source", func(t *testing.T) {
		secret := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(secret, []byte("do-not-copy"), 0o644); err != nil {
			t.Fatal(err)
		}
		dir := newByHost(t, map[string]string{byHostFile("b", baseID): "base"})
		if err := os.Symlink(secret, filepath.Join(dir, byHostFile("a", baseID))); err != nil {
			t.Fatal(err)
		}
		wantLine(t, carryByHost(dir, thisID, me()), "host_prefs=carried files=1 kept=0 skipped=0 failed=0")
		if _, err := os.Lstat(filepath.Join(dir, byHostFile("a", thisID))); err == nil {
			t.Error("the linked source got a destination")
		}
		for _, n := range listing(t, dir) {
			if fi, _ := os.Lstat(filepath.Join(dir, n)); fi.Mode().IsRegular() && readFile(t, filepath.Join(dir, n)) == "do-not-copy" {
				t.Errorf("%s holds the link's target", n)
			}
		}
	})
	t.Run("only linked sources", func(t *testing.T) {
		secret := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(secret, []byte("do-not-copy"), 0o644); err != nil {
			t.Fatal(err)
		}
		dir := newByHost(t, nil)
		if err := os.Symlink(secret, filepath.Join(dir, byHostFile("a", baseID))); err != nil {
			t.Fatal(err)
		}
		wantLine(t, carryByHost(dir, thisID, me()), "host_prefs=skipped reason=no_other_identity")
		listing(t, dir)
	})
	t.Run("a dangling link at the destination", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing")
		dir := newByHost(t, map[string]string{byHostFile("a", baseID): "base"})
		dest := filepath.Join(dir, byHostFile("a", thisID))
		if err := os.Symlink(missing, dest); err != nil {
			t.Fatal(err)
		}
		wantLine(t, carryByHost(dir, thisID, me()), "host_prefs=carried files=0 kept=1 skipped=0 failed=0")
		if _, err := os.Lstat(missing); err == nil {
			t.Error("the link's target was created through the link")
		}
		if got, err := os.Readlink(dest); err != nil || got != missing {
			t.Errorf("destination link = %q, %v; want it unchanged, %q", got, err, missing)
		}
		listing(t, dir)
	})
	t.Run("the destination", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
			t.Fatal(err)
		}
		dir := newByHost(t, map[string]string{byHostFile("a", baseID): "base"})
		dest := filepath.Join(dir, byHostFile("a", thisID))
		if err := os.Symlink(target, dest); err != nil {
			t.Fatal(err)
		}
		wantLine(t, carryByHost(dir, thisID, me()), "host_prefs=carried files=0 kept=1 skipped=0 failed=0")
		if got := readFile(t, target); got != "target" {
			t.Errorf("link target = %q, want it unchanged", got)
		}
		if fi, err := os.Lstat(dest); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("destination = %v, %v; want the link left", fi, err)
		}
		listing(t, dir)
	})
}

// A file with a second hard link is skipped, and a folder that is not the given uid's is refused. Catches a
// copy of a file the agent does not own alone.
func TestCarryByHostOwnFilesOnly(t *testing.T) {
	dir := newByHost(t, map[string]string{byHostFile("a", baseID): "shared", byHostFile("b", baseID): "alone"})
	if err := os.Link(filepath.Join(dir, byHostFile("a", baseID)), filepath.Join(t.TempDir(), "elsewhere")); err != nil {
		t.Fatal(err)
	}
	wantLine(t, carryByHost(dir, thisID, me()), "host_prefs=carried files=1 kept=0 skipped=1 failed=0")
	if _, err := os.Lstat(filepath.Join(dir, byHostFile("a", thisID))); err == nil {
		t.Error("the file with a second link got a destination")
	}
	wantLine(t, carryByHost(dir, thisID, me()+1), "host_prefs=skipped reason=no_byhost")
	listing(t, dir)
}

// A source swapped between the listing and the open is judged as opened: a link is not followed, a pipe is
// not read, a file grown past 1 MiB is not copied. Catches checks made on the name rather than the opened
// file.
func TestCarryByHostSwapBetweenListAndOpen(t *testing.T) {
	swaps := map[string]func(p string) error{
		"a link": func(p string) error {
			secret := filepath.Join(filepath.Dir(p), "..", "secret")
			if err := os.WriteFile(secret, []byte("do-not-copy"), 0o644); err != nil {
				return err
			}
			if err := os.Remove(p); err != nil {
				return err
			}
			return os.Symlink(secret, p)
		},
		"a pipe": func(p string) error {
			if err := os.Remove(p); err != nil {
				return err
			}
			return syscall.Mkfifo(p, 0o600)
		},
		"grown past 1 MiB": func(p string) error {
			return os.WriteFile(p, bytes.Repeat([]byte("g"), 1<<20+1), 0o644)
		},
	}
	for name, swap := range swaps {
		t.Run(name, func(t *testing.T) {
			dir := newByHost(t, map[string]string{byHostFile("a", baseID): "base"})
			setVar(t, &hostPrefsBeforeOpen, func(n string) {
				if err := swap(filepath.Join(dir, n)); err != nil {
					t.Error(err)
				}
			})
			done := make(chan string, 1)
			go func() { done <- carryByHost(dir, thisID, me()) }()
			select {
			case got := <-done:
				wantLine(t, got, "host_prefs=carried files=0 kept=0 skipped=1 failed=0")
			case <-time.After(10 * time.Second):
				t.Fatal("the copy hung on a swapped source")
			}
			if _, err := os.Lstat(filepath.Join(dir, byHostFile("a", thisID))); err == nil {
				t.Error("the swapped source got a destination")
			}
			for _, n := range listing(t, dir) {
				if fi, _ := os.Lstat(filepath.Join(dir, n)); fi.Mode().IsRegular() && readFile(t, filepath.Join(dir, n)) == "do-not-copy" {
					t.Errorf("%s holds the link's target", n)
				}
			}
		})
	}
}

// 1 MiB is copied; one byte more is skipped and gets no destination.
func TestCarryByHostSizeCap(t *testing.T) {
	dir := newByHost(t, map[string]string{
		byHostFile("fits", baseID):     strings.Repeat("f", 1<<20),
		byHostFile("toolarge", baseID): strings.Repeat("l", 1<<20+1),
	})
	wantLine(t, carryByHost(dir, thisID, me()), "host_prefs=carried files=1 kept=0 skipped=1 failed=0")
	if got := readFile(t, filepath.Join(dir, byHostFile("fits", thisID))); len(got) != 1<<20 {
		t.Errorf("the 1 MiB file's copy is %d bytes", len(got))
	}
	if _, err := os.Lstat(filepath.Join(dir, byHostFile("toolarge", thisID))); err == nil {
		t.Error("the file over 1 MiB got a destination")
	}
	listing(t, dir)
}

// 256 files of the base's id are all copied; 257 copy nothing.
func TestCarryByHostFileCap(t *testing.T) {
	for _, c := range []struct {
		n    int
		want string
	}{
		{256, "host_prefs=carried files=256 kept=0 skipped=0 failed=0"},
		{257, "host_prefs=skipped reason=too_many_files"},
	} {
		t.Run(strconv.Itoa(c.n), func(t *testing.T) {
			files := map[string]string{}
			for i := range c.n {
				files[byHostFile("d"+strconv.Itoa(i), baseID)] = "x"
			}
			dir := newByHost(t, files)
			wantLine(t, carryByHost(dir, thisID, me()), c.want)
			want := c.n
			if c.n == 256 {
				want = 2 * c.n
			}
			if got := len(listing(t, dir)); got != want {
				t.Errorf("%d files, want %d", got, want)
			}
		})
	}
}

// After a copy, a kept destination, a destination that appears at the link and a failed open, no temp file
// is left.
func TestCarryByHostLeavesNoTemp(t *testing.T) {
	dir := newByHost(t, map[string]string{byHostFile("a", baseID): "a", byHostFile("b", baseID): "b", byHostFile("b", thisID): "mine"})
	wantLine(t, carryByHost(dir, thisID, me()), "host_prefs=carried files=1 kept=1 skipped=0 failed=0")
	listing(t, dir)

	dir = newByHost(t, map[string]string{byHostFile("a", baseID): "a"})
	setVar(t, &hostPrefsBeforeOpen, func(string) { _ = os.WriteFile(filepath.Join(dir, byHostFile("a", thisID)), nil, 0o644) })
	wantLine(t, carryByHost(dir, thisID, me()), "host_prefs=carried files=0 kept=1 skipped=0 failed=0")
	listing(t, dir)

	// A destination name too long for the temp name's room fails after the open, with nothing left.
	long := strings.Repeat("l", 255-len(byHostFile("", baseID)))
	dir = newByHost(t, nil)
	if err := os.WriteFile(filepath.Join(dir, byHostFile(long, baseID)), []byte("x"), 0o644); err != nil {
		t.Skipf("this file system takes no 255-byte name: %v", err)
	}
	wantLine(t, carryByHost(dir, thisID, me()), "host_prefs=carried files=0 kept=0 skipped=0 failed=1")
	listing(t, dir)
}

// The status folder is made when absent, the file is one line at 0644, and a link at its path is replaced,
// not followed.
func TestWriteHostPrefsStatus(t *testing.T) {
	root := stockLayout(t)
	path := root + MacHostPrefsStatusPath
	if filepath.Dir(MacHostPrefsStatusPath) != MacSessionEnvDir {
		t.Fatalf("status path %s is not in %s", MacHostPrefsStatusPath, MacSessionEnvDir)
	}
	if err := WriteHostPrefsStatus(path, carriedLine+" console=none at=1"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Dir(path)); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o755 {
		t.Errorf("folder = %v, %v; want one made at 0755", fi, err)
	}
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o644 {
		t.Errorf("file = %v, %v; want a regular file at 0644", fi, err)
	}
	if got := readFile(t, path); got != carriedLine+" console=none at=1\n" {
		t.Errorf("status = %q", got)
	}

	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(elsewhere, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}
	if err := WriteHostPrefsStatus(path, "host_prefs=off"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("file = %v, %v; want the link replaced by a regular file", fi, err)
	}
	if got := readFile(t, path); got != "host_prefs=off\n" {
		t.Errorf("status = %q", got)
	}
	if got := readFile(t, elsewhere); got != "untouched" {
		t.Errorf("the link's target = %q, want it unchanged", got)
	}
}
