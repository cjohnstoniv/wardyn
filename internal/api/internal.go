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

	"github.com/cjohnstoniv/wardyn/internal/broker"
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
		writeError(w, http.StatusUnauthorized, "missing run claims")
		return
	}
	var dl egress.DecisionLog
	if err := json.NewDecoder(r.Body).Decode(&dl); err != nil {
		writeError(w, http.StatusBadRequest, "invalid decision log")
		return
	}

	runID := claims.RunID

	// A synthetic "blind" decision is PURELY an LLM-inspection coverage signal
	// (an opaque CONNECT to a model host that could not be inspected). Emit only
	// the llm.scan.blind degradation event — not a duplicate egress.allow for
	// the tunnel, which the real CONNECT decision already recorded.
	if dl.Scan != nil && dl.Scan.Action == "blind" {
		s.recordLLMScanAudit(r.Context(), runID, claims.SPIFFEID, r.RemoteAddr, dl.Scan, dl.Request.Host)
		writeJSON(w, http.StatusAccepted, nil)
		return
	}

	data, _ := json.Marshal(map[string]any{
		"host":        dl.Request.Host,
		"port":        dl.Request.Port,
		"method":      dl.Request.Method,
		"path":        dl.Request.Path,
		"rule_source": dl.RuleSource,
		"approval_id": dl.ApprovalID,
	})
	outcome := decisionOutcome(dl.Decision)
	ev := s.auditEvent(&runID, types.ActorAgent, claims.SPIFFEID,
		"egress."+string(dl.Decision), dl.Request.Host, outcome, data)
	ev.SourceIP = r.RemoteAddr
	s.recordAudit(r.Context(), ev)

	// Optional outbound content-inspection summary rides the same decision. When
	// present it becomes a SEPARATE, content-free llm.scan.* audit event so the
	// model-channel inspection is independently visible/queryable in the log.
	if dl.Scan != nil {
		s.recordLLMScanAudit(r.Context(), runID, claims.SPIFFEID, r.RemoteAddr, dl.Scan, dl.Request.Host)
	}

	writeJSON(w, http.StatusAccepted, nil)
}

// recordLLMScanAudit records a CONTENT-FREE llm.scan.* audit event for an
// outbound content-inspection pass. The Data payload carries detector names,
// field paths, offsets, counts and MASKED samples only — never the matched
// bytes and never a reversible hash (the audit log is append-only and fans to
// every SIEM sink, so it must not become a durable copy of a secret).
func (s *Server) recordLLMScanAudit(ctx context.Context, runID uuid.UUID, actor, sourceIP string, sc *egress.ScanSummary, host string) {
	if sc == nil {
		return
	}
	outcome := "success"
	switch sc.Action {
	case "block":
		outcome = "denied"
	case "error":
		outcome = "failure"
	}
	data, _ := json.Marshal(map[string]any{
		"host":          host,
		"channel":       sc.Channel,
		"mode":          sc.Mode,
		"coverage":      sc.Coverage,
		"scanned":       sc.Scanned,
		"skipped":       sc.Skipped,
		"skip_reason":   sc.SkipReason,
		"finding_count": len(sc.Findings),
		"findings":      sc.Findings,
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

// handleGroundtruthEvents ingests a batch of eBPF/Tetragon kernel events from
// the host-scoped sensor and persists each as an append-only audit event — the
// SECOND audit stream. Because it routes through s.recordAudit, every event
// lands in Postgres AND fans to every configured SIEM sink with ZERO new fanout
// code, keyed on run_id and discriminated by the kernel.* action prefix +
// data.stream="ebpf".
//
// SECURITY MODEL (deliberate deviation, commented):
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
//     agent_runs (a forged run_id that names no real run is rejected). The
//     residual is published, not hidden.
//   - Every event's action MUST carry the "kernel." prefix; anything else is
//     rejected (the sensor cannot forge an egress./credential./identity. event).
//   - run_id NULL is allowed (unmapped events + heartbeat + blind events).
func (s *Server) handleGroundtruthEvents(w http.ResponseWriter, r *http.Request) {
	var batch groundtruthBatch
	if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid ground-truth batch")
		return
	}
	if len(batch.Events) == 0 {
		writeJSON(w, http.StatusAccepted, map[string]any{"accepted": 0})
		return
	}
	const maxBatch = 1000
	if len(batch.Events) > maxBatch {
		writeError(w, http.StatusRequestEntityTooLarge, "batch too large")
		return
	}

	// PHASE 1 — VALIDATE THE WHOLE BATCH BEFORE COMMITTING ANY EVENT.
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
	for _, ev := range batch.Events {
		// Enforce the kernel.* namespace (fail closed): the host sensor may only
		// write kernel-prefixed events. This prevents a compromised sensor from
		// forging egress./credential./identity./policy. events.
		if !strings.HasPrefix(ev.Action, groundtruth.KernelActionPrefix) {
			writeError(w, http.StatusBadRequest, "action must use the kernel. prefix")
			return
		}
		// Validate a non-NULL run_id against agent_runs. NULL is allowed for
		// unmapped events, the sensor heartbeat, and blind events. A forged
		// run_id that names no real run is rejected (fail closed).
		if ev.RunID != nil {
			if _, err := s.cfg.Store.GetRun(r.Context(), *ev.RunID); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					writeError(w, http.StatusBadRequest, "run_id does not exist")
					return
				}
				writeError(w, http.StatusInternalServerError, "validate run_id: "+err.Error())
				return
			}
		}
	}

	// PHASE 2 — COMMIT. Every event in the batch is now known-valid.
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
			writeError(w, http.StatusBadGateway, "record ground-truth event: "+err.Error())
			return
		}
		accepted++
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": accepted})
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

// internalApprovalRequest is the proxy's POST /internal/approvals body.
type internalApprovalRequest struct {
	Kind           types.ApprovalKind `json:"kind"`
	RequestedScope json.RawMessage    `json:"requested_scope"`
}

// handleInternalRequestApproval raises (or dedups to an existing) approval on
// behalf of a run — the first-use egress approval flow. The run id is bound from
// the verified token, never the body.
func (s *Server) handleInternalRequestApproval(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "missing run claims")
		return
	}
	var body internalApprovalRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid approval request")
		return
	}
	switch body.Kind {
	case types.ApprovalEgressDomain, types.ApprovalToolCall:
		// Sidecars may only raise egress/tool approvals. credential approvals are
		// created by the broker mint path, never by an untrusted sidecar.
	default:
		writeError(w, http.StatusBadRequest, "unsupported approval kind for internal request")
		return
	}
	if len(body.RequestedScope) == 0 {
		writeError(w, http.StatusBadRequest, "requested_scope is required")
		return
	}

	req := types.ApprovalRequest{
		RunID:          claims.RunID,
		Kind:           body.Kind,
		RequestedScope: body.RequestedScope,
	}
	created, err := s.cfg.Approvals.Request(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "request approval: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// handleInternalGetApproval lets a sidecar poll the state of an approval it
// raised. It may only read approvals belonging to its own run (fail closed).
func (s *Server) handleInternalGetApproval(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "missing run claims")
		return
	}
	id, ok := parseIDParam(w, r, "id", "approval")
	if !ok {
		return
	}
	ap, err := s.cfg.Approvals.Get(r.Context(), id)
	if notFoundIf(w, err, "approval") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get approval: "+err.Error())
		return
	}
	if ap.RunID != claims.RunID {
		// Do not confirm existence of another run's approval.
		writeError(w, http.StatusNotFound, "approval not found")
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

// handleInternalMint is the broker chokepoint over HTTP. The caller's verified
// claims (run + SPIFFE id) are passed to the broker, which enforces ownership,
// the approval gate, and the no-widening invariant inside a single transaction.
// Responses: 200 minted | 401 unauthorized | 409 {"approval_id"} pending/denied.
func (s *Server) handleInternalMint(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "missing run claims")
		return
	}
	if s.cfg.Broker == nil {
		writeError(w, http.StatusServiceUnavailable, "broker not configured")
		return
	}
	var body mintRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.GrantID == uuid.Nil {
		writeError(w, http.StatusBadRequest, "grant_id is required")
		return
	}

	minted, err := s.cfg.Broker.MintForGrant(r.Context(), claims, body.GrantID)
	if err != nil {
		s.writeMintError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mintResponse{
		Kind:       minted.Kind,
		Token:      minted.Token,
		Username:   minted.Username,
		JTI:        minted.JTI,
		ExpiresAt:  minted.ExpiresAt.UTC().Format(rfc3339),
		Injection:  minted.Injection,
		KnownHosts: minted.KnownHosts,
	})
}

// writeMintError maps broker errors to the documented fail-closed HTTP shape.
func (s *Server) writeMintError(w http.ResponseWriter, err error) {
	var pending broker.ErrApprovalPending
	if errors.As(err, &pending) {
		// Approval still open: 409 with the approval id so the caller can poll.
		writeJSON(w, http.StatusConflict, map[string]any{"approval_id": pending.ApprovalID})
		return
	}
	var denied broker.ErrApprovalDenied
	if errors.As(err, &denied) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"approval_id": denied.ApprovalID, "denied": true, "reason": denied.Reason,
		})
		return
	}
	switch {
	case errors.Is(err, broker.ErrRunMismatch):
		writeError(w, http.StatusForbidden, "caller run does not own this grant")
	case errors.Is(err, broker.ErrGrantNotFound):
		writeError(w, http.StatusNotFound, "grant not found")
	case errors.Is(err, broker.ErrRequiresSPIRE):
		writeError(w, http.StatusUnprocessableEntity, "grant requires the spire identity provider")
	case errors.Is(err, broker.ErrScopeMismatch):
		writeError(w, http.StatusConflict, "requested scope does not match grant (no-widening)")
	case errors.Is(err, broker.ErrAlreadyMinted):
		writeError(w, http.StatusConflict, "credential already minted (single-use)")
	default:
		writeError(w, http.StatusInternalServerError, "mint: "+err.Error())
	}
}

// rfc3339 is the timestamp format used in mint responses.
const rfc3339 = "2006-01-02T15:04:05Z07:00"

// decisionOutcome maps an egress decision to an audit outcome.
func decisionOutcome(d egress.Decision) string {
	switch d {
	case egress.Allow:
		return "success"
	case egress.Deny:
		return "denied"
	default: // pending
		return "success"
	}
}
