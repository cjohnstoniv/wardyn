// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (s *Server) mountRunnerRegistrationRoutes(r chi.Router) {
	r.Post("/runners/register", s.handleRunnerRegister)
	r.Group(func(r chi.Router) {
		r.Use(s.humanOrAdminAuth)
		r.With(s.requireOperator).Post("/runners/tokens", s.handleRunnerTokenAdmin)
		r.Post("/me/runners/tokens", s.handleRunnerTokenSelf)
		r.Post("/me/runners/{id}/claim", s.handleRunnerClaim)
	})
}

// mountOrgRunnerRoutes are the organization runner doors, all on the security tier (routes.go):
//
//	POST /runners/org-tokens       mint a token for an org runner
//	POST /runners/{id}/claim-org   claim it, under a human session
//	PUT  /runners/{id}             set an org runner's limits (runner_limits.go)
//	PUT  /runners/{id}/trust       change a runner's trust class (runner_registration_trust.go)
//	POST /runs/{id}/node-record    release a run its Kubernetes runner holds as containment pending (run_containment.go)
func (s *Server) mountOrgRunnerRoutes(securityOps chi.Router) {
	securityOps.Post("/runners/org-tokens", s.handleRunnerOrgToken)
	securityOps.Post("/runners/{id}/claim-org", s.handleRunnerClaimOrg)
	securityOps.Put("/runners/{id}", s.handleUpdateOrgRunner)
	securityOps.Put("/runners/{id}/trust", s.handlePutRunnerTrust)
	securityOps.Post("/runs/{id}/node-record", s.handlePostRunNodeRecord)
}

func (s *Server) runnerRegistrationStore(w http.ResponseWriter) (store.RunnerRegistrationStore, bool) {
	if err := federation.CheckOrgURL(s.cfg.RunnerOrgURL); err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRunnerRegistrationUnavailable, "configure WARDYN_RUNNER_ORG_URL with this organisation's public HTTPS URL to enable runner registration")
		return nil, false
	}
	rs, ok := s.cfg.Store.(store.RunnerRegistrationStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonRunnerRegistrationUnavailable, "runner registration is unavailable")
	}
	return rs, ok
}

// runnerSessionOwner is the signed-in person a runner door acts for: never an administrative or
// delegated credential, which owns no runner. The refusal's reason and sentence are the caller's.
func (s *Server) runnerSessionOwner(w http.ResponseWriter, r *http.Request, reason, msg string) (string, bool) {
	owner := oidcHumanFromContext(r.Context())
	if owner == "" || neverOperator(r.Context()) {
		writeErrorReason(w, http.StatusForbidden, reason, msg)
		return "", false
	}
	return owner, true
}

func (s *Server) runnerPersonalOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	return s.runnerSessionOwner(w, r, string(placement.ReasonRunnerClaimMismatch), "sign in as the runner's owner; administrative and delegated credentials cannot claim for a person")
}

const runnerSelfHostRefusal = "hosting your own runner isn't available to you. Ask your admin for the self_host_runner permission."

func (s *Server) handleRunnerTokenSelf(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return
	}
	if !s.runnerTokenLimiter.allow(owner, s.cfg.Now().UTC()) {
		w.Header().Set("Retry-After", "360")
		writeErrorReason(w, http.StatusTooManyRequests, reasonRunnerTokenRateLimited, "too many runner registration tokens; retry later")
		return
	}
	// An operator is exempt even with WARDYN_GOVERN_ADMIN_RUNS on: the admin mint (POST /runners/tokens)
	// lets an operator name themself, so asking here would bound nothing.
	if !s.isOperator(r.Context()) && s.denyUserCapability(w, r, capFeature, featureSelfHostRunner, "me.runners.tokens", runnerSelfHostRefusal) {
		return
	}
	var req struct{}
	if !decodeStrict(w, r, &req) {
		return
	}
	s.mintRunnerToken(w, r, types.RunnerRegistrationToken{Owner: owner}, time.Hour, nil)
}

func (s *Server) handleRunnerTokenAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	var req types.RunnerTokenRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	owner := strings.TrimSpace(req.Owner)
	if owner == "" || len(owner) > 512 || !controlCharFree(owner) || s.isReservedPrincipal(owner) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonRunnerOwnerInvalid, "owner must name a person's principal")
		return
	}
	// The same resolver every admin-names-a-person route uses, so an email reaches the principal the
	// person signs in as. A token for a name nobody is known by could only mint a runner no one can claim.
	resolved, known, refusal, err := s.resolveSecretOwner(r.Context(), owner)
	if err != nil {
		writeServerError(w, r, "resolve the runner token's owner", err)
		return
	}
	if refusal != "" || !known {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonRunnerOwnerInvalid, runnerOwnerRefusal(refusal))
		return
	}
	s.mintRunnerToken(w, r, types.RunnerRegistrationToken{Owner: resolved}, 72*time.Hour, nil)
}

// govTargetRunnerOrgToken is the target_kind of the governance.change.bypass row an admin-token org mint
// writes. It is never held: a human mint is direct, and the four-eyes rule falls on the claim.
const govTargetRunnerOrgToken = "runner_org_token"

// handleRunnerOrgToken mints a single-use, 72 h token for an organization's runner (POST
// /runners/org-tokens, the security tier). With WARDYN_GOVERNANCE_SECOND_HUMAN on, a human's mint is
// direct and the claim must be someone else's; the admin token's is break-glass: applied, stored
// break_glass, with a governance.change.bypass row, and never counted as a second human.
func (s *Server) handleRunnerOrgToken(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	var req types.RunnerOrgTokenRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 200 || !controlCharFree(name) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonRunnerRegistrationInvalid, "name must be 1 to 200 bytes without control characters")
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	s.mintRunnerToken(w, r, types.RunnerRegistrationToken{OwnerKind: types.RunnerOwnerOrg, BreakGlass: mode == govBypass}, 72*time.Hour,
		map[string]any{"name": name})
}

// runnerOwnerRefusal words resolveSecretOwner's refusal for this route's `owner` field: its own sentences
// open "?owner=", which this route does not take.
func runnerOwnerRefusal(refusal string) string {
	switch refusal {
	case secretOwnerAmbiguousMsg:
		return "That matches more than one person. Name the owner by subject, exactly as the Audit log shows it."
	case secretOwnerUnresolvedMsg:
		return "That email address doesn't match anyone this deployment knows. Name the owner by subject, as the Audit log shows it."
	}
	return "No person on this deployment is known by that name, so no one could claim a runner minted for it. Name someone who has signed in."
}

// mintRunnerToken mints seed (its Owner, OwnerKind and BreakGlass) for ttl as the caller and answers
// it. extra joins the runner.token.create row's data.
func (s *Server) mintRunnerToken(w http.ResponseWriter, r *http.Request, seed types.RunnerRegistrationToken, ttl time.Duration, extra map[string]any) {
	rs, ok := s.runnerRegistrationStore(w)
	if !ok {
		return
	}
	now := s.cfg.Now().UTC()
	raw := newBearer("wdr_")
	seed.ID, seed.MintedBy, seed.CreatedAt, seed.ExpiresAt = uuid.New(), principalFromRequest(r), now, now.Add(ttl)
	seed.OrgURLSHA256 = federation.OrgURLSHA256(s.cfg.RunnerOrgURL)
	seed.OwnerKind = cmp.Or(seed.OwnerKind, types.RunnerOwnerPerson)
	seed.MintedByKind = types.RunnerMintedByHuman
	if isAdminTokenCaller(r) {
		seed.MintedByKind = types.RunnerMintedByAdminToken
	} else if seed.OwnerKind == types.RunnerOwnerOrg {
		seed.MintedByEmail = oidcEmailFromContext(r.Context()) // only the org claim's four-eyes check reads it
	}
	mintCtx := r.Context()
	if seed.OwnerKind == types.RunnerOwnerPerson {
		mintCtx = guardOwner(mintCtx, seed.Owner) // a deactivated owner is refused inside the insert
	}
	token, err := rs.MintRunnerRegistrationToken(mintCtx, raw, seed)
	if err != nil {
		writeServerError(w, r, "mint runner registration token", err)
		return
	}
	data := map[string]any{"owner": seed.Owner, "expires_at": token.ExpiresAt, "owner_kind": seed.OwnerKind, "minted_by_kind": seed.MintedByKind}
	for k, v := range extra {
		data[k] = v
	}
	if seed.BreakGlass {
		data["break_glass"] = true
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		placement.ActionRunnerTokenCreate, token.ID.String(), "success", mustJSON(data)))
	if seed.BreakGlass {
		s.recordGovernanceBypass(r, govTargetRunnerOrgToken, token.ID.String(), "success", nil)
	}
	token.Token, token.OrgURL = raw, s.cfg.RunnerOrgURL
	// The runner's architecture is unknown at mint, so a per-architecture image is chosen at register,
	// and the mint answer then stays silent rather than printing one architecture's image for every
	// computer. Without per-architecture images it carries WARDYN_PROXY_IMAGE as it always has.
	if len(s.cfg.ProxyImageByArch) == 0 {
		token.ProxyImage = s.cfg.ProxyImage
	}
	writeJSON(w, http.StatusCreated, token)
}
