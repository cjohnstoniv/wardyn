// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The decision door also takes a batch of records (#4063): a post of proxy.DecisionBatchContentType carries
// {"records": [...]}, at most proxy.MaxDecisionBatch of them, and the door appends the batch's rows under one
// take of the audit chain lock. Every answer of the door, once internalAuth has read the run token, says so
// (advertiseDecisionBatch): a proxy posts a batch only after reading that, never from a status.
const (
	// maxDecisionBatchBody caps a batch post: proxy.MaxDecisionBatch records, each far under maxJSONBody.
	maxDecisionBatchBody = 4 << 20
	// A batch row's time is the proxy's queue time when it is at most decisionBatchPast before the post's
	// arrival and at most decisionBatchSkew after it, else the arrival time (that row then does not dedupe on
	// a retry).
	decisionBatchPast = 10 * time.Minute
	decisionBatchSkew = 5 * time.Minute
)

// advertiseDecisionBatch says on an answer of the decision door that it takes batches, and how many records.
func advertiseDecisionBatch(w http.ResponseWriter) {
	w.Header().Set(proxy.DecisionBatchHeader, strconv.Itoa(proxy.MaxDecisionBatch))
}

// decisionBatchPost reports whether r is a batch post: its media type is proxy.DecisionBatchContentType.
func decisionBatchPost(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == proxy.DecisionBatchContentType
}

// decisionPostRetried reports whether the proxy will send r again: a batch post without the final mark. Its
// retryable 503 is not a lost report; the attempt that carries the mark is.
func decisionPostRetried(r *http.Request) bool {
	return decisionBatchPost(r) && r.Header.Get(proxy.DecisionFinalHeader) != "1"
}

// decisionRequest reads, once per request, whether the post's decisions are relayed: a via_org engine's post
// marks a relayed flow with DecisionRelayedHeader. Only the engine's in-process call for a VM run is trusted
// with it; anyone else's copy is dropped. Every record of a relayed batch keeps the mark.
func (s *Server) decisionRequest(r *http.Request) (relayed bool) {
	mark := r.Header.Get(proxy.DecisionRelayedHeader) == "1"
	r.Header.Del(proxy.DecisionRelayedHeader)
	if !mark || !viaOrgCall(r.Context()) {
		return false
	}
	run, ok := liveRunFromContext(r.Context())
	return ok && run.MachineKind.IsVM()
}

// decisionRecord normalises one decision log and sets the request's relayed mark (Relayed is not serialised).
func decisionRecord(dl *egress.DecisionLog, relayed bool) {
	normalizeN1Decision(dl)
	if relayed {
		dl.Request.Relayed = true
	}
}

// decisionDurable reports whether dl's row is the post's durable egress row, answered 503 when it was not
// kept. A DNS query's row and a scan bypass's row are best-effort.
func decisionDurable(dl egress.DecisionLog) bool {
	return dl.DNS == nil && (dl.Scan == nil || dl.Scan.Action != "bypass")
}

// decisionTouch moves the run's idle clock and the pause's presence clock for one decision. Not called for a
// DNS query: a run that only asks names idles out.
//
// An egress decision means the agent actually did something, so it resets the
// idle clock the reaper reads (agent_runs.updated_at) — the seam
// internal/lifecycle documents. Best-effort like the attach keepalive; a nil
// Store (test harness) and an unknown run id are both non-events.
// ponytail: only runs that make egress calls stay alive — a pure-local-compute
// run is still wall-clocked. Runner-reported liveness is the upgrade path.
// Debounced: a chatty agent can emit many decisions a second, and each touch
// is an UPDATE on the same agent_runs row; the reaper thresholds are minutes,
// so one touch per touchDebounce per run loses nothing.
// An allowed request is activity, a refused one is not (egress.CountsAsActivity): a run that is only
// being refused idles out. The run's kind comes from the liveness gate's read, for the macOS boot hosts.
// A run that is over reaches this door only inside its tail grace (terminalGraceRoutes): its last
// decisions are kept on its trail and, as on the runner spool, are not activity.
func (s *Server) decisionTouch(ctx context.Context, runID uuid.UUID, kind types.MachineKind, isOver func() bool, dl egress.DecisionLog) {
	if s.cfg.Store != nil && s.shouldTouch(runID, kind, dl) && !isOver() {
		_ = s.cfg.Store.TouchRun(ctx, runID)
	}
	// The same decision moves the pause's presence clock (run_pause.go).
	if decisionIsActivity(kind, dl) && s.pause.stampDue(false, runID, s.cfg.Now()) && !isOver() {
		s.noteAgentActive(ctx, runID)
	}
}

// decisionEvents builds, without recording them, the rows one decision writes: the DNS query's row, the scan
// bypass's row, or the egress.<decision> row and, when the decision carries a scan summary, its llm.scan.*
// row in extra.
func (s *Server) decisionEvents(ctx context.Context, claims *identity.Claims, sourceIP string, dl egress.DecisionLog) (primary *types.AuditEvent, extra []types.AuditEvent) {
	runID := claims.RunID
	// A DNS query the run's sandbox asked (internal/egress/proxy/dns_audit.go) is its own row, not an
	// egress decision.
	if dl.DNS != nil {
		fields := map[string]any{}
		dl.DNS.AuditFields(fields)
		data, _ := json.Marshal(fields)
		ev := s.auditEvent(&runID, types.ActorAgent, claims.SPIFFEID, egress.DNSQueryAction, dl.DNS.Name, dl.DNS.AuditOutcome(), data)
		ev.SourceIP = sourceIP
		return &ev, nil
	}
	// A synthetic "bypass" decision is PURELY an LLM-inspection coverage signal
	// (an opaque CONNECT to a model host that could not be inspected). Emit only
	// the llm.scan.bypass degradation event — not a duplicate egress.allow for
	// the tunnel, which the real CONNECT decision already recorded.
	if dl.Scan != nil && dl.Scan.Action == "bypass" {
		ev := s.llmScanAuditEvent(runID, claims.SPIFFEID, sourceIP, dl.Scan, dl.Request.Host)
		return &ev, nil
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
	// The allow of an opaque CONNECT to a model host; counted by RunOpaqueTunnels.
	if dl.ModelTunnel {
		fields["model_tunnel"] = true
	}
	// A desktop VM's relay flow id, and the port rule's dry-run mark.
	if dl.Flow != "" {
		fields["flow"] = dl.Flow
	}
	if dl.PortRule != "" {
		fields["port_rule"] = dl.PortRule
	}
	// The id the gateway's access log shares with this row; absent on a run with no gateway route.
	if dl.GatewayCallID != "" {
		fields["gateway_call_id"] = dl.GatewayCallID
	}
	// The id the gateway or its provider assigned to the attempt; absent where none was read.
	if dl.GatewayRequestID != "" {
		fields["gateway_request_id"] = dl.GatewayRequestID
	}
	// Whose refusal a deny was ("refusal", egress.Refusal); absent on an unclassified row.
	dl.Refusal.AuditFields(fields)
	if dl.MatchedRule != "" {
		fields["matched_rule"] = dl.MatchedRule // the tool_rules key behind a policy:tool-* row
	}
	// A Bedrock data-plane refusal the proxy relayed (bedrock_dataplane_fault.go). A run that is over takes
	// its hint on a write that leaves updated_at, which the terminal tail grace is counted from, so a post
	// kept inside the grace cannot open it again.
	if dl.UpstreamFault != "" {
		fields["upstream_fault"] = dl.UpstreamFault
		s.noteBedrockDataPlaneFault(ctx, runID, dl.UpstreamFault)
	}
	data, _ := json.Marshal(fields)
	ev := s.auditEvent(&runID, types.ActorAgent, claims.SPIFFEID,
		"egress."+string(dl.Decision), dl.Request.Host, decisionOutcome(dl.Decision), data)
	ev.SourceIP = sourceIP
	// Optional outbound content-inspection summary rides the same decision. When
	// present it becomes a SEPARATE, content-free llm.scan.* audit event so the
	// model-channel inspection is independently visible/queryable in the log.
	if dl.Scan != nil {
		extra = append(extra, s.llmScanAuditEvent(runID, claims.SPIFFEID, sourceIP, dl.Scan, dl.Request.Host))
	}
	return &ev, extra
}

// llmScanAuditEvent is the llm.scan.* row of a scan summary: the content-free payload and finding cap runner
// replay shares; no matched bytes or reversible hashes enter either path.
func (s *Server) llmScanAuditEvent(runID uuid.UUID, actor, sourceIP string, sc *egress.ScanSummary, host string) types.AuditEvent {
	action, outcome, data := egress.ScanAudit(sc, host)
	ev := s.auditEvent(&runID, types.ActorAgent, actor, action, host, outcome, data)
	ev.SourceIP = sourceIP
	return ev
}

// decisionAfter counts one recorded egress decision.
//
// wardyn_egress_denies_total is exposed as "denied by policy", and it
// is the only egress counter Wardyn has. Some rows arrive here as
// egress.Deny without being a denial by policy; isPolicyDeny (metrics.go)
// lists every one and says why. Counting them would page operators for
// policy denials that never happened and make the true deny rate unreadable
// off the series. Each still records its egress.deny AUDIT row unchanged —
// only the counter is scoped. llm_routes.go's gatewayTarget GUARD refusal
// has its own rule_source, so it counts here like any other guard denial.
func (s *Server) decisionAfter(ctx context.Context, dl egress.DecisionLog) {
	if dl.Decision == egress.Deny && isPolicyDeny(dl.RuleSource) {
		s.metrics.egressDenied()
	}
	s.countReauthTimeout(ctx, dl)
}

// validDecisionBatch refuses a batch the proxy never sends: no records, a record without an id or with one
// another record has, a usage record (posted alone), or a contact label other than "lost".
func validDecisionBatch(b proxy.DecisionBatch) bool {
	if len(b.Records) == 0 {
		return false
	}
	seen := make(map[uuid.UUID]bool, len(b.Records))
	for _, rec := range b.Records {
		if rec.ID == uuid.Nil || seen[rec.ID] || rec.Decision.Usage != nil || (rec.CPContact != "" && rec.CPContact != "lost") {
			return false
		}
		seen[rec.ID] = true
	}
	return true
}

// handlePostDecisionBatch records a batch of decisions as handlePostDecision records each, and appends the
// rows in one call through the audit chain (audit.RecordBatch), under one take of the chain lock.
//
// Each record's rows take the record's id (its llm.scan.* row an id derived from it) and, inside the window,
// its queue time, and are recorded as replays: a retried batch adds no row twice, and a row an earlier
// attempt stored is held, not counted again. The answer is a 202 once every durable egress row was appended,
// held or spooled, else the retryable 503. A decision is counted on the attempt that appended its row, or, for
// a spooled row, on the attempt that answers 202. Sink emits of spooled rows wait for the attempt that answers
// 202 or carries the final mark, so a row spooled on several attempts reaches the sinks once.
func (s *Server) handlePostDecisionBatch(w http.ResponseWriter, r *http.Request, claims *identity.Claims) {
	var b proxy.DecisionBatch
	if err := types.DecodeOneJSONRows(http.MaxBytesReader(w, r.Body, maxDecisionBatchBody), &b, false, "records", proxy.MaxDecisionBatch); err != nil || !validDecisionBatch(b) {
		s.refuseDecisionLog(w, r, claims.RunID)
		return
	}
	ctx := r.Context()
	runID := claims.RunID // every record's: the body's run id is advisory, as on a single post
	relayed := s.decisionRequest(r)
	kind, isOver := s.decisionRunOver(ctx)
	now := s.cfg.Now().UTC()
	type durableRecord struct {
		id uuid.UUID
		dl egress.DecisionLog
	}
	var durable []durableRecord
	evs := make([]types.AuditEvent, 0, len(b.Records)+1)
	for i := range b.Records {
		rec := &b.Records[i]
		decisionRecord(&rec.Decision, relayed)
		dl := rec.Decision
		if dl.DNS == nil {
			s.decisionTouch(ctx, runID, kind, isOver, dl)
		}
		primary, extra := s.decisionEvents(ctx, claims, r.RemoteAddr, dl)
		at := now
		if !rec.Time.Before(now.Add(-decisionBatchPast)) && !rec.Time.After(now.Add(decisionBatchSkew)) {
			at = rec.Time.UTC()
		}
		primary.ID, primary.Time = rec.ID, at
		if rec.CPContact == "lost" {
			primary.Data = withContactLost(primary.Data, rec.ContactAgeS)
		}
		evs = append(evs, s.stampAuditEvent(ctx, *primary))
		for _, ev := range extra {
			ev.ID, ev.Time = uuid.NewSHA1(rec.ID, []byte("llm.scan")), at
			evs = append(evs, s.stampAuditEvent(ctx, ev))
		}
		if decisionDurable(dl) {
			durable = append(durable, durableRecord{rec.ID, dl})
		}
	}

	rctx, out := audit.WithBatchOutcome(store.WithAuditReplay(ctx))
	if s.cfg.Audit == nil {
		for _, ev := range evs {
			audit.NoteBatch(rctx, ev.ID, audit.BatchAppended)
		}
	} else if err := audit.RecordBatch(rctx, s.cfg.Audit, evs); err != nil {
		slog.WarnContext(ctx, "api: a decision batch was not wholly stored", slog.Int("records", len(b.Records)), slog.Any("err", err))
	}
	kept := true
	for _, d := range durable {
		switch out.Status(d.id) {
		case audit.BatchAppended, audit.BatchHeld, audit.BatchSpooled:
		default:
			kept = false
		}
	}
	for _, d := range durable {
		if st := out.Status(d.id); st == audit.BatchAppended || (st == audit.BatchSpooled && kept) {
			s.decisionAfter(ctx, d.dl)
		}
	}
	answerDecisionPost(w, kept)
	out.Release(rctx, kept || r.Header.Get(proxy.DecisionFinalHeader) == "1")
}

// withContactLost adds the proxy's statement that it decided while it could not reach the control plane, and
// for how many seconds it had not.
func withContactLost(data json.RawMessage, ageS int64) json.RawMessage {
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &m); err != nil {
		return data
	}
	m["cp_contact"] = json.RawMessage(`"lost"`)
	m["contact_age_s"] = json.RawMessage(strconv.FormatInt(max(0, ageS), 10))
	// A map of RawMessage cannot fail to marshal.
	out, _ := json.Marshal(m)
	return out
}
