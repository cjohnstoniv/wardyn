// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Governance changes (migration 0126) against Postgres: the proposal transaction and the decision
// transaction. Guarded by WARDYN_TEST_PG; skipped cleanly when unset. Every case mints a unique
// target_key and asserts only on rows it created, so these are safe on the shared substrate.
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func newChange(key string) types.GovernanceChange {
	return types.GovernanceChange{
		TargetKind: "governance_profile", Op: "update", TargetKey: key,
		Payload: json.RawMessage(`{"name":"p"}`), Diff: json.RawMessage(`{"changed":["name"]}`),
		BaseHash: `"base"`, DeploymentHash: `"dep"`,
		ProposedBy: "alice-" + key, ProposedByEmail: "Alice@Corp.Example",
	}
}

func TestPG_GovernanceChange_ProposeRefusesALiveOneAndExpiresALapsedOne(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	key := uuid.NewString()

	first, expired, err := st.ProposeGovernanceChange(ctx, newChange(key), time.Hour)
	if err != nil || len(expired) != 0 || first.State != types.GovernanceChangePending || !first.ExpiresAt.After(first.ProposedAt) {
		t.Fatalf("first proposal = %+v, %v, %v; want a pending change that expires after it was proposed", first, expired, err)
	}
	var pending *store.ErrGovernanceChangePending
	if _, _, err := st.ProposeGovernanceChange(ctx, newChange(key), time.Hour); !errors.As(err, &pending) || pending.ID != first.ID {
		t.Fatalf("second proposal at a live target = %v, want *ErrGovernanceChangePending naming %s", err, first.ID)
	}
	// A different target is independent.
	if _, _, err := st.ProposeGovernanceChange(ctx, newChange(uuid.NewString()), time.Hour); err != nil {
		t.Fatalf("a proposal at another target: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE governance_changes SET expires_at = now() - interval '1 second' WHERE id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	read, err := st.GetGovernanceChange(ctx, first.ID)
	if err != nil || read.State != types.GovernanceChangeExpired {
		t.Fatalf("a lapsed pending change reads %q (%v), want expired without any write", read.State, err)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT state FROM governance_changes WHERE id = $1`, first.ID).Scan(&stored); err != nil || stored != types.GovernanceChangePending {
		t.Fatalf("the read wrote the row: state %q (%v)", stored, err)
	}
	second, expired, err := st.ProposeGovernanceChange(ctx, newChange(key), time.Hour)
	if err != nil || len(expired) != 1 || expired[0].ID != first.ID || expired[0].TargetKey != key {
		t.Fatalf("re-proposal = %+v, expired %v, %v; want the lapsed change expired in the same transaction", second, expired, err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM governance_changes WHERE id = $1`, first.ID).Scan(&stored); err != nil || stored != types.GovernanceChangeExpired {
		t.Fatalf("the lapsed change is %q (%v), want expired", stored, err)
	}
}

func TestPG_GovernanceChange_DecideTransaction(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	approve := store.GovernanceDecision{To: types.GovernanceChangeApplied, By: "bob", ByEmail: "bob@corp.example"}
	scratch := func(q store.Querier, name string) error {
		_, err := q.Exec(ctx, `INSERT INTO governance_profiles (id, name, ceiling, limits, created_by) VALUES ($1,$2,'{}','{}','t')`, uuid.New(), name)
		return err
	}
	exists := func(name string) bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM governance_profiles WHERE name = $1`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	propose := func() types.GovernanceChange {
		ch, _, err := st.ProposeGovernanceChange(ctx, newChange(uuid.NewString()), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}

	// Committed: fn's write and the state move land together, stamped with the decider.
	ch := propose()
	name := "decided-" + uuid.NewString()
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM governance_profiles WHERE name = $1`, name) })
	out, err := st.DecideGovernanceChange(ctx, ch.ID, approve, func(q store.Querier, got types.GovernanceChange) error {
		if got.ID != ch.ID || got.State != types.GovernanceChangePending || got.ProposedByEmail != "Alice@Corp.Example" {
			t.Errorf("fn saw %+v, want the locked pending change with its server-side fields", got)
		}
		return scratch(q, name)
	})
	if err != nil || out.State != types.GovernanceChangeApplied || out.DecidedBy != "bob" || out.DecidedAt == nil || !exists(name) {
		t.Fatalf("committed decision = %+v, %v (target written: %v)", out, err, exists(name))
	}
	if _, err := st.DecideGovernanceChange(ctx, ch.ID, approve, nil); err == nil {
		t.Fatal("an applied change was decided again")
	} else {
		var np *store.ErrGovernanceChangeNotPending
		if !errors.As(err, &np) || np.State != types.GovernanceChangeApplied {
			t.Fatalf("second decision = %v, want *ErrGovernanceChangeNotPending (applied)", err)
		}
	}

	// Rolled back: an error from fn unwrites what fn wrote and leaves the change pending.
	ch = propose()
	name = "rolled-back-" + uuid.NewString()
	boom := errors.New("boom")
	if _, err := st.DecideGovernanceChange(ctx, ch.ID, approve, func(q store.Querier, _ types.GovernanceChange) error {
		if err := scratch(q, name); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("decision = %v, want fn's own error", err)
	}
	read, _ := st.GetGovernanceChange(ctx, ch.ID)
	if exists(name) || read.State != types.GovernanceChangePending {
		t.Fatalf("after a failed apply: target written %v, change %q; want neither", exists(name), read.State)
	}

	// Stale: the state is committed, and the target is never written.
	if _, err := st.DecideGovernanceChange(ctx, ch.ID, approve, func(store.Querier, types.GovernanceChange) error {
		return store.ErrGovernanceChangeStale
	}); !errors.Is(err, store.ErrGovernanceChangeStale) {
		t.Fatalf("stale decision = %v", err)
	}
	if read, _ = st.GetGovernanceChange(ctx, ch.ID); read.State != types.GovernanceChangeStale {
		t.Fatalf("a stale decision left the change %q, want stale", read.State)
	}

	// Lapsed: moved to expired and committed, fn never runs.
	ch = propose()
	if _, err := pool.Exec(ctx, `UPDATE governance_changes SET expires_at = now() - interval '1 second' WHERE id = $1`, ch.ID); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = st.DecideGovernanceChange(ctx, ch.ID, approve, func(store.Querier, types.GovernanceChange) error { called = true; return nil })
	var np *store.ErrGovernanceChangeNotPending
	if !errors.As(err, &np) || !np.Lapsed || called {
		t.Fatalf("lapsed decision = %v (fn called %v), want *ErrGovernanceChangeNotPending{Lapsed}", err, called)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT state FROM governance_changes WHERE id = $1`, ch.ID).Scan(&stored); err != nil || stored != types.GovernanceChangeExpired {
		t.Fatalf("the lapsed change is %q (%v), want expired", stored, err)
	}

	// Rejected, with its reason; and an unknown id.
	ch = propose()
	rej, err := st.DecideGovernanceChange(ctx, ch.ID, store.GovernanceDecision{To: types.GovernanceChangeRejected, By: "bob", Reason: "no"}, nil)
	if err != nil || rej.State != types.GovernanceChangeRejected || rej.Reason != "no" {
		t.Fatalf("reject = %+v, %v", rej, err)
	}
	if _, err := st.DecideGovernanceChange(ctx, uuid.New(), approve, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown change = %v, want ErrNotFound", err)
	}
}

func TestPG_GovernanceChange_DryRunAlwaysRollsBack(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	name := "dry-" + uuid.NewString()
	if err := st.DryRunGovernance(ctx, func(q store.Querier) error {
		_, err := q.Exec(ctx, `INSERT INTO governance_profiles (id, name, ceiling, limits, created_by) VALUES ($1,$2,'{}','{}','t')`, uuid.New(), name)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM governance_profiles WHERE name = $1`, name).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a dry run left %d rows (%v)", n, err)
	}
}

func TestPG_GovernanceChange_EraseClearsPrincipalAndEmailMatches(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	who := "erase-" + uuid.NewString()
	mine := newChange(uuid.NewString())
	mine.ProposedBy, mine.ProposedByEmail = who, who+"@Corp.Example"
	byEmail := newChange(uuid.NewString())
	byEmail.ProposedBy, byEmail.ProposedByEmail = "another-principal", who+"@corp.example"
	theirs := newChange(uuid.NewString())
	theirs.ProposedBy, theirs.ProposedByEmail = "someone-else", "else@corp.example"
	// Another person whose email claim spells the erased person's principal.
	spelled := newChange(uuid.NewString())
	spelled.ProposedBy, spelled.ProposedByEmail = "claimant", strings.ToUpper(who)
	var ids []uuid.UUID
	for _, c := range []types.GovernanceChange{mine, byEmail, theirs, spelled} {
		saved, _, err := st.ProposeGovernanceChange(ctx, c, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, saved.ID)
	}
	n, err := st.EraseGovernanceChangePersonalFields(ctx, []string{who}, []string{who + "@corp.example"})
	if err != nil || n != 2 {
		t.Fatalf("erase touched %d rows (%v), want the principal's and the email's", n, err)
	}
	read := func(id uuid.UUID) (state, by, email string) {
		if err := pool.QueryRow(ctx, `SELECT state, proposed_by, proposed_by_email FROM governance_changes WHERE id = $1`, id).Scan(&state, &by, &email); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, id := range ids[:2] {
		if state, by, email := read(id); state == types.GovernanceChangePending || by != "" || email != "" {
			t.Errorf("an erased person's change = %q %q %q, want not pending and no personal fields", state, by, email)
		}
	}
	for i, want := range map[int]string{2: "someone-else", 3: "claimant"} {
		if state, by, _ := read(ids[i]); state != types.GovernanceChangePending || by != want {
			t.Errorf("another person's change = %q %q, want it untouched", state, by)
		}
	}
	if n, err := st.EraseGovernanceChangePersonalFields(ctx, []string{who}, nil); err != nil || n != 0 {
		t.Errorf("a retry touched %d rows (%v), want 0", n, err)
	}
	if n, err := st.EraseGovernanceChangePersonalFields(ctx, nil, nil); err != nil || n != 0 {
		t.Errorf("an empty name list touched %d rows (%v)", n, err)
	}
}

func TestPG_GovernanceChange_EraseClearsTheDecidersReason(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	who := "erase-" + uuid.NewString()
	reject := func(decider, email string) uuid.UUID {
		saved, _, err := st.ProposeGovernanceChange(ctx, newChange(uuid.NewString()), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE governance_changes SET state = 'rejected', decided_at = now(),
			decided_by = $2, decided_by_email = $3, reason = 'typed by the decider' WHERE id = $1`, saved.ID, decider, email); err != nil {
			t.Fatal(err)
		}
		return saved.ID
	}
	mine, theirs := reject(who, who+"@corp.example"), reject("someone-else", "else@corp.example")
	if n, err := st.EraseGovernanceChangePersonalFields(ctx, []string{who}, nil); err != nil || n != 1 {
		t.Fatalf("erase touched %d rows (%v), want the one the person decided", n, err)
	}
	reason := func(id uuid.UUID) (r string) {
		if err := pool.QueryRow(ctx, `SELECT reason FROM governance_changes WHERE id = $1`, id).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return
	}
	if r := reason(mine); r != "" {
		t.Errorf("the erased decider's reason = %q, want it cleared", r)
	}
	if r := reason(theirs); r != "typed by the decider" {
		t.Errorf("another decider's reason = %q, want it untouched", r)
	}
}

func nameChange(op, key, name string) types.GovernanceChange {
	ch := newChange(key)
	ch.Op = op
	ch.Payload = json.RawMessage(`{"name":"` + name + `"}`)
	return ch
}

// A rename holds the name it takes exactly as a create does, in either order; a same-name update, an
// update of a profile that is gone and a lapsed holder hold nothing.
func TestProposeGovernanceChange_ARenameHoldsItsNewName(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	newProfile := func(t *testing.T, name string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO governance_profiles (id, name, ceiling, limits, created_by) VALUES ($1,$2,'{}','{}','t')`, id, name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM governance_profiles WHERE id = $1`, id) }) //nolint:errcheck // best-effort
		return id
	}
	refused := func(t *testing.T, err error, want uuid.UUID) {
		t.Helper()
		var pending *store.ErrGovernanceChangePending
		if !errors.As(err, &pending) || pending.ID != want {
			t.Fatalf("proposal = %v, want *ErrGovernanceChangePending naming %s", err, want)
		}
	}
	suffix := uuid.NewString()

	t.Run("a pending create refuses a rename to its name", func(t *testing.T) {
		name := "x-" + suffix
		p := newProfile(t, "orig-a-"+suffix)
		create, _, err := st.ProposeGovernanceChange(ctx, nameChange("create", uuid.NewString(), name), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = st.ProposeGovernanceChange(ctx, nameChange("update", p.String(), name), time.Hour)
		refused(t, err, create.ID)
	})
	t.Run("a pending rename refuses a create of its name", func(t *testing.T) {
		name := "y-" + suffix
		p := newProfile(t, "orig-b-"+suffix)
		rename, _, err := st.ProposeGovernanceChange(ctx, nameChange("update", p.String(), name), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = st.ProposeGovernanceChange(ctx, nameChange("create", uuid.NewString(), name), time.Hour)
		refused(t, err, rename.ID)
	})
	t.Run("a pending rename refuses another rename to its name", func(t *testing.T) {
		name := "z-" + suffix
		p, q := newProfile(t, "orig-c-"+suffix), newProfile(t, "orig-d-"+suffix)
		first, _, err := st.ProposeGovernanceChange(ctx, nameChange("update", p.String(), name), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = st.ProposeGovernanceChange(ctx, nameChange("update", q.String(), name), time.Hour)
		refused(t, err, first.ID)
	})
	t.Run("a same-name update holds no name", func(t *testing.T) {
		name := "keep-" + suffix
		p := newProfile(t, name)
		if _, _, err := st.ProposeGovernanceChange(ctx, nameChange("update", p.String(), name), time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.ProposeGovernanceChange(ctx, nameChange("create", uuid.NewString(), name), time.Hour); err != nil {
			t.Fatalf("a create of a name a same-name update only keeps: %v", err)
		}
	})
	t.Run("a same-name update is not refused by a pending claim on its name", func(t *testing.T) {
		name := "kept-" + suffix
		p := newProfile(t, name)
		if _, _, err := st.ProposeGovernanceChange(ctx, nameChange("create", uuid.NewString(), name), time.Hour); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.ProposeGovernanceChange(ctx, nameChange("update", p.String(), name), time.Hour); err != nil {
			t.Fatalf("an update that keeps its name was refused by a pending create of that name: %v", err)
		}
	})
	t.Run("a lapsed rename is expired and no longer holds its name", func(t *testing.T) {
		name := "lapsed-" + suffix
		p := newProfile(t, "orig-e-"+suffix)
		rename, _, err := st.ProposeGovernanceChange(ctx, nameChange("update", p.String(), name), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE governance_changes SET expires_at = now() - interval '1 second' WHERE id = $1`, rename.ID); err != nil {
			t.Fatal(err)
		}
		_, expired, err := st.ProposeGovernanceChange(ctx, nameChange("create", uuid.NewString(), name), time.Hour)
		if err != nil || len(expired) != 1 || expired[0].ID != rename.ID {
			t.Fatalf("create over a lapsed rename = expired %v, %v; want the rename expired", expired, err)
		}
	})
}
