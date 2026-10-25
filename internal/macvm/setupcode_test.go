// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
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
