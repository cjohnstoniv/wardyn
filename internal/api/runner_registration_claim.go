// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (s *Server) handleRunnerClaim(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return
	}
	rs, ok := s.runnerRegistrationStore(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "runner")
	if !ok {
		return
	}
	var req types.RunnerClaimRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	row, err := rs.GetRunner(r.Context(), id)
	// Another person's runner and an absent one are the same answer, so a refusal never says whose runner it is.
	if errors.Is(err, store.ErrNotFound) || (err == nil && row.Owner != owner) {
		writeErrorReason(w, http.StatusForbidden, string(placement.ReasonRunnerClaimMismatch), runnerClaimUnknownMsg)
		return
	}
	if err != nil {
		writeServerError(w, r, "read runner claim", err)
		return
	}
	now := s.cfg.Now().UTC()
	if msg := runnerClaimRefusal(row, federation.OrgURLSHA256(s.cfg.RunnerOrgURL), now); msg != "" {
		writeErrorReason(w, http.StatusForbidden, string(placement.ReasonRunnerClaimMismatch), msg)
		return
	}
	claimed, err := rs.ClaimRunner(r.Context(), id, owner, req.Fingerprint, now)
	if errors.Is(err, store.ErrConflict) {
		writeErrorReason(w, http.StatusForbidden, string(placement.ReasonRunnerClaimMismatch), runnerClaimAlreadyMsg)
		return
	}
	if errors.Is(err, store.ErrRunnerClaimMismatch) || errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusForbidden, string(placement.ReasonRunnerClaimMismatch), "the fingerprint does not match the one this runner registered with; compare it with what the runner printed")
		return
	}
	if err != nil {
		writeServerError(w, r, "claim runner", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), owner, placement.ActionRunnerClaim,
		claimed.ID.String(), "success", mustJSON(map[string]any{"fingerprint": claimed.KeyFingerprint})))
	writeJSON(w, http.StatusOK, types.RunnerRegistration{RunnerID: claimed.ID, Fingerprint: claimed.KeyFingerprint, State: claimed.State, OrgURLSHA256: claimed.OrgURLSHA256, Limits: claimed.Limits})
}

// orgRunnerClaimStore is the PG capability behind the org claim.
type orgRunnerClaimStore interface {
	ClaimOrgRunner(ctx context.Context, id uuid.UUID, fingerprint, orgHash, by string, now time.Time) (types.Runner, error)
	GetRunnerRegistrationToken(ctx context.Context, id uuid.UUID) (types.RunnerRegistrationToken, error)
}

var _ orgRunnerClaimStore = store.PG{}

// handleRunnerClaimOrg claims an organization's runner (POST /runners/{id}/claim-org, the security tier)
// under a human session, as the person claim does, binding the org owner kind, the organisation URL, the
// fingerprint, the 24 h wait and the unclaimed state in one conditional write; it sets trust_class
// org_cluster. A person's runner is refused here, as an org runner is on the person claim. With
// WARDYN_GOVERNANCE_SECOND_HUMAN on, the claimer must be a different human from a human minter, by
// principal or case-folded email; an admin-token (break-glass) token's claim is allowed and audited so.
func (s *Server) handleRunnerClaimOrg(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	claimer, ok := s.runnerSessionOwner(w, r, string(placement.ReasonRunnerClaimMismatch),
		"sign in under your own account to claim an organization's runner; administrative and delegated credentials cannot claim")
	if !ok {
		return
	}
	rs, ok := s.runnerRegistrationStore(w)
	if !ok {
		return
	}
	oc, ok := rs.(orgRunnerClaimStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonRunnerRegistrationUnavailable, "claiming an organization's runner requires the Postgres store backend")
		return
	}
	id, ok := parseIDParam(w, r, "id", "runner")
	if !ok {
		return
	}
	var req types.RunnerClaimRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	row, err := rs.GetRunner(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && row.OwnerKind != types.RunnerOwnerOrg) {
		writeErrorReason(w, http.StatusForbidden, string(placement.ReasonRunnerClaimMismatch), "no organization runner with that id is waiting to be claimed")
		return
	}
	if err != nil {
		writeServerError(w, r, "read runner claim", err)
		return
	}
	now := s.cfg.Now().UTC()
	orgHash := federation.OrgURLSHA256(s.cfg.RunnerOrgURL)
	if msg := runnerClaimRefusal(row, orgHash, now); msg != "" {
		writeErrorReason(w, http.StatusForbidden, string(placement.ReasonRunnerClaimMismatch), msg)
		return
	}
	if row.RegistrationTokenID == nil {
		writeServerError(w, r, "read the org runner's registration token", errors.New("an org runner row names no registration token"))
		return
	}
	minter, err := oc.GetRunnerRegistrationToken(r.Context(), *row.RegistrationTokenID)
	if err != nil {
		writeServerError(w, r, "read the org runner's registration token", err)
		return
	}
	fourEyes := GovernanceSecondHumanEnabled()
	if fourEyes && minter.MintedByKind != types.RunnerMintedByAdminToken &&
		sameGovernanceHuman(types.GovernanceChange{ProposedBy: minter.MintedBy, ProposedByEmail: minter.MintedByEmail}, claimer, oidcEmailFromContext(r.Context())) {
		s.refuse(w, r, authz.Deny(authz.ReasonSecondHumanRequired, r.URL.Path, envGovernanceSecondHuman+
			" is set: you minted this organization runner's token, so someone else claims it"))
		return
	}
	claimed, err := oc.ClaimOrgRunner(r.Context(), id, req.Fingerprint, orgHash, claimer, now)
	if errors.Is(err, store.ErrConflict) {
		writeErrorReason(w, http.StatusForbidden, string(placement.ReasonRunnerClaimMismatch), runnerClaimAlreadyMsg)
		return
	}
	if errors.Is(err, store.ErrRunnerClaimMismatch) || errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusForbidden, string(placement.ReasonRunnerClaimMismatch), "the fingerprint does not match the one this runner registered with; compare it with what the runner printed")
		return
	}
	if err != nil {
		writeServerError(w, r, "claim org runner", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), claimer, placement.ActionRunnerClaim, claimed.ID.String(), "success",
		mustJSON(map[string]any{"fingerprint": claimed.KeyFingerprint, "owner_kind": claimed.OwnerKind, "trust_class": claimed.TrustClass,
			"four_eyes": fourEyes && minter.MintedByKind == types.RunnerMintedByHuman, "break_glass": minter.BreakGlass})))
	writeJSON(w, http.StatusOK, types.RunnerRegistration{RunnerID: claimed.ID, Fingerprint: claimed.KeyFingerprint, State: claimed.State, OrgURLSHA256: claimed.OrgURLSHA256, Limits: claimed.Limits})
}

const runnerClaimAlreadyMsg = "this runner has already been claimed"

// runnerClaimUnknownMsg answers a claim naming a row that is another person's, absent or swept alike:
// the 24 hour sweep deletes an unclaimed registration, so the row's owner cannot be told apart.
const runnerClaimUnknownMsg = "no unclaimed runner of yours has this id; a runner not claimed within 24 hours is removed, so register it again with a new token and a fresh state directory"

// runnerClaimRefusal says why the owner's own runner row cannot be claimed, or "" when it can. It runs only
// after the owner matched, so naming the cause tells the owner nothing about anyone else's runner.
func runnerClaimRefusal(row types.Runner, orgHash string, now time.Time) string {
	switch {
	case row.OrgURLSHA256 != orgHash:
		return "this runner registered against a different organisation URL than this control plane's now; register it again"
	case row.State == types.RunnerRevoked:
		return "this runner was revoked and cannot be claimed; register it again"
	case row.State != types.RunnerUnclaimed:
		return runnerClaimAlreadyMsg
	case row.ClaimExpired(now):
		return "this runner's registration expired before it was claimed; mint a new token and register it again"
	}
	return ""
}
