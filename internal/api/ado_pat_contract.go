// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The contract a `minted_pat` run's personal access token is built against:
// the value the vssps API returns, the client that creates and revokes it, the
// refusal it can come back with, and the reasons and audit actions every lane
// that touches one uses. It holds no behaviour beyond naming these: the
// client (a vssps caller), the org check, dispatch, renewal and the sweeps
// each live with their own lane and code against this file.
package api

import (
	"context"
	"fmt"
	"time"
)

// adoPAT is a personal access token Azure DevOps created for a person. Token is
// the secret value, which the create response carries once and nothing else
// returns; it lives in memory only, is never logged or audited, and is never
// written to a row (ado_run_pats holds no secret).
type adoPAT struct {
	AuthorizationID string
	Token           string
	Scope           string
	ValidTo         time.Time
}

// adoPATRequest is what a create asks for. The token is always organisation
// scoped (`allOrgs:false`), so that is not a field: no caller may ask for a
// token that reaches every organisation the person belongs to.
type adoPATRequest struct {
	DisplayName string
	Scope       string // adoscope.PATScope: unqualified, space-joined
	ValidTo     time.Time
}

// adoPATClient creates and revokes a person's personal access tokens with that
// person's delegated Entra access token. There is deliberately no Update: whether
// an update keeps the token value is undocumented, so a widened token is a new
// create and a revoke of the old, and nothing relies on the difference.
//
// Every failure is an *adoPATError when Azure DevOps answered, so a caller can
// read its Reason; any other error means the call did not complete.
type adoPATClient interface {
	Create(ctx context.Context, org, accessToken string, req adoPATRequest) (adoPAT, error)
	Revoke(ctx context.Context, org, accessToken, authorizationID string) error
}

// The SessionTokenError values Azure DevOps names that Reason tells apart.
const (
	adoPATErrLifespan        = "patLifespanPolicyViolation"
	adoPATErrGlobalPolicy    = "globalPatPolicyViolation"
	adoPATErrFullScopePolicy = "fullScopePatPolicyViolation"
	adoPATErrAccessDenied    = "accessDenied"
)

// The reasons an adoPATError maps to. Machine-readable, and the fix each one
// names is the admin's, not the person's.
const (
	adoPATReasonPolicyBlocked  = "ado_pat_policy_blocked"  // the organisation restricts who may create tokens
	adoPATReasonLifespanPolicy = "ado_pat_lifespan_policy" // the requested life is above the organisation's maximum
	adoPATReasonConsentNeeded  = "ado_pat_consent_needed"  // the sign-in's grant cannot create tokens: consent or scope is missing
	adoPATReasonMintRefused    = "ado_pat_mint_refused"    // any other refusal
)

// adoPATError is Azure DevOps refusing a create or a revoke: the HTTP status,
// the X-TFS-ServiceError header text if any, and the response body's
// patTokenError if any. It never holds a token.
type adoPATError struct {
	Status        int
	ServiceError  string
	PatTokenError string
}

func (e *adoPATError) Error() string {
	return fmt.Sprintf("azure devops personal access token: HTTP %d, patTokenError %q, service error %q",
		e.Status, e.PatTokenError, e.ServiceError)
}

// Reason maps the refusal to one of the adoPATReason* values.
//
// The named patTokenError decides first. With none, a 401 or 403 is a token
// that could not create at all — the missing-scope cause — and anything else
// is a refusal nobody has classified. How the organisation's "Restrict
// personal access token creation" policy answers an API call is not published,
// so accessDenied is read as that policy; the refusal walk in the live tests
// is what confirms or corrects it.
func (e *adoPATError) Reason() string {
	switch e.PatTokenError {
	case adoPATErrLifespan:
		return adoPATReasonLifespanPolicy
	case adoPATErrGlobalPolicy, adoPATErrFullScopePolicy, adoPATErrAccessDenied:
		return adoPATReasonPolicyBlocked
	case "":
		if e.Status == 401 || e.Status == 403 {
			return adoPATReasonConsentNeeded
		}
	}
	return adoPATReasonMintRefused
}

// Why a token was created, and why it was revoked: the `reason` field of the
// audit rows below, so a reader can tell a renewal from a widening.
const (
	adoPATMintDispatch = "dispatch"
	adoPATMintRenewal  = "renewal"
	adoPATMintWiden    = "widen"
	adoPATMintResume   = "resume"
	adoPATMintRestart  = "restart"

	adoPATRevokeRunEnd     = "run_end"
	adoPATRevokeKill       = "kill"
	adoPATRevokePause      = "pause"
	adoPATRevokeDrift      = "drift"
	adoPATRevokeRenewal    = "renewal"
	adoPATRevokeWiden      = "widen"
	adoPATRevokeDisconnect = "disconnect"
	adoPATRevokeSweep      = "sweep"
)

// The audit actions the personal-access-token lanes write. None carries the
// token value: an authorization id, a scope, an expiry and a reason at most.
const (
	adoPATAuditMint           = "ado_pat.mint"
	adoPATAuditRevoke         = "ado_pat.revoke"
	adoPATAuditMintDenied     = "ado_pat.mint.denied"
	adoPATAuditRevokeFailed   = "ado_pat.revoke.failed"
	adoPATAuditOrgCheck       = "ado_pat.org_check"
	adoPATAuditOwnStore       = "ado_pat.own.store"
	adoPATAuditOwnDelete      = "ado_pat.own.delete"
	adoPATAuditOwnMismatch    = "ado_pat.own.identity_mismatch"
	adoBearerAuditRefusedMint = "ado_bearer.refused_mint_scopes"
	adoSharedCredentialRetire = "ado_shared_credential.retire"
)
