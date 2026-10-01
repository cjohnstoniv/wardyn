// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The `minted_pat` lane: a personal access token per run, created in the
// person's name with only the run's access. Dispatch creates the first; the
// resolves create the rest, when the run's token nears its validTo (renewal),
// when an approval adds access (widening), when Azure DevOps refuses the
// current one (stale_jti), and after a pause or a restart left the cache empty.
//
// Every token is recorded in ado_run_pats BEFORE it is handed out, so a crash
// between the create and the first use still leaves it revocable. Nothing here
// revokes a token a host may still hold (ado_run_pat_cache.go says why); the
// revokes live in ado_run_pat_sweep.go.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// adoRunPATInjectFormat is the wire shape of a run's token: Basic with an
	// empty user name, which both the REST surface and git accept.
	adoRunPATInjectFormat = "Basic %s"
	// adoRunPATEndGrace is how far past the run's own end its token may live,
	// and the shortest life a token is ever created with.
	adoRunPATEndGrace = 15 * time.Minute

	// Reasons beyond the contract's: a mint Azure DevOps' refusal of the
	// current token forced, the revoke of that token, and the admin's erase
	// of a person.
	adoPATMintUpstream401   = "upstream_401"
	adoPATRevokeUpstream401 = "upstream_401"
	adoPATRevokeOffboarding = "offboarding"
)

// The launch and resolve refusals: the approved mock's sentences and the
// owner-approved additions (2026-09-30).
const (
	adoRunPATNotConnected  = "Connect Azure DevOps once before launching; Wardyn creates the run's token from that connection."
	adoRunPATPolicyBlocked = "Azure DevOps refused to create a token for this run: your organisation restricts who can create personal access tokens. Ask an Azure DevOps administrator to add you to the allow list."
	adoRunPATLifespan      = "Longest token life is above your organisation's maximum token lifespan. Lower it."
	adoRunPATConsentNeeded = "Your app registration doesn't have the Azure DevOps token permissions yet. Add vso.pats and vso.pats_manage and grant admin consent."
	adoRunPATRefused       = "Azure DevOps refused to create a token for this run."
	adoRunPATUnreachable   = "Wardyn couldn't reach Azure DevOps to create this run's token. Try again."
	adoRunPATUnavailable   = "This deployment can't create Azure DevOps tokens for runs, so this run can't reach Azure DevOps. Ask an administrator to choose another way to connect."
	adoRunPATPaused        = "This run is paused, so Wardyn creates no Azure DevOps token for it until it resumes."
	adoRunPATEnded         = "This run has ended, so Wardyn creates no Azure DevOps token for it."
	adoRunPATRunUnread     = "Could not read this run, so no Azure DevOps token was created for it"
)

var (
	errADOPATUnavailable = errors.New("azure devops personal access tokens: this deployment cannot create one")
	errADOPATRunPaused   = errors.New("azure devops personal access tokens: the run is paused")
	errADOPATRunEnded    = errors.New("azure devops personal access tokens: the run has ended")
	errADOPATRunUnread   = errors.New("azure devops personal access tokens: the run could not be read")
)

// adoInjectFormat is the header format a run's grants are authored with.
func adoInjectFormat(m types.ADOTokenMode) string {
	if m == types.ADOTokenModeMintedPAT {
		return adoRunPATInjectFormat
	}
	return adoEntraInjectFormat
}

// adoRunPATValue is the header value a token is sent as.
func adoRunPATValue(token string) string {
	return fmt.Sprintf(adoRunPATInjectFormat, base64.StdEncoding.EncodeToString([]byte(":"+token)))
}

// adoRunPATValidTo is when a token created now expires: the row's longest
// token life, but never more than adoRunPATEndGrace past the run's own end,
// and never less than adoRunPATEndGrace from now.
func adoRunPATValidTo(now time.Time, hours int, endsAt *time.Time) time.Time {
	v := now.Add(time.Duration(hours) * time.Hour)
	if endsAt != nil {
		if end := endsAt.Add(adoRunPATEndGrace); end.Before(v) {
			v = end
		}
	}
	if floor := now.Add(adoRunPATEndGrace); v.Before(floor) {
		v = floor
	}
	return v.UTC().Truncate(time.Second)
}

// runPATClient is the token API a run's creates and revokes go to: the
// sign-in configuration's own vssps client, unless a test installed one.
func (s *Server) runPATClient(cfg ADOEntraConfig) (adoPATClient, error) {
	if s.adoPATs != nil {
		return s.adoPATs, nil
	}
	client, err := cfg.patClient()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errADOPATUnavailable, err)
	}
	return client, nil
}

// runPATRow is the ado_run_pats row for pat.
func runPATRow(runID uuid.UUID, sn adoEntraScopeSnapshot, pat adoPAT) (store.RunPAT, error) {
	id, err := uuid.Parse(pat.AuthorizationID)
	if err != nil {
		return store.RunPAT{}, fmt.Errorf("azure devops returned authorization id %q: %w", pat.AuthorizationID, err)
	}
	return store.RunPAT{
		RunID: runID, AuthorizationID: id, Owner: sn.OwnerSubject, ProviderRowID: sn.ProviderRowID,
		Org: sn.Organisation, Scope: pat.Scope, ValidTo: pat.ValidTo,
	}, nil
}

// mintRunPAT creates a token for runID scoped to caps, records it, masks it and
// audits it. The caller holds the run's cache entry.
func (s *Server) mintRunPAT(ctx context.Context, runID uuid.UUID, sn adoEntraScopeSnapshot, cfg ADOEntraConfig,
	caps []adoscope.Capability, validTo time.Time, reason string,
) (adoPAT, error) {
	st, ok := s.cfg.Store.(store.RunPATStore)
	if !ok {
		return adoPAT{}, errADOPATUnavailable
	}
	client, err := s.runPATClient(cfg)
	if err != nil {
		return adoPAT{}, err
	}
	scope, err := adoscope.PATScope(caps)
	if err != nil {
		return adoPAT{}, err
	}
	denied := func(err error) (adoPAT, error) {
		data := map[string]any{"reason": reason, "owner": sn.OwnerSubject, "provider_row": sn.ProviderRowID,
			"organisation": sn.Organisation, "scope": scope}
		var pe *adoPATError
		if errors.As(err, &pe) {
			data["refusal"], data["status"], data["pat_token_error"] = pe.Reason(), pe.Status, pe.PatTokenError
		} else {
			data["refusal"] = string(ADOEntraClassify(err))
		}
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", adoPATAuditMintDenied,
			runID.String(), "failure", mustJSON(data)))
		return adoPAT{}, err
	}
	// Read before the redeem: a disconnect or erase from here on is caught
	// after the record (adoSignInEnds).
	ends := s.adoSignInEnds.read(sn.OwnerSubject)
	// mintAccess holds S1: the sign-in is redeemed only with the console's
	// own secret.
	access, err := s.mintAccess(ctx, cfg, sn.OwnerSubject)
	if err != nil {
		return denied(err)
	}
	pat, err := client.Create(ctx, sn.Organisation, access.AccessToken, adoPATRequest{
		DisplayName: "Wardyn run " + runID.String()[:8], Scope: scope, ValidTo: validTo,
	})
	var pe *adoPATError
	if errors.As(err, &pe) && pe.Reason() == reasonADOPATPolicyBlocked {
		s.noteADOMintBlocked(ctx, cfg.RowID, sn.OwnerSubject, true) // /me/scm-access: blocked by the organisation
	}
	if err != nil {
		return denied(err)
	}
	s.noteADOMintBlocked(ctx, cfg.RowID, sn.OwnerSubject, false)
	// Mask before anything can render it: the value, and both forms it rides in.
	s.cfg.MaskRegistry.Add(runID, []byte(pat.Token))
	s.cfg.MaskRegistry.Add(runID, []byte(base64.StdEncoding.EncodeToString([]byte(":"+pat.Token))))
	s.cfg.MaskRegistry.Add(runID, []byte(adoRunPATValue(pat.Token)))
	if pat.ValidTo.IsZero() {
		pat.ValidTo = validTo
	}
	if pat.Scope == "" {
		pat.Scope = scope
	}
	row, err := runPATRow(runID, sn, pat)
	if err == nil {
		err = st.InsertRunPAT(ctx, row)
	}
	if err != nil {
		// An unrecorded token is never handed out: nothing could revoke it
		// after a crash. Revoke it now, best-effort.
		_ = client.Revoke(ctx, sn.Organisation, access.AccessToken, pat.AuthorizationID)
		return adoPAT{}, fmt.Errorf("record the run's personal access token: %w", err)
	}
	if why, ended := s.adoSignInEnds.since(sn.OwnerSubject, ends); ended {
		s.revokeRunPATCreatedAcrossEnd(ctx, st, client, access.AccessToken, row, why)
		return denied(fmt.Errorf("%w: the sign-in ended while this token was created", ErrADOEntraNotCaptured))
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", adoPATAuditMint,
		pat.AuthorizationID, "success", mustJSON(map[string]any{
			"reason": reason, "authorization_id": pat.AuthorizationID, "scope": pat.Scope,
			"valid_to": pat.ValidTo.UTC().Format(time.RFC3339), "owner": sn.OwnerSubject,
			"provider_row": sn.ProviderRowID, "organisation": sn.Organisation,
		})))
	return pat, nil
}

// adoRunPATRefusal maps a failed mint onto its answer. A transient status
// (adoPATTransientStatus) is a 503: the proxy keeps its last-good header and
// asks again.
func adoRunPATRefusal(err error) (status int, reason, body string) {
	var pe *adoPATError
	switch {
	case errors.Is(err, errADOPATUnavailable):
		return http.StatusForbidden, reasonADOPATUnavailable, adoRunPATUnavailable
	case errors.Is(err, ErrADOMintNeedsSecret):
		return http.StatusForbidden, ReasonADOPATNeedsConsoleApp, adoPATNeedsConsoleAppRefusal
	case errors.Is(err, errADOPATRunPaused):
		return http.StatusConflict, reasonADOPATRunInactive, adoRunPATPaused
	case errors.Is(err, errADOPATRunEnded):
		return http.StatusConflict, reasonADOPATRunInactive, adoRunPATEnded
	case errors.Is(err, errADOPATRunUnread):
		return http.StatusServiceUnavailable, reasonRunUnreadable, adoRunPATRunUnread
	case errors.As(err, &pe):
		status = http.StatusForbidden
		if adoPATTransientStatus(pe.Status) {
			status = http.StatusServiceUnavailable
		}
		switch reason = pe.Reason(); reason {
		case reasonADOPATPolicyBlocked:
			return status, reason, adoRunPATPolicyBlocked
		case reasonADOPATLifespanPolicy:
			return status, reason, adoRunPATLifespan
		case reasonADOPATConsentNeeded:
			return status, reason, adoRunPATConsentNeeded
		}
		return status, reason, adoRunPATRefused
	}
	class := ADOEntraClassify(err)
	if class == ADOEntraFailureUnavailable {
		return http.StatusServiceUnavailable, string(class), adoRunPATUnreachable
	}
	status, body = adoResolveFailureAnswer(class)
	if class == ADOEntraFailureNotCaptured {
		body = adoRunPATNotConnected
	}
	return status, string(class), body
}

// dispatchRunPAT creates the run's first token before any grant is written, so
// a person who has not connected is refused at launch. false means the run has
// been marked FAILED.
func (s *Server) dispatchRunPAT(ctx context.Context, run types.AgentRun, ado adoEntraRun) bool {
	sn := ado.snapshot()
	cfg, status, reason, body := s.adoEntraConfigFor(ctx, sn)
	if status != 0 {
		return s.refuseADOEntraDispatch(ctx, run, reason, body)
	}
	e, unlock := s.adoRunPATs.lock(run.ID)
	pat, err := s.mintRunPAT(ctx, run.ID, sn, cfg, ado.caps,
		adoRunPATValidTo(s.cfg.Now(), ado.patHours, run.EndsAt), adoPATMintDispatch)
	if err == nil {
		e.cur, e.caps = pat, slices.Clone(ado.caps)
	}
	// Unlocked before refusing: the refusal's revoke cascade takes this entry.
	unlock()
	if err != nil {
		_, reason, body := adoRunPATRefusal(err)
		return s.refuseADOEntraDispatch(ctx, run, reason, body)
	}
	return true
}

// resolveADORunPAT is resolveADOInjection's arm for a `minted_pat` run, reached
// once every snapshot check and the capability arm have passed. It always
// writes the answer.
func (s *Server) resolveADORunPAT(w http.ResponseWriter, r *http.Request, claims *identity.Claims,
	minted broker.Minted, grantID uuid.UUID, sn adoEntraScopeSnapshot, cfg ADOEntraConfig, sc types.SiteConfig,
	capAsk adoCapabilityGrant, responseCaps []adoscope.Capability, fail adoFail,
) bool {
	want := slices.Clone(responseCaps)
	if capAsk.capability != "" && !slices.Contains(want, capAsk.capability) {
		want = append(want, capAsk.capability)
	}
	pat, err := s.runPATFor(r.Context(), claims.RunID, sn, cfg, sc, want, r.URL.Query().Get("stale_jti"))
	if err != nil {
		if s.answerADOSignInEnded(w, r, claims, sn, ADOEntraClassify(err), fail) {
			return true
		}
		status, reason, body := adoRunPATRefusal(err)
		return fail(status, reason, body, map[string]any{"owner": sn.OwnerSubject})
	}
	if capAsk.once != nil && s.spendADOOnce(w, r, claims, sn, grantID, minted.JTI, capAsk, fail) {
		return true
	}
	data := map[string]any{
		"purpose": "proxy-injection-ado", "grant_id": grantID, "jti": minted.JTI,
		"owner": sn.OwnerSubject, "provider_row": sn.ProviderRowID,
		"organisation": sn.Organisation, "capabilities": responseCaps,
		"authorization_id": pat.AuthorizationID, "pat_scope": pat.Scope,
	}
	if capAsk.capability != "" {
		data["capability"] = capAsk.capability
		if capAsk.once != nil {
			data["approval_id"] = capAsk.once.ID
		}
	}
	s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"secret.read", types.ADOEntraAccessTokenSecret, "success", mustJSON(data)))
	// JTI is the token's own authorization id: the proxy names it back as
	// stale_jti when Azure DevOps refuses the header it holds.
	writeJSON(w, http.StatusOK, injectionResponse{
		Host: minted.Injection.Host, Header: adoEntraInjectHeader, Value: adoRunPATValue(pat.Token),
		JTI: pat.AuthorizationID, ExpiresAt: pat.ValidTo.UnixMilli(),
		Organisation: sn.Organisation, Capabilities: adoCapabilityStrings(responseCaps),
	})
	return true
}

// runPATFor returns the token a resolve hands out, creating one when the run
// has none, when the cached one does not carry want, when it is inside
// adoRunPATRenewWindow of its validTo, or when staleJTI names it (at most once
// per adoRunPATForcedEvery). Only the last revokes the token it replaces, since
// Azure DevOps has already refused it.
func (s *Server) runPATFor(ctx context.Context, runID uuid.UUID, sn adoEntraScopeSnapshot, cfg ADOEntraConfig,
	sc types.SiteConfig, want []adoscope.Capability, staleJTI string,
) (adoPAT, error) {
	e, unlock := s.adoRunPATs.lock(runID)
	defer unlock()
	run, err := s.cfg.Store.GetRun(ctx, runID)
	switch {
	case err != nil:
		return adoPAT{}, fmt.Errorf("%w: %w", errADOPATRunUnread, err)
	case run.State.IsTerminal():
		return adoPAT{}, errADOPATRunEnded
	case run.PausedAt != nil:
		return adoPAT{}, errADOPATRunPaused
	}
	row, _ := gitProviderRowByID(sc, sn.ProviderRowID) // driftFrom proved it live with an Entra block
	now := s.cfg.Now()
	validTo := adoRunPATValidTo(now, row.Entra.PATHours(), run.EndsAt)
	stale := e.cur
	var reason string
	switch {
	case e.cur.Token == "":
		reason = adoPATMintRestart
		if e.paused {
			reason = adoPATMintResume
		}
		want = s.adoRunPATStanding(ctx, runID, sn, row.Entra.CapabilityCeiling, want)
	case staleJTI != "" && staleJTI == e.cur.AuthorizationID && now.Sub(e.forcedAt) >= adoRunPATForcedEvery:
		reason = adoPATMintUpstream401
		e.forcedAt = now
	case !e.covers(want):
		reason = adoPATMintWiden
	case e.cur.ValidTo.Sub(now) < adoRunPATRenewWindow && validTo.After(e.cur.ValidTo):
		reason = adoPATMintRenewal
	default:
		return e.cur, nil
	}
	// A new token never drops access the run already holds, only what the
	// live ceiling no longer allows.
	var caps []adoscope.Capability
	for _, c := range append(slices.Clone(e.caps), want...) {
		if slices.Contains(row.Entra.CapabilityCeiling, c) && !slices.Contains(caps, c) {
			caps = append(caps, c)
		}
	}
	pat, err := s.mintRunPAT(ctx, runID, sn, cfg, caps, validTo, reason)
	if err != nil {
		if reason == adoPATMintRenewal && e.cur.ValidTo.After(now) {
			// The current token still works until its validTo; the next resolve
			// tries again. Past it, the failure is the answer.
			return e.cur, nil
		}
		return adoPAT{}, err
	}
	e.cur, e.caps, e.paused = pat, caps, false
	if reason == adoPATMintUpstream401 {
		if st, ok := s.cfg.Store.(store.RunPATStore); ok {
			if old, rerr := runPATRow(runID, sn, stale); rerr == nil {
				s.revokeRunPAT(ctx, st, old, adoPATRevokeUpstream401)
			}
		}
	}
	return pat, nil
}

// adoRunPATStanding is want plus every capability approved for the whole run,
// for a token created with nothing cached to carry them forward (a restart or
// a resume). An unreadable approval list adds nothing.
func (s *Server) adoRunPATStanding(ctx context.Context, runID uuid.UUID, sn adoEntraScopeSnapshot,
	ceiling, want []adoscope.Capability,
) []adoscope.Capability {
	if _, ok := s.cfg.Store.(runApprovalLister); !ok && s.cfg.Approvals == nil {
		return want
	}
	rows, err := s.runApprovals(ctx, runID, "")
	if err != nil {
		return want
	}
	out := slices.Clone(want)
	for _, c := range adoStanding(sn, ceiling, rows) {
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}
