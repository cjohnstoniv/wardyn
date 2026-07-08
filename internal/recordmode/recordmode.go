// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package recordmode is the deterministic core of Wardyn's "Recording Mode": it
// OBSERVES what a fully-open (allow-all-egress, broad-grant) run actually used —
// purely from already-captured audit events — and SYNTHESIZES a tightened,
// least-privilege RunPolicySpec the operator can review and promote.
//
// PURITY. Like internal/composer/risk.go's Grade, the two entry points here are
// pure functions of their inputs. They take SLICES (audit events, grants, the
// run) and return values; they touch NO database, NO network, NO clock, and NO
// global state. This makes them trivially unit-testable and makes the synthesis
// a function of the captured evidence, not of anything an in-sandbox (possibly
// prompt-injected) agent can influence after the fact.
//
// DETERMINISM. Both functions are deterministic and input-order INDEPENDENT:
//   - every set (domains, methods, argv[0]s, file writes, connects, grant ids,
//     anomalies) is de-duplicated and then SORTED, so the same evidence yields
//     byte-identical output regardless of the order events were recorded in;
//   - egress decision COUNTS are sums (order-independent by construction);
//   - Synthesize iterates already-sorted Observations fields (never a Go map),
//     so its returned spec and warnings are stable across runs.
//
// HONESTY. Recording Mode tightens egress and credential surface from evidence,
// but it deliberately does NOT auto-author everything:
//   - it NEVER auto-wildcards a domain (exact hosts only) — wildcards widen, and
//     a recording cannot prove a wildcard is needed;
//   - it FORCES allow_all_egress=false and first_use_approval=true, so the
//     tightened policy fails toward human escalation rather than silent denial
//     of a host the (necessarily incomplete) recording happened not to exercise;
//   - it does NOT synthesize WorkspaceMounts (operator-authored, admin-gated)
//     and the policy model has no exec/connect/file allowlist, so kernel
//     ground-truth is surfaced as warnings/Observations, never as silent policy.
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

// Audit action discriminators this package reads. The egress.* values are
// derived from the egress.Decision enum so they stay in lockstep with the wire
// values the proxy actually emits (handlePostDecision writes "egress."+decision).
const (
	actionEgressAllow   = "egress." + string(egress.Allow)
	actionEgressDeny    = "egress." + string(egress.Deny)
	actionEgressPending = "egress." + string(egress.Pending)
	// actionCredentialMint is the broker's mint audit action (broker.auditMint).
	actionCredentialMint = "credential.mint"
)

// Audit outcome values (the audit_events.outcome CHECK domain).
const (
	outcomeSuccess = "success"
	outcomeFailure = "failure"
)

// mountTargetPrefixes are the in-container paths a host WorkspaceMount may be
// mounted under (see types.WorkspaceMount / validatePolicySpec: /home/agent,
// /work, /workspace). A captured sensitive write under one of these is the only
// signal available to a pure function that the recording MAY have used a mount —
// the audit streams do not carry mount configuration, so this is a heuristic
// that triggers an operator warning, never an auto-authored mount.
var mountTargetPrefixes = []string{"/home/agent", "/work", "/workspace"}

// DomainObservation is one egress host the run actually reached, with the HTTP
// method set observed at the proxy and the per-decision counts. Methods is
// de-duplicated and sorted; counts are sums over every decision for the host.
type DomainObservation struct {
	// Host is the lowercased, trimmed egress hostname (no port).
	Host string `json:"host"`
	// Methods is the de-duplicated, sorted set of HTTP methods observed (an
	// upper-cased "CONNECT" appears for tunneled TLS). May be empty when the
	// proxy only saw opaque CONNECTs without a method or recorded none.
	Methods []string `json:"methods,omitempty"`
	// AllowCount/DenyCount/PendingCount are the number of egress.allow/deny/
	// pending decisions recorded for this host.
	AllowCount   int `json:"allow_count"`
	DenyCount    int `json:"deny_count"`
	PendingCount int `json:"pending_count"`
}

// Observations is the deterministic aggregate of what a run actually used,
// computed purely from its already-captured audit events. Every slice is
// de-duplicated and sorted, so equal evidence yields equal Observations.
type Observations struct {
	// Domains is the per-host egress aggregate (deduped, sorted by host).
	Domains []DomainObservation `json:"domains,omitempty"`
	// MintedGrantIDs is the deduped, sorted set of grant ids the run SUCCESSFULLY
	// minted a credential for (credential.mint with outcome=success). A denied or
	// failed mint is NOT included — it did not actually yield a credential.
	MintedGrantIDs []uuid.UUID `json:"minted_grant_ids,omitempty"`
	// ExecArgv0s is the deduped, sorted set of argv[0] (program paths) the kernel
	// sensor observed the run exec.
	ExecArgv0s []string `json:"exec_argv0s,omitempty"`
	// FileWrites is the deduped, sorted set of sensitive file paths the kernel
	// sensor observed the run write.
	FileWrites []string `json:"file_writes,omitempty"`
	// Connects is the deduped, sorted set of "ip:port" destinations the kernel
	// sensor observed the run connect to.
	Connects []string `json:"connects,omitempty"`
	// Anomalies is the deduped, sorted set of human-readable signals a
	// least-privilege synthesis must NOT silently bless. It captures exactly:
	// an egress.deny during the open recording, an unmapped kernel connect
	// (possible proxy bypass), an exec of a dynamic linker (the ld-linux/mmap
	// execve-hook bypass surface), and a failed/escape kernel connect.
	Anomalies []string `json:"anomalies,omitempty"`
}

// domainAgg is the mutable per-host accumulator used while capturing.
type domainAgg struct {
	methods              map[string]bool
	allow, deny, pending int
}

// egressData is the JSON shape of an egress.* audit event's Data (the map
// handlePostDecision marshals: {host,port,method,path,rule_source,approval_id}).
type egressData struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	RuleSource string `json:"rule_source"`
}

// mintData is the subset of a credential.mint audit event's Data we read
// (broker.auditMint marshals {grant_id, scope, approval_id?, jti?}).
type mintData struct {
	GrantID string `json:"grant_id"`
}

// Capture aggregates one run's already-captured audit events into a deduped,
// sorted Observations. It is pure and input-order independent: it reads only the
// egress.*, credential.mint, and kernel.* (eBPF ground-truth) streams and
// silently ignores any other action, so it is robust to new audit verbs.
func Capture(events []types.AuditEvent) Observations {
	domains := map[string]*domainAgg{}
	minted := map[uuid.UUID]bool{}
	execs := map[string]bool{}
	files := map[string]bool{}
	connects := map[string]bool{}
	anomalies := map[string]bool{}

	for _, ev := range events {
		switch ev.Action {
		case actionEgressAllow, actionEgressDeny, actionEgressPending:
			captureEgress(ev, domains, anomalies)
		case actionCredentialMint:
			// Only a SUCCESSFUL mint actually yielded a credential the run used.
			if ev.Outcome == outcomeSuccess {
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

	return Observations{
		Domains:        buildDomains(domains),
		MintedGrantIDs: sortedUUIDs(minted),
		ExecArgv0s:     sortedStrings(execs),
		FileWrites:     sortedStrings(files),
		Connects:       sortedStrings(connects),
		Anomalies:      sortedStrings(anomalies),
	}
}

// captureEgress folds one egress.* decision into the per-host aggregate and
// records an anomaly for a deny seen during the (open) recording.
func captureEgress(ev types.AuditEvent, domains map[string]*domainAgg, anomalies map[string]bool) {
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
	case actionEgressPending:
		agg.pending++
	case actionEgressDeny:
		agg.deny++
		rs := strings.TrimSpace(d.RuleSource)
		if rs == "" {
			rs = "unknown"
		}
		anomalies[fmt.Sprintf("egress.deny to %s during open recording (rule_source=%s)", host, rs)] = true
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
	// Trust the sensor's loader flag, but also re-derive it (defense in depth:
	// the ld-linux/mmap execve-hook bypass is surfaced, never assumed absent).
	if d.Loader || groundtruth.IsDynamicLinker(argv0) {
		anomalies[fmt.Sprintf("dynamic-linker exec (loader) argv=[%s] — ld-linux/mmap execve-hook bypass surface", strings.Join(d.Argv, " "))] = true
	}
}

// captureConnect records a connect destination and flags unmapped / failed
// connects (a possible proxy bypass, or a reach to the cloud metadata IP —
// the only destination the mapper stamps outcome=failure post-FIX #17).
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
	if d.Correlation == groundtruth.CorrelationUnmapped {
		anomalies[fmt.Sprintf("unmapped kernel connect to %s (correlation=unmapped; possible proxy bypass)", label)] = true
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

// Synthesize derives a tightened, least-privilege RunPolicySpec from the
// Observations of an open run plus the run's grant catalog and the run itself.
// It is pure and deterministic, and returns the spec alongside human-readable
// warnings explaining every tightening decision and everything it deliberately
// did NOT auto-author. See the package doc for the honesty guarantees.
func Synthesize(obs Observations, runGrants []types.CredentialGrant, run types.AgentRun) (types.RunPolicySpec, []string) {
	var warnings []string
	var spec types.RunPolicySpec

	// ── Egress allowlist: EXACT hosts that were actually ALLOWED. ──
	// A host that was only denied or only held pending is NOT added — promoting a
	// denied host would WIDEN past what the open run was permitted. Never wildcard.
	var allowed []string
	for _, d := range obs.Domains {
		switch {
		case d.AllowCount > 0:
			allowed = append(allowed, d.Host)
		case d.DenyCount > 0:
			warnings = append(warnings, fmt.Sprintf("host %s observed but only DENIED (never allowed); excluded from allowed_domains", d.Host))
		default: // pending only
			warnings = append(warnings, fmt.Sprintf("host %s observed but only PENDING (never allowed); excluded from allowed_domains", d.Host))
		}
	}
	sort.Strings(allowed)
	spec.AllowedDomains = allowed
	if len(allowed) == 0 {
		warnings = append(warnings, "no allowed egress observed; synthesized spec denies ALL egress (allow_all_egress=false, empty allowlist) — confirm the run genuinely needed none")
	}

	// ── Forced invariants (mitigate the recording's inherent under-coverage). ──
	// The snapshot can only prove what the run HAPPENED to use, never the full set
	// it may need, so we fail toward human escalation, not silent denial.
	spec.AllowAllEgress = false // recordings exist to REMOVE allow-all.
	// Unknown (un-recorded) hosts escalate to a human, not silently fail. Use
	// deny_with_review (raise + retry) — the direct successor to the legacy
	// forced-true; not wait_for_review, so a replay never hangs unattended.
	spec.FirstUseApproval = types.FirstUseDenyWithReview
	spec.AllowedMethods = nil // method capture is brittle; do not over-restrict.

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

// githubPermSummary renders a github_token grant scope's permissions map as a
// sorted "k:v" list for an operator-facing warning (mirrors risk.go's parsing).
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

// buildDomains converts the per-host accumulator into the sorted DomainObservation
// slice (hosts sorted; each host's methods deduped+sorted).
func buildDomains(m map[string]*domainAgg) []DomainObservation {
	if len(m) == 0 {
		return nil
	}
	hosts := make([]string, 0, len(m))
	for h := range m {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	out := make([]DomainObservation, 0, len(hosts))
	for _, h := range hosts {
		a := m[h]
		out = append(out, DomainObservation{
			Host:         h,
			Methods:      sortedStrings(a.methods),
			AllowCount:   a.allow,
			DenyCount:    a.deny,
			PendingCount: a.pending,
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
