// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for custom components (migration 0136_components) and
// run snapshots (migration 0137_run_components). Guarded by WARDYN_TEST_PG;
// skipped cleanly when unset. The components table is shared by every test on
// the substrate, so each test owns its rows through per-test owners and ids.
package store_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func testComponentDefinition() types.ComponentDefinition {
	return types.ComponentDefinition{
		Hosts: []string{"api.example.com", "*.example.org:443"},
		Secrets: []types.ComponentSecret{
			{SecretName: "stripe-key", Shared: true, Delivery: types.ComponentDelivery{
				Mode: types.ComponentDeliveryHeader, Host: "api.example.com", Header: "X-Api-Key", Format: "%s", PlainHTTP: true,
			}},
			{SecretName: "stripe-key", Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: "STRIPE_KEY"}},
		},
		Config: map[string]string{"STRIPE_REGION": "eu-west-1"},
	}
}

// newComponent creates one row and removes it when the test ends.
func newComponent(t *testing.T, st store.PG, owner, name string) types.Component {
	t.Helper()
	c, err := st.CreateComponent(context.Background(), types.Component{
		ID: uuid.New(), Owner: owner, Name: name, Definition: testComponentDefinition(), CreatedBy: "creator-of-" + name,
	})
	if err != nil {
		t.Fatalf("create component %q for %q: %v", name, owner, err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool.Exec(context.Background(), `DELETE FROM components WHERE id = $1`, c.ID)
	})
	return c
}

func componentIDs(cs []types.Component) []uuid.UUID {
	out := make([]uuid.UUID, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

// TestPG_Components_CreateUpdateVersion: a row is created at version 1 under
// the caller's id and never replaced by a second create; every update moves
// the version by one and reaches only its owner's row.
func TestPG_Components_CreateUpdateVersion(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	alice, bob := "alice-"+uuid.NewString(), "bob-"+uuid.NewString()

	if _, err := st.CreateComponent(ctx, types.Component{Owner: alice, Name: "no id"}); err == nil || errors.Is(err, store.ErrConflict) {
		t.Fatalf("create with no id: err = %v, want a refusal that is not a conflict (the caller chooses the id)", err)
	}

	c := newComponent(t, st, alice, "Stripe")
	if c.Version != 1 || c.Owner != alice || c.Name != "Stripe" || c.CreatedBy != "creator-of-Stripe" ||
		c.CreatedAt.IsZero() || !c.CreatedAt.Equal(c.UpdatedAt) {
		t.Fatalf("created row = %+v, want version 1, the given owner/name/creator and one database timestamp", c)
	}
	if !reflect.DeepEqual(c.Definition, testComponentDefinition()) {
		t.Fatalf("definition did not round-trip:\n got %+v\nwant %+v", c.Definition, testComponentDefinition())
	}
	got, err := st.GetComponent(ctx, c.ID, alice)
	if err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("get = %+v, %v; want the created row", got, err)
	}

	// Neither an existing id nor an existing (owner, name) is ever overwritten.
	for name, dup := range map[string]types.Component{
		"same id, another owner and name": {ID: c.ID, Owner: bob, Name: "Other"},
		"same owner and name, a new id":   {ID: uuid.New(), Owner: alice, Name: "Stripe"},
	} {
		if _, err := st.CreateComponent(ctx, dup); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("create %s: err = %v, want ErrConflict", name, err)
		}
	}
	if got, err := st.GetComponent(ctx, c.ID, alice); err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("after refused creates: get = %+v, %v; want the row untouched", got, err)
	}
	// A name is unique per owner, not globally.
	newComponent(t, st, bob, "Stripe")

	next := c
	next.Name = "Stripe (live)"
	next.Definition = types.ComponentDefinition{Config: map[string]string{"A": "b"}}
	next.CreatedBy = "ignored-on-update"
	up, err := st.UpdateComponent(ctx, next)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if up.Version != 2 || up.Name != "Stripe (live)" || up.CreatedBy != c.CreatedBy || !up.CreatedAt.Equal(c.CreatedAt) {
		t.Fatalf("updated row = %+v, want version 2, the new name, and the original creator and created_at", up)
	}
	// No hosts is stored and read back as an empty list, never null.
	if up.Definition.Hosts == nil || len(up.Definition.Hosts) != 0 || up.Definition.Secrets != nil ||
		!reflect.DeepEqual(up.Definition.Config, map[string]string{"A": "b"}) {
		t.Fatalf("updated definition = %#v, want empty hosts, no secrets and the new config", up.Definition)
	}
	if up, err = st.UpdateComponent(ctx, next); err != nil || up.Version != 3 {
		t.Fatalf("second update = version %d, %v; want 3 (every update moves the version)", up.Version, err)
	}

	// Another owner cannot reach the row, and a rename onto a taken name is a conflict; neither moves it.
	foreign := next
	foreign.Owner = bob
	if _, err := st.UpdateComponent(ctx, foreign); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update as another owner: err = %v, want ErrNotFound", err)
	}
	newComponent(t, st, alice, "Taken")
	clash := next
	clash.Name = "Taken"
	if _, err := st.UpdateComponent(ctx, clash); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("rename onto a taken name: err = %v, want ErrConflict", err)
	}
	if got, err := st.GetComponent(ctx, c.ID, alice); err != nil || got.Version != 3 || got.Name != "Stripe (live)" {
		t.Fatalf("after refused updates: %+v, %v; want version 3 and the name unchanged", got, err)
	}
	missing := next
	missing.ID = uuid.New()
	if _, err := st.UpdateComponent(ctx, missing); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("update of an absent id: err = %v, want ErrNotFound", err)
	}
}

// TestPG_Components_OwnerScoping: every read and delete answers for one
// owner, and another person's row is indistinguishable from an absent one.
func TestPG_Components_OwnerScoping(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	alice, bob := "alice-"+uuid.NewString(), "bob-"+uuid.NewString()
	aB := newComponent(t, st, alice, "b-second")
	aA := newComponent(t, st, alice, "a-first")
	bobs := newComponent(t, st, bob, "bobs")
	org := newComponent(t, st, "", "org-"+uuid.NewString())
	absent := uuid.New()

	for name, tc := range map[string]struct {
		id    uuid.UUID
		owner string
	}{
		"bob's row as alice":     {bobs.ID, alice},
		"alice's row as the org": {aA.ID, ""},
		"the org row as alice":   {org.ID, alice},
		"an absent id":           {absent, alice},
	} {
		if _, err := st.GetComponent(ctx, tc.id, tc.owner); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("get %s: err = %v, want ErrNotFound", name, err)
		}
		if _, err := st.DeleteComponent(ctx, tc.id, tc.owner); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("delete %s: err = %v, want ErrNotFound", name, err)
		}
	}

	list, err := st.ListComponents(ctx, alice)
	if err != nil || !slices.Equal(componentIDs(list), []uuid.UUID{aA.ID, aB.ID}) {
		t.Fatalf("alice's list = %v, %v; want her two rows by name", componentIDs(list), err)
	}
	if n, err := st.CountComponents(ctx, alice); err != nil || n != 2 {
		t.Fatalf("alice's count = %d, %v; want 2", n, err)
	}
	if list, err := st.ListComponents(ctx, "nobody-"+uuid.NewString()); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("a stranger's list = %#v, %v; want empty and not nil", list, err)
	}
	if n, err := st.CountComponents(ctx, ""); err != nil || n < 1 {
		t.Fatalf("org count = %d, %v; want at least this test's row", n, err)
	}

	// The gate's read: alice's own rows and the organisation's, never bob's.
	byID, err := st.ListComponentsByIDs(ctx, alice, []uuid.UUID{bobs.ID, aB.ID, org.ID, absent, aA.ID})
	if err != nil || !slices.Equal(componentIDs(byID), []uuid.UUID{org.ID, aA.ID, aB.ID}) {
		t.Fatalf("by ids as alice = %v, %v; want the org row then her own two", componentIDs(byID), err)
	}
	if byID, err := st.ListComponentsByIDs(ctx, "", []uuid.UUID{bobs.ID, aA.ID, org.ID}); err != nil || !slices.Equal(componentIDs(byID), []uuid.UUID{org.ID}) {
		t.Fatalf("by ids as the org = %v, %v; want only the org row", componentIDs(byID), err)
	}
	if byID, err := st.ListComponentsByIDs(ctx, alice, nil); err != nil || byID == nil || len(byID) != 0 {
		t.Fatalf("by no ids = %#v, %v; want empty and not nil", byID, err)
	}

	// The refused deletes above removed nothing; the owner's own delete returns the row.
	gone, err := st.DeleteComponent(ctx, bobs.ID, bob)
	if err != nil || !reflect.DeepEqual(gone, bobs) {
		t.Fatalf("delete as the owner = %+v, %v; want bob's row back", gone, err)
	}
	if _, err := st.GetComponent(ctx, bobs.ID, bob); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete: err = %v, want ErrNotFound", err)
	}
	for _, c := range []types.Component{aA, aB, org} {
		if _, err := st.GetComponent(ctx, c.ID, c.Owner); err != nil {
			t.Fatalf("%q did not survive another row's delete: %v", c.Name, err)
		}
	}
}

// TestPG_RunComponents_SnapshotAndCascade: a run's components are a copy that
// outlives the stored component and goes with the run.
func TestPG_RunComponents_SnapshotAndCascade(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	alice := "alice-" + uuid.NewString()
	saved := newComponent(t, st, alice, "saved")
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))

	if got, err := st.ListRunComponents(ctx, run.ID); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("a run launched without components lists %#v, %v; want empty and not nil", got, err)
	}

	orgID := uuid.New()
	rows := []types.RunComponent{
		// RunID and Ordinal are the store's to set, whatever the caller left in them.
		{RunID: uuid.New(), Ordinal: 7, ComponentID: &saved.ID, Owner: alice, Name: saved.Name, Version: saved.Version,
			Definition: saved.Definition, SelfDefined: true},
		{Owner: alice, Name: "", Definition: types.ComponentDefinition{}, SelfDefined: true},
		{ComponentID: &orgID, Name: "org component", Version: 4, Definition: testComponentDefinition()},
	}
	if err := st.PutRunComponents(ctx, run.ID, rows); err != nil {
		t.Fatalf("put: %v", err)
	}
	want := slices.Clone(rows)
	for i := range want {
		want[i].RunID, want[i].Ordinal = run.ID, i
	}
	want[1].Definition.Hosts = []string{}
	got, err := st.ListRunComponents(ctx, run.ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot:\n got %+v (%v)\nwant %+v", got, err, want)
	}

	// The snapshot is a copy: deleting the stored component leaves it whole.
	if _, err := st.DeleteComponent(ctx, saved.ID, alice); err != nil {
		t.Fatalf("delete the stored component: %v", err)
	}
	if got, err := st.ListRunComponents(ctx, run.ID); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot after the stored component was deleted = %+v, %v; want it unchanged", got, err)
	}

	// A snapshot is written once: it is the run's authorization record, never replaced.
	if err := st.PutRunComponents(ctx, run.ID, rows[2:]); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second put: err = %v, want ErrConflict", err)
	}
	if got, err := st.ListRunComponents(ctx, run.ID); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("after a refused second put = %+v, %v; want the first snapshot unchanged", got, err)
	}
	// The table refuses an org row marked self-defined, and a person's row that is not.
	other := persistRun(t, ctx, pool, newRun(types.RunRunning))
	for name, bad := range map[string]types.RunComponent{
		"an org row marked self-defined":  {Name: "x", SelfDefined: true},
		"a person's row not self-defined": {Owner: alice, Name: "x"},
	} {
		if err := st.PutRunComponents(ctx, other.ID, []types.RunComponent{bad}); err == nil {
			t.Fatalf("put %s: err = nil, want the CHECK to refuse it", name)
		}
	}
	if got, err := st.ListRunComponents(ctx, other.ID); err != nil || len(got) != 0 {
		t.Fatalf("refused puts left %+v, %v; want nothing written", got, err)
	}

	if err := st.PutRunComponents(ctx, uuid.New(), rows); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("put for a run that does not exist: err = %v, want ErrNotFound", err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM agent_runs WHERE id = $1`, run.ID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_components WHERE run_id = $1`, run.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left after the run was deleted = %d, %v; want 0 (ON DELETE CASCADE)", left, err)
	}
}

// TestPG_Components_EraseByOwner: a person's erasure deletes the rows they
// saved and clears the content of the snapshot rows of what they defined,
// keeping each as the run's authorization tombstone — and touches nothing of
// another person's, the organisation's, or the run row itself.
func TestPG_Components_EraseByOwner(t *testing.T) {
	pool := runsPGPool(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	alice, bob := "alice-"+uuid.NewString(), "bob-"+uuid.NewString()
	a1 := newComponent(t, st, alice, "one")
	newComponent(t, st, alice, "two")
	bobs := newComponent(t, st, bob, "bobs")
	org := newComponent(t, st, "", "org-"+uuid.NewString())
	aliceRun := persistRun(t, ctx, pool, newRun(types.RunRunning))
	bobRun := persistRun(t, ctx, pool, newRun(types.RunRunning))
	if err := st.PutRunComponents(ctx, aliceRun.ID, []types.RunComponent{
		{ComponentID: &a1.ID, Owner: alice, Name: a1.Name, Version: 1, Definition: a1.Definition, SelfDefined: true},
		{ComponentID: &org.ID, Name: org.Name, Version: 1, Definition: org.Definition},
		{Owner: alice, Name: "inline", Definition: testComponentDefinition(), SelfDefined: true},
	}); err != nil {
		t.Fatalf("put alice's snapshot: %v", err)
	}
	if err := st.PutRunComponents(ctx, bobRun.ID, []types.RunComponent{
		{ComponentID: &bobs.ID, Owner: bob, Name: bobs.Name, Version: 1, Definition: bobs.Definition, SelfDefined: true},
	}); err != nil {
		t.Fatalf("put bob's snapshot: %v", err)
	}
	bobBefore, err := st.ListRunComponents(ctx, bobRun.ID)
	if err != nil {
		t.Fatalf("list bob's snapshot: %v", err)
	}

	// The organisation's rows are no person's: owner "" is refused, and takes nothing.
	if n, err := st.DeleteComponentsByOwner(ctx, ""); err == nil || n != 0 {
		t.Fatalf("erase components of owner \"\" = %d, %v; want a refusal", n, err)
	}
	if n, err := st.EraseRunComponentsByOwner(ctx, ""); err == nil || n != 0 {
		t.Fatalf("erase run components of owner \"\" = %d, %v; want a refusal", n, err)
	}

	if n, err := st.DeleteComponentsByOwner(ctx, alice); err != nil || n != 2 {
		t.Fatalf("erase alice's components = %d, %v; want 2", n, err)
	}
	if n, err := st.EraseRunComponentsByOwner(ctx, alice); err != nil || n != 2 {
		t.Fatalf("erase alice's run components = %d, %v; want 2", n, err)
	}
	for name, erase := range map[string]func(context.Context, string) (int, error){
		"components": st.DeleteComponentsByOwner, "run components": st.EraseRunComponentsByOwner,
	} {
		if n, err := erase(ctx, alice); err != nil || n != 0 {
			t.Fatalf("a second erase of %s = %d, %v; want 0 and no error", name, n, err)
		}
	}

	if list, err := st.ListComponents(ctx, alice); err != nil || len(list) != 0 {
		t.Fatalf("alice's saved components after erasure = %v, %v; want none", componentIDs(list), err)
	}
	// Content gone, tombstones kept: the run still says it carried three components, two
	// self-defined, and the org row still names the grant it needs.
	got, err := st.ListRunComponents(ctx, aliceRun.ID)
	if err != nil {
		t.Fatalf("list alice's run: %v", err)
	}
	tomb := func(ordinal int) types.RunComponent {
		return types.RunComponent{RunID: aliceRun.ID, Ordinal: ordinal, SelfDefined: true, Erased: true}
	}
	want := []types.RunComponent{
		tomb(0),
		{RunID: aliceRun.ID, Ordinal: 1, ComponentID: &org.ID, Name: org.Name, Version: 1, Definition: org.Definition},
		tomb(2),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("alice's run after erasure:\n got %+v\nwant %+v", got, want)
	}
	// Nothing descriptive survives in the cleared rows, read straight from the table.
	var leftover int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_components WHERE run_id = $1 AND self_defined
		AND (owner IS NOT NULL OR name IS NOT NULL OR version IS NOT NULL OR definition IS NOT NULL OR component_id IS NOT NULL)`,
		aliceRun.ID).Scan(&leftover); err != nil || leftover != 0 {
		t.Fatalf("cleared rows still holding content = %d, %v; want 0", leftover, err)
	}
	if _, err := st.GetRun(ctx, aliceRun.ID); err != nil {
		t.Fatalf("the run row did not survive its owner's component erasure: %v", err)
	}
	if _, err := st.GetComponent(ctx, bobs.ID, bob); err != nil {
		t.Fatalf("bob's component: %v", err)
	}
	if _, err := st.GetComponent(ctx, org.ID, ""); err != nil {
		t.Fatalf("the org component: %v", err)
	}
	if got, err := st.ListRunComponents(ctx, bobRun.ID); err != nil || !reflect.DeepEqual(got, bobBefore) {
		t.Fatalf("bob's snapshot = %+v, %v; want it untouched", got, err)
	}
}

// TestPG_Components_ConcurrentCreateAndUpdate: racing creates of one
// (owner, name) admit exactly one row, and racing updates each move the
// version — no two land on the same one.
func TestPG_Components_ConcurrentCreateAndUpdate(t *testing.T) {
	st := store.NewPG(runsPGPool(t))
	ctx := context.Background()
	owner := "alice-" + uuid.NewString()
	t.Cleanup(func() {
		_, _ = st.Pool.Exec(context.Background(), `DELETE FROM components WHERE owner = $1`, owner)
	})

	const n = 8
	var wg sync.WaitGroup
	created := make([]types.Component, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			created[i], errs[i] = st.CreateComponent(ctx, types.Component{ID: uuid.New(), Owner: owner, Name: "raced"})
		}()
	}
	wg.Wait()
	var winner types.Component
	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
			winner = created[i]
		case !errors.Is(err, store.ErrConflict):
			t.Fatalf("racing create %d: err = %v, want nil or ErrConflict", i, err)
		}
	}
	if count, err := st.CountComponents(ctx, owner); won != 1 || err != nil || count != 1 {
		t.Fatalf("%d creates won and %d rows exist (%v); want exactly one of each", won, count, err)
	}

	versions := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var up types.Component
			up, errs[i] = st.UpdateComponent(ctx, winner)
			versions[i] = up.Version
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racing update %d: %v", i, err)
		}
	}
	slices.Sort(versions)
	if want := []int{2, 3, 4, 5, 6, 7, 8, 9}; !slices.Equal(versions, want) {
		t.Fatalf("racing updates returned versions %v, want %v — each one its own", versions, want)
	}
}
