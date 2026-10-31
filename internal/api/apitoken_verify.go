// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The wdn_ bearer resolver: the checks every surface that accepts an API token runs, apart from the
// identity it then publishes. The API (apiTokenAuth) takes every kind but inbound; the app gateway takes an
// inbound token only.
package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// tokenVerdict is verifyAPIToken's answer.
type tokenVerdict int

const (
	// tokenVerified: the bearer is a live credential of the surface asked for.
	tokenVerified tokenVerdict = iota
	// tokenRefused: not a credential here. Nothing is written; the caller answers its one 401 with the
	// request verifyAPIToken returned, which carries the auth.fail reason when a check named one.
	tokenRefused
	// tokenAnswered: the response is written (a 503 lookup failure, or a refusal that records its own
	// auth.fail row); the caller stops.
	tokenAnswered
)

// verifiedAPIToken is a bearer verifyAPIToken accepted: its row, and the live record of the service
// identity it belongs to when it is a service's (nil for a person's).
type verifiedAPIToken struct {
	types.APIToken
	service *types.ServiceIdentity
}

// verifyAPIToken is the one check a wdn_ bearer passes on every surface that accepts one: the lookup, the
// kind the surface takes, then the owner's state — for a service identity its live record and sponsor
// (verifyServiceToken), for a person the reserved-principal refusal, the revocation cutoff and the stale
// role stamp — and the last-used touch. It publishes no identity: the caller does, as the surface needs.
//
// inbound names the surface. The API passes false and refuses an inbound token before any other check, so
// one can never act on a console route; the app gateway passes true and takes nothing else, so no API, CLI,
// admin or portal credential ever calls a port. A bearer without apiTokenPrefix is refused on both.
func (s *Server) verifyAPIToken(w http.ResponseWriter, r *http.Request, raw string, inbound bool) (verifiedAPIToken, *http.Request, tokenVerdict) {
	if !strings.HasPrefix(raw, apiTokenPrefix) || s.cfg.Store == nil {
		return verifiedAPIToken{}, r, tokenRefused
	}
	t, err := s.cfg.Store.GetAPITokenByRaw(r.Context(), raw)
	if errors.Is(err, store.ErrNotFound) {
		return verifiedAPIToken{}, r, tokenRefused
	}
	if err != nil {
		// Visible server-side, because this 500s an entire authentication
		// lane — every CI job and script holding a wdn_ token — and the
		// only other evidence is a client complaint. writeError does not
		// log, no audit row is written (there is no authenticated principal
		// to attribute one to), and wardyn_store_up keeps scraping 1: that
		// gauge answers a PING, which a pool passes while one table denies
		// a read or one statement times out. A health signal that stays
		// green through the outage it appears to cover argues AGAINST the
		// operator's own evidence, so the honest fix is to say what the
		// gauge asserts and give this lane its own series.
		//
		// ERROR level by internal/audit/sink.go's own rule: a lost event is
		// an ERROR-level fact an operator can alert on.
		slog.ErrorContext(r.Context(), "api: api-token lookup failed; this request could not be authenticated",
			"error", err, "path", r.URL.Path)
		s.metrics.authStoreErrorInc()
		writeErrorReason(w, http.StatusServiceUnavailable, reasonTokenLookupUnavailable, "api token lookup failed")
		return verifiedAPIToken{}, r, tokenAnswered
	}
	// The surface check comes first, before any check that could answer or audit: a token of the other
	// surface is exactly an unknown one here.
	if (t.Kind == types.APITokenKindInbound) != inbound {
		return verifiedAPIToken{}, r, tokenRefused
	}
	v := verifiedAPIToken{APIToken: t}
	if isServicePrincipal(t.Principal) {
		// A service identity's token is checked against its live record and its sponsor, not a sign-in stamp.
		var verdict tokenVerdict
		if v.service, r, verdict = s.verifyServiceToken(w, r, t); verdict != tokenVerified {
			return verifiedAPIToken{}, r, verdict
		}
	} else {
		// A row minted for a reserved principal (before the sign-in callback
		// refused one) would replay it as that identity (#1162). Audited and
		// answered here: the fallback would only record invalid_admin_token.
		if s.refuseReservedPrincipal(w, r, t.Principal, apiTokenReservedRefusal) {
			return verifiedAPIToken{}, r, tokenAnswered
		}
		// The same cutoff the session lane obeys, applied to the token's
		// created_at. Without it POST /sessions/revoke was a race the sweep
		// could lose FOREVER: revokeAPITokensFor takes a ListAPITokens snapshot,
		// and a mint whose INSERT commits after that snapshot is never reachable
		// by that revoke again — a token minted without an expiry never ages out, so the escaped row is
		// a permanent credential. Closing it on the READ side rather than by
		// locking the writer also removes the sweep's dependence on winning the
		// race at all: the sweep still runs (it is what makes GET /api/v1/tokens
		// show the row revoked), but a row it missed no longer authenticates.
		//
		// A token created at or before the cutoff is not a credential; one
		// minted AFTER it is, which is correct — that is a new credential the
		// principal minted from a session that itself cleared the cutoff.
		//
		// Refused rather than answered here, for the reason the not-found
		// arm above is: unknown, revoked and cut-off collapse into one 401
		// from one place, and no branch here becomes an existence oracle.
		//
		// A store FAILURE fails closed with the same 503 the lookup failure
		// gives, and for the same reason — an unanswerable revocation check must
		// never read as "not revoked". 503 rather than 500: the token may
		// be perfectly good, this deployment simply cannot decide, and the SSO
		// lane now answers the identical condition the identical way.
		if s.cfg.SessionRevocations != nil {
			// The same read asks whether the owner's identity is deactivated (a token has no epoch of
			// its own: a suspension revokes the tokens it can see, and the mint guard refuses the rest).
			status, rerr := oidc.CheckSession(r.Context(), s.cfg.SessionRevocations, t.Principal, t.Email, oidc.Issue{At: t.CreatedAt, Seq: t.RevSeq}, -1)
			if rerr != nil {
				slog.ErrorContext(r.Context(), "api: session-revocation lookup failed; this api token could not be authenticated",
					"error", rerr, "path", r.URL.Path)
				s.metrics.authStoreErrorInc()
				writeErrorReason(w, http.StatusServiceUnavailable, reasonTokenLookupUnavailable, "api token lookup failed")
				return verifiedAPIToken{}, r, tokenAnswered
			}
			if status != oidc.SessionLive {
				if status == oidc.SessionDeactivated {
					r = r.WithContext(oidc.WithSessionRejected(r.Context(), authFailedIdentityDeactivated))
				}
				return verifiedAPIToken{}, r, tokenRefused
			}
		}
		if s.refuseStaleRoleStamp(w, r, t) {
			return verifiedAPIToken{}, r, tokenAnswered
		}
	}
	// Best effort by contract (see Store.TouchAPIToken): a failed touch must
	// never fail an otherwise-valid request. "Last used" is an operator
	// hygiene signal — which tokens are dead and can be revoked — not an
	// authorization input, so nothing downstream reads it.
	_ = s.cfg.Store.TouchAPIToken(r.Context(), t.ID, s.cfg.Now().UTC())
	if t.Principal == "" {
		// A person-less token row would read as the admin-token lane
		// (no human on the context): refuse it rather than treat it as the operator.
		return verifiedAPIToken{}, r, tokenRefused
	}
	return v, r, tokenVerified
}
