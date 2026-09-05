// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── the walled-dispatch harness ──────────────────────────────────────────────

// govCeilingSecret is the operator-stored artifact-registry token the corp
// mirror's redirect injects, and govCorpDeny the wildcard an assigned profile
// walls the corp estate off with.
const (
	govCeilingSecret = "corp-registry-token"
	govCorpDeny      = "*.corp.example"
	govCorpMirror    = "mirror.corp.example"
)

// ceilingDispatchStore is dispatchTestStore with the two reads the widening
// phases need: an operator site-config carrying an egress redirect, and a
// CreateGrant that succeeds so planArtifactRedirect really authors the corp
// token injection this phase then has to drop.
type ceilingDispatchStore struct {
	*dispatchTestStore
	site types.SiteConfig
}

func (s ceilingDispatchStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.site, nil
}

func (s ceilingDispatchStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	return g, nil
}

// walledDispatch is one dispatch driven straight at dispatchRun, with the
// ceiling arriving the way every lane delivers it: as the required
// dispatchCeiling argument.
//
// DRIVEN DIRECTLY, DELIBERATELY. Each lane's own job is to RESOLVE the ceiling
// and hand it over (ceilingForDispatch / resolveDispatchCeiling); this harness's
// job is the phase that consumes it, which is where every widening that defeats
// a create-time deny actually happens. Both halves are pinned, in the lane that
// owns each.
type walledDispatch struct {
	deny       []string // the ASSIGNED profile's denies; nil = an unassigned principal
	policy     types.RunPolicySpec
	site       types.SiteConfig
	gitGrants  map[string]uuid.UUID
	patGrants  map[string]string
	injections []runner.InjectionGrant
}

// runWalledDispatch dispatches one run and returns the run.policy.effective
// envelope (the defined post-widening truth), the SandboxSpec the runner
// actually received (where injections and broker lanes live — the policy
// envelope cannot see them), and the audit trail.
func runWalledDispatch(t *testing.T, d walledDispatch) (types.RunPolicySpec, runner.SandboxSpec, []types.AuditEvent, uuid.UUID) {
	t.Helper()
	fr := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	srv.cfg.Store = ceilingDispatchStore{dispatchTestStore: st, site: d.site}
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{govCeilingSecret: []byte("v")}}
	run.Task = "" // no agent exec / completion watcher: this is about composition

	var firstGitHub *uuid.UUID
	for _, id := range d.gitGrants {
		gid := id
		firstGitHub = &gid
		break
	}
	ceiling := ceilingForDispatch(governanceCeiling{})
	if len(d.deny) > 0 {
		ceiling = ceilingForDispatch(governanceCeiling{
			Spec:    types.RunPolicySpec{DeniedDomains: d.deny},
			Profile: &types.GovernanceProfile{Name: "walled"},
		})
	}
	srv.dispatchRun(context.Background(), run, ceiling, dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy:             d.policy,
		FirstGitHubGrantID: firstGitHub,
		GitGrants:          d.gitGrants,
		GitPATGrants:       d.patGrants,
		// The never-resident PAT lane rides ProxyConfig.PATGrants with NOTHING
		// reaching the sandbox — precisely the lane that bypasses
		// denied_domains. It is no longer a dispatchParams field: dispatchRun
		// derives it from Config.DisableGitPATBroker, which this fixture leaves
		// at its zero value, i.e. the broker ON (the documented default).
		Injections: d.injections,
	})

	ev := findAudit(audit.events, run.ID, "run.policy.effective", "success")
	if ev == nil {
		t.Fatalf("dispatch recorded no run.policy.effective envelope; events=%s", auditDump(audit.events, run.ID))
	}
	var spec types.RunPolicySpec
	if err := json.Unmarshal(ev.Data, &spec); err != nil {
		t.Fatalf("envelope is not a RunPolicySpec: %v (%s)", err, ev.Data)
	}
	return spec, fr.lastSpec, audit.events, run.ID
}

// govMirrorSite is the operator site-config that makes a dispatch WIDEN: the
// npm redirect substitutes the corp mirror into this run's allowlist and
// authors a proxy-side token injection for it, both after create returned.
func govMirrorSite() types.SiteConfig {
	return types.SiteConfig{EgressRedirects: []types.EgressRedirect{{
		From: "https://registry.npmjs.org/", To: "https://" + govCorpMirror,
		Ecosystem: "npm", TokenSecretRef: govCeilingSecret,
	}}}
}

// injectionHosts is the set of hosts the proxy would inject a credential onto.
func injectionHosts(spec runner.SandboxSpec) []string {
	out := make([]string, 0, len(spec.ProxyConfig.Injection))
	for _, in := range spec.ProxyConfig.Injection {
		out = append(out, in.Rule.Host)
	}
	return out
}

// ─── the ordering argument, as a test ─────────────────────────────────────────

// TestCeilingReassertion_RunsBelowEveryWideningPhase is the placement proof.
//
// The phase sits after confineGitBrokerEgress and after every phase that WIDENS
// the run. Hoist it anywhere above the artifact block and this test goes red in
// the way that matters: the deny-union would still be visible in the envelope
// (denies are monotone — a later phase only adds hosts, it never removes a
// deny), so the DENY assertion alone would not notice. The injection would.
// planArtifactRedirect authors the corp mirror's token injection at dispatch
// time, and an injection authored after the phase is one the ceiling never
// sees: the sandbox gets the operator's registry token on a host its own
// profile walls off, or — because buildInjector fails CLOSED on a rule whose
// host is denied — the proxy refuses to boot and the run dies with no egress at
// all and nothing saying why.
func TestCeilingReassertion_RunsBelowEveryWideningPhase(t *testing.T) {
	envelope, spec, events, runID := runWalledDispatch(t, walledDispatch{
		deny: []string{govCorpDeny},
		site: govMirrorSite(),
		policy: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com", "registry.npmjs.org"},
			MinConfinementClass: types.CC1,
		},
	})

	// The widening really happened — without it this test proves nothing.
	if !slices.Contains(envelope.AllowedDomains, govCorpMirror) {
		t.Fatalf("the artifact redirect did not widen this run (allowed=%v); the fixture, not the phase, is broken", envelope.AllowedDomains)
	}
	// (1) the ceiling's wall is present on the post-widening envelope.
	if !slices.Contains(envelope.DeniedDomains, govCorpDeny) {
		t.Errorf("denied_domains = %v — the profile's wall is missing from the envelope the proxy enforces", envelope.DeniedDomains)
	}
	// (2) the injection authored INSIDE dispatch for a now-denied host is gone.
	if hosts := injectionHosts(spec); slices.Contains(hosts, govCorpMirror) {
		t.Errorf("injection hosts = %v — the corp registry token still injects onto a host this principal's profile denies", hosts)
	}
	// (3) allows are NEVER re-intersected: the operator's substitution survives.
	// Deny beats allow at the proxy, so the wall holds without shredding an
	// operator-declared widening — that is the MEET, and it is the reason this
	// phase can be applied to a fully-composed run at all.
	if !slices.Contains(spec.ProxyConfig.Policy.AllowedDomains, govCorpMirror) {
		t.Errorf("allowed_domains = %v — the phase re-intersected allows; it must union denies only", spec.ProxyConfig.Policy.AllowedDomains)
	}
	// (4) and the drop is disclosed, not silent.
	ev := findAudit(events, runID, "run.ceiling.reassert", "success")
	if ev == nil {
		t.Fatalf("no run.ceiling.reassert event; the drop is invisible (the policy envelope cannot show injections)")
	}
	if !slices.Contains(auditStrings(t, ev.Data, "dropped_injection_hosts"), govCorpMirror) {
		t.Errorf("run.ceiling.reassert does not name the dropped injection host: %s", ev.Data)
	}
}

// auditStrings pulls one []string field out of an audit event's JSON payload.
func auditStrings(t *testing.T, data json.RawMessage, field string) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("audit data is not an object: %v (%s)", err, data)
	}
	var out []string
	if raw, ok := m[field]; ok {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("audit field %q is not a string list: %v (%s)", field, err, raw)
		}
	}
	return out
}

// ─── the absent-row doctrine ──────────────────────────────────────────────────

// TestCeilingReassertion_UnassignedIsAProvableNoOp is the other half of the
// feature, and the one that decides whether it can ship: a deployment that has
// never authored a governance profile must behave byte-for-byte as it did
// before this phase existed. Not "approximately unchanged" — zero denies added,
// zero injections dropped, zero broker lanes withheld, and not even an audit
// event, on a dispatch whose composition is otherwise IDENTICAL to the walled
// one above.
//
// Counterfactual: gate the phase on anything other than an assigned profile's
// deny list (the run's own merged policy.DeniedDomains being the tempting one)
// and this goes red at the broker lane — confineGitBrokerEgress appends
// github.com to DeniedDomains on every brokered run, so a merged-list matcher
// would withhold the git credential on a deployment with no profiles at all.
func TestCeilingReassertion_UnassignedIsAProvableNoOp(t *testing.T) {
	grantID := uuid.New()
	envelope, spec, events, runID := runWalledDispatch(t, walledDispatch{
		deny: nil, // NO assignment: the whole point
		site: govMirrorSite(),
		policy: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com", "registry.npmjs.org", "github.com"},
			MinConfinementClass: types.CC1,
		},
		gitGrants: map[string]uuid.UUID{"octocat/hello-world": grantID},
		patGrants: map[string]string{"git.corp.example": uuid.NewString()},
	})

	if slices.Contains(envelope.DeniedDomains, govCorpDeny) {
		t.Errorf("denied_domains = %v — an unassigned principal's run gained a wall from nowhere", envelope.DeniedDomains)
	}
	if hosts := injectionHosts(spec); !slices.Contains(hosts, govCorpMirror) {
		t.Errorf("injection hosts = %v — the operator's corp registry token was dropped from an UNASSIGNED run", hosts)
	}
	if len(spec.ProxyConfig.GitGrants) != 1 {
		t.Errorf("git broker grants = %v — an unassigned run lost its brokered git lane", spec.ProxyConfig.GitGrants)
	}
	if len(spec.ProxyConfig.PATGrants) != 1 {
		t.Errorf("PAT broker grants = %v — an unassigned run lost its brokered PAT lane", spec.ProxyConfig.PATGrants)
	}
	if spec.Env["WARDYN_GITHUB_GRANT_ID"] != grantID.String() {
		t.Errorf("WARDYN_GITHUB_GRANT_ID = %q, want the grant id — the phase unset it with no profile in play", spec.Env["WARDYN_GITHUB_GRANT_ID"])
	}
	if ev := findAudit(events, runID, "run.ceiling.reassert", "success"); ev != nil {
		t.Errorf("an unassigned run emitted a ceiling event: %s", ev.Data)
	}
}

// ─── the meet, and the matcher ────────────────────────────────────────────────

// TestUnionCeilingDeniesIsAMeet pins the two properties the union has to have:
// the run's OWN denies survive (it narrows, it never replaces), and a ceiling
// entry that is STRONGER than one the run already carries still lands. The
// second is the trap confineGitBrokerEgress documents for its own dedupe: key
// on the bare host and a run denying "corp.example:443" would swallow the
// ceiling's port-less "corp.example", leaving every other port open under
// allow_all_egress.
func TestUnionCeilingDeniesIsAMeet(t *testing.T) {
	policy := types.RunPolicySpec{DeniedDomains: []string{"evil.example", "corp.example:443"}}
	added := unionCeilingDenies(&policy, []string{"corp.example", "EVIL.EXAMPLE", govCorpDeny})

	for _, want := range []string{"evil.example", "corp.example:443", "corp.example", govCorpDeny} {
		if !slices.Contains(policy.DeniedDomains, want) {
			t.Errorf("denied_domains = %v, missing %q", policy.DeniedDomains, want)
		}
	}
	if len(added) != 2 {
		t.Errorf("added = %v, want exactly the two entries the run did not already carry (the case-folded duplicate is not one)", added)
	}
}

// TestCeilingDenies walks the matcher against the shapes a profile really
// writes. It uses the CEILING's list alone — never the run's merged
// denied_domains, which dispatch legitimately pollutes.
func TestCeilingDenies(t *testing.T) {
	deny := []string{"corp.internal", "*.corp.example", "gitlab.corp.io:443"}
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"corp.internal", true},
		{"CORP.INTERNAL.", true},     // proxy normalization: lowercase, trailing dot
		{"sub.corp.internal", false}, // an exact deny is not a suffix deny
		{"git.corp.example", true},   // wildcard
		{"corp.example", false},      // "*.x" does not cover bare x — matchWild's own rule
		{"gitlab.corp.io", true},     // a port-qualified ceiling entry still walls the host
		{"api.anthropic.com", false}, // the ordinary case: nothing matches
		{"github.com:443", false},    // ports on the CANDIDATE are stripped before matching
	} {
		if got := ceilingDenies(deny, tc.host); got != tc.want {
			t.Errorf("ceilingDenies(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// ─── the record lane (PF-24) ──────────────────────────────────────────────────

// ceilingRecordStore is recordLLMModeStore that also answers the governance
// resolver — the reads a MEMBER-or-security-admin principal reaches (an
// operator short-circuits at effectiveCeiling's step 1 and never gets here).
type ceilingRecordStore struct {
	*recordLLMModeStore
	profile *types.GovernanceProfile
}

func (s ceilingRecordStore) ResolveGovernanceProfile(context.Context, []string, []string) (*types.GovernanceProfile, types.CapabilitySubjectType, error) {
	return s.profile, types.CapabilitySubjectUser, nil
}

func (s ceilingRecordStore) HasGroupTierAssignments(context.Context) (bool, error) {
	return false, nil
}

// TestLaunchRecordRun_ThreadsActingPrincipalCeiling is PF-24, and it exists
// because Phase 3 moved POST /workspaces/{id}/record onto the SECURITY tier.
//
// launchRecordRun builds its OWN spec: AllowAllEgress = !confined, operator
// credential injections, and no member clamp anywhere. All of that is
// deliberate and stays — an open sandbox is what Record Mode's capture IS. What
// cannot stand beside it is a profile-walled security admin asking the server
// for an allow-all credentialed sandbox and then attaching to it. Their own
// ceiling's denies have to ride into dispatch, where deny beats allow_all at
// the proxy.
//
// Counterfactual: pass ceilingForDispatch(governanceCeiling{}) instead of the record
// lane's dispatchParams and the run comes up allow-all with the walled host
// wide open — a create-time clamp cannot help, because this lane never passes
// through one.
func TestLaunchRecordRun_ThreadsActingPrincipalCeiling(t *testing.T) {
	h := newHarness(t)
	ws := types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned}
	fr := &fakeRunner{}
	profile := govProfile("walled")
	profile.Ceiling.DeniedDomains = []string{govCorpDeny}
	// FIXTURE CORRECTION, not a weakening: this test is about the DENY axis
	// riding into dispatch, and govProfile carries Limits{DenyInteractive:true}
	// incidentally for the create-path tests it was written for. A record
	// session is always interactive, so once the Limits axis binds this lane
	// (recordCeilingLimits) that stray limit refuses the launch before the
	// dispatch spec this test reads is ever built. Clearing it states the
	// fixture's real intent; the assertion below is unchanged and still fails if
	// the deny stops riding along.
	profile.Limits = types.GovernanceLimits{}
	cfg := baseTestConfig(h, ceilingRecordStore{recordLLMModeStore: newRecordLLMModeStore(ws), profile: profile})
	cfg.Runner = fr
	cfg.Broker = h.broker
	srv := New(cfg)

	// A signed-in MEMBER-shaped principal: isOperator is false, so the ceiling
	// really resolves. (govMemberCtx is the same context shape the resolver's
	// own precedence table drives.)
	if _, _, err := srv.launchRecordRun(govMemberCtx([]string{"eng"}, false),
		"walled@corp.example", ws, "build", "build", false); err != nil {
		t.Fatalf("launchRecordRun: %v", err)
	}

	got := fr.lastSpec.ProxyConfig.Policy
	if !got.AllowAllEgress {
		t.Fatalf("the record session is no longer allow-all; this test must prove the deny binds DESPITE that posture, not instead of it")
	}
	if !slices.Contains(got.DeniedDomains, govCorpDeny) {
		t.Errorf("denied_domains = %v — a profile-walled principal got a server-authored allow-all sandbox with their own walls missing", got.DeniedDomains)
	}
}

// TestLaunchRecordRun_UnassignedPrincipalIsUnchanged is the moat half: Record
// Mode's learning posture for everyone who is not walled. No assignment ⇒ no
// denies ⇒ the session is exactly what it was before this thread existed.
func TestLaunchRecordRun_UnassignedPrincipalIsUnchanged(t *testing.T) {
	h := newHarness(t)
	ws := types.Workspace{ID: uuid.New(), Status: types.WorkspaceScanned}
	fr := &fakeRunner{}
	cfg := baseTestConfig(h, ceilingRecordStore{recordLLMModeStore: newRecordLLMModeStore(ws)}) // nil profile
	cfg.Runner = fr
	cfg.Broker = h.broker
	srv := New(cfg)

	if _, _, err := srv.launchRecordRun(govMemberCtx([]string{"eng"}, false),
		"alice@corp.example", ws, "build", "build", false); err != nil {
		t.Fatalf("launchRecordRun: %v", err)
	}
	got := fr.lastSpec.ProxyConfig.Policy
	if !got.AllowAllEgress || len(got.DeniedDomains) != 0 {
		t.Errorf("an unassigned principal's record session changed shape: allow_all=%v denied=%v", got.AllowAllEgress, got.DeniedDomains)
	}
}
