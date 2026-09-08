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
	"strconv"
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

// ruleSourceConst finds every `ruleSource<Name> = "<value>"` constant declared
// across internal/egress/proxy (excluding tests) — the closed vocabulary
// AUDIT-ACTIONS.md's rule_source table has to enumerate.
var ruleSourceConst = regexp.MustCompile(`(?m)^\truleSource[A-Za-z]*\s+= "([a-z0-9:-]+)"`)

// inlineRuleSourceLiteral finds evaluate()'s OWN decisionLog(...) call sites
// in internal/egress/proxy/proxy.go whose rule_source argument is an inline
// string literal rather than a named ruleSource* constant — the second,
// previously-undocumented family F093's B1 blocking item added (the
// "evaluator's own inline sources" table).
var inlineRuleSourceLiteral = regexp.MustCompile(`decisionLog\([^,]+,\s*egress\.[A-Za-z]+,\s*"([a-z:-]+)"\)`)

// TestAuditActionsDocEnumeratesEveryRuleSource (F093) pins the new
// egress.*'s rule_source values table to the actual closed set of
// ruleSource* constants — a new constant with no doc row fails here instead
// of silently drifting, the same guard the finding's remediation asked for.
// It also pins the second family (F093 B1): the inline literals evaluate()
// itself writes at its decisionLog call sites in proxy.go, plus the
// "approval:"+approvalID concatenation form.
func TestAuditActionsDocEnumeratesEveryRuleSource(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "internal", "egress", "proxy")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read internal/egress/proxy: %v", err)
	}
	var values []string
	var proxySrc string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range ruleSourceConst.FindAllStringSubmatch(string(b), -1) {
			if !slices.Contains(values, m[1]) {
				values = append(values, m[1])
			}
		}
		if name == "proxy.go" {
			proxySrc = string(b)
		}
	}
	if len(values) < 15 {
		t.Fatalf("found %d ruleSource* constants (%v) — the matcher needs updating, it is checking almost nothing", len(values), values)
	}
	if proxySrc == "" {
		t.Fatal("internal/egress/proxy/proxy.go not found — the inline-literal scan needs updating")
	}
	for _, m := range inlineRuleSourceLiteral.FindAllStringSubmatch(proxySrc, -1) {
		if !slices.Contains(values, m[1]) {
			values = append(values, m[1])
		}
	}
	if !strings.Contains(proxySrc, `"approval:"+`) {
		t.Fatal(`proxy.go no longer builds a rule_source via "approval:"+approvalID.String() — re-derive the approval:<approval-id> doc row before trusting this guard`)
	}
	values = append(values, "approval:<approval-id>")

	// F093 B1 premise: the branch-ns-off row's Meaning cell claims both the
	// deployment-wide env switch and the per-run git_push_any_branch policy
	// field feed the SAME branch of this condition — re-derive the row before
	// trusting this guard if the branch itself changes shape.
	if !strings.Contains(readSrc(t, "internal", "egress", "proxy", "git_broker.go"),
		"if isPush && (!BranchNSEnforced() || p.policy.GitPushAnyBranch()) {") {
		t.Fatal("internal/egress/proxy/git_broker.go no longer branches on " +
			"\"if isPush && (!BranchNSEnforced() || p.policy.GitPushAnyBranch())\" — " +
			"re-derive the brokered:git:branch-ns-off row's two-switches claim before trusting this guard")
	}

	doc := readDoc(t, "docs/AUDIT-ACTIONS.md")
	for _, v := range values {
		if !strings.Contains(doc, "`"+v+"`") {
			t.Errorf("docs/AUDIT-ACTIONS.md's rule_source tables never name %q, a rule_source value emitted by internal/egress/proxy", v)
		}
	}
	mustSay(t, doc, "docs/AUDIT-ACTIONS.md",
		"the only decisions carved out of it",
		"one ALLOW that REPLACES the ordinary",
		"never which of the six causes",
		"both switches take the same branch",
		"git_push_any_branch",
	)
}

// TestTestGapsDocMatchesTheCoverpkgBlindSpot (F092) pins docs/TEST-GAPS.md's
// "Untested" bucket claim to test-report.sh's actual go test invocation.
//
// The union profile the inventory is built from runs `go test` with no
// -coverpkg, so Go instruments only each package's own code for its own
// tests: a func exercised solely through a DIFFERENT package's tests reads
// 0.0% and lands in "Untested" despite being covered. The doc (and its
// generator template in scripts/test-gaps.sh) called that bucket "genuinely
// no test reaches these" — stronger than the measurement supports.
func TestTestGapsDocMatchesTheCoverpkgBlindSpot(t *testing.T) {
	if strings.Contains(readSrc(t, "scripts", "test-report.sh"), "-coverpkg") {
		t.Fatal("test-report.sh now passes -coverpkg — the blind spot this guard pins may be closed; re-derive the doc claim before trusting this skip")
	}
	gen := readSrc(t, "scripts", "test-gaps.sh")
	for _, want := range []string{"no same-package test", "-coverpkg"} {
		if !strings.Contains(gen, want) {
			t.Errorf("scripts/test-gaps.sh no longer mentions %q", want)
		}
	}
	mustNotSay(t, gen, "scripts/test-gaps.sh (header comment)",
		"Untested       — genuinely no test reaches it",
	)

	doc := readDoc(t, "docs/TEST-GAPS.md")
	mustNotSay(t, doc, "docs/TEST-GAPS.md",
		"## Untested — genuinely no test reaches these",
	)
	mustSay(t, doc, "docs/TEST-GAPS.md",
		"no same-package test",
		"WITHOUT `-coverpkg`",
		"## Untested — no test in the SAME PACKAGE reaches these",
	)
}

// TestAuditActionsDocNamesTheDroppedDecisionSummary (F067) pins the
// egress.* row to the synthetic egress.decisions.dropped marker
// droppedSummaryLog posts on buffer overflow — a Deny with an empty target
// that lands as an ordinary egress.deny row.
func TestAuditActionsDocNamesTheDroppedDecisionSummary(t *testing.T) {
	src := readSrc(t, "internal", "egress", "proxy", "decisions.go")
	if !strings.Contains(src, `RuleSource: fmt.Sprintf("egress.decisions.dropped:%d", n)`) {
		t.Fatal("droppedSummaryLog no longer emits the egress.decisions.dropped:<n> marker — re-derive the doc row before trusting this guard")
	}
	doc := readDoc(t, "docs/AUDIT-ACTIONS.md")
	mustSay(t, doc, "docs/AUDIT-ACTIONS.md",
		"egress.decisions.dropped:<n>",
		"droppedSummaryLog",
		"not a policy denial of an actual request",
	)
}

// TestLiteralIPRedirectDocsNameThePortScope (F053, re-derived for F106) pins
// OPERATIONS.md and THREAT-MODEL.md's "scoped to that address" claim to what
// substituteArtifactEgress now actually writes: a PORT-QUALIFIED entry, so the
// trust it grants is to `to:port` and not to that address on any port.
//
// The claim it originally pinned was the opposite one — the port was STRIPPED,
// and the docs had to say so. F106 fixed the code; the guard is re-derived
// against the merged tree rather than skipped, so the docs can never drift back
// to describing either shape while the other one ships.
func TestLiteralIPRedirectDocsNameThePortScope(t *testing.T) {
	src := readSrc(t, "internal", "api", "workspace_egress.go")
	if !strings.Contains(src, "entry := net.JoinHostPort(to, strconv.Itoa(redirectPort(r.To)))") {
		t.Fatal("substituteArtifactEgress no longer writes a net.JoinHostPort-qualified entry — re-derive the doc claim before trusting this guard")
	}
	policy := readSrc(t, "internal", "egress", "proxy", "policy.go")
	if !strings.Contains(policy, "or a port-qualified one") {
		t.Fatal("Policy.AllowsLiteralIP's doc comment no longer describes a port-qualified entry as an alternative — re-derive the doc claim before trusting this guard")
	}

	ops := readDoc(t, "docs/OPERATIONS.md")
	mustSay(t, ops, "docs/OPERATIONS.md",
		"PORT-QUALIFIED", "net.JoinHostPort(hostrules.HostOf(r.To), redirectPort(r.To))",
		"A bare address would have matched\nEVERY port instead",
	)
	mustNotSay(t, ops, "docs/OPERATIONS.md",
		"strips a `:port`",
	)
	tm := readDoc(t, "threatmodel/THREAT-MODEL.md")
	mustSay(t, tm, "threatmodel/THREAT-MODEL.md",
		"only on the ONE PORT the redirect's", "`net.JoinHostPort`-qualified",
	)
	mustNotSay(t, tm, "threatmodel/THREAT-MODEL.md",
		"the port is\ndropped", "that\naddress on ANY port",
	)
}

// TestOperationsDocNamesTheUnscopedNetworkRedirectDeny (F052) pins the
// "Egress redirects: two tiers" section to appendNetworkRedirectDenials'
// actual scope — or rather its absence.
//
// The substitution and the token plan are both scoped to a run that reaches
// one of the redirect's public hosts; appendNetworkRedirectDenials takes no
// such scope input at all (just the running deny-list and the site config),
// so it denies a network-only row's From host in every run regardless. The
// two-tier table and prose described only the two scoped effects.
func TestOperationsDocNamesTheUnscopedNetworkRedirectDeny(t *testing.T) {
	src := readSrc(t, "internal", "api", "workspace_egress.go")
	if !strings.Contains(src, "func appendNetworkRedirectDenials(denied []string, sc types.SiteConfig) []string {") {
		t.Fatal("appendNetworkRedirectDenials's signature changed — it may now take a run-scope input, which would close this residual; re-derive the doc claim before trusting this guard")
	}
	dispatch := readSrc(t, "internal", "api", "runs_dispatch.go")
	if !strings.Contains(dispatch, "policy.DeniedDomains = appendNetworkRedirectDenials(policy.DeniedDomains, siteCfg)") {
		t.Fatal("runs_dispatch.go no longer calls appendNetworkRedirectDenials this way — re-derive the doc claim before trusting this guard")
	}
	if strings.Contains(src, "// A run that names none of a redirect's public hosts is left\n// entirely untouched by it.\n") {
		t.Error("workspace_egress.go's SCOPE comment still claims an out-of-scope run is left entirely untouched, unqualified — appendNetworkRedirectDenials' unconditional deny contradicts it")
	}

	doc := readDoc(t, "docs/OPERATIONS.md")
	mustSay(t, doc, "docs/OPERATIONS.md",
		"it denies its own `from` host outright, in EVERY run",
		"appendNetworkRedirectDenials",
		"Deny beats `allow_all_egress`",
	)
}

// TestAuditActionsDocCredentialRevokeRowMatchesRevokeRun (F042, re-derived at
// the R3 credentials merge) pins the credential.revoke row to what
// Broker.RevokeRun actually emits.
//
// The credentials lane (F096/F122/F013) moved RevokeRun to internal/broker/revoke.go,
// replaced the approvals-only MintedJTIs bulk read with MintedCredentials
// (mintedCredentialsSQL: the approvals burn UNION the run's successful
// credential.mint audit rows), and replaced the one GitHub-shaped note with the
// per-kind revokeNote. The row was re-derived with it; this guard is re-derived
// against the merged tree rather than skipped.
func TestAuditActionsDocCredentialRevokeRowMatchesRevokeRun(t *testing.T) {
	src := readSrc(t, "internal", "broker", "revoke.go")
	fn := funcBody(t, src, "(b *Broker) RevokeRun")
	if !strings.Contains(fn, `"jti":  mc.JTI`) || !strings.Contains(fn, `"note": revokeNote(mc.Kind)`) {
		t.Fatal("Broker.RevokeRun no longer writes jti+revokeNote(kind) — re-derive the doc row before trusting this guard")
	}
	if !strings.Contains(fn, "b.db.MintedCredentials(ctx, runID)") {
		t.Fatal("Broker.RevokeRun no longer enumerates via MintedCredentials — re-derive the doc row before trusting this guard")
	}
	if !strings.Contains(fn, `d["kind"] = mc.Kind`) {
		t.Fatal("Broker.RevokeRun no longer stamps kind conditionally — the doc row's \"absent when the grant row behind the mint is gone\" claim depends on it")
	}
	// The honesty limitation the row publishes: GitHub's endpoint is REAL but
	// needs the token itself, and RevokeRun holds only the jti.
	if !strings.Contains(src, "DELETE /installation/token") || !strings.Contains(src, "RevokeRun has only the jti") {
		t.Fatal("RevokeRun's doc comment no longer states the per-token revocation limitation — re-derive the doc row before trusting this guard")
	}
	note := funcBody(t, src, "revokeNote")
	for _, want := range []string{
		"wardyn does not call GitHub's DELETE /installation/token",
		"operator must rotate this secret at the forge",
		"api_key values stay proxy-side",
	} {
		if !strings.Contains(note, want) {
			t.Fatalf("revokeNote no longer says %q — re-derive the doc row before trusting this guard", want)
		}
	}

	// The cascade's approvals half must still select on kind='credential' with no
	// grant-kind narrowing — every brokered grant kind (github_token, api_key,
	// git_pat, ssh_key) inserts its approval with kind='credential' (sql.go), so
	// the doc's "every kind alike" claim depends on it never narrowing to one
	// grant kind; the audit half must stay keyed on a SUCCESSFUL credential.mint.
	pgxSrc := readSrc(t, "internal", "broker", "pgx.go")
	if !strings.Contains(pgxSrc, "a.kind = 'credential' AND a.minted_jti <> ''") {
		t.Fatal("mintedCredentialsSQL's approvals half no longer selects kind = 'credential' AND minted_jti <> '' — re-derive the doc row before trusting this guard")
	}
	if !strings.Contains(pgxSrc, "e.action = 'credential.mint' AND e.outcome = 'success'") {
		t.Fatal("mintedCredentialsSQL's audit half no longer selects successful credential.mint rows — re-derive the doc row before trusting this guard")
	}

	doc := readDoc(t, "docs/AUDIT-ACTIONS.md")
	mustNotSay(t, doc, "docs/AUDIT-ACTIONS.md",
		"`credential.revoke` | A minted credential is revoked (kill-switch, run stop) | —",
	)
	mustNotSay(t, doc, "docs/AUDIT-ACTIONS.md",
		"Every minted GitHub App installation `jti`",
	)
	// The row must not resurrect the two claims the merged code falsifies: the
	// approvals-only enumeration, and "GitHub has no per-token revocation API".
	mustNotSay(t, doc, "docs/AUDIT-ACTIONS.md",
		"`MintedJTIs`",
		"GitHub has no per-token revocation API",
	)
	mustSay(t, doc, "docs/AUDIT-ACTIONS.md",
		"`credential.revoke`", "`jti`", "`note`", "`kind`",
		"the cascade enumerates the run's successful `credential.mint` rows UNION the approvals whose `minted_jti` was burnt",
		"`mintedCredentialsSQL`",
		"`revokeNote`",
		"wardyn does not call GitHub's `DELETE /installation/token`",
		"`RevokeRun` holds only the `jti`",
		"`api_key`", "`ssh_key`",
		"TTL expiry",
	)
}

// TestAuditActionsDocNamesTheErrorScanAction (F041) pins AUDIT-ACTIONS.md's
// llm.scan.* suffix enumeration, and egress.ScanSummary.Action's own comment,
// to the literal `scanSummaryFrom` actually assigns on a scanner error or an
// unparsed body.
func TestAuditActionsDocNamesTheErrorScanAction(t *testing.T) {
	body := funcBody(t, readSrc(t, "internal", "egress", "proxy", "llm_routes.go"), "scanSummaryFrom")
	if !strings.Contains(body, `s.Action = "error"`) {
		t.Fatalf(`scanSummaryFrom no longer assigns s.Action = "error" — re-derive AUDIT-ACTIONS.md's suffix list before trusting this guard`)
	}

	doc := readDoc(t, "docs/AUDIT-ACTIONS.md")
	mustSay(t, doc, "docs/AUDIT-ACTIONS.md", "`llm.scan.error`")

	comment := readSrc(t, "internal", "egress", "egress.go")
	if i := strings.Index(comment, `Action     string        `+"`json:\"action\"`"); i < 0 {
		t.Fatal("ScanSummary.Action field declaration not found — the guard's anchor moved")
	} else {
		line := comment[i:]
		if end := strings.Index(line, "\n"); end >= 0 {
			line = line[:end]
		}
		if !strings.Contains(line, `"error"`) {
			t.Errorf("egress.ScanSummary.Action's enumeration comment omits \"error\": %q", line)
		}
	}
}

// llmScanAuditKey pulls one quoted map[string]any key out of
// recordLLMScanAudit's body — deliberately narrow to that literal's shape
// (lowercase, underscored) so it does not also match the doc strings or the
// audit action literal sitting in the same function.
var llmScanAuditKey = regexp.MustCompile(`"([a-z][a-z_]*)":`)

// maxAuditFindingsConst pins the llm.scan.* doc row's truncation-bound prose
// to the const it names, rather than a hand-typed number that can drift.
var maxAuditFindingsConst = regexp.MustCompile(`(?m)^const maxAuditFindings = (\d+)`)

// TestAuditActionsDocLLMScanDataFieldsMatchTheEmitSite (B1-AUDITACTIONS-LLMSCAN-ROW-STALE)
// pins AUDIT-ACTIONS.md's llm.scan.* row's Data column to every key
// recordLLMScanAudit actually marshals onto the wire.
//
// F075 added findings_capped, findings_past_cap and findings_reported to the
// emit site's map[string]any{...} literal, and repointed finding_count away
// from len(sc.Findings) to the pre-cap total — a SIEM rule author reading only
// the doc would never learn either fact, which is exactly the case an auditor
// hits when a scan was truncated. No prior guard read the emit site's key set
// at all, which is why every other gate stayed green with the row wrong.
func TestAuditActionsDocLLMScanDataFieldsMatchTheEmitSite(t *testing.T) {
	body := methodBody(t, readSrc(t, "internal", "api", "internal.go"), "recordLLMScanAudit")
	mapStart := strings.Index(body, "json.Marshal(map[string]any{")
	if mapStart < 0 {
		t.Fatalf("recordLLMScanAudit's map[string]any literal not found — the guard's anchor moved")
	}
	mapEnd := strings.Index(body[mapStart:], "})")
	if mapEnd < 0 {
		t.Fatalf("recordLLMScanAudit's map[string]any literal has no closing '})' — the guard's anchor moved")
	}
	mapLiteral := body[mapStart : mapStart+mapEnd]

	matches := llmScanAuditKey.FindAllStringSubmatch(mapLiteral, -1)
	if len(matches) == 0 {
		t.Fatalf("recordLLMScanAudit's map[string]any literal has no quoted keys — the guard's anchor moved")
	}
	seen := map[string]bool{}
	var keys []string
	for _, m := range matches {
		if k := m[1]; !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}

	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "docs", "AUDIT-ACTIONS.md"))
	if err != nil {
		t.Fatalf("read docs/AUDIT-ACTIONS.md: %v", err)
	}
	var dataCell, proseCell string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "| `llm.scan.*`") {
			cells := strings.Split(line, "|")
			if len(cells) < 4 {
				t.Fatalf("llm.scan.* row has fewer cells than expected: %q", line)
			}
			proseCell = cells[2]
			dataCell = cells[3]
			break
		}
	}
	if dataCell == "" {
		t.Fatalf("docs/AUDIT-ACTIONS.md's llm.scan.* row not found — the guard's anchor moved")
	}
	for _, k := range keys {
		if !strings.Contains(dataCell, "`"+k+"`") {
			t.Errorf("docs/AUDIT-ACTIONS.md's llm.scan.* row Data column is missing %q, which recordLLMScanAudit marshals onto the wire: %q", k, strings.TrimSpace(dataCell))
		}
	}

	// B1/B2-LLMSCAN-FINDINGS-REPORTED-NOT-THE-ROWS-ON-THE-WIRE pin the row's
	// PROSE (not just its Data-column key list) to two code facts that a
	// truncated-scan reader relies on: (a) the embedded `findings` array is
	// truncated to maxAuditFindings records, a number that must move with the
	// const rather than being hand-typed and drifting, and (b) `finding_count`
	// exceeding `findings_reported` is a one-way implication of
	// `findings_capped`, not an iff — block mode's severity keep-backs can
	// leave a capped scan's two counts equal.
	internalSrc := readSrc(t, "internal", "api", "internal.go")
	constMatch := maxAuditFindingsConst.FindStringSubmatch(internalSrc)
	if constMatch == nil {
		t.Fatalf("const maxAuditFindings = <N> not found in internal/api/internal.go — the guard's anchor moved")
	}
	maxFindings, err := strconv.Atoi(constMatch[1])
	if err != nil {
		t.Fatalf("parse maxAuditFindings value %q: %v", constMatch[1], err)
	}
	if !strings.Contains(proseCell, strconv.Itoa(maxFindings)) {
		t.Errorf("docs/AUDIT-ACTIONS.md's llm.scan.* row prose does not name the maxAuditFindings bound (%d) that recordLLMScanAudit truncates the embedded `findings` array to: %q", maxFindings, strings.TrimSpace(proseCell))
	}
	if !strings.Contains(proseCell, "ONLY when `findings_capped`") {
		t.Errorf("docs/AUDIT-ACTIONS.md's llm.scan.* row prose must state the finding_count-exceeds-findings_reported relation as a ONE-WAY implication of findings_capped (block mode's keep-backs make the converse false), not an \"exactly when\" iff: %q", strings.TrimSpace(proseCell))
	}
	if strings.Contains(proseCell, "exactly when `findings_capped`") {
		t.Errorf("docs/AUDIT-ACTIONS.md's llm.scan.* row prose still claims finding_count exceeds findings_reported \"exactly when\" findings_capped is true — that is a false iff, reproduced false by block mode's severity keep-backs: %q", strings.TrimSpace(proseCell))
	}
}

// TestArchitectureDocGitPATProxyInjectionClaimMatchesTheBroker (F040) pins
// ARCHITECTURE.md's git-egress prose to the PAT broker's default-on posture.
//
// The section's table row already named the PAT broker, but the paragraph right
// below it still said "git_pat and ssh_key cannot be proxy-injected at all" —
// contradicting the row above it in the same section. Only ssh_key has no
// credential-helper seam; git_pat is proxy-injected by its own broker whenever
// WARDYN_GIT_PAT_BROKER is not turned off.
func TestArchitectureDocGitPATProxyInjectionClaimMatchesTheBroker(t *testing.T) {
	boot := readSrc(t, "cmd", "wardynd", "boot_flags.go")
	if !strings.Contains(boot, `flagEnv("git-pat-broker", "WARDYN_GIT_PAT_BROKER", "on"`) {
		t.Fatalf("WARDYN_GIT_PAT_BROKER's default moved off boot_flags.go's known declaration — re-derive the doc claim before trusting this guard")
	}
	arch := readDoc(t, "ARCHITECTURE.md")
	mustNotSay(t, arch, "ARCHITECTURE.md",
		"Conversely `git_pat` and `ssh_key` cannot be proxy-injected at all",
	)
	mustNotSay(t, arch, "ARCHITECTURE.md",
		"### Git egress: two mechanisms",
		"Git has TWO credential lanes",
	)
	mustSay(t, arch, "ARCHITECTURE.md",
		"`git_pat` CAN be proxy-injected",
		"`ssh_key` cannot be proxy-injected at all",
		"WARDYN_GIT_PAT_BROKER=on",
		"three credential lanes",
	)
}

// TestThreatModelDocLiteralIPBoundNamesNonCanonicalResidual (F114, RE-DERIVED
// in the adversarial fix-up round) pins the upstream corp-proxy residual's
// bound (3) to what the step-0 guard actually parses.
//
// The threat model said a literal private/loopback/link-local/metadata IP "is
// still denied at the literal-IP guard". Before R3's fix wave that promise held
// only for spellings net.ParseIP accepts; the POSIX inet_aton forms (127.1,
// 0x7f000001, 2130706433, 0251.0376.0.1) proceeded as ordinary hostnames and,
// on the corp-upstream lane, reached the corp proxy verbatim. F105 closed that,
// and the adversarial round found the SAME assumption still open on a second
// axis — a zone-suffixed IPv6 literal (fe80::1%eth0, and the RFC 6874
// authority spelling fe80::1%25eth0), which net.ParseIP refuses, netip.ParseAddr
// parses and net.Dial dials: the canonical fe80::1 denied at step 0 while the
// zoned spelling went to policy and could raise a first-use approval for a
// link-local address, which literalIPGuard's own invariant 3 says must never be
// raisable.
//
// Three things are pinned, because the doc asserts all three:
//   - the COVERAGE: step 0 and egressTarget's upstream branch both run the
//     gap-filler, and it covers both axes;
//   - the PAIRINGS: the doc's examples must name the address each spelling
//     ACTUALLY resolves to (0251.0376.0.1 is 169.254.0.1, link-local — not
//     127.0.0.1, which the merged text claimed and which glibc, the repo's own
//     ipguard table and the guard itself all contradict);
//   - the QUALIFIER on bound (3), so the unhedged "is still denied at the
//     literal-IP guard" promise cannot return unnoticed: the guard covers the
//     spellings it PARSES, and the residual it does not cover is stated.
func TestThreatModelDocLiteralIPBoundNamesNonCanonicalResidual(t *testing.T) {
	// (1) The premise: step 0 is literalIPGuard and its non-canonical arm covers
	// both axes.
	eval := methodBody(t, readSrc(t, "internal", "egress", "proxy", "proxy.go"), "evaluate")
	if !strings.Contains(eval, "literalIPGuard(") {
		t.Fatalf("evaluate no longer routes step 0 through literalIPGuard — re-derive the doc's literal-IP bound before trusting this guard")
	}
	// The THIRD embedded-v4 shape the doc now claims is CLOSED rather than
	// residual: ::127.0.0.1 / ::169.254.169.254 (RFC 4291 §2.5.5.1). net.ParseIP
	// PARSES those, so the gap-filler is not involved — isBlockedIP's own arm is,
	// and this assertion is what makes the doc and that arm fail together.
	// (The behaviour itself is executed in the proxy package:
	// TestBlockedRangesMatchPreExtractionLists and
	// TestNonCanonicalLiteralIPIsDeniedLikeItsCanonicalSpelling.)
	blockedIP := funcBody(t, readSrc(t, "internal", "egress", "proxy", "policy.go"), "isBlockedIP")
	if !strings.Contains(blockedIP, "v4CompatibleEmbeddedV4(") {
		t.Fatalf("isBlockedIP no longer re-runs the embedded v4 of an IPv4-compatible ::/96 address — " +
			"THREAT-MODEL.md §5.1 claims ::127.0.0.1 is denied at step 0 AND in VetHost, and it is that " +
			"arm that makes both true; re-derive the doc before deleting it")
	}
	lig := readSrc(t, "internal", "egress", "proxy", "literal_ip_guard.go")
	for _, want := range []string{
		"func nonCanonicalLiteralIP(", "nonCanonicalLiteralIP(host)",
		"func nonCanonicalIPv4(", "func zonedIPv6Literal(",
	} {
		if !strings.Contains(lig, want) {
			t.Fatalf("literal_ip_guard.go no longer contains %q — the non-canonical coverage the doc claims has changed shape", want)
		}
	}

	// (2) The corp-upstream branch re-runs the block check on the same reading
	// before it forwards the name.
	target := methodBody(t, readSrc(t, "internal", "egress", "proxy", "egress_target.go"), "egressTarget")
	if !strings.Contains(target, "nonCanonicalLiteralIP(host)") {
		t.Fatalf("egressTarget's upstream branch no longer re-checks non-canonical literals — re-derive the doc's bound before trusting this guard")
	}

	// (3) The doc states the coverage, the correct PAIRINGS, the deny-only
	// property, and bound (3)'s qualifier.
	doc := readDoc(t, "threatmodel/THREAT-MODEL.md")
	mustSay(t, doc, "threatmodel/THREAT-MODEL.md",
		"covers the NON-CANONICAL spellings too",
		"`127.1`",
		"`0x7f000001` and `2130706433` as `127.0.0.1`",
		"`0251.0376.0.1` as\n  `169.254.0.1`", // the pairing the merged text got wrong
		"fe80::1%eth0",
		"fe80::1%25eth0",
		"`nonCanonicalLiteralIP`",
		"Deny only: a spelling the",
		// Bound (3) is qualified, and the residual is named rather than implied.
		"**in every literal spelling that guard parses**",
		"**Residual, stated rather than hedged:**",
		"::127.0.0.1",
		// The IPv4-compatible form is stated as CLOSED, by the arm that closes it.
		"`v4CompatibleEmbeddedV4`",
		"step 0 AND in `VetHost`",
		// ...and what is genuinely left over is named, not implied.
		"NETWORK-SPECIFIC RFC 6052 NAT64",
	)
	mustNotSay(t, doc, "threatmodel/THREAT-MODEL.md",
		"in Go's `net.ParseIP` syntax",
		// The false pairing, in the exact shape the merge shipped it.
		"`2130706433` and `0251.0376.0.1` as `127.0.0.1`",
		// The false residual the adversarial round found: isBlockedIP admitted
		// ::127.0.0.1 at step 0 AND in VetHost (vetHostLift's literal fast path
		// calls the same predicate), so "VetHost still binds it" was never true.
		"so it is not denied at step 0",
		"(`VetHost`, step 4) still binds it",
	)
	// §4.2's FIRST statement of the same bound must carry the same qualifier and
	// cite the file the guard actually lives in (it moved out of proxy.go).
	s42 := doc[strings.Index(doc, "### 4.2 The unconditional IP guard"):]
	if i := strings.Index(s42, "\n### "); i > 0 {
		s42 = s42[:i]
	}
	for _, want := range []string{
		"internal/egress/proxy/literal_ip_guard.go",
		"in every spelling that guard parses",
	} {
		if !strings.Contains(s42, want) {
			t.Fatalf("THREAT-MODEL.md §4.2's opening statement of the literal-IP bound is missing %q — "+
				"the section bound (3) forwards the reader to must not restate the promise unqualified, "+
				"nor cite proxy.go for a guard that lives in literal_ip_guard.go", want)
		}
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

// TestAuditActionsRuleSourceRowsCiteEveryLiveEmitSite (F114 item 4, and the
// adversarial fix-up's own correction of the same shape) pins the two
// rule_source rows whose citations the R3 wave moved.
//
// TestAuditActionsDocCitationsAreLive cannot see this class. It parses a
// backtick span as a CITATION only when the span carries a path, so the
// "same file, second line" shorthand these rows used — a `proxy.go` line
// number followed by a bare `:N` sibling — is read as a bare ANCHOR and never
// resolved against anything. Both were wrong: the step-0 private-IP guard moved out of
// proxy.go entirely (literal_ip_guard.go), and `builtin:dial-failed` is emitted
// from four places, none of them the duplicated line. A citation nobody checks
// is how the doc came to name a file the guard no longer lives in.
//
// So the rule here is the one the live-citation guard can then enforce: in
// these rows every site is spelled out in full, and no bare `:N` shorthand is
// left for a reader (or a guard) to resolve by guesswork.
func TestAuditActionsRuleSourceRowsCiteEveryLiveEmitSite(t *testing.T) {
	doc := readRepoFile(t, "docs/AUDIT-ACTIONS.md")
	bareLineSpan := regexp.MustCompile("`:[0-9]+`")
	citation := regexp.MustCompile("`(internal/[A-Za-z0-9_/.-]+\\.go):([0-9]+)`")

	for _, ruleSource := range []string{"builtin:private-ip", "builtin:dial-failed"} {
		var row string
		for _, line := range strings.Split(doc, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "| `"+ruleSource+"` |") {
				row = line
				break
			}
		}
		if row == "" {
			t.Fatalf("docs/AUDIT-ACTIONS.md has no rule_source row for %q — that table is where an "+
				"operator looks up what a decision line means; re-derive this guard before deleting it",
				ruleSource)
		}
		if m := bareLineSpan.FindString(row); m != "" {
			t.Errorf("the %s row still carries the bare line shorthand %s: "+
				"TestAuditActionsDocCitationsAreLive resolves a span only when it names a path, so a "+
				"bare %s is a citation nothing checks — spell the file out",
				ruleSource, m, m)
		}
		sites := citation.FindAllStringSubmatch(row, -1)
		if len(sites) < 2 {
			t.Errorf("the %s row cites %d site(s): this rule_source is emitted from more than one "+
				"place, and a row that names only one tells an operator the other emitters do not exist",
				ruleSource, len(sites))
		}
		for _, m := range sites {
			lines := strings.Split(readRepoFile(t, m[1]), "\n")
			n, err := strconv.Atoi(m[2])
			if err != nil || n < 1 || n > len(lines) {
				t.Errorf("%s cites %s:%s, which is past the end of the file", ruleSource, m[1], m[2])
				continue
			}
			if !strings.Contains(lines[n-1], ruleSource) {
				t.Errorf("%s cites %s:%s, but that line does not emit it:\n\t%s",
					ruleSource, m[1], m[2], strings.TrimSpace(lines[n-1]))
			}
		}
	}
}
