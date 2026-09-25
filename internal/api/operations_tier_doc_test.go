// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// operationsDoc is the operator manual's text, normalized for prose assertions:
// markdown hard-wraps sentences, so a claim that reads as one line to a human is
// two or three lines to strings.Contains.
func operationsDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/OPERATIONS.md")
	if err != nil {
		t.Fatalf("read docs/OPERATIONS.md: %v", err)
	}
	return string(b)
}

func unwrapped(s string) string { return strings.Join(strings.Fields(s), " ") }

// docTierGate is what the "What admin-only still means" table must say in the
// Gate column for a route of each router class.
var docTierGate = map[routeClass]string{
	classAdmin:    "⛔ admin only",
	classSecurity: "⛔ admin or `security_admin`",
}

// docTierRow pairs a route the OPERATIONS.md tier table names with the exact
// text it is named by. The route key is looked up in routeMatrix — the
// authoritative classification the chi.Walk test enforces against the live
// router — so a route that MOVES between operatorOnly and securityOps fails
// here until the table moves with it.
var docTierRows = []struct{ route, token string }{
	// operatorOnly (SUPER)
	{"PUT /api/v1/site-config", "`PUT /site-config`"},
	// R1 F316's four operator-topology reads: re-tiered to admin this wave and
	// filed as docTierUndocumented until the docs pass landed the row naming
	// them. Now that it has, they move here keyed on the row's own token.
	{"GET /api/v1/site-config", "the operator-topology READS"},
	{"GET /api/v1/sources", "the operator-topology READS"},
	{"GET /api/v1/sources/{id}", "the operator-topology READS"},
	{"GET /api/v1/base-images", "the operator-topology READS"},
	{"GET /metrics", "`GET /metrics`"},
	{"POST /api/v1/access/mappings", "`/access` role-mapping routes"},
	{"PUT /api/v1/workspaces/{id}/llm-cred", "`llm-cred`"},
	{"PUT /api/v1/workspaces/{id}/requirements", "`requirements`"},
	{"POST /api/v1/workspaces/{id}/env-as-code/write", "`env-as-code/write`"},
	{"POST /api/v1/workspaces/{id}/reassign", "`reassign`"},
	// The /drives family arrived with the user-drives merge and its own table
	// rows; keyed on the mount function the row names, which is also what
	// decides the tier. Issue #168 (0.8) split the family across two doc rows
	// — the host-naming four stay on `mountUserDriveRoutes`'s token, and the
	// three that moved to securityOps get their own tokens below.
	{"POST /api/v1/drives", "`mountUserDriveRoutes`"},
	// The workspace-provider policy (0.7.2). Separate tokens per verb, the
	// record/promote-egress precedent: the two share a tier today, and a later
	// widening of the READ to a member-safe projection must red here rather than
	// pass on the write's row.
	{"GET /api/v1/workspace-providers", "`GET /workspace-providers`"},
	{"PUT /api/v1/workspace-providers", "`PUT /workspace-providers`"},
	// The agent roster (0.7.2), separate tokens per verb for the same reason as
	// the two rows above: the READ is the half a later member-safe projection
	// could widen, and that must red here rather than pass on the write's row.
	{"GET /api/v1/agent-providers", "`GET /agent-providers`"},
	{"PUT /api/v1/agent-providers", "`PUT /agent-providers`"},
	// Model providers (0.8), per verb for the same reason.
	{"GET /api/v1/model-providers", "`GET /model-providers`"},
	{"PUT /api/v1/model-providers", "`PUT /model-providers`"},
	// securityOps (SEC) — the eight the pre-0.7 table marked admin-only, plus
	// the rest of the tier the same table now names.
	{"PUT /api/v1/workspaces/{id}/approved-egress", "`approved-egress`"},
	{"PUT /api/v1/workspaces/{id}/denied-egress", "`denied-egress`"},
	// SEPARATE TOKENS, because 0.7 re-tiered one of these and not the other.
	// They shared the surface token "`record` + `promote-egress`" while they
	// shared a tier; once record moved to admin-only and promote-egress stayed
	// on the security tier, one token could not express two gates and this
	// guard could not pass whatever the doc said. Each names its own row now.
	{"POST /api/v1/workspaces/{id}/record", "`POST /workspaces/{id}/record`"},
	{"POST /api/v1/workspaces/{id}/record/{task}/promote-egress", "`promote-egress`"},
	{"POST /api/v1/site-config/test-proxy", "`POST /site-config/test-proxy`"},
	{"POST /api/v1/site-config/test-redirect", "`/test-redirect`"},
	{"GET /api/v1/permissions", "the `/permissions` routes below"},
	{"PUT /api/v1/permissions/enforcement", "the `/permissions` routes below"},
	{"GET /api/v1/tokens", "`GET`/`DELETE /tokens`"},
	{"POST /api/v1/sessions/revoke", "`POST /sessions/revoke`"},
	{"GET /api/v1/audit/chain/verify", "`GET /audit/chain/verify`"},
	{"POST /api/v1/governance/profiles", "`/governance` profile and assignment routes"},
	{"GET /api/v1/access/directory/search", "`GET /access/directory/search`"},
	// Issue #168 (0.8): the three /drives routes that DON'T name a host path
	// moved off classAdmin. Each gets its own token — the doc row lists all
	// three by name rather than through `mountUserDriveRoutes`, which stays
	// the host-naming four's token below.
	{"POST /api/v1/drives/grants", "`POST /drives/grants`"},
	{"DELETE /api/v1/drives/grants/{id}", "`DELETE /drives/grants/{id}`"},
	{"POST /api/v1/drives/preview", "`POST /drives/preview`"},

	// the rest of the gated surface (R1 F316)
	//
	// Through 0.7 this list was 22 hand-picked representatives out of 59 gated
	// routes, and nothing bounded it: the guard checked that the rows we had
	// named were right, never that every gated route had a row. Four
	// operator-topology READS were re-tiered to admin in this wave and appear
	// nowhere in the table, and no test could see it.
	//
	// So every gated route now names the token that covers it, and a route with
	// NO covering token is listed in docTierUndocumented below with the reason.
	// Several rows deliberately cover a FAMILY — the doc names
	// `mountUserDriveRoutes` rather than four /drives lines — and that is the
	// document being readable rather than the guard being loose: the mapping is
	// per route either way, so a route that leaves the family still has to be
	// re-pointed here. (The other three /drives routes moved to their own
	// tokens above in #168.)
	{"GET /api/v1/drives", "`mountUserDriveRoutes`"},
	{"PUT /api/v1/drives/{id}", "`mountUserDriveRoutes`"},
	{"DELETE /api/v1/drives/{id}", "`mountUserDriveRoutes`"},
	{"DELETE /api/v1/access/mappings/{id}", "`/access` role-mapping routes"},
	{"GET /api/v1/access", "`/access` role-mapping routes"},
	{"POST /api/v1/access/preview", "`/access` role-mapping routes"},
	{"POST /api/v1/policies", "policy create/update/delete"},
	{"PUT /api/v1/policies/{id}", "policy create/update/delete"},
	{"DELETE /api/v1/policies/{id}", "policy create/update/delete"},
	// POST /setup/harness-login is NOT here: 0.7.2 moved the container LOGIN
	// launch off the gated tier (classMember with an in-handler predicate), and
	// this list covers gated routes only. The tier table names it in its own
	// member row instead.
	{"PUT /api/v1/setup/harness-credential/{provider}", "managed harness credential"},
	{"DELETE /api/v1/setup/harness-credential/{provider}", "managed harness credential"},
	{"DELETE /api/v1/tokens/{id}", "`GET`/`DELETE /tokens`"},
	{"GET /api/v1/governance", "`/governance` profile and assignment routes"},
	{"PUT /api/v1/governance/profiles/{id}", "`/governance` profile and assignment routes"},
	{"DELETE /api/v1/governance/profiles/{id}", "`/governance` profile and assignment routes"},
	{"POST /api/v1/governance/assignments", "`/governance` profile and assignment routes"},
	{"DELETE /api/v1/governance/assignments/{id}", "`/governance` profile and assignment routes"},
	{"POST /api/v1/governance/preview", "`/governance` profile and assignment routes"},
	{"GET /api/v1/user-types", "the `/user-types` routes"},
	{"POST /api/v1/user-types", "the `/user-types` routes"},
	{"PUT /api/v1/user-types/{id}", "the `/user-types` routes"},
	{"DELETE /api/v1/user-types/{id}", "the `/user-types` routes"},
	{"POST /api/v1/permissions/grants", "the `/permissions` routes below"},
	{"DELETE /api/v1/permissions/grants/{id}", "the `/permissions` routes below"},

	// The gated routes below are rowed in docs/OPERATIONS.md like the rest;
	// none remain in docTierUndocumented.
	{"POST /api/v1/sources", "the `/sources` writes"},
	{"POST /api/v1/sources/{id}/scan", "the `/sources` writes"},
	{"DELETE /api/v1/sources/{id}", "the `/sources` writes"},
	{"POST /api/v1/base-images", "the `/base-images` writes"},
	{"DELETE /api/v1/base-images/{id}", "the `/base-images` writes"},
	{"PUT /api/v1/integrations/{id}", "`PUT`/`DELETE /integrations/{id}`"},
	{"DELETE /api/v1/integrations/{id}", "`PUT`/`DELETE /integrations/{id}`"},
	{"POST /api/v1/admin/sandboxes/sweep", "`POST /admin/sandboxes/sweep`"},
	{"POST /api/v1/setup/onboarding-complete", "`POST /setup/onboarding-complete`"},
	{"GET /api/v1/runs/{id}/attach", "`GET /runs/{id}/attach`"},
	// Hybrid enrolment (0.8): the mint is SUPER, the inventory and revoke are
	// the security tier's inventory-then-revoke pair.
	{"POST /api/v1/admin/devices/enrolment-tokens", "`POST /admin/devices/enrolment-tokens`"},
	{"GET /api/v1/admin/devices", "`GET /admin/devices` and `DELETE /admin/devices/{id}`"},
	{"DELETE /api/v1/admin/devices/{id}", "`GET /admin/devices` and `DELETE /admin/devices/{id}`"},
	{"GET /api/v1/admin/devices/enrolment-tokens", "`GET /admin/devices/enrolment-tokens` and `DELETE /admin/devices/enrolment-tokens/{id}`"},
	{"DELETE /api/v1/admin/devices/enrolment-tokens/{id}", "`GET /admin/devices/enrolment-tokens` and `DELETE /admin/devices/enrolment-tokens/{id}`"},
}

// docTierUndocumented names the gated routes the tier table does not cover, each
// with the reason it is not covered yet. It is a RATCHET, not an exemption list:
// a route here is admitted debt, and a NEW gated route that lands in neither
// list fails the completeness check below rather than going unnoticed.
//
// The map is empty: every gated route the completeness check has surfaced is
// rowed in docs/OPERATIONS.md and listed in docTierRows above. It stays
// declared, empty, as the ratchet's landing spot for the next gap a
// completeness failure surfaces.
var docTierUndocumented = map[string]string{}

// TestOperationsTierTableMatchesRouteMatrix pins docs/OPERATIONS.md's "What
// admin-only still means" table — the section the doc's own intro sends a
// reader to for "who can change what" — to the router's real classification.
//
// Through 0.6 every gated route was operatorOnly and the table could say "admin
// only" about all of them. 0.7 added securityOps (admin OR security_admin) and
// moved ten routes onto it, including the four that decide a workspace's egress
// ceiling and the whole /permissions family; the table kept marking them admin
// only, so an operator deciding whether to delegate the new tier read that it
// could not widen an egress ceiling or write the capability grant table, when it
// can do both. A doc that is wrong about a delegation boundary is a security
// defect in slow motion, and nothing failed when it drifted.
func TestOperationsTierTableMatchesRouteMatrix(t *testing.T) {
	rows := tierTableRows(t, operationsDoc(t))
	for _, dr := range docTierRows {
		rc, ok := routeMatrix[dr.route]
		if !ok {
			t.Errorf("OPERATIONS.md's tier table names %q, which the route matrix does not classify", dr.route)
			continue
		}
		want, ok := docTierGate[rc.class]
		if !ok {
			t.Errorf("route %s is class %q — the tier table documents only the two gated classes", dr.route, rc.class)
			continue
		}
		var got []string
		for _, row := range rows {
			if strings.Contains(row.surface, dr.token) {
				got = append(got, row.gate)
			}
		}
		if len(got) != 1 {
			t.Errorf("%s: the tier table names %s in %d rows, want exactly 1", dr.route, dr.token, len(got))
			continue
		}
		// The gate cell LEADS with the tier and may append a rationale after it
		// (the drives rows do). Match the tier as a prefix: the two tier strings
		// are not prefixes of each other, so this still fails on a wrong tier —
		// it only tolerates the document's habit of explaining itself in place.
		if !strings.HasPrefix(got[0], want) {
			t.Errorf("%s is %s in the router, but OPERATIONS.md's tier table gates %s as %q, want it to lead with %q",
				dr.route, rc.class, dr.token, got[0], want)
		}
	}

	// The reverse direction, which is what makes this a guard rather than a
	// spot-check (R1 F316). Everything above asks "are the rows we listed
	// right"; this asks "is every gated route listed", which is what the
	// section claims to be. Without it the list was 22 of 59 and a re-tiering
	// could land in routeMatrix with no doc row and nothing to say so — exactly
	// how four operator-topology reads became admin-only in silence.
	covered := map[string]bool{}
	for _, dr := range docTierRows {
		covered[dr.route] = true
	}
	for route, rc := range routeMatrix {
		if rc.class != classAdmin && rc.class != classSecurity {
			continue
		}
		if covered[route] || docTierUndocumented[route] != "" {
			continue
		}
		t.Errorf("%s is gated (%s) and the OPERATIONS.md tier table does not cover it. An operator reads that "+
			"table to decide what to delegate, so a gated surface missing from it is a delegation boundary "+
			"nobody can see. Name it in docTierRows with the token that covers it, or add it to "+
			"docTierUndocumented with the reason and file the row for the docs pass", route, rc.class)
	}

	// …and the debt list must not rot. An entry naming a route the table now
	// covers, or one the router no longer gates, is a stale licence to omit.
	for route, why := range docTierUndocumented {
		rc, ok := routeMatrix[route]
		if !ok || (rc.class != classAdmin && rc.class != classSecurity) {
			t.Errorf("docTierUndocumented names %q (%s), which the router no longer gates — drop the entry", route, why)
			continue
		}
		if covered[route] {
			t.Errorf("docTierUndocumented names %q, which docTierRows now covers — drop the entry", route)
		}
	}
}

type tierRow struct{ surface, gate string }

// tierTableRows returns the body rows of the "What admin-only still means"
// markdown table.
func tierTableRows(t *testing.T, doc string) []tierRow {
	t.Helper()
	i := strings.Index(doc, "**What admin-only still means**")
	if i < 0 {
		t.Fatal(`docs/OPERATIONS.md has no "What admin-only still means" section`)
	}
	var rows []tierRow
	started := false
	for _, line := range strings.Split(doc[i:], "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			if started {
				break // the table ended
			}
			continue
		}
		started = true
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 2 {
			t.Fatalf("tier table row is not two columns: %q", line)
		}
		surface, gate := strings.TrimSpace(cells[0]), strings.TrimSpace(cells[1])
		if surface == "Surface" || strings.HasPrefix(surface, "---") {
			continue // header / separator
		}
		rows = append(rows, tierRow{surface: surface, gate: gate})
	}
	if len(rows) == 0 {
		t.Fatal("no rows parsed from the tier table")
	}
	return rows
}

// TestOperationsDocDescribesThreeRoleAuthz pins the prose around that table:
// the role model itself, the rule that folds two matching map entries into one
// role, and the two paragraphs that justify a gate by naming the tier the gated
// route sits on. Each was written against the pre-0.7 two-role router and each
// stayed literally true-sounding after the third role landed, which is why no
// test caught them.
func TestOperationsDocDescribesThreeRoleAuthz(t *testing.T) {
	doc := unwrapped(operationsDoc(t))

	// deriveRole can return three roles, so the manual cannot describe two.
	for _, role := range []string{oidc.RoleAdmin, oidc.RoleSecurityAdmin, oidc.RoleUser} {
		if !oidc.ValidRole(role) {
			t.Fatalf("%q is no longer a role — revisit this guard, not the doc", role)
		}
		if !strings.Contains(doc, role) {
			t.Errorf("OPERATIONS.md never names the derivable role %q", role)
		}
	}
	for _, stale := range []struct{ claim, why string }{
		{"a real two-role model",
			"deriveRole derives three roles (oidc.ValidRole)"},
		{"**Any match resolving to `admin` wins** over one resolving to `member`",
			"the fold is roleRank's three ranks: user < security_admin < admin"},
		{"both already `operatorOnly`",
			"approved-egress and denied-egress are registered on securityOps (routes.go)"},
		{"(all `operatorOnly` except the last)",
			"the four /permissions routes are mounted on securityOps (mountPermissionRoutes)"},
	} {
		if strings.Contains(doc, unwrapped(stale.claim)) {
			t.Errorf("OPERATIONS.md still claims %q — %s", stale.claim, stale.why)
		}
	}
	// And it must say what the fold actually is, not merely stop being wrong.
	if !strings.Contains(doc, "`user` < `security_admin` < `admin`") {
		t.Error(`OPERATIONS.md's "Deriving the role" never states roleRank's order (user < security_admin < admin)`)
	}
}

// TestEnvDocRoleMapNamesTheSecurityTier is the ENV.md half of the same drift:
// WARDYN_OIDC_ROLE_MAP's row is where an admin reads what values a `value=role`
// pair may take, and it listed only the pre-0.7 two. The role map is the ONLY
// way a session reaches security_admin (oidc.RoleSecurityAdmin's doc), so a row
// that never names the value documents the tier out of existence.
func TestEnvDocRoleMapNamesTheSecurityTier(t *testing.T) {
	b, err := os.ReadFile("../../docs/ENV.md")
	if err != nil {
		t.Fatalf("read docs/ENV.md: %v", err)
	}
	var row string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "| `WARDYN_OIDC_ROLE_MAP`") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatal("docs/ENV.md has no WARDYN_OIDC_ROLE_MAP row")
	}
	if !strings.Contains(row, oidc.RoleSecurityAdmin) {
		t.Errorf("ENV.md's WARDYN_OIDC_ROLE_MAP row never names %q, the only role value this map can grant that nothing else can", oidc.RoleSecurityAdmin)
	}
}
