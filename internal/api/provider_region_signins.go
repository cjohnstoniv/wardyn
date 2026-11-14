// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// What an AWS sign-in request ended by a write to its provider records, on the row and on its
// approval.expire audit row: the provider now signs in elsewhere (another region, or a pin to
// another account or role), or it is gone (deleted, or changed to another kind, which mints a
// new UID).
const (
	providerSignInChangedReason = "provider_sign_in_changed"
	providerRemovedReason       = "provider_removed"
)

// signInEndReason is why no sign-in can answer a request raised for the bedrock_sso provider
// held.ProviderUID by a run that holds a session for held's region, account and role, under the
// providers mp: "" while one still can. It is the session resolve's own refusal
// (providerSSOScopeAt): a run whose provider is gone, now names another region, or pins another
// account or role is refused at its next session resolve whatever is signed in. A pin removed
// is no refusal: the resolve then takes any account.
func signInEndReason(mp *types.ModelProviders, held awsSSOScopeSnapshot) string {
	if mp != nil {
		for _, p := range mp.Providers {
			if p.UID != held.ProviderUID {
				continue
			}
			b := providerBedrockSettings(p)
			if b.Region != held.Region || (b.SSOAccountID != "" && (b.SSOAccountID != held.SSOAccountID || b.SSORoleName != held.SSORoleName)) {
				return providerSignInChangedReason
			}
			return ""
		}
	}
	return providerRemovedReason
}

// endUnanswerableSignIns ends, as expired, every pending AWS sign-in request raised for a
// bedrock_sso provider whose region, pinned account or pinned role this save changed, or that it
// deleted or changed the kind of, when no sign-in can answer it any more: signInEndReason, held
// as the request's run holds the provider (its grant), so a run already signed in for the account
// and role pinned now keeps its request. A run whose grant cannot be read is held as the
// provider was before the save, since a dispatch takes no session for another region or pin
// (providerBedrockRefusal). Both write doors call it after the save and before their lock is
// released, so a request raised between the purge and the save is ended too; one raised after
// this list is ended by the resolve that raised it (raisedSignInEndReason). A request already answered is left
// as it is. Best-effort: the save stands, and a request it could not end ends with its run or
// ages out; a run paused on one is resumed by the pause sweep, since resuming takes the run's
// lock, which this lock must not precede.
func (s *Server) endUnanswerableSignIns(ctx context.Context, before, after *types.ModelProviders) {
	if before == nil || s.cfg.Approvals == nil {
		return
	}
	ended := map[string]string{}
	for _, old := range before.Providers {
		// A provider stored before UIDs has none: "" is also what a CLI or roster request names.
		if old.Kind != types.ModelProviderBedrockSSO || old.UID == "" {
			continue
		}
		b := providerBedrockSettings(old)
		held := awsSSOScopeSnapshot{ProviderUID: old.UID, Region: b.Region, SSOAccountID: b.SSOAccountID, SSORoleName: b.SSORoleName}
		if why := signInEndReason(after, held); why != "" {
			ended[old.UID] = why
		}
	}
	if len(ended) == 0 {
		return
	}
	rows, err := s.pendingReauths(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not list the AWS sign-in requests a provider write ends", slog.Any("err", err))
		return
	}
	for _, ap := range rows {
		var sc struct {
			ProviderUID string `json:"provider_uid"`
			Lane        string `json:"lane"`
		}
		if ap.Kind != types.ApprovalCredentialReauth || ap.State != types.ApprovalPending ||
			json.Unmarshal(ap.RequestedScope, &sc) != nil || sc.Lane != "" || ended[sc.ProviderUID] == "" {
			continue
		}
		why := ended[sc.ProviderUID]
		if held, ok := s.runSignInGrant(ctx, ap.RunID, sc.ProviderUID); ok {
			why = signInEndReason(after, held)
		}
		if why != "" {
			s.endSignInRequest(ctx, ap.ID, why)
		}
	}
}

// runSignInGrant is the dispatch-time snapshot of the run's AWS sign-in grant for the provider
// uid, the account, role and region its session resolve compares (providerSSOScopeAt); false
// when the run's grants cannot be read or none is for that provider.
func (s *Server) runSignInGrant(ctx context.Context, runID uuid.UUID, uid string) (awsSSOScopeSnapshot, bool) {
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, runID)
	if err != nil {
		return awsSSOScopeSnapshot{}, false
	}
	for _, g := range grants {
		var sc struct {
			Snapshot awsSSOScopeSnapshot `json:"snapshot"`
		}
		if json.Unmarshal(g.Spec.Scope, &sc) == nil && sc.Snapshot.Mechanism == string(types.ModelProviderBedrockSSO) && sc.Snapshot.ProviderUID == uid {
			return sc.Snapshot, true
		}
	}
	return awsSSOScopeSnapshot{}, false
}

// raisedSignInEndReason is signInEndReason for the request a resolve has just raised, on the
// providers as stored now. A provider write that landed after the resolve read its provider
// listed the pending requests before this one existed; raised first and read second, either that
// list saw the request or this read sees the write. "" for a CLI or roster request, and when the
// read fails: the request stays open, as before.
func (s *Server) raisedSignInEndReason(ctx context.Context, sn awsSSOScopeSnapshot) string {
	if sn.ProviderUID == "" || sn.ComponentID != "" {
		return ""
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return ""
	}
	return signInEndReason(sc.ModelProviders, sn)
}

// endSignInRequest ends one pending AWS sign-in request as expired, the server's own act, and
// counts the outcome only when this call moved the row: a request a save and a resolve both end,
// or one a sign-in or the sweep decided first, is counted once, where it moved. Best-effort: a
// request it could not end ends with its run or ages out.
func (s *Server) endSignInRequest(ctx context.Context, id uuid.UUID, why string) {
	err := s.cfg.Approvals.ExpireOne(ctx, id, "wardynd", why)
	switch {
	case errors.Is(err, types.ErrApprovalAlreadyDecided):
		// Another end, a sign-in or the sweep moved it first, and counted its own outcome.
	case err != nil:
		slog.WarnContext(ctx, "wardynd: could not end an AWS sign-in request no sign-in can answer",
			slog.String("approval_id", id.String()), slog.String("reason", why), slog.Any("err", err))
	default:
		s.metrics.credentialReauthRecorded(credentialReauthOutcomeExpired)
	}
}
