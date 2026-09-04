// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Doc guards for the published SECURITY documents — threatmodel/THREAT-MODEL.md,
// threatmodel/AGENT-THREAT-MODEL.md, ARCHITECTURE.md, PROVENANCE.md, SECURITY.md.
//
// Every sentence in those files is a CLAIM ABOUT THIS CODE, and the R6 review
// found twenty-three of them describing a tree that no longer exists: four
// capability kinds where six ship, a fail-closed canary with three shipped
// operator overrides nobody documented, an "unwired primitive" mounted on a
// route, a two-role authorization model on a three-tier deployment. None of
// those had a gate, which is why each survived the release it was written for.
//
// Each test below derives its expectation FROM THE CODE (a const, a route
// registration, a struct field, an env default) and asserts the document agrees.
// A rewrite of the code therefore reddens the document, which is the direction
// that matters: a security document is only worth publishing if it is the
// cheapest thing to keep true, not the easiest thing to leave stale.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ─── shared helpers ──────────────────────────────────────────────────────────

// readRepoFile reads one repo-relative file, failing the test if it is missing —
// a guard that silently skips its own subject passes vacuously forever.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// funcBody returns the source of the named top-level func, from its `func` line
// to the next top-level `func` (or EOF). Crude on purpose: these guards ask
// "does this function mention X at all", which does not need an AST.
func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "\nfunc "+name+"(")
	if start < 0 {
		t.Fatalf("func %s not found — the guard's anchor moved, so it is asserting nothing", name)
	}
	rest := src[start+1:]
	if end := strings.Index(rest[1:], "\nfunc "); end >= 0 {
		return rest[:end+1]
	}
	return rest
}

// numberWord spells a small count the way the prose does, so a guard can demand
// the document's WORD track a code-derived NUMBER.
var numberWord = map[int]string{
	1: "One", 2: "Two", 3: "Three", 4: "Four", 5: "Five",
	6: "Six", 7: "Seven", 8: "Eight", 9: "Nine", 10: "Ten",
}

// ─── F003: the capability kinds are a closed set, and the doc states its size ──

var (
	capKindsDecl  = regexp.MustCompile(`(?m)^var capabilityKinds = \[\]string\{([^}]*)\}`)
	capKindConst  = regexp.MustCompile(`(?m)^\t(cap[A-Za-z]+) = "([a-z_]+)"`)
	closedKindsRe = regexp.MustCompile(`(?m)^(One|Two|Three|Four|Five|Six|Seven|Eight|Nine|Ten) closed kinds`)
)

func TestThreatModelDocCapabilityKindsMatchCode(t *testing.T) {
	code := readRepoFile(t, "internal/api/capabilities.go")
	m := capKindsDecl.FindStringSubmatch(code)
	if m == nil {
		t.Fatal("capabilityKinds declaration not found in internal/api/capabilities.go")
	}
	values := map[string]string{}
	for _, c := range capKindConst.FindAllStringSubmatch(code, -1) {
		values[c[1]] = c[2]
	}
	var kinds []string
	for _, ident := range strings.Split(m[1], ",") {
		ident = strings.TrimSpace(ident)
		if ident == "" {
			continue
		}
		v, ok := values[ident]
		if !ok {
			t.Fatalf("capabilityKinds names %q but no `%s = \"...\"` const was found", ident, ident)
		}
		kinds = append(kinds, v)
	}
	if len(kinds) == 0 {
		t.Fatal("parsed zero capability kinds — this guard would pass vacuously")
	}

	doc := readRepoFile(t, "threatmodel/THREAT-MODEL.md")
	want := numberWord[len(kinds)]
	if got := closedKindsRe.FindStringSubmatch(doc); got == nil {
		t.Errorf("threatmodel/THREAT-MODEL.md no longer opens §4.3 with %q closed kinds — the code ships %d", want, len(kinds))
	} else if got[1] != want {
		t.Errorf("threatmodel/THREAT-MODEL.md says %q closed kinds; internal/api/capabilities.go ships %d (%s)",
			got[1], len(kinds), strings.Join(kinds, ", "))
	}
	for _, k := range kinds {
		if !strings.Contains(doc, "`"+k+"`") {
			t.Errorf("capability kind %q ships in capabilityKinds but is named nowhere in threatmodel/THREAT-MODEL.md", k)
		}
	}
	// The §4 attack row states the same number in words; it drifted separately.
	if strings.Contains(doc, "four closed kinds") {
		t.Error(`threatmodel/THREAT-MODEL.md still says "four closed kinds" in the §4 attack table`)
	}
	// The authz.denied reason vocabulary is docs/AUDIT-ACTIONS.md's, and it is
	// guarded there. A second, shorter copy here is how it went stale.
	if strings.Contains(doc, "closed vocabulary (`capability_workspace`") {
		t.Error("threatmodel/THREAT-MODEL.md restates the authz.denied reason vocabulary; point at docs/AUDIT-ACTIONS.md instead")
	}
}

// ─── F006: every fail-closed boot gate that ships an operator override ────────

// overrideEnv finds the WARDYN_* env vars a substrate register.go reads into a
// fail-closed override field. These are the knobs that let a deployment boot
// PAST a gate the threat model describes as unconditional, so each one is a
// residual the published document has to carry.
var overrideEnv = regexp.MustCompile(`(?:AllowUnenforced\w*|AllowUnenforceable\w*|AckAmbient\w*):\s*os\.Getenv\("(WARDYN_[A-Z0-9_]+)"\)`)

func TestThreatModelDocNamesFailClosedOverrides(t *testing.T) {
	var envs []string
	for _, rel := range []string{"internal/runner/k8s/register.go", "internal/runner/docker/register.go"} {
		for _, m := range overrideEnv.FindAllStringSubmatch(readRepoFile(t, rel), -1) {
			envs = append(envs, m[1])
		}
	}
	if len(envs) == 0 {
		t.Fatal("found no fail-closed override env vars in the substrate register.go files — the pattern moved")
	}
	doc := readRepoFile(t, "threatmodel/THREAT-MODEL.md")
	for _, env := range envs {
		if !strings.Contains(doc, env) {
			t.Errorf("%s lets a deployment boot past a gate threatmodel/THREAT-MODEL.md describes as fail-closed, "+
				"and the document never names it", env)
		}
	}
}

// ─── F014/F017/F019: the role set is closed, and three documents state it ─────

var rolesDecl = regexp.MustCompile(`(?m)^var Roles = \[\]string\{([^}]*)\}`)

func TestSecurityDocsNameEveryRole(t *testing.T) {
	code := readRepoFile(t, "internal/auth/oidc/derive.go")
	m := rolesDecl.FindStringSubmatch(code)
	if m == nil {
		t.Fatal("`var Roles` not found in internal/auth/oidc/derive.go")
	}
	constVal := regexp.MustCompile(`(?m)^\t(Role[A-Za-z]+)\s+= "([a-z_]+)"`)
	values := map[string]string{}
	for _, c := range constVal.FindAllStringSubmatch(code, -1) {
		values[c[1]] = c[2]
	}
	var roles []string
	for _, ident := range strings.Split(m[1], ",") {
		if ident = strings.TrimSpace(ident); ident != "" {
			v, ok := values[ident]
			if !ok {
				t.Fatalf("Roles names %q but no `%s = \"...\"` const was found", ident, ident)
			}
			roles = append(roles, v)
		}
	}
	if len(roles) < 2 {
		t.Fatalf("parsed %d roles — this guard would pass vacuously", len(roles))
	}
	for _, rel := range []string{"threatmodel/THREAT-MODEL.md", "ARCHITECTURE.md", "SECURITY.md"} {
		doc := readRepoFile(t, rel)
		for _, role := range roles {
			if !strings.Contains(doc, role) {
				t.Errorf("%s never names the %q role, which internal/auth/oidc's Roles ships", rel, role)
			}
		}
	}
}

// ─── F020/F021: the CSP the docs quote is the CSP the server sends ────────────

func TestSecurityDocsQuoteServedCSP(t *testing.T) {
	code := readRepoFile(t, "internal/api/security_headers.go")
	start := strings.Index(code, `const csp = `)
	if start < 0 {
		t.Fatal("`const csp` not found in internal/api/security_headers.go")
	}
	end := strings.Index(code[start:], "\n\treturn ")
	if end < 0 {
		t.Fatal("could not bound the csp const literal")
	}
	lit := code[start : start+end]
	var directives []string
	for _, part := range strings.Split(lit, ";") {
		// Each source line is a quoted fragment; keep only the directive NAME,
		// which is what the prose has to account for.
		part = strings.TrimSpace(strings.NewReplacer(`"`, "", "+", "", "\n", " ", "\t", " ").Replace(part))
		part = strings.TrimSpace(strings.TrimPrefix(part, "const csp = "))
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		if name := fields[0]; strings.Contains(name, "-src") || strings.Contains(name, "-ancestors") || strings.Contains(name, "-uri") {
			directives = append(directives, name)
		}
	}
	if len(directives) < 5 {
		t.Fatalf("parsed %d CSP directives from the served policy — the literal's shape moved", len(directives))
	}
	doc := readRepoFile(t, "threatmodel/THREAT-MODEL.md")
	for _, d := range directives {
		if !strings.Contains(doc, d) {
			t.Errorf("the served CSP carries %s and threatmodel/THREAT-MODEL.md's "+
				"\"Console auth token storage\" section never mentions it — the section prices the XSS risk of a "+
				"full-admin token in browser storage, so an unlisted directive is an unpriced one", d)
		}
	}
	// ARCHITECTURE.md's one-line summary of the same header must not read as a
	// closed list while three of those directives admit an external destination.
	arch := readRepoFile(t, "ARCHITECTURE.md")
	if strings.Contains(arch, "exactly one class of external resource") && !strings.Contains(arch, "connect-src") {
		t.Error(`ARCHITECTURE.md calls the console CSP "exactly one class of external resource" without naming ` +
			`connect-src/script-src/font-src, which also admit destinations`)
	}
}

func TestSecurityDocsCiteTheFileThatDeclaresSecurityHeaders(t *testing.T) {
	root := repoRoot(t)
	var owner string
	entries, err := os.ReadDir(filepath.Join(root, "internal", "api"))
	if err != nil {
		t.Fatalf("read internal/api: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		if strings.Contains(readRepoFile(t, "internal/api/"+e.Name()), "func securityHeaders(") {
			owner = "internal/api/" + e.Name()
		}
	}
	if owner == "" {
		t.Fatal("no file under internal/api declares func securityHeaders — the guard's anchor moved")
	}
	for _, rel := range []string{"threatmodel/THREAT-MODEL.md", "ARCHITECTURE.md"} {
		for i, line := range strings.Split(readRepoFile(t, rel), "\n") {
			if !strings.Contains(line, "securityHeaders") || !strings.Contains(line, "internal/api/") {
				continue
			}
			if !strings.Contains(line, owner) {
				t.Errorf("%s:%d cites securityHeaders to the wrong file — it is declared in %s", rel, i+1, owner)
			}
		}
	}
}

// ─── F023: the sandbox sweep is mounted on a route ───────────────────────────

func TestThreatModelDocSweepIsWired(t *testing.T) {
	routes := readRepoFile(t, "internal/api/routes.go")
	if !strings.Contains(routes, `"/admin/sandboxes/sweep"`) {
		t.Skip("the sweep route is gone; this guard has nothing to assert")
	}
	doc := readRepoFile(t, "threatmodel/THREAT-MODEL.md")
	for _, stale := range []string{"unwired primitive", "nothing calls it yet"} {
		if strings.Contains(doc, stale) {
			t.Errorf("threatmodel/THREAT-MODEL.md calls SweepTerminalSandboxes %q, but internal/api/routes.go "+
				"mounts POST /admin/sandboxes/sweep on it", stale)
		}
	}
	if !strings.Contains(doc, "/admin/sandboxes/sweep") {
		t.Error("threatmodel/THREAT-MODEL.md never names the shipped operator sweep route")
	}
}

// ─── F024/F025: the tool-enforcement plane partly ships ──────────────────────

func TestThreatModelDocToolPlaneIsPartlyShipped(t *testing.T) {
	rules := readRepoFile(t, "internal/egress/proxy/tool_rules.go")
	if !strings.Contains(rules, "func (p *Proxy) decideByToolRules(") {
		t.Skip("the proxy-side tool_rules plane is gone; this guard has nothing to assert")
	}
	doc := readRepoFile(t, "threatmodel/THREAT-MODEL.md")
	if strings.Contains(doc, "L3 does not exist today") {
		t.Error("threatmodel/THREAT-MODEL.md says \"L3 does not exist today\" while decideByToolRules resolves " +
			"allow/deny proxy-side and cmd/wardyn-toolgate ships in the agent images")
	}
	for _, want := range []string{"tool_rules", "wardyn-toolgate"} {
		if !strings.Contains(doc, want) {
			t.Errorf("threatmodel/THREAT-MODEL.md never names %s, which ships as part of the tool plane", want)
		}
	}
	// The sandbox-facing approvals route is dispatched unconditionally — on every
	// run, not only hold-mode runs — so it is an inbound write surface the
	// document has to model.
	if !strings.Contains(doc, "/wardyn/v1/approvals") {
		t.Error("threatmodel/THREAT-MODEL.md never names POST /wardyn/v1/approvals, the sandbox's inbound " +
			"write surface into the control plane")
	}
}

func TestArchitectureDocListsShippedSandboxBinaries(t *testing.T) {
	arch := readRepoFile(t, "ARCHITECTURE.md")
	// Only the binaries that are BUILT INTO an agent image or a compose service
	// are components a reader has to know about; the standalone harnesses are
	// already called out as dev-only where they appear.
	for _, bin := range []string{"wardyn-toolgate", "wardyn-aws-sso"} {
		if _, err := os.Stat(filepath.Join(repoRoot(t), "cmd", bin)); err != nil {
			continue
		}
		if !strings.Contains(arch, bin) {
			t.Errorf("cmd/%s ships and is built into the agent images, but ARCHITECTURE.md's components table "+
				"never names it", bin)
		}
	}
}

// ─── F026: residual #14's route table vs. the router ─────────────────────────

func TestThreatModelDocRouteTableMatchesRouter(t *testing.T) {
	routes := readRepoFile(t, "internal/api/routes.go")
	doc := readRepoFile(t, "threatmodel/THREAT-MODEL.md")

	// Secret write/delete moved to the plain signed-in-human group (self-service).
	if strings.Contains(routes, `r.Put("/secrets/{name}"`) && strings.Contains(doc, "secret write/delete") {
		t.Error("residual #14 still promises members 403 on secret write/delete; internal/api/routes.go mounts " +
			"PUT/DELETE /secrets/{name} on the plain signed-in-human group")
	}
	// Grant CRUD is securityOps (admin OR security_admin), not operatorOnly.
	if strings.Contains(routes, "s.mountPermissionRoutes(securityOps)") &&
		strings.Contains(doc, "the enforcement switches are themselves\n    `operatorOnly`") {
		t.Error("residual #14 calls grant CRUD + the enforcement switches `operatorOnly`; " +
			"internal/api/routes.go mounts them on securityOps")
	}
	// Kill is owner-or-admin, not open to any signed-in human.
	lifecycle := readRepoFile(t, "internal/api/runs_lifecycle.go")
	if strings.Contains(funcBody(t, lifecycle, "(s *Server) handleKillRun"), "s.getRunAuthorized(") &&
		strings.Contains(doc, "`POST /runs`, `POST /runs/{id}/kill`, every read") {
		t.Error("residual #14 lists POST /runs/{id}/kill as open to any signed-in human; handleKillRun goes " +
			"through getRunAuthorized (owner-or-admin, byte-identical 404 on a foreign run)")
	}
}

// ─── F027/F028: the git_pat lane is never-resident by default ────────────────

var gitPATDefault = regexp.MustCompile(`flagEnv\("git-pat-broker", "(WARDYN_GIT_PAT_BROKER)", "([a-z]+)"`)

func TestThreatModelDocGitPATBrokerDefault(t *testing.T) {
	m := gitPATDefault.FindStringSubmatch(readRepoFile(t, "cmd/wardynd/boot_flags.go"))
	if m == nil {
		t.Fatal("the WARDYN_GIT_PAT_BROKER flag declaration moved — the guard's anchor is gone")
	}
	if m[2] != "on" {
		t.Skipf("WARDYN_GIT_PAT_BROKER now defaults to %q; the residency claim below is only wrong when it is on", m[2])
	}
	for _, rel := range []string{"threatmodel/THREAT-MODEL.md", "ARCHITECTURE.md"} {
		doc := readRepoFile(t, rel)
		if !strings.Contains(doc, m[1]) {
			t.Errorf("%s describes the git_pat lane without naming %s, which ships ON by default and makes "+
				"the PAT never-resident", rel, m[1])
		}
	}
	// The reason the doc gave for residency is refuted by the shipped lane.
	if strings.Contains(readRepoFile(t, "threatmodel/THREAT-MODEL.md"),
		"an opaque CONNECT tunnel the proxy cannot inject Basic auth into without MITM") {
		t.Error("§5.1a still says proxy-side injection is impossible for git_pat; " +
			"internal/egress/proxy/pat_broker.go does exactly that by terminating the request instead")
	}
}

// exceptionRows counts the rows of a markdown table, separator line excluded.
// Both copies of the resident-secret exception table are indented differently,
// so leading whitespace is part of the match.
var (
	tableRow = regexp.MustCompile(`(?m)^[ \t]*\|`)
	tableSep = regexp.MustCompile(`(?m)^[ \t]*\|[-| ]+\|[-| ]*$`)
)

func exceptionRows(s string) int {
	return len(tableRow.FindAllString(s, -1)) - len(tableSep.FindAllString(s, -1))
}

func TestArchitectureDocDoesNotCopyTheResidentSecretTable(t *testing.T) {
	arch := readRepoFile(t, "ARCHITECTURE.md")
	if !strings.Contains(arch, "authoritative, complete table") {
		return
	}
	// Count the rows of any table ARCHITECTURE.md prints under that sentence,
	// and of the table it defers to. A shorter copy beside the word "complete"
	// is the drift surface the R6 review found.
	idx := strings.Index(arch, "authoritative, complete table")
	tail := arch[idx:]
	if end := strings.Index(tail, "\n\n   Secret values are masked"); end > 0 {
		tail = tail[:end]
	}
	copied := exceptionRows(tail)
	if copied <= 0 {
		return // the pointer alone: nothing to drift.
	}
	tm := readRepoFile(t, "threatmodel/THREAT-MODEL.md")
	sec := tm[strings.Index(tm, "Resident-secret exceptions"):]
	if end := strings.Index(sec, "\n\n**`ssh_key` and `git_pat`"); end > 0 {
		sec = sec[:end]
	}
	authoritative := exceptionRows(sec)
	if copied != authoritative {
		t.Errorf("ARCHITECTURE.md prints %d resident-secret exception rows while calling §5.1a's %d-row table "+
			"\"the authoritative, complete table\" — drop the copy or print all of it", copied, authoritative)
	}
}

// ─── F039/F040: PROVENANCE's claims are checkable, and true ──────────────────

// provenanceExclusions are the paths that legitimately carry a FOREIGN copyright
// line: vendored upstream licence texts and the generated attribution notices
// (plus the script that generates them). PROVENANCE.md's "no foreign copyright
// notice exists anywhere in the tracked tree" claim is only true with this scope,
// so the document must state it — a claim an evaluator runs and sees fail is
// worse than one that was never made.
var provenanceExclusions = []string{
	"ui/node_modules",
	"ui/dist",
	"licenses/",
	"NOTICE",
	"THIRD-PARTY-NOTICES.md",
	"ui/public/THIRD-PARTY-NOTICES.txt",
	"scripts/third-party-notices.sh",
}

var foreignCopyright = regexp.MustCompile(`Copyright [0-9(]`)

func TestProvenanceDocCopyrightScopeIsTrue(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable path is not this guard's business
		}
		rel := filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))
		if d.IsDir() {
			if rel == ".git" || rel == "ui/node_modules" || rel == "ui/dist" || rel == "licenses" {
				return filepath.SkipDir
			}
			return nil
		}
		for _, ex := range provenanceExclusions {
			if strings.HasPrefix(rel, ex) || strings.HasSuffix(rel, "/"+ex) || rel == ex {
				return nil
			}
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > 1<<20 {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, line := range strings.Split(string(b), "\n") {
			if foreignCopyright.MatchString(line) && !strings.Contains(line, "The Wardyn Authors") {
				offenders = append(offenders, rel+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	for _, o := range offenders {
		t.Errorf("foreign copyright notice outside PROVENANCE.md's stated scope: %s", o)
	}

	prov := readRepoFile(t, "PROVENANCE.md")
	for _, ex := range provenanceExclusions {
		if !strings.Contains(prov, strings.TrimSuffix(ex, "/")) {
			t.Errorf("PROVENANCE.md's no-foreign-copyright claim excludes %q in practice but never says so — "+
				"an evaluator who runs the claim as written sees it fail", ex)
		}
	}
	// A frozen occurrence count rots on the next commit; the command does not.
	if regexp.MustCompile(`— [0-9,]{3,} occurrences`).MatchString(prov) {
		t.Error("PROVENANCE.md freezes a copyright-line COUNT; state the runnable check instead")
	}
	if regexp.MustCompile(`of the [0-9,]{3,} non-merge commits`).MatchString(prov) {
		t.Error("PROVENANCE.md freezes a non-merge commit TOTAL, which rots every release; " +
			"state the count that does not move, plus the command")
	}
}

func TestProvenanceDocDCOMatchesCI(t *testing.T) {
	ci := readRepoFile(t, ".github/workflows/ci.yml")
	prov := readRepoFile(t, "PROVENANCE.md")
	if !strings.Contains(ci, `DCO_RANGE="$BEFORE..HEAD"`) {
		t.Skip("the dco job no longer range-checks a push; the disclosed gap may be current again")
	}
	if strings.Contains(prov, "a direct push to `main` checks only the tip commit, so commits\narriving that way were never examined") {
		t.Error("PROVENANCE.md discloses a DCO gap the ci.yml dco job already closed " +
			"(it range-checks github.event.before..HEAD on a push)")
	}
	if !strings.Contains(prov, "github.event.before") {
		t.Error("PROVENANCE.md describes the DCO gate without naming the range it actually checks " +
			"(github.event.before..HEAD)")
	}
	// The claim that a SIBLING document carries the scope statement is only
	// checkable if that document actually carries one.
	if strings.Contains(prov, "`CONTRIBUTING.md` now\nstates the enforcement scope accurately") {
		t.Error("PROVENANCE.md credits CONTRIBUTING.md with an enforcement-scope statement it does not contain")
	}
}

// ─── F050: a named CVE has to be one this tree actually reasons about ────────

var (
	cveRe = regexp.MustCompile(`CVE-\d{4}-\d{4,7}`)
	// The Kata floor writes a PAIR as "CVE-2026-44210/-47243"; expand the
	// second half so the document may use the same shorthand the code does.
	cvePairRe = regexp.MustCompile(`CVE-(\d{4})-\d{4,7}/-(\d{4,7})`)
)

func TestThreatModelDocCVEsAreBackedByTheTree(t *testing.T) {
	backed := map[string]bool{}
	for _, rel := range []string{"cmd/wardyn/setup.go", "internal/runner/docker/hardening.go", "SECURITY.md"} {
		src := readRepoFile(t, rel)
		for _, id := range cveRe.FindAllString(src, -1) {
			backed[id] = true
		}
		for _, m := range cvePairRe.FindAllStringSubmatch(src, -1) {
			backed["CVE-"+m[1]+"-"+m[2]] = true
		}
	}
	if len(backed) == 0 {
		t.Fatal("no CVE id found anywhere in the tree — this guard would pass vacuously")
	}
	for _, rel := range []string{"threatmodel/THREAT-MODEL.md", "threatmodel/AGENT-THREAT-MODEL.md"} {
		src := readRepoFile(t, rel)
		ids := cveRe.FindAllString(src, -1)
		for _, m := range cvePairRe.FindAllStringSubmatch(src, -1) {
			ids = append(ids, "CVE-"+m[1]+"-"+m[2])
		}
		for _, id := range ids {
			if !backed[id] {
				t.Errorf("%s names %s, which nothing in this tree floors, denylists or otherwise reasons about — "+
					"a CVE cited as an example must be one a reader can trace to code, or it should not be cited",
					rel, id)
			}
		}
	}
}

// ─── F051: RFC 8707 belongs to the identity mint, not the GitHub one ─────────

func TestThreatModelDocRFC8707NotOnTheGitHubMint(t *testing.T) {
	mint := readRepoFile(t, "internal/broker/github.go")
	if strings.Contains(mint, "8707") || strings.Contains(mint, "Audience") {
		t.Skip("the GitHub mint now carries an audience; the doc claim may be current")
	}
	doc := readRepoFile(t, "threatmodel/THREAT-MODEL.md")
	for i, line := range strings.Split(doc, "\n") {
		if !strings.Contains(line, "8707") {
			continue
		}
		// A line that DENIES the binding is the correction, not the defect —
		// and the correction has to be allowed to name both halves in one
		// breath, or the only way to pass would be to say nothing.
		if strings.Contains(line, "not audience-bound") || strings.Contains(line, "no audience") {
			continue
		}
		for _, brokered := range []string{"git_broker.go", "installation token", "repo + permission"} {
			if strings.Contains(line, brokered) {
				t.Errorf("threatmodel/THREAT-MODEL.md:%d attributes RFC 8707 audience binding to the GitHub "+
					"installation-token mint; internal/broker/github.go sends only {Repositories, Permissions}", i+1)
				break
			}
		}
	}
	// The exact overclaim this finding removed must not come back in any line.
	if strings.Contains(doc, "audience-bound per RFC 8707") {
		t.Error(`threatmodel/THREAT-MODEL.md carries "audience-bound per RFC 8707" again — ` +
			"GitHub's installation-token endpoint takes no audience or resource indicator")
	}
}

// ─── F055: every runtime family that is auto-granted CC3 ─────────────────────

var cc3Decl = regexp.MustCompile(`(?m)^var cc3Runtimes = \[\]string\{([^}]*)\}`)

func TestThreatModelDocCC3RuntimeFamilies(t *testing.T) {
	code := readRepoFile(t, "internal/runner/docker/hardening.go")
	m := cc3Decl.FindStringSubmatch(code)
	if m == nil {
		t.Fatal("`var cc3Runtimes` not found in internal/runner/docker/hardening.go")
	}
	constVal := regexp.MustCompile(`(?m)^\t(runtime[A-Za-z]+)\s+= "([a-z]+)"`)
	values := map[string]string{}
	for _, c := range constVal.FindAllStringSubmatch(code, -1) {
		values[c[1]] = c[2]
	}
	var families []string
	for _, ident := range strings.Split(m[1], ",") {
		if ident = strings.TrimSpace(ident); ident != "" {
			v, ok := values[ident]
			if !ok {
				t.Fatalf("cc3Runtimes names %q but no matching const was found", ident)
			}
			families = append(families, v)
		}
	}
	if len(families) == 0 {
		t.Fatal("parsed zero CC3 runtime families — this guard would pass vacuously")
	}
	doc := readRepoFile(t, "threatmodel/THREAT-MODEL.md")
	cc3 := doc[strings.Index(doc, "### CC3 —"):]
	for _, f := range families {
		if !strings.Contains(cc3, f) {
			t.Errorf("classToRuntime auto-grants CC3 to a %q runtime, and §7's CC3 section never names that family — "+
				"the published tier claim is narrower than the code's", f)
		}
	}
	// The operator-pin seam grants CC3 to a name outside the allowlist entirely.
	if strings.Contains(code, "func resolveRuntime(") && !strings.Contains(cc3, "resolveRuntime") {
		t.Error("§7's CC3 section does not mention resolveRuntime, which grants CC3 to any operator-pinned " +
			"runtime outside the known-non-vault list")
	}
}

// ─── F056: the rootless refusal's real owner ─────────────────────────────────

func TestThreatModelDocRootlessRefusalOwner(t *testing.T) {
	body := funcBody(t, readRepoFile(t, "internal/runner/docker/hardening.go"), "classToRuntime")
	if strings.Contains(strings.ToLower(body), "rootless") {
		t.Skip("classToRuntime now probes for rootlessness; the doc claim may be current")
	}
	doc := readRepoFile(t, "threatmodel/THREAT-MODEL.md")
	for i, line := range strings.Split(doc, "\n") {
		if strings.Contains(line, "classToRuntime") && strings.Contains(strings.ToLower(line), "rootless") {
			t.Errorf("threatmodel/THREAT-MODEL.md:%d credits classToRuntime with a rootless refusal; the "+
				"function consults only the daemon's registered runtimes and has no rootless awareness", i+1)
		}
	}
	if !strings.Contains(doc, "cmd/wardyn/setup.go") {
		t.Error("§7's rootless note never names cmd/wardyn/setup.go, which owns the only rootless detection " +
			"in the tree")
	}
}

// ─── F062: resource caps, per substrate ──────────────────────────────────────

func TestAgentThreatModelDocResourceCaps(t *testing.T) {
	k8s := readRepoFile(t, "internal/runner/k8s/sandbox.go")
	doc := readRepoFile(t, "threatmodel/AGENT-THREAT-MODEL.md")
	if strings.Contains(k8s, "PidsLimit requested but not enforced") &&
		strings.Contains(doc, "CPU, memory, PID and disk caps are enforced") {
		t.Error("AGENT-THREAT-MODEL.md row 14 says all four caps are enforced; the k8s substrate requests " +
			"neither a pids nor a disk cap and warns that the pod runs without them")
	}
	lifecycle := readRepoFile(t, "internal/lifecycle/lifecycle.go")
	def := readRepoFile(t, "examples/policies/default.json")
	if strings.Contains(lifecycle, "if run.PolicyAutoStopAfterSec <= 0 {") &&
		strings.Contains(def, `"auto_stop_after_sec": 0`) &&
		strings.Contains(doc, "run lifetime is bounded") {
		t.Error("AGENT-THREAT-MODEL.md row 14 says run lifetime is bounded; the reaper skips every run whose " +
			"policy sets auto_stop_after_sec <= 0, and the shipped default policy sets 0")
	}
}

// ─── F068: an unanswerable group snapshot no longer fails open ───────────────

func TestThreatModelDocGroupDenyNotFailOpen(t *testing.T) {
	caps := readRepoFile(t, "internal/api/capabilities.go")
	if !strings.Contains(caps, "capUnresolvableGroupDeny") {
		t.Skip("the unresolvable-group-deny refusal is gone; residual #20's fail-open text may be current")
	}
	doc := readRepoFile(t, "threatmodel/THREAT-MODEL.md")
	if strings.Contains(doc, "that resolves as allow-by-default") {
		t.Error("residual #20 still describes an unanswerable group snapshot as resolving allow-by-default; " +
			"capScan now reports the group deny it cannot rule out")
	}
	if !strings.Contains(doc, "capUnresolvableGroupDeny") {
		t.Error("residual #20 never names capUnresolvableGroupDeny, the refusal that replaced the fail-open")
	}
}

// ─── F032: a pre-0.7 session cookie is not a session ─────────────────────────

func TestThreatModelDocSessionCodecForcesOneRelogin(t *testing.T) {
	codec := readRepoFile(t, "internal/auth/oidc/session_codec.go")
	if !strings.Contains(codec, "sess.V != SessionCodecVersion") {
		t.Skip("decodeSession no longer version-gates the payload; the doc claim may be current")
	}
	doc := readRepoFile(t, "threatmodel/THREAT-MODEL.md")
	// The old text listed "a human on a pre-0.6 session" among the LIVE
	// unanswerable-snapshot shapes, which presumes such a cookie still
	// authenticates. Under an exact version compare it does not: it is
	// ErrInvalidSession, and the human re-logs in once.
	if strings.Contains(doc, "a human on a pre-0.6\n      session") ||
		strings.Contains(doc, "a human on a pre-0.6 session") {
		t.Error("threatmodel/THREAT-MODEL.md counts a pre-0.6 session cookie among the shapes that still " +
			"resolve capabilities; decodeSession refuses any payload whose version is not an exact match")
	}
	if !strings.Contains(doc, "SessionCodecVersion") {
		t.Error("threatmodel/THREAT-MODEL.md never names SessionCodecVersion, the exact-match gate that " +
			"makes the 0.6 → 0.7 upgrade cost one forced re-login")
	}
	// The exactness is the load-bearing half: under `<` an old binary in a
	// rolling upgrade would half-trust a new cookie.
	if !strings.Contains(doc, "mixed-version rollout") {
		t.Error("threatmodel/THREAT-MODEL.md does not state what the EXACT (not `<`) compare costs a " +
			"mixed-version rollout — repeated logins, never containment")
	}
}

// ─── F069: the desktop deployment tier exists ────────────────────────────────

func TestArchitectureDocNamesTheDesktopTier(t *testing.T) {
	if _, err := os.Stat(filepath.Join(repoRoot(t), "deploy", "desktop", "docker-compose.yaml")); err != nil {
		t.Skip("the desktop tier is gone; nothing to name")
	}
	arch := readRepoFile(t, "ARCHITECTURE.md")
	if !strings.Contains(strings.ToLower(arch), "desktop") {
		t.Error("deploy/desktop ships its own compose entrypoint, installer, service units and a 600-line " +
			"document, and ARCHITECTURE.md's deployment-surface section never mentions it")
	}
}

// ─── F070: the reserved drive target is pinned on both substrates ────────────

func TestThreatModelDocDriveTargetPin(t *testing.T) {
	dockerPins := strings.Contains(readRepoFile(t, "internal/runner/docker/driver_mounts.go"),
		"if drive.Target != runner.DriveTarget {")
	if !dockerPins {
		t.Skip("the Docker driver no longer pins the drive target by equality")
	}
	doc := readRepoFile(t, "threatmodel/THREAT-MODEL.md")
	if strings.Contains(doc, "checks the allowed-prefix rule only") {
		t.Error("§4.6 says Docker's driveMount checks the allowed-prefix rule only; it refuses any target that " +
			"is not equal to runner.DriveTarget, which the same section states twenty lines later")
	}
}

// guardedClaimCount keeps the file honest about its own coverage: a guard file
// whose subjects all quietly moved would otherwise pass while asserting nothing.
func TestThreatModelDocGuardSubjectsExist(t *testing.T) {
	for _, rel := range []string{
		"threatmodel/THREAT-MODEL.md", "threatmodel/AGENT-THREAT-MODEL.md",
		"ARCHITECTURE.md", "PROVENANCE.md", "SECURITY.md",
	} {
		if n := len(readRepoFile(t, rel)); n < 1000 {
			t.Errorf("%s is %d bytes — too small to be the document these guards assert against", rel, n)
		}
	}
}
