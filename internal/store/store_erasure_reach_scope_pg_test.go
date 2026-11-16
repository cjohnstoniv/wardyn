// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ApprovalErasureStripsOperationKey: erasing a person's audit fields removes the operation key from
// the scope of every approval raised in their runs, pending or decided, in the statement that clears what
// they decided. The call's arguments stay, a decider who is someone else stays, and an approval in another
// person's run keeps its key even when the erased person decided it.
func TestPG_ApprovalErasureStripsOperationKey(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	owner, other := "erase-keys-"+uuid.NewString(), "keep-keys-"+uuid.NewString()
	raise := func(runOwner, key string) uuid.UUID {
		t.Helper()
		run := newRun(types.RunPending)
		run.CreatedBy = runOwner
		if _, err := st.CreateRun(ctx, run); err != nil {
			t.Fatalf("create run: %v", err)
		}
		scope := `{"tool":"t-` + uuid.NewString() + `","arguments":{"invoice":"7"},"operation_key":"` + key + `"}`
		ap, err := st.CreateApproval(ctx, types.ApprovalRequest{ID: uuid.New(), RunID: run.ID, Kind: types.ApprovalToolCall,
			State: types.ApprovalPending, RequestedAt: time.Now().UTC(), RequestedScope: json.RawMessage(scope)})
		if err != nil {
			t.Fatalf("create approval: %v", err)
		}
		return ap.ID
	}
	pending, decided := raise(owner, "payroll-7.refund"), raise(owner, "payroll-8.refund")
	herDecision, theirs := raise(other, "payroll-9.refund"), raise(other, "payroll-10.refund")
	for id, by := range map[uuid.UUID]string{decided: "approver-" + uuid.NewString(), herDecision: owner} {
		if _, err := pool.Exec(ctx, `UPDATE approvals SET state = 'APPROVED', decided_at = now(), decided_by = $2, reason = 'ok'
			WHERE id = $1`, id, by); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := st.EraseApprovalDecisionPersonalFields(ctx, []string{owner}); err != nil || n != 3 {
		t.Fatalf("erase = %d, %v; want 3 (her runs' two approvals and the one she decided)", n, err)
	}
	type row struct {
		keyed            bool
		invoice, by, why string
	}
	read := func(id uuid.UUID) row {
		t.Helper()
		var r row
		if err := pool.QueryRow(ctx, `SELECT requested_scope ? 'operation_key', requested_scope->'arguments'->>'invoice',
			decided_by, reason FROM approvals WHERE id = $1`, id).Scan(&r.keyed, &r.invoice, &r.by, &r.why); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := read(pending); r.keyed || r.invoice != "7" {
		t.Errorf("her pending approval = %+v; want the key gone and the arguments kept", r)
	}
	if r := read(decided); r.keyed || r.invoice != "7" || r.by == "" || r.why != "ok" {
		t.Errorf("her decided approval = %+v; want the key gone, the arguments and another person's decision kept", r)
	}
	if r := read(herDecision); !r.keyed || r.by != "" || r.why != "" {
		t.Errorf("the approval she decided in another person's run = %+v; want its key kept, her name and reason gone", r)
	}
	if r := read(theirs); !r.keyed || r.invoice != "7" {
		t.Errorf("another person's approval = %+v; want it untouched", r)
	}
	if n, err := st.EraseApprovalDecisionPersonalFields(ctx, []string{owner}); err != nil || n != 0 {
		t.Fatalf("a second erase = %d, %v; want nothing to change", n, err)
	}
}

// TestPG_ApprovalErasureNamesNoDownloadAsker: erasing a person's audit fields puts types.FileDownloadAskerErased in
// place of their name as who asked for a file download, in anyone's run; the scope still parses and keeps the
// file's name, size and hash, and another person's download is untouched.
func TestPG_ApprovalErasureNamesNoDownloadAsker(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	asker, other := "erase-asker-"+uuid.NewString(), "keep-asker-"+uuid.NewString()
	run := newRun(types.RunPending)
	run.CreatedBy = other
	if _, err := st.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	sum := strings.Repeat("ab", 32)
	ask := func(name, by string) uuid.UUID {
		t.Helper()
		ap, err := st.CreateApproval(ctx, types.ApprovalRequest{ID: uuid.New(), RunID: run.ID, Kind: types.ApprovalFileDownload,
			State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
			RequestedScope: json.RawMessage(`{"name":"` + name + `","bytes":12,"sha256":"` + sum + `","requested_by":"` + by + `"}`)})
		if err != nil {
			t.Fatalf("create approval: %v", err)
		}
		return ap.ID
	}
	hers, theirs := ask("build.log", asker), ask("test.log", other)
	if n, err := st.EraseApprovalDecisionPersonalFields(ctx, []string{asker}); err != nil || n != 1 {
		t.Fatalf("erase = %d, %v; want her one download", n, err)
	}
	scope := func(id uuid.UUID) types.FileDownloadScope {
		t.Helper()
		var raw json.RawMessage
		if err := pool.QueryRow(ctx, `SELECT requested_scope FROM approvals WHERE id = $1`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		sc, ok := types.ParseFileDownloadScope(raw)
		if !ok {
			t.Fatalf("scope %s does not parse", raw)
		}
		return sc
	}
	want := types.FileDownloadScope{Name: "build.log", Bytes: 12, SHA256: sum, RequestedBy: types.FileDownloadAskerErased}
	if got := scope(hers); got != want {
		t.Errorf("her download's scope = %+v; want %+v (the asker erased, the file kept)", got, want)
	}
	if got := scope(theirs); got.RequestedBy != other || got.Name != "test.log" {
		t.Errorf("another person's download = %+v; want it untouched", got)
	}
	if n, err := st.EraseApprovalDecisionPersonalFields(ctx, []string{asker}); err != nil || n != 0 {
		t.Fatalf("a second erase = %d, %v; want nothing to change", n, err)
	}
}
