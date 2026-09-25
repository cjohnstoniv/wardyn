// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The dispatch-time half of a governance profile: the phase that re-asserts an
// ASSIGNED ceiling's DENY set over a run dispatch has finished composing.
//
// Why this exists, in one sentence: a create-time deny is not sufficient,
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

// dispatchCeiling is the acting principal's ceiling in the shape dispatch needs
// it, and it is a required positional argument of dispatchRun/dispatchAndSettle
// rather than a field on dispatchParams. That is the fix for the defect class,
// not a style preference.
//
// A silently-unset optional field is exactly the failure a positional
// argument forecloses: dispatchRun cannot compile without a caller deciding
// what ceiling to pass, so a lane can no longer run with no enforcement and
// no run.ceiling.reassert row to say so.
//
// Two properties make forgetting impossible now:
//
//   - COMPILE. The ceiling is an argument, so a new dispatch lane cannot be
//     written without deciding what to pass.
//   - RUNTIME. `resolved` is set only by the constructors below, so the zero
//     value — the thing a hurried caller would reach for to satisfy the compiler
//     — is REFUSED by dispatchRun and the run fails closed. There is
//     deliberately no "exempt" constructor: every one of the lanes can resolve a
//     real ceiling, and for an operator effectiveCeiling short-circuits to
//     (nil profile, no store read), which is the same provable no-op an
//     exemption would have been, without a door that can be claimed by mistake.
type dispatchCeiling struct {
	// resolved records that this value came from a constructor. Never set it by
	// hand; the zero value must stay the "nobody decided" state.
	resolved bool
	// deny is the ceiling's OWN deny list — never the run's merged
	// policy.DeniedDomains; see ceilingDenies for why a merged-list matcher
	// would kill every broker lane on a deployment with no profile at all.
	deny []string
	// profile is the assigned profile's NAME, for the run.ceiling.reassert audit
	// event. Nothing branches on it.
	profile string
	// maxEphemeralDiskMiB is GovernanceLimits.MaxEphemeralDiskMiB — the profile's
	// ceiling on the writable scratch a run gets, 0 = unlimited. It rides here for
	// the same reason deny does: applyEphemeralDisk is a dispatch phase, and the
	// ceiling is already this function's argument.
	//
	// A clamp, not a deny, so it shares nothing with the deny machinery below: no
	// lane is withheld and no run is refused, which is forced by disk_mib being
	// authored on POLICIES (a refusal would break every stored policy the day an
	// admin first writes a limit — internal/types/governance.go says so).
	maxEphemeralDiskMiB int
	// adoEntra is what the autonomy gate RESOLVED about this run's per-person
	// Azure DevOps lane at create (adoEntraGrade, runs_dispatch_ado_inject.go).
	//
	// It rides HERE, on the required positional argument, for the reason the
	// type comment above already argues: the gate freezes a level from one
	// site-config read and dispatch authors the credential from a later one, so
	// the resolution has to travel, and a dispatchParams field that a lane can
	// silently leave unset is the defect class this struct exists to close.
	adoEntra adoEntraGrade
	// bedrock is the same freeze for the Amazon Bedrock model credential
	// (bedrockCredGrade, runs_autonomy_bedrock.go), on the same argument.
	bedrock bedrockCredGrade
}

// ceilingForDispatch is the ONE translation from a resolved ceiling into the
// dispatch value, so "absent row ⇒ absent behaviour" cannot be re-decided per
// call site.
//
// It carries denies only for an ASSIGNED profile. That is the whole scoping
// rule: a member with no assignment resolves to Config.DefaultPolicy, whose
// denies are ALREADY in their run's policy by every ordinary path — re-asserting
// them would be a no-op on a good day and a behaviour change on a bad one, on
// deployments that have never authored a profile. An operator short-circuits
// earlier still, at effectiveCeiling's step 1. Either way the result is
// resolved: "this principal has no profile" is an answer, and the zero value is
// not.
//
// ado is the SECOND thing every lane must now decide: what the autonomy gate
// resolved about the per-person Azure DevOps lane for this run. It is a
// parameter and not a field for the same reason the ceiling itself is — a lane
// that could leave it unset would be back to authoring a credential nobody
// graded — and adoEntraUngraded() is the honest answer for the four lanes that
// run no gate, not a way of skipping the question. bedrock is the third, for
// the Bedrock model credential, and bedrockCredUngraded() answers it for the
// same lanes.
func ceilingForDispatch(c governanceCeiling, ado adoEntraGrade, bedrock bedrockCredGrade) dispatchCeiling {
	if c.Profile == nil {
		return dispatchCeiling{resolved: true, adoEntra: ado, bedrock: bedrock}
	}
	return dispatchCeiling{
		resolved: true, deny: c.Spec.DeniedDomains, profile: c.Profile.Name,
		maxEphemeralDiskMiB: c.Limits.MaxEphemeralDiskMiB, adoEntra: ado, bedrock: bedrock,
	}
}

// effectivePolicyDatum is the run.policy.resolve snapshot: the audited policy
// plus one fact about it that is not a policy field.
//
// The spec is EMBEDDED, so the JSON object stays byte-for-byte the policy
// snapshot every existing reader parses, plus at most one key. That matters —
// docs/AUDIT-ACTIONS.md calls this datum "(full policy snapshot)" and the
// run-detail Effective-policy widget reads it.
//
// disk_mib_filled rather than an enforcement WORD: the provenance of the size is
// what dispatch actually holds. Whether a cap binds is the host's answer, decided
// per driver inside applyDiskQuota from `docker info`, and the orchestrator
// aggregates only the WEAKEST word across substrates — so naming one here would
// disclose a guess. The bit says the thing that follows from it: this size was
// filled in from the org default, so a host that cannot keep it runs UNCAPPED
// instead of refusing the run.
type effectivePolicyDatum struct {
	types.RunPolicySpec
	DiskMiBFilled bool `json:"disk_mib_filled,omitempty"`
}

// applyEphemeralDisk is THE site where a run's ephemeral scratch size is decided,
// and it decides it for every dispatch lane there is. Returns whether the number
// it left on the policy was FILLED IN rather than asked for (runner.Resources.
// DiskMiBFilled — the bit a driver that cannot enforce a cap needs).
//
// Precedence, once: the request/policy's own resources.disk_mib → FILLED from the
// org's storage.ephemeral.default_disk_mib when that is zero → CLAMPED to
// min(storage.ephemeral.max_disk_mib, the profile's MaxEphemeralDiskMiB), zeros
// meaning "no bound" on either side.
//
// A zero request with no org default stays zero — unbounded scratch, byte-for-byte
// today. A maximum bounds a REQUEST; it never invents one, and this is not
// composer.Clamp's capField idiom (which does fill a zero up to a SPEC cap):
// filling from a maximum would give every request-less run a non-zero DiskMiB, and
// the docker driver fails a create closed on overlay2-over-ext4 — Docker Desktop,
// WSL2, stock Ubuntu — so a single admin number would have bricked every laptop
// run in the estate. The FILL itself is bounded by the same asymmetry on the other
// side: it degrades to uncapped there rather than failing the run closed, which is
// what DiskMiBFilled buys (see applyDiskQuota).
//
// WHY HERE and not on the create path: resolvePolicy deliberately does not clamp
// an admin-authored STORED policy, and resourceLimitsToRunner is a pure mapper
// with neither the ceiling nor the site config in scope. Dispatch is the one seam
// holding all three — after every widening phase, before resourceLimitsToRunner
// and before the run.policy.resolve envelope, which is what discloses the
// effective number to every caller. A profile's own clamp is additionally
// disclosed in run.ceiling.reassert below.
//
// Scope, which differs per ceiling on purpose: storage.ephemeral.max_disk_mib is
// the ORG's number and binds EVERY caller, operators and unassigned members
// included; MaxEphemeralDiskMiB binds assigned members only, because
// ceilingForDispatch above returns no limits at all for Profile == nil (an
// operator short-circuits earlier still, at effectiveCeiling's step 1).
//
// The WHOLE expression is ephemeralDiskFor below, which POST /runs/preflight
// also calls (previewEphemeralDisk): the preview reports the number this run
// will get, org fill and both ceilings included.
// TestPreflightAndDispatchAgreeOnEphemeralDisk is the pin; a comment claiming
// the two "cannot disagree" is not one.
func applyEphemeralDisk(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec,
	siteCfg types.SiteConfig, c dispatchCeiling,
) bool {
	eph := orgEphemeralOf(siteCfg)
	requested := 0
	if policy.Resources != nil {
		requested = policy.Resources.DiskMiB
	}
	disk, filled := ephemeralDiskFor(requested, eph, c.maxEphemeralDiskMiB)
	if disk == requested {
		return false
	}
	// A FRESH block, never an in-place write: policy is a SHALLOW copy of the
	// caller's spec (runs_dispatch.go), so its Resources pointer is still the
	// caller's — and for a run that authored no policy that is the process-global
	// default/ceiling spec. Same reason composer.Clamp rebuilds the struct.
	rl := types.ResourceLimits{}
	if policy.Resources != nil {
		rl = *policy.Resources
	}
	rl.DiskMiB = disk
	policy.Resources = &rl
	if filled {
		slog.InfoContext(ctx, "wardynd: ephemeral disk filled from the org default; a host that cannot enforce it runs uncapped rather than failing closed",
			slog.String("run_id", run.ID.String()), slog.Int("disk_mib", disk))
		return true
	}
	slog.InfoContext(ctx, "wardynd: "+composer.WarnResourcesCapped,
		slog.String("run_id", run.ID.String()), slog.Int("requested_disk_mib", requested),
		slog.Int("disk_mib", disk), slog.Int("provider_max_disk_mib", eph.MaxDiskMiB),
		slog.Int("profile_max_disk_mib", c.maxEphemeralDiskMiB))
	return false
}

// ephemeralDiskFor is §6.3's precedence, as ONE expression with TWO callers that
// must never disagree: applyEphemeralDisk above (what the sandbox gets) and
// previewEphemeralDisk (what POST /runs/preflight reports). request/policy
// disk_mib → FILLED from the org's default_disk_mib when that is zero → CLAMPED
// to min(the org's max_disk_mib, the profile's MaxEphemeralDiskMiB), zeros
// meaning "no bound" on either ceiling.
//
// SCOPE is the CALLER's to decide and differs per ceiling on purpose: eph is the
// ORG's block and binds every caller, operators and unassigned members included;
// profileMaxDiskMiB is passed 0 for anyone the profile does not bind.
//
// `filled` is the provenance bit, not a second number: a size the org filled in
// degrades to uncapped on a host that cannot keep it (runner.Resources.
// DiskMiBFilled → applyDiskQuota), while a policy-authored one fails the create
// closed there. Only the ORG DEFAULT is a fill — a maximum bounds a request and
// never invents one.
func ephemeralDiskFor(requested int, eph types.EphemeralProvider, profileMaxDiskMiB int) (disk int, filled bool) {
	disk = requested
	if disk == 0 && eph.DefaultDiskMiB > 0 {
		disk, filled = eph.DefaultDiskMiB, true
	}
	disk = composer.CapDiskMiB(disk, eph.MaxDiskMiB)
	disk = composer.CapDiskMiB(disk, profileMaxDiskMiB)
	return disk, filled
}

// orgEphemeralOf reads the org's storage.ephemeral block out of a site config,
// absent blocks reading as the zero value (no default, no maximum).
func orgEphemeralOf(siteCfg types.SiteConfig) types.EphemeralProvider {
	if wp := siteCfg.WorkspaceProviders; wp != nil && wp.Storage != nil && wp.Storage.Ephemeral != nil {
		return *wp.Storage.Ephemeral
	}
	return types.EphemeralProvider{}
}

// boundEphemeralDisk is the ephemeral-disk half of resolveRunPolicy, called
// identically from its inline arm and its stored/default arm (the drift that
// let one of them preview a size the other did not).
//
// Scope, per ceiling: the profile's MaxEphemeralDiskMiB binds an ASSIGNED MEMBER
// only — an operator short-circuits at effectiveCeiling's step 1 and an
// unassigned member resolves no limits — so it is ZEROED for anyone else rather
// than the block being skipped, because the ORG's numbers bind every caller.
//
// PREVIEW (dryRun) gets the WHOLE dispatch expression, org fill and org clamp
// included, so POST /runs/preflight reports the number the run will get. LAUNCH
// gets the clamp half only, and there it is a provable no-op: composer.Clamp (or
// the stored arm's boundMemberSpec) already applied the same profile min(), and
// applyEphemeralDisk applies all of it again at dispatch. The FILL is dispatch's
// alone — written into a spec that goes on to launch it would reach the driver
// as a POLICY-AUTHORED size and be refused at create on every overlay2-over-ext4
// host, which is the whole reason runner.Resources carries DiskMiBFilled.
func (s *Server) boundEphemeralDisk(ctx context.Context, r *http.Request, spec *types.RunPolicySpec,
	ceiling governanceCeiling, dryRun bool,
) []string {
	profileMax := 0
	if ceiling.Profile != nil && !s.isOperator(r.Context()) {
		profileMax = ceiling.Limits.MaxEphemeralDiskMiB
	}
	capped := false
	if dryRun {
		capped = s.previewEphemeralDisk(ctx, spec, profileMax)
	} else {
		capped = capEphemeralDiskPreview(spec, profileMax)
	}
	if capped {
		return []string{composer.WarnResourcesCapped}
	}
	return nil
}

// previewEphemeralDisk writes the number dispatch will settle on into a PREVIEW
// spec and reports whether that was a CLAMP (a fill is a difference, but it is
// not "capped to operator maximum" and must not borrow that sentence).
//
// Preview only — resolveRunPolicy calls it on the dryRun arm alone. A fill
// written into a spec that goes on to LAUNCH would reach the driver as a
// POLICY-AUTHORED size and be refused at create on every overlay2-over-ext4
// host; the fill is dispatch's precisely so DiskMiBFilled can carry the
// provenance with it. The clamp half is safe on either arm and the create arm
// keeps doing its own (capEphemeralDiskPreview), so the asymmetry stays.
//
// A FRESH Resources block, never an in-place write — the same aliasing rule
// applyEphemeralDisk obeys. A site-config read that fails degrades to "no org
// block": the preview is advisory and never blocks Review.
func (s *Server) previewEphemeralDisk(ctx context.Context, spec *types.RunPolicySpec, profileMaxDiskMiB int) bool {
	var siteCfg types.SiteConfig
	if s.cfg.Store != nil {
		got, err := s.cfg.Store.GetSiteConfig(ctx)
		if err != nil {
			slog.WarnContext(ctx, "wardynd: preflight could not read the org storage block; previewing without it", slog.Any("error", err))
		}
		siteCfg = got
	}
	requested := 0
	if spec.Resources != nil {
		requested = spec.Resources.DiskMiB
	}
	disk, filled := ephemeralDiskFor(requested, orgEphemeralOf(siteCfg), profileMaxDiskMiB)
	if disk == requested {
		return false
	}
	rl := types.ResourceLimits{}
	if spec.Resources != nil {
		rl = *spec.Resources
	}
	rl.DiskMiB = disk
	spec.Resources = &rl
	return !filled
}

// resolveDispatchCeiling is effectiveCeiling + ceilingForDispatch, for the lanes
// that do not already hold a resolved ceiling (the source scan, the site-config
// probes, the managed-harness login).
//
// FAIL CLOSED on a resolver error, exactly as launchRecordRun does: carrying on
// would silently substitute the deployment ceiling for a profile that may be far
// narrower — a widening caused by a database hiccup, on lanes that hand out
// brokered clone credentials and proxy-side injections.
func (s *Server) resolveDispatchCeiling(ctx context.Context) (dispatchCeiling, governanceCeiling, error) {
	c, err := s.effectiveCeiling(ctx)
	if err != nil {
		return dispatchCeiling{}, governanceCeiling{}, fmt.Errorf("resolve governance ceiling: %w", err)
	}
	// adoEntraUngraded: every lane that resolves its own ceiling here — the scan,
	// the probe and the harness login — runs no autonomy gate, so no rubric
	// capped the run and there is no grade for dispatch to be held to.
	return ceilingForDispatch(c, adoEntraUngraded(), bedrockCredUngraded()), c, nil
}

// ceilingDenies reports whether the CEILING's deny list covers host — an exact
// bare host, a "*." wildcard entry, or either carrying a ":port" qualifier.
//
// The list is the ceiling's alone, and that is not a detail. A matcher compiled
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
// A meet, not a replace. The run's own denies survive untouched (they are the
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
//  3. DROP the brokered credential lanes the ceiling denies — the one a
//     naive implementation misses. The proxy's /wardyn/gh/ and /wardyn/git/
//     routes mint proxy-side and re-originate WITHOUT consulting DeniedDomains;
//     confineGitBrokerEgress makes "github.com denied at evalHost while broker
//     traffic flows" the DESIGNED norm (runs_dispatch_gitbroker.go). So a denied
//     corporate forge still receives a brokered credential unless the lane
//     itself is dropped here, and "credential injection to company hosts is
//     structurally impossible" would be false at exactly the hosts holding the
//     company's code.
//
// Where it runs, and why there. Immediately AFTER confineGitBrokerEgress and
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
// No confinement floor-raise: SandboxSpec.ConfinementClass comes from
// run.ConfinementClass, fixed at create — raising policy.MinConfinementClass
// here would be decoration that looks enforced and is not.
//
// A provable NO-OP when the principal has no assigned profile: c.deny is empty
// for an unassigned member and for an operator (effectiveCeiling's step-1
// short-circuit). It is NOT a no-op merely because a lane forgot to resolve a
// ceiling — that state is unrepresentable now, since dispatchCeiling is a
// required argument whose zero value dispatchRun refuses (see dispatchCeiling).
func (s *Server) reassertCeilingDenies(ctx context.Context, run types.AgentRun,
	policy *types.RunPolicySpec, injections *[]runner.InjectionGrant, c dispatchCeiling,
	p *dispatchParams, sandboxEnv map[string]string, llm *llmTransport, mitmHosts *[]string,
) {
	// THE ENFORCEMENT half is gated on the deny list, because with nothing to
	// deny there is nothing to union, no injection to drop and no lane to
	// withhold. The AUDIT half below is gated on the PROFILE instead — those are
	// two different questions, and folding them into one early return is what
	// made "always audited when a profile applies" false for the commonest
	// assigned shape there is: a profile that grants rather than denies.
	var added, droppedInjection, droppedLane []string
	if len(c.deny) > 0 {
		added = unionCeilingDenies(policy, c.deny)

		kept := (*injections)[:0:0] // :0:0 — never alias the caller's array
		for _, in := range *injections {
			if ceilingDenies(c.deny, in.Rule.Host) {
				droppedInjection = append(droppedInjection, in.Rule.Host)
				continue
			}
			kept = append(kept, in)
		}
		*injections = kept

		droppedLane = s.dropBrokeredLanes(c, p, sandboxEnv)
		droppedLane = append(droppedLane, narrowCeilingBedrockLane(c, llm, mitmHosts, sandboxEnv)...)
		slices.Sort(droppedLane)

		if len(droppedInjection) > 0 || len(droppedLane) > 0 {
			slog.WarnContext(ctx, "wardynd: governance ceiling — withholding credential lanes for hosts this principal's profile denies",
				slog.String("run_id", run.ID.String()), slog.String("profile", c.profile),
				slog.Any("injection_hosts", droppedInjection), slog.Any("broker_lanes", droppedLane))
		}
	}
	// No assigned profile — an operator (effectiveCeiling short-circuits at step
	// 1) or an unassigned member: no run.ceiling.reassert row, which is what
	// makes the row's ABSENCE mean "no profile applies" rather than "this door
	// skipped the ceiling". ceilingForDispatch leaves deny and profile empty
	// together, so this is the same provable no-op the deny check stood for.
	if c.profile == "" {
		return
	}
	// ALWAYS audited when a profile applies, even with nothing to drop: "which
	// ceiling did this run actually run under" is the question the envelope at
	// run.policy.resolve cannot answer (it records a policy, not whose walls
	// they are), and the drops themselves are invisible there — injections and
	// broker grants ride ProxyConfig, not the spec. A profile with an EMPTY
	// denied_domains is exactly where that question is hardest to answer any
	// other way: nothing about the dispatch changes, so this row is the ONLY
	// evidence of which walls the run stood inside.
	data := map[string]any{
		"profile":                 c.profile,
		"denied_added":            added,
		"dropped_injection_hosts": droppedInjection,
		"dropped_broker_lanes":    droppedLane,
		"note": "the acting principal's governance profile denies these hosts; the denies are unioned into the run policy " +
			"(deny beats allow and allow_all_egress at the proxy) and every credential lane that reaches a denied host is withheld — " +
			"the brokered git/PAT routes mint proxy-side and never consult denied_domains, so dropping the lane is the only thing that binds them",
	}
	// The SIZE half of the profile, present only when the profile sets one — so a
	// profile written before 0.7.2 produces a byte-identical row. applyEphemeralDisk
	// ran just above this phase, so ephemeral_disk_mib is the effective number the
	// sandbox gets; run.policy.resolve discloses it to everyone, and this says
	// whose ceiling shaped it.
	if c.maxEphemeralDiskMiB > 0 {
		data["max_ephemeral_disk_mib"] = c.maxEphemeralDiskMiB
		effective := 0
		if policy.Resources != nil {
			effective = policy.Resources.DiskMiB
		}
		data["ephemeral_disk_mib"] = effective
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.ceiling.reassert",
		run.ID.String(), "success", mustJSON(data)))
}

// dropBrokeredLanes withholds the git-broker and PAT-broker grants whose
// upstream host the ceiling denies, returning what it dropped (sorted).
//
// Git: the /wardyn/gh/ route re-originates to gitBrokerManagedHosts, so a
// ceiling denying ANY of them walls off the traffic this lane exists to carry
// and the whole map goes. Deliberately the re-origination set and not just the
// forge: the route dials all of them on the sandbox's behalf and consults no
// deny list while doing it, so a partial deny would otherwise be a lane that
// routes around exactly the host it names.
//
// And the env var with it, which is the half that turns a fix into a hole if it
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
// filtering. This phase is about the lanes that BYPASS denied_domains; a
// resident credential for a denied host still meets that deny on every named
// dial.
func (s *Server) dropBrokeredLanes(c dispatchCeiling, p *dispatchParams, sandboxEnv map[string]string) []string {
	var dropped []string
	if len(p.GitGrants) > 0 && ceilingDeniesAny(c.deny, gitBrokerManagedHosts) {
		dropped = append(dropped, slices.Sorted(maps.Keys(p.GitGrants))...)
		p.GitGrants = nil
		delete(sandboxEnv, "WARDYN_GITHUB_GRANT_ID")
	}
	if len(p.GitPATGrants) > 0 {
		kept := map[string]string{} // a new map: the caller's is still read elsewhere
		for host, grantID := range p.GitPATGrants {
			if ceilingDenies(c.deny, host) {
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

// bedrockCeilingLane is how the re-assertion names the Bedrock credential lane
// in dropped_broker_lanes. A lane name rather than a host list: the lane is one
// decision (which Bedrock credential this run gets), the hosts it reaches are
// already in denied_added, and the run's OWN denies are what an auditor reads to
// see why.
const bedrockCeilingLane = "bedrock"

// narrowCeilingBedrockLane withholds the Bedrock credential lane when the
// acting principal's profile denies a host that lane's traffic goes to, and
// reports whether it did.
//
// Why this exists alongside dropBrokeredLanes. The re-assertion already
// dropped the Bedrock BEARER injection, because a bearer token rides an
// injection rule and injection rules are filtered by host. But the bearer is the
// one Bedrock mode that is NEVER RESIDENT. The resident modes were untouched:
// the SigV4 keys applyBedrockTransport wrote into sandboxEnv stayed there,
// llm.secretEnvKeys still named them so splitSecretEnv moved them onto
// SandboxSpec.SecretEnv, buildRunMounts still bind-mounted the operator's whole
// host ~/.aws read-only into the sandbox, and the per-run MITM host survived. So
// a profile that walls off Bedrock produced a run that could not REACH Bedrock
// and held the credentials for it anyway — credential RESIDENCY inside a
// sandbox whose principal is denied the service those credentials are for.
//
// It narrows the TRANSPORT rather than the spec, because the transport is what
// every consumer downstream of this phase reads: buildRunMounts takes
// llm.bedrockReady/awsMount, splitSecretEnv takes llm.secretEnvKeys, and
// ProxyConfig.MITMHosts takes the plan's bedrock entries. Narrowing here means
// none of them has to learn about ceilings.
//
// Deliberately NOT a refusal: this phase's whole doctrine is that it only adds
// denies and removes credentials, never fails a run (reassertCeilingDenies' own
// doc). A run that asked for no model call, or that has another lane, still
// launches; one that needed Bedrock meets the deny at the proxy with the
// ordinary refusal, which is the same thing it already did for the bearer mode.
func narrowCeilingBedrockLane(c dispatchCeiling, llm *llmTransport, mitmHosts *[]string, sandboxEnv map[string]string) []string {
	if llm == nil || !llm.bedrockReady || !ceilingDeniesAny(c.deny, llm.bedrock.egressHosts) {
		return nil
	}
	// By KEY, from the plan applyBedrockTransport actually wrote: deleting by an
	// "AWS_" prefix would also take an env_secret grant's variable (resolved
	// after this phase today, but the provenance is what makes it correct rather
	// than the ordering).
	for k := range llm.bedrock.env {
		delete(sandboxEnv, k)
	}
	llm.bedrock = bedrockAuth{}
	llm.bedrockReady = false
	llm.injectBedrockBearer = false
	// The captured-SSO lane is narrowed with its sibling, and it MUST be: its
	// injection grant is authored AFTER this phase, so leaving the flag set
	// would author a grant and a portal.sso MITM entry for a lane whose egress
	// this ceiling just denied — a credential wired to a host the run cannot
	// reach, and a MITM entry for it.
	llm.injectBedrockSSO = false
	llm.secretEnvKeys = nil
	if mitmHosts != nil {
		*mitmHosts = nil
	}
	return []string{bedrockCeilingLane}
}
