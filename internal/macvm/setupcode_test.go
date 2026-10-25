// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// handBootstrap is stage 1 after a setup by hand: it records its input and
// whether the setup-code file was still there when it replaced the code.
type handBootstrap struct {
	mu        sync.Mutex
	in        macvm.BootstrapInput
	codePath  string
	fileAfter bool
}

func (b *handBootstrap) Bootstrap(_ context.Context, in macvm.BootstrapInput) (macvm.BootstrapResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.in = in
	if in.Replaced != nil {
		in.Replaced()
	}
	_, err := os.Stat(b.codePath)
	b.fileAfter = err == nil
	return macvm.BootstrapResult{Desktop: macvm.DesktopNone, AgentVersion: "0.10.0", Harnesses: []string{"none"}}, nil
}

// handPrep is a prepare whose first boot asks for setup by hand: Remote
// Login answers only once the Mac's person has read the code from its file.
func handPrep(t *testing.T) (*prepFixture, *handBootstrap, *macvm.SetupCodeRecord) {
	t.Helper()
	f := newPrep(t)
	b := &handBootstrap{codePath: f.opts.Layout.SetupCode()}
	f.opts.Bootstrap = b
	f.opts.RemoteLoginWait = 50 * time.Millisecond
	f.opts.SetupByHandWait = 5 * time.Second
	seen := &macvm.SetupCodeRecord{}
	f.net.dialErr = errors.New("connection refused")
	f.net.onDial = func() {
		raw, err := os.ReadFile(f.opts.Layout.SetupCode())
		if err != nil || seen.Code != "" {
			return
		}
		fi, _ := os.Lstat(f.opts.Layout.SetupCode())
		if fi.Mode().Perm() != 0o600 || json.Unmarshal(raw, seen) != nil {
			t.Errorf("setup-code file mode %v, %s", fi.Mode(), raw)
		}
		f.net.dialErr = nil // the person finished Setup Assistant and turned Remote Login on
	}
	return f, b, seen
}

// The setup-by-hand path writes the code only to its 0600 file, emits
// setup_by_hand with no detail, logs in once with the code and has stage 1
// replace it at once, deleting the file; the code is in no log, event or
// other file.
func TestSetupByHandCodeLivesOnlyInItsFile(t *testing.T) {
	f, b, seen := handPrep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if seen.Account != "wardyn-admin" || len(seen.Code) != 8 {
		t.Fatalf("setup-code record %+v", seen)
	}
	// A host with provisioning (macOS 27) provisions first; setup by hand is its fallback, in a window.
	if args := f.fb.Last(); f.fb.Boots() != 1 || args.Username != "wardyn-admin" || len(args.Password) < 40 {
		t.Fatalf("first boot %d times as %q; want one provisioned boot", f.fb.Boots(), args.Username)
	}
	if opened, closed := f.fb.Windows(); opened != 1 || closed != 1 {
		t.Fatalf("windows opened %d, closed %d; want 1 and 1", opened, closed)
	}
	if b.in.Password != seen.Code || len(b.in.NewPassword) < 32 || b.in.NewPassword == seen.Code || b.fileAfter {
		t.Fatalf("stage 1 got password=code %v, a new password %v, file after replace %v",
			b.in.Password == seen.Code, len(b.in.NewPassword) >= 32, b.fileAfter)
	}
	var got bool
	for _, ev := range f.ev.evs {
		if ev.Phase == macvm.PhaseSetupByHand {
			got = true
			if ev.Detail != "" {
				t.Fatalf("setup_by_hand carries detail %q", ev.Detail)
			}
		}
	}
	if !got {
		t.Fatal("no setup_by_hand event")
	}
	if _, err := os.Stat(f.opts.Layout.SetupCode()); !os.IsNotExist(err) {
		t.Fatalf("the setup-code file outlived prepare: %v", err)
	}
	grepTree(t, f.opts.Layout.Root, seen.Code, f.logs.Bytes(), eventsJSON(t, f.ev))
}

// A setup by hand nobody finished stops prepare and deletes the code; with
// setup by hand off, prepare stops and writes no code at all.
func TestSetupByHandUnfinishedOrOffLeavesNoCode(t *testing.T) {
	f, _, _ := handPrep(t)
	f.net.onDial = nil
	f.opts.SetupByHandWait = 100 * time.Millisecond
	if _, err := macvm.Prepare(context.Background(), f.opts); !errors.Is(err, macvm.ErrManualSetup) {
		t.Fatalf("got %v, want ErrManualSetup", err)
	}
	if _, err := os.Stat(f.opts.Layout.SetupCode()); !os.IsNotExist(err) {
		t.Fatalf("an unfinished setup left its code: %v", err)
	}
	g, _, _ := handPrep(t)
	g.net.onDial, g.opts.SetupByHandWait = nil, 0
	var wrote bool
	g.ev.on = func(ev macvm.PrepareEvent) { wrote = wrote || ev.Phase == macvm.PhaseSetupByHand }
	if _, err := macvm.Prepare(context.Background(), g.opts); !errors.Is(err, macvm.ErrManualSetup) || wrote {
		t.Fatalf("setup by hand off: %v, setup_by_hand emitted %v", err, wrote)
	}
}

// The host floor is macOS 26: a 26 host runs macOS VMs (its base is set up by hand), a 25 host is
// refused with the reason, and a version that does not parse is refused too.
func TestCheckHostVersionFloorIs26(t *testing.T) {
	for v, ok := range map[string]bool{"26.0": true, "26.7": true, "27.0.1": true, "25.6": false, "15.4": false, "": false, "x.1": false} {
		err := macvm.CheckHostVersion(v)
		if (err == nil) != ok {
			t.Errorf("%q: %v", v, err)
		}
		if err != nil && !strings.Contains(err.Error(), "macOS VMs need macOS 26 or later") {
			t.Errorf("%q: the refusal %q does not name the floor", v, err)
		}
	}
}

// errHostNoProvisioning is what a macOS 26 host's provisioning check says.
var errHostNoProvisioning = fmt.Errorf("macvm provision: this Mac's Virtualization framework has no class VZMacGuestProvisioningOptions; %w needs macOS 27 or later", macvm.ErrNoProvisioning)

// On a host without guest provisioning, prepare boots the base plain and goes to setup by hand at once:
// no account in the boot, the code written before any dial of the guest (no Remote Login wait), the
// guest's window opened and closed again, and stage 1 logging in with the code.
func TestPrepareWithoutProvisioningGoesStraightToSetupByHand(t *testing.T) {
	f, b, seen := handPrep(t)
	f.fb.Unsupported = errHostNoProvisioning
	f.opts.RemoteLoginWait = time.Hour // waiting it out would time the test out
	var early atomic.Int32
	byHand := f.net.onDial
	f.net.onDial = func() {
		if _, err := os.Stat(f.opts.Layout.SetupCode()); err != nil {
			early.Add(1)
		}
		byHand()
	}
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if args := f.fb.Last(); f.fb.Boots() != 1 || args.Username != "" || args.Password != "" {
		t.Fatalf("first boot %d times, account %q, password set %v; want one plain boot", f.fb.Boots(), args.Username, args.Password != "")
	}
	if n := early.Load(); n != 0 {
		t.Fatalf("%d dials of the guest before the setup code existed: prepare waited for a provisioned Remote Login", n)
	}
	if b.in.Password != seen.Code || len(seen.Code) != 8 || len(b.in.NewPassword) < 32 {
		t.Fatalf("stage 1 got the code %v, a new password %v", b.in.Password == seen.Code, len(b.in.NewPassword) >= 32)
	}
	if opened, closed := f.fb.Windows(); opened != 1 || closed != 1 {
		t.Fatalf("windows opened %d, closed %d; want 1 and 1", opened, closed)
	}
}

// Without provisioning and with setup by hand off, prepare stops at its check, before any download or boot.
func TestPrepareWithoutProvisioningNeedsSetupByHand(t *testing.T) {
	f := newPrep(t)
	f.fb.Unsupported = errHostNoProvisioning
	if _, err := macvm.Prepare(context.Background(), f.opts); !errors.Is(err, macvm.ErrNoProvisioning) {
		t.Fatalf("got %v, want ErrNoProvisioning", err)
	}
	if f.srv.reqs.Load() != 0 || f.fb.Boots() != 0 {
		t.Fatalf("downloaded %d times, booted %d times", f.srv.reqs.Load(), f.fb.Boots())
	}
}

// A window that cannot open stops setup by hand at once and deletes the code: nobody could finish it.
func TestSetupByHandWithoutAWindowStops(t *testing.T) {
	f, _, _ := handPrep(t)
	f.fb.Unsupported = errHostNoProvisioning
	f.fb.ScreenErr = errors.New("no window server")
	_, err := macvm.Prepare(context.Background(), f.opts)
	if err == nil || !strings.Contains(err.Error(), "no window server") {
		t.Fatalf("got %v, want the window's error", err)
	}
	if _, err := os.Stat(f.opts.Layout.SetupCode()); !os.IsNotExist(err) {
		t.Fatalf("the setup-code file outlived prepare: %v", err)
	}
}

// errNoDesktopOverSSH is what a Mac's window check says over SSH.
var errNoDesktopOverSSH = fmt.Errorf("this process has no window-server session: %w", macvm.ErrNoDesktopSession)

// A prepare that will need setup by hand (no guest provisioning) and cannot show the guest's window, with no
// desktop session or as root, stops at its check: no download, no boot, nothing written under the root.
func TestPrepareWithoutADesktopStopsAtItsCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		noScreen error
		euid     int
		want     string
	}{
		"over SSH": {errNoDesktopOverSSH, 501, "no window-server session"},
		"as root":  {nil, 0, "prepare runs as root"},
	} {
		t.Run(name, func(t *testing.T) {
			defer macvm.SetGeteuid(tc.euid)()
			f := newPrep(t)
			f.fb.Unsupported, f.fb.NoScreen = errHostNoProvisioning, tc.noScreen
			f.opts.SetupByHandWait = time.Hour
			_, err := macvm.Prepare(context.Background(), f.opts)
			if !errors.Is(err, macvm.ErrNoDesktopSession) || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "run prepare in Terminal on the Mac, signed in at its desktop, not over SSH") {
				t.Fatalf("got %v, want the desktop refusal (%s)", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), macvm.NoDesktopFailurePrefix) {
				t.Fatalf("got %q, want the %q failure prefix", err, macvm.NoDesktopFailurePrefix)
			}
			if f.srv.reqs.Load() != 0 || f.fb.Boots() != 0 || f.eng.Installs() != 0 {
				t.Fatalf("downloaded %d, booted %d, installed %d", f.srv.reqs.Load(), f.fb.Boots(), f.eng.Installs())
			}
			if left, _ := os.ReadDir(f.opts.Layout.Root); len(left) != 0 {
				t.Fatalf("the refused prepare wrote %v under the root", left)
			}
			for _, ev := range f.ev.evs {
				if ev.Phase != macvm.PhaseCheck {
					t.Fatalf("the refused prepare reached %s", ev.Phase)
				}
			}
		})
	}
}

// A host with guest provisioning (macOS 27) needs no desktop session, nor a user that is not root: its
// prepare provisions as before and opens no window, since its setup by hand never runs.
func TestPrepareWithProvisioningNeedsNoDesktop(t *testing.T) {
	defer macvm.SetGeteuid(0)()
	f := newPrep(t)
	f.fb.NoScreen = errNoDesktopOverSSH
	f.opts.SetupByHandWait = time.Hour
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if args := f.fb.Last(); f.fb.Boots() != 1 || args.Username != "wardyn-admin" {
		t.Fatalf("first boot %d times as %q; want one provisioned boot", f.fb.Boots(), args.Username)
	}
	if opened, _ := f.fb.Windows(); opened != 0 {
		t.Fatalf("a provisioned prepare opened %d windows", opened)
	}
}

// On macOS 27, a provisioned boot that falls back to setup by hand where no window can open stops at once,
// not after the hour, and deletes the code: the check is the window's own.
func TestSetupByHandFallbackWithoutADesktopStopsAtOnce(t *testing.T) {
	f, _, _ := handPrep(t)
	f.fb.NoScreen = errNoDesktopOverSSH
	f.opts.SetupByHandWait = time.Hour // waiting it out would time the test out
	_, err := macvm.Prepare(context.Background(), f.opts)
	if !errors.Is(err, macvm.ErrNoDesktopSession) || !strings.HasPrefix(err.Error(), macvm.NoDesktopFailurePrefix) {
		t.Fatalf("got %v, want the desktop refusal with the %q failure prefix", err, macvm.NoDesktopFailurePrefix)
	}
	if args := f.fb.Last(); f.fb.Boots() != 1 || args.Username != "wardyn-admin" {
		t.Fatalf("first boot %d times as %q; want one provisioned boot", f.fb.Boots(), args.Username)
	}
	if _, err := os.Stat(f.opts.Layout.SetupCode()); !os.IsNotExist(err) {
		t.Fatalf("the setup-code file outlived prepare: %v", err)
	}
}
