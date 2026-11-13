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
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A confined replay's clean verdict is read from its run's audit trail, and the trail can still grow after
// the capture read it: an egress decision whose insert was still in flight, or one waiting in the audit
// spool of a replica other than the one that took the capture, lands later. And the trail can be missing an
// event for good, when a spool set it aside because the store refused it. So the capture records the audit
// position it read through, a later workspace read takes a clean verdict again when an egress decision has
// landed past that position, and a spool that sets a run's event aside notes it on that run's trail.

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
// entry's EvidenceSeq, an egress decision or a note that an event was set aside. The entry is captured again
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
// allow, deny and hold can change the verdict) or a note that an event was set aside. A failed probe
// reports nothing, and the next read probes again.
func cleanVerdictMoved(ctx context.Context, ev store.RecordEvidence, res RecordTaskResult) bool {
	for _, f := range []store.AuditFilter{{ActionPrefix: "egress."}, {Action: auditSpoolQuarantineAction}} {
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
	for _, ev := range events {
		var d struct {
			RuleSource string `json:"rule_source"`
		}
		if ev.Action == "egress.deny" && json.Unmarshal(ev.Data, &d) == nil && strings.HasPrefix(d.RuleSource, ruleSourceDroppedDecisions) {
			return true
		}
	}
	return false
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
