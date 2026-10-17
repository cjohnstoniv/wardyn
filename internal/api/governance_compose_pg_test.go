// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// composedWriteEnv is the real handlers over a real Postgres store, which is where the graph lock, the
// CHECK constraints and "absent keeps" are the database's own answers.
type composedWriteEnv struct {
	t     *testing.T
	srv   *Server
	pg    store.PG
	audit *recRecorder
	ctx   context.Context
}

func newComposedWriteEnv(t *testing.T) *composedWriteEnv {
	t.Helper()
	pool := throwawayPGPool(t)
	pg := store.NewPG(pool)
	h := newHarness(t)
	return &composedWriteEnv{t: t, srv: New(baseTestConfig(h, pg)), pg: pg, audit: h.audit, ctx: context.Background()}
}

type profileAnswer struct {
	code int
	body string
	resp governanceProfileResponse
	errb errorBody
}

func (e *composedWriteEnv) send(method, path, body string) profileAnswer {
	e.t.Helper()
	w := do(e.t, e.srv, method, path, adminToken, body)
	a := profileAnswer{code: w.Code, body: w.Body.String()}
	_ = json.Unmarshal(w.Body.Bytes(), &a.resp)
	_ = json.Unmarshal(w.Body.Bytes(), &a.errb)
	return a
}

func (e *composedWriteEnv) post(body string) profileAnswer {
	return e.send(http.MethodPost, "/api/v1/governance/profiles", body)
}

func (e *composedWriteEnv) put(id uuid.UUID, body string) profileAnswer {
	return e.send(http.MethodPut, "/api/v1/governance/profiles/"+id.String(), body)
}

func (e *composedWriteEnv) del(id uuid.UUID) profileAnswer {
	return e.send(http.MethodDelete, "/api/v1/governance/profiles/"+id.String(), "")
}

func (e *composedWriteEnv) mustCreate(body string) types.GovernanceProfile {
	e.t.Helper()
	a := e.post(body)
	if a.code != http.StatusCreated {
		e.t.Fatalf("POST %s = %d %s, want 201", body, a.code, a.body)
	}
	return a.resp.Profile.GovernanceProfile
}

func (e *composedWriteEnv) stored(id uuid.UUID) types.GovernanceProfile {
	e.t.Helper()
	p, err := e.pg.GetGovernanceProfile(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

const baseBody = `"name":"base","ceiling":{"min_confinement_class":"CC2","allowed_domains":["a.example","b.example"],"allowed_methods":["GET","POST"]}`

func TestPG_ComposedProfileWrites(t *testing.T) {
	e := newComposedWriteEnv(t)
	base := e.mustCreate(`{` + baseBody + `}`)

	t.Run("a composed profile stores its policy as an overlay and answers with effective", func(t *testing.T) {
		a := e.post(`{"name":"child","base_profile_id":"` + base.ID.String() + `","overlay":{"allowed_domains":["a.example"]},"overlay_limits":{"deny_interactive":true}}`)
		if a.code != http.StatusCreated {
			t.Fatalf("POST composed = %d %s", a.code, a.body)
		}
		p := a.resp.Profile
		if p.BaseProfileID == nil || *p.BaseProfileID != base.ID || p.Overlay == nil || p.OverlayLimits == nil {
			t.Errorf("response = %+v, want base_profile_id, overlay and overlay_limits", p.GovernanceProfile)
		}
		if !slices.Equal(p.Effective.Ceiling.AllowedDomains, []string{"a.example"}) || !p.Effective.Limits.DenyInteractive {
			t.Errorf("effective = %+v, want the overlay narrowing the base and the limit set", p.Effective)
		}
		if p.Effective.Ceiling.MinConfinementClass != types.CC2 || !slices.Equal(p.Effective.Ceiling.AllowedMethods, []string{"GET", "POST"}) {
			t.Errorf("effective = %+v, want the base's confinement and methods inherited", p.Effective.Ceiling)
		}
		row := e.stored(p.ID)
		if !reflect.DeepEqual(row.Ceiling, types.RunPolicySpec{}) || !reflect.DeepEqual(row.Limits, types.GovernanceLimits{}) {
			t.Errorf("stored ceiling/limits = %+v %+v, want {} for a composed row", row.Ceiling, row.Limits)
		}
		// The audit row carries the EFFECTIVE values and the overlay's field NAMES.
		var last types.AuditEvent
		for _, ev := range e.audit.snapshot() {
			if ev.Action == "governance.profile.write" && ev.Target == p.ID.String() {
				last = ev
			}
		}
		var d map[string]any
		if err := json.Unmarshal(last.Data, &d); err != nil {
			t.Fatalf("audit data: %v", err)
		}
		if d["base_profile_id"] != base.ID.String() || d["min_confinement_class"] != "CC2" {
			t.Errorf("audit data = %v, want base_profile_id and the effective confinement", d)
		}
		if f, _ := json.Marshal(d["overlay_fields"]); string(f) != `["allowed_domains","deny_interactive"]` {
			t.Errorf("overlay_fields = %s, want the names only", f)
		}
		if strings.Contains(string(last.Data), "a.example") {
			t.Errorf("audit row carries an overlay VALUE: %s", last.Data)
		}
	})

	t.Run("an overlay outside the base is refused", func(t *testing.T) {
		for name, overlay := range map[string]string{
			"a domain the base does not allow": `{"allowed_domains":["c.example"]}`,
			"a method the base excludes":       `{"allowed_methods":["DELETE"]}`,
			"allow-all on a base without it":   `{"allow_all_egress":true}`,
			"an empty methods list":            `{"allowed_methods":[]}`,
			"an unknown field":                 `{"allowed_domainz":["a.example"]}`,
		} {
			a := e.post(`{"name":"x-` + strings.ReplaceAll(name, " ", "-") + `","base_profile_id":"` + base.ID.String() + `","overlay":` + overlay + `}`)
			if a.code != http.StatusBadRequest || a.errb.Reason != reasonGovernanceOverlayInvalid {
				t.Errorf("%s: %d %s, want 400 %s", name, a.code, a.body, reasonGovernanceOverlayInvalid)
			}
		}
		if a := e.post(`{"name":"nobase","base_profile_id":"` + uuid.NewString() + `","overlay":{}}`); a.code != http.StatusBadRequest {
			t.Errorf("a base that does not exist = %d %s, want 400", a.code, a.body)
		}
	})

	t.Run("a composed profile cannot also carry a raw ceiling or limits", func(t *testing.T) {
		for _, extra := range []string{`"ceiling":{"min_confinement_class":"CC3"}`, `"limits":{"deny_interactive":true}`} {
			a := e.post(`{"name":"both","overlay":{},` + extra + `}`)
			if a.code != http.StatusBadRequest || a.errb.Reason != reasonGovernanceOverlayInvalid {
				t.Errorf("%s: %d %s, want 400 %s, not a CHECK violation", extra, a.code, a.body, reasonGovernanceOverlayInvalid)
			}
		}
	})

	t.Run("a PUT that omits the composition fields keeps them, and null clears them", func(t *testing.T) {
		child := e.mustCreate(`{"name":"keeper","base_profile_id":"` + base.ID.String() + `","overlay":{"allowed_domains":["b.example"]},"overlay_limits":{"max_concurrent_runs":2}}`)
		// What an older client sends back: name, ceiling {}, limits {}.
		if a := e.put(child.ID, `{"name":"keeper-renamed","ceiling":{},"limits":{}}`); a.code != http.StatusOK {
			t.Fatalf("PUT = %d %s", a.code, a.body)
		}
		got := e.stored(child.ID)
		if got.Name != "keeper-renamed" || got.BaseProfileID == nil || *got.BaseProfileID != base.ID || got.Overlay == nil ||
			got.Overlay.AllowedDomains == nil || got.OverlayLimits == nil || got.OverlayLimits.MaxConcurrentRuns == nil {
			t.Fatalf("after an omitting PUT: %+v, want the composition kept", got)
		}
		// null overlay_limits clears only the limits.
		if a := e.put(child.ID, `{"name":"keeper-renamed","overlay_limits":null}`); a.code != http.StatusOK {
			t.Fatalf("PUT overlay_limits null = %d %s", a.code, a.body)
		}
		if got := e.stored(child.ID); got.OverlayLimits != nil || got.Overlay == nil {
			t.Errorf("after overlay_limits null: %+v, want limits cleared and the overlay kept", got)
		}
		// null base_profile_id re-bases on the deployment default.
		if a := e.put(child.ID, `{"name":"keeper-renamed","base_profile_id":null,"overlay":{}}`); a.code != http.StatusOK {
			t.Fatalf("PUT base null = %d %s", a.code, a.body)
		}
		if got := e.stored(child.ID); got.BaseProfileID != nil || got.Overlay == nil {
			t.Errorf("after base null: %+v, want the deployment as the base", got)
		}
		// null overlay flattens it to a standalone profile carrying its own ceiling.
		if a := e.put(child.ID, `{"name":"keeper-renamed","overlay":null,"ceiling":{"min_confinement_class":"CC2"}}`); a.code != http.StatusOK {
			t.Fatalf("PUT overlay null = %d %s", a.code, a.body)
		}
		if got := e.stored(child.ID); got.Overlay != nil || got.BaseProfileID != nil || got.Ceiling.MinConfinementClass != types.CC2 {
			t.Errorf("after overlay null: %+v, want a standalone profile with the ceiling sent", got)
		}
		// A standalone profile becomes composed again with an overlay, its ceiling and limits cleared.
		if a := e.put(child.ID, `{"name":"keeper-renamed","base_profile_id":"`+base.ID.String()+`","overlay":{"allowed_domains":["a.example"]}}`); a.code != http.StatusOK {
			t.Fatalf("PUT overlay on a standalone profile = %d %s", a.code, a.body)
		}
		if got := e.stored(child.ID); got.Overlay == nil || got.BaseProfileID == nil || !reflect.DeepEqual(got.Ceiling, types.RunPolicySpec{}) {
			t.Errorf("after composing again: %+v, want an overlay on the base and an empty stored ceiling", got)
		}
		// A base without an overlay is not a profile.
		lone := e.mustCreate(`{"name":"lone","ceiling":{"min_confinement_class":"CC2"}}`)
		if a := e.put(lone.ID, `{"name":"lone","ceiling":{"min_confinement_class":"CC2"},"base_profile_id":"`+base.ID.String()+`"}`); a.code != http.StatusBadRequest {
			t.Errorf("a base on a standalone profile = %d %s, want 400", a.code, a.body)
		}
	})

	t.Run("a base edit that empties a descendant's meet is refused, naming it", func(t *testing.T) {
		b := e.mustCreate(`{"name":"meet-base","ceiling":{"min_confinement_class":"CC2","allowed_methods":["GET","POST"]}}`)
		c := e.mustCreate(`{"name":"meet-child","base_profile_id":"` + b.ID.String() + `","overlay":{"allowed_methods":["POST"]}}`)
		a := e.put(b.ID, `{"name":"meet-base","ceiling":{"min_confinement_class":"CC2","allowed_methods":["GET"]}}`)
		if a.code != http.StatusConflict || a.errb.Reason != reasonGovernanceOverlayUnsatisfiable || !strings.Contains(a.body, "meet-child") {
			t.Fatalf("base PUT = %d %s, want 409 governance_overlay_unsatisfiable naming meet-child", a.code, a.body)
		}
		if got := e.stored(b.ID); !slices.Equal(got.Ceiling.AllowedMethods, []string{"GET", "POST"}) {
			t.Errorf("the refused write changed the base: %v", got.Ceiling.AllowedMethods)
		}
		_ = c
	})

	t.Run("deleting a base with children is a 409 naming them", func(t *testing.T) {
		b := e.mustCreate(`{"name":"del-base","ceiling":{"min_confinement_class":"CC2"}}`)
		e.mustCreate(`{"name":"del-child-b","base_profile_id":"` + b.ID.String() + `","overlay":{}}`)
		e.mustCreate(`{"name":"del-child-a","base_profile_id":"` + b.ID.String() + `","overlay":{}}`)
		a := e.del(b.ID)
		if a.code != http.StatusConflict || a.errb.Reason != reasonGovernanceProfileInUse ||
			!strings.Contains(a.body, "del-child-a, del-child-b") {
			t.Fatalf("DELETE = %d %s, want 409 naming both children in order", a.code, a.body)
		}
		if _, err := e.pg.GetGovernanceProfile(e.ctx, b.ID); err != nil {
			t.Errorf("the base was deleted: %v", err)
		}
	})

	t.Run("a cycle and a depth overflow are refused", func(t *testing.T) {
		a := e.mustCreate(`{"name":"cyc-a","overlay":{}}`)
		b := e.mustCreate(`{"name":"cyc-b","base_profile_id":"` + a.ID.String() + `","overlay":{}}`)
		if r := e.put(a.ID, `{"name":"cyc-a","base_profile_id":"`+b.ID.String()+`","overlay":{}}`); r.code != http.StatusConflict || r.errb.Reason != reasonGovernanceProfileCycle {
			t.Errorf("a base that loops back = %d %s, want 409 %s", r.code, r.body, reasonGovernanceProfileCycle)
		}
		if r := e.put(a.ID, `{"name":"cyc-a","base_profile_id":"`+a.ID.String()+`","overlay":{}}`); r.code != http.StatusConflict || r.errb.Reason != reasonGovernanceProfileCycle {
			t.Errorf("a profile as its own base = %d %s, want 409 %s", r.code, r.body, reasonGovernanceProfileCycle)
		}
		// cyc-a <- cyc-b <- cyc-c is three deep; a fourth refuses.
		c := e.mustCreate(`{"name":"cyc-c","base_profile_id":"` + b.ID.String() + `","overlay":{}}`)
		if r := e.post(`{"name":"cyc-d","base_profile_id":"` + c.ID.String() + `","overlay":{}}`); r.code != http.StatusConflict || r.errb.Reason != reasonGovernanceProfileDepth {
			t.Errorf("a fourth level = %d %s, want 409 %s", r.code, r.body, reasonGovernanceProfileDepth)
		}
		// Re-basing the ROOT under another profile pushes its descendants past three.
		top := e.mustCreate(`{"name":"cyc-top","ceiling":{"min_confinement_class":"CC2"}}`)
		if r := e.put(a.ID, `{"name":"cyc-a","base_profile_id":"`+top.ID.String()+`","overlay":{}}`); r.code != http.StatusConflict || r.errb.Reason != reasonGovernanceProfileDepth {
			t.Errorf("re-basing a root under cyc-top = %d %s, want 409 %s for its descendants", r.code, r.body, reasonGovernanceProfileDepth)
		}
	})

	t.Run("the CHECK constraints refuse what the handler would not write", func(t *testing.T) {
		for name, sql := range map[string]string{
			"a base without an overlay":      `UPDATE governance_profiles SET base_profile_id = $1 WHERE id = $2`,
			"a self base":                    `UPDATE governance_profiles SET base_profile_id = id, overlay = '{}' WHERE id = $2 AND $1::uuid IS NOT NULL`,
			"limits without an overlay":      `UPDATE governance_profiles SET overlay_limits = '{}' WHERE id = $2 AND $1::uuid IS NOT NULL`,
			"an overlay that is not objects": `UPDATE governance_profiles SET overlay = '[]' WHERE id = $2 AND $1::uuid IS NOT NULL`,
			"a composed row with a ceiling":  `UPDATE governance_profiles SET overlay = '{}', ceiling = '{"max_holds":1}' WHERE id = $2 AND $1::uuid IS NOT NULL`,
		} {
			if _, err := e.pg.Pool.Exec(e.ctx, sql, base.ID, base.ID); err == nil {
				t.Errorf("%s was accepted by the database", name)
			}
		}
	})
}

// TestPG_ConcurrentGraphWritesNeverMakeACycleOrADepthFourChain races the writes whose combination
// would break the graph; the graph lock lets at most the safe ones through.
func TestPG_ConcurrentGraphWritesNeverMakeACycleOrADepthFourChain(t *testing.T) {
	e := newComposedWriteEnv(t)
	for round := 0; round < 8; round++ {
		suffix := string(rune('a' + round))
		a := e.mustCreate(`{"name":"r-a-` + suffix + `","overlay":{}}`)
		b := e.mustCreate(`{"name":"r-b-` + suffix + `","overlay":{}}`)
		c := e.mustCreate(`{"name":"r-c-` + suffix + `","overlay":{}}`)
		d := e.mustCreate(`{"name":"r-d-` + suffix + `","overlay":{}}`)
		type write struct {
			id   uuid.UUID
			name string
			base uuid.UUID
		}
		// Loop: a under b while b under a. Depth: b under a, c under b, d under c is four deep.
		writes := []write{
			{a.ID, a.Name, b.ID}, {b.ID, b.Name, a.ID},
			{c.ID, c.Name, b.ID}, {d.ID, d.Name, c.ID},
		}
		var wg sync.WaitGroup
		for _, w := range writes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				e.put(w.id, `{"name":"`+w.name+`","base_profile_id":"`+w.base.String()+`","overlay":{}}`)
			}()
		}
		wg.Wait()
		all, err := e.pg.ListGovernanceProfiles(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		byID := profilesByID(all)
		for _, id := range []uuid.UUID{a.ID, b.ID, c.ID, d.ID} {
			if _, err := chainFromRows(byID, id); err != nil {
				t.Fatalf("round %d: after concurrent writes %s has no clean chain: %v", round, byID[id].Name, err)
			}
		}
	}
}

// TestPG_ChainReadIsOneStatementBoundedAtThree: the store hands back at most three rows however deep a
// seeded graph runs, and the resolver refuses what the bound cut short.
func TestPG_ChainReadIsOneStatementBoundedAtThree(t *testing.T) {
	e := newComposedWriteEnv(t)
	root := e.mustCreate(`{"name":"chain-root","ceiling":{"min_confinement_class":"CC2"}}`)
	prev := root
	var chain []types.GovernanceProfile
	for _, name := range []string{"chain-1", "chain-2", "chain-3"} {
		// Seeded below the handler's own depth check, as a direct write or a bug would leave it.
		id := uuid.New()
		if _, err := e.pg.Pool.Exec(e.ctx, `INSERT INTO governance_profiles (id, name, ceiling, limits, base_profile_id, overlay) VALUES ($1,$2,'{}','{}',$3,'{}')`,
			id, name, prev.ID); err != nil {
			t.Fatal(err)
		}
		prev = types.GovernanceProfile{ID: id, Name: name}
		chain = append(chain, prev)
	}
	got, err := e.pg.GetGovernanceProfileChain(e.ctx, chain[2].ID)
	if err != nil || len(got) != 3 {
		t.Fatalf("chain read = %d rows err %v, want 3 (bounded)", len(got), err)
	}
	if _, err := e.srv.composeChain(got); err == nil {
		t.Error("the resolver composed a chain the bound cut short")
	}
	if _, err := e.srv.resolveProfileByID(e.ctx, chain[2].ID); err == nil {
		t.Error("resolveProfileByID composed a depth-four chain")
	}
	if ok, err := e.srv.resolveProfileByID(e.ctx, chain[1].ID); err != nil || !ok.Composed {
		t.Errorf("a three-deep chain: %v %v, want composed", ok, err)
	}
	if _, err := e.pg.GetGovernanceProfileChain(e.ctx, uuid.New()); err == nil {
		t.Error("an unknown profile read as a chain")
	}
}

// TestPG_AuditRecordsTheEffectiveAllowAll: a child of an allow-all base writes allow_all_egress: true,
// the value its own (empty) ceiling would hide.
func TestPG_AuditRecordsTheEffectiveAllowAll(t *testing.T) {
	e := newComposedWriteEnv(t)
	base := e.mustCreate(`{"name":"open-base","ceiling":{"allow_all_egress":true,"min_confinement_class":"CC2"}}`)
	child := e.mustCreate(`{"name":"open-child","base_profile_id":"` + base.ID.String() + `","overlay":{}}`)
	var found bool
	for _, ev := range e.audit.snapshot() {
		if ev.Action != "governance.profile.write" || ev.Target != child.ID.String() {
			continue
		}
		found = true
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d["allow_all_egress"] != true {
			t.Errorf("audit allow_all_egress = %v, want true: the base's value is the child's", d["allow_all_egress"])
		}
	}
	if !found {
		t.Fatal("no governance.profile.write row for the child")
	}
}

// TestPG_GovernanceListShowsTheChainToAdminsOnly: GET /governance carries effective, with the base-level
// warnings and the reason a broken chain has no content.
func TestPG_GovernanceListShowsEffective(t *testing.T) {
	e := newComposedWriteEnv(t)
	base := e.mustCreate(`{"name":"list-base","ceiling":{"min_confinement_class":"CC2","allowed_domains":["a.example"]}}`)
	child := e.mustCreate(`{"name":"list-child","base_profile_id":"` + base.ID.String() + `","overlay":{"allowed_domains":["a.example"]}}`)
	// A seeded loop, written below the handler.
	loopA, loopB := uuid.New(), uuid.New()
	for _, q := range []string{
		`INSERT INTO governance_profiles (id, name, ceiling, limits, overlay) VALUES ('` + loopA.String() + `','loop-a','{}','{}','{}')`,
		`INSERT INTO governance_profiles (id, name, ceiling, limits, overlay) VALUES ('` + loopB.String() + `','loop-b','{}','{}','{}')`,
		`UPDATE governance_profiles SET base_profile_id = '` + loopB.String() + `' WHERE id = '` + loopA.String() + `'`,
		`UPDATE governance_profiles SET base_profile_id = '` + loopA.String() + `' WHERE id = '` + loopB.String() + `'`,
	} {
		if _, err := e.pg.Pool.Exec(e.ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	a := e.send(http.MethodGet, "/api/v1/governance", "")
	if a.code != http.StatusOK {
		t.Fatalf("GET /governance = %d %s", a.code, a.body)
	}
	var snap governanceResponse
	if err := json.Unmarshal([]byte(a.body), &snap); err != nil {
		t.Fatal(err)
	}
	byName := map[string]governanceProfileView{}
	for _, p := range snap.Profiles {
		byName[p.Name] = p
	}
	if got := byName[child.Name].Effective.Ceiling.AllowedDomains; !slices.Equal(got, []string{"a.example"}) || byName[child.Name].Effective.Error != "" {
		t.Errorf("child effective = %+v, want the composed content", byName[child.Name].Effective)
	}
	if byName[child.Name].Effective.Ceiling.MinConfinementClass != types.CC2 {
		t.Errorf("child effective confinement = %q, want the base's", byName[child.Name].Effective.Ceiling.MinConfinementClass)
	}
	for _, n := range []string{"loop-a", "loop-b"} {
		if e := byName[n].Effective; e.Error == "" || e.Ceiling.MinConfinementClass != "" {
			t.Errorf("%s effective = %+v, want an error and no content", n, e)
		}
	}
	if byName[base.Name].Effective.Ceiling.MinConfinementClass != types.CC2 {
		t.Errorf("a standalone profile has no effective content: %+v", byName[base.Name].Effective)
	}
}
