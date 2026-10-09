// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Run creates that carry components, racing each other and the writes that
// take a component away: a person's update of their saved row, the erasure of
// their components, and the revoke of an organisation component's grant. Over
// a real Postgres, under the race detector (`make test-race-pg` runs every
// TestPG_.*Concurrent name here). Guarded by WARDYN_TEST_PG; skipped cleanly
// when unset.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// snapshotLockKey is the advisory lock a test holds to stop every run create
// at its snapshot write, after the gate has read the component.
const snapshotLockKey = 81789

// compRace is one person's member session over a real Postgres server whose
// runner settles every dispatch and can replace a revived run's proxy.
type compRace struct {
	h       *harness
	pg      store.PG
	rn      *pgReviveRunner
	owner   string
	session *http.Cookie
}

func newCompRace(t *testing.T, owner string) compRace {
	t.Helper()
	h, _ := newRunOwnerPGHarness(t)
	rn := &pgReviveRunner{fakeRunner: &fakeRunner{}}
	h.srv.cfg.Runner = rn
	h.srv.cfg.RunConfigKey = make([]byte, 32)
	h.srv.cfg.ControlPlaneURL = "http://127.0.0.1:8080" // a hop the proxy config loader accepts
	e := compRace{h: h, pg: h.srv.cfg.Store.(store.PG), rn: rn, owner: owner, session: ssoSession(t, owner, ownerEmail, oidc.RoleUser)}
	if w := doSSO(t, h.srv, http.MethodPut, "/api/v1/secrets/person-secret", e.session, `{"value":"person-secret-value-0000"}`); w.Code != http.StatusNoContent {
		t.Fatalf("PUT /secrets = %d %s", w.Code, w.Body)
	}
	return e
}

// raceDef is a person's component reaching host with their header secret on it.
func raceDef(host string) string {
	return `{"hosts":["` + host + `"],"secrets":[{"secret_name":"person-secret","delivery":{"mode":"header","host":"` + host + `"}}]}`
}

// save stores a component of the person's own and returns its id and version.
func (e compRace) save(t *testing.T, name, host string) (uuid.UUID, int) {
	t.Helper()
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/me/components", e.session, saveComponentBody(name, raceDef(host)))
	if w.Code != http.StatusCreated {
		t.Fatalf("save %q = %d %s", name, w.Code, w.Body)
	}
	got := decodeSaved(t, w)
	return got.ID, got.Version
}

// create launches a run naming the component id, as the person.
func (e compRace) create(t *testing.T, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"agent":"claude-code","task":"t","interactive":true,` +
		`"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]},` +
		`"components":[{"id":"` + id.String() + `"}]}`
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs", e.session, body)
}

func runIDOf(t *testing.T, w *httptest.ResponseRecorder) uuid.UUID {
	t.Helper()
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == uuid.Nil {
		t.Fatalf("201 body %s: %v", w.Body, err)
	}
	return created.ID
}

// settle waits until dispatch has left PENDING and STARTING for every run, so
// no dispatch goroutine outlives the test's database.
func (e compRace) settle(t *testing.T, ids []uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for _, id := range ids {
		for {
			r, err := e.pg.GetRun(context.Background(), id)
			if err != nil {
				t.Fatalf("GetRun %s: %v", id, err)
			}
			if r.State != types.RunPending && r.State != types.RunStarting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("run %s never settled: %s", id, r.State)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (e compRace) snapshot(t *testing.T, id uuid.UUID) []types.RunComponent {
	t.Helper()
	rows, err := e.pg.ListRunComponents(context.Background(), id)
	if err != nil {
		t.Fatalf("ListRunComponents %s: %v", id, err)
	}
	return rows
}

// contentRows counts run_components rows that still carry owner's content.
func (e compRace) contentRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pg.Pool.QueryRow(context.Background(), `SELECT count(*) FROM run_components WHERE owner = $1`, e.owner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// runsOf counts the person's run rows.
func (e compRace) runsOf(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pg.Pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_runs WHERE created_by = $1`, e.owner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// holdSnapshots stops every run_components insert until the returned release
// runs: a statement trigger takes snapshotLockKey, which this connection holds.
func (e compRace) holdSnapshots(t *testing.T) (release func()) {
	t.Helper()
	ctx := context.Background()
	for _, ddl := range []string{
		`CREATE FUNCTION c17_hold_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(81789); RETURN NULL; END $$`,
		`CREATE TRIGGER c17_hold_snapshot BEFORE INSERT ON run_components FOR EACH STATEMENT EXECUTE FUNCTION c17_hold_snapshot()`,
	} {
		if _, err := e.pg.Pool.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := e.pg.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, snapshotLockKey); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, snapshotLockKey)
			conn.Release()
			_, _ = e.pg.Pool.Exec(ctx, `DROP TRIGGER c17_hold_snapshot ON run_components`)
		})
	}
	t.Cleanup(release)
	return release
}

// waitHeld returns once n inserts are waiting on snapshotLockKey.
func (e compRace) waitHeld(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var waiting int
		if err := e.pg.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND objid = $1 AND NOT granted`, snapshotLockKey).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d creates waiting at the snapshot write, want %d", waiting, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// replaced is how many proxies the runner has replaced.
func (e compRace) replaced() int {
	e.rn.mu.Lock()
	defer e.rn.mu.Unlock()
	return e.rn.replaced
}

// race runs every fn at once, released together, and waits for all of them.
func race(fns ...func()) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fn()
		}()
	}
	close(start)
	wg.Wait()
}

// TestPG_ConcurrentComponentCreate_OneSavedComponentGivesEveryRunOneConsistentSnapshot:
// six runs launch with the same saved component while its owner updates it.
// Every run gets exactly one snapshot row, and that row is one version of the
// component whole — its version, hosts and the run's own header grant agree —
// never a mix of the two, never another run's row, never none.
func TestPG_ConcurrentComponentCreate_OneSavedComponentGivesEveryRunOneConsistentSnapshot(t *testing.T) {
	e := newCompRace(t, "sub-c17-same-saved")
	id, v1 := e.save(t, "Shared Tool", "v1-api.example")
	const n = 6
	creates := make([]*httptest.ResponseRecorder, n)
	var update *httptest.ResponseRecorder
	fns := []func(){func() {
		update = doSSO(t, e.h.srv, http.MethodPut, "/api/v1/me/components/"+id.String(), e.session, saveComponentBody("Shared Tool", raceDef("v2-api.example")))
	}}
	for i := range creates {
		fns = append(fns, func() { creates[i] = e.create(t, id) })
	}
	race(fns...)
	if update.Code != http.StatusOK {
		t.Fatalf("update = %d %s", update.Code, update.Body)
	}
	v2 := decodeSaved(t, update).Version
	hostOf := map[int]string{v1: "v1-api.example", v2: "v2-api.example"}
	if v1 == v2 {
		t.Fatalf("the update kept version %d", v1)
	}

	var ids []uuid.UUID
	for i, w := range creates {
		if w.Code != http.StatusCreated {
			t.Fatalf("create %d = %d %s, want 201: every launch names a component its owner holds", i, w.Code, w.Body)
		}
		ids = append(ids, runIDOf(t, w))
	}
	e.settle(t, ids)
	seen := map[int]int{}
	for _, run := range ids {
		rows := e.snapshot(t, run)
		if len(rows) != 1 {
			t.Fatalf("run %s has %d snapshot rows, want 1: %+v", run, len(rows), rows)
		}
		r := rows[0]
		host, known := hostOf[r.Version]
		switch {
		case r.RunID != run || r.Ordinal != 0 || !r.SelfDefined || r.Erased || r.Owner != e.owner || r.ComponentID == nil || *r.ComponentID != id:
			t.Errorf("run %s snapshot = %+v, want the person's saved component at ordinal 0", run, r)
		case !known:
			t.Errorf("run %s snapshot version %d is neither %d nor %d", run, r.Version, v1, v2)
		case !slices.Equal(r.Definition.Hosts, []string{host}) || len(r.Definition.Secrets) != 1 || r.Definition.Secrets[0].Delivery.Host != host:
			t.Errorf("run %s snapshot is version %d with definition %+v: a version and another version's content", run, r.Version, r.Definition)
		}
		seen[r.Version]++
		grants, err := e.pg.ListGrantsByRun(context.Background(), run)
		if err != nil {
			t.Fatal(err)
		}
		var keyed []string
		for _, g := range grants {
			if g.Spec.Kind == types.GrantAPIKey {
				keyed = append(keyed, apiKeyGrantScopeHost(g.Spec.Scope))
			}
		}
		if !slices.Equal(keyed, []string{host}) {
			t.Errorf("run %s snapshot is version %d (%s) but its header grants are on %v", run, r.Version, host, keyed)
		}
	}
	t.Logf("snapshot versions across %d runs: %v", n, seen)
}

// TestPG_ComponentEraseDuringCreate_ConcurrentCreatesFailClosedOrAreErased: the
// person's components are erased while runs naming their saved component are
// being created. When both are done, no run_components row holds the erased
// content: a create that answered 201 has a content-free tombstone, and one
// that did not left no content behind. First with the erase forced into the
// window between the gate's read and the snapshot write, then six creates and
// the erase released together.
func TestPG_ComponentEraseDuringCreate_ConcurrentCreatesFailClosedOrAreErased(t *testing.T) {
	e := newCompRace(t, "sub-c17-erase")
	erase := func() *httptest.ResponseRecorder {
		return do(t, e.h.srv, http.MethodPost, "/api/v1/people/"+e.owner+"/erasure", adminToken, erasureBody("components"))
	}
	// check is the invariant once every create and the erase have answered.
	check := func(t *testing.T, phase string, creates []*httptest.ResponseRecorder, erased *httptest.ResponseRecorder) {
		t.Helper()
		if erased.Code != http.StatusOK {
			t.Fatalf("%s: erasure = %d %s", phase, erased.Code, erased.Body)
		}
		var ids []uuid.UUID
		for _, w := range creates {
			if w.Code == http.StatusCreated {
				ids = append(ids, runIDOf(t, w))
			}
		}
		e.settle(t, ids)
		for _, run := range ids {
			if rows := e.snapshot(t, run); len(rows) != 1 || !rows[0].Erased || !rows[0].SelfDefined {
				t.Errorf("%s: run %s answered 201 and its snapshot after the erasure returned is %+v, want one content-free tombstone (half state: the erased component lives on in the run's snapshot)", phase, run, rows)
			}
		}
		if n := e.contentRows(t); n != 0 {
			t.Errorf("%s: %d run_components rows still carry the erased person's content after the erasure returned", phase, n)
		}
	}

	t.Run("erase between the gate and the snapshot write", func(t *testing.T) {
		id, _ := e.save(t, "Erased Tool", "erased-api.example")
		release := e.holdSnapshots(t)
		created := make(chan *httptest.ResponseRecorder, 1)
		go func() { created <- e.create(t, id) }()
		e.waitHeld(t, 1)
		erased := make(chan *httptest.ResponseRecorder, 1)
		go func() { erased <- erase() }()
		var ew *httptest.ResponseRecorder
		select {
		case ew = <-erased:
			t.Log("the erasure returned while the create was past the gate: nothing fences it")
		case <-time.After(5 * time.Second):
			t.Log("the erasure waits on the create: it fences")
		}
		release()
		cw := <-created
		if ew == nil {
			ew = <-erased
		}
		check(t, "forced window", []*httptest.ResponseRecorder{cw}, ew)
	})

	t.Run("six creates and the erase released together", func(t *testing.T) {
		id, _ := e.save(t, "Raced Tool", "raced-api.example")
		creates := make([]*httptest.ResponseRecorder, 6)
		var ew *httptest.ResponseRecorder
		fns := []func(){func() { ew = erase() }}
		for i := range creates {
			fns = append(fns, func() { creates[i] = e.create(t, id) })
		}
		race(fns...)
		for i, w := range creates {
			if w.Code != http.StatusCreated && (w.Code != http.StatusForbidden || errorReasonOf(w.Body.String()) != "capability_component") {
				t.Errorf("create %d = %d %s, want 201, or the refusal an absent id gets", i, w.Code, w.Body)
			}
		}
		check(t, "released together", creates, ew)
	})
}

// TestPG_ComponentGrantRevokeDuringCreate_ConcurrentRunsKeepTheirSnapshotAndReviveIsRefused:
// an organisation component's grant is revoked while runs naming it are being
// created. A create the gate admitted keeps its whole snapshot — the evidence
// a revive re-checks — and a create refused leaves no run. After the revoke a
// new create is refused, and a run that launched before it cannot be revived.
func TestPG_ComponentGrantRevokeDuringCreate_ConcurrentRunsKeepTheirSnapshotAndReviveIsRefused(t *testing.T) {
	e := newCompRace(t, "sub-c17-revoke")
	ctx := context.Background()
	if err := e.h.srv.cfg.Secrets.For("").Put(ctx, "org-tool-token", []byte("operator-token-value")); err != nil {
		t.Fatal(err)
	}
	org, err := e.pg.CreateComponent(ctx, types.Component{ID: uuid.New(), Name: "Org Tool", CreatedBy: "admin",
		Definition: types.ComponentDefinition{Hosts: []string{"org-api.example"}, Secrets: []types.ComponentSecret{{SecretName: "org-tool-token", Shared: true,
			Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.pg.SetCapabilityRestriction(ctx, capComponent, org.ID.String(), true, "admin"); err != nil {
		t.Fatal(err)
	}
	allow := func(t *testing.T) uuid.UUID {
		t.Helper()
		g, err := e.pg.UpsertCapabilityGrant(ctx, grant(types.CapabilitySubjectUser, e.owner, capComponent, org.ID.String(), types.CapabilityAllow))
		if err != nil {
			t.Fatal(err)
		}
		return g.ID
	}
	revoke := func(id uuid.UUID) *httptest.ResponseRecorder {
		return do(t, e.h.srv, http.MethodDelete, "/api/v1/permissions/grants/"+id.String(), adminToken, "")
	}
	// admitted checks a 201's snapshot names the org component whole.
	admitted := func(t *testing.T, w *httptest.ResponseRecorder) uuid.UUID {
		t.Helper()
		run := runIDOf(t, w)
		rows := e.snapshot(t, run)
		if len(rows) != 1 || rows[0].SelfDefined || rows[0].Erased || rows[0].Owner != "" || rows[0].ComponentID == nil || *rows[0].ComponentID != org.ID || rows[0].Version != org.Version {
			t.Errorf("run %s answered 201 with snapshot %+v, want the organisation's component whole", run, rows)
		}
		return run
	}
	// reviveRefused loses each run and asks for its revive, as the owner.
	reviveRefused := func(t *testing.T, runs []uuid.UUID) {
		t.Helper()
		e.settle(t, runs)
		for _, id := range runs {
			deadline := time.Now().Add(10 * time.Second)
			for {
				ok, err := e.pg.MarkRunLost(ctx, id, types.LostOutage, time.Now(), 0)
				if err != nil {
					t.Fatal(err)
				}
				if ok {
					break
				}
				if time.Now().After(deadline) {
					r, _ := e.pg.GetRun(ctx, id)
					t.Fatalf("run %s never became markable as lost: %s", id, r.State)
				}
				time.Sleep(10 * time.Millisecond)
			}
			run, err := e.pg.GetRun(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			before := e.replaced()
			if code, reason := reviveReason(t, e.h, run, true); code != http.StatusForbidden || reason != reasonOwnerCapabilityComponent || e.replaced() != before {
				t.Errorf("revive of %s after the revoke = %d %s, want 403 %s and no new proxy", id, code, reason, reasonOwnerCapabilityComponent)
			}
		}
	}

	t.Run("revoke between the gate and the snapshot write", func(t *testing.T) {
		g := allow(t)
		release := e.holdSnapshots(t)
		created := make(chan *httptest.ResponseRecorder, 1)
		go func() { created <- e.create(t, org.ID) }()
		e.waitHeld(t, 1)
		if w := revoke(g); w.Code != http.StatusNoContent {
			t.Fatalf("revoke = %d %s", w.Code, w.Body)
		}
		release()
		w := <-created
		if w.Code != http.StatusCreated {
			t.Fatalf("the create the gate admitted before the revoke = %d %s, want 201", w.Code, w.Body)
		}
		run := admitted(t, w)
		if after := e.create(t, org.ID); after.Code != http.StatusForbidden || strings.TrimSpace(after.Body.String()) != componentRefusalBody {
			t.Errorf("a create after the revoke = %d %s, want the refusal bytes", after.Code, after.Body)
		}
		reviveRefused(t, []uuid.UUID{run})
	})

	t.Run("six creates and the revoke released together", func(t *testing.T) {
		g := allow(t)
		runsBefore := e.runsOf(t)
		creates := make([]*httptest.ResponseRecorder, 6)
		var rw *httptest.ResponseRecorder
		fns := []func(){func() { rw = revoke(g) }}
		for i := range creates {
			fns = append(fns, func() { creates[i] = e.create(t, org.ID) })
		}
		race(fns...)
		if rw.Code != http.StatusNoContent {
			t.Fatalf("revoke = %d %s", rw.Code, rw.Body)
		}
		var runs []uuid.UUID
		for i, w := range creates {
			switch w.Code {
			case http.StatusCreated:
				runs = append(runs, admitted(t, w))
			case http.StatusForbidden:
				if errorReasonOf(w.Body.String()) != "capability_component" {
					t.Errorf("create %d = %d %s, want the component refusal", i, w.Code, w.Body)
				}
			default:
				t.Errorf("create %d = %d %s, want 201 or 403", i, w.Code, w.Body)
			}
		}
		if got := e.runsOf(t) - runsBefore; got != len(runs) {
			t.Errorf("%d run rows were written for %d admitted creates: a refused create left a run", got, len(runs))
		}
		t.Logf("%d of 6 creates admitted before the revoke", len(runs))
		reviveRefused(t, runs)
	})
}
