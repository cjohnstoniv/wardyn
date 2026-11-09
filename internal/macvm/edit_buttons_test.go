// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// layoutStable is the edit code's alphabet: the 20 lowercase letters on the same key on QWERTY, QWERTZ and
// AZERTY keyboards (AZERTY swaps a/q and w/z and moves m; QWERTZ swaps y/z), with no digit and no shift.
const layoutStable = "bcdefghijklnoprstuvx"

// The edit's one-time code is 10 letters that sit on the same key on QWERTY, QWERTZ and AZERTY, so Type code
// types it right on any of them; the generator draws every one of the 20 and nothing else.
func TestTypableCodeUsesLayoutStableLetters(t *testing.T) {
	e := newEdit(t)
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	code := e.codeSeen.Code
	if len(code) != 10 || strings.Trim(code, layoutStable) != "" {
		t.Errorf("the edit's code %q: want 10 letters of %q", code, layoutStable)
	}
	seen := map[byte]bool{}
	for range 200 {
		c, err := macvm.TypableCode(10)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range c {
			if strings.IndexByte(layoutStable, b) < 0 {
				t.Fatalf("typableCode drew %q, which moves between keyboard layouts or needs shift", b)
			}
			seen[b] = true
		}
	}
	if len(seen) != len(layoutStable) {
		t.Errorf("2000 draws used %d letters, want all %d of %q", len(seen), len(layoutStable), layoutStable)
	}
}

// buttonGuest is the window boot's guest with the edit actions: it records each, refuses Trust the list's CAs
// when refuse is set, answers verify-cert by certificate name and Full Disk Access with fda, and fails every
// read while down is set.
type buttonGuest struct {
	*windowGuest
	bmu     sync.Mutex
	calls   []string
	refuse  bool
	trusted map[string]bool
	fda     string
	fv      string // FileVault as the posture reads it
	down    bool
	termErr error // what Open admin Terminal answers, when set
}

func (g *buttonGuest) record(name string) {
	g.bmu.Lock()
	defer g.bmu.Unlock()
	g.calls = append(g.calls, name)
}

func (g *buttonGuest) EditAdminTerminal(context.Context) (guestagent.EditActionResult, error) {
	g.record("admin-terminal")
	g.bmu.Lock()
	defer g.bmu.Unlock()
	if g.termErr != nil {
		return guestagent.EditActionResult{}, g.termErr
	}
	return guestagent.EditActionResult{Opened: true}, nil
}

func (g *buttonGuest) EditTrustCAs(context.Context) (guestagent.EditActionResult, error) {
	g.record("trust-cas")
	g.bmu.Lock()
	defer g.bmu.Unlock()
	if g.refuse {
		return guestagent.EditActionResult{}, fmt.Errorf("%w: nobody is signed in at the guest's console", guestagent.ErrEditRefused)
	}
	return guestagent.EditActionResult{Opened: true, Certificates: 2}, nil
}

func (g *buttonGuest) EditFullDiskAccess(context.Context) (guestagent.EditActionResult, error) {
	g.record("full-disk-access")
	return guestagent.EditActionResult{Opened: true, Revealed: true}, nil
}

func (g *buttonGuest) setTermErr(err error) { g.bmu.Lock(); g.termErr = err; g.bmu.Unlock() }

// walkRefusal is the guest's words for a script directory under a parent the daemon group can change.
const walkRefusal = `"/private/var/run/wardyn-edit": "/private/var/run" is writable by gid 1: a link or a directory not only root can change`

func (g *buttonGuest) isDown() bool { g.bmu.Lock(); defer g.bmu.Unlock(); return g.down }

func (g *buttonGuest) Run(ctx context.Context, argv []string) (string, int, error) {
	if len(argv) > 1 && argv[1] == "verify-cert" {
		if g.isDown() {
			return "", 0, errors.New("the tools session is down")
		}
		if g.trusted[filepath.Base(filepath.Dir(argv[len(argv)-1]))] {
			return "", 0, nil
		}
		return "", 1, nil
	}
	return g.windowGuest.Run(ctx, argv)
}

func (g *buttonGuest) Posture(ctx context.Context) (guestagent.ToolsPosture, error) {
	if g.isDown() {
		return guestagent.ToolsPosture{}, errors.New("the tools session is down")
	}
	p, err := g.editGuest.Posture(ctx)
	p.FullDiskAccess, p.FileVault = g.fda, g.fv
	return p, err
}

// liveWin is the edit window with its live line, which it records.
type liveWin struct {
	macvm.Window
	mu    sync.Mutex
	texts []string
}

func (w *liveWin) ShowLive(text string) { w.mu.Lock(); w.texts = append(w.texts, text); w.mu.Unlock() }

func (w *liveWin) last() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.texts) == 0 {
		return ""
	}
	return w.texts[len(w.texts)-1]
}

// waitFor waits up to 5 s for the live line to hold want, and says whether it did.
func (w *liveWin) waitFor(want string) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if strings.Contains(w.last(), want) {
			return true
		}
	}
	return false
}

// liveScreen opens the edit window as a liveWin and runs person on it, then closes it as the person does.
type liveScreen struct {
	*editScreen
	person func(text macvm.WindowText, w *liveWin)
}

func (s *liveScreen) Open(vm unsafe.Pointer, text macvm.WindowText) (macvm.Window, error) {
	w, err := s.fakeScreen.Open(vm, text)
	if err != nil {
		return nil, err
	}
	lw := &liveWin{Window: w}
	go func() {
		s.person(text, lw)
		text.AskStop()
	}()
	return lw, nil
}

// buttonEdit is an edit whose window boot's guest is a buttonGuest, whose base's tools.json holds items and
// agent signature sig, and whose window person runs on the live window. The live line is read every
// 10 ms.
func buttonEdit(t *testing.T, items []macvm.ItemRecord, sig string, person func(macvm.WindowText, *liveWin)) (*editFix, *buttonGuest) {
	t.Helper()
	t.Cleanup(macvm.SetEditLiveEvery(10 * time.Millisecond))
	e := newEdit(t)
	rec, err := macvm.ReadToolsRecord(e.base)
	if err != nil {
		t.Fatal(err)
	}
	rec.Items, rec.AgentSignature = items, sig
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(e.base, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	bg := &buttonGuest{trusted: map[string]bool{}, fda: "refused"}
	e.opts.WindowBoot = func(context.Context, macvm.MachineSpec, *os.File) (macvm.ToolsGuest, func(context.Context) error, error) {
		bg.windowGuest = &windowGuest{editGuest: e.g, done: make(chan struct{})}
		return bg, func(context.Context) error { return nil }, nil
	}
	e.opts.Screen = &liveScreen{editScreen: e.scr, person: person}
	return e, bg
}

var corpCA = []macvm.ItemRecord{{Kind: macvm.ItemKindCA, Name: "corp", State: macvm.ItemNotTrusted}}

// Each of the window's buttons asks the window boot's guest for its action and shows what came of it in the
// window: what to do next, or the guest's own words when it refused with nothing done or failed. Only an
// answer that never came says the click did not reach the guest agent.
func TestEditWindowButtonsAskTheGuestAndSayWhatHappened(t *testing.T) {
	var got []string
	var g *buttonGuest
	e, g := buttonEdit(t, corpCA, "", func(text macvm.WindowText, w *liveWin) {
		if !w.waitFor("Read by the guest now") {
			got = append(got, "no first read")
			return
		}
		for _, step := range []struct {
			click func()
			want  string
		}{
			{text.AdminTerminal, "Terminal is open in the VM at su - wardyn-admin. Type the one-time code there (Type code types it), then press Return."},
			{text.TrustCAs, "Terminal is open in the VM with 2 certificates to trust. Check each one, and type the one-time code each time it asks (Type code types it). In macOS's dialog, enter wardyn-admin and the code."},
			{func() { g.bmu.Lock(); g.refuse = true; g.bmu.Unlock(); text.TrustCAs() }, "Trust the list's CAs did nothing: nobody is signed in at the guest's console."},
			{text.FullDiskAccess, "System Settings is open at Full Disk Access in the VM, and Finder shows the guest agent: drag it into the list and turn it on."},
			{func() {
				g.setTermErr(fmt.Errorf("%w: the Terminal script was not written: %s", guestagent.ErrEditFailed, walkRefusal))
				text.AdminTerminal()
			},
				"Open admin Terminal failed in the VM: the Terminal script was not written: " + walkRefusal + "."},
			{func() { g.setTermErr(errors.New("the tools session is down")); text.AdminTerminal() },
				"Open admin Terminal did not reach the guest agent. Try again, or follow the steps above by hand."},
		} {
			if step.click == nil {
				got = append(got, "a button with no callback")
				return
			}
			step.click()
			if !w.waitFor(step.want) {
				got = append(got, fmt.Sprintf("after a click the window shows %q, want %q", w.last(), step.want))
			}
		}
	})
	if _, err := macvm.EditBase(editCtx(t), e.opts); err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		t.Error(f)
	}
	if want := []string{"admin-terminal", "trust-cas", "trust-cas", "full-disk-access", "admin-terminal", "admin-terminal"}; !slices.Equal(g.calls, want) {
		t.Errorf("the guest was asked %q, want %q", g.calls, want)
	}
}

// While the window is up it reads, every few seconds, each certificate's trust and Full Disk Access from the
// guest and shows them as the guest read them; a read that fails shows "could not be read", never trusted or
// allowed.
func TestEditWindowShowsTheGuestsLiveRead(t *testing.T) {
	var g *buttonGuest
	var got []string
	e, g := buttonEdit(t, corpCA, macvm.AgentSignedThisMac, func(_ macvm.WindowText, w *liveWin) {
		read := "Read by the guest now: certificate corp trusted; Full Disk Access allowed."
		if !w.waitFor(read) {
			got = append(got, fmt.Sprintf("the live line %q, want %q", w.last(), read))
		}
		g.bmu.Lock()
		g.down = true
		g.bmu.Unlock()
		unread := "Read by the guest now: certificate corp could not be read; Full Disk Access could not be read."
		if !w.waitFor(unread) {
			got = append(got, fmt.Sprintf("after the reads failed the live line %q, want %q", w.last(), unread))
		}
	})
	g.trusted["corp"], g.fda = true, "allowed"
	if _, err := macvm.EditBase(editCtx(t), e.opts); err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		t.Error(f)
	}
}

// Trust the list's CAs is enabled only when it has a ca item to trust, whatever the agent's signature. With
// no ca item the window says the list has none.
func TestEditWindowTrustNeedsACertificate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []macvm.ItemRecord
		sig   string
		want  bool
	}{
		{"no certificate", nil, "", false},
		{"signed on this Mac, no ca item", nil, macvm.AgentSignedThisMac, false},
		{"a ca item", corpCA, "", true},
		{"a ca item, signed on this Mac", corpCA, macvm.AgentSignedThisMac, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var canTrust bool
			var line string
			e, _ := buttonEdit(t, tc.items, tc.sig, func(text macvm.WindowText, w *liveWin) {
				canTrust = text.CanTrust
				w.waitFor("Read by the guest now")
				line = w.last()
			})
			if _, err := macvm.EditBase(editCtx(t), e.opts); err != nil {
				t.Fatal(err)
			}
			if canTrust != tc.want {
				t.Errorf("Trust the list's CAs enabled %v, want %v", canTrust, tc.want)
			}
			if none := strings.HasPrefix(line, "The install list has no certificates. "); none != (tc.items == nil) {
				t.Errorf("the live line %q", line)
			}
		})
	}
}

// The window's Type code key table (screen_darwin.m, built only on a Mac, pinned by its text) has a key for
// each letter of the code's alphabet and for nothing else, each the US key of that letter, and no Return: the
// person presses it. Its buttons come in the order a person uses them, and the numbers they pass to Go are
// the Go side's.
func TestEditWindowTypeCodeKeysAndButtons(t *testing.T) {
	read := func(f string) string {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	m, g := read("screen_darwin.m"), read("screen_darwin.go")
	_, table, _ := strings.Cut(m, "static int wardynScreenKeyCode(unichar c) {\n")
	table, _, _ = strings.Cut(table, "\n}\n")
	var letters []byte
	for _, l := range strings.Split(table, "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "case '") {
			continue
		}
		c := l[len("case '")]
		if want := fmt.Sprintf("case '%c': return kVK_ANSI_%c;", c, c-'a'+'A'); l != want {
			t.Errorf("key table line %q, want %q", l, want)
		}
		letters = append(letters, c)
	}
	if string(letters) != layoutStable {
		t.Errorf("the key table types %q, want the code's alphabet %q", letters, layoutStable)
	}
	for _, banned := range []string{"kVK_Return", "kVK_ANSI_KeypadEnter", "modifierFlags:NSEventModifierFlag"} {
		if strings.Contains(m, banned) {
			t.Errorf("screen_darwin.m mentions %s: Type code presses only the code's letters, unshifted", banned)
		}
	}
	order := []string{
		`add(wardynScreenAdminTerminal, @selector(act:), WARDYN_WINDOW_ADMIN_TERMINAL);`,
		`add(wardynScreenTypeCode, @selector(typeCode:), 0);`,
		`NSButton *trust = add(wardynScreenTrustCAs, @selector(act:), WARDYN_WINDOW_TRUST_CAS);`,
		`add(wardynScreenFullDiskAccess, @selector(act:), WARDYN_WINDOW_FULL_DISK_ACCESS);`,
		`add(wardynScreenInstallRosetta, @selector(act:), WARDYN_WINDOW_INSTALL_ROSETTA);`,
	}
	at := 0
	for _, want := range order {
		i := strings.Index(m[at:], want)
		if i < 0 {
			t.Fatalf("screen_darwin.m lost or reordered the button:\n%s", want)
		}
		at += i
	}
	for _, want := range []string{
		`wardynScreenAdminTerminal = @"Open admin Terminal";`, `wardynScreenTypeCode = @"Type code";`,
		`wardynScreenTrustCAs = @"Trust the list's CAs";`, `wardynScreenFullDiskAccess = @"Full Disk Access";`, `wardynScreenInstallRosetta = @"Install Rosetta";`,
		`wardynScreenNoCerts = @"The install list has no certificates.";`,
		"if (!canTrust) {\n\t\t\ttrust.enabled = NO;\n\t\t\ttrust.toolTip = wardynScreenNoCerts;",
		"#define WARDYN_WINDOW_ASK_STOP 0\n#define WARDYN_WINDOW_ADMIN_TERMINAL 1\n#define WARDYN_WINDOW_TRUST_CAS 2\n#define WARDYN_WINDOW_FULL_DISK_ACCESS 3\n#define WARDYN_WINDOW_INSTALL_ROSETTA 4\n",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("screen_darwin.m lost:\n%s", want)
		}
	}
	if want := "\twindowActionAskStop = iota\n\twindowActionAdminTerminal\n\twindowActionTrustCAs\n\twindowActionFullDiskAccess\n\twindowActionInstallRosetta\n"; !strings.Contains(g, want) {
		t.Errorf("screen_darwin.go's action numbers are not\n%s", want)
	}
}

// A second Type code click while the first one's keys are still being pressed does nothing (screen_darwin.m,
// pinned by its text): the click is refused before the code file is read, the button is off and the flag set
// before the first key is scheduled, and both are cleared only after the last key, so two codes never
// interleave.
func TestEditWindowTypeCodeIgnoresASecondClickWhileTyping(t *testing.T) {
	b, err := os.ReadFile("screen_darwin.m")
	if err != nil {
		t.Fatal(err)
	}
	_, body, ok := strings.Cut(string(b), "- (void)typeCode:(id)sender {\n")
	if !ok {
		t.Fatal("screen_darwin.m has no typeCode: click action")
	}
	body, _, _ = strings.Cut(body, "\n}\n")
	at := func(s string) int {
		i := strings.Index(body, s)
		if i < 0 {
			t.Fatalf("typeCode lost:\n%s", s)
		}
		return i
	}
	guard, read := at("\tif (self.typing) {\n\t\treturn;\n\t}\n"), at("wardynScreenReadCode(self.codeFile)")
	on, off, first := at("\tself.typing = YES;\n\tbutton.enabled = NO;\n"), at("\t\tstrip.typing = NO;\n\t\tbutton.enabled = YES;\n"), at("dispatch_after(")
	last := strings.LastIndex(body, "dispatch_after(dispatch_time(DISPATCH_TIME_NOW, (int64_t)(code.length * 50 * NSEC_PER_MSEC))")
	switch {
	case guard > read:
		t.Error("a click reads the code file before it checks for a sequence in progress")
	case on > first:
		t.Error("the first key is scheduled before the sequence is marked in progress")
	case last < 0 || last > off || !strings.Contains(body, "(int64_t)(i * 50 * NSEC_PER_MSEC)"):
		t.Error("the sequence is not cleared after its last key (keys at i*50 ms, the clear at length*50 ms)")
	}
}

// shutGuest is an edit window's guest whose agent shuts the guest down itself: EditShutDown stops the VM, as
// the guest's own shutdown does, or fails with err.
type shutGuest struct {
	*windowGuest
	smu   sync.Mutex
	asked int
	err   error
}

func (g *shutGuest) EditShutDown(context.Context) error {
	g.smu.Lock()
	defer g.smu.Unlock()
	g.asked++
	if g.err != nil {
		return g.err
	}
	g.end()
	return nil
}

// Closing the edit window asks the guest agent to shut the guest down, which macOS does not confirm with its
// "Are you sure you want to shut down your computer now?" dialog; the VM's stop request, which macOS does
// confirm, is not sent. An agent that does not accept (one without the action) gets the VM's stop request
// instead, and the edit goes on to its check either way.
func TestEditWindowCloseShutsTheGuestDownThroughTheAgent(t *testing.T) {
	for _, fail := range []bool{false, true} {
		e := newEdit(t)
		var sg *shutGuest
		e.opts.WindowBoot = func(context.Context, macvm.MachineSpec, *os.File) (macvm.ToolsGuest, func(context.Context) error, error) {
			sg = &shutGuest{windowGuest: &windowGuest{editGuest: e.g, done: make(chan struct{})}}
			if fail {
				sg.err = fmt.Errorf("%w: \"404 page not found\"", guestagent.ErrNoExec)
			}
			return sg, func(context.Context) error { return nil }, nil
		}
		if _, err := macvm.EditBase(editCtx(t), e.opts); err != nil {
			t.Fatalf("agent fails %v: %v", fail, err)
		}
		wantStops := -1 // the VM stopped with no stop request
		if fail {
			wantStops = 1
		}
		if sg.asked != 1 || sg.stopsAsked() != wantStops {
			t.Errorf("agent fails %v: the agent was asked %d times and the VM's stop request %d, want 1 and %d", fail, sg.asked, sg.stopsAsked(), wantStops)
		}
	}
}
