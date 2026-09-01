// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The dispatch-time half of a governance profile: the phase that re-asserts an
// ASSIGNED ceiling's DENY set over a run dispatch has finished composing.
//
// WHY THIS EXISTS, in one sentence: a create-time deny is not sufficient,
// because dispatch legitimately WIDENS the run after create returned. The
// artifact-redirect phase substitutes a corporate mirror into the allowlist and
// authors a token injection for it (runs_dispatch.go, artifact_redirect.go);
// the LLM transport widens egress for Bedrock; subscription and Bedrock bearer
// injections are appended after that. So a profile that denied a corporate host
// at create time gets that host added back — with a credential on it — by a
// later phase, and the "phases narrow only" contract at the top of dispatchRun
// is true of the git-broker phase alone.
//
// Split out of runs_dispatch.go for the 1000-line file-size gate
// (scripts/check-file-size.sh), the same way the broker and mount halves were.

// ceilingDispatchDenies is the ONE translation from a resolved ceiling into the
// two dispatchParams fields, so "absent row ⇒ absent behaviour" cannot be
// re-decided per call site.
//
// It returns something only for an ASSIGNED profile. That is the whole scoping
// rule (§A): a member with no assignment resolves to Config.DefaultPolicy, whose
// denies are ALREADY in their run's policy by every ordinary path — re-asserting
// them would be a no-op on a good day and a behaviour change on a bad one, on
// deployments that have never authored a profile. An operator short-circuits
// earlier still, at effectiveCeiling's step 1.
func ceilingDispatchDenies(c governanceCeiling) (deny []string, profile string) {
	if c.Profile == nil {
		return nil, ""
	}
	return c.Spec.DeniedDomains, c.Profile.Name
}

// ceilingDenies reports whether the CEILING's deny list covers host — an exact
// bare host, a "*." wildcard entry, or either carrying a ":port" qualifier.
//
// THE LIST IS THE CEILING'S ALONE, and that is not a detail. A matcher compiled
// from the run's merged policy.DeniedDomains would be wrong in the most
// expensive direction: dispatch pollutes that list on purpose —
// confineGitBrokerEgress appends github.com and the forge's SSH host to EVERY
// brokered run (runs_dispatch_gitbroker.go), appendNetworkRedirectDenials adds
// each redirect's From host — so a merged-list predicate would drop every
// broker lane and every corp-mirror injection on a deployment with no
// governance profile at all. Same recipe the archived Fleet plan's
// reconcileFleetInjections converged on, for the same reason.
//
// Matching reuses entryCoversAny (artifact_redirect.go) so the wildcard rule has
// ONE definition in this package and cannot drift from the substitution's.
//
// A PORT-QUALIFIED ceiling entry ("gitlab.corp.io:443") matches the bare host
// here, which is deliberately stronger than the proxy's per-request rule: a
// credential is not port-scoped, so a lane or an injection that presents one to
// a host the ceiling walls off on ANY port is withheld. The deny UNION keeps the
// operator's entry verbatim, so what the proxy evaluates per request is
// unchanged — only the credential decision is the conservative one.
func ceilingDenies(deny []string, host string) bool {
	h := map[string]bool{egressEntryHost(host): true}
	for _, d := range deny {
		if entryCoversAny(d, h) {
			return true
		}
	}
	return false
}

// ceilingDeniesAny is ceilingDenies over a set of hosts — "does the ceiling wall
// off ANY host this lane's traffic goes to".
func ceilingDeniesAny(deny []string, hosts []string) bool {
	return slices.ContainsFunc(hosts, func(h string) bool { return ceilingDenies(deny, h) })
}

// unionCeilingDenies appends the ceiling's denies the run does not already
// carry and returns what it added.
//
// A MEET, NOT A REPLACE. The run's own denies survive untouched (they are the
// operator's, or the broker phase's, and both are narrower-is-better), and no
// ALLOW is re-intersected — an operator-declared workspace widening still works,
// it simply cannot cross a wall the profile put up. Deny beats allow AND beats
// allow_all_egress at the proxy (Policy.evalHost), which is what makes the
// union the load-bearing half rather than cosmetic.
//
// Keyed on the RAW normalized entry, never egressEntryHost — exactly as
// confineGitBrokerEgress's own dedupe is, and for the same reason: a run that
// already denies "corp.example:443" must still get the ceiling's stronger bare
// "corp.example", which covers every port. Copies before appending so the
// caller's backing array is never aliased.
func unionCeilingDenies(policy *types.RunPolicySpec, deny []string) []string {
	have := make(map[string]bool, len(policy.DeniedDomains))
	for _, d := range policy.DeniedDomains {
		have[strings.ToLower(strings.TrimSpace(d))] = true
	}
	var add []string
	for _, d := range deny {
		key := strings.ToLower(strings.TrimSpace(d))
		if key == "" || have[key] {
			continue
		}
		have[key] = true
		add = append(add, d)
	}
	if len(add) > 0 {
		policy.DeniedDomains = append(append([]string(nil), policy.DeniedDomains...), add...)
	}
	return add
}

// reassertCeilingDenies re-applies the acting principal's governance-profile
// denies to a fully-composed run. Three effects:
//
//  1. UNION the ceiling's denies into policy.DeniedDomains (unionCeilingDenies).
//
//  2. DROP every injection rule whose host the ceiling denies. Without this the
//     run does not merely keep an unwanted injection, it DIES OPAQUELY:
//     buildInjector rejects any rule whose host fails AllowedExactHost — which
//     checks deny FIRST — and the proxy fails closed at startup
//     (internal/egress/proxy/inject.go, server.go), leaving the run with zero
//     egress and a sidecar boot error. Five lines convert that into a disclosed
//     drop.
//
//  3. DROP the BROKERED CREDENTIAL LANES the ceiling denies (PF-1b) — the one a
//     naive implementation misses. The proxy's /wardyn/gh/ and /wardyn/git/
//     routes mint proxy-side and re-originate WITHOUT consulting DeniedDomains;
//     confineGitBrokerEgress makes "github.com denied at evalHost while broker
//     traffic flows" the DESIGNED norm (runs_dispatch_gitbroker.go). So a denied
//     corporate forge still receives a brokered credential unless the lane
//     itself is dropped here, and §A's "credential injection to company hosts is
//     structurally impossible" would be false at exactly the hosts holding the
//     company's code.
//
// WHERE IT RUNS, AND WHY THERE. Immediately AFTER confineGitBrokerEgress and
// before buildRunMounts / the ProxyConfig snapshot. Every phase that can WIDEN
// the run is above it — the artifact substitution and its injections
// (substituteArtifactEgress / planArtifactRedirect), the LLM transport's Bedrock
// egress, the subscription and Bedrock-bearer injections, and the artifact
// injections appended last — so the ceiling is asserted over the FINAL
// composition rather than a snapshot something later re-widens. Run it any
// earlier and it re-opens the hole it closes: injections authored after it would
// never be tested against the ceiling at all (which the ordering test pins).
//
// It does not violate confineGitBrokerEgress's "runs LAST" contract, which is
// about nothing RE-ADDING a broker-managed host to the allowlist: this phase
// only adds DENIES and only removes credentials. It never touches AllowedDomains
// and can therefore not un-confine anything.
//
// NO CONFINEMENT FLOOR-RAISE (PF-6): SandboxSpec.ConfinementClass comes from
// run.ConfinementClass, fixed at create — raising policy.MinConfinementClass
// here would be decoration that looks enforced and is not.
//
// A provable NO-OP when the principal has no assigned profile: p.CeilingDeny is
// nil for an unassigned member, an operator, and every scan/probe/harness lane
// with no member principal at all.
func (s *Server) reassertCeilingDenies(ctx context.Context, run types.AgentRun,
	policy *types.RunPolicySpec, injections *[]runner.InjectionGrant, p *dispatchParams,
	sandboxEnv map[string]string,
) {
	if len(p.CeilingDeny) == 0 {
		return
	}
	added := unionCeilingDenies(policy, p.CeilingDeny)

	var droppedInjection []string
	kept := (*injections)[:0:0] // :0:0 — never alias the caller's array
	for _, in := range *injections {
		if ceilingDenies(p.CeilingDeny, in.Rule.Host) {
			droppedInjection = append(droppedInjection, in.Rule.Host)
			continue
		}
		kept = append(kept, in)
	}
	*injections = kept

	droppedLane := s.dropBrokeredLanes(p, sandboxEnv)

	if len(droppedInjection) > 0 || len(droppedLane) > 0 {
		slog.WarnContext(ctx, "wardynd: governance ceiling — withholding credential lanes for hosts this principal's profile denies",
			slog.String("run_id", run.ID.String()), slog.String("profile", p.CeilingProfile),
			slog.Any("injection_hosts", droppedInjection), slog.Any("broker_lanes", droppedLane))
	}
	// ALWAYS audited when a profile applies, even with nothing to drop: "which
	// ceiling did this run actually run under" is the question the envelope at
	// run.policy.effective cannot answer (it records a policy, not whose walls
	// they are), and the drops themselves are invisible there — injections and
	// broker grants ride ProxyConfig, not the spec.
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.ceiling.reassert",
		run.ID.String(), "success", mustJSON(map[string]any{
			"profile":                 p.CeilingProfile,
			"denied_added":            added,
			"dropped_injection_hosts": droppedInjection,
			"dropped_broker_lanes":    droppedLane,
			"note": "the acting principal's governance profile denies these hosts; the denies are unioned into the run policy " +
				"(deny beats allow and allow_all_egress at the proxy) and every credential lane that reaches a denied host is withheld — " +
				"the brokered git/PAT routes mint proxy-side and never consult denied_domains, so dropping the lane is the only thing that binds them",
		})))
}

// dropBrokeredLanes withholds the git-broker and PAT-broker grants whose
// upstream host the ceiling denies, returning what it dropped (sorted).
//
// GIT: the /wardyn/gh/ route re-originates to gitBrokerManagedHosts, so a
// ceiling denying ANY of them walls off the traffic this lane exists to carry
// and the whole map goes. Deliberately the re-origination set and not just the
// forge: the route dials all of them on the sandbox's behalf and consults no
// deny list while doing it, so a partial deny would otherwise be a lane that
// routes around exactly the host it names.
//
// AND THE ENV VAR WITH IT, which is the half that turns a fix into a hole if it
// is left out. WARDYN_GITHUB_GRANT_ID was written into the sandbox env long
// before this phase (applyDispatchModeEnv), and the proxy's mint refusal keys on
// a NON-EMPTY broker map — isBrokeredGitGrant returns false the moment
// ProxyConfig.GitGrants is empty (internal/egress/proxy/git_broker.go). Emptying
// the map alone would therefore hand the sandbox back the resident-token lane
// the broker exists to close: the agent POSTs the mint route with that grant id
// and gets a real GitHub App installation token in-sandbox. WARDYN_GIT_BROKER_
// REPOS deliberately STAYS — it is what makes wardyn-git-helper refuse to mint a
// GitHub credential at all, so leaving it is strictly the more restrictive
// choice.
//
// PAT: GitPATGrants is keyed by HOST, so it drops per host. Nothing to unset in
// env — when the never-resident lane is on, WARDYN_GIT_PAT_BROKER_HOSTS carries
// host names and no grant id, so it cannot mint anything.
//
// ponytail: the RESIDENT lanes are out of scope here and stay bounded by the
// name-keyed deny alone — WARDYN_GIT_PAT_GRANTS (PATBroker off) and
// WARDYN_SSH_GRANTS are marshalled into env at applyDispatchModeEnv, above this
// phase, and re-deriving them here would duplicate dropBrokeredGrants' own
// filtering. PF-1b is about the lanes that BYPASS denied_domains; a resident
// credential for a denied host still meets that deny on every named dial.
func (s *Server) dropBrokeredLanes(p *dispatchParams, sandboxEnv map[string]string) []string {
	var dropped []string
	if len(p.GitGrants) > 0 && ceilingDeniesAny(p.CeilingDeny, gitBrokerManagedHosts) {
		dropped = append(dropped, slices.Sorted(maps.Keys(p.GitGrants))...)
		p.GitGrants = nil
		delete(sandboxEnv, "WARDYN_GITHUB_GRANT_ID")
	}
	if len(p.GitPATGrants) > 0 {
		kept := map[string]string{} // a new map: the caller's is still read elsewhere
		for host, grantID := range p.GitPATGrants {
			if ceilingDenies(p.CeilingDeny, host) {
				dropped = append(dropped, host)
				continue
			}
			kept[host] = grantID
		}
		p.GitPATGrants = kept
	}
	slices.Sort(dropped)
	return dropped
}
