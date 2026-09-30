// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// ado_pat_orgcheck.go is "Check organisation settings" for a minted_pat row:
// the evidence an admin needs before people's runs depend on it.
//
// Wardyn cannot read an organisation's token policies, so the check tries
// them with the admin's own connection:
//
//  1. the stored grant must hold both token permissions;
//  2. canary 1, a vso.profile token for the row's longest token life, must be
//     created (then revoked at once) — refused with the lifespan violation, the
//     row's life is above the organisation's maximum;
//  3. canary 2, the same for 364 days, is the lifespan probe. The organisation
//     policy is "on" ONLY when Azure DevOps answers patLifespanPolicyViolation;
//     a created token means "off" (and is revoked at once); anything else,
//     invalidValidTo included, is "unknown" — never read as on. 364 rather
//     than 366 days: a year is Azure DevOps' own ceiling on every tenant, so a
//     longer canary is refused whatever the policy says.
//
// Each canary is one "token created" email to the admin (D16), which is why the
// check runs only when an admin asks for it.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoOrgCheckCanaryLife is canary 2's life: under the one-year ceiling, far
// above any lifespan policy worth having.
const adoOrgCheckCanaryLife = 364 * 24 * time.Hour

// adoOrgCheckTokenName is the display name both canaries carry, so the admin
// can tell them apart from a run's in Azure DevOps.
const adoOrgCheckTokenName = "Wardyn organisation check"

// The check's answers.
const (
	adoOrgCheckGranted  = "granted"
	adoOrgCheckMissing  = "missing"
	adoOrgCheckAccepted = "accepted"
	adoOrgCheckRefused  = "refused"
	adoOrgCheckOn       = "on"
	adoOrgCheckOff      = "off"
	adoOrgCheckUnknown  = "unknown"
)

// adoOrgCheckResult is the check's answer. An empty TokenLife or Lifespan
// means that step was not reached.
type adoOrgCheckResult struct {
	CheckedAt    time.Time `json:"checked_at"`
	Organisation string    `json:"organisation"`
	PATMaxHours  int       `json:"pat_max_hours"`
	// Permissions: granted | missing — the stored grant's token permissions.
	Permissions string `json:"permissions"`
	// TokenLife: accepted | refused — canary 1, a token of PATMaxHours.
	TokenLife string `json:"token_life,omitempty"`
	// Refusal is canary 1's reasonADOPAT* when it was refused.
	Refusal string `json:"refusal,omitempty"`
	// Lifespan: on | off | unknown — the organisation's maximum token lifespan
	// policy, from canary 2.
	Lifespan string `json:"lifespan,omitempty"`
	// Unrevoked names canaries Wardyn created and could not revoke: the admin
	// revokes them under Personal access tokens.
	Unrevoked []string `json:"unrevoked,omitempty"`
}

// mountADOOrgCheckRoute mounts the check on the operator tier, beside the
// provider rows it checks.
func (s *Server) mountADOOrgCheckRoute(operatorOnly chi.Router) {
	operatorOnly.Post("/workspace-providers/git/{id}/org-check", s.handleADOOrgCheck)
}

// handleADOOrgCheck serves POST /workspace-providers/git/{id}/org-check,
// optionally ?organisation= when the row's base URL names none.
//
// D-6: an unknown row, a row that is not Azure DevOps, not minted_pat, or not
// the deployment's sign-in row, are all answered identically.
func (s *Server) handleADOOrgCheck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	subject := oidcHumanFromContext(ctx)
	if subject == "" {
		writeErrorReason(w, http.StatusForbidden, reasonADOSignInNoSession,
			"The organisation check uses your own Azure DevOps connection, so it must be run from a signed-in browser session")
		return
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeServerError(w, r, "read site config", err)
		return
	}
	row, cfg, ok, err := s.adoOrgCheckRow(ctx, sc, chi.URLParam(r, "id"))
	switch {
	case err != nil:
		writeServerError(w, r, "read the Azure DevOps sign-in configuration", err)
		return
	case !ok:
		writeErrorReason(w, http.StatusNotFound, reasonADOOrgCheckUnknownRow, "No Azure DevOps row that creates tokens has that id")
		return
	case cfg.ClientSecret == "" || errors.Is(cfg.validate(), ErrADOMintNeedsSecret):
		writeErrorReason(w, http.StatusConflict, ReasonADOPATNeedsConsoleApp, adoPATNeedsConsoleAppRefusal)
		return
	}
	org := r.URL.Query().Get("organisation")
	if org == "" {
		org = adoRowOrganisation(row)
	}
	if _, valid := adoOrganisationLabel(org); !valid || !rowServesOrganisation(row, org) {
		writeErrorReason(w, http.StatusBadRequest, reasonADOOrgCheckOrganisation,
			"Name the Azure DevOps organisation to check (?organisation=): this row's address names none, or not that one")
		return
	}
	res, err := s.runADOOrgCheck(ctx, cfg, subject, org, row.Entra.PATHours())
	if err != nil {
		class := ADOEntraClassify(err)
		status, body := adoResolveFailureAnswer(class)
		writeErrorReason(w, status, string(class), body)
		return
	}
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorHuman, subject, adoPATAuditOrgCheck, row.ID, "success", mustJSON(res)))
	writeJSON(w, http.StatusOK, res)
}

// adoOrgCheckRow is the row the check may run on: an enabled minted_pat Azure
// DevOps row that is this deployment's sign-in row. ok=false for every other
// id, whatever the reason (D-6).
func (s *Server) adoOrgCheckRow(ctx context.Context, sc types.SiteConfig, id string) (types.GitProvider, ADOEntraConfig, bool, error) {
	row, found := gitProviderRowByID(sc, id)
	if !found || row.Kind != types.GitProviderAzureDevOps || row.Entra == nil ||
		row.Entra.TokenMode != types.ADOTokenModeMintedPAT || s.cfg.ADOEntra == nil {
		return types.GitProvider{}, ADOEntraConfig{}, false, nil
	}
	cfg, found, err := s.cfg.ADOEntra(ctx)
	if err != nil || !found || cfg.RowID != row.ID {
		return types.GitProvider{}, ADOEntraConfig{}, false, err
	}
	return row, cfg, true, nil
}

// adoRowOrganisation is the first organisation a row's base URLs name, or "".
func adoRowOrganisation(row types.GitProvider) string {
	for _, raw := range row.BaseURLs {
		if org, ok := adoOrganisationOf(raw); ok {
			return org
		}
	}
	return ""
}

// runADOOrgCheck runs the three steps. An error is a redemption failure other
// than missing consent (not connected, a dead sign-in, an outage): nothing was
// learned about the organisation.
func (s *Server) runADOOrgCheck(ctx context.Context, cfg ADOEntraConfig, owner, org string, hours int) (adoOrgCheckResult, error) {
	now := s.cfg.Now()
	res := adoOrgCheckResult{CheckedAt: now.UTC(), Organisation: org, PATMaxHours: hours, Permissions: adoOrgCheckGranted}
	if _, err := s.mintAccess(ctx, cfg, owner); errors.Is(err, ErrADOEntraConsentRequired) {
		res.Permissions = adoOrgCheckMissing
		return res, nil
	} else if err != nil {
		return adoOrgCheckResult{}, err
	}

	refusal, err := s.adoOrgCheckCanary(ctx, cfg, owner, org, now.Add(time.Duration(hours)*time.Hour), &res)
	switch {
	case err != nil:
		return adoOrgCheckResult{}, err
	case refusal == nil:
		res.TokenLife = adoOrgCheckAccepted
	case refusal.PatTokenError == adoPATErrLifespan:
		// The row's own life is over the policy's maximum: the policy is on,
		// measured by the one answer that means it.
		res.TokenLife, res.Refusal, res.Lifespan = adoOrgCheckRefused, refusal.Reason(), adoOrgCheckOn
		return res, nil
	default:
		res.TokenLife, res.Refusal, res.Lifespan = adoOrgCheckRefused, refusal.Reason(), adoOrgCheckUnknown
		return res, nil
	}

	switch refusal, err = s.adoOrgCheckCanary(ctx, cfg, owner, org, now.Add(adoOrgCheckCanaryLife), &res); {
	case err == nil && refusal == nil:
		res.Lifespan = adoOrgCheckOff
	case err == nil && refusal.PatTokenError == adoPATErrLifespan:
		res.Lifespan = adoOrgCheckOn
	default:
		res.Lifespan = adoOrgCheckUnknown
	}
	return res, nil
}

// adoOrgCheckCanary creates one vso.profile token valid to validTo and revokes
// it at once. (nil, nil) when it was created; the refusal when Azure DevOps
// refused it; an error when the call did not complete.
func (s *Server) adoOrgCheckCanary(ctx context.Context, cfg ADOEntraConfig, owner, org string, validTo time.Time, res *adoOrgCheckResult) (*adoPATError, error) {
	pat, err := s.mintADOPAT(ctx, cfg, owner, org, adoPATRequest{DisplayName: adoOrgCheckTokenName, Scope: "vso.profile", ValidTo: validTo})
	var perr *adoPATError
	if errors.As(err, &perr) {
		return perr, nil
	} else if err != nil {
		return nil, err
	}
	if rerr := s.revokeADOPAT(ctx, cfg, owner, org, pat.AuthorizationID); rerr != nil {
		res.Unrevoked = append(res.Unrevoked, pat.AuthorizationID)
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorHuman, owner, adoPATAuditRevokeFailed, cfg.RowID, "failure",
			mustJSON(map[string]any{"authorization_id": pat.AuthorizationID, "organisation": org, "reason": "org_check"})))
	}
	return nil, nil
}
