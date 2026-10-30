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
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// readBackGuest is the edit's guest whose verify-cert fails with verifyErr when it is set: a read-back that
// cannot finish. seq is the posture reads and the verify-certs in order, each with the codes set by then.
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

// The edit names each staged profile (StagedProfiles, of the base) by its path before the window. The
// recheck's boot removes it (staged 0600 at prepare: TestProfileItemsStagedForApproval) and reads the items
// back (ReadBackItems) after the accounts and Remote Login, into the copy, whose tools.json is swapped in
// with it.
func TestEditBaseNamesProfilesThenReadsItemsBack(t *testing.T) {
	e, g := newItemsEdit(t)
	if _, err := macvm.EditBase(context.Background(), e.opts); err != nil {
		t.Fatal(err)
	}
	lines := e.lines()
	want := "To install a staged profile: signed in as wardyn-admin, run sudo open /Library/Wardyn/stage/wifi/item.mobileconfig in Terminal, then approve it in System Settings."
	if len(lines) != 5 || lines[2] != want || lines[3] != macvm.EditCheckingLine {
		t.Fatalf("lines:\n%s", e.out.String())
	}
	want = "/bin/rm -rf /Library/Wardyn/stage/wifi after 2 codes"
	if !slices.Equal(g.seq, []string{want, "posture after 2 codes", "verify-cert after 2 codes"}) {
		t.Fatalf("the recheck's boot ran %q, want the profile removed, then the read-back after the posture", g.seq)
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
