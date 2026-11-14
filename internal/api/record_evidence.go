// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A confined replay's clean verdict is read from its run's audit trail, and the trail can still grow after
// the capture read it: an egress decision whose insert was still in flight, or one waiting in the audit
// spool of a replica other than the one that took the capture, lands later. And the trail can be missing an
// event for good, when a spool set it aside because the store refused it, or when the decision door refused
// the egress proxy's post. So the capture records the audit position it read through, a later workspace
// read takes a clean verdict again when an egress decision has landed past that position, a spool that sets
// a run's event aside notes it on that run's trail, and the door keeps a run's last decisions inside its
// tail grace (terminalGraceRoutes) and writes a row on the run's trail when it refuses a post past it or
// cannot take one (once per run: claimDecisionRefusal).

// auditSpoolQuarantineAction is the row an audit spool writes on a run's trail when it sets one of that
// run's events aside (AuditSpool.noteRunEventSetAside).
const auditSpoolQuarantineAction = "audit.spool.quarantine"

// recordEvidenceSetAsideHint is the failure hint of a confined replay whose run's trail is missing an event.
const recordEvidenceSetAsideHint = "One of this replay's audit events could not be stored and was set aside in the " +
	"audit spool's quarantine file, so the replay cannot be judged clean. An administrator can re-feed it from there " +
	"(docs/OPERATIONS.md, \"The audit log can't quietly rot\"); replay again afterwards."

// recordDecisionsDroppedHint is the failure hint of a confined replay whose egress proxy dropped decisions.
const recordDecisionsDroppedHint = "The egress proxy could not deliver every decision of this replay to the server " +
	"and reported how many it dropped, so the replay cannot be judged clean. Replay again."

// recordDecisionsLostHint is the failure hint of a confined replay whose runner reported egress decisions its
// spool lost (ruleSourceSpoolLost).
const recordDecisionsLostHint = "The runner lost decisions of this replay before they reached the server and " +
	"reported how many, so the replay cannot be judged clean. Replay again."

// ruleSourceSpoolLost is the rule source of the row a runner's spool writes for decisions of sent requests that
// it lost (runnerspool's EmitLoss).
const ruleSourceSpoolLost = "builtin:spool-lost"

// recordSpoolFullHint is the failure hint of a confined replay whose runner's spool was full (ruleSourceSpoolFull).
const recordSpoolFullHint = "The runner's egress audit was full during this replay: some requests were refused " +
	"before the policy judged them, or their decisions were not recorded, so the replay cannot be judged clean. " +
	"Replay again."

// ruleSourceSpoolFull is the rule source of the row a runner's spool writes for the records it refused for room
// (runnerspool's Overflow): requests refused at admission, and decisions of requests that were not sent.
const ruleSourceSpoolFull = "builtin:spool-full"

// recordDecisionRefusedHint is the failure hint of a confined replay whose egress proxy had a post refused, or
// one the decision door could not take (run.report.drop).
const recordDecisionRefusedHint = "The server did not keep a report the egress proxy sent for this replay, which " +
	"could have been one of its decisions, so the replay cannot be judged clean. Replay again."

// decisionsDoor is the path the egress proxy posts its reports to, and so the target of the authz.denied row
// internalAuth writes when it refuses one (auditInternalDenied).
const decisionsDoor = InternalPathPrefix + "decisions"

// recordEvidenceSeq is the audit position a confined capture reads its run's trail through, taken before the
// read: every row of the run at or below it has committed and is in the read, and every row that commits
// later is above it (store.RecordEvidence). An open recording has no verdict to take again and needs none.
// ok is false when there is no position to be had, and the entry then stays recording: a clean verdict
// that could not be checked again later is not stamped.
func (s *Server) recordEvidenceSeq(ctx context.Context, runID uuid.UUID, confined bool) (int64, bool) {
	if !confined {
		return 0, true
	}
	ev, ok := s.cfg.Store.(store.RecordEvidence)
	if !ok {
		return 0, false
	}
	seq, err := ev.RunAuditSeq(ctx, runID)
	return seq, err == nil
}

// recheckCleanReplays takes each clean confined verdict in ws again when its run's trail has, past the
// entry's EvidenceSeq, an egress decision, a note that an event was set aside, or a refused or lost post to
// the decision door (cleanVerdictMoved). The entry is captured again
// from the run's whole trail and swapped in only while the stored entry is still the one read, so a
// promotion, a re-record or another replica's recheck that wrote first is kept. A store error leaves the
// entry as it is, for the next read. Reports whether ws is stale: an entry was swapped, or a swap found
// the entry already replaced, and the caller answers with the row as it now is.
func (s *Server) recheckCleanReplays(ctx context.Context, ws types.Workspace) bool {
	ev, ok := s.cfg.Store.(store.RecordEvidence)
	var entries map[string]json.RawMessage
	if !ok || len(ws.RecordResults) == 0 || json.Unmarshal(ws.RecordResults, &entries) != nil {
		return false
	}
	stale := false
	for key, old := range entries {
		var res RecordTaskResult
		if json.Unmarshal(old, &res) != nil || !res.Confined || res.Status != recordStatusRecorded ||
			res.Clean == nil || !*res.Clean || !cleanVerdictMoved(ctx, ev, res) {
			continue
		}
		run, err := s.cfg.Store.GetRun(ctx, res.RunID)
		if err != nil {
			continue
		}
		seq, err := ev.RunAuditSeq(ctx, res.RunID)
		if err != nil {
			continue
		}
		events, err := s.cfg.Store.QueryAuditEvents(ctx, res.RunID, maxCaptureAuditEvents)
		if err != nil {
			continue
		}
		res = s.takeRecordCapture(ctx, run, res, events, seq)
		if _, err := ev.SwapWorkspaceRecordResult(ctx, ws.ID, key, old, mustJSON(res)); err == nil {
			stale = true
		}
	}
	return stale
}

// cleanVerdictMoved reports whether res's run has, past res.EvidenceSeq, an egress decision (each of
// allow, deny and hold can change the verdict), a note that an event was set aside, or a refused or lost
// post to the decision door. A failed probe reports nothing, and the next read probes again.
func cleanVerdictMoved(ctx context.Context, ev store.RecordEvidence, res RecordTaskResult) bool {
	for _, f := range []store.AuditFilter{{ActionPrefix: "egress."}, {Action: auditSpoolQuarantineAction},
		{Action: authz.AuditAction, Target: decisionsDoor}, {Action: runReportLostAction}} {
		if found, err := ev.HasRunAuditEventAfter(ctx, res.RunID, res.EvidenceSeq, f); err == nil && found {
			return true
		}
	}
	return false
}

// runEventSetAside reports whether events hold the note that an audit spool set one of the run's events
// aside.
func runEventSetAside(events []types.AuditEvent) bool {
	for _, ev := range events {
		if ev.Action == auditSpoolQuarantineAction {
			return true
		}
	}
	return false
}

// runDecisionsDropped reports whether events hold the egress proxy's summary of decisions it dropped
// (ruleSourceDroppedDecisions). The summary is a deny with no host, which the capture leaves out, and it
// stands for decisions the trail never got: any of them could have been a deny.
func runDecisionsDropped(events []types.AuditEvent) bool {
	return runHasDeny(events, func(rule string) bool { return strings.HasPrefix(rule, ruleSourceDroppedDecisions) })
}

// runDecisionsLost reports whether events hold a runner spool's report of decisions it lost
// (ruleSourceSpoolLost). Like the proxy's summary it is a deny with no host, which the capture leaves out, and
// the decisions it stands for are of requests that were sent: any of them could have been a deny.
func runDecisionsLost(events []types.AuditEvent) bool {
	return runHasDeny(events, func(rule string) bool { return rule == ruleSourceSpoolLost })
}

// runSpoolWasFull reports whether events hold a runner spool's count of the records it refused for room
// (ruleSourceSpoolFull). It is a deny with no host, which the capture leaves out. A request refused at
// admission was never judged by the policy, and a refused decision is one the trail never got: either could
// have been a deny.
func runSpoolWasFull(events []types.AuditEvent) bool {
	return runHasDeny(events, func(rule string) bool { return rule == ruleSourceSpoolFull })
}

// runHasDeny reports whether events hold an egress deny whose rule source matches.
func runHasDeny(events []types.AuditEvent, match func(rule string) bool) bool {
	for _, ev := range events {
		var d struct {
			RuleSource string `json:"rule_source"`
		}
		if ev.Action == "egress.deny" && json.Unmarshal(ev.Data, &d) == nil && match(d.RuleSource) {
			return true
		}
	}
	return false
}

// runDecisionPostRefused reports whether events hold internalAuth's refusal of a post to the decision door,
// such as one the egress proxy sent after its run's tail grace (terminalGraceRoutes), or a post the door
// could not take (runReportLostAction). What it posted is not on the trail, and it could have been a deny.
func runDecisionPostRefused(events []types.AuditEvent) bool {
	for _, ev := range events {
		if (ev.Action == authz.AuditAction || ev.Action == runReportLostAction) && ev.Target == decisionsDoor {
			return true
		}
	}
	return false
}

// rejudgedForList is ws as a list read hands it out: a clean confined verdict whose run's trail has moved
// is taken again first (recheckCleanReplays), as a read of the one workspace does, and the row is read
// again when an entry was swapped. A row that cannot be read again is listed as it was.
func (s *Server) rejudgedForList(ctx context.Context, ws types.Workspace) types.Workspace {
	if !s.recheckCleanReplays(ctx, ws) {
		return ws
	}
	if fresh, err := s.cfg.Store.GetWorkspace(ctx, ws.ID); err == nil {
		return fresh
	}
	return ws
}

// noteRunEventSetAside writes on ev's run's own trail that this spool is setting ev aside, so a confined
// replay of that run is not judged clean on a trail that is missing it, whichever replica takes the verdict.
// Drain calls it before quarantineLine moves the line, once the store has refused ev alone, and keeps the
// line in the spool when the note cannot be written, as it does through an outage. The row names ev by
// its id and action and carries nothing of its payload, which is what the store refused. The action is the
// one thing of ev's the note holds, so a store that answers and refuses the note is asked once more without
// it: a note is never refused for the reason its event was, and a line it is kept for is one the store
// refused a row that holds nothing of the event. An event of no run needs no note, and one already noted
// by this process needs no second. The caller holds a.mu.
func (a *AuditSpool) noteRunEventSetAside(ctx context.Context, rec audit.Recorder, ev types.AuditEvent) error {
	if ev.RunID == nil || (ev.ID != uuid.Nil && ev.ID == a.noted) {
		return nil
	}
	note := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), RunID: ev.RunID, ActorType: types.ActorSystem, Actor: "wardynd",
		Action: auditSpoolQuarantineAction, Target: ev.ID.String(), Outcome: "success",
		Data: mustJSON(map[string]string{"action": ev.Action}),
	}
	err := rec.Record(ctx, note)
	if err != nil && ctx.Err() == nil && !auditWriteTimedOut(err) {
		note.Data = nil
		err = rec.Record(ctx, note)
	}
	if err != nil {
		return err
	}
	a.noted = ev.ID
	return nil
}
