// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// editGuest is a fake stage-2 guest for edit-base: the tools guest plus the admin code and posture, and a
// control probe that moves its pointer when control is on.
type editGuest struct {
	*guest
	mu      sync.Mutex
	codes   []string
	posture guestagent.ToolsPosture
	control bool
}

func (g *editGuest) AdminCode(_ context.Context, code []byte) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.codes = append(g.codes, string(code))
	return true, nil
}

func (g *editGuest) Posture(context.Context) (guestagent.ToolsPosture, error) { return g.posture, nil }

func (g *editGuest) MovePointer(_ context.Context, _ []byte, x, y int) error {
	if g.control {
		g.guest.mu.Lock()
		g.pointerX, g.pointerY = x, y
		g.guest.mu.Unlock()
	}
	return nil
}

// editFix is a prepared base (its desktop observe, so a swap shows) and an edit of it whose window's guest
// shuts down once the window is up, reading the code file as the person would.
type editFix struct {
	f        *prepFixture
	g        *editGuest
	opts     macvm.EditOptions
	out      bytes.Buffer
	boots    int
	fbBoots  int // the first boots before the edit's (prepare's)
	codeSeen macvm.SetupCodeRecord
	base     string
}

func newEdit(t *testing.T) *editFix {
	t.Helper()
	f, _, _ := stage2Prep(t)
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	e := &editFix{f: f, base: f.opts.Layout.BaseDir(0)}
	m.Desktop, m.Harnesses = macvm.DesktopObserve, []string{macvm.HarnessNone, macvm.HarnessClaudeCode}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(e.base, macvm.ManifestFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	e.g = &editGuest{guest: newGuest(), control: true, posture: guestagent.ToolsPosture{Accounts: []guestagent.GuestAccount{
		{Name: "root", UID: 0}, {Name: "nobody", UID: -2}, {Name: "_spotlight", UID: 89}, {Name: "wardyn-admin", UID: 501}, {Name: "agent", UID: 502}}}}
	e.opts = macvm.EditOptions{
		Engine: f.eng, Layout: f.opts.Layout, FirstBoot: f.fb, PrepNet: f.opts.PrepNet, CPUs: 4, MemoryMiB: 8192, Out: &e.out,
		Guest: func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
			e.boots++
			return e.g, func(context.Context) error { return nil }, nil
		},
		Now: func() time.Time { return time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC) },
	}
	e.fbBoots = f.fb.Boots()
	opened0, _ := f.fb.Windows()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	wait := func(more func() bool) bool {
		for more() {
			select {
			case <-stop:
				return false
			case <-time.After(time.Millisecond):
			}
		}
		return true
	}
	go func() {
		noWindow := func() bool { opened, _ := f.fb.Windows(); return opened == opened0 }
		if !wait(func() bool { return f.fb.Boots() == e.fbBoots }) || !wait(noWindow) {
			return
		}
		b, _ := os.ReadFile(f.opts.Layout.SetupCode())
		_ = json.Unmarshal(b, &e.codeSeen)
		f.fb.Shutdown(nil)
	}()
	return e
}

func (e *editFix) lines() []string { return strings.Split(strings.TrimSpace(e.out.String()), "\n") }

// The base the edit started from is still the base, its staging copy and code file are gone, and no
// window or boot is left.
func (e *editFix) oldBaseKept(t *testing.T) {
	t.Helper()
	if m, err := macvm.ReadyBase(e.base); err != nil || m.Desktop != macvm.DesktopObserve {
		t.Fatalf("the old base was not kept: %+v %v", m, err)
	}
	for _, p := range []string{e.base + ".update", e.f.opts.Layout.SetupCode()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind", p)
		}
	}
}

// A passing edit: the code is set, given to the person in its file and replaced after the session; the
// copy is checked and swapped in, its edit recorded, and the lines are the approved ones in order.
func TestEditBaseSwapsOnlyAPassingCopy(t *testing.T) {
	e := newEdit(t)
	m, err := macvm.EditBase(context.Background(), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := macvm.ReadyBase(e.base)
	if err != nil || got.Desktop != macvm.DesktopControl || m.Desktop != macvm.DesktopControl {
		t.Fatalf("the edited base was not swapped in: %+v %v", got, err)
	}
	if !slices.Equal(got.Harnesses, []string{macvm.HarnessNone, macvm.HarnessClaudeCode}) {
		t.Errorf("harnesses %v", got.Harnesses)
	}
	if _, err := macvm.ReadToolsRecord(e.base); err != nil {
		t.Errorf("the edited base lost its tools record: %v", err)
	}
	if e.boots != 2 || e.f.fb.Boots()-e.fbBoots != 1 {
		t.Fatalf("boots: %d headless, %d windowed", e.boots, e.f.fb.Boots()-e.fbBoots)
	}
	if a := e.f.fb.Last(); a.Username != "" || a.Password != "" || !strings.HasSuffix(a.Disk, ".update/"+macvm.DiskFile) {
		t.Errorf("the window booted %+v, want the staging copy, plain", a)
	}
	if len(e.g.codes) != 2 || e.g.codes[0] == e.g.codes[1] || e.codeSeen.Code != e.g.codes[0] || e.codeSeen.Account != "wardyn-admin" {
		t.Fatalf("codes %q, the file held %+v", e.g.codes, e.codeSeen)
	}
	want := []string{
		"Editing this Mac's macOS base. Runs keep using the current base until the edited one passes its checks.",
		"Sign in as wardyn-admin with the one-time code in " + e.f.opts.Layout.SetupCode() + ".",
		"The window closed and the guest shut down. Checking the edited base: desktop control, accounts, Remote Login and the launcher's self-test.",
		"The edited base is ready, and runs now use it. Desktop: control.",
	}
	if !slices.Equal(e.lines(), want) {
		t.Fatalf("lines:\n%s", e.out.String())
	}
	if strings.Contains(e.out.String(), e.g.codes[0]) {
		t.Error("the code was printed")
	}
	if at := macvm.BaseEditedAt(e.base, got); at == nil || !at.Equal(e.opts.Now()) {
		t.Errorf("edited at %v", at)
	}
	if rec, ok := macvm.ReadEditRecord(e.base, got); !ok || rec.Recheck != macvm.EditRecheckPassed {
		t.Errorf("record %+v %v", rec, ok)
	}
	if _, err := os.Stat(e.f.opts.Layout.SetupCode()); !os.IsNotExist(err) {
		t.Error("the code file outlived the edit")
	}
	if opened, closed := e.f.fb.Windows(); opened != closed || opened == 0 {
		t.Errorf("windows opened %d closed %d", opened, closed)
	}
}

// Each failed check discards the copy with its own line and the old base's date, records why, and runs
// keep the old base.
func TestEditBaseDiscardsOnEachFailedCheck(t *testing.T) {
	for _, tc := range []struct {
		name, recheck, line string
		set                 func(g *editGuest)
	}{
		{"desktop control off", macvm.EditRecheckDesktopOff,
			"The edited base was discarded: desktop control is off, because Screen Sharing was not turned on with control. Runs keep using the base from ",
			func(g *editGuest) { g.control = false }},
		{"extra account", macvm.EditRecheckExtraAccount,
			"The edited base was discarded: it has an account Wardyn didn't make, tester. Runs keep using the base from ",
			func(g *editGuest) {
				g.posture.Accounts = append(g.posture.Accounts, guestagent.GuestAccount{Name: "tester", UID: 503})
			}},
		{"hidden account", macvm.EditRecheckExtraAccount,
			"The edited base was discarded: it has an account Wardyn didn't make, hidden. Runs keep using the base from ",
			func(g *editGuest) {
				g.posture.Accounts = append(g.posture.Accounts, guestagent.GuestAccount{Name: "hidden", UID: 400})
			}},
		{"Remote Login on", macvm.EditRecheckRemoteLogin,
			"The edited base was discarded: Remote Login is on. Runs keep using the base from ",
			func(g *editGuest) { g.posture.RemoteLogin = true }},
		{"self-test of a harness", macvm.EditRecheckSelftest,
			"The edited base was discarded: the launcher's self-test failed for claude-code. Runs keep using the base from ",
			func(g *editGuest) {
				g.selftest = func(env []string) (string, int) {
					if slices.Contains(env, "WARDYN_START_HARNESS=claude-code") {
						return "FAIL", 1
					}
					return "", 0
				}
			}},
		{"self-test of the command run", macvm.EditRecheckSelftest,
			"The edited base was discarded: the launcher's self-test failed for none. Runs keep using the base from ",
			func(g *editGuest) { g.selftest = func([]string) (string, int) { return "FAIL", 1 } }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEdit(t)
			tc.set(e.g)
			before, _ := macvm.ReadyBase(e.base)
			_, err := macvm.EditBase(context.Background(), e.opts)
			var d *macvm.EditDiscardedError
			if !errors.As(err, &d) || d.Recheck != tc.recheck {
				t.Fatalf("err = %v", err)
			}
			if want := tc.line + before.CreatedAt.Local().Format("2 Jan") + "."; err.Error() != want {
				t.Fatalf("line %q, want %q", err.Error(), want)
			}
			e.oldBaseKept(t)
			if strings.Contains(e.out.String(), "is ready") {
				t.Error("a discarded edit said it is ready")
			}
			if rec, ok := macvm.ReadEditRecord(e.base, before); !ok || rec.Recheck != tc.recheck || !rec.EditedAt.IsZero() {
				t.Errorf("record %+v %v", rec, ok)
			}
			if macvm.BaseEditedAt(e.base, before) != nil {
				t.Error("a discarded edit reads as an edit")
			}
			if len(e.g.codes) != 2 {
				t.Errorf("the one-time code was not replaced: %d codes", len(e.g.codes))
			}
		})
	}
}

// A copy whose swap fails leaves the old base as it was: a base is never half swapped.
func TestEditBaseFailedSwapKeepsOldBase(t *testing.T) {
	e := newEdit(t)
	defer macvm.SetSwapHook(func() { _ = os.WriteFile(filepath.Join(e.base+".update", "stray"), nil, 0o600) })()
	if _, err := macvm.EditBase(context.Background(), e.opts); err == nil {
		t.Fatal("an edit whose new base held a stray file was swapped in")
	}
	e.oldBaseKept(t)
	if m, _ := macvm.ReadyBase(e.base); macvm.BaseEditedAt(e.base, m) != nil {
		t.Error("a failed swap was recorded as an edit")
	}
}

// The refusals start nothing: a prepare or tools update holds the update lock, there is no ready base, or
// this process has no desktop (no window session, or root).
func TestEditBaseRefusals(t *testing.T) {
	e := newEdit(t)
	unlock, err := macvm.LockUpdate(e.base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := macvm.EditBase(context.Background(), e.opts); !errors.Is(err, macvm.ErrEditBusy) ||
		err.Error() != "This Mac is preparing or updating its macOS base. Run edit-base when that finishes." {
		t.Fatalf("busy: %v", err)
	}
	unlock()
	o := e.opts
	o.Layout = macvm.Layout{Root: t.TempDir()}
	if _, err := macvm.EditBase(context.Background(), o); !errors.Is(err, macvm.ErrEditNoBase) ||
		err.Error() != "This Mac has no ready macOS base to edit. Run `wardyn-runnerd macos-vm prepare` first." {
		t.Fatalf("no base: %v", err)
	}
	e.f.fb.NoScreen = macvm.ErrNoDesktopSession
	const noDesktop = "edit-base needs this Mac's desktop: run it in Terminal on the Mac, signed in at its desktop, not over SSH and not with sudo."
	if _, err := macvm.EditBase(context.Background(), e.opts); !errors.Is(err, macvm.ErrEditNoDesktop) || err.Error() != noDesktop {
		t.Fatalf("no desktop: %v", err)
	}
	e.f.fb.NoScreen = nil
	restore := macvm.SetGeteuid(0)
	_, err = macvm.EditBase(context.Background(), e.opts)
	restore()
	if !errors.Is(err, macvm.ErrEditNoDesktop) {
		t.Fatalf("root: %v", err)
	}
	if e.boots != 0 || e.f.fb.Boots() != e.fbBoots || e.out.Len() != 0 {
		t.Fatalf("a refused edit booted (%d, %d) or printed %q", e.boots, e.f.fb.Boots()-e.fbBoots, e.out.String())
	}
	if macvm.ErrEditSlots.Error() != "Editing uses one of this Mac's 2 macOS VM slots, and both are in use. Try again when a run ends." {
		t.Error("slots refusal")
	}
}
