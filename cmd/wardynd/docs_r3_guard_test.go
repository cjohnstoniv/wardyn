// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Doc guards for the round-3 docs fix lane. Each test derives its expectation
// from the thing the prose describes — a migration loop, a route registration,
// a closed const set, a naming function — so the sentence cannot drift away
// from the code without going red. None of them reads a line number.

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// readSrc reads a repo file verbatim (no whitespace folding), for the tests
// below that scan CODE rather than prose.
func readSrc(t *testing.T, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{repoRoot(t)}, rel...)...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(rel...), err)
	}
	return string(b)
}

// mustSay fails when a folded document is missing a claim it has to carry.
func mustSay(t *testing.T, doc, name string, claims ...string) {
	t.Helper()
	for _, want := range claims {
		if !strings.Contains(doc, strings.Join(strings.Fields(want), " ")) {
			t.Errorf("%s no longer states: %q", name, want)
		}
	}
}

// mustNotSay fails when a folded document has drifted back to a retired claim.
func mustNotSay(t *testing.T, doc, name string, claims ...string) {
	t.Helper()
	for _, gone := range claims {
		if strings.Contains(doc, strings.Join(strings.Fields(gone), " ")) {
			t.Errorf("%s is back to the retired claim: %q", name, gone)
		}
	}
}

// TestMigrationFailureIsDocumentedAsHalfApplied (F165) pins the upgrade
// runbook to what the migration loop actually leaves behind.
//
// The runbook told operators that per-migration transactions mean wardynd
// "refuses to boot rather than half-applying". The transaction is real and it
// bounds ONE migration: applyMigration commits the migration together with its
// schema_migrations row, and the loop returns on the first error, so a failure
// at N leaves 0..N-1 committed AND recorded. There are no down migrations, so
// the pre-upgrade dump is the only way back — the opposite of the reassurance
// an operator was reading at the moment they had to choose.
func TestMigrationFailureIsDocumentedAsHalfApplied(t *testing.T) {
	// (1) The premise, read off the loop itself: one transaction per migration,
	// the schema_migrations INSERT inside it, and a return on the first error.
	src := readSrc(t, "internal", "db", "db.go")
	apply := funcBody(t, src, "applyMigration")
	for _, want := range []string{"db.Begin(ctx)", "INSERT INTO schema_migrations", "tx.Commit(ctx)"} {
		if !strings.Contains(apply, want) {
			t.Errorf("applyMigration no longer contains %q — the doc's per-migration-atomicity premise has changed; re-read the runbook before trusting this guard", want)
		}
	}
	loop := funcBody(t, src, "migrateOn")
	if !strings.Contains(loop, "if err := applyMigration(ctx, db, name, string(data)); err != nil {\n\t\t\treturn err") {
		t.Error("migrateOn no longer returns on the first applyMigration error — if the sequence became atomic, the runbook's half-applied warning must be re-widened deliberately")
	}

	// (2) Forward-only: a down path would give the operator a second recovery
	// and the doc would owe them that sentence instead.
	entries, err := os.ReadDir(filepath.Join(repoRoot(t), "internal", "db", "migrations"))
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Name()), "down") {
			t.Errorf("a down migration exists (%s) — the runbook says the dump is the only rollback", e.Name())
		}
	}

	// (3) What the runbook must now say, and what it must never say again.
	doc := readDoc(t, "docs/OPERATIONS.md")
	mustSay(t, doc, "docs/OPERATIONS.md",
		"**Per-migration atomicity bounds ONE migration, not the sequence.**",
		"leaves `0…N-1` **committed and recorded**",
		"**restoring the pre-upgrade dump is the only supported recovery**",
	)
	mustNotSay(t, doc, "docs/OPERATIONS.md",
		"wardynd refuses to boot rather than half-applying",
	)
}

// TestDesktopDocSecretTierMatchesTheRouter (F224) pins the member-tier doc's
// secret claim to where the routes are actually registered.
//
// DESKTOP.md listed "secret writes" among the powers that "stay admin-only",
// in one sentence with three clauses that are still true — so nothing signalled
// that 0.7 moved this one. The write verbs now hang off the member-reachable
// router and the namespace comes from secretOwnerFromRequest.
func TestDesktopDocSecretTierMatchesTheRouter(t *testing.T) {
	routes := readSrc(t, "internal", "api", "routes.go")
	for _, verb := range []string{"Put", "Delete"} {
		if strings.Contains(routes, `operatorOnly.`+verb+`("/secrets/{name}"`) {
			t.Errorf(`%s /secrets/{name} is registered on operatorOnly again — DESKTOP.md now documents it as self-service`, strings.ToUpper(verb))
		}
		if !strings.Contains(routes, `r.`+verb+`("/secrets/{name}"`) {
			t.Errorf(`%s /secrets/{name} is no longer registered on the member-reachable router — re-check DESKTOP.md's secret paragraph`, strings.ToUpper(verb))
		}
	}
	// The namespace split and the two carve-outs the doc names, read off the
	// handlers rather than restated.
	for _, want := range []struct{ file, src string }{
		{"runs_policy.go", "func (s *Server) secretOwnerFromRequest("},
		{"secrets.go", `"?owner= is admin-only"`},
		{"secrets.go", "sinkReservedSecret(name) || name == bedrockAPIKeySecret"},
	} {
		if !strings.Contains(readSrc(t, "internal", "api", want.file), want.src) {
			t.Errorf("internal/api/%s no longer carries %q — DESKTOP.md's secret paragraph names it", want.file, want.src)
		}
	}

	doc := readDoc(t, "docs/DESKTOP.md")
	mustNotSay(t, doc, "docs/DESKTOP.md",
		"policy CRUD, secret writes and `PUT /site-config` all stay admin-only",
	)
	mustSay(t, doc, "docs/DESKTOP.md",
		"`PUT`/`DELETE /secrets/{name}` is self-service for any signed-in human",
		"`secretOwnerFromRequest` returns `\"\"` for an operator",
		"`?owner=<principal>`",
	)
	// Every Bedrock/SigV4 name the write boundary refuses to a member has to be
	// in the doc's list, derived from the constants rather than typed twice.
	for _, name := range bedrockReservedSecretNames(t) {
		if !strings.Contains(doc, "`"+name+"`") {
			t.Errorf("docs/DESKTOP.md's admin-only secret list omits %q, which writableSecretName refuses to a member", name)
		}
	}
}

// bedrockReservedSecretNames is the four-name set a non-operator PUT/DELETE is
// refused, read off the constants the refusal is written against.
func bedrockReservedSecretNames(t *testing.T) []string {
	t.Helper()
	src := readSrc(t, "internal", "api", "runs_bedrock.go")
	re := regexp.MustCompile(`bedrock\w*Secret\s+=\s+"([a-z0-9-]+)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	if len(out) < 4 {
		t.Fatalf("found %d Bedrock secret-name constants (%v) — this guard's matcher needs updating, it is checking almost nothing", len(out), out)
	}
	slices.Sort(out)
	return out
}

// TestAuthzDeniedGovernanceProfileRowNamesEveryTarget (F179) closes the level
// the reason-set guard cannot see.
//
// docs/OPERATIONS.md's denial table declares itself the source of record for
// authz.denied's reason vocabulary, and internal/api's own guard pins the set
// of REASONS. Nothing pinned the CAUSES inside a row: the governance_profile
// row enumerated four, and 0.7 added a fifth emitter — the user-drive door —
// that the row never learned about, so a runs.drive denial had no documented
// cause. The targets are derived from the emit sites, so the next cause that
// lands undocumented fails here.
func TestAuthzDeniedGovernanceProfileRowNamesEveryTarget(t *testing.T) {
	targets := denyMemberFieldTargets(t, "governance_profile")
	if len(targets) < 5 {
		t.Fatalf("found %d denyMemberField targets for governance_profile (%v) — the matcher needs updating, it is checking almost nothing", len(targets), targets)
	}
	row := opsTableRow(t, readDoc(t, "docs/OPERATIONS.md"), "governance_profile")
	for _, target := range targets {
		if !strings.Contains(row, "`"+target+"`") {
			t.Errorf("docs/OPERATIONS.md's `governance_profile` row does not name the target %q, so a denial with that target has no documented cause in the section that calls itself the source of record", target)
		}
	}
}

// denyMemberFieldTargets returns every `target` internal/api denies with the
// given authz.denied reason, read off the emit sites.
func denyMemberFieldTargets(t *testing.T, reason string) []string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "internal", "api")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read internal/api: %v", err)
	}
	re := regexp.MustCompile(`denyMemberField\(w, r, "([a-z_.]+)", "` + regexp.QuoteMeta(reason) + `"`)
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			if !slices.Contains(out, m[1]) {
				out = append(out, m[1])
			}
		}
	}
	slices.Sort(out)
	return out
}

// opsTableRow returns the markdown table row of the folded runbook whose first
// cell is the backticked key, up to the row's end.
func opsTableRow(t *testing.T, doc, key string) string {
	t.Helper()
	start := strings.Index(doc, "| `"+key+"` | ")
	if start < 0 {
		t.Fatalf("no table row keyed `%s` in docs/OPERATIONS.md — the guard's anchor moved", key)
	}
	rest := doc[start+1:]
	if end := strings.Index(rest, " | ⛔ "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// TestManagedObjectNameRationaleMatchesTheNamingFunction (F189) pins the
// runbook's REASONING about managed object names, which the name-token guard
// structurally cannot see.
//
// Every minted name now folds the drive slug in, so a name identifies the
// (drive, home) pair and wardyn.subject is what tells two PEOPLE apart inside
// one home. Two runbook sentences still argued from the older rule — the object
// is named for the home "and nothing else" — and neither contains a
// wardyn-drive-… token, so the repo-wide token guard passed over both. The
// shapes here come from the naming function itself, so if the slug is ever
// dropped again the doc's argument is re-examined rather than silently restored.
func TestManagedObjectNameRationaleMatchesTheNamingFunction(t *testing.T) {
	for backend, shape := range mintedShapes(t) {
		if !strings.Contains(shape, "<drive-slug>") {
			t.Fatalf("%s mints %q, which no longer carries the drive slug — the runbook's naming rationale is written against a name that identifies the (drive, home) pair; re-derive the prose before this guard", backend, shape)
		}
		if !strings.Contains(shape, "<home>") {
			t.Fatalf("%s mints %q, which no longer carries the home — re-derive the runbook's collision argument", backend, shape)
		}
	}
	doc := readDoc(t, "docs/OPERATIONS.md")
	mustNotSay(t, doc, "docs/OPERATIONS.md",
		"a volume name carries only the *home*",
		"names a managed object after the home and nothing else",
	)
	mustSay(t, doc, "docs/OPERATIONS.md",
		"a volume name carries the drive and the *home*",
		"names a managed object after the drive and the home",
	)
}

// TestThreatModelDrivePreviewResidualMatchesTheHandler (F190) pins §4.6's
// published residual to what the preview handler actually runs.
//
// The residual said the preview skips the governance door, the stale-snapshot
// arm and driveMountFor, and that it touches no substrate. Three of those four
// stopped being true when the preview was widened to run the enforcement path's
// own gates in the enforcement path's own order — and §4.6's own earlier
// paragraph already described the arm the residual said was skipped. A threat
// model that overstates a gap is not conservative; it is wrong about its own
// system, in the document a reviewer checks first.
func TestThreatModelDrivePreviewResidualMatchesTheHandler(t *testing.T) {
	resolve := readSrc(t, "internal", "api", "user_drives_resolve.go")
	preview := methodBody(t, resolve, "handlePreviewUserDrive")
	for _, gate := range []string{"drivePreviewDoorIsOpen", "previewResolveUserDrive", "driveIsMountableHere"} {
		if !strings.Contains(preview, gate) {
			t.Errorf("handlePreviewUserDrive no longer runs %s — §4.6 publishes it as something the preview DOES run; re-widen the residual deliberately", gate)
		}
	}
	if strings.Contains(preview, "driveMountFor") {
		t.Error("handlePreviewUserDrive now calls driveMountFor — §4.6 names its narrowing arm as the one thing the preview skips")
	}
	if !strings.Contains(methodBody(t, resolve, "previewResolveUserDrive"), "driveWithUnusableGroups") {
		t.Error("previewResolveUserDrive no longer takes the driveWithUnusableGroups arm — §4.6 publishes the preview as running the identical unusable-groups function the launch does")
	}
	// The substrate half: a share's home is really stat'd, and a MANAGED
	// backend short-circuits before any of that, which is why the Kubernetes
	// half of the residual survives.
	// F269 (round 3) split the bindability check into a pure DECISION,
	// driveShareBindFailure, reached from driveIsMountableHere via driveBindFailureHere, so
	// /me and the preview can ask the same question without paying the writer's
	// metric and WARN. The substrate facts §4.6 publishes now live in the
	// decision; the writer is pinned to route through it, so inspecting the
	// decision is inspecting what the preview runs.
	run := readSrc(t, "internal", "api", "user_drives_run.go")
	if !strings.Contains(methodBody(t, run, "driveIsMountableHere"), "driveBindFailureHere(") ||
		!strings.Contains(methodBody(t, run, "driveBindFailureHere"), "driveShareBindFailure(") {
		t.Error("driveIsMountableHere no longer routes through driveBindFailureHere → driveShareBindFailure — the decision this guard inspects is not the one the preview runs")
	}
	bindable := methodBody(t, run, "driveShareBindFailure")
	if !strings.Contains(bindable, "os.Stat(resolved.ObjectName)") {
		t.Error("driveShareBindFailure no longer stats the person's home — §4.6 says the preview DOES touch the substrate for a share")
	}
	if !strings.Contains(bindable, "resolved.Drive.Backend != types.DriveBackendHostPath") {
		t.Error("driveShareBindFailure no longer short-circuits for a managed backend — §4.6 keeps the Kubernetes half of the residual on exactly that")
	}

	tm := readDoc(t, "threatmodel/THREAT-MODEL.md")
	mustNotSay(t, tm, "threatmodel/THREAT-MODEL.md",
		"the preview skips the door, the stale-snapshot arm and `driveMountFor`, and neither touches the substrate",
	)
	mustSay(t, tm, "threatmodel/THREAT-MODEL.md",
		"the preview now runs the governance door",
		"What it does NOT run is `driveMountFor`'s narrowing arm",
		"nothing here asks the CLUSTER whether a claim can bind",
	)
}

// methodBody returns the source of a method by name, from its `func (recv)`
// line to the closing brace in column 0. funcBody's anchor only matches a
// plain top-level func.
func methodBody(t *testing.T, src, name string) string {
	t.Helper()
	loc := regexp.MustCompile(`(?m)^func \([^)]*\) ` + regexp.QuoteMeta(name) + `\(`).FindStringIndex(src)
	if loc == nil {
		t.Fatalf("method %s not found — the guard's anchor moved, so it is asserting nothing", name)
	}
	rest := src[loc[0]:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// TestMembersDocStatesTheThreeKeyDriveContract (F169) pins the member-facing
// doc to the shape GET /me actually returns.
//
// MEMBERS.md calls /me "the ground truth" and described the two-state contract
// the 0.7 fix superseded: user_drive null == nothing allocated. The fix exists
// because null meant four different things, so the doc taught an external
// consumer the exact wrong inference the fix was written to prevent — read
// null, tell the member to ask an admin for an allocation, which is the wrong
// remedy in four of the five states. The token set is closed and lives in one
// const block, so the doc is checked against that block rather than a list
// typed twice.
func TestMembersDocStatesTheThreeKeyDriveContract(t *testing.T) {
	src := readSrc(t, "internal", "api", "user_drives_resolve.go")
	tokens := driveUnavailableTokens(t, src)
	if len(tokens) < 4 {
		t.Fatalf("found %d driveUnavailable* tokens (%v) — the matcher needs updating, it is checking almost nothing", len(tokens), tokens)
	}
	// The key is always written, so "" is a real answer and a MISSING key means
	// an older daemon. The doc's "always present" claim rests on that.
	me := methodBody(t, readSrc(t, "internal", "api", "me.go"), "handleMe")
	for _, key := range []string{`body["user_drive"]`, `body["user_drive_denied_by_profile"]`, `body["user_drive_unavailable"]`} {
		if !strings.Contains(me, key) {
			t.Errorf("GET /me no longer writes %s — MEMBERS.md documents a three-key contract", key)
		}
	}

	doc := readDoc(t, "docs/MEMBERS.md")
	mustNotSay(t, doc, "docs/MEMBERS.md",
		"`GET /me` carries `user_drive` (`null` when none is allocated to you) and is the ground truth for what you have.",
	)
	mustSay(t, doc, "docs/MEMBERS.md",
		"`user_drive_denied_by_profile`",
		"`user_drive_unavailable`",
		"no longer means",
	)
	for _, tok := range tokens {
		if !strings.Contains(doc, "`"+tok+"`") {
			t.Errorf("docs/MEMBERS.md never names the `user_drive_unavailable` value %q, so a member or an external consumer cannot tell that state from 'you have no allocation'", tok)
		}
	}
}

// driveUnavailableTokens reads the closed user_drive_unavailable vocabulary off
// its const block — the same single place the resolver and the launch door
// share.
func driveUnavailableTokens(t *testing.T, src string) []string {
	t.Helper()
	re := regexp.MustCompile(`driveUnavailable[A-Za-z]+\s+=\s+"([a-z_]+)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	slices.Sort(out)
	return out
}

// The canonical page that actually carries the nested-membership sentence, and
// the page an earlier pass attributed it to by mistake.
const (
	entraGroupClaimsPage = "https://learn.microsoft.com/en-us/entra/identity/hybrid/connect/how-to-connect-fed-group-claims"
	entraOptionalClaims  = "identity-platform/optional-claims"
	nestedMembershipRule = "nested groups are not included and the user must be a direct member of the group assigned to the application"
)

// TestGroupClaimCaveatCitesThePageThatCarriesIt (F103) pins WHERE the runbook's
// load-bearing Entra quote comes from.
//
// The overage remedy carries a verbatim quotation, and the whole point of the
// paragraph is that an operator will re-key every group-subject grant before
// changing a claim configuration on the strength of it. An earlier pass
// attributed the sentence to the optional-claims page, which recommends the
// setting and never mentions nested membership — so an operator who followed
// the link to check found a page that reads as if the caveat were overstated.
// The existing caveat guard pins that the caveat is PRESENT; nothing pinned
// where it came from, which is why the wrong URL survived a round.
func TestGroupClaimCaveatCitesThePageThatCarriesIt(t *testing.T) {
	doc := readDoc(t, "docs/OPERATIONS.md")
	i := strings.Index(doc, nestedMembershipRule)
	if i < 0 {
		t.Fatalf("docs/OPERATIONS.md no longer quotes the nested-membership rule — the caveat guard owns its presence, but this guard is now asserting nothing")
	}
	window := doc[i:min(i+600, len(doc))]
	if !strings.Contains(window, entraGroupClaimsPage) {
		t.Errorf("the nested-membership quote is not attributed to %s, the page that carries it — an operator checking the caveat before re-keying their grants lands somewhere that does not state it", entraGroupClaimsPage)
	}
	if strings.Contains(window, entraOptionalClaims) {
		t.Errorf("the nested-membership quote is attributed to %s again; that page recommends the option and never states the exclusion", entraOptionalClaims)
	}
}

// TestDataFlowAuditSinkRowCarriesTheOutageQualifier (F048, round-2 residue)
// extends the audit-sink guard's AUDIT-ACTIONS.md-style assertion to the third
// file that makes the same off-box promise.
//
// The round-1 fix qualified the promise in the runbook, the audit-actions
// reference and the threat model, and the standing guard pins all three. The
// data-flow page repeats it verbatim — and cites the runbook as its authority,
// so after that fix it contradicted the very file it points at. It carried no
// wording the standing guard looks for, so it passed while it was wrong.
func TestDataFlowAuditSinkRowCarriesTheOutageQualifier(t *testing.T) {
	// The premise this whole family of claims rests on, re-asserted here so the
	// data-flow row cannot be "fixed" against a rule that has since changed:
	// the hashes come from the store write, and the drain has no fanout.
	adapters := readSrc(t, "cmd", "wardynd", "adapters.go")
	if !strings.Contains(adapters, "A failed store write leaves both empty and the event still fans out.") {
		t.Error("fanoutRecorder no longer records that a failed store write still fans out — re-read the sink claims in all four documents before trusting this guard")
	}

	doc := readDoc(t, "docs/DATA-FLOW.md")
	mustNotSay(t, doc, "docs/DATA-FLOW.md",
		"Each event carries its hash-chain `prev_hash`/`row_hash` (migration `0047`), so what the SIEM holds off-box is a head hash Wardyn cannot later disown",
	)
	mustSay(t, doc, "docs/DATA-FLOW.md",
		"whose Postgres write succeeded",
		"is not re-streamed when the spool drains",
	)
}
