// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// requireSeeded fails unless the fresh test schema starts as a fresh install does: hard_limits with
// nothing listed and floor CC1 (internal/db/fresh_seed.go).
func requireSeeded(t *testing.T, e *govEnv) {
	t.Helper()
	b, err := e.pg.GetBoundary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b.Mode != types.BoundaryHardLimits || b.Spec.MinConfinementClass == nil || *b.Spec.MinConfinementClass != types.CC1 ||
		b.Spec.AllowedDomains != nil || b.Spec.DeniedDomains != nil || b.Spec.EligibleGrants != nil || b.Spec.AllowAllEgress != nil {
		t.Fatalf("fresh schema boundary = %+v, want the seed: hard_limits, floor CC1, nothing listed", b)
	}
}

// TestSeededReachEqualsNoLimits: a fresh install's seeded boundary reaches exactly what no_limits
// does. The corpus resolves to the golden recorded before the boundary existed; the seal changes no
// resolved policy and decides no knob; the dispatch and revive credential phases have nothing to
// withhold. A hard_limits boundary with no floor at all resolves the same.
func TestSeededReachEqualsNoLimits(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	requireSeeded(t, e)
	c := seedBoundaryCorpus(t, e)
	resolved := resolveBoundaryCorpus(t, e.srv, c)
	compareOrUpdateGolden(t, "testdata/boundary_no_limits_golden.json", resolved)

	layers := e.srv.outerLayers(context.Background(), types.AgentRun{ID: uuid.New()})
	if len(layers) != 1 {
		t.Fatalf("outer layers = %+v, want the boundary's one", layers)
	}
	if (withholding{layers: layers}).binds() {
		t.Errorf("the seeded boundary's layer %+v binds the credential phases", layers[0])
	}
	for _, r := range resolved {
		sealed := r.Spec.Clone()
		if d := sealPolicy(&sealed, layers); len(d) != 0 {
			t.Errorf("%s: the seal decided %+v under the seeded boundary", r.Case, d)
		}
		before, _ := json.Marshal(canonicalKnobs(r.Spec))
		after, _ := json.Marshal(canonicalKnobs(sealed))
		if string(before) != string(after) {
			t.Errorf("%s: sealed %s, want %s", r.Case, after, before)
		}
	}

	putBoundary(t, e, types.BoundaryWrite{Mode: types.BoundaryHardLimits})
	compareOrUpdateGolden(t, "testdata/boundary_no_limits_golden.json", resolveBoundaryCorpus(t, e.srv, c))
}

// TestBreakGlassFreshInstall: on a fresh install, seeded and with no profile yet, an admin-token
// run needs no break-glass reason and is not tagged.
func TestBreakGlassFreshInstall(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	requireSeeded(t, e)
	w := e.admin(http.MethodPost, "/api/v1/runs", `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","allow_all_egress":true}}`)
	var created createRunResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("admin-token run, no reason: %d %s, want 201", w.Code, w.Body)
	}
	run, err := e.pg.GetRun(context.Background(), created.ID)
	if err != nil || run.BreakGlass || len(e.audits("run.break_glass.open")) != 0 {
		t.Errorf("run break_glass = %v, open rows %d (%v), want an untagged run", run.BreakGlass, len(e.audits("run.break_glass.open")), err)
	}
}

// TestFreshSeedMarkedByEveryWrite: a boundary write marks the row seeded, so a seed that runs after
// it (a replica still booting) leaves the person's document alone (internal/db's
// TestFreshSeedLosesToAWriteThatLandsFirst).
func TestFreshSeedMarkedByEveryWrite(t *testing.T) {
	pool := throwawayPGPool(t)
	e := newGovEnvOn(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE governance_boundary SET mode = 'no_limits', spec = '{}', seeded_at = NULL WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	putBoundary(t, e, types.BoundaryWrite{Mode: types.BoundaryNoLimits})
	var seeded bool
	if err := pool.QueryRow(t.Context(), `SELECT seeded_at IS NOT NULL FROM governance_boundary WHERE singleton`).Scan(&seeded); err != nil || !seeded {
		t.Errorf("seeded_at set after a write = %v (%v), want true", seeded, err)
	}
}
