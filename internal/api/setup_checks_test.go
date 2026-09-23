// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/setup"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// M5: the golden-id test (setup_check_ids_test.go) pins that these two rows
// EXIST under one fixture, but an id alone doesn't pin its Status — a check
// that always returned "warn" (or always "ok") would pass that test
// unnoticed. These table tests assert Status (and presence) directly against
// the pure check functions, no HTTP/Server plumbing required.

func TestSsoRBACCheck(t *testing.T) {
	cases := []struct {
		name              string
		oidcConfigured    bool
		roleMapConfigured bool
		consoleRows       bool
		wantOK            bool
		wantStatus        string
	}{
		{"OIDC off: absent regardless of role map", false, true, false, false, ""},
		{"OIDC off, everything unset: still absent", false, false, false, false, ""},
		{"OIDC on, chart map set: ok", true, true, false, true, "ok"},
		{"OIDC on, chart+console both unset: warn", true, false, false, true, "warn"},
		// The widened case this signature exists for: no chart map, but the
		// People step has at least one console row — still ok, not warn.
		{"OIDC on, env unset + console rows: ok", true, false, true, true, "ok"},
		{"OIDC on, chart AND console both set: still ok", true, true, true, true, "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk, ok := ssoRBACCheck(tc.oidcConfigured, tc.roleMapConfigured, tc.consoleRows)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if chk.ID != "sso_rbac" {
				t.Errorf("ID = %q, want %q", chk.ID, "sso_rbac")
			}
			if chk.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", chk.Status, tc.wantStatus)
			}
			if tc.wantStatus == "warn" && chk.Fix == "" {
				t.Error("warn status must carry a Fix")
			}
		})
	}
}

func TestConfinementFloorCheck(t *testing.T) {
	cases := []struct {
		name       string
		classes    []string
		driver     string
		floor      types.ConfinementClass
		wantOK     bool
		wantStatus string
	}{
		{"no floor configured: absent regardless of classes", []string{"CC1"}, "docker", "", false, ""},
		{"no advertised classes: absent (runnerCheck already owns that failure)", nil, "docker", types.CC2, false, ""},
		{"floor advertised (CC1 always is): absent", []string{"CC1", "CC2"}, "docker", types.CC1, false, ""},
		{"floor advertised exactly: absent", []string{"CC1", "CC2"}, "docker", types.CC2, false, ""},
		{"floor NOT advertised: warn", []string{"CC1"}, "docker", types.CC2, true, "warn"},
		// M8: a Kata-only host advertises [CC1, CC3], no CC2 — membership, not
		// rank, so a CC3-outranks-CC2 argument must NOT suppress this row.
		{"Kata-only host [CC1,CC3] with a CC2 floor: warn, rank does not save it", []string{"CC1", "CC3"}, "docker", types.CC2, true, "warn"},
		{"k8s driver with an unadvertised floor: warn, RuntimeClass fix", []string{"CC1"}, "k8s", types.CC2, true, "warn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk, ok := confinementFloorCheck(SetupRunner{Driver: tc.driver, ConfinementClasses: tc.classes}, tc.floor)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (chk=%+v)", ok, tc.wantOK, chk)
			}
			if !ok {
				return
			}
			if chk.ID != "confinement_floor" {
				t.Errorf("ID = %q, want confinement_floor", chk.ID)
			}
			if chk.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", chk.Status, tc.wantStatus)
			}
			if chk.Fix == "" {
				t.Error("warn status must carry a Fix")
			}
			if tc.driver == "k8s" && !strings.Contains(chk.Fix, "RuntimeClass") {
				t.Errorf("Fix = %q, want it to name the k8s RuntimeClass lever", chk.Fix)
			}
			if !strings.Contains(chk.Detail, string(tc.floor)) {
				t.Errorf("Detail = %q, want it to name the configured floor", chk.Detail)
			}
		})
	}
}

func TestTlsCookiePostureCheck(t *testing.T) {
	cases := []struct {
		name           string
		oidcConfigured bool
		redirectURL    string
		secureCookies  bool
		wantOK         bool
		wantStatus     string
	}{
		{"OIDC off: absent", false, "https://wardyn.example.com/auth/callback", false, false, ""},
		{"http redirect: absent (nothing to warn about)", true, "http://localhost/auth/callback", false, false, ""},
		{"http redirect even with secureCookies true: absent", true, "http://localhost/auth/callback", true, false, ""},
		{"https + secureCookies true: ok", true, "https://wardyn.example.com/auth/callback", true, true, "ok"},
		{"https + secureCookies false: warn (the ingress footgun)", true, "https://wardyn.example.com/auth/callback", false, true, "warn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk, ok := tlsCookiePostureCheck(tc.oidcConfigured, tc.redirectURL, tc.secureCookies)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if chk.ID != "tls_cookie_posture" {
				t.Errorf("ID = %q, want %q", chk.ID, "tls_cookie_posture")
			}
			if chk.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", chk.Status, tc.wantStatus)
			}
			if tc.wantStatus == "warn" && chk.Fix == "" {
				t.Error("warn status must carry a Fix")
			}
		})
	}
}

// TestK8sEgressContainmentCheck: absent on every non-k8s driver (docker's L0
// structural containment has no analogous row); on a k8s driver, FAIL for
// both "unenforced" (the opt-out is a standing risk acceptance, never a
// success) and "" (indeterminate — an old daemon build, or in principle any
// unproven state; see the field's doc for why a genuinely indeterminate LIVE
// canary can never reach here). Never confuses Indeterminate with Enforcing.
// TestSiteConfigCheck_DanglingSecretRef pins W26-S1-2: on base 763beb5,
// siteConfigCheck graded "info" ("every run inherits it") purely off whether
// UpstreamProxySecretRef/EgressRedirects/ScmHosts were SET — never whether the
// secret they name is actually present. After the documented reset+apply
// recovery (`wardyn site-config get > f` before a reset, `wardyn site-config
// apply f` after) with the referenced secret never restored, that read as
// fully configured while the credentialed path was dead. It must now grade
// "warn" and name the missing secret.
func TestSiteConfigCheck_DanglingSecretRef(t *testing.T) {
	cases := []struct {
		name       string
		sc         types.SiteConfig
		present    map[string]bool
		wantStatus string
		wantNames  []string // must all appear in Detail when wantStatus == "warn"
	}{
		{"unconfigured", types.SiteConfig{}, nil, "info", nil},
		{
			"upstream proxy secret present: ok",
			types.SiteConfig{UpstreamProxySecretRef: "corp-proxy-url"},
			map[string]bool{"corp-proxy-url": true},
			"info", nil,
		},
		{
			"upstream proxy secret DANGLING: warn",
			types.SiteConfig{UpstreamProxySecretRef: "corp-proxy-url"},
			map[string]bool{},
			"warn", []string{"corp-proxy-url"},
		},
		{
			"egress redirect token secret DANGLING: warn",
			types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "ghcr.io", To: "registry.corp.internal/ghcr-remote", TokenSecretRef: "ghcr-token"},
			}},
			map[string]bool{},
			"warn", []string{"ghcr-token"},
		},
		{
			"plain upstream_proxy_url (no secret ref) never dangles",
			types.SiteConfig{UpstreamProxyURL: "http://proxy.corp:3128"},
			map[string]bool{},
			"info", nil,
		},
		{
			"scm hosts only, no secret refs at all: ok",
			types.SiteConfig{ScmHosts: []string{"dev.azure.com"}},
			map[string]bool{},
			"info", nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk := siteConfigCheck(tc.sc, tc.present)
			if chk.Status != tc.wantStatus {
				t.Fatalf("Status = %q, want %q (detail=%q)", chk.Status, tc.wantStatus, chk.Detail)
			}
			for _, name := range tc.wantNames {
				if !strings.Contains(chk.Detail, name) {
					t.Errorf("Detail = %q, want it to name dangling secret %q", chk.Detail, name)
				}
			}
			if tc.wantStatus == "warn" && chk.Fix == "" {
				t.Error("warn status must carry a Fix")
			}
		})
	}
}

// TestSiteConfigCheck_InternalHostsOnlyIsNotUnconfigured is B7-F5: a document
// declaring ONLY InternalHosts (the one override that LIFTS the proxy's
// private/reserved-IP SSRF guard) used to read as "No operator-wide site
// config yet (optional)" — the emptiness test never looked at InternalHosts,
// UpstreamProxyNoProxy or WorkspaceProviders.
func TestSiteConfigCheck_InternalHostsOnlyIsNotUnconfigured(t *testing.T) {
	cases := []struct {
		name string
		sc   types.SiteConfig
	}{
		{"internal_hosts only", types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "svc.cluster.local"}}}},
		{"upstream_proxy_no_proxy only", types.SiteConfig{UpstreamProxyNoProxy: []string{"169.254.169.254"}}},
		{"workspace_providers only (all rows disabled)", types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{
			Git: []types.GitProvider{{ID: "gh", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://github.example.com"}, Disabled: true}},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk := siteConfigCheck(tc.sc, nil)
			if strings.Contains(chk.Detail, "No operator-wide site config yet") {
				t.Errorf("%s: siteConfigCheck still reads as unconfigured: %q", tc.name, chk.Detail)
			}
		})
	}
}

// TestInternalHostsCheck is B7-F5's dedicated row: absent when InternalHosts
// is empty, present and "info" (never warn/fail — the neg control: the setup
// gate only fires on warn/fail, so an install with nothing else configured
// stays gate-inactive with only this row) when it is set, and its Detail is
// EXACTLY the sentence the write-time log already carries — one claim, two
// surfaces.
func TestInternalHostsCheck(t *testing.T) {
	if _, ok := internalHostsCheck(types.SiteConfig{}); ok {
		t.Error("internalHostsCheck present with no InternalHosts declared")
	}
	hosts := []types.InternalHost{{HostSuffix: "svc.cluster.local"}}
	chk, ok := internalHostsCheck(types.SiteConfig{InternalHosts: hosts})
	if !ok {
		t.Fatal("internalHostsCheck absent with InternalHosts declared")
	}
	if chk.ID != "internal_hosts" {
		t.Errorf("ID = %q, want internal_hosts", chk.ID)
	}
	if chk.Label != internalHostsCheckLabel {
		t.Errorf("Label = %q, want the DRAFT constant %q", chk.Label, internalHostsCheckLabel)
	}
	if chk.Status != "info" {
		t.Errorf("Status = %q, want info (a fail/warn here would wrongly activate the setup gate on a deliberate, "+
			"Liftable-validated declaration)", chk.Status)
	}
	if want := internalHostsDeclaredSentence(hosts); chk.Detail != want {
		t.Errorf("Detail = %q, want the shared sentence %q", chk.Detail, want)
	}
}

// TestArtifactRepoCheck_NoBareEcosystemsClauseWhenNetworkOnly is B7-F10:
// every redirect network-only used to render "(ecosystems: ; 2
// network-only)" — a bare, truncated-looking clause. The ecosystems: segment
// must be OMITTED, not empty, when there are no ecosystem-tagged rows.
func TestArtifactRepoCheck_NoBareEcosystemsClauseWhenNetworkOnly(t *testing.T) {
	chk := artifactRepoCheck(types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "registry.corp.example", To: "10.40.2.11:8443"},
		{From: "mirror.corp.example", To: "10.40.2.12:8443"},
	}})
	if strings.Contains(chk.Detail, "ecosystems: ;") || strings.Contains(chk.Detail, "ecosystems: ,") {
		t.Errorf("Detail = %q, still carries a bare ecosystems clause", chk.Detail)
	}
	if !strings.Contains(chk.Detail, "2 network-only") {
		t.Errorf("Detail = %q, want it to still count the network-only rows", chk.Detail)
	}
	// The populated case must keep naming its ecosystems — this is an
	// omission fix, not a removal of the clause entirely.
	withEco := artifactRepoCheck(types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/api/npm/npm-remote/", Ecosystem: "npm"},
	}})
	if !strings.Contains(withEco.Detail, "ecosystems: npm") {
		t.Errorf("Detail = %q, want the ecosystems clause preserved when non-empty", withEco.Detail)
	}
}

func TestK8sEgressContainmentCheck(t *testing.T) {
	cases := []struct {
		name             string
		driver           string
		netpolProven     string
		wantOK           bool
		wantStatus       string
		wantFixHasOptOut bool // the fix string must name the opt-out env var (unenforced only)
	}{
		{"docker driver: absent regardless of the field", "docker", "enforced", false, "", false},
		{"no driver: absent", "none", "", false, "", false},
		{"k8s enforced: ok, no fix needed", "k8s", "enforced", true, "ok", false},
		{"k8s unenforced (opted out): fail, fix names the opt-out to unset", "k8s", "unenforced", true, "fail", true},
		{"k8s indeterminate (field absent): fail, never reads as enforcing", "k8s", "", true, "fail", false},
		// B1: acknowledged is neither ok (never proven — phase B never ran)
		// nor fail (the operator made an informed call on a managed cluster,
		// which is a warn, not a red the checklist blocks on).
		{"k8s acknowledged (B1): warn, never fail or ok", "k8s", "acknowledged", true, "warn", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk, ok := k8sEgressContainmentCheck(tc.driver, tc.netpolProven)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if chk.ID != "k8s_egress_containment" {
				t.Errorf("ID = %q, want %q", chk.ID, "k8s_egress_containment")
			}
			if chk.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", chk.Status, tc.wantStatus)
			}
			if (chk.Status == "fail" || chk.Status == "warn") && chk.Fix == "" {
				t.Errorf("%s status must carry a Fix", chk.Status)
			}
			if strings.Contains(chk.Detail, "Enforcing") && chk.Status != "ok" {
				t.Errorf("a non-ok row must never claim Enforcing in its Detail: %q", chk.Detail)
			}
			hasOptOut := strings.Contains(chk.Fix, "WARDYN_K8S_ALLOW_UNENFORCED_NETPOL")
			if hasOptOut != tc.wantFixHasOptOut {
				t.Errorf("fix mentions the opt-out env var = %v, want %v (fix: %q)", hasOptOut, tc.wantFixHasOptOut, chk.Fix)
			}
			// Every dual-form env-var mention carries the helm form too.
			if strings.Contains(chk.Fix, "WARDYN_") && !strings.Contains(chk.Fix, "helm: env.") {
				t.Errorf("fix mentions a WARDYN_ env var with no dual helm form: %q", chk.Fix)
			}
		})
	}
}

// TestK8sEgressContainmentCheck_Acknowledged asserts the B1 row's exact
// content: never claims proof, names the ack env, and its Fix points at the
// real remedy (exempt Wardyn's pods, then unset the env) rather than just
// suppressing the warning.
func TestK8sEgressContainmentCheck_Acknowledged(t *testing.T) {
	chk, ok := k8sEgressContainmentCheck("k8s", "acknowledged")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if chk.Status != "warn" {
		t.Errorf("Status = %q, want warn", chk.Status)
	}
	if !strings.Contains(chk.Detail, "not proven") {
		t.Errorf("Detail = %q, want it to say the canary never proved enforcement", chk.Detail)
	}
	if !strings.Contains(chk.Detail, "WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY") {
		t.Errorf("Detail = %q, want it to name the ack env", chk.Detail)
	}
	if !strings.Contains(chk.Fix, "wardyn.managed") || !strings.Contains(chk.Fix, "WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY") {
		t.Errorf("Fix = %q, want it to name both the exemption selector and the env to unset", chk.Fix)
	}
}

// TestRunnerCheckCC1OnlyFixIsDriverAware (W4-S1-5/W27-S1-4): a CC1-only host's
// Fix used to unconditionally read "run `wardyn setup wall` (or `wardyn setup
// vault`)" — a DOCKER host command that means nothing on a k8s runner, where
// the actual lever is pinning a cluster-registered RuntimeClass via Helm
// (k8s.runtimeClasses.CC2/.CC3). The docker driver keeps the original command;
// only k8s swaps to the Helm-shaped fix.
func TestRunnerCheckCC1OnlyFixIsDriverAware(t *testing.T) {
	cases := []struct {
		name       string
		driver     string
		wantWardyn bool // fix names the `wardyn setup wall/vault` docker command
		wantHelm   bool // fix names k8s.runtimeClasses via the Helm pin command
	}{
		{"docker driver: the host-side `wardyn setup` command", "docker", true, false},
		{"k8s driver: the Helm RuntimeClass pin, never the docker command", "k8s", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk := runnerCheck(SetupRunner{Driver: tc.driver, ConfinementClasses: []string{"CC1"}})
			if chk.Status != "info" {
				t.Fatalf("Status = %q, want %q", chk.Status, "info")
			}
			hasWardyn := strings.Contains(chk.Fix, "wardyn setup wall")
			hasHelm := strings.Contains(chk.Fix, "k8s.runtimeClasses")
			if hasWardyn != tc.wantWardyn {
				t.Errorf("fix names `wardyn setup wall` = %v, want %v (fix: %q)", hasWardyn, tc.wantWardyn, chk.Fix)
			}
			if hasHelm != tc.wantHelm {
				t.Errorf("fix names k8s.runtimeClasses = %v, want %v (fix: %q)", hasHelm, tc.wantHelm, chk.Fix)
			}
		})
	}
}

// TestPermissionsPostureCheck is the #19b regression: the row must always be
// "info" (a posture choice, never a misconfiguration to warn/fail about — an
// operator may legitimately leave every kind fail-open) and must name exactly
// which of the four capability kinds are enforced vs left at the fail-open
// default, for every point on that 0-to-4 spectrum.
func TestPermissionsPostureCheck(t *testing.T) {
	cases := []struct {
		name        string
		enforcement map[string]bool
		wantOn      []string
		wantOff     []string
	}{
		{"nil map: all four fail-open (zero-config default)", nil,
			nil, []string{capEgressHost, capSecret, capWorkspace, capImage}},
		{"all four enforced", map[string]bool{capEgressHost: true, capSecret: true, capWorkspace: true, capImage: true},
			[]string{capEgressHost, capSecret, capWorkspace, capImage}, nil},
		{"mixed", map[string]bool{capEgressHost: true, capWorkspace: true},
			[]string{capEgressHost, capWorkspace}, []string{capSecret, capImage}},
		{"an explicit false is still fail-open (not merely absent)",
			map[string]bool{capEgressHost: true, capSecret: false, capWorkspace: false, capImage: false},
			[]string{capEgressHost}, []string{capSecret, capWorkspace, capImage}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chk := permissionsPostureCheck(tc.enforcement)
			if chk.ID != "permissions_posture" {
				t.Errorf("ID = %q, want permissions_posture", chk.ID)
			}
			if chk.Status != "info" {
				t.Errorf("Status = %q, want info (a posture choice, never warn/fail)", chk.Status)
			}
			for _, k := range tc.wantOn {
				if !strings.Contains(chk.Detail, k) {
					t.Errorf("Detail = %q, want it to name enforced kind %q", chk.Detail, k)
				}
			}
			for _, k := range tc.wantOff {
				if !strings.Contains(chk.Detail, k) {
					t.Errorf("Detail = %q, want it to name fail-open kind %q", chk.Detail, k)
				}
			}
		})
	}
}

// helmUpgradeWithArgs matches a `helm [-n ns] upgrade <release> <chart>`
// invocation: `upgrade` followed by TWO positional arguments, neither of them a
// flag. That is not a style preference — `helm upgrade` genuinely requires 2
// arguments and exits with that message when given none, so a Fix hint printing
// the bare form hands the operator a command that cannot run.
var helmUpgradeWithArgs = regexp.MustCompile(`helm(?:\s+-n\s+\S+)?\s+upgrade\s+[^-\s]\S*\s+[^-\s]\S*`)

// assertHelmFixRunnable is the R5 F236 (deduped twin ADV3-05) invariant, applied
// to one SetupCheck.Fix. Three rules, each one a shape a reviewer found shipped:
//
//  1. never the bare `helm upgrade --set` — it exits "requires 2 arguments";
//  2. a Helm `--set` must re-pass the install's values (`-f …`) or ask Helm to
//     do it (`--reset-then-reuse-values`). docs/OPERATIONS.md § "`helm upgrade`,
//     and why `--wait` is not optional" documents why: a bare `--set` resets
//     every OTHER value to chart defaults, dropping auth.adminToken.*,
//     k8s.proxyImage, serviceAccount.automount and secrets.ageKeyFromSecret —
//     the chart then refuses the render, so the operator who repairs the missing
//     arguments by hand walks into a second, more confusing failure;
//  3. the invocation names a release and a chart, so it runs as printed.
func assertHelmFixRunnable(t *testing.T, where, fix string) {
	t.Helper()
	if !strings.Contains(fix, "helm") || !strings.Contains(fix, "--set") {
		return
	}
	if strings.Contains(fix, "helm upgrade --set") {
		t.Errorf("%s: Fix prints `helm upgrade --set …`, which exits \"requires 2 arguments\" — name the release and chart (`helm -n <namespace> upgrade <release> ./deploy/helm/wardyn …`). Fix = %q", where, fix)
	}
	if !strings.Contains(fix, "-f ") && !strings.Contains(fix, "--reset-then-reuse-values") {
		t.Errorf("%s: Fix passes `--set` with neither `-f <values>` nor `--reset-then-reuse-values` — that is the shape docs/OPERATIONS.md documents as resetting every other value to chart defaults (dropping auth.adminToken.*, k8s.proxyImage, serviceAccount.automount, secrets.ageKeyFromSecret). Fix = %q", where, fix)
	}
	if !helmUpgradeWithArgs.MatchString(fix) {
		t.Errorf("%s: Fix names a Helm --set but no `upgrade <release> <chart>` pair — the command cannot run as printed. Fix = %q", where, fix)
	}
}

// TestSetupFixHelmCommandsAreRunnable sweeps every Fix the checklist can compose
// on a Kubernetes runner — both through the pure check functions (which need a
// driver/floor combination no single fixture produces) and across the whole live
// GET /setup/status payload, so a Helm hint added to some FUTURE check is
// covered by the same invariant rather than needing its own test.
func TestSetupFixHelmCommandsAreRunnable(t *testing.T) {
	helmFixes := 0
	sweep := func(where, fix string) {
		if strings.Contains(fix, "helm") && strings.Contains(fix, "--set") {
			helmFixes++
		}
		assertHelmFixRunnable(t, where, fix)
	}

	// The two driver-conditional arms, called directly.
	sweep("runnerCheck(k8s, CC1-only)",
		runnerCheck(SetupRunner{Driver: "k8s", ConfinementClasses: []string{"CC1"}}).Fix)
	floorChk, ok := confinementFloorCheck(SetupRunner{Driver: "k8s", ConfinementClasses: []string{"CC1"}}, types.CC2)
	if !ok {
		t.Fatal("confinementFloorCheck(k8s, CC1-only, CC2 floor) emitted no row — the fixture no longer reaches the Helm arm this guard exists for")
	}
	sweep("confinementFloorCheck(k8s, CC2 floor)", floorChk.Fix)

	// ...and the whole wire payload, on a fixture that puts a k8s runner and an
	// unadvertised floor in play at once.
	srv := New(Config{
		AdminToken:    adminToken,
		Runner:        k8sRunner{networkPolicy: true},
		DefaultPolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
	})
	code, st := decodeSetup(t, srv, adminToken)
	if code != 200 {
		t.Fatalf("GET /setup/status: code = %d, want 200", code)
	}
	for _, c := range st.Checks {
		sweep("check "+c.ID, c.Fix)
	}

	// Guard the guard: an invariant that never sees a Helm string passes
	// vacuously, which is exactly how the bare `--set` survived to a release
	// candidate.
	if helmFixes < 2 {
		t.Fatalf("swept only %d Helm Fix strings — the k8s fixtures no longer reach the Helm hints; re-point this guard rather than letting it pass on nothing", helmFixes)
	}
}

// TestAgeKeyCheckFixSteersToASecretBackedKey is R5 F159/F190. The warn arm's Fix
// used to offer `helm: env.WARDYN_AGE_KEY` — which renders the secret store's
// MASTER key as a plaintext literal in the Deployment object, readable by
// anything with `get deploy` and captured in every `helm get manifest`. The
// chart has two Secret-backed doors (deploy/helm/wardyn/values.yaml's
// secrets.ageKeyFromSecret over the postgres.dsn.secretRef Secret's `age-key`
// entry, and secrets.ageKeySecretRef.name for a separate Secret), and the
// console's own remedy has to name them.
// In store mode there is no local key to be durable: the age-key row gives
// way to store_external, which names the store (design §3).
func TestSecretStoreCheck_StoreModeReplacesTheAgeKeyRow(t *testing.T) {
	chks := secretStoreChecks("Vault at vault.example:8200", "", true, true)
	if len(chks) != 1 || chks[0].ID != "store_external" || chks[0].Status != "ok" || !strings.Contains(chks[0].Detail, "Vault at vault.example:8200") {
		t.Fatalf("store mode rows = %+v", chks)
	}
	if got := secretStoreChecks("", "", false, false); len(got) != 1 || got[0].ID != "age_key" || got[0].Status != "warn" {
		t.Fatalf("local mode rows = %+v, want the age-key warning unchanged", got)
	}
}

// A key service replaces the age-key row; the local key on a multi-user
// install adds the amber kek_local row (design §3, K3), and a single-user one
// does not.
func TestSecretStoreChecks_KeyServiceAndLocalKey(t *testing.T) {
	chks := secretStoreChecks("", "Vault Transit at vault.example:8200", true, true)
	if len(chks) != 1 || chks[0].ID != "kek_service" || chks[0].Status != "ok" || !strings.Contains(chks[0].Detail, "Vault Transit at vault.example:8200") {
		t.Fatalf("key service rows = %+v", chks)
	}
	chks = secretStoreChecks("", "", true, true)
	if len(chks) != 2 || chks[0].ID != "age_key" || chks[1].ID != "kek_local" || chks[1].Status != "warn" ||
		chks[1].Detail != "Credentials are encrypted with a key this deployment holds. Anyone with both the database and that key can read them. Connect a key service to keep the two apart." {
		t.Fatalf("multi-user local key rows = %+v", chks)
	}
	if got := secretStoreChecks("", "", true, false); len(got) != 1 || got[0].ID != "age_key" {
		t.Fatalf("single-user local key rows = %+v, want the age-key row alone", got)
	}
}

func TestAgeKeyCheckFixSteersToASecretBackedKey(t *testing.T) {
	if fix := ageKeyCheck(true).Fix; fix != "" {
		t.Errorf("durable arm carries a Fix (%q) — an ok row has nothing to fix", fix)
	}

	chk := ageKeyCheck(false)
	if chk.Status != "warn" {
		t.Fatalf("Status = %q, want warn", chk.Status)
	}
	// The two Secret-backed chart keys, named as the Helm answer.
	for _, want := range []string{"secrets.ageKeyFromSecret", "secrets.ageKeySecretRef"} {
		if !strings.Contains(chk.Fix, want) {
			t.Errorf("Fix does not name the Secret-backed chart key %q — Fix = %q", want, chk.Fix)
		}
	}
	// The plaintext-literal door is not offered as the Helm answer. Naming it as
	// the thing NOT to do is the point (an operator who already wired it needs to
	// recognise it), so the assertion is on the `helm: env.…` STEERING shape the
	// old string used, not on the substring appearing at all.
	if strings.Contains(chk.Fix, "helm: env.WARDYN_AGE_KEY") {
		t.Errorf("Fix still steers the master key into env.WARDYN_AGE_KEY, a plaintext literal in the Deployment — Fix = %q", chk.Fix)
	}
	if !strings.Contains(chk.Fix, "plaintext") {
		t.Errorf("Fix does not say WHY env.WARDYN_AGE_KEY is refused (a plaintext literal in the Deployment); \"not X\" with no reason is how the old advice survived three reviews — Fix = %q", chk.Fix)
	}
	// The host answer stays: on a bare binary there is no Deployment object to
	// leak into, and -age-key / the env var is what compose and install.sh write.
	if !strings.Contains(chk.Fix, "-age-key") {
		t.Errorf("Fix dropped the host-side -age-key answer — Fix = %q", chk.Fix)
	}
}

// ── finding 3: bedrock_provider / llm_provider under a per-principal caller ──

// bedrockRowVia is bedrockProviderCheck fed the SAME setupBedrock a real
// request would compute for scope — real per_user zeroing included — so
// these tests pin the end-to-end behaviour, not a hand-built SetupBedrock the
// production code path would never actually produce.
func bedrockRowVia(t *testing.T, scope awsSSOScope) SetupCheck {
	t.Helper()
	srv := New(Config{
		BedrockRegion: "us-east-1", BedrockModel: "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Secrets: &memSecrets{m: map[string][]byte{}},
	})
	bedrock := srv.setupBedrock(context.Background(), map[string]bool{}, scope)
	chk, ok := bedrockProviderCheck(bedrock, types.SiteConfig{}, true)
	if !ok {
		t.Fatal("a region+model-configured Bedrock row must always surface a check")
	}
	return chk
}

// TestBedrockProviderCheck_PerUserNamesOnlyTheSignIn is finding 3: a per_user
// admin with no session of their own must be told to sign in, not offered the
// three operator-only remedies per_user resolution skips outright (the bearer,
// host-~/.aws-mount and static-key arms).
func TestBedrockProviderCheck_PerUserNamesOnlyTheSignIn(t *testing.T) {
	chk := bedrockRowVia(t, awsSSOScope{perUser: true, owner: "member-x"})
	if chk.Status != "warn" {
		t.Errorf("status = %q, want warn (a real person has something to do)", chk.Status)
	}
	if strings.Contains(chk.Fix, "-bedrock-aws-dir") {
		t.Errorf("fix = %q, must not offer the operator-only ~/.aws mount flag to a per-user caller", chk.Fix)
	}
	if !strings.Contains(chk.Fix, "Sign in to AWS") {
		t.Errorf("fix = %q, want it to name the sign-in", chk.Fix)
	}
	if chk.Detail != bedrockPerUserDetail {
		t.Errorf("detail = %q, want the per_user DRAFT sentence verbatim", chk.Detail)
	}
}

// TestBedrockProviderCheck_MechanismPrincipalIsInfoNotWarn is finding 5's
// other half: the shared admin token under a per_user row cannot act on this
// row, so it must never read as a warning the operator will chase forever.
func TestBedrockProviderCheck_MechanismPrincipalIsInfoNotWarn(t *testing.T) {
	chk := bedrockRowVia(t, awsSSOScope{perUser: true, owner: adminTokenPrincipal})
	if chk.Status != "info" {
		t.Errorf("status = %q, want info", chk.Status)
	}
	if chk.Detail != bedrockMechanismDetail || chk.Fix != bedrockMechanismFix {
		t.Errorf("mechanism row text drifted: %+v", chk)
	}
}

// TestBedrockProviderCheck_SharedRowUnchanged pins the load-bearing regression:
// the ORIGINAL shared-row text, byte-for-byte, through the per_user/mechanism
// refactor.
func TestBedrockProviderCheck_SharedRowUnchanged(t *testing.T) {
	chk := bedrockRowVia(t, awsSSOScope{})
	want := SetupCheck{
		ID: "bedrock_provider", Label: "AWS Bedrock", Status: "warn",
		Detail: "Bedrock is partially configured; runs will NOT use it until this is complete.",
		Fix:    "Still needed: a credential — a read-only ~/.aws mount (-bedrock-aws-dir), a bedrock-api-key bearer secret, a container AWS SSO login, or aws-access-key-id + aws-secret-access-key secrets.",
	}
	if chk != want {
		t.Errorf("shared-row text drifted:\n got  %+v\n want %+v", chk, want)
	}
}

// TestLLMProviderCheck_NoBedrockRowUnchanged pins the OTHER load-bearing
// regression: an install with no Bedrock row at all keeps today's exact
// optional-provider sentence.
func TestLLMProviderCheck_NoBedrockRowUnchanged(t *testing.T) {
	got := llmProviderCheck("", SetupBedrock{})
	want := SetupCheck{
		ID: "llm_provider", Label: "LLM access", Status: "info",
		Detail: "No model/harness provider configured (optional): needed only for agent-harness runs. Bring-your-own-container and interactive runs work without one.",
		Fix:    "Optional — connect a Claude subscription/API key or Bedrock (Settings → Model provider, or the \"Secrets\" setup step), or bind creds to a workspace/container.",
	}
	if got != want {
		t.Errorf("no-bedrock-row text drifted:\n got  %+v\n want %+v", got, want)
	}
}

// TestLLMProviderCheck_PerUserBedrockRowDoesNotSayNoProviderConfigured is
// finding 3's second contradiction: llm_provider must not claim "no provider
// configured" while bedrock_provider (fed the same bedrock value) says Bedrock
// IS configured.
func TestLLMProviderCheck_PerUserBedrockRowDoesNotSayNoProviderConfigured(t *testing.T) {
	b := SetupBedrock{Region: "us-east-1", Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0", PerUser: true}
	got := llmProviderCheck("", b)
	if strings.Contains(got.Detail, "No model/harness provider configured") {
		t.Errorf("detail = %q, must not claim no provider when Bedrock IS configured per_user", got.Detail)
	}
	if got.Detail != llmProviderPerUserDetail || got.Fix != llmProviderPerUserFix {
		t.Errorf("detail/fix = %+v, want the per_user DRAFT sentence verbatim", got)
	}
}

// TestLLMProviderCheck_MechanismPrincipalIsInfo mirrors the Bedrock row's
// info-not-warn rule for the same caller.
func TestLLMProviderCheck_MechanismPrincipalIsInfo(t *testing.T) {
	b := SetupBedrock{Region: "us-east-1", Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0", PerUser: true, Mechanism: true}
	got := llmProviderCheck("", b)
	if got.Status != "info" {
		t.Errorf("status = %q, want info", got.Status)
	}
	if got.Detail != llmProviderMechanismDetail {
		t.Errorf("detail = %q, want the mechanism DRAFT sentence verbatim", got.Detail)
	}
}

// ------------------------------------------------------------------------
// Blocking — 0.7.8: the daemon marks the console-confiscating rows itself
// (SetupCheck.Blocking) instead of the console guessing from a hard-coded id
// set. Exactly three arms may ever set it; every other row, at every status
// it can carry, must not.
// ------------------------------------------------------------------------

// setupCheckBlockingStatus names, for each of the three ids that can ever set
// Blocking, the ONE status that does. The same id at any OTHER status
// (sso_rbac's "ok", runner's "info"/"ok") must read Blocking=false, exactly
// like every id in setupCheckNeverBlocks below — there is no id-level
// exemption, only a (id, status) one.
var setupCheckBlockingStatus = map[string]string{
	"runner":            "fail", // no live confinement class: runs cannot launch at all
	"confinement_floor": "warn", // every run on the default policy refused before launch
	"sso_rbac":          "warn", // no role mapping: every SSO user is an admin
}

// setupCheckNeverBlocks is every OTHER id /setup/status can emit. NOT the
// golden fixture's 14 ids (setup_check_ids_golden.json's six fixtures never
// set DefaultPolicy, so confinement_floor never appears there at all) — this
// is the full inventory, read off every `ID: "..."` literal in setup.go,
// setup_checks.go and modelaccess.go. assertSetupCheckBlocking is strict
// about an id in NEITHER table on purpose: a check added later with no
// Blocking decision recorded here must fail the build, not default quietly
// to non-blocking.
var setupCheckNeverBlocks = map[string]bool{
	"env_builder": true, "k8s_egress_containment": true, "age_key": true, "store_external": true,
	"kek_service": true, "kek_local": true,
	"site_config": true, "internal_hosts": true, "tls_cookie_posture": true,
	"scm_provider": true, "host_proxy": true, "artifact_repo": true,
	"permissions_posture": true, "llm_provider": true, "bedrock_provider": true,
	"claude_subscription_staging": true, "agent_image": true,
	"harness_credential": true, "harness_credential_aws": true,
	"github_ref_ruleset": true, "platform_wsl": true, "platform_macos": true,
}

// assertSetupCheckBlocking is the one gate every case in TestSetupCheckBlocking
// runs through.
func assertSetupCheckBlocking(t *testing.T, chk SetupCheck) {
	t.Helper()
	if wantStatus, known := setupCheckBlockingStatus[chk.ID]; known {
		want := chk.Status == wantStatus
		if chk.Blocking != want {
			t.Errorf("%s (status=%s): Blocking = %v, want %v", chk.ID, chk.Status, chk.Blocking, want)
		}
		return
	}
	if !setupCheckNeverBlocks[chk.ID] {
		t.Fatalf("check id %q is in neither setupCheckBlockingStatus nor setupCheckNeverBlocks — "+
			"record a Blocking decision for it in one of those two tables, don't let it default silently", chk.ID)
	}
	if chk.Blocking {
		t.Errorf("%s (status=%s): Blocking = true, want false — only the three console-confiscating arms may set it", chk.ID, chk.Status)
	}
}

// TestSetupCheckBlocking calls the pure check constructors directly (not a
// golden-style HTTP fixture matrix — that route can't reach confinement_floor,
// which needs a DefaultPolicy no golden fixture sets) across every arm/status
// each one can produce, and asserts Blocking through assertSetupCheckBlocking.
func TestSetupCheckBlocking(t *testing.T) {
	// The three blocking-capable ids: the blocking arm, and their other arms.
	assertSetupCheckBlocking(t, runnerCheck(SetupRunner{Driver: "none"}))
	assertSetupCheckBlocking(t, runnerCheck(SetupRunner{Driver: "docker", ConfinementClasses: []string{"CC1"}}))
	assertSetupCheckBlocking(t, runnerCheck(SetupRunner{Driver: "docker", ConfinementClasses: []string{"CC1", "CC2"}}))

	if chk, ok := confinementFloorCheck(SetupRunner{Driver: "docker", ConfinementClasses: []string{"CC1"}}, types.CC2); ok {
		assertSetupCheckBlocking(t, chk)
	} else {
		t.Fatal("confinementFloorCheck absent, want a floor-mismatch row")
	}

	if chk, ok := ssoRBACCheck(true, false, false); ok {
		assertSetupCheckBlocking(t, chk)
	} else {
		t.Fatal("ssoRBACCheck absent")
	}
	if chk, ok := ssoRBACCheck(true, true, false); ok {
		assertSetupCheckBlocking(t, chk)
	} else {
		t.Fatal("ssoRBACCheck absent")
	}

	// Every remaining id: false at every status it can carry.
	assertSetupCheckBlocking(t, envBuilderCheck(true))
	assertSetupCheckBlocking(t, envBuilderCheck(false))

	for _, netpol := range []string{"enforced", "acknowledged", "unenforced", ""} {
		chk, ok := k8sEgressContainmentCheck("k8s", netpol)
		if !ok {
			t.Fatalf("k8sEgressContainmentCheck(%q) absent", netpol)
		}
		assertSetupCheckBlocking(t, chk)
	}

	assertSetupCheckBlocking(t, ageKeyCheck(true))
	assertSetupCheckBlocking(t, ageKeyCheck(false))
	for _, chks := range [][]SetupCheck{
		secretStoreChecks("Vault at vault.example:8200", "", true, true),
		secretStoreChecks("", "Vault Transit at vault.example:8200", true, true),
		secretStoreChecks("", "", true, true),
	} {
		for _, chk := range chks {
			assertSetupCheckBlocking(t, chk)
		}
	}

	assertSetupCheckBlocking(t, siteConfigCheck(types.SiteConfig{}, nil))
	assertSetupCheckBlocking(t, siteConfigCheck(types.SiteConfig{UpstreamProxySecretRef: "x"}, map[string]bool{}))

	if chk, ok := internalHostsCheck(types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "svc.cluster.local"}}}); ok {
		assertSetupCheckBlocking(t, chk)
	} else {
		t.Fatal("internalHostsCheck absent")
	}

	if chk, ok := tlsCookiePostureCheck(true, "https://wardyn.example.com/auth/callback", false); ok {
		assertSetupCheckBlocking(t, chk)
	} else {
		t.Fatal("tlsCookiePostureCheck absent")
	}
	if chk, ok := tlsCookiePostureCheck(true, "https://wardyn.example.com/auth/callback", true); ok {
		assertSetupCheckBlocking(t, chk)
	} else {
		t.Fatal("tlsCookiePostureCheck absent")
	}

	assertSetupCheckBlocking(t, scmProviderCheck(false, nil, setup.SCMPosture{}))
	assertSetupCheckBlocking(t, scmProviderCheck(true, nil, setup.SCMPosture{}))
	assertSetupCheckBlocking(t, scmProviderCheck(false, []string{"ssh-key-github-com"}, setup.SCMPosture{}))

	assertSetupCheckBlocking(t, hostProxyCheck(setup.HostProxyDetection{}, false))
	assertSetupCheckBlocking(t, hostProxyCheck(setup.HostProxyDetection{}, true))

	assertSetupCheckBlocking(t, artifactRepoCheck(types.SiteConfig{}))

	assertSetupCheckBlocking(t, permissionsPostureCheck(nil))

	assertSetupCheckBlocking(t, llmProviderCheck("a model provider is connected", SetupBedrock{}))
	assertSetupCheckBlocking(t, llmProviderCheck("", SetupBedrock{}))
	// per_user warn — one of the four per-person rows; must stay non-blocking.
	assertSetupCheckBlocking(t, llmProviderCheck("", SetupBedrock{Region: "us-east-1", Model: "m", PerUser: true}))

	if chk, ok := bedrockProviderRow(SetupBedrock{Region: "us-east-1", Model: "m", CredsPresent: true}); ok {
		assertSetupCheckBlocking(t, chk)
	} else {
		t.Fatal("bedrockProviderRow absent")
	}
	// per_user warn — the other of the four per-person rows.
	if chk, ok := bedrockProviderRow(SetupBedrock{Region: "us-east-1", Model: "m", PerUser: true}); ok {
		assertSetupCheckBlocking(t, chk)
	} else {
		t.Fatal("bedrockProviderRow absent")
	}
	if chk, ok := bedrockProviderRow(SetupBedrock{Region: "us-east-1", Model: "m", Mechanism: true}); ok {
		assertSetupCheckBlocking(t, chk)
	} else {
		t.Fatal("bedrockProviderRow absent")
	}

	if chk, ok := claudeSubscriptionStagingCheck(true, false, "~/.claude/.credentials.json"); ok {
		assertSetupCheckBlocking(t, chk)
	} else {
		t.Fatal("claudeSubscriptionStagingCheck absent")
	}
	if chk, ok := claudeSubscriptionStagingCheck(true, true, ""); ok {
		assertSetupCheckBlocking(t, chk)
	} else {
		t.Fatal("claudeSubscriptionStagingCheck absent")
	}

	assertSetupCheckBlocking(t, agentImageCheck(nil))

	if chk, ok := harnessCredentialCheck(SetupHarness{Captured: true, Provider: "anthropic", Aging: true}, SetupModelAccess{}); ok {
		assertSetupCheckBlocking(t, chk)
	} else {
		t.Fatal("harnessCredentialCheck absent")
	}
	if chk, ok := harnessCredentialCheck(SetupHarness{Captured: true, Provider: "anthropic"}, SetupModelAccess{}); ok {
		assertSetupCheckBlocking(t, chk)
	} else {
		t.Fatal("harnessCredentialCheck absent")
	}
	// harness_credential_aws — the 0.7.6 field report's own row: a lapsed
	// admin AWS SSO session grades warn here and must never gate.
	if chk, ok := harnessCredentialCheck(
		SetupHarness{Captured: true, Provider: awsSSOProvider, ExpiresAt: "2020-01-01T00:00:00Z"},
		SetupModelAccess{State: modelAccessExpiredSignin},
	); ok {
		assertSetupCheckBlocking(t, chk)
	} else {
		t.Fatal("harnessCredentialCheck (aws) absent")
	}

	assertSetupCheckBlocking(t, refRulesetCheck("acme/widgets", false, "detail", nil))
	assertSetupCheckBlocking(t, refRulesetCheck("acme/widgets", true, "detail", nil))
}
