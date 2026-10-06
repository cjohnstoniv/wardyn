// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// handlePostDecision ingests an egress decision log from the proxy and persists
// it as an append-only audit event. The action is egress.<decision> with
// actor_type=agent (the proxy acts on behalf of the run). The run id is taken
// from the verified token claims, NOT from the body (the body is advisory).
func (s *Server) handlePostDecision(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunClaims, "missing run claims")
		return
	}
	var dl egress.DecisionLog
	// Capped generously (maxJSONBody, 1 MiB): a decision log is a host/port/path
	// plus an optional scan summary, so nothing legitimate comes near it, and a
	// too-tight cap here would DROP an egress audit record behind a 413.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&dl); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInternalDecisionLogInvalid, "invalid decision log")
		return
	}
	normalizeN1Decision(&dl)

	runID := claims.RunID

	// An egress decision means the agent actually did something, so it resets the
	// idle clock the reaper reads (agent_runs.updated_at) — the seam
	// internal/lifecycle documents. Best-effort like the attach keepalive; a nil
	// Store (test harness) and an unknown run id are both non-events.
	// ponytail: only runs that make egress calls stay alive — a pure-local-compute
	// run is still wall-clocked. Runner-reported liveness is the upgrade path.
	// Debounced: a chatty agent can emit many decisions a second, and each touch
	// is an UPDATE on the same agent_runs row; the reaper thresholds are minutes,
	// so one touch per touchDebounce per run loses nothing.
	if s.cfg.Store != nil && s.shouldTouch(runID, dl.RuleSource) {
		_ = s.cfg.Store.TouchRun(r.Context(), runID)
	}
	// The same decision moves the pause's presence clock (run_pause.go).
	if agentActivityDecision(dl.RuleSource) {
		s.noteAgentActive(r.Context(), runID)
	}

	// A synthetic "bypass" decision is PURELY an LLM-inspection coverage signal
	// (an opaque CONNECT to a model host that could not be inspected). Emit only
	// the llm.scan.bypass degradation event — not a duplicate egress.allow for
	// the tunnel, which the real CONNECT decision already recorded.
	if dl.Scan != nil && dl.Scan.Action == "bypass" {
		s.recordLLMScanAudit(r.Context(), runID, claims.SPIFFEID, r.RemoteAddr, dl.Scan, dl.Request.Host)
		writeJSON(w, http.StatusAccepted, nil)
		return
	}

	fields := map[string]any{
		"host":        dl.Request.Host,
		"port":        dl.Request.Port,
		"method":      dl.Request.Method,
		"path":        dl.Request.Path,
		"rule_source": dl.RuleSource,
		"approval_id": dl.ApprovalID,
	}
	// Streak-summary only: an ordinary decision's data column stays as it was.
	if dl.Repeat > 0 {
		fields["repeat"] = dl.Repeat
	}
	// Cause/Via ride a dial- or tunnel-shaped failure only (egress.DecisionLog's
	// own doc comments) — absent on every ordinary decision, same as repeat above.
	if dl.Cause != "" {
		fields["cause"] = dl.Cause
	}
	if dl.Via != "" {
		fields["via"] = dl.Via
	}
	// A Bedrock data-plane refusal the proxy relayed (bedrock_dataplane_fault.go).
	if dl.UpstreamFault != "" {
		fields["upstream_fault"] = dl.UpstreamFault
		s.noteBedrockDataPlaneFault(r.Context(), runID, dl.UpstreamFault)
	}
	data, _ := json.Marshal(fields)
	outcome := decisionOutcome(dl.Decision)
	ev := s.auditEvent(&runID, types.ActorAgent, claims.SPIFFEID,
		"egress."+string(dl.Decision), dl.Request.Host, outcome, data)
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)
	// wardyn_egress_denies_total is exposed as "denied by policy", and it
	// is the only egress counter Wardyn has. A builtin:dial-failed (a flaky
	// upstream, on a request policy ALLOWED), a builtin:tunnel-failed (a
	// connection that died after it opened) and the synthetic
	// egress:dropped-decisions-<n> audit-fidelity summary all arrive here as
	// egress.Deny; counting them would page operators for policy denials that
	// never happened and make the true deny rate unreadable off the series. Each
	// still records its egress.deny AUDIT row unchanged — only the counter is
	// scoped. See isPolicyDeny (metrics.go): llm_routes.go's gatewayTarget GUARD
	// refusal has its own rule_source, so it counts here like any other guard
	// denial.
	if dl.Decision == egress.Deny && isPolicyDeny(dl.RuleSource) {
		s.metrics.egressDenied()
	}
	s.countReauthTimeout(dl)

	// Optional outbound content-inspection summary rides the same decision. When
	// present it becomes a SEPARATE, content-free llm.scan.* audit event so the
	// model-channel inspection is independently visible/queryable in the log.
	if dl.Scan != nil {
		s.recordLLMScanAudit(r.Context(), runID, claims.SPIFFEID, r.RemoteAddr, dl.Scan, dl.Request.Host)
	}

	writeJSON(w, http.StatusAccepted, nil)
}

// maxAuditFindings bounds how many per-finding records one llm.scan.* audit
// event embeds. finding_count stays the honest total, so truncation costs
// detail, never the signal — and one pathological scan cannot turn an
// append-only row (fanned to every SIEM sink) into a megabyte.
const maxAuditFindings = 100

// recordLLMScanAudit records a CONTENT-FREE llm.scan.* audit event for an
// outbound content-inspection pass. The Data payload carries detector names,
// field paths, offsets, counts and MASKED samples only — never the matched
// bytes and never a reversible hash (the audit log is append-only and fans to
// every SIEM sink, so it must not become a durable copy of a secret).
func (s *Server) recordLLMScanAudit(ctx context.Context, runID uuid.UUID, actor, sourceIP string, sc *egress.ScanSummary, host string) {
	if sc == nil {
		return
	}
	findings := sc.Findings
	if len(findings) > maxAuditFindings {
		findings = findings[:maxAuditFindings]
	}
	outcome := "success"
	switch sc.Action {
	case "block":
		outcome = "denied"
	case "fail":
		outcome = "failure"
	}
	// finding_count is the number of findings the scan PRODUCED before the cap
	// truncated the list, which the proxy counts as it produces them and
	// sends as findings_total: the proxy caps how many findings it REPORTS, so
	// len(sc.Findings) is the reported count, and an audit row that states it as
	// the finding count makes a truncated scan indistinguishable from one that
	// found exactly the cap. findings_capped / findings_past_cap carry the
	// truncation itself, and findings_past_cap is an UPPER bound on what was
	// pushed out (block mode's severity keep-backs are counted past the cap and
	// still reported), which is why finding_count is not derived from it.
	findingCount := len(sc.Findings)
	if sc.FindingsTotal > findingCount {
		findingCount = sc.FindingsTotal
	}
	data, _ := json.Marshal(map[string]any{
		"host":              host,
		"channel":           sc.Channel,
		"mode":              sc.Mode,
		"coverage":          sc.Coverage,
		"scanned":           sc.Scanned,
		"skipped":           sc.Skipped,
		"skip_reason":       sc.SkipReason,
		"finding_count":     findingCount,
		"findings_reported": len(sc.Findings),
		"findings_capped":   sc.FindingsCapped,
		"findings_past_cap": sc.FindingsPastCap,
		"findings":          findings,
	})
	ev := s.auditEvent(&runID, types.ActorAgent, actor,
		"llm.scan."+sc.Action, host, outcome, data)
	ev.SourceIP = sourceIP
	s.recordAudit(ctx, ev)
}

// groundtruthBatch is the POST /api/v1/internal/groundtruth body: a batch of
// kernel-derived audit events from the host eBPF sensor (wardyn-tetragon-ingest).
type groundtruthBatch struct {
	Events []types.AuditEvent `json:"events"`
}

// maxGroundtruthBatchBytes caps the sensor's batch POST, and maxSidecarBody the
// two small sidecar-authored bodies (an approval's requested_scope, mint). All
// three are DoS ceilings on a compromised sidecar/sensor, not shape checks —
// the real bounds are maxBatch below and the request structs themselves.
// maxApprovalRaiseBody is a raise with a push's path list beside its scope:
// the list's bytes, each JSON-escapable to six ("\u00XX"), plus three per
// quoted, comma-separated entry.
const (
	maxGroundtruthBatchBytes = 8 << 20  // 8 MiB
	maxSidecarBody           = 64 << 10 // 64 KiB
	maxApprovalRaiseBody     = maxSidecarBody + 6*types.PushPathListMaxBytes + 3*types.PushPathListMaxPaths
)

// handleGroundtruthEvents ingests a batch of eBPF/Tetragon kernel events from
// the host-scoped sensor and persists each as an append-only audit event — the
// SECOND audit stream. Because it routes through s.recordAudit, every event
// lands in Postgres AND fans to every configured SIEM sink with ZERO new fanout
// code, keyed on run_id and discriminated by the kernel.* action prefix +
// data.stream="ebpf".
//
// Security model (deliberate deviation, commented):
//   - Unlike handlePostDecision, the run_id is taken from the BODY, not from
//     token claims. This is the one intentional deviation from the
//     token-derived pattern: the sensor is HOST-scoped, not per-run, so it has
//     no single run identity to bind. Each kernel event names its own run (or
//     NULL for unmapped/heartbeat events). RESIDUAL: a compromised host sensor
//     could therefore MIS-ATTRIBUTE an event to the wrong run. This is mitigated
//     by (a) FORCING actor_type=system + actor="wardyn-tetragon-ingest" on every
//     event server-side (the sensor can never impersonate a human or an agent
//     run), (b) the audit-write-only token scope (aud=wardyn-groundtruth cannot
//     mint or approve), and (c) validating any non-NULL run_id against
//     agent_runs: a run_id naming no real run (stale/orphaned after a DB
//     reset/re-point or a run-row purge, or a forged id) is DOWNGRADED to
//     unmapped — run_id cleared, data.correlation="unmapped",
//     data.reason="run_id_not_found" — rather than rejected, so one bad
//     correlation costs only that event's attribution, never the rest of the
//     batch (see handleGroundtruthEvents below). The residual is published,
//     not hidden.
//   - Every event's action MUST carry the "kernel." prefix; anything else is
//     rejected (the sensor cannot forge an egress./credential./identity. event).
//   - run_id NULL is allowed (unmapped events + heartbeat + blind events).
func (s *Server) handleGroundtruthEvents(w http.ResponseWriter, r *http.Request) {
	var batch groundtruthBatch
	// Byte ceiling under maxBatch below: 1000 kernel events legitimately exceed
	// maxJSONBody, so this stream gets its own larger cap rather than 413ing a
	// full sensor batch.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxGroundtruthBatchBytes)).Decode(&batch); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonGroundtruthBatchInvalid, "invalid ground-truth batch")
		return
	}
	if len(batch.Events) == 0 {
		writeJSON(w, http.StatusAccepted, map[string]any{"accepted": 0})
		return
	}
	const maxBatch = 1000
	if len(batch.Events) > maxBatch {
		writeErrorReason(w, http.StatusRequestEntityTooLarge, reasonGroundtruthBatchTooLarge, "batch too large")
		return
	}

	// Phase 1 — validate the whole batch before committing any event.
	//
	// FINDING (medium, fixed): the old loop validated-and-committed interleaved,
	// so a single bad event (non-kernel action or a run_id naming no real run)
	// AFTER one or more good events left the good ones already persisted while the
	// response was a 4xx — silently LOSING events AND miscounting (the caller saw
	// a reject and could not know what landed). We now validate every event first;
	// a single bad event rejects the whole batch atomically with nothing
	// committed, so good events are never half-dropped behind a 4xx. Validation is
	// read-only (kernel-prefix check + run_id existence), so doing it up front is
	// cheap and side-effect-free.
	for i := range batch.Events {
		ev := &batch.Events[i]
		// Enforce the kernel.* namespace (fail closed): the host sensor may only
		// write kernel-prefixed events. This prevents a compromised sensor from
		// forging egress./credential./identity./policy. events.
		if !strings.HasPrefix(ev.Action, groundtruth.KernelActionPrefix) {
			writeErrorReason(w, http.StatusBadRequest, reasonGroundtruthActionNotKernel, "action must use the kernel. prefix")
			return
		}
		// Validate a non-NULL run_id against agent_runs. NULL is allowed for
		// unmapped events, the sensor heartbeat, and blind events. A run_id
		// that names no real run (stale/orphaned after a DB reset/re-point or
		// a run-row purge, or a forged id) is DOWNGRADED to unmapped rather
		// than rejecting the whole batch: this stream's own rule is that
		// blindness must stay visible, never that a good event gets dropped
		// behind someone else's bad one. One stale correlation now costs only
		// that event's attribution instead of every co-batched event (and any
		// heartbeat) behind it. Only a genuine store failure stays a hard
		// error (fail closed).
		if ev.RunID != nil {
			if _, err := s.cfg.Store.GetRun(r.Context(), *ev.RunID); err != nil {
				if !errors.Is(err, store.ErrNotFound) {
					writeServerError(w, r, "validate run_id", err)
					return
				}
				ev.RunID = nil
				ev.Data = downgradeToUnmapped(ev.Data, "run_id_not_found")
			}
		}
	}

	// Phase 2 — commit. Every event in the batch is now known-valid.
	//
	// FINDING (medium, fixed): a write failure on this "tamper-proof" stream used
	// to be SWALLOWED (recordAudit ignores the Recorder error) yet the endpoint
	// still reported the events accepted — so a Postgres blip silently dropped
	// ground-truth events with no chance to recover. We now record through the
	// Recorder directly and, on ANY write failure, fail CLOSED with a 502 so the
	// sender retries the batch (durability over a false 202). The whole batch is
	// retried, which is safe: audit_events are append-only and the stream is a
	// detection feed, so a rare duplicate on retry is acceptable; silent loss is
	// not.
	accepted := 0
	for _, ev := range batch.Events {
		// FORCE attribution server-side: the sensor can never set actor_type or
		// actor to anything but the fixed system sensor identity.
		ev.ActorType = types.ActorSystem
		ev.Actor = groundtruth.SensorActor
		ev.SourceIP = r.RemoteAddr
		// FORCE the event time server-side too. /healthz keys ebpf_groundtruth
		// health off Now().Sub(latest heartbeat Time) <= TTL, so a supplied future
		// Time would make the diff negative and pin "healthy" forever even after
		// the sensor dies. The sensor sends zero Time on the normal path, so
		// clamping to our own clock costs nothing and keeps honest degradation.
		ev.Time = s.cfg.Now().UTC()
		if err := s.recordGroundtruthAudit(r.Context(), ev); err != nil {
			// Propagate as a non-2xx so the sender retries (fail-closed
			// durability). accepted so far is not reported as success: the caller
			// re-sends the whole batch.
			writeErrorReason(w, http.StatusBadGateway, reasonGroundtruthWriteFailed, loggedMsg(r.Context(), "record ground-truth event", err))
			return
		}
		accepted++
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": accepted})
}

// downgradeToUnmapped marks a kernel event's data unmapped with reason,
// preserving every other field the sensor already set (subtype, cgroup_id,
// container_id, argv/dst/path, loader, ...). Used when a sensor-supplied
// run_id fails validation (see the Phase 1 loop above): the event is still
// recorded — blindness must stay visible, per this stream's own rule —
// just without the stale/forged run attribution.
func downgradeToUnmapped(data json.RawMessage, reason string) json.RawMessage {
	var ed groundtruth.EventData
	_ = json.Unmarshal(data, &ed) // best-effort; missing/invalid data still gets marked
	ed.Correlation = groundtruth.CorrelationUnmapped
	ed.Reason = reason
	out, err := json.Marshal(ed)
	if err != nil {
		return data
	}
	return out
}

// recordGroundtruthAudit records a ground-truth event and RETURNS the Recorder
// error (unlike s.recordAudit, which deliberately swallows it for best-effort
// control-plane events). The ground-truth stream is the "tamper-proof"
// counterpart to the agent self-report, so a durability failure must be
// surfaced to the sender (a non-2xx) for retry, not silently dropped. It mirrors
// recordAudit's ID/Time defaulting so the stored event is well-formed. A nil
// Recorder is treated as success (no store wired — nothing to persist to).
func (s *Server) recordGroundtruthAudit(ctx context.Context, ev types.AuditEvent) error {
	if s.cfg.Audit == nil {
		return nil
	}
	if ev.ID == uuid.Nil {
		ev.ID = uuid.New()
	}
	if ev.Time.IsZero() {
		ev.Time = s.cfg.Now().UTC()
	}
	return s.cfg.Audit.Record(ctx, ev)
}

// maxApprovalsPerRun caps how many approvals ONE run may ever raise, in any
// state. The sandbox chooses the hosts and tools it asks about, so the row count
// a single run can create was bounded by nothing at all: the dedup guard
// collapses repeats of the SAME scope, and a thousand DIFFERENT unknown hosts is
// a thousand rows plus a thousand queue entries in front of a human. 4096 is
// deliberately far above any legitimate run (the `always_deny` and
// already-approved paths raise nothing, and a real run asks about a handful of
// hosts) and low enough to bound one run's share of the approvals table.
//
// Past it the raise answers 429 and writes NO audit action of its own: the
// refusal is a rate bound, not a security event — the 4096 rows it already
// raised are the trail, and inventing an action here would mean a run that
// hits the cap floods the audit log with the refusal instead, which is the same
// mistake one wave over.
const maxApprovalsPerRun = 4096

// internalApprovalRequest is the proxy's POST /internal/approvals body.
//
// RequestedScope is stored VERBATIM and is what the approver is shown, so its
// shape is a contract with the console rather than an internal detail — see
// docs/AUDIT-ACTIONS.md and the per-kind scopes in internal/egress/proxy
// (egressScope) and the toolgate. For an `egress_domain` scope specifically:
// the host it carries is a BARE host and the decision it records is
// HOST-WIDE, reaching every port of that host for
// whatever span its decision_scope names. The sidecar guarantees the bare host
// (approvalHostKey, which is also what it keys its own approval cache on), and
// hostrules.ValidApprovedHost refuses a port by construction on the durable
// always-write. Port scoping is a planned change that moves all three of those
// at once; it is deliberately NOT a field added here alone.
type internalApprovalRequest struct {
	Kind           types.ApprovalKind `json:"kind"`
	RequestedScope json.RawMessage    `json:"requested_scope"`
	// PathList is a push_content raise's complete path list; a previous-release
	// sidecar sends none. Refused on any other kind.
	PathList *types.PushPathList `json:"path_list"`
}

// handleInternalRequestApproval raises (or dedups to an existing) approval on
// behalf of a run — the first-use egress approval flow. The run id is bound from
// the verified token, never the body.
func (s *Server) handleInternalRequestApproval(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunClaims, "missing run claims")
		return
	}
	var body internalApprovalRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxApprovalRaiseBody)).Decode(&body); err != nil ||
		len(body.RequestedScope) > maxSidecarBody || (body.PathList != nil && body.Kind != types.ApprovalPushContent) {
		writeErrorReason(w, http.StatusBadRequest, reasonInternalApprovalRequestInvalid, "invalid approval request")
		return
	}
	switch body.Kind {
	case types.ApprovalEgressDomain, types.ApprovalToolCall, types.ApprovalPushContent:
		// Sidecars may only raise egress/tool/held-push approvals. credential
		// approvals are created by the broker mint path, never by an untrusted
		// sidecar.
	default:
		// Recorded: this refusal is the forgery the case above exists to
		// stop — a sidecar asking Wardyn to raise a `credential` approval. Same
		// rate-bound auth.fail row, limiter and suppressed counter as every
		// other refusal; the KIND is a closed enum of our own types, never
		// echoed from the body. The SAME string is now the wire reason too.
		s.auditAuthFailedAs(r, internalApprovalActor, reasonUnsupportedInternalApprovalKind)
		writeErrorReason(w, http.StatusBadRequest, reasonUnsupportedInternalApprovalKind, "unsupported approval kind for internal request")
		return
	}
	if len(body.RequestedScope) == 0 {
		s.auditAuthFailedAs(r, internalApprovalActor, reasonMissingRequestedScope)
		writeErrorReason(w, http.StatusBadRequest, reasonMissingRequestedScope, "requested_scope is required")
		return
	}
	// `lane` names a control-plane-raised escalation (the Azure DevOps
	// capability hold). Decidability does not key off it — it keys off
	// grant_id, which this route never sets — but a sidecar that tries to
	// write it is probing that boundary, so it is refused and recorded.
	if scopeNamesLane(body.RequestedScope) {
		s.auditAuthFailedAs(r, internalApprovalActor, reasonReservedScopeKey)
		writeErrorReason(w, http.StatusBadRequest, reasonReservedScopeKey, "requested_scope may not name a lane")
		return
	}
	if body.Kind == types.ApprovalPushContent {
		var ok bool
		if body.RequestedScope, ok = s.admitPushContentRaise(w, r, claims, body.RequestedScope, body.PathList); !ok {
			return
		}
	}

	// Per-run cap, checked BEFORE the raise. Fail CLOSED on a count
	// error: an unbounded raise path is the thing being bounded, so "we could not
	// tell how many this run has" must not read as "allow another one".
	n, cerr := s.cfg.Approvals.CountForRun(r.Context(), claims.RunID)
	if cerr != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonInternalApprovalCountUnavailable, loggedMsg(r.Context(), "count approvals for run", cerr))
		return
	}
	if n >= maxApprovalsPerRun {
		writeErrorReason(w, http.StatusTooManyRequests, reasonInternalApprovalCapReached, "this run has raised too many approvals; no more will be accepted")
		return
	}

	req := types.ApprovalRequest{
		RunID:          claims.RunID,
		Kind:           body.Kind,
		RequestedScope: body.RequestedScope,
	}
	created, err := s.cfg.Approvals.Request(r.Context(), req)
	if err != nil {
		writeServerError(w, r, "request approval", err)
		return
	}
	if body.PathList != nil && !s.recordPushPathList(w, r, claims, created, *body.PathList) {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// scopeNamesLane reports whether a top-level `lane` key, in any letter case
// (encoding/json matches keys case-insensitively), is present.
func scopeNamesLane(scope json.RawMessage) bool {
	var top map[string]json.RawMessage
	if json.Unmarshal(scope, &top) != nil {
		return false
	}
	for k := range top {
		if strings.EqualFold(k, "lane") {
			return true
		}
	}
	return false
}

// handleInternalGetApproval lets a sidecar poll the state of an approval it
// raised. It may only read approvals belonging to its own run (fail closed).
func (s *Server) handleInternalGetApproval(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunClaims, "missing run claims")
		return
	}
	id, ok := parseIDParam(w, r, "id", "approval")
	if !ok {
		return
	}
	ap, err := s.cfg.Approvals.Get(r.Context(), id)
	if notFoundIf(w, err, "approval", reasonApprovalNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "get approval", err)
		return
	}
	if ap.RunID != claims.RunID {
		// Do not confirm existence of another run's approval.
		writeErrorReason(w, http.StatusNotFound, reasonApprovalNotFound, "approval not found")
		return
	}
	// Reconcile-on-read for a mid-run credential re-auth: a PENDING row whose
	// owner's stored credential was captured by a login run created after the
	// raise IS resolved, it just has not been written down yet (the capture and
	// the resolution are two writes, and a crash between them would otherwise
	// strand a valid credential behind a PENDING row until the hold's budget
	// ended, with no second human action able to repair it). Idempotent,
	// derivable from capture provenance, and gated on exactly what the eager
	// path checks. A no-op for every other kind and state.
	ap = s.reconcileReauthOnRead(r.Context(), ap)
	// The same repair for an Azure DevOps consent or sign-in request: the
	// person's new sign-in is the resolution (injection_ado_signin.go).
	ap = s.reconcileADOReauthOnRead(r.Context(), ap)
	ap = s.reconcileAzureReauthOnRead(r.Context(), ap)
	writeJSON(w, http.StatusOK, ap)
}

// handleInternalExpireApproval lets wardyn-toolgate close its OWN tool_call
// approval the moment it gives up waiting for one (its -deadline reached, or
// its poll loop otherwise exhausted), instead of leaving the row PENDING for
// the periodic sweep (approval-expiry-after/-interval) to catch up to — up to
// one sweep interval later. In that window an operator could still approve a
// call the gate has already answered deny for (#811).
//
// Scoped exactly like handleInternalGetApproval (own run only, 404 on any
// mismatch so existence is never confirmed for another run's row) plus one
// more restriction: only a tool_call approval the SANDBOX raised
// (handleBrokerCreateApproval) may be expired here — a grant_id marks a row
// the control plane raised (an Azure DevOps escalation, adoEscalationScope),
// which is the operator's to decide, not the sandbox's to withdraw. The
// transition itself is idempotent — an approval a human or the sweep already
// decided is left untouched — and the answer is the row's FINAL state, so a
// gate whose expire lost the race to an approval honours that approval.
func (s *Server) handleInternalExpireApproval(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunClaims, "missing run claims")
		return
	}
	id, ok := parseIDParam(w, r, "id", "approval")
	if !ok {
		return
	}
	ap, err := s.cfg.Approvals.Get(r.Context(), id)
	if notFoundIf(w, err, "approval", reasonApprovalNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "get approval", err)
		return
	}
	if ap.RunID != claims.RunID || ap.Kind != types.ApprovalToolCall || ap.GrantID != nil {
		// Do not confirm existence of another run's approval, or of an
		// approval this route was never meant to touch.
		writeErrorReason(w, http.StatusNotFound, reasonApprovalNotFound, "approval not found")
		return
	}
	if err := s.cfg.Approvals.ExpireOne(r.Context(), id, claims.SPIFFEID, "client_withdrawn"); err != nil {
		writeServerError(w, r, "expire approval", err)
		return
	}
	if ap, err = s.cfg.Approvals.Get(r.Context(), id); err != nil {
		writeServerError(w, r, "get approval", err)
		return
	}
	writeJSON(w, http.StatusOK, ap)
}

// mintRequest is the in-sandbox credential helper's mint body.
type mintRequest struct {
	GrantID uuid.UUID `json:"grant_id"`
}

// mintResponse mirrors the documented success shape:
// 200 {"kind","token","jti","expires_at"}. For api_key grants the secret value
// is never returned; the injection rule is included so the proxy can wire it.
// For git_pat the stored PAT value is returned in Token plus the resolved git
// Username (ADO=pat, GitLab=oauth2, or an explicit override).
type mintResponse struct {
	Kind      types.GrantKind       `json:"kind"`
	Token     string                `json:"token,omitempty"`
	Username  string                `json:"username,omitempty"`
	JTI       string                `json:"jti"`
	ExpiresAt string                `json:"expires_at"`
	Injection *egress.InjectionRule `json:"injection,omitempty"`
	// KnownHosts carries operator-supplied OpenSSH known_hosts material for an
	// ssh_key grant (empty otherwise; agent-run falls back to the image-baked
	// /etc/ssh/ssh_known_hosts). Public host-key data, not a secret.
	KnownHosts string `json:"known_hosts,omitempty"`
}
