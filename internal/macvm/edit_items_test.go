// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// readBackGuest is the edit's guest whose verify-cert fails with verifyErr when it is set: a read-back that
// cannot finish. seq is the pushes, removals, posture reads and verify-certs in order, each with the codes
// set by then.
type readBackGuest struct {
	*editGuest
	verifyErr error
	seq       []string
}

func (g *readBackGuest) note(what string) {
	g.editGuest.mu.Lock()
	n := len(g.codes)
	g.editGuest.mu.Unlock()
	g.seq = append(g.seq, fmt.Sprintf("%s after %d codes", what, n))
}

func (g *readBackGuest) Posture(ctx context.Context) (guestagent.ToolsPosture, error) {
	g.note("posture")
	return g.editGuest.Posture(ctx)
}

func (g *readBackGuest) Push(_ context.Context, dest string, mode fs.FileMode, r io.Reader, _ int64) error {
	_, _ = io.Copy(io.Discard, r)
	g.note(fmt.Sprintf("push %s %o", dest, mode))
	return nil
}

func (g *readBackGuest) Run(ctx context.Context, argv []string) (string, int, error) {
	if argv[0] == "/bin/rm" {
		g.note(strings.Join(argv, " "))
	}
	if len(argv) > 1 && argv[1] == "verify-cert" {
		g.note("verify-cert")
		if g.verifyErr != nil {
			return "", 0, g.verifyErr
		}
	}
	return g.editGuest.Run(ctx, argv)
}

// newItemsEdit is an edit of a base whose tools.json holds a staged profile and a CA read as not trusted,
// and no guest posture; the install list names the profile's file on this Mac, whose path it returns.
func newItemsEdit(t *testing.T) (*editFix, *readBackGuest, string) {
	t.Helper()
	e := newEdit(t)
	rec, err := macvm.ReadToolsRecord(e.base)
	if err != nil {
		t.Fatal(err)
	}
	wifi, pin := itemFile(t, t.TempDir(), "wifi.mobileconfig", "<plist>wifi</plist>")
	e.opts.Items = macvm.BaseList{Version: 1, Items: []macvm.BaseItem{
		{Kind: macvm.ItemKindProfile, Name: "wifi", Path: wifi, SHA256: pin, Identifier: "com.example.wifi"}}}
	rec.Guest, rec.Items = nil, []macvm.ItemRecord{
		{Kind: macvm.ItemKindProfile, Name: "wifi", SHA256: pin, State: macvm.ItemStaged, Identifier: "com.example.wifi"},
		{Kind: macvm.ItemKindCA, Name: "orgca", SHA256: strings.Repeat("b", 64), State: macvm.ItemNotTrusted, SHA1: strings.Repeat("C", 40)},
	}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(e.base, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	g := &readBackGuest{editGuest: e.g}
	e.opts.Guest = func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
		e.boots++
		return g, func(context.Context) error { return nil }, nil
	}
	return e, g, wifi
}

// again readies e for another edit: its window's guest shuts down once the window is up.
func (e *editFix) again(t *testing.T) {
	boots := e.f.fb.Boots()
	opened0, _ := e.f.fb.Windows()
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
		noWindow := func() bool { opened, _ := e.f.fb.Windows(); return opened == opened0 }
		if wait(func() bool { return e.f.fb.Boots() == boots }) && wait(noWindow) {
			e.f.fb.Shutdown(nil)
		}
	}()
	e.out.Reset()
}

const (
	wifiPush = "push /Library/Wardyn/stage/wifi/item.mobileconfig 600"
	wifiLine = "To install a staged profile: signed in as wardyn-admin, run sudo open /Library/Wardyn/stage/wifi/item.mobileconfig in Terminal, then approve it in System Settings."
)

// The edit stages each waiting profile again before the window, 0600 as prepare does, and names it by its
// path. The recheck's boot removes it (StagedProfiles, of the base) and reads the items back
// (ReadBackItems) after the accounts and Remote Login, into the copy, whose tools.json is swapped in with
// it.
func TestEditBaseNamesProfilesThenReadsItemsBack(t *testing.T) {
	e, g, _ := newItemsEdit(t)
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	lines := e.lines()
	if len(lines) != 5 || lines[2] != wifiLine || lines[3] != macvm.EditCheckingLine {
		t.Fatalf("lines:\n%s", e.out.String())
	}
	want := []string{wifiPush + " after 1 codes", "/bin/rm -rf /Library/Wardyn/stage/wifi after 2 codes", "posture after 2 codes", "verify-cert after 2 codes"}
	if !slices.Equal(g.seq, want) {
		t.Fatalf("the edit's boots ran %q, want the profile staged before the window, then removed, then the read-back after the posture", g.seq)
	}
	rec, err := macvm.ReadToolsRecord(e.base)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Guest == nil || len(rec.Items) != 2 || rec.Items[1].State != macvm.ItemTrusted || rec.Items[0].State != macvm.ItemStaged {
		t.Fatalf("the swapped-in base's record was not read back: %+v", rec)
	}
}

// A read-back that cannot finish discards the copy: runs keep the old base and its record. None of the
// discard lines (MAC-E09, E10, E11, E18) names it, so it is a plain error and no recheck is recorded.
func TestEditBaseDiscardsOnAFailedReadBack(t *testing.T) {
	e, g, _ := newItemsEdit(t)
	g.verifyErr = errors.New("guest agent gone")
	_, err := macvm.EditBase(context.Background(), e.opts)
	var discarded *macvm.EditDiscardedError
	if err == nil || !strings.Contains(err.Error(), "items read-back") || errors.As(err, &discarded) {
		t.Fatalf("err %v", err)
	}
	e.oldBaseKept(t)
	rec, err := macvm.ReadToolsRecord(e.base)
	if err != nil || rec.Guest != nil || rec.Items[1].State != macvm.ItemNotTrusted {
		t.Fatalf("the old base's record changed: %+v %v", rec, err)
	}
	m, _ := macvm.ReadyBase(e.base)
	if r, ok := macvm.ReadEditRecord(e.base, m); ok && r.Recheck == macvm.EditRecheckPassed {
		t.Errorf("a discarded edit was recorded as passed: %+v", r)
	}
}

// A second edit, after the first one's recheck removed the staged file, pushes it again from the install
// list and prints its path. With the file gone from this Mac, an edit pushes and prints nothing, and the
// profile stays staged.
func TestEditBaseRestagesTheWaitingProfiles(t *testing.T) {
	e, g, wifi := newItemsEdit(t)
	pushed := func() int {
		return len(slices.DeleteFunc(slices.Clone(g.seq), func(s string) bool { return !strings.HasPrefix(s, wifiPush+" ") }))
	}
	for i := range 2 {
		if i > 0 {
			e.again(t)
		}
		g.seq = nil
		if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
			t.Fatalf("edit %d: %v", i+1, err)
		}
		if pushed() != 1 || !slices.Contains(e.lines(), wifiLine) || !slices.Contains(g.seq, "/bin/rm -rf /Library/Wardyn/stage/wifi after "+fmt.Sprint(2*i+2)+" codes") {
			t.Fatalf("edit %d: ran %q, printed\n%s", i+1, g.seq, e.out.String())
		}
	}
	if err := os.Remove(wifi); err != nil {
		t.Fatal(err)
	}
	e.again(t)
	g.seq = nil
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	if pushed() != 0 || slices.Contains(e.lines(), wifiLine) {
		t.Fatalf("with no file on this Mac: ran %q, printed\n%s", g.seq, e.out.String())
	}
	if rec, err := macvm.ReadToolsRecord(e.base); err != nil || rec.Items[0].State != macvm.ItemStaged {
		t.Fatalf("record %+v %v", rec, err)
	}
}
