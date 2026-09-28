// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Doc guards for the R6 docs-ops wave. Every test here derives its expectation
// from the thing the doc describes — a cobra flag set, a chi route table, a
// launcher script on disk, a Go const — so the doc cannot drift away from the
// code without going red. None of them read a line number.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// readOpsDoc is the one-line read+fatal every test below shares.
func readOpsDoc(t *testing.T, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{repoRoot(t)}, rel...)...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(rel...), err)
	}
	return string(b)
}

// TestDocsOpsExamplePoliciesREADMENamesNoPhantomCLIFlag: the shipped example
// README told the reader to "Launch it with --tool-approvals hold", a flag the
// CLI has never had. tool_approvals is a run-CREATE body field (the console
// wizard sets it; POST /runs validates it) with no CLI surface at all, so the
// one worked example of the tool_rules feature could not be launched the way
// its own page said.
//
// Derived, not hard-coded: the flag set comes out of cmd/wardyn's cobra
// definitions. Add a real --tool-approvals flag and this guard goes quiet on
// its own; leave the doc naming a flag that does not exist and it fails.
func TestDocsOpsExamplePoliciesREADMENamesNoPhantomCLIFlag(t *testing.T) {
	root := repoRoot(t)

	cli, err := os.ReadFile(filepath.Join(root, "cmd", "wardyn", "commands.go"))
	if err != nil {
		t.Fatalf("read cmd/wardyn/commands.go: %v", err)
	}
	// Every long flag cobra registers looks like `Flags().<Kind>Var*(&x, "name"`.
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`Flags\(\)\.\w+\(&?\w+(?:\.\w+)*,\s*"([a-z0-9-]+)"`).
		FindAllStringSubmatch(string(cli), -1) {
		declared[m[1]] = true
	}
	if len(declared) == 0 {
		t.Fatal("parsed zero flags out of cmd/wardyn/commands.go — this guard's matcher needs updating, it is checking nothing")
	}

	docs := []string{
		filepath.Join("examples", "policies", "README.md"),
		filepath.Join("docs", "POLICIES.md"),
	}
	for _, rel := range docs {
		doc := readOpsDoc(t, strings.Split(rel, string(filepath.Separator))...)
		for _, m := range regexp.MustCompile(`--([a-z][a-z0-9-]{2,})`).FindAllStringSubmatch(doc, -1) {
			flag := m[1]
			// Only claim the ones this wave is about: a run-create field name
			// written with a leading `--` is the exact mistake, and a generic
			// unknown-word scan would flag prose and third-party flags.
			if flag != "tool-approvals" && flag != "tool-rules" {
				continue
			}
			if !declared[flag] {
				t.Errorf("%s tells the reader to pass `--%s`, which cmd/wardyn does not define — it is a POST /runs body field, not a CLI flag", rel, flag)
			}
		}
	}
}

// TestDocsOpsExamplePoliciesREADMEDocumentsEveryPolicyField: a botched edit
// deleted `git_push_any_branch`'s name from the remote-workspace prose, leaving
// a subject-less sentence and a spliced markdown link — the one field that
// disables push branch-namespace confinement went unnamed on the page that
// exists to explain the example that sets it.
//
// Derived from the example policies themselves: every top-level key any shipped
// example sets must be named somewhere on the page that documents them.
func TestDocsOpsExamplePoliciesREADMEDocumentsEveryPolicyField(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "examples", "policies")
	doc := readOpsDoc(t, "examples", "policies", "README.md")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read examples/policies: %v", err)
	}
	// Only the keys whose whole point is that an operator opted INTO them:
	// a key every example carries is table stakes and documented elsewhere.
	interesting := map[string]bool{
		"git_push_any_branch": true,
		"tool_rules":          true,
		"ui_apps":             true,
		"llm_inspection":      true,
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		// Top-level keys, JSON and YAML alike, without decoding either: a key
		// at column 0 (YAML) or with two-space indent inside the opening brace
		// (JSON) — both shapes the shipped examples use.
		for _, m := range regexp.MustCompile(`(?m)^(?:  )?"?([a-z_]+)"?:`).FindAllStringSubmatch(string(b), -1) {
			if interesting[m[1]] {
				seen[m[1]] = true
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("no opt-in policy keys found across examples/policies — this guard is checking nothing")
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.Contains(doc, k) {
			t.Errorf("examples/policies/README.md never names `%s`, which a shipped example sets — the page documenting these examples must say what the field does", k)
		}
	}
}

// TestDocsOpsExampleUIAppsHaveAShippedLauncher: the only shipped ui_apps
// example declared app name "code", and the gateway builds the launcher path as
// /usr/local/bin/wardyn-ui-<name>. No image ships wardyn-ui-code, so the
// feature's one worked policy fail-closed on the feature's one shipped image.
//
// Derived from deploy/images/: an app name is valid exactly when some image
// carries a launcher of that name.
func TestDocsOpsExampleUIAppsHaveAShippedLauncher(t *testing.T) {
	root := repoRoot(t)

	launchers := map[string]bool{}
	imgs, err := os.ReadDir(filepath.Join(root, "deploy", "images"))
	if err != nil {
		t.Fatalf("read deploy/images: %v", err)
	}
	for _, img := range imgs {
		if !img.IsDir() {
			continue
		}
		files, ferr := os.ReadDir(filepath.Join(root, "deploy", "images", img.Name()))
		if ferr != nil {
			continue
		}
		for _, f := range files {
			if app := strings.TrimPrefix(f.Name(), "wardyn-ui-"); app != f.Name() {
				launchers[app] = true
			}
		}
	}
	if len(launchers) == 0 {
		t.Fatal("no wardyn-ui-* launcher found under deploy/images — this guard is checking nothing")
	}

	dir := filepath.Join(root, "examples", "policies")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read examples/policies: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		var spec struct {
			UIApps []struct {
				Name string `json:"name"`
			} `json:"ui_apps"`
		}
		if json.Unmarshal(b, &spec) != nil {
			continue // a template with a __comment key; not a launchable spec
		}
		for _, app := range spec.UIApps {
			checked++
			if !launchers[app.Name] {
				have := make([]string, 0, len(launchers))
				for l := range launchers {
					have = append(have, l)
				}
				sort.Strings(have)
				t.Errorf("examples/policies/%s declares ui_apps name %q, but no image ships /usr/local/bin/wardyn-ui-%s — the relay fails closed on it. Shipped launchers: %v",
					e.Name(), app.Name, app.Name, have)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no ui_apps entry found in any example policy — this guard is checking nothing")
	}
}

// TestDocsOpsLibraryRoutesDocMatchesRegistrations: the workspace-model section
// enumerated GET/PUT/DELETE on /sources/{id} and GET on /base-images/{id}, and
// told the reader to author a source's requirements contract through the PUT —
// a route whose handler the code deleted outright (the file's own DEADCODE
// comment says "gone, not stubbed"). The enumeration is now derived from
// mountLibraryRoutes' actual registrations.
func TestDocsOpsLibraryRoutesDocMatchesRegistrations(t *testing.T) {
	root := repoRoot(t)

	src, err := os.ReadFile(filepath.Join(root, "internal", "api", "sources.go"))
	if err != nil {
		t.Fatalf("read internal/api/sources.go: %v", err)
	}
	body := regexp.MustCompile(`(?s)func \(s \*Server\) mountLibraryRoutes\(.*?\n}`).FindString(string(src))
	if body == "" {
		t.Fatal("could not find mountLibraryRoutes in internal/api/sources.go — re-anchor this guard if it was renamed")
	}
	registered := map[string]bool{} // "GET /sources/{id}"
	for _, m := range regexp.MustCompile(`\.(Get|Post|Put|Patch|Delete)\("(/[^"]*)"`).FindAllStringSubmatch(body, -1) {
		registered[strings.ToUpper(m[1])+" "+m[2]] = true
	}
	if len(registered) == 0 {
		t.Fatal("parsed zero routes out of mountLibraryRoutes — this guard is checking nothing")
	}

	doc := readOpsDoc(t, "docs", "OPERATIONS.md")
	// The doc writes them as `GET/POST /api/v1/sources`. Expand each cell into
	// the individual verbs it claims and check every one is real.
	claimRe := regexp.MustCompile("`((?:GET|POST|PUT|PATCH|DELETE)(?:/(?:GET|POST|PUT|PATCH|DELETE))*)\\s+(/api/v1/(?:sources|base-images)[^`]*)`")
	claims := 0
	for _, m := range claimRe.FindAllStringSubmatch(doc, -1) {
		path := strings.TrimSpace(strings.TrimPrefix(m[2], "/api/v1"))
		for _, verb := range strings.Split(m[1], "/") {
			claims++
			if !registered[verb+" "+path] {
				t.Errorf("docs/OPERATIONS.md claims `%s /api/v1%s`, which mountLibraryRoutes does not register", verb, path)
			}
		}
	}
	if claims == 0 {
		t.Fatal("docs/OPERATIONS.md no longer enumerates the library routes — re-anchor this guard")
	}
}

// TestDocsOpsSessionCookieDocsMatchTheCodec: three shipped documents said
// a pre-0.6 session cookie stays valid across the upgrade with no forced
// re-login, and docs/ENV.md contradicted itself inside a single table row. The
// codec disagrees — decodeSession compares the payload's version EXACTLY, so a
// cookie with no `v` key at all is not a stale-groups session, it is not a
// session.
//
// Derived from the const: if the exact-match codec is ever relaxed, this guard
// stops demanding the docs say otherwise.
func TestDocsOpsSessionCookieDocsMatchTheCodec(t *testing.T) {
	root := repoRoot(t)

	codec, err := os.ReadFile(filepath.Join(root, "internal", "auth", "oidc", "session_codec.go"))
	if err != nil {
		t.Fatalf("read internal/auth/oidc/session_codec.go: %v", err)
	}
	exactMatch := regexp.MustCompile(`sess\.V != SessionCodecVersion`).Match(codec)
	if !exactMatch {
		t.Skip("decodeSession no longer requires an exact codec version — the docs claim this guard enforces is no longer the code's claim")
	}

	staleClaim := regexp.MustCompile(`(?i)pre-0\.[67][^.\n]{0,120}stays valid`)
	for _, rel := range [][]string{{"docs", "OPERATIONS.md"}, {"docs", "ENV.md"}} {
		doc := readOpsDoc(t, rel...)
		if m := staleClaim.FindString(doc); m != "" {
			t.Errorf("%s still says a pre-upgrade cookie stays valid (%q), but decodeSession refuses any payload whose codec version differs — the human is bounced to sign in",
				filepath.Join(rel...), m)
		}
	}

	// And the fact has to be WHERE an operator planning an upgrade reads.
	ops := readOpsDoc(t, "docs", "OPERATIONS.md")
	upgrades := ops[strings.Index(ops, "\n## Upgrades"):]
	if i := strings.Index(upgrades[1:], "\n## "); i >= 0 {
		upgrades = upgrades[:i+1]
	}
	if !strings.Contains(upgrades, "SessionCodecVersion") {
		t.Error(`docs/OPERATIONS.md's "## Upgrades" section never mentions SessionCodecVersion — upgrading signs every SSO human out once, and the runbook is where that belongs`)
	}
}

// TestDocsOpsDiskCapDocSaysWhatBothSubstratesDo pins the disk-cap row to what
// each substrate does, and the two differ. Docker warns and runs uncapped when a
// cap is demanded but unsupported — it does not fail closed. The k8s substrate
// sets the agent container's resources.limits[ephemeral-storage] and the kubelet
// enforces it by evicting the pod, so it has no such warning, and "both
// substrates warn" is false.
//
// Anchored on all three code facts — Docker's uncapped warning, the ABSENCE of
// the k8s one, and the limit that replaced it — so a change of posture on either
// side re-opens this guard rather than leaving the row quietly wrong.
func TestDocsOpsDiskCapDocSaysWhatBothSubstratesDo(t *testing.T) {
	root := repoRoot(t)
	readGo := func(rel ...string) string {
		b, err := os.ReadFile(filepath.Join(append([]string{root}, rel...)...))
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Join(rel...), err)
		}
		return string(b)
	}

	uncapped := regexp.MustCompile(`(?i)WITHOUT a disk cap`)
	hardening := readGo("internal", "runner", "docker", "hardening.go")
	if !uncapped.MatchString(hardening) {
		t.Fatalf("internal/runner/docker/hardening.go no longer carries the uncapped-disk warning this guard is anchored on — re-check whether the docker substrate now fails closed, and update docs/POLICIES.md with it")
	}
	if uncapped.MatchString(readGo("internal", "runner", "k8s", "sandbox.go")) {
		t.Fatalf("internal/runner/k8s/sandbox.go warns about an unenforced disk cap again — if the ephemeral-storage limit was dropped, docs/POLICIES.md's disk_mib row has to go back to saying the k8s substrate runs uncapped")
	}
	if !strings.Contains(readGo("internal", "runner", "k8s", "naming.go"), "ResourceEphemeralStorage") {
		t.Fatalf("internal/runner/k8s/naming.go no longer sets ephemeral-storage on the agent container — docs/POLICIES.md's disk_mib row promises the kubelet bounds a k8s run's writable layer")
	}

	doc := readOpsDoc(t, "docs", "POLICIES.md")
	row := ""
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "| `disk_mib`") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatal("docs/POLICIES.md has no `disk_mib` row — re-anchor this guard")
	}
	if regexp.MustCompile(`(?i)fails? closed`).MatchString(row) {
		t.Errorf("docs/POLICIES.md's disk_mib row still says the cap fails closed when unsupported; Docker warns and runs UNCAPPED there: %s", row)
	}
	if !regexp.MustCompile(`(?i)uncapped`).MatchString(row) {
		t.Errorf("docs/POLICIES.md's disk_mib row does not say the run proceeds UNCAPPED on a docker storage driver that cannot take a size quota — that is the outcome an operator has to plan for: %s", row)
	}
	if !regexp.MustCompile(`(?i)evict`).MatchString(row) {
		t.Errorf("docs/POLICIES.md's disk_mib row never says the k8s substrate enforces the cap by EVICTION — the kubelet kills the pod, it does not refuse the write, and a run that dies mid-work is what an operator is actually planning for: %s", row)
	}

	// The doc half. The THIRD outcome is the one the mainstream host gets:
	// overlay2's size storage-opt is an xfs project quota, so on overlay2 over
	// ext4 (Docker Desktop/WSL2, stock Ubuntu/Debian) the daemon refuses the
	// create and the run never starts. Anchored on the warning applyDiskQuota
	// emits for exactly that branch, so a change of posture re-opens this guard
	// rather than leaving the row quietly wrong.
	if !regexp.MustCompile(`(?i)needs xfs mounted with the pquota option`).MatchString(hardening) {
		t.Fatalf("internal/runner/docker/hardening.go no longer warns that overlay2 needs xfs+pquota — re-check what the disk cap does on a non-xfs overlay2 host and update docs/POLICIES.md's disk_mib row with it")
	}
	if !regexp.MustCompile(`(?i)\bxfs\b`).MatchString(row) {
		t.Errorf("docs/POLICIES.md's disk_mib row never names xfs, the one backing filesystem overlay2's size quota works on — an operator on the default ext4 host reads this row, sets disk_mib, and the daemon refuses every create: %s", row)
	}
}

// TestDocsOpsDirectoryEgressIsInTheDataFlowPage: the page designated as the
// answer to a vendor security questionnaire enumerated the daemon's outbound
// destinations as exhaustive, and omitted the identity-directory connector —
// the one place the daemon itself dials a hostname hard-coded in Wardyn's own
// source.
func TestDocsOpsDirectoryEgressIsInTheDataFlowPage(t *testing.T) {
	root := repoRoot(t)

	b, err := os.ReadFile(filepath.Join(root, "internal", "directory", "entra.go"))
	if err != nil {
		t.Fatalf("read internal/directory/entra.go: %v", err)
	}
	hosts := map[string]bool{}
	for _, m := range regexp.MustCompile(`https://([a-z0-9.-]+\.(?:com|net|org))`).FindAllStringSubmatch(string(b), -1) {
		hosts[m[1]] = true
	}
	if len(hosts) == 0 {
		t.Fatal("no literal https host found in internal/directory/entra.go — this guard is checking nothing")
	}

	doc := readOpsDoc(t, "docs", "DATA-FLOW.md")
	names := make([]string, 0, len(hosts))
	for h := range hosts {
		names = append(names, h)
	}
	sort.Strings(names)
	for _, h := range names {
		if !strings.Contains(doc, h) {
			t.Errorf("docs/DATA-FLOW.md's outbound-destination enumeration never names %s, which the daemon itself dials when WARDYN_DIRECTORY_PROVIDER is set — the page states its list as exhaustive", h)
		}
	}
}

// TestDocsOpsContinuousImageLaneDocumentedHonestly: docs/VERIFY.md opened
// "Every Wardyn image is signed, carries an SBOM you can read, and records how
// it was built" without qualification, while RELEASING.md called the same
// continuous lane "Unsigned." Both were wrong, in opposite directions:
// publish-image.yml signs keyless and attests nothing.
func TestDocsOpsContinuousImageLaneDocumentedHonestly(t *testing.T) {
	root := repoRoot(t)

	wf, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "publish-image.yml"))
	if err != nil {
		t.Fatalf("read .github/workflows/publish-image.yml: %v", err)
	}
	signs := regexp.MustCompile(`cosign sign\b`).Match(wf)
	attests := regexp.MustCompile(`cosign attest|attest-build-provenance`).Match(wf)

	releasing := readOpsDoc(t, "RELEASING.md")
	verify := readOpsDoc(t, "docs", "VERIFY.md")

	if signs && regexp.MustCompile(`publish-image\.yml(?s).{0,600}?\bUnsigned\.`).MatchString(releasing) {
		t.Error(`RELEASING.md still calls the continuous lane "Unsigned." — publish-image.yml runs cosign sign on the pushed digest`)
	}
	if !attests {
		if !strings.Contains(verify, "publish-image.yml") {
			t.Error("docs/VERIFY.md never mentions publish-image.yml — its :latest/:sha- tags carry no SBOM and no provenance, and the release identity regexp on that page structurally cannot verify them")
		}
		if regexp.MustCompile(`(?m)^Every Wardyn image is signed`).MatchString(verify) {
			t.Error("docs/VERIFY.md still opens with an unqualified \"Every Wardyn image is signed, carries an SBOM…\" — the continuous lane attests nothing")
		}
	}
}

// TestDocsOpsGDPRRowDoesNotPointAtExportControl: the docs index routed
// "Export or erase a run's data (GDPR-shaped requests)" to docs/EXPORT.md,
// which is an export-CONTROL page (ECCN 5D002, EAR) and contains the string
// "GDPR" exactly zero times.
func TestDocsOpsGDPRRowDoesNotPointAtExportControl(t *testing.T) {
	export := readOpsDoc(t, "docs", "EXPORT.md")
	if regexp.MustCompile(`(?i)gdpr|data subject`).MatchString(export) {
		t.Skip("docs/EXPORT.md now covers data-subject requests — the index may legitimately route there")
	}
	index := readOpsDoc(t, "docs", "README.md")
	for _, line := range strings.Split(index, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		if regexp.MustCompile(`(?i)gdpr|erase|data-subject|data subject`).MatchString(line) &&
			strings.Contains(line, "EXPORT.md") {
			t.Errorf("docs/README.md routes a data-subject question to EXPORT.md, which is about export control and never mentions GDPR: %s", line)
		}
	}
}

// TestDocsOpsAuditStreamTriggerClaimIsScoped: the front page said a Postgres
// trigger will not let you rewrite any of the three audit streams. Only
// audit_events has one — PTY casts live in `recordings`, which no CREATE
// TRIGGER in the schema covers and which the store upserts in place.
func TestDocsOpsAuditStreamTriggerClaimIsScoped(t *testing.T) {
	root := repoRoot(t)

	migDir := filepath.Join(root, "internal", "db", "migrations")
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	triggered := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(migDir, e.Name()))
		if rerr != nil {
			continue
		}
		for _, m := range regexp.MustCompile(`(?is)CREATE\s+TRIGGER\s+\w+.*?\sON\s+(\w+)`).FindAllStringSubmatch(string(b), -1) {
			triggered[strings.ToLower(m[1])] = true
		}
	}
	if !triggered["audit_events"] {
		t.Fatal("no CREATE TRIGGER on audit_events found in the migrations — this guard is checking nothing")
	}
	if triggered["recordings"] {
		t.Skip("recordings now carries a trigger too — the README's original three-stream claim would be true again")
	}

	readme := readOpsDoc(t, "README.md")
	for _, line := range strings.Split(readme, "\n") {
		if !strings.Contains(line, "Audit + attach") {
			continue
		}
		if regexp.MustCompile(`(?i)three append-only streams a postgres trigger`).MatchString(line) {
			t.Errorf("README.md still claims a Postgres trigger protects all three streams; only audit_events has one (recordings is upserted in place and retention-swept): %s", line)
		}
		return
	}
	t.Fatal("README.md has no \"Audit + attach\" capability row — re-anchor this guard")
}

// TestDocsOpsStatusSectionCarriesNoStaleVersion: the README's Status section is
// written version-free, so it cannot name an older release than the file's own
// install line pins. This keeps it that way, or forces any literal reintroduced
// there to match the shipped version.
func TestDocsOpsStatusSectionCarriesNoStaleVersion(t *testing.T) {
	root := repoRoot(t)

	vb, err := os.ReadFile(filepath.Join(root, "internal", "version", "version.go"))
	if err != nil {
		t.Fatalf("read internal/version/version.go: %v", err)
	}
	vm := regexp.MustCompile(`Version\s*=\s*"([^"]+)"`).FindStringSubmatch(string(vb))
	if vm == nil {
		t.Fatal("could not read the shipped Version const — re-anchor this guard")
	}
	shipped := vm[1]

	readme := readOpsDoc(t, "README.md")
	i := strings.Index(readme, "\n## Status")
	if i < 0 {
		t.Fatal("README.md has no \"## Status\" section — re-anchor this guard")
	}
	section := readme[i+1:]
	if j := strings.Index(section[1:], "\n## "); j >= 0 {
		section = section[:j+1]
	}
	for _, m := range regexp.MustCompile(`v?(\d+\.\d+\.\d+)`).FindAllStringSubmatch(section, -1) {
		if m[1] != shipped {
			t.Errorf("README.md's Status section names version %s, but the shipped version is %s — write it version-free, or bump it here too (RELEASING.md step 1b)", m[1], shipped)
		}
	}
}

// TestDocsOpsRestoreRunbookRestoresUserDrives: the Restore runbook had no
// user-drive step, though the Backup runbook backs drives up as step 4 and the
// section forbids reversing the backup order. Following it verbatim on a host
// rebuild brings back the drive ROWS (they are in the pg_dump) with none of the
// bytes, and the runner then creates a fresh empty volume on first use.
//
// The label set is derived from the hand-restore recipe the same document
// carries, so a label change lands here rather than silently in one of the two.
func TestDocsOpsRestoreRunbookRestoresUserDrives(t *testing.T) {
	ops := readOpsDoc(t, "docs", "OPERATIONS.md")

	i := strings.Index(ops, "\n### Restore them")
	if i < 0 {
		t.Fatal("docs/OPERATIONS.md has no \"### Restore them\" section — re-anchor this guard")
	}
	restore := ops[i+1:]
	if j := strings.Index(restore[1:], "\n### "); j >= 0 {
		restore = restore[:j+1]
	}

	// The labels the runner requires, read out of the doc's own docker-volume
	// create recipe rather than hard-coded here.
	recipe := regexp.MustCompile(`(?s)docker volume create.*?wardyn-drive-`).FindString(ops)
	if recipe == "" {
		t.Fatal("docs/OPERATIONS.md no longer carries the `docker volume create` hand-restore recipe — re-anchor this guard")
	}
	labels := regexp.MustCompile(`--label (wardyn\.[a-z]+)=`).FindAllStringSubmatch(recipe, -1)
	if len(labels) == 0 {
		t.Fatal("parsed no wardyn.* labels out of the hand-restore recipe — this guard is checking nothing")
	}
	for _, m := range labels {
		if !strings.Contains(restore, m[1]) {
			t.Errorf("the Restore runbook never mentions the %s label the hand-restore recipe requires — a rebuild that follows it verbatim brings back the drive rows with none of the bytes", m[1])
		}
	}
	if !strings.Contains(restore, "wardyn-drive-") {
		t.Error("the Restore runbook has no user-drive step at all, though Backup backs drives up and the section says the order must not simply be reversed")
	}
}

// TestDocsOpsExitCodeTableNamesTheLocalCatchAll: the exit-code taxonomy's `1`
// row described only a response the CLI could not classify, while exitCodeFor's
// bare `return 1` is the catch-all every LOCAL usage/validation failure lands on
// — unknown flag, malformed id, unreadable --policy-file. A pipeline branching
// on the table could not tell "your invocation was malformed and nothing ever
// ran" from "the run FAILED with an unreadable exit code".
func TestDocsOpsExitCodeTableNamesTheLocalCatchAll(t *testing.T) {
	root := repoRoot(t)

	main, err := os.ReadFile(filepath.Join(root, "cmd", "wardyn", "main.go"))
	if err != nil {
		t.Fatalf("read cmd/wardyn/main.go: %v", err)
	}
	body := regexp.MustCompile(`(?s)func exitCodeFor\(err error\) int \{.*?\n\}`).FindString(string(main))
	if body == "" {
		t.Fatal("could not find exitCodeFor in cmd/wardyn/main.go — re-anchor this guard")
	}
	// The catch-all is the LAST return in the function.
	returns := regexp.MustCompile(`return (\d+)`).FindAllStringSubmatch(body, -1)
	if len(returns) == 0 {
		t.Fatal("exitCodeFor returns no literal codes — re-anchor this guard")
	}
	if catchAll := returns[len(returns)-1][1]; catchAll != "1" {
		t.Skipf("exitCodeFor's catch-all is now %s, not 1 — re-check docs/CI.md's taxonomy against it", catchAll)
	}

	doc := readOpsDoc(t, "docs", "CI.md")
	row := ""
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "| `1` |") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatal("docs/CI.md's exit-code table has no `1` row — re-anchor this guard")
	}
	for _, want := range []string{"usage", "policy-file"} {
		if !strings.Contains(strings.ToLower(row), want) {
			t.Errorf("docs/CI.md's exit-1 row never mentions %q, but exitCodeFor's catch-all is what every local usage/validation failure returns: %s", want, row)
		}
	}
}

// TestDocsOpsRunAuditRowsCiteEveryEmitFile: the declared audit-vocabulary
// reference described ONE emit site each for run.create and run.build; there
// are eight and four, and the failure family carries Data fields, an outcome
// and (in one case) a Target that the documented set never mentioned. The emit
// files are derived by grepping the action literal out of internal/, so a new
// emit site in a new file goes red here.
func TestDocsOpsRunAuditRowsCiteEveryEmitFile(t *testing.T) {
	root := repoRoot(t)
	doc := readOpsDoc(t, "docs", "AUDIT-ACTIONS.md")

	for _, action := range []string{"run.create", "run.build"} {
		emitRe := regexp.MustCompile(`recordAudit\([^)]*"` + regexp.QuoteMeta(action) + `"`)
		files := map[string]bool{}
		err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, werr error) error {
			if werr != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return werr
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			// recordAudit's call spans lines; match on the whole file with the
			// action literal near the auditEvent constructor.
			if regexp.MustCompile(`auditEvent\((?s).{0,120}?"`+regexp.QuoteMeta(action)+`"`).Match(b) || emitRe.Match(b) {
				rel, _ := filepath.Rel(root, path)
				files[filepath.ToSlash(rel)] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk internal/: %v", err)
		}
		if len(files) == 0 {
			t.Fatalf("found no %s emit site under internal/ — this guard is checking nothing", action)
		}

		row := ""
		for _, line := range strings.Split(doc, "\n") {
			if strings.HasPrefix(line, "| `"+action+"` |") {
				row = line
				break
			}
		}
		if row == "" {
			t.Fatalf("docs/AUDIT-ACTIONS.md has no `%s` row — re-anchor this guard", action)
		}
		names := make([]string, 0, len(files))
		for f := range files {
			names = append(names, f)
		}
		sort.Strings(names)
		for _, f := range names {
			if !strings.Contains(row, f) {
				t.Errorf("docs/AUDIT-ACTIONS.md's `%s` row does not cite %s, which emits it — the row described one emit site where there are %d", action, f, len(files))
			}
		}
	}
}

// TestDocsOpsExternalAttributionsAreRight pins two attribution facts a reader cannot
// check without leaving the page:
//
//   - Kata Containers is not a CNCF-graduated or incubating project, so
//     PLUGGABILITY.md's governance due-diligence sentence must not list it
//     among them. Kata is an OpenInfra Foundation project and appears in the
//     CNCF directory at no maturity level.
//   - OPERATIONS.md's quotation about nested group membership is from Entra's
//     "Configure group claims for applications", not "Configure optional
//     claims" — that page's only nested-group sentence points the other way.
//
// Neither has an in-repo source to derive from, so these are literal guards: their
// job is to fail loudly, with the reason attached, if either claim comes back.
func TestDocsOpsExternalAttributionsAreRight(t *testing.T) {
	plug := readOpsDoc(t, "docs", "PLUGGABILITY.md")
	cncf := regexp.MustCompile(`CNCF-graduated/incubating where available \(([^)]*)\)`).FindStringSubmatch(plug)
	if cncf == nil {
		t.Fatal("docs/PLUGGABILITY.md no longer carries the CNCF-graduated/incubating parenthetical — re-anchor this guard")
	}
	if strings.Contains(cncf[1], "Kata") {
		t.Errorf("docs/PLUGGABILITY.md lists Kata inside the CNCF parenthetical (%q); Kata Containers is an OpenInfra Foundation project and is not a CNCF project at any maturity level", cncf[1])
	}

	ops := readOpsDoc(t, "docs", "OPERATIONS.md")
	i := strings.Index(ops, "nested groups are not included")
	if i < 0 {
		t.Fatal("docs/OPERATIONS.md no longer quotes Entra's nested-group sentence — re-anchor this guard")
	}
	window := ops[i:min(i+500, len(ops))]
	if strings.Contains(window, "identity-platform/optional-claims") {
		t.Error(`docs/OPERATIONS.md cites the quoted nested-group sentence to Entra's "Configure optional claims", which does not contain it — the sentence is on "Configure group claims for applications" (how-to-connect-fed-group-claims)`)
	}
	if !strings.Contains(window, "how-to-connect-fed-group-claims") {
		t.Error("docs/OPERATIONS.md's nested-group quotation is not cited to the page that carries it (how-to-connect-fed-group-claims)")
	}
}

// siteInternalHostsCIDRHintFromSource reads B7's frozen hint straight out of
// internal/egress/proxy/policy.go, so this guard fails the moment the doc's
// quote and the landed constant disagree instead of trusting a copy-paste.
func siteInternalHostsCIDRHintFromSource(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "internal", "egress", "proxy", "policy.go"))
	if err != nil {
		t.Fatalf("read internal/egress/proxy/policy.go: %v", err)
	}
	decl := regexp.MustCompile(`siteInternalHostsCIDRHint\s*=\s*((?:"(?:[^"\\]|\\.)*"\s*\+?\s*)+)`).
		FindSubmatch(b)
	if decl == nil {
		t.Fatal("internal/egress/proxy/policy.go no longer defines siteInternalHostsCIDRHint — re-anchor this guard")
	}
	var sb strings.Builder
	for _, lit := range regexp.MustCompile(`"(?:[^"\\]|\\.)*"`).FindAllString(string(decl[1]), -1) {
		s, err := strconv.Unquote(lit)
		if err != nil {
			t.Fatalf("unquote %q out of siteInternalHostsCIDRHint: %v", lit, err)
		}
		sb.WriteString(s)
	}
	return sb.String()
}

// networkSection extracts docs/OPERATIONS.md's "Network: upstream proxy and
// egress redirects" section (up to the next top-level heading) — the region
// B-δ owns and the only one this guard is allowed to read from.
func networkSection(t *testing.T, doc string) string {
	t.Helper()
	const heading = "## Network: upstream proxy and egress redirects"
	start := strings.Index(doc, heading)
	if start < 0 {
		t.Fatal("docs/OPERATIONS.md no longer has the Network section heading — re-anchor this guard")
	}
	rest := doc[start+len(heading):]
	end := strings.Index(rest, "\n## ")
	if end < 0 {
		t.Fatal("docs/OPERATIONS.md's Network section has no following top-level heading — re-anchor this guard")
	}
	return doc[start : start+len(heading)+end]
}

// flattenProse joins a blockquote's wrapped "> " lines back into one run of
// words, so a verbatim-quote check survives the doc's own line wrapping
// without caring where the markdown source happens to break a line.
func flattenProse(s string) string {
	var words []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), ">")
		words = append(words, strings.Fields(line)...)
	}
	return strings.Join(words, " ")
}

// networkSectionCIDRGuidanceGaps is the actual check, pulled out of the guard
// test so TestDocsOpsNetworkCIDRGuardActuallyReds can prove it fires on real
// drift instead of only ever passing.
func networkSectionCIDRGuidanceGaps(section, hint string) []string {
	var gaps []string
	lower := strings.ToLower(section)
	if !strings.Contains(lower, "leave `cidrs` empty") || !strings.Contains(lower, "is the default") {
		gaps = append(gaps, "no sentence saying an empty cidrs is the default")
	}
	if !strings.Contains(flattenProse(section), flattenProse(hint)) {
		gaps = append(gaps, "does not quote siteInternalHostsCIDRHint verbatim")
	}
	return gaps
}

// TestDocsOpsNetworkSectionInvertsTheCIDRSDefault: the Network section must
// not tell an operator to "declare tight cidrs" — the ranges an operator's
// own machine resolves for a private endpoint are not the ranges the sandbox
// resolves into, so that advice fails runs. The doc must say the opposite
// (empty cidrs is the default) and must quote the 403's own hint verbatim,
// not a paraphrase that can drift from it.
func TestDocsOpsNetworkSectionInvertsTheCIDRSDefault(t *testing.T) {
	root := repoRoot(t)
	section := networkSection(t, readOpsDoc(t, "docs", "OPERATIONS.md"))
	hint := siteInternalHostsCIDRHintFromSource(t, root)
	if gaps := networkSectionCIDRGuidanceGaps(section, hint); len(gaps) > 0 {
		t.Errorf("docs/OPERATIONS.md's Network section is missing: %v", gaps)
	}
}

// TestDocsOpsNetworkCIDRGuardActuallyReds mutates a scratch copy of the real
// section to prove networkSectionCIDRGuidanceGaps is not vacuous — each piece
// it checks for, once removed, must turn up as a gap.
func TestDocsOpsNetworkCIDRGuardActuallyReds(t *testing.T) {
	root := repoRoot(t)
	section := networkSection(t, readOpsDoc(t, "docs", "OPERATIONS.md"))
	hint := siteInternalHostsCIDRHintFromSource(t, root)
	if gaps := networkSectionCIDRGuidanceGaps(section, hint); len(gaps) != 0 {
		t.Fatalf("guard already reds against the real doc (%v) — fix the doc before trusting this mutation test", gaps)
	}

	withoutDefault := strings.Replace(section,
		"**Leave `cidrs` empty — that is the default, and it is the right one.**", "", 1)
	if section == withoutDefault {
		t.Fatal("the empty-cidrs default sentence is not in the section verbatim — re-anchor this mutation")
	}
	if gaps := networkSectionCIDRGuidanceGaps(withoutDefault, hint); len(gaps) == 0 {
		t.Error("removing the empty-cidrs default sentence did not red the guard")
	}

	i := strings.Index(section, "> Leave `cidrs` empty")
	j := strings.Index(section, "cluster sees.")
	if i < 0 || j < 0 || j < i {
		t.Fatal("the quoted siteInternalHostsCIDRHint blockquote is not in the section as expected — re-anchor this mutation")
	}
	withoutHint := section[:i] + section[j+len("cluster sees."):]
	if gaps := networkSectionCIDRGuidanceGaps(withoutHint, hint); len(gaps) == 0 {
		t.Error("removing the quoted siteInternalHostsCIDRHint text did not red the guard")
	}
}
