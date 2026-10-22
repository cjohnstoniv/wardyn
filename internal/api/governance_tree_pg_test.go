// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The policy tree's PG pins (the TREE-MODEL lane): one context per run, chosen from a person's
// memberships, its path composed from the top.

// treeCtx is a signed-in member with groups and a user type.
func treeCtx(sub string, groups []string, userType string) context.Context {
	if groups == nil {
		groups = []string{} // a snapshot that names no group, not a missing one
	}
	ctx := withOIDCGroups(memberCtxFor(sub), groups)
	if userType != "" {
		ctx = withOIDCUserType(ctx, userType)
	}
	return ctx
}

// treeMember adds one membership (legacy false, as every write after the upgrade is).
func treeMember(t *testing.T, e *govEnv, unit types.GovernanceUnit, st types.CapabilitySubjectType, subject string, priority int, home bool) types.GovernanceMembership {
	t.Helper()
	m, err := e.pg.UpsertGovernanceMembership(context.Background(), types.GovernanceMembership{
		SubjectType: st, Subject: subject, UnitID: unit.ID, Priority: priority, Home: home, CreatedBy: "seed"})
	if err != nil {
		t.Fatalf("seed membership %s %q -> %s: %v", st, subject, unit.Name, err)
	}
	return m
}

// treeChild writes a composed unit through the API, as an admin does.
func treeChild(t *testing.T, e *govEnv, body string) types.GovernanceUnit {
	t.Helper()
	w := e.admin(http.MethodPost, "/api/v1/governance/units", body)
	var resp governanceUnitResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
		t.Fatalf("POST %s = %d %s, want 201", body, w.Code, w.Body)
	}
	return resp.Unit.GovernanceUnit
}

func treeCeiling(t *testing.T, e *govEnv, ctx context.Context) governanceCeiling {
	t.Helper()
	c, err := e.srv.effectiveCeiling(ctx)
	if err != nil {
		t.Fatalf("effective ceiling: %v", err)
	}
	return c
}

func unitIDOf(c governanceCeiling) uuid.UUID {
	if c.Unit == nil {
		return uuid.Nil
	}
	return c.Unit.ID
}

// upgradeCorpusDowngrade puts the schema back in its shape before the unit-tree migration
// (governance_unit_tree), so a corpus written the old way meets the migration itself. Every step is
// guarded: on a tree without that migration it changes nothing, so the same test records the golden
// there.
const upgradeCorpusDowngrade = `
ALTER TABLE governance_memberships DROP CONSTRAINT IF EXISTS governance_memberships_unit_subject_key;
ALTER TABLE governance_memberships DROP COLUMN IF EXISTS home, DROP COLUMN IF EXISTS legacy;
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'governance_memberships_subject_type_subject_key') THEN
    ALTER TABLE governance_memberships ADD CONSTRAINT governance_memberships_subject_type_subject_key UNIQUE (subject_type, subject);
  END IF;
END $$;
ALTER TABLE governance_units
  DROP CONSTRAINT IF EXISTS governance_units_kind_check, DROP CONSTRAINT IF EXISTS governance_units_mode_check,
  DROP CONSTRAINT IF EXISTS governance_units_depth_check, DROP CONSTRAINT IF EXISTS governance_units_default_shape,
  DROP CONSTRAINT IF EXISTS governance_units_unit_shape, DROP CONSTRAINT IF EXISTS governance_units_pass_through_shape;
DROP INDEX IF EXISTS governance_units_one_default;
DELETE FROM governance_units WHERE id = '00000000-0000-0000-0000-000000000001';
ALTER TABLE governance_units DROP COLUMN IF EXISTS kind, DROP COLUMN IF EXISTS mode, DROP COLUMN IF EXISTS depth;
`

// upgradeCorpusSeed is a pre-tree corpus in the old shape: a standalone unit, one composed on no
// base, a chain of three, and one membership per subject at every tier with ties.
const upgradeCorpusSeed = `
INSERT INTO governance_units (id, name, ceiling, limits, parent_id, overlay, created_by) VALUES
 ('10000000-0000-0000-0000-000000000001','s-standalone','{"allowed_domains":["s.example","pypi.org"],"min_confinement_class":"CC2"}','{"max_concurrent_runs":4}',NULL,NULL,'seed'),
 ('10000000-0000-0000-0000-000000000002','c-no-base','{}','{}',NULL,'{"denied_domains":["evil.example"]}','seed'),
 ('10000000-0000-0000-0000-000000000003','r-root','{"allowed_domains":["r.example","pypi.org","npmjs.org"],"min_confinement_class":"CC2"}','{}',NULL,NULL,'seed');
INSERT INTO governance_units (id, name, ceiling, limits, parent_id, overlay, created_by) VALUES
 ('10000000-0000-0000-0000-000000000004','m-mid','{}','{}','10000000-0000-0000-0000-000000000003','{"allowed_domains":["pypi.org","npmjs.org"]}','seed');
INSERT INTO governance_units (id, name, ceiling, limits, parent_id, overlay, created_by) VALUES
 ('10000000-0000-0000-0000-000000000005','l-leaf','{}','{}','10000000-0000-0000-0000-000000000004','{"allowed_domains":["pypi.org"]}','seed');
INSERT INTO governance_memberships (id, subject_type, subject, unit_id, priority, created_by) VALUES
 (gen_random_uuid(),'user','sub-up1','10000000-0000-0000-0000-000000000005',0,'seed'),
 (gen_random_uuid(),'group','eng','10000000-0000-0000-0000-000000000004',5,'seed'),
 (gen_random_uuid(),'group','ops','10000000-0000-0000-0000-000000000002',5,'seed'),
 (gen_random_uuid(),'user_type','up-contractor','10000000-0000-0000-0000-000000000002',0,'seed'),
 (gen_random_uuid(),'all','','10000000-0000-0000-0000-000000000001',0,'seed'),
 (gen_random_uuid(),'user','sub-up5','10000000-0000-0000-0000-000000000001',0,'seed'),
 (gen_random_uuid(),'user','sub-up5@corp.example','10000000-0000-0000-0000-000000000005',9,'seed'),
 (gen_random_uuid(),'group','late','10000000-0000-0000-0000-000000000003',1,'seed');
`

// readMigration is the SQL of the one migration file named NNNN_<slug>.sql, found by its slug so a
// renumber leaves this test alone; os.ErrNotExist when the tree has no such migration.
func readMigration(slug string) (string, error) {
	names, err := filepath.Glob(filepath.Join("..", "db", "migrations", "*_"+slug+".sql"))
	if err != nil {
		return "", err
	}
	switch len(names) {
	case 0:
		return "", os.ErrNotExist
	case 1:
		b, err := os.ReadFile(names[0])
		return string(b), err
	default:
		return "", fmt.Errorf("%d migrations named %s: %v", len(names), slug, names)
	}
}

type upgradeCorpusResolved struct {
	Person   string                 `json:"person"`
	Unit     string                 `json:"unit"`
	Spec     types.RunPolicySpec    `json:"ceiling_spec"`
	Limits   types.GovernanceLimits `json:"ceiling_limits"`
	Warnings []string               `json:"ceiling_warnings"`
	Sealed   types.RunPolicySpec    `json:"sealed_spec"`
	OK       bool                   `json:"ok"`
}

type upgradeCorpusPerson struct {
	name     string
	sub      string
	groups   []string
	userType string
}

var upgradeCorpusPeople = []upgradeCorpusPerson{
	{"user_row_on_a_chain_leaf", "sub-up1", []string{"eng"}, ""},
	{"group_tie_by_unit_name", "sub-up2", []string{"eng", "ops"}, ""},
	{"user_type_over_all", "sub-up3", nil, "up-contractor"},
	{"all_only", "sub-up4", nil, ""},
	{"sub_over_email", "sub-up5", []string{"late"}, ""},
	{"group_below_user", "sub-up6", []string{"late", "ops"}, ""},
}

func upgradeCorpusResolve(t *testing.T, e *govEnv) []upgradeCorpusResolved {
	t.Helper()
	var out []upgradeCorpusResolved
	for _, p := range upgradeCorpusPeople {
		ctx := treeCtx(p.sub, p.groups, p.userType)
		c := treeCeiling(t, e, ctx)
		req := createRunRequest{Agent: "claude-code", Repo: "acme/widgets"}
		spec, _, _, _, ok := e.srv.resolveRunPolicy(ctx, httptest.NewRecorder(), governReq(ctx), &req, true)
		r := upgradeCorpusResolved{Person: p.name, Spec: c.Spec, Limits: c.Limits, Warnings: c.Warnings, Sealed: spec, OK: ok}
		if c.Unit != nil {
			r.Unit = c.Unit.Name
		}
		out = append(out, r)
	}
	return out
}

// TestUpgradeCorpusOneContextPerPerson: a pre-tree corpus upgrades with exactly today's winner as each
// person's one context, and what each resolves and seals equals the golden recorded at the head
// before the tree. Only the first legacy membership by precedence is a context: asking to run in a
// unit a person reached only through a dormant legacy membership is refused.
func TestUpgradeCorpusOneContextPerPerson(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	if _, err := e.pg.CreateUserType(ctx, types.UserType{ID: "up-contractor", Name: "up-contractor", CreatedBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, upgradeCorpusDowngrade); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if _, err := e.pool.Exec(ctx, upgradeCorpusSeed); err != nil {
		t.Fatalf("seed the pre-tree corpus: %v", err)
	}
	switch sql, err := readMigration("governance_unit_tree"); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		t.Fatal(err)
	default:
		if _, err := e.pool.Exec(ctx, sql); err != nil {
			t.Fatalf("the unit-tree migration over the corpus: %v", err)
		}
	}
	compareOrUpdateGolden(t, "testdata/governance_upgrade_corpus_golden.json", upgradeCorpusResolve(t, e))
	// TREE-ONLY: sub-up5 holds a legacy user row on s-standalone (the winner) and one on l-leaf.
	loser := uuid.MustParse("10000000-0000-0000-0000-000000000005")
	if _, err := e.srv.effectiveCeiling(withContextChoice(treeCtx("sub-up5", []string{"late"}, ""), contextChoice{RunIn: loser})); !errors.Is(err, errUnitNotAContext) {
		t.Errorf("RunIn naming a dormant legacy membership's unit: %v, want errUnitNotAContext", err)
	}
	var legacy int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM governance_memberships WHERE legacy`).Scan(&legacy); err != nil || legacy != 8 {
		t.Errorf("legacy memberships after the upgrade = %d (%v), want all 8", legacy, err)
	}
	// TREE-ONLY END
}

// TestUnitContextSiblingsNeverBlend: a member of two sibling units runs in exactly one of them, as
// asked: never their meet, never their union.
func TestUnitContextSiblingsNeverBlend(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	a := e.seedProfile("sib-a", "a.example", "pypi.org")
	b := e.seedProfile("sib-b", "b.example", "npmjs.org")
	treeMember(t, e, a, types.CapabilitySubjectUser, "sub-sib", 1, false)
	treeMember(t, e, b, types.CapabilitySubjectUser, "sub-sib", 0, false)
	for _, want := range []types.GovernanceUnit{a, b} {
		c := treeCeiling(t, e, withContextChoice(treeCtx("sub-sib", nil, ""), contextChoice{RunIn: want.ID}))
		if unitIDOf(c) != want.ID || !slices.Equal(c.Spec.AllowedDomains, want.Ceiling.AllowedDomains) {
			t.Errorf("RunIn %s: unit %s domains %v, want %s's own %v", want.Name, unitIDOf(c), c.Spec.AllowedDomains, want.Name, want.Ceiling.AllowedDomains)
		}
	}
}

// TestUnitContextHomeDefault: with no unit asked for, the home membership's unit is the context;
// with no home, the precedence winner.
func TestUnitContextHomeDefault(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	a := e.seedProfile("home-a", "a.example")
	b := e.seedProfile("home-b", "b.example")
	treeMember(t, e, a, types.CapabilitySubjectUser, "sub-home", 9, false)
	treeMember(t, e, b, types.CapabilitySubjectUser, "sub-home", 1, true)
	if got := unitIDOf(treeCeiling(t, e, treeCtx("sub-home", nil, ""))); got != b.ID {
		t.Errorf("with a home membership the context is %s, want home-b", got)
	}
	treeMember(t, e, b, types.CapabilitySubjectUser, "sub-home", 1, false)
	if got := unitIDOf(treeCeiling(t, e, treeCtx("sub-home", nil, ""))); got != a.ID {
		t.Errorf("with no home membership the context is %s, want the precedence winner home-a", got)
	}
}

// TestUnitContextStaleGroups: with a stale snapshot and a group-tier membership somewhere, an all-tier
// legacy winner is refused, while a user-tier legacy winner and a RunIn naming a matched
// non-legacy membership both resolve.
func TestUnitContextStaleGroups(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	everyone := e.seedProfile("stale-all", "all.example")
	mine := e.seedProfile("stale-user", "user.example")
	typed := e.seedProfile("stale-typed", "typed.example")
	if _, err := e.pg.CreateUserType(ctx, types.UserType{ID: "stale-type", Name: "stale-type", CreatedBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	treeMember(t, e, everyone, types.CapabilitySubjectAll, "", 0, false)
	treeMember(t, e, mine, types.CapabilitySubjectUser, "sub-stale-user", 0, false)
	treeMember(t, e, typed, types.CapabilitySubjectUserType, "stale-type", 0, false)
	treeMember(t, e, typed, types.CapabilitySubjectGroup, "somewhere", 0, false)
	if _, err := e.pool.Exec(ctx, `UPDATE governance_memberships SET legacy = true WHERE subject_type IN ('all','user')`); err != nil {
		t.Fatal(err)
	}
	stale := func(sub, userType string) context.Context {
		return withOIDCGroupsTruncated(treeCtx(sub, []string{"a"}, userType), true)
	}
	if _, err := e.srv.effectiveCeiling(stale("sub-stale-nobody", "")); !errors.Is(err, errGroupsSnapshotStale) {
		t.Errorf("an all-tier legacy winner on a stale snapshot: %v, want groups_snapshot_stale", err)
	}
	if c, err := e.srv.effectiveCeiling(stale("sub-stale-user", "")); err != nil || unitIDOf(c) != mine.ID {
		t.Errorf("a user-tier legacy winner on a stale snapshot: %v %v, want stale-user", unitIDOf(c), err)
	}
	if c, err := e.srv.effectiveCeiling(withContextChoice(stale("sub-stale-typed", "stale-type"), contextChoice{RunIn: typed.ID})); err != nil || unitIDOf(c) != typed.ID {
		t.Errorf("RunIn naming a matched non-legacy membership on a stale snapshot: %v %v, want stale-typed", unitIDOf(c), err)
	}
}

// TestUnitContextStaleHiddenGroupHome: on a stale snapshot, a lone user-tier candidate that is not a
// home row is refused once any group home row exists, since the hidden group row could win the home
// step and move the run. A user-tier home row and a RunIn naming the user row still resolve.
func TestUnitContextStaleHiddenGroupHome(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	mine := e.seedProfile("sh-user", "user.example")
	theirs := e.seedProfile("sh-group", "group.example")
	treeMember(t, e, mine, types.CapabilitySubjectUser, "sub-sh", 0, false)
	treeMember(t, e, theirs, types.CapabilitySubjectGroup, "sh-eng", 0, false)
	stale := func() context.Context {
		return withOIDCGroupsTruncated(treeCtx("sub-sh", []string{"a"}, ""), true)
	}
	if c, err := e.srv.effectiveCeiling(stale()); err != nil || unitIDOf(c) != mine.ID {
		t.Fatalf("no group home row, stale snapshot: %v %v, want sh-user", unitIDOf(c), err)
	}
	treeMember(t, e, theirs, types.CapabilitySubjectGroup, "sh-eng", 0, true)
	if c, err := e.srv.effectiveCeiling(treeCtx("sub-sh", []string{"sh-eng"}, "")); err != nil || unitIDOf(c) != theirs.ID {
		t.Errorf("the full snapshot with the group home row: %v %v, want sh-group", unitIDOf(c), err)
	}
	if c, err := e.srv.effectiveCeiling(stale()); !errors.Is(err, errGroupsSnapshotStale) {
		t.Errorf("a group home row exists, stale snapshot: %v %v, want groups_snapshot_stale", unitIDOf(c), err)
	}
	if c, err := e.srv.effectiveCeiling(withContextChoice(stale(), contextChoice{RunIn: mine.ID})); err != nil || unitIDOf(c) != mine.ID {
		t.Errorf("RunIn naming the user row, stale snapshot: %v %v, want sh-user", unitIDOf(c), err)
	}
	treeMember(t, e, mine, types.CapabilitySubjectUser, "sub-sh", 0, true)
	if c, err := e.srv.effectiveCeiling(stale()); err != nil || unitIDOf(c) != mine.ID {
		t.Errorf("a user-tier home row, stale snapshot: %v %v, want sh-user", unitIDOf(c), err)
	}
}

// TestDefaultUnitIsTheDeploymentDefault: the Default unit composes as the deployment default, and
// GET /governance lists it with that as its effective ceiling.
func TestDefaultUnitIsTheDeploymentDefault(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	got, err := e.srv.resolveUnitByID(context.Background(), store.DefaultUnitID)
	if err != nil {
		t.Fatal(err)
	}
	want := e.srv.deploymentAuthority()
	if !sameAsJSON(t, got.Ceiling, want.Ceiling) || !sameAsJSON(t, got.Limits, want.Limits) {
		t.Errorf("Default resolves to %+v %+v, want the deployment default %+v", got.Ceiling, got.Limits, want.Ceiling)
	}
	w := e.admin(http.MethodGet, "/api/v1/governance", "")
	var body struct {
		Units    []governanceUnitView `json:"units"`
		MaxDepth int                  `json:"max_depth"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatalf("GET /governance = %d %s", w.Code, w.Body)
	}
	i := slices.IndexFunc(body.Units, func(u governanceUnitView) bool { return u.Kind == types.UnitKindDefault })
	if i < 0 || !sameAsJSON(t, body.Units[i].Effective.Ceiling, redactSpecForRead(want.Ceiling, true)) || body.MaxDepth != 16 {
		t.Errorf("GET /governance Default = %+v, max_depth %d; want the deployment default and 16", body.Units, body.MaxDepth)
	}
}

func sameAsJSON(t *testing.T, a, b any) bool {
	t.Helper()
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// TestUnitNilIffDefault: Unit is nil exactly when the context is Default: for a person with no
// membership, and not for a member of a pass-through child of Default.
func TestUnitNilIffDefault(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	if c := treeCeiling(t, e, treeCtx("sub-nobody", nil, "")); c.Unit != nil {
		t.Errorf("no membership: Unit = %+v, want nil (the Default unit)", c.Unit)
	}
	pt := treeChild(t, e, `{"name":"pt-under-default","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	treeMember(t, e, pt, types.CapabilitySubjectUser, "sub-pt", 0, false)
	if c := treeCeiling(t, e, treeCtx("sub-pt", nil, "")); unitIDOf(c) != pt.ID {
		t.Errorf("a member of a pass-through child of Default: Unit %s, want that child", unitIDOf(c))
	}
}

// TestUnitDepthLimit: a path one past the default 16 is refused naming the limit, one at 16 passes,
// and the site config cannot lower the limit below the deepest path, which the refusal names.
func TestUnitDepthLimit(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	prev := e.seedProfile("depth-1", "pypi.org")
	for i := 2; i <= 16; i++ {
		prev = treeChild(t, e, fmt.Sprintf(`{"name":"depth-%d","parent_id":"%s","overlay":{}}`, i, prev.ID))
	}
	w := e.admin(http.MethodPost, "/api/v1/governance/units", fmt.Sprintf(`{"name":"depth-17","parent_id":"%s","overlay":{}}`, prev.ID))
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceUnitDepth || !strings.Contains(w.Body.String(), "16") {
		t.Errorf("a 17-deep write = %d %s, want 409 %s naming 16", w.Code, w.Body, reasonGovernanceUnitDepth)
	}
	w = e.admin(http.MethodPut, "/api/v1/site-config", `{"governance_max_depth":15}`)
	if w.Code != http.StatusBadRequest || wireReason(t, w) != reasonSiteConfigInvalid || !strings.Contains(w.Body.String(), "16 deep (depth-1") {
		t.Errorf("lowering below the deepest path = %d %s, want 400 %s naming it", w.Code, w.Body, reasonSiteConfigInvalid)
	}
	if w = e.admin(http.MethodPut, "/api/v1/site-config", `{"governance_max_depth":16}`); w.Code != http.StatusOK {
		t.Errorf("a limit at the deepest path = %d %s, want 200", w.Code, w.Body)
	}
}

// TestUnitSaveWithNullParentStaysUnderDefault: the console shows a unit under Default as having no
// parent, so its no-op save sends parent_id null (with overlay {}) and, for a pass-through unit, no
// mode. For each unit mode, with the mode left out and with it sent, the write must keep Default as the
// parent and the unit must compose byte-identically before and after.
func TestUnitSaveWithNullParentStaysUnderDefault(t *testing.T) {
	def := store.DefaultUnitID.String()
	for _, c := range []struct {
		mode, create, save string
	}{
		{types.UnitModeOwnWalls, `"overlay":{},"overlay_limits":{"deny_interactive":true}`, `"overlay":{},"overlay_limits":{"deny_interactive":true}`},
		{types.UnitModePassThrough, `"mode":"pass_through"`, `"overlay":{}`},
	} {
		for _, sendMode := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mode sent=%v", c.mode, sendMode), func(t *testing.T) {
				e := newGovEnvOn(t, throwawayPGPool(t))
				u := treeChild(t, e, `{"name":"under-default","parent_id":"`+def+`",`+c.create+`}`)
				before, err := e.srv.resolveUnitByID(context.Background(), u.ID)
				if err != nil {
					t.Fatal(err)
				}
				body := `{"name":"under-default","parent_id":null,` + c.save
				if sendMode {
					body += `,"mode":"` + c.mode + `"`
				}
				w := e.admin(http.MethodPut, "/api/v1/governance/units/"+u.ID.String(), body+`}`)
				if w.Code != http.StatusOK {
					t.Fatalf("no-op save = %d %s, want 200", w.Code, w.Body)
				}
				rows, err := e.pg.ListGovernanceUnits(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range rows {
					if r.ID == u.ID && (r.ParentID == nil || *r.ParentID != store.DefaultUnitID || r.Mode != c.mode) {
						t.Errorf("stored parent_id %v mode %q after the save, want %s %q", r.ParentID, r.Mode, def, c.mode)
					}
				}
				after, err := e.srv.resolveUnitByID(context.Background(), u.ID)
				if err != nil || !sameAsJSON(t, before.Ceiling, after.Ceiling) || !sameAsJSON(t, before.Limits, after.Limits) ||
					!sameAsJSON(t, before.AuthoredResources, after.AuthoredResources) {
					t.Errorf("composed after the save = %+v (%v), want %+v", after, err, before)
				}
			})
		}
	}
}

// TestPassThroughSaveKeepsItsRealBase: moving the null-parent mapping ahead of the pass-through check
// changes nothing for a pass-through unit with a real base, a missing one, or no base at all.
func TestPassThroughSaveKeepsItsRealBase(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	base := e.seedProfile("pt-base", "pypi.org")
	u := treeChild(t, e, `{"name":"pt-kept","mode":"pass_through","parent_id":"`+base.ID.String()+`"}`)
	w := e.admin(http.MethodPut, "/api/v1/governance/units/"+u.ID.String(), `{"name":"pt-kept","parent_id":"`+base.ID.String()+`","overlay":{},"mode":"pass_through"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("a pass-through save naming its base = %d %s, want 200", w.Code, w.Body)
	}
	rows, err := e.pg.ListGovernanceUnits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == u.ID && (r.ParentID == nil || *r.ParentID != base.ID) {
			t.Errorf("stored parent_id = %v, want the named base %s", r.ParentID, base.ID)
		}
	}
	for name, body := range map[string]string{
		"missing base": `{"name":"pt-orphan","mode":"pass_through","parent_id":"` + uuid.New().String() + `","overlay":{}}`,
		"no base":      `{"name":"pt-top","mode":"pass_through"}`,
	} {
		w := e.admin(http.MethodPost, "/api/v1/governance/units", body)
		if w.Code != http.StatusBadRequest || wireReason(t, w) != reasonGovernanceUnitRequestInvalid {
			t.Errorf("%s: POST = %d %s, want 400 %s", name, w.Code, w.Body, reasonGovernanceUnitRequestInvalid)
		}
	}
}

// TestUnitPassThrough: a pass-through child has its parent's walls exactly, and a pass-through unit
// that names no parent is refused.
func TestUnitPassThrough(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	parent := e.seedProfile("pt-parent", "pypi.org", "parent.example")
	child := treeChild(t, e, `{"name":"pt-child","mode":"pass_through","parent_id":"`+parent.ID.String()+`"}`)
	pr, err := e.srv.resolveUnitByID(context.Background(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	cr, err := e.srv.resolveUnitByID(context.Background(), child.ID)
	if err != nil || !sameAsJSON(t, cr.Ceiling, pr.Ceiling) || !sameAsJSON(t, cr.Limits, pr.Limits) {
		t.Errorf("pass-through child = %+v (%v), want its parent's %+v", cr, err, pr.Ceiling)
	}
	w := e.admin(http.MethodPost, "/api/v1/governance/units", `{"name":"pt-top","mode":"pass_through"}`)
	if w.Code != http.StatusBadRequest || wireReason(t, w) != reasonGovernanceUnitRequestInvalid || !strings.Contains(w.Body.String(), passThroughTopLevelMsg) {
		t.Errorf("a top-level pass-through = %d %s, want 400 %s", w.Code, w.Body, reasonGovernanceUnitRequestInvalid)
	}
}

// TestMembershipKeysOnUnit: one subject is a member of two units (two rows), re-posting one pair
// updates it, and with the second human on two held adds for one subject coexist and each applies.
func TestMembershipKeysOnUnit(t *testing.T) {
	e := newGovEnv(t)
	a := e.seedProfile("mk-a", "a.example")
	b := e.seedProfile("mk-b", "b.example")
	var ids []uuid.UUID
	for _, u := range []types.GovernanceUnit{a, b} {
		w := e.call(e.alice, http.MethodPost, "/api/v1/governance/memberships",
			fmt.Sprintf(`{"subject_type":"user","subject":"sub-mk","unit_id":"%s","priority":1}`, u.ID))
		if w.Code != http.StatusAccepted {
			t.Fatalf("held add to %s = %d %s, want 202", u.Name, w.Code, w.Body)
		}
		ids = append(ids, e.pending(w).ID)
	}
	for _, id := range ids {
		if w := e.call(e.bob, http.MethodPost, approvePath(id), ""); w.Code != http.StatusOK {
			t.Fatalf("approve %s = %d %s, want 200", id, w.Code, w.Body)
		}
	}
	rows := func() []types.GovernanceMembership {
		all, err := e.pg.ListGovernanceMemberships(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return slices.DeleteFunc(all, func(m types.GovernanceMembership) bool { return m.Subject != "sub-mk" })
	}
	if got := rows(); len(got) != 2 {
		t.Fatalf("memberships of sub-mk = %+v, want one per unit", got)
	}
	m, err := e.pg.UpsertGovernanceMembership(context.Background(), types.GovernanceMembership{
		SubjectType: types.CapabilitySubjectUser, Subject: "sub-mk", UnitID: a.ID, Priority: 7, CreatedBy: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	got := rows()
	if len(got) != 2 || !slices.ContainsFunc(got, func(r types.GovernanceMembership) bool { return r.ID == m.ID && r.Priority == 7 && r.UnitID == a.ID }) {
		t.Errorf("after re-posting (mk-a, sub-mk) at 7: %+v, want the same two rows, mk-a's at 7", got)
	}
}

// TestReviveMembershipGone: a revive is refused unit_membership_gone once the owner holds no
// membership placing them in the run's unit. The owner's own session re-resolves in full; a super
// admin's revive and an admin's bulk restart of a group-assigned owner's run keep working while the
// unit still has a membership that could match the owner.
func TestReviveMembershipGone(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	u := e.seedProfile("rv-unit", "pypi.org")
	own := treeMember(t, e, u, types.CapabilitySubjectUser, "sub-rv-owner", 0, false)
	run := types.AgentRun{ID: uuid.New(), UnitID: &u.ID, CreatedBy: "sub-rv-owner", UserType: types.UserTypeStandard}
	owner := treeCtx("sub-rv-owner", []string{"rv-eng"}, "")
	if _, ref := e.srv.ownerUnit(owner, run); ref != nil {
		t.Fatalf("the owner's revive with the membership: %+v", ref)
	}
	if _, err := e.pg.DeleteGovernanceMembership(ctx, own.ID); err != nil {
		t.Fatal(err)
	}
	if _, rerr := e.srv.reviveCeiling(owner, run, types.ActorHuman, "sub-rv-owner"); rerr == nil || rerr.reason != reasonUnitMembershipGone || rerr.status != http.StatusConflict {
		t.Errorf("the owner's revive after the membership was deleted: %+v, want 409 %s", rerr, reasonUnitMembershipGone)
	}
	if !slices.ContainsFunc(e.audits("run.revive"), func(ev types.AuditEvent) bool {
		return strings.Contains(string(ev.Data), `"arm":"owner_session"`) && strings.Contains(string(ev.Data), reasonUnitMembershipGone)
	}) {
		t.Errorf("no run.revive denied row naming the owner_session arm: %+v", e.audits("run.revive"))
	}
	group := treeMember(t, e, u, types.CapabilitySubjectGroup, "rv-eng", 0, false)
	superAdmin := operatorCtx("sub-rv-super", "rv-super@corp.example", oidc.RoleAdmin)
	for name, c := range map[string]context.Context{"super-admin revive": superAdmin, "admin bulk restart": ctx} {
		if _, ref := e.srv.ownerUnit(c, run); ref != nil {
			t.Errorf("%s of a group-assigned owner's run: %+v, want it to pass while the group membership exists", name, ref)
		}
	}
	if _, err := e.pg.DeleteGovernanceMembership(ctx, group.ID); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]context.Context{"super-admin revive": superAdmin, "admin bulk restart": ctx} {
		if _, ref := e.srv.ownerUnit(c, run); ref == nil || ref.reason != reasonUnitMembershipGone || ref.arm != "row_can_match" {
			t.Errorf("%s with no membership left that could match: %+v, want %s (row_can_match)", name, ref, reasonUnitMembershipGone)
		}
	}
	// Above and below: a membership on a unit above the captured one places the owner in it; one only
	// on a unit below it does not, on either arm.
	top := treeChild(t, e, `{"name":"rv-top","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	mid := treeChild(t, e, `{"name":"rv-mid","mode":"pass_through","parent_id":"`+top.ID.String()+`"}`)
	low := treeChild(t, e, `{"name":"rv-low","mode":"pass_through","parent_id":"`+mid.ID.String()+`"}`)
	inMid := types.AgentRun{ID: uuid.New(), UnitID: &mid.ID, CreatedBy: "sub-rv-owner", UserType: types.UserTypeStandard}
	arms := map[string]context.Context{"the owner's revive": treeCtx("sub-rv-owner", []string{"rv-eng"}, ""),
		"super-admin revive": superAdmin, "admin bulk restart": ctx}
	below := treeMember(t, e, low, types.CapabilitySubjectUser, "sub-rv-owner", 0, false)
	for name, c := range arms {
		if _, ref := e.srv.ownerUnit(c, inMid); ref == nil || ref.reason != reasonUnitMembershipGone {
			t.Errorf("%s with a membership only below the run's unit: %+v, want %s", name, ref, reasonUnitMembershipGone)
		}
	}
	if _, err := e.pg.DeleteGovernanceMembership(ctx, below.ID); err != nil {
		t.Fatal(err)
	}
	treeMember(t, e, top, types.CapabilitySubjectUser, "sub-rv-owner", 0, false)
	for name, c := range arms {
		if _, ref := e.srv.ownerUnit(c, inMid); ref != nil {
			t.Errorf("%s with a membership above the run's unit: %+v, want it to pass", name, ref)
		}
	}
}

// TestReviveEmailRowNeverAdmits: when anyone but the owner revives or restarts a run, an email-keyed
// user row on the run's unit never counts as the owner's membership: with no other row that could
// match, the revive is refused unit_membership_gone on the row_can_match arm.
func TestReviveEmailRowNeverAdmits(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	u := e.seedProfile("rv-email", "pypi.org")
	treeMember(t, e, u, types.CapabilitySubjectUser, "rv-owner@corp.example", 0, false)
	run := types.AgentRun{ID: uuid.New(), UnitID: &u.ID, CreatedBy: "sub-rv-email-owner", UserType: types.UserTypeStandard}
	superAdmin := operatorCtx("sub-rv-email-super", "rv-super@corp.example", oidc.RoleAdmin)
	for name, c := range map[string]context.Context{"super-admin revive": superAdmin, "admin bulk restart": context.Background()} {
		if _, ref := e.srv.ownerUnit(c, run); ref == nil || ref.reason != reasonUnitMembershipGone || ref.arm != "row_can_match" {
			t.Errorf("%s with only an email row on the unit: %+v, want %s (row_can_match)", name, ref, reasonUnitMembershipGone)
		}
	}
}

// TestBreakGlassUnitsPassThroughOnly: in hard_limits, a tree of only Default and a pass-through
// child lifts nothing, so an admin-token run needs no reason and is untagged; a unit with walls of
// its own makes it ask.
func TestBreakGlassUnitsPassThroughOnly(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	putBoundary(t, e, testBoundary(types.BoundaryHardLimits))
	treeChild(t, e, `{"name":"bg-pass","mode":"pass_through","parent_id":"`+store.DefaultUnitID.String()+`"}`)
	post := func() *httptest.ResponseRecorder {
		return e.admin(http.MethodPost, "/api/v1/runs/preflight", `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC1","allow_all_egress":true}}`)
	}
	if w := post(); w.Code != http.StatusOK {
		t.Errorf("Default and a pass-through child only, no reason: %d %s, want 200", w.Code, w.Body)
	}
	e.seedProfile("bg-walls", "pypi.org")
	if w := post(); w.Code != http.StatusBadRequest || wireReason(t, w) != reasonBreakGlassReasonRequired || !strings.Contains(w.Body.String(), "units with their own walls exist") {
		t.Errorf("with a unit of its own walls, no reason: %d %s, want 400 %s", w.Code, w.Body, reasonBreakGlassReasonRequired)
	}
}

// TestQueuedStartKeepsTheContext: a queued launch starts in the unit it was queued under, and one
// whose membership was removed while it waited ends queue_profile_changed.
func TestQueuedStartKeepsTheContext(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	a := e.seedProfile("q-a", "a.example")
	b := e.seedProfile("q-b", "b.example")
	treeMember(t, e, a, types.CapabilitySubjectUser, "sub-q", 5, false)
	mb := treeMember(t, e, b, types.CapabilitySubjectUser, "sub-q", 1, false)
	start := func(launch queuedLaunch) *queueEnd {
		t.Helper()
		auth, _ := json.Marshal(queueAuthority{Sub: "sub-q", Role: "user", UserType: types.UserTypeStandard, Groups: []string{}})
		launch.Request = json.RawMessage(`{}`)
		raw, _ := json.Marshal(launch)
		run := types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: "sub-q"}
		st, err := newQueuedStart(context.Background(), context.Background(), store.PromotedRun{Run: run, Entry: store.RunQueueEntry{Authority: auth, Launch: raw}})
		if err != nil {
			t.Fatal(err)
		}
		_, end := e.srv.queueGovernance(st, createRunRequest{Agent: "claude-code"})
		return end
	}
	if end := start(queuedLaunch{UnitID: &b.ID, RunIn: b.ID}); end != nil && end.token == string(types.RunEndQueueProfileChanged) {
		t.Errorf("a launch queued in q-b (not the precedence winner) ended %+v at the start, want it to start in q-b", end)
	}
	if _, err := e.pg.DeleteGovernanceMembership(context.Background(), mb.ID); err != nil {
		t.Fatal(err)
	}
	if end := start(queuedLaunch{UnitID: &b.ID}); end == nil || end.token != string(types.RunEndQueueProfileChanged) {
		t.Errorf("membership removed while queued: end %+v, want %s", end, types.RunEndQueueProfileChanged)
	}
}

// TestGovernanceCheckNamesTheHomeContext: the admin check names the unit effectiveCeiling resolves
// for the same claims: the home context, not the first membership by precedence.
func TestGovernanceCheckNamesTheHomeContext(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	a := e.seedProfile("chk-a", "a.example")
	b := e.seedProfile("chk-b", "b.example")
	treeMember(t, e, a, types.CapabilitySubjectUser, "sub-chk", 9, false)
	treeMember(t, e, b, types.CapabilitySubjectUser, "sub-chk", 1, true)
	w := e.admin(http.MethodPost, "/api/v1/governance/check", `{"user_subjects":["sub-chk"]}`)
	var got governancePreviewResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("check = %d %s", w.Code, w.Body)
	}
	want := unitIDOf(treeCeiling(t, e, treeCtx("sub-chk", nil, "")))
	if got.UnitID != want || want != b.ID {
		t.Errorf("check names %s, effectiveCeiling %s; want both chk-b (the home context)", got.UnitID, want)
	}
}
