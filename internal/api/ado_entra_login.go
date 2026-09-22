// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// ado_entra_login.go makes the organisation's own console sign-in the PRIMARY
// way a person gets Azure DevOps access, and leaves the dedicated sign-in
// (ado_entra.go) as the fallback it should have been all along.
//
// THE REQUIREMENT IT ANSWERS. An admin configures this once for the
// organisation and it works for everyone; a member may have to allow Wardyn
// access once, and with tenant-wide administrator consent not even that. Nobody
// sets up their own Azure DevOps connectivity. The console login is already an
// authorization-code flow against the organisation's Entra tenant, so the same
// request carries the Azure DevOps scopes and the same callback keeps the
// refresh token. A member who signs into Wardyn is connected, with nothing else
// to do.
//
// WHEN THE LOGIN IS WIDENED, AND ONLY THEN. The Azure DevOps row must name the
// console's OWN sign-in application, in the sign-in tenant. That is the same
// boundary the dedicated sign-in enforces and it exists for the same reason:
// one authorization request yields one id_token from one app registration, and
// binding a credential to a session is only sound when both come from it. A row
// naming any other tenant or client widens nothing — those deployments keep
// using the dedicated sign-in, which was built for exactly that case.
//
// WITH NO AZURE DEVOPS ROW CONFIGURED, NOTHING HERE DOES ANYTHING. LoginScopes
// returns nil, the authorization request is not widened, no parameter is
// overridden, and the callback stores nothing. That is the property
// TestLoginScopesAreEmptyWithoutAConfiguredRow pins, because "this changes
// nothing for every existing deployment" is a claim, and a claim needs a test.
//
// THE LOGIN IS NEVER AT RISK. Every path here returns without complaint: a
// tenant that will not issue the Azure DevOps scopes, a person who declines
// consent, a wedged secret store. The person is signed into the console and
// simply has no Azure DevOps credential yet, which is a state the rest of the
// system already describes. Failing a login over a second resource's consent is
// how an organisation gets locked out of its own console.

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// adoEntraSourceLogin marks a credential acquired by the organisation's console
// sign-in; adoEntraSourceSignIn (ado_entra.go) marks one acquired by the
// dedicated sign-in. The pair is what lets a reader of the stored state tell
// "connected by the organisation's sign-in" from "connected by a person's own
// errand" — which is the difference between a deployment where this works out
// of the box and one where each person had to go and fix it themselves.
const adoEntraSourceLogin = "login"

// The Server IS the login-grant sink. Asserted here so a change to either side
// of the seam fails at compile time rather than by silently detaching the
// primary path and leaving only the fallback.
var _ oidc.LoginGrantSink = (*Server)(nil)

// LoginScopes implements the request half of oidc.LoginGrantSink: the extra
// scopes the console login should ask consent for.
//
// It returns the row's whole ceiling plus `offline_access`, never a subset.
// Consent is the only narrowing that has any effect on this service — a later
// token request naming a subset is answered with everything consented — so the
// ceiling asked for here IS the reach the organisation is choosing to grant,
// and asking for less at login would mean a second consent prompt later for
// anything left out. `openid` is already in the login's base scopes.
//
// nil on every doubt: no source wired, no row, an unusable row, or a row
// against another tenant or application. A nil answer leaves the authorization
// request exactly as it was.
func (s *Server) LoginScopes(ctx context.Context) []string {
	cfg, ok := s.adoEntraForLogin(ctx, "compose the login request")
	if !ok {
		return nil
	}
	return append(slices.Clone(cfg.Scopes), entraOfflineAccessScope)
}

// CaptureLoginGrant implements the capture half of oidc.LoginGrantSink: it
// stores the Azure DevOps refresh token the console login just earned.
//
// THE IDENTITY BINDING IS SATISFIED BY CONSTRUCTION HERE, not skipped. The
// dedicated sign-in must prove that the id_token it received belongs to the
// session that started the flow, because those are two different flows. At
// login they are ONE flow: subject is the verified id_token's own subject, and
// it is the principal the session is being issued for. There is no second token
// to disagree with it, so the guarantee is the same one, reached by a shorter
// road — and the credential still lands under that subject's own namespace and
// nowhere else.
//
// It reports nothing and refuses nothing. Every early return below is a case
// where the person is signed in and simply has no Azure DevOps credential yet.
func (s *Server) CaptureLoginGrant(ctx context.Context, subject string, grant oidc.LoginGrant) {
	cfg, ok := s.adoEntraForLogin(ctx, "capture the login grant")
	if !ok || subject == "" || grant.RefreshToken == "" {
		return
	}
	// The GRANTED scopes, intersected with the row's ceiling. Reading the
	// granted string rather than assuming the request was honoured is the whole
	// discipline of this lane: the tenant decides, and it routinely grants a
	// different set than was asked for.
	granted := adoEntraSplitScope(grant.Scope)
	var usable []string
	for _, sc := range cfg.Scopes {
		if slices.Contains(granted, sc) {
			usable = append(usable, sc)
		}
	}
	if len(usable) == 0 {
		// CONSENT DECLINED, or a tenant that will not issue these scopes. The
		// login has already succeeded and stays succeeded; there is simply
		// nothing durable to store.
		//
		// No audit row: this is one event per login per person on a tenant that
		// never issues them, so a row here would flood the trail of an
		// organisation that has not finished setting this up — and it records
		// nothing a reader cannot already see, since "this person has no Azure
		// DevOps credential" is exactly the absence of a stored blob. A capture
		// that HAD material and failed is audited, below.
		slog.DebugContext(ctx, "wardynd: the console login carried no Azure DevOps scopes; no credential captured",
			slog.String("row", cfg.RowID))
		return
	}

	// Mask BEFORE anything can log or persist it.
	s.cfg.MaskRegistry.AddGlobal([]byte(grant.RefreshToken))

	now := s.cfg.Now()
	expiresAt := grant.Expiry.UTC()
	if expiresAt.IsZero() {
		expiresAt = now.UTC()
	}
	blob := adoEntraBlob{
		RefreshToken: grant.RefreshToken,
		Scopes:       usable,
		ExpiresAt:    expiresAt,
		TenantID:     cfg.TenantID,
		ClientID:     cfg.ClientID,
		Subject:      subject,
		CapturedAt:   now.UTC(),
		Source:       adoEntraSourceLogin,
	}
	// Under the same per-owner lock a redemption takes, so a login landing
	// while a redemption is persisting its rotation cannot interleave with it.
	unlock := s.adoEntra.lock(subject, cfg.RowID)
	defer unlock()
	if err := s.storeADOEntraBlob(ctx, subject, cfg.RowID, blob); err != nil {
		slog.ErrorContext(ctx, "wardynd: storing the Azure DevOps credential this login earned failed; the person is signed in without one",
			slog.String("row", cfg.RowID), slog.Any("err", err))
		s.auditADOCapture(ctx, subject, cfg.RowID, "failure", map[string]any{
			"reason": "store_error", "error": err.Error(), "source": adoEntraSourceLogin,
			"tenant_id": cfg.TenantID, "client_id": cfg.ClientID,
		})
		return
	}
	s.auditADOCapture(ctx, subject, cfg.RowID, "success", map[string]any{
		"tenant_id": cfg.TenantID, "client_id": cfg.ClientID,
		"scopes": usable, "source": adoEntraSourceLogin,
		"expires_at": blob.ExpiresAt.Format(time.RFC3339),
	})
}

// adoEntraForLogin resolves the row for a LOGIN-TIME decision and applies the
// two conditions under which the login may be involved at all: the row must be
// usable, and it must name the console's own sign-in application in the sign-in
// tenant.
//
// Every refusal is SILENT at debug level rather than an error, and that is the
// point of separating it from resolveADOEntra (which answers a human who asked
// for this and therefore gets a sentence). This one runs on every login of
// every person on every deployment, including the overwhelming majority that
// have no Azure DevOps row at all; it must be unremarkable when it declines.
func (s *Server) adoEntraForLogin(ctx context.Context, what string) (ADOEntraConfig, bool) {
	if s.cfg.ADOEntra == nil {
		return ADOEntraConfig{}, false
	}
	cfg, found, err := s.cfg.ADOEntra(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading the Azure DevOps sign-in configuration failed; the console login is unaffected",
			slog.String("step", what), slog.Any("err", err))
		return ADOEntraConfig{}, false
	}
	if !found {
		return ADOEntraConfig{}, false
	}
	if err := cfg.validate(); err != nil {
		slog.WarnContext(ctx, "wardynd: the Azure DevOps sign-in configuration is unusable; the console login is unaffected",
			slog.String("step", what), slog.Any("err", err))
		return ADOEntraConfig{}, false
	}
	if !cfg.isLoginApplication() {
		// Not a failure: a row against another tenant or application is a
		// SUPPORTED configuration, served by the dedicated sign-in. Widening
		// the console login for it would produce an id_token this deployment
		// cannot bind a credential to.
		slog.DebugContext(ctx, "wardynd: the Azure DevOps row names another application, so the console login is not widened for it",
			slog.String("step", what), slog.String("row", cfg.RowID))
		return ADOEntraConfig{}, false
	}
	return cfg, true
}
