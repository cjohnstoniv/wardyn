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
	{"GET /metrics", "`GET /metrics`"},
	{"POST /api/v1/access/mappings", "`/access` role-mapping routes"},
	{"PUT /api/v1/workspaces/{id}/llm-cred", "`llm-cred`"},
	{"PUT /api/v1/workspaces/{id}/requirements", "`requirements`"},
	{"POST /api/v1/workspaces/{id}/env-as-code/write", "`env-as-code/write`"},
	{"POST /api/v1/workspaces/{id}/reassign", "`reassign`"},
	// securityOps (SEC) — the eight the pre-0.7 table marked admin-only, plus
	// the rest of the tier the same table now names.
	{"PUT /api/v1/workspaces/{id}/approved-egress", "`approved-egress`"},
	{"PUT /api/v1/workspaces/{id}/denied-egress", "`denied-egress`"},
	{"POST /api/v1/workspaces/{id}/record", "`record` + `promote-egress`"},
	{"POST /api/v1/workspaces/{id}/record/{task}/promote-egress", "`record` + `promote-egress`"},
	{"POST /api/v1/site-config/test-proxy", "`POST /site-config/test-proxy`"},
	{"POST /api/v1/site-config/test-redirect", "`/test-redirect`"},
	{"GET /api/v1/permissions", "the `/permissions` routes below"},
	{"PUT /api/v1/permissions/enforcement", "the `/permissions` routes below"},
	{"GET /api/v1/tokens", "`GET`/`DELETE /tokens`"},
	{"POST /api/v1/sessions/revoke", "`POST /sessions/revoke`"},
	{"GET /api/v1/audit/chain/verify", "`GET /audit/chain/verify`"},
	{"POST /api/v1/governance/profiles", "`/governance` profile and assignment routes"},
	{"GET /api/v1/access/directory/search", "`GET /access/directory/search`"},
}

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
		if got[0] != want {
			t.Errorf("%s is %s in the router, but OPERATIONS.md's tier table gates %s as %q, want %q",
				dr.route, rc.class, dr.token, got[0], want)
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
	for _, role := range []string{oidc.RoleAdmin, oidc.RoleSecurityAdmin, oidc.RoleMember} {
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
			"the fold is roleRank's three ranks: member < security_admin < admin"},
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
	if !strings.Contains(doc, "`member` < `security_admin` < `admin`") {
		t.Error(`OPERATIONS.md's "Deriving the role" never states roleRank's order (member < security_admin < admin)`)
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
