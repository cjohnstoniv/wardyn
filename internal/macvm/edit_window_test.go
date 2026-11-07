// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// The edit's window boots the staging copy on a tools boot (the window boot: the guest agent's control and
// tools sessions, as the edit's headless boots have), on a NIC the no-egress prep network serves, and never
// on a first boot's provisioner.
func TestEditWindowBootsOnAToolsBoot(t *testing.T) {
	e := newEdit(t)
	firstBoots := e.f.fb.Boots()
	nets := 0
	prep := e.opts.PrepNet
	e.opts.PrepNet = func() (macvm.PrepNetwork, error) { nets++; return prep() }
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if len(e.windows) != 1 || e.boots != 2 {
		t.Fatalf("%d window boots and %d headless boots, want 1 and 2", len(e.windows), e.boots)
	}
	if n := e.f.fb.Boots() - firstBoots; n != 0 {
		t.Fatalf("the edit booted the first boot's provisioner %d times", n)
	}
	if d := e.specs[0].DiskPath; !strings.HasSuffix(d, ".update/"+macvm.DiskFile) {
		t.Errorf("the window booted %s, want the staging copy", d)
	}
	if e.nics[0] == nil || nets != 1 {
		t.Errorf("the window boot's NIC %v, prep networks %d: want the one prep network's", e.nics[0], nets)
	}
}

// The window's close button asks the window boot's guest to shut down; the edit waits until the guest has
// stopped, then closes the window and rechecks the copy. A guest that ignores the close keeps the window up
// and the edit waiting, and the copy is never swapped in.
func TestEditWindowCloseAsksTheGuestToShutDown(t *testing.T) {
	e := newEdit(t)
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if n := e.windows[0].stopsAsked(); n != 1 {
		t.Fatalf("the close asked the guest to shut down %d times, want 1", n)
	}
	if n := e.scr.win(0).closes(); n != 1 {
		t.Errorf("the window was closed %d times, want once after the guest stopped", n)
	}
	if e.boots != 2 {
		t.Errorf("%d headless boots: the recheck did not run after the window", e.boots)
	}

	e = newEdit(t)
	e.ignore = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	click := e.scr.onOpen
	e.scr.onOpen = func(text macvm.WindowText) {
		click(text)
		time.Sleep(100 * time.Millisecond) // the edit keeps waiting for a guest that has not stopped
		cancel()
	}
	_, err := macvm.EditBase(ctx, e.opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want the edit still waiting for the guest when its context ended", err)
	}
	if e.windows[0].stopsAsked() != 1 || e.boots != 1 {
		t.Fatalf("stops asked %d, headless boots %d: want the close asked and no recheck", e.windows[0].stopsAsked(), e.boots)
	}
	if n := e.scr.win(0).closes(); n != 1 {
		t.Errorf("the window was closed %d times when the edit ended", n)
	}
	e.oldBaseKept(t)
}

// The edit window draws the window boot's VM with the edit's title and steps and the strip's Show code
// control, which reads the one-time code from the setup-code file: the file names the code the guest took.
func TestEditWindowGetsTheCodeControl(t *testing.T) {
	e := newEdit(t)
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if e.scr.opens() != 1 {
		t.Fatalf("%d windows opened", e.scr.opens())
	}
	text := e.scr.texts[0]
	if text.Title != "Wardyn: edit this macOS VM base" || !strings.HasPrefix(text.Strip, "Sign in as wardyn-admin with the one-time code.") {
		t.Errorf("the window's text %+v", text)
	}
	if text.CodeFile != e.f.opts.Layout.SetupCode() || text.AskStop == nil {
		t.Errorf("the window's code file %q (want %q), close control set %v", text.CodeFile, e.f.opts.Layout.SetupCode(), text.AskStop != nil)
	}
	if e.scr.vms[0] != unsafe.Pointer(&vmByte) {
		t.Errorf("the window drew %v, not the window boot's VM", e.scr.vms[0])
	}
	if e.codeSeen.Account != "wardyn-admin" || e.codeSeen.Code == "" || e.codeSeen.Code != e.g.codes[0] {
		t.Errorf("the code file held %+v while the window was up, want the code the guest took", e.codeSeen)
	}
}
