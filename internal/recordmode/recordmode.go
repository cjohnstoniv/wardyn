// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package recordmode is the deterministic core of Wardyn's "Recording Mode": it OBSERVES what a
// fully-open (allow-all-egress, broad-grant) run actually used — purely from already-captured audit
// events — and SYNTHESIZES a tightened, least-privilege RunPolicySpec the operator can review and promote.
//
// PURITY: the two entry points are pure functions of their inputs (audit events, grants, the run
// in; values out) — no database, network, clock, or global state, so synthesis is a function of
// captured evidence, not of anything an in-sandbox agent can influence after the fact.
//
// DETERMINISM: every set is de-duplicated and SORTED, egress decision counts are sums, and
// Synthesize iterates already-sorted fields, so equal evidence always yields byte-identical output.
//
// HONESTY: Recording Mode tightens from evidence but deliberately does NOT auto-author everything —
// it never auto-wildcards a domain, it forces allow_all_egress=false and first_use_approval=true so
// the tightened policy fails toward human escalation rather than silent denial, and it does not
// synthesize WorkspaceMounts or an exec/connect/file allowlist, surfacing kernel ground-truth as
// warnings/Observations instead.
package recordmode

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Audit action discriminators this package reads, derived from the egress.Decision enum so they
// stay in lockstep with the wire values the proxy emits (handlePostDecision writes "egress."+decision).
const (
	actionEgressAllow    = "egress." + string(egress.Allow)
	actionEgressDeny     = "egress." + string(egress.Deny)
	actionEgressPending  = "egress." + string(egress.Pending)
	actionCredentialMint = "credential.mint" // broker's mint audit action (broker.auditMint)
)

// Audit outcome values (the audit_events.outcome CHECK domain).
const (
	outcomeSuccess = "success"
	outcomeFailure = "failure"
)

// mountTargetPrefixes are the in-container paths a host WorkspaceMount may be mounted under. A
// captured sensitive write under one is the only signal a pure function has that the recording MAY
// have used a mount, so this triggers an operator warning, never an auto-authored mount.
var mountTargetPrefixes = []string{"/home/agent", "/work", "/workspace"}

// DomainObservation is one egress host the run actually reached, with the HTTP method set observed
// at the proxy and the per-decision counts. Methods is de-duplicated and sorted; counts are sums
// over every decision for the host.
type DomainObservation struct {
	Host string `json:"host"` // lowercased, trimmed egress hostname (no port)
	// Methods is the de-duplicated, sorted set of HTTP methods observed (an upper-cased "CONNECT"
	// appears for tunneled TLS); may be empty when the proxy only saw opaque CONNECTs.
	Methods      []string `json:"methods,omitempty"`
	AllowCount   int      `json:"allow_count"` // number of egress.allow/deny/pending decisions recorded for this host
	DenyCount    int      `json:"deny_count"`
	PendingCount int      `json:"pending_count"`
	// ApprovalCount is the subset of AllowCount RELEASED by a live first-use approval rather than the
	// standing policy. CleanReplay treats any of these as caught: approving mid-replay must not earn
	// a green the standing policy didn't.
	ApprovalCount int `json:"approval_count"`
}

// Observations is the deterministic aggregate of what a run actually used, computed purely from its
// already-captured audit events. Every slice is de-duplicated and sorted, so equal evidence yields equal Observations.
type Observations struct {
	Domains []DomainObservation `json:"domains,omitempty"` // per-host egress aggregate (deduped, sorted by host)
	// MintedGrantIDs is the deduped, sorted set of grant ids the run SUCCESSFULLY minted a credential
	// for (credential.mint with outcome=success); a denied or failed mint is NOT included.
	MintedGrantIDs []uuid.UUID `json:"minted_grant_ids,omitempty"`
	ExecArgv0s     []string    `json:"exec_argv0s,omitempty"` // deduped, sorted argv[0] the kernel sensor observed the run exec
	FileWrites     []string    `json:"file_writes,omitempty"` // deduped, sorted sensitive file paths the kernel sensor observed the run write
	Connects       []string    `json:"connects,omitempty"`    // deduped, sorted "ip:port" destinations the kernel sensor observed the run connect to
	// Anomalies is the deduped, sorted set of human-readable signals a least-privilege synthesis must
	// NOT silently bless: an egress.deny during open recording, a dynamic-linker exec, a
	// failed/escape kernel connect, and the sensor dropping events as unmapped during this capture.
	Anomalies []string `json:"anomalies,omitempty"`
}

// sensorCorrelatedNothingAnomaly is the one anomaly sourced from the sensor's own counters rather
// than from this run's events. It says what the counters PROVE and no more: the count is cumulative
// since the sensor started, so the sentence does not claim it happened during this capture.
//
// DRAFT (M2 canon pending)
const sensorCorrelatedNothingAnomaly = "the kernel sensor was alive during this capture and correlated none " +
	"of what it saw to any run (%d event(s) dropped as unmapped, cumulative since the sensor started): either " +
	"the run-correlation is broken, or activity reached the kernel outside every sandbox — this capture has no " +
	"kernel corroboration either way"

// KernelWindow is what the host's eBPF sensor said about ITSELF while this capture was running —
// the one fact a capture's own audit events cannot carry. Must be scoped to the capture's window:
// the sensor is host-wide and its heartbeat global, so a beat from days later is not evidence about
// this capture. The zero value (no heartbeat inside the window) is the honest answer for a host with no sensor.
type KernelWindow struct {
	Beat            bool   // true when a sensor heartbeat landed inside the capture window
	DroppedUnmapped uint64 // that beat's cumulative count of kernel events refused as correlating to no run
	ObservedTotal   uint64 // that beat's cumulative count of kernel events the sensor DID bind to a run
}

// domainAgg is the mutable per-host accumulator used while capturing.
type domainAgg struct {
	methods                        map[string]bool
	allow, deny, pending, approval int
}

// egressData is the JSON shape of an egress.* audit event's Data (handlePostDecision marshals
// {host,port,method,path,rule_source,approval_id}).
type egressData struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	RuleSource string `json:"rule_source"`
}

// mintData is the subset of a credential.mint audit event's Data we read (broker.auditMint marshals
// {grant_id, scope, approval_id?, jti?}).
type mintData struct {
	GrantID string `json:"grant_id"`
}

// Capture aggregates one run's already-captured audit events into a deduped, sorted Observations.
// Pure and input-order independent: reads only the egress.*, credential.mint, and kernel.* streams.
//
// confined distinguishes an OPEN (learning) recording, where a deny is a real anomaly, from a
// CONFINED replay, where a deny is the containment proof working as designed — still captured on
// the per-host DomainObservation, but not landed in Anomalies.
//
// kernel carries the one anomaly this run's OWN events cannot show: the sensor's unmapped-drop
// count while the capture was running. See KernelWindow.
func Capture(events []types.AuditEvent, confined bool, kernel KernelWindow) Observations {
	domains := map[string]*domainAgg{}
	minted := map[uuid.UUID]bool{}
	execs := map[string]bool{}
	files := map[string]bool{}
	connects := map[string]bool{}
	anomalies := map[string]bool{}

	for _, ev := range events {
		switch ev.Action {
		case actionEgressAllow, actionEgressDeny, actionEgressPending:
			captureEgress(ev, domains, anomalies, confined)
		case actionCredentialMint:
			if ev.Outcome == outcomeSuccess { // only a SUCCESSFUL mint actually yielded a credential the run used
				captureMint(ev, minted)
			}
		case groundtruth.ActionProcessExec:
			captureExec(ev, execs, anomalies)
		case groundtruth.ActionNetworkConnect:
			captureConnect(ev, connects, anomalies)
		case groundtruth.ActionFileWrite:
			captureFileWrite(ev, files)
		}
	}

	// The ONE anomaly this run's own events can never carry: an unmapped kernel event correlated to
	// NO run never reaches this function's own event loop. Condition is drops AND NOTHING
	// CORRELATED, not drops alone — DroppedUnmapped is cumulative over the sensor's whole process
	// lifetime and is non-zero within seconds on any box that does anything; paired with
	// ObservedTotal == 0 it means the sensor saw kernel events and bound NONE of them to any run.
	if kernel.Beat && kernel.DroppedUnmapped > 0 && kernel.ObservedTotal == 0 {
		anomalies[fmt.Sprintf(sensorCorrelatedNothingAnomaly, kernel.DroppedUnmapped)] = true
	}

	return Observations{
		Domains:        buildDomains(domains),
		MintedGrantIDs: sortedUUIDs(minted),
		ExecArgv0s:     sortedStrings(execs),
		FileWrites:     sortedStrings(files),
		Connects:       sortedStrings(connects),
		Anomalies:      sortedStrings(anomalies),
	}
}

// captureEgress folds one egress.* decision into the per-host aggregate and, for an OPEN recording
// only, records an anomaly for a deny — a deny during a CONFINED replay is the containment proof
// working as intended, not an anomaly.
func captureEgress(ev types.AuditEvent, domains map[string]*domainAgg, anomalies map[string]bool, confined bool) {
	var d egressData
	_ = json.Unmarshal(ev.Data, &d) // best-effort: a malformed body still has Target

	host := strings.ToLower(strings.TrimSpace(d.Host))
	if host == "" {
		host = strings.ToLower(strings.TrimSpace(ev.Target))
	}
	if host == "" {
		return
	}

	agg := domains[host]
	if agg == nil {
		agg = &domainAgg{methods: map[string]bool{}}
		domains[host] = agg
	}
	if m := strings.ToUpper(strings.TrimSpace(d.Method)); m != "" {
		agg.methods[m] = true
	}

	switch ev.Action {
	case actionEgressAllow:
		agg.allow++
		// Scoped to exactly this branch: "approval:denied"/"approval:pending" share the "approval:"
		// prefix but land on the deny/pending actions, never here.
		if strings.HasPrefix(strings.TrimSpace(d.RuleSource), "approval:") {
			agg.approval++
		}
	case actionEgressPending:
		agg.pending++
	case actionEgressDeny:
		agg.deny++
		if !confined {
			rs := strings.TrimSpace(d.RuleSource)
			if rs == "" {
				rs = "unknown"
			}
			anomalies[fmt.Sprintf("egress.deny to %s during open recording (rule_source=%s)", host, rs)] = true
		}
		// confined: still captured on the per-host DomainObservation (agg.deny) — a plain
		// containment observation, not an anomaly.
	}
}

// captureMint records the grant id of a successful credential mint.
func captureMint(ev types.AuditEvent, minted map[uuid.UUID]bool) {
	var d mintData
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return
	}
	id, err := uuid.Parse(strings.TrimSpace(d.GrantID))
	if err != nil || id == uuid.Nil {
		return
	}
	minted[id] = true
}

// captureExec records argv[0] and flags a dynamic-linker (loader) exec.
func captureExec(ev types.AuditEvent, execs, anomalies map[string]bool) {
	var d groundtruth.EventData
	if err := json.Unmarshal(ev.Data, &d); err != nil || len(d.Argv) == 0 {
		return
	}
	argv0 := strings.TrimSpace(d.Argv[0])
	if argv0 != "" {
		execs[argv0] = true
	}
	// Trust the sensor's loader flag, but also re-derive it (defense in depth): the ld-linux/mmap
	// execve-hook bypass is surfaced, never assumed absent.
	if d.Loader || groundtruth.IsDynamicLinker(argv0) {
		anomalies[fmt.Sprintf("dynamic-linker exec (loader) argv=[%s] — ld-linux/mmap execve-hook bypass surface", strings.Join(d.Argv, " "))] = true
	}
}

// captureConnect records a connect destination and flags a FAILED connect (a reach to the cloud
// metadata IP — the only destination the mapper stamps outcome=failure). It does NOT look for
// correlation=unmapped: that signal is the sensor's dropped_unmapped counter, read via KernelWindow.
func captureConnect(ev types.AuditEvent, connects, anomalies map[string]bool) {
	var d groundtruth.EventData
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return
	}
	dst := strings.TrimSpace(d.Dst)
	if dst != "" {
		connects[dst] = true
	}
	label := dst
	if label == "" {
		label = "(unknown dst)"
	}
	if ev.Outcome == outcomeFailure {
		anomalies[fmt.Sprintf("anomalous kernel connect to %s (outcome=failure; cloud metadata-IP reach — credential-theft blind spot)", label)] = true
	}
}

// captureFileWrite records a sensitive file-write path.
func captureFileWrite(ev types.AuditEvent, files map[string]bool) {
	var d groundtruth.EventData
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		return
	}
	if p := strings.TrimSpace(d.Path); p != "" {
		files[p] = true
	}
}

// CleanReplay is the server-side verdict for a CONFINED replay (Workstream B): true iff the capture
// is clean FOR WHAT WAS REPLAYED. Pure; meaningful only for a settled `recorded` capture, which the
// caller gates on before calling this.
//
// clean iff, across every observed domain: zero DenyCount, zero PendingCount, and zero
// ApprovalCount (no allow was released by a live first-use approval — approving mid-replay must not
// earn a green the standing policy didn't; the honest loop is approve, then replay again) — AND the
// capture itself was not truncated (maxCaptureAuditEvents, workspace_run.go). Hard-walled denies
// count as caught like any other DenyCount.
//
// Empty obs with truncated=false is clean. "Clean" does NOT mean "nothing could go wrong" — a
// replay the operator ends early earns the same verdict as a full one if nothing else caught it.
func CleanReplay(obs []DomainObservation, truncated bool) bool {
	if truncated {
		return false
	}
	for _, d := range obs {
		if d.DenyCount > 0 || d.PendingCount > 0 || d.ApprovalCount > 0 {
			return false
		}
	}
	return true
}

// Synthesize derives a tightened, least-privilege RunPolicySpec from the Observations of an open
// run plus the run's grant catalog and the run itself. Pure and deterministic; returns the spec
// alongside human-readable warnings explaining every tightening decision and everything it
// deliberately did NOT auto-author. See the package doc for the honesty guarantees.
func Synthesize(obs Observations, runGrants []types.CredentialGrant, run types.AgentRun) (types.RunPolicySpec, []string) {
	var warnings []string
	var spec types.RunPolicySpec

	// ── Egress allowlist: EXACT hosts that were actually ALLOWED. ──
	// A host only denied or only held pending is NOT added — promoting a denied host would WIDEN
	// past what the open run was permitted. Never wildcard.
	var allowed []string
	for _, d := range obs.Domains {
		switch {
		case d.AllowCount > 0:
			allowed = append(allowed, d.Host)
		case d.DenyCount > 0:
			// A recording session is allow-all, so an observed denial is either the unconditional
			// builtin blocks (metadata/private-IP) or a permanent `always`-scoped workspace deny
			// (Phase 4), which allow-all does NOT override — name both so an operator isn't
			// re-scanning the policy that isn't the cause.
			warnings = append(warnings, fmt.Sprintf("host %s observed but only DENIED (never allowed) — a builtin block or a permanent workspace deny, not this session's policy; excluded from allowed_domains", d.Host))
		default: // pending only
			warnings = append(warnings, fmt.Sprintf("host %s observed but only PENDING (never allowed); excluded from allowed_domains", d.Host))
		}
	}
	sort.Strings(allowed)
	spec.AllowedDomains = allowed
	if len(allowed) == 0 {
		// policy.go's AllowedDomains carries no `omitempty`, so a nil slice would serialize as
		// `allowed_domains: null` — nil -> [] here, once, at the producer.
		spec.AllowedDomains = []string{}
		warnings = append(warnings, "no allowed egress observed; synthesized spec denies ALL egress (allow_all_egress=false, empty allowlist) — confirm the run genuinely needed none")
	}
	// DeniedDomains is never synthesized: a recording only proves what WAS reached, never what
	// should stay blocked. Not fail-open — a workspace's own permanent deny list still applies at
	// replay time — but say so, or the synthesized spec reads as a complete envelope when it is allow-only.
	warnings = append(warnings, "denied_domains are never synthesized from a recording; a promoted policy inherits only the workspace's own permanent deny list, applied at run creation")

	// ── Forced invariants (mitigate the recording's inherent under-coverage). ──
	// The snapshot can only prove what the run HAPPENED to use, never the full set it may need, so
	// we fail toward human escalation, not silent denial.
	spec.AllowAllEgress = false // recordings exist to REMOVE allow-all
	// Unknown (un-recorded) hosts escalate to a human: deny_with_review (raise + retry), not
	// wait_for_review, so a replay never hangs unattended.
	spec.FirstUseApproval = types.FirstUseDenyWithReview
	spec.AllowedMethods = nil // method capture is brittle; do not over-restrict
	// A recording carries no lifetime signal, and the reaper skips on <= 0 — mirror the
	// workspace-run cap rather than synthesize a never-reaped spec.
	spec.AutoStopAfterSec = 3600
	warnings = append(warnings, "auto_stop_after_sec defaulted to 3600 (1h idle); the recording carries no lifetime signal — widen it, or set -1 for a deliberate never-reap session")

	// ── Confinement class. ──
	cc := run.ConfinementClass
	if strings.TrimSpace(string(cc)) == "" {
		warnings = append(warnings, "run carries no confinement class; min_confinement_class left empty")
	}
	spec.MinConfinementClass = cc

	// ── Eligible grants: only the grants the run actually minted, by id. ──
	byID := make(map[uuid.UUID]types.GrantSpec, len(runGrants))
	for _, g := range runGrants {
		byID[g.ID] = g.Spec
	}
	for _, id := range obs.MintedGrantIDs { // already sorted → deterministic
		gs, ok := byID[id]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("minted grant %s not found among run grants; omitted from eligible_grants", id))
			continue
		}
		// NEVER carry a shared-subscription sentinel into a stored profile: it resolves to ONE
		// operator's live Anthropic OAuth token at the injection sink, so copying it here would turn
		// a recorded run into a durable, shareable, policy-id-addressable grant on that person's
		// personal credential, usable by anyone who can launch a run against the profile. The sink
		// refuses off-posture anyway; this stops the grant being written down at all.
		if gs.Kind == types.GrantAPIKey && grantNamesOAuthSentinel(gs) {
			warnings = append(warnings, fmt.Sprintf("minted grant %s injects a subscription OAuth sentinel; omitted from eligible_grants (a stored profile must not carry a live subscription credential)", id))
			continue
		}
		// A `shared` grant is an organisation component's: the component gate
		// sets the mark and the policy validator refuses it when authored, so a
		// profile carrying one could be neither saved nor launched. The
		// component is what a later run attaches to get the grant again.
		if grantIsShared(gs) {
			warnings = append(warnings, fmt.Sprintf("minted grant %s was added by an organisation's component; omitted from eligible_grants (attach the component at launch to use it again)", id))
			continue
		}
		spec.EligibleGrants = append(spec.EligibleGrants, gs)
		if gs.Kind == types.GrantGitHubToken {
			warnings = append(warnings, fmt.Sprintf("eligible grant %s (github_token) carries scope permissions %s; confirm they intersect the least-privilege need", id, githubPermSummary(gs.Scope)))
		}
	}

	// ── Workspace mounts: NEVER synthesized (operator-authored, admin-gated). ──
	var mountHits []string
	for _, p := range obs.FileWrites { // sorted → deterministic
		if mountTargetPrefix(p) != "" {
			mountHits = append(mountHits, p)
		}
	}
	if len(mountHits) > 0 {
		warnings = append(warnings, fmt.Sprintf("recording wrote under host-mount target prefix(es): %s; workspace_mounts are operator-authored and were NOT synthesized", strings.Join(mountHits, ", ")))
	}

	// ── Kernel ground-truth: informational only (no policy field exists). ──
	if len(obs.ExecArgv0s) > 0 {
		warnings = append(warnings, fmt.Sprintf("recording exec'd %d distinct program(s); the policy model has no exec-allowlist, review out-of-band: %s", len(obs.ExecArgv0s), strings.Join(obs.ExecArgv0s, ", ")))
	}
	if len(obs.Connects) > 0 {
		warnings = append(warnings, fmt.Sprintf("kernel observed %d connect destination(s) not represented in policy: %s", len(obs.Connects), strings.Join(obs.Connects, ", ")))
	}
	if len(obs.FileWrites) > 0 {
		warnings = append(warnings, fmt.Sprintf("kernel observed %d sensitive file-write(s) not represented in policy: %s", len(obs.FileWrites), strings.Join(obs.FileWrites, ", ")))
	}

	// ── Surface every captured anomaly into the synthesis output. ──
	for _, a := range obs.Anomalies { // sorted → deterministic
		warnings = append(warnings, "anomaly: "+a)
	}

	return spec, warnings
}

// githubPermSummary renders a github_token grant scope's permissions map as a sorted "k:v" list for
// an operator-facing warning (mirrors risk.go's parsing).
func githubPermSummary(scope json.RawMessage) string {
	var s struct {
		Permissions map[string]string `json:"permissions"`
	}
	if len(scope) == 0 || json.Unmarshal(scope, &s) != nil || len(s.Permissions) == 0 {
		return "{}"
	}
	parts := make([]string, 0, len(s.Permissions))
	for k, v := range s.Permissions {
		parts = append(parts, k+":"+v)
	}
	sort.Strings(parts)
	return "{" + strings.Join(parts, ", ") + "}"
}

// mountTargetPrefix returns the host-mount target prefix p falls under, or "".
func mountTargetPrefix(p string) string {
	p = strings.TrimSpace(p)
	for _, pre := range mountTargetPrefixes {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return pre
		}
	}
	return ""
}

// buildDomains converts the per-host accumulator into the sorted DomainObservation slice (hosts
// sorted; each host's methods deduped+sorted).
func buildDomains(m map[string]*domainAgg) []DomainObservation {
	if len(m) == 0 {
		return nil
	}
	hosts := slices.Sorted(maps.Keys(m))
	out := make([]DomainObservation, 0, len(hosts))
	for _, h := range hosts {
		a := m[h]
		out = append(out, DomainObservation{
			Host:          h,
			Methods:       sortedStrings(a.methods),
			AllowCount:    a.allow,
			DenyCount:     a.deny,
			PendingCount:  a.pending,
			ApprovalCount: a.approval,
		})
	}
	return out
}

// sortedStrings returns the set's keys de-duplicated and sorted (nil if empty).
func sortedStrings(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(set))
}

// sortedUUIDs returns the set's ids sorted by string form (nil if empty).
func sortedUUIDs(set map[uuid.UUID]bool) []uuid.UUID {
	if len(set) == 0 {
		return nil
	}
	return slices.SortedFunc(maps.Keys(set), func(a, b uuid.UUID) int {
		return strings.Compare(a.String(), b.String())
	})
}

// grantIsShared reports whether a grant's scope carries the `shared` mark the
// component gate sets for an organisation's provided secret.
func grantIsShared(gs types.GrantSpec) bool {
	var scope struct {
		Shared bool `json:"shared"`
	}
	return json.Unmarshal(gs.Scope, &scope) == nil && scope.Shared
}

// grantNamesOAuthSentinel reports whether an api_key grant's scope names one of the SENTINEL secret
// names that resolve to a live Anthropic OAuth token rather than a stored secret (see
// types.SubscriptionOAuthSecret / ManagedOAuthSecret, a person's own wardyn-provider-<uid>-oauth,
// and the injection sink in internal/api/injection.go). Unparseable scope reads as "yes": a grant
// whose scope we cannot inspect is not a grant to write into a durable least-privilege profile.
func grantNamesOAuthSentinel(gs types.GrantSpec) bool {
	var scope struct {
		SecretName string `json:"secret_name"`
	}
	if err := json.Unmarshal(gs.Scope, &scope); err != nil {
		return true
	}
	return scope.SecretName == types.SubscriptionOAuthSecret || scope.SecretName == types.ManagedOAuthSecret ||
		strings.HasPrefix(scope.SecretName, types.ModelProviderSecretPrefix) && strings.HasSuffix(scope.SecretName, "-oauth")
}
