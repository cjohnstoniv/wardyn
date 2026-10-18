// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A Kubernetes runner that deleted a run's pods but cannot prove the processes stopped reports the run
// as containment pending. The organisation keeps that report until the runner
// reports the proxy stopped, holds the run for leases and refuses its token renewal meanwhile, and shows
// its egress bound on run detail. A security operator's node record ("node powered off" or "no prior
// holder") is the release when no evidence will come: made by one or two people as the rules
// below say, sent to the runner on a stop_proxy, and accepted or refused there against the runner's own
// acceptance list.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	govKindRunNodeRecord = types.GovernanceTargetRunNodeRecord

	// auditRunNodeRecord is the node record's own row; auditRunContainmentPending a state page's
	// containment-pending transition.
	auditRunNodeRecord         = "run.node.record"
	auditRunContainmentPending = "run.containment.hold"

	maxNodeRecordSourceID = 256

	// nodeRecordReasonWithheld is what a reader below the security tier reads for a refused record.
	nodeRecordReasonWithheld = "The runner refused this record. A security operator can see why."
)

// The sentences the reviewer of a held node record reads beside the diff.
var nodeRecordReviewNotes = []string{
	"A deleted Node object is not confirmation that a machine is off.",
	"A machine cut off from the network may still be running the run's pods.",
	"\"Node powered off\" covers every machine that held this node name from the binding until this record.",
}

// runContainmentStore is the PG capability behind these routes and the renewal check.
type runContainmentStore interface {
	GetRunContainmentPending(ctx context.Context, runID uuid.UUID) (store.RunContainmentPendingRow, error)
	ListRunNodeRecords(ctx context.Context, runID uuid.UUID) ([]types.RunNodeRecord, error)
	QueueRunNodeRecord(ctx context.Context, rec store.RunNodeRecordWrite) (types.RunnerPendingAction, error)
}

var _ runContainmentStore = store.PG{}

// runContainmentPending reports whether the run's runner holds it as containment pending. A store that
// keeps no such report answers false; a read that fails is an error.
func (s *Server) runContainmentPending(ctx context.Context, run types.AgentRun) (bool, error) {
	st, ok := s.cfg.Store.(runContainmentStore)
	if !ok || run.Placement != types.PlacementLocal || run.RunnerID == nil {
		return false, nil
	}
	_, err := st.GetRunContainmentPending(ctx, run.ID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// nodeRecordNeedsTwo is whether a node record for held needs two people: always after a recorded name
// reuse and on the no-association path, and otherwise when four-eyes is on.
func nodeRecordNeedsTwo(held store.RunContainmentPendingRow) bool {
	return GovernanceSecondHumanEnabled() || held.NameReused || held.NoCausalAssociation
}

func (s *Server) handleGetRunContainment(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrSponsorOrAdmin)
	if !ok {
		return
	}
	out := types.RunContainment{Records: []types.RunNodeRecord{}}
	st, ok := s.cfg.Store.(runContainmentStore)
	if !ok || run.Placement != types.PlacementLocal {
		writeJSON(w, http.StatusOK, out)
		return
	}
	held, err := st.GetRunContainmentPending(r.Context(), id)
	switch {
	case err == nil:
		bound := held.TokenRenewedAt.Add(identity.RunTokenTTL).UTC()
		out.Pending = &types.RunContainmentPending{Kind: held.Kind, Since: held.Since, BoundUntil: bound, BoundPassed: !s.cfg.Now().Before(bound),
			NameReused: held.NameReused, NoCausalAssociation: held.NoCausalAssociation, TwoPeople: nodeRecordNeedsTwo(held)}
		// The pod and node identities are for the people who can make a record.
		if s.isSecurityOperator(r.Context()) {
			out.Pending.Tuple = held.Tuple
		}
	case !errors.Is(err, store.ErrNotFound):
		writeServerError(w, r, "read run containment", err)
		return
	}
	if out.Records, err = st.ListRunNodeRecords(r.Context(), id); err != nil {
		writeServerError(w, r, "read node records", err)
		return
	}
	// A record's source id and the runner's refusal text name infrastructure, as the tuple does:
	// security operators only. Others read a fixed sentence for the reason.
	if !s.isSecurityOperator(r.Context()) {
		for i := range out.Records {
			out.Records[i].SourceID = ""
			if out.Records[i].Reason != "" {
				out.Records[i].Reason = nodeRecordReasonWithheld
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// runNodeRecordChange is the validated record a held change replays. NameReused, NoCausalAssociation
// and Tuple are what the proposer was shown; the record is refused if the runner's record moved since.
type runNodeRecordChange struct {
	RunID               uuid.UUID       `json:"run_id"`
	Attestation         string          `json:"attestation"`
	Source              string          `json:"source"`
	SourceID            string          `json:"source_id"`
	Tuple               json.RawMessage `json:"tuple"`
	NameReused          bool            `json:"name_reused"`
	NoCausalAssociation bool            `json:"no_causal_association"`
	FourEyes            bool            `json:"four_eyes"`
}

// runNodeRecordState is the held run a proposal and its approval hash.
func runNodeRecordState(held store.RunContainmentPendingRow) any {
	return map[string]any{"name_reused": held.NameReused, "no_causal_association": held.NoCausalAssociation, "tuple": held.Tuple}
}

// nodeRecordRequestRefusal says why req cannot be a record, or "".
func nodeRecordRequestRefusal(req types.RunNodeRecordRequest) string {
	switch {
	case req.Attestation != types.NodeRecordPoweredOff && req.Attestation != types.NodeRecordNoPriorHolder:
		return "attestation must be node_powered_off or no_prior_holder"
	case req.Source != types.NodeRecordSourceProviderConsole && req.Source != types.NodeRecordSourceManagementController && req.Source != types.NodeRecordSourceOther:
		return "source must be provider_console, management_controller or other"
	case strings.TrimSpace(req.SourceID) == "" || utf8.RuneCountInString(req.SourceID) > maxNodeRecordSourceID || !controlCharFree(req.SourceID):
		return "source_id is required: the identifier where you confirmed it, at most 256 characters, without control characters"
	case len(req.Tuple) == 0 || string(req.Tuple) == "null":
		return "tuple is required: send the tuple the run's containment shows"
	}
	return ""
}

// nodeRecordAttestationRefusal applies the attestation rules the organisation can check itself: after a
// recorded name reuse only "node powered off" is accepted, and "no prior holder" is for the no-association
// path only. Preconditions (i) and (ii) need the runner's acceptance list, so
// the runner decides them.
func nodeRecordAttestationRefusal(attestation string, held store.RunContainmentPendingRow) string {
	if attestation != types.NodeRecordNoPriorHolder {
		return ""
	}
	if held.NameReused {
		return `This run's node name was reused, so only "node powered off" can release it.`
	}
	if !held.NoCausalAssociation {
		return `"No prior holder" is only for a run with no causal association. Use "node powered off".`
	}
	return ""
}

func (s *Server) handlePostRunNodeRecord(w http.ResponseWriter, r *http.Request) {
	st, ok := s.cfg.Store.(runContainmentStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonNodeRecordRefused, "node records require the Postgres store backend")
		return
	}
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	if isAdminTokenCaller(r) {
		writeErrorReason(w, http.StatusForbidden, reasonNodeRecordHumanOnly, "A node record is made by people. The admin token cannot make one.")
		return
	}
	var req types.RunNodeRecordRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	req.SourceID = strings.TrimSpace(req.SourceID)
	if msg := nodeRecordRequestRefusal(req); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonNodeRecordInvalid, msg)
		return
	}
	run, err := s.cfg.Store.GetRun(r.Context(), id)
	if notFoundIf(w, err, "run", reasonRunNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "read run", err)
		return
	}
	held, err := st.GetRunContainmentPending(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && run.Placement != types.PlacementLocal) {
		writeErrorReason(w, http.StatusConflict, reasonNodeRecordRefused, "This run is not waiting for containment evidence, so there is nothing to release.")
		return
	}
	if err != nil {
		writeServerError(w, r, "read run containment", err)
		return
	}
	if len(held.Tuple) == 0 || !store.SameJSON(held.Tuple, req.Tuple) {
		writeErrorReason(w, http.StatusConflict, reasonNodeRecordRefused, "The runner's record of this run changed since you loaded it. Load the run again and check the machines it names.")
		return
	}
	if msg := nodeRecordAttestationRefusal(req.Attestation, held); msg != "" {
		writeErrorReason(w, http.StatusConflict, reasonNodeRecordRefused, msg)
		return
	}
	two := nodeRecordNeedsTwo(held)
	if two && s.cfg.LocalMode {
		writeErrorReason(w, http.StatusForbidden, reasonNodeRecordHumanOnly, "This record needs two people, and local mode cannot show who they are. Sign in with SSO to make it.")
		return
	}
	change := runNodeRecordChange{RunID: id, Attestation: req.Attestation, Source: req.Source, SourceID: req.SourceID, Tuple: held.Tuple,
		NameReused: held.NameReused, NoCausalAssociation: held.NoCausalAssociation, FourEyes: GovernanceSecondHumanEnabled()}
	if two {
		after := map[string]any{"run_id": id, "attestation": req.Attestation, "source": req.Source, "source_id": req.SourceID,
			"name_reused": held.NameReused, "no_causal_association": held.NoCausalAssociation, "tuple": held.Tuple}
		s.proposeGovernanceChange(w, r, govProposal{kind: govKindRunNodeRecord, op: "create", key: id.String(), payload: change,
			after: after, changed: changedPaths(nil, after), notes: nodeRecordReviewNotes, baseState: runNodeRecordState(held)})
		return
	}
	approver, err := s.nodeRecordApprover(principalFromRequest(r), oidcEmailFromContext(r.Context()))
	if err != nil {
		writeServerError(w, r, "node record approver", err)
		return
	}
	rec, err := change.record(approver)
	if err != nil {
		writeServerError(w, r, "encode node record", err)
		return
	}
	a, err := st.QueueRunNodeRecord(r.Context(), store.RunNodeRecordWrite{RunID: id, Record: rec, NameReused: held.NameReused, NoCausalAssociation: held.NoCausalAssociation})
	if msg := nodeRecordQueueRefusal(err); msg != "" {
		writeErrorReason(w, http.StatusConflict, reasonNodeRecordRefused, msg)
		return
	}
	if err != nil {
		writeServerError(w, r, "queue node record", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(&id, actorTypeFromRequest(r), principalFromRequest(r), auditRunNodeRecord, id.String(), "success",
		mustJSON(nodeRecordAuditData(change, rec, a))))
	s.deliverNodeRecord(r.Context(), a.RunnerID)
	writeJSON(w, http.StatusCreated, types.RunNodeRecord{ID: rec.ID, Attestation: rec.Attestation, Source: rec.Source, SourceID: rec.SourceID,
		State: types.NodeRecordSent, CreatedAt: a.QueuedAt})
}

// nodeRecordQueueRefusal is the sentence for a queue the store refused, or "" for any other result.
func nodeRecordQueueRefusal(err error) string {
	switch {
	case errors.Is(err, store.ErrRunNodeRecordStale), errors.Is(err, store.ErrRunnerActionRefused), errors.Is(err, store.ErrNotFound):
		return "The runner's record of this run changed since this record was made, so its approvals lapse. Load the run again and make a new record."
	case errors.Is(err, store.ErrConflict):
		return "A stop for this run is already waiting for its runner. Make the record once the runner has answered it."
	}
	return ""
}

// record is the runner.NodeRecord for this change, made by approvers.
func (c runNodeRecordChange) record(approvers ...runner.NodeRecordApprover) (runner.NodeRecord, error) {
	var tuple runner.NodeTuple
	if err := json.Unmarshal(c.Tuple, &tuple); err != nil {
		return runner.NodeRecord{}, err
	}
	return runner.NodeRecord{ID: uuid.New(), Attestation: c.Attestation, Tuple: tuple, Approvers: approvers, FourEyes: c.FourEyes,
		Source: c.Source, SourceID: c.SourceID}, nil
}

// nodeRecordApprover is one person as the runner receives them: keyed pseudonyms of the case-folded
// principal and email, HMAC-SHA256 under a key derived from the server's run config key. The runner only
// tests them for equality, so it can still tell people apart (one principal, or one email, is one person).
// The action row holds the pseudonyms, which tie back to a person only with the server's key; who approved
// is on the audit rows. With no key the record is refused rather than written with a guessable value.
func (s *Server) nodeRecordApprover(principal, email string) (runner.NodeRecordApprover, error) {
	if len(s.cfg.RunConfigKey) != kek.DEKSize {
		return runner.NodeRecordApprover{}, errors.New("no server key for node-record pseudonyms")
	}
	derive := hmac.New(sha256.New, s.cfg.RunConfigKey)
	derive.Write([]byte("wardyn node-record approver"))
	key := derive.Sum(nil)
	pseudonym := func(v string) string {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			return ""
		}
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(v))
		return "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil))
	}
	return runner.NodeRecordApprover{Principal: pseudonym(principal), Email: pseudonym(email), Human: true}, nil
}

func nodeRecordAuditData(c runNodeRecordChange, rec runner.NodeRecord, a types.RunnerPendingAction) map[string]any {
	return map[string]any{"record_id": rec.ID, "action_id": a.ID, "runner_id": a.RunnerID, "attestation": c.Attestation,
		"source": c.Source, "source_id": c.SourceID, "people": len(rec.Approvers), "four_eyes": c.FourEyes,
		"name_reused": c.NameReused, "no_causal_association": c.NoCausalAssociation}
}

// deliverNodeRecord ends the runner's session on this replica, if it holds one, so the runner reconnects
// and reads the record on its fresh session's PENDING; a runner offline or on another replica reads it on
// its next connection. A failure only delays delivery.
func (s *Server) deliverNodeRecord(ctx context.Context, runnerID uuid.UUID) {
	st, ok := s.cfg.Store.(store.RunnerStore)
	if !ok {
		return
	}
	if err := s.runnerHub(st).Goaway(ctx, runnerID); err != nil {
		slog.WarnContext(ctx, "wardynd: a node record waits for the runner's next session", slog.String("runner_id", runnerID.String()), slog.Any("err", err))
	}
}

// applyRunNodeRecordChange applies a held node record inside the decision transaction: the proposer and
// the approver are its two people. The admin token is never one of them. The store re-checks the run's
// containment record against what the proposal was shown; a moved record lapses the approvals.
func applyRunNodeRecordChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	if isAdminTokenCaller(r) {
		return govApplied{}, &govRefusal{why: "admin token on a node record", write: func(w http.ResponseWriter, _ *http.Request) {
			writeErrorReason(w, http.StatusForbidden, reasonNodeRecordHumanOnly, "A node record is made by people. The admin token cannot approve one.")
		}}
	}
	var c runNodeRecordChange
	if err := decodeHeldPayload(ch.Payload, &c); err != nil {
		return govApplied{}, err
	}
	proposer, err := s.nodeRecordApprover(ch.ProposedBy, ch.ProposedByEmail)
	if err != nil {
		return govApplied{}, err
	}
	approver, err := s.nodeRecordApprover(principalFromRequest(r), oidcEmailFromContext(r.Context()))
	if err != nil {
		return govApplied{}, err
	}
	rec, err := c.record(proposer, approver)
	if err != nil {
		return govApplied{}, err
	}
	a, err := store.QueueRunNodeRecordQ(r.Context(), q, store.RunNodeRecordWrite{RunID: c.RunID, Record: rec, NameReused: c.NameReused, NoCausalAssociation: c.NoCausalAssociation})
	if nodeRecordQueueRefusal(err) != "" {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: auditRunNodeRecord, target: c.RunID.String(), data: nodeRecordAuditData(c, rec, a),
		afterCommit: func() map[string]any {
			s.deliverNodeRecord(context.WithoutCancel(r.Context()), a.RunnerID)
			return nil
		}}, nil
}

// recordRunContainmentEvents audits a committed state page's containment-pending transitions.
func (s *Server) recordRunContainmentEvents(ctx context.Context, events []store.RunContainmentEvent) {
	for _, ev := range events {
		runID := ev.RunID
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "runner:"+ev.RunnerID.String(), auditRunContainmentPending, runID.String(), "success",
			mustJSON(map[string]any{"state": ev.State, "kind": ev.Kind, "name_reused": ev.NameReused, "no_causal_association": ev.NoCause,
				"evidence_source": types.RunEvidenceRunnerAsserted})))
	}
}
