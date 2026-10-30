// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// readBackGuest is the edit's guest whose verify-cert fails with verifyErr when it is set: a read-back that
// cannot finish.
type readBackGuest struct {
	*editGuest
	verifyErr error
}

func (g *readBackGuest) Run(ctx context.Context, argv []string) (string, int, error) {
	if g.verifyErr != nil && len(argv) > 1 && argv[1] == "verify-cert" {
		return "", 0, g.verifyErr
	}
	return g.editGuest.Run(ctx, argv)
}

// newItemsEdit is an edit of a base whose tools.json holds a staged profile and a CA read as not trusted,
// and no guest posture.
func newItemsEdit(t *testing.T) (*editFix, *readBackGuest) {
	t.Helper()
	e := newEdit(t)
	rec, err := macvm.ReadToolsRecord(e.base)
	if err != nil {
		t.Fatal(err)
	}
	rec.Guest, rec.Items = nil, []macvm.ItemRecord{
		{Kind: macvm.ItemKindProfile, Name: "wifi", SHA256: strings.Repeat("a", 64), State: macvm.ItemStaged},
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
	return e, g
}

// runAt is the index of argv in the guest's runs, -1 when it never ran.
func runAt(e *editFix, argv ...string) int {
	e.g.guest.mu.Lock()
	defer e.g.guest.mu.Unlock()
	return slices.IndexFunc(e.g.guest.runs, func(r []string) bool { return slices.Equal(r, argv) })
}

var (
	offerArgv    = []string{"/usr/bin/ditto", guestagent.ItemDir("wifi") + "/" + guestagent.ItemProfile, guestagent.ProfileOffer("wifi")}
	withdrawArgv = []string{"/bin/rm", "-rf", guestagent.MacProfileOffer}
	verifyArgv   = []string{"/usr/bin/security", "verify-cert", "-L", "-p", "ssl", "-c", guestagent.ItemDir("orgca") + "/" + guestagent.ItemCert}
)

// The edit offers each staged profile before the window (StagedProfiles), names it to the person, then in
// the recheck removes the offer and reads the items back (ReadBackItems) into the copy, whose tools.json
// is swapped in with it.
func TestEditBaseOffersProfilesThenReadsItemsBack(t *testing.T) {
	e, _ := newItemsEdit(t)
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	offer, withdraw, verify := runAt(e, offerArgv...), runAt(e, withdrawArgv...), runAt(e, verifyArgv...)
	if offer < 0 || withdraw <= offer || verify <= withdraw {
		t.Fatalf("offer %d, its removal %d, the read-back %d: want them in that order", offer, withdraw, verify)
	}
	lines := e.lines()
	want := "To install the profile wifi, open /Users/Shared/Wardyn/wifi.mobileconfig in the guest, then approve it in System Settings."
	if len(lines) != 5 || lines[2] != want || lines[3] != macvm.EditCheckingLine {
		t.Fatalf("lines:\n%s", e.out.String())
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
	e, g := newItemsEdit(t)
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
