// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Per-user API tokens (migration 0045): the THIRD auth branch of the public
// API, plus the self-service and admin CRUD around it.
//
// The problem it solves: before this, the only non-interactive credential was
// WARDYN_ADMIN_TOKEN — one shared string, deployment-wide admin, attributable to
// nobody. Every script and CI job that touched the API shared it, and the audit
// log recorded "admin-token" for all of them. An api token is the opposite: it
// belongs to ONE human, carries THEIR role, and is revocable on its own.
//
// The load-bearing property is that authenticating with one is indistinguishable
// downstream from authenticating with that human's SSO session. apiTokenAuth
// publishes the identity through withHumanIdentity — the same function the
// session branch calls — so ownership (AgentRun.CreatedBy), the admin gate
// (isOperator) and capability grants (capabilitySubjects) all bind to the owning
// human for free, with no per-feature token awareness anywhere. A token is NEVER
// the admin identity: minting one requires a verified human in the first place
// (handleCreateAPIToken), so there is no path by which the shared admin token
// becomes a per-user one.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// apiTokenPrefix marks a bearer as a per-user api token rather than the shared
// admin token. It is a routing hint, not a secret and not a security boundary —
// the 256 bits after it are — but it buys two real things: a leaked token is
// greppable in a log or a repo scan, and the auth branch can skip a store round
// trip for every admin-token request.
const apiTokenPrefix = "wdn_"

// apiTokenMaxPerPrincipal bounds how many LIVE tokens one human may hold — the
// same generous cap sshMaxKeysPerPrincipal applies to registered keys, against
// an accidental (or scripted) unbounded mint loop. Revoking one frees a slot.
const apiTokenMaxPerPrincipal = 20

// apiTokenNameMaxLen bounds the display name. A name is only ever echoed back to
// its owner and to an admin inventory screen, so this is length hygiene, not a
// parser.
const apiTokenNameMaxLen = 200

// apiTokenIDCtxKey carries the id of the api token that authenticated the
// current request, when one did. It is NOT an identity key — withHumanIdentity
// already published who the caller is, deliberately indistinguishably from a
// session — it answers the narrower question "was this request made with a
// token", which exactly one caller needs: handleCreateAPIToken, to refuse a
// token minting another token. Without that refusal, revoking a leaked token
// would not end the compromise, because the leaked token could already have
// minted a successor with the same powers and a different id.
type apiTokenIDCtxKey struct{}

func withAPITokenID(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, apiTokenIDCtxKey{}, id)
}

func apiTokenIDFromContext(ctx context.Context) uuid.UUID {
	id, _ := ctx.Value(apiTokenIDCtxKey{}).(uuid.UUID)
	return id
}

// apiTokenAuth is the third auth branch, mounted by humanOrAdminAuth in front of
// the admin bearer compare. A bearer carrying apiTokenPrefix is resolved against
// api_tokens; anything else (and any request with no bearer at all) is handed
// straight to fallback, which is the pre-existing admin path.
//
// An UNRESOLVABLE prefixed token also falls through to fallback rather than
// answering 401 here, and that is deliberate on two counts. It keeps the branch
// from becoming an existence oracle (the store already collapses unknown,
// revoked and hash-mismatch into ErrNotFound; falling through collapses the
// response too — one 401 from one place). And it removes a footgun: an operator
// whose WARDYN_ADMIN_TOKEN happens to start with `wdn_` still authenticates,
// because the admin compare still gets to run.
//
// A store FAILURE is not a rejection and must not fall through — falling
// through would turn a database outage into "your token is invalid", and worse,
// into an admin-token compare the caller never asked for. It fails closed with a
// 500 that says the lookup failed, not that the credential did.
//
// This branch deliberately does NOT emit an auth.failed audit event; that
// vocabulary belongs to a separate lane.
func (s *Server) apiTokenAuth(next, fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := bearerToken(r)
		if !ok || !strings.HasPrefix(tok, apiTokenPrefix) || s.cfg.Store == nil {
			fallback.ServeHTTP(w, r)
			return
		}
		t, err := s.cfg.Store.GetAPITokenByRaw(r.Context(), tok)
		if errors.Is(err, store.ErrNotFound) {
			fallback.ServeHTTP(w, r)
			return
		}
		if err != nil {
			// VISIBLE SERVER-SIDE, because this 500s an entire authentication
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
			writeError(w, http.StatusInternalServerError, "api token lookup failed")
			return
		}
		// THE SAME CUTOFF THE SESSION LANE OBEYS, applied to the token's
		// created_at. Without it POST /sessions/revoke was a race the sweep
		// could lose FOREVER: revokeAPITokensFor takes a ListAPITokens snapshot,
		// and a mint whose INSERT commits after that snapshot is never reachable
		// by that revoke again — api_tokens has no expiry, so the escaped row is
		// a permanent credential. Closing it on the READ side rather than by
		// locking the writer also removes the sweep's dependence on winning the
		// race at all: the sweep still runs (it is what makes GET /api/v1/tokens
		// show the row revoked), but a row it missed no longer authenticates.
		//
		// A token created AT OR BEFORE the cutoff is not a credential; one
		// minted AFTER it is, which is correct — that is a new credential the
		// principal minted from a session that itself cleared the cutoff.
		//
		// FALLS THROUGH rather than 401ing here, for the reason the not-found
		// arm above does: unknown, revoked and cut-off collapse into one 401
		// from one place, and no branch here becomes an existence oracle.
		//
		// A store FAILURE fails closed with the same 500 the lookup failure
		// gives, and for the same reason — an unanswerable revocation check must
		// never read as "not revoked".
		if s.cfg.SessionRevocations != nil {
			revoked, rerr := s.cfg.SessionRevocations.IsSessionRevoked(r.Context(), t.Principal, t.Email, t.CreatedAt)
			if rerr != nil {
				slog.ErrorContext(r.Context(), "api: session-revocation lookup failed; this api token could not be authenticated",
					"error", rerr, "path", r.URL.Path)
				s.metrics.authStoreErrorInc()
				writeError(w, http.StatusInternalServerError, "api token lookup failed")
				return
			}
			if revoked {
				fallback.ServeHTTP(w, r)
				return
			}
		}
		// Best effort by contract (see Store.TouchAPIToken): a failed touch must
		// never fail an otherwise-valid request. "Last used" is an operator
		// hygiene signal — which tokens are dead and can be revoked — not an
		// authorization input, so nothing downstream reads it.
		_ = s.cfg.Store.TouchAPIToken(r.Context(), t.ID, s.cfg.Now().UTC())
		// The SAME five keys the session branch publishes, through the SAME
		// function, from the snapshot stamped when this token was minted. This is
		// what makes ownership, RBAC and capability grants bind to the owning
		// human with no token-specific code anywhere downstream.
		//
		// A NULL groups_truncated — a token minted before 0.7, when nothing
		// recorded the bit — reads as TRUNCATED (PF-26). Its snapshot's
		// completeness is genuinely unknown, and the fail-OPEN reading would let
		// a legacy token shed a group-assigned governance profile the cookie lane
		// already refuses to shed. The cost is one re-mint, and only on a
		// deployment that actually assigns profiles to groups — the resolver's
		// refusal is itself gated on a group-tier row existing at all.
		ctx := withHumanIdentity(r.Context(), t.Principal, t.Email, t.Role, t.Groups,
			t.GroupsTruncated == nil || *t.GroupsTruncated)
		next.ServeHTTP(w, r.WithContext(withAPITokenID(ctx, t.ID)))
	})
}

// createAPITokenRequest is the POST /api/v1/me/tokens body.
type createAPITokenRequest struct {
	Name string `json:"name"`
}

// handleCreateAPIToken is POST /api/v1/me/tokens: mint a token for the caller's
// OWN identity and return the plaintext exactly once.
//
// Two refusals guard the identity invariant, and both are 403 rather than a
// validation error because both are about WHO is asking, not what they sent:
//
//  1. No verified human on the context. That is the admin token, or local mode
//     — neither is a person, so there is no identity for a "per-user" token to
//     carry. Minting one anyway would produce a second admin-tier credential
//     attributable to nobody, which is the exact problem this feature exists to
//     remove.
//  2. The caller is itself a token. A token minting a successor token would make
//     revocation meaningless: pull the leaked credential and its child, minted
//     minutes after the leak, still works under a different id.
func (s *Server) handleCreateAPIToken(w http.ResponseWriter, r *http.Request) {
	var req createAPITokenRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	ctx := r.Context()
	sub := oidcHumanFromContext(ctx)
	if sub == "" {
		writeError(w, http.StatusForbidden,
			"an API token belongs to a signed-in human — sign in to the console and create one from Account, or keep using the admin token directly")
		return
	}
	if apiTokenIDFromContext(ctx) != uuid.Nil {
		writeError(w, http.StatusForbidden,
			"an API token cannot create another API token — sign in to the console to mint one")
		return
	}
	name := strings.TrimSpace(req.Name)
	if len(name) > apiTokenNameMaxLen || !controlCharFree(name) {
		writeError(w, http.StatusUnprocessableEntity, "name: invalid")
		return
	}

	existing, err := s.cfg.Store.ListAPITokensByPrincipal(ctx, sub)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list api tokens: "+err.Error())
		return
	}
	live := 0
	for _, t := range existing {
		if t.RevokedAt == nil {
			live++
		}
	}
	if live >= apiTokenMaxPerPrincipal {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("too many live API tokens (max %d) — revoke one first", apiTokenMaxPerPrincipal))
		return
	}

	raw := make([]byte, 32)
	// crypto/rand.Read never returns an error (go1.24+): it crashes the program
	// irrecoverably instead, so there is no failure path to serve a 500 on.
	rand.Read(raw)
	plaintext := apiTokenPrefix + hex.EncodeToString(raw)

	// The identity SNAPSHOT. Role is the caller's REAL session role, copied
	// verbatim — NOT a two-valued isOperator re-derivation (PF-19). This token
	// replays the human's WHOLE session identity through withHumanIdentity
	// (see the middleware above), not merely their foreign-run reach, so
	// collapsing a security_admin session to "member" here would make every
	// security-governance route unreachable by API or CLI for exactly the
	// persona whose surfaces ship API-first.
	//
	// SAFE, and not by accident: the three consumers that turn a stamped role
	// into REACH INTO SOMEONE ELSE'S RUN all require == oidc.RoleAdmin
	// (attach.go's ticket lane, uigateway.go, sshgateway.go), so a
	// security_admin stamp grants precisely zero run reach. The SSH-key and
	// attach-ticket stamps keep their never-stamp-anything-but-admin/member
	// derivation for the mirror-image reason — there, the role field means
	// foreign-run reach and nothing else.
	//
	// Fallback for a caller with no OIDC role on ctx: unreachable here (the
	// no-verified-human refusal above returned already), but RoleMember is the
	// fail-closed value if that ever changes.
	role := oidcRoleFromContext(ctx)
	if role == "" {
		role = oidc.RoleMember
	}
	// The group snapshot's PF-26 completeness marker, stamped from the MINTING
	// session's own bit (migration 0052's api_tokens.groups_truncated). The
	// token replays this snapshot into the very same capabilitySubjects the
	// ceiling resolver reads (apiTokenAuth above), so without the marker the
	// token lane re-imports exactly the tier evaporation the cookie codec bump
	// closed: a member in enough groups mints a token, the walling group is
	// missing from the frozen snapshot, and every API/CLI call quietly resolves
	// to the deployment ceiling.
	//
	// Addressed, because the column is three-valued: this row must be able to
	// say "known complete" (false) distinctly from a pre-0.7 row's "nobody
	// recorded it" (NULL). See types.APIToken.GroupsTruncated.
	groupsTruncated := oidcGroupsTruncatedFromContext(ctx)
	created, err := s.cfg.Store.CreateAPIToken(ctx, types.APIToken{
		ID:              uuid.New(),
		Principal:       sub,
		Email:           oidcEmailFromContext(ctx),
		Role:            role,
		Groups:          oidcGroupsFromContext(ctx),
		GroupsTruncated: &groupsTruncated,
		Name:            name,
		CreatedAt:       s.cfg.Now().UTC(),
	}, plaintext)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create api token: "+err.Error())
		return
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"token.create", created.ID.String(), "success",
		mustJSON(map[string]any{"name": created.Name, "role": created.Role})))

	// The ONLY response that carries the plaintext. It is not stored, so this
	// body is the sole opportunity to read it; a lost token is re-minted, never
	// recovered.
	created.Token = plaintext
	writeJSON(w, http.StatusCreated, created)
}

// handleListAPITokens is GET /api/v1/me/tokens: the caller's own tokens,
// revoked ones included (a human has to be able to SEE that the credential they
// retired is retired). No row anywhere in this response carries the plaintext or
// the hash.
func (s *Server) handleListAPITokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.cfg.Store.ListAPITokensByPrincipal(r.Context(), principalFromRequest(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list api tokens: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

// handleListAllAPITokens is GET /api/v1/tokens: every token in the deployment.
// securityOps (routes.go) — admin OR security_admin. Token inventory is
// authority over the verdict, not reach into a run, which is the line that puts
// a route on that tier; it names other humans, so it is not a member surface.
func (s *Server) handleListAllAPITokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.cfg.Store.ListAPITokens(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list api tokens: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

// handleRevokeAPIToken is DELETE /api/v1/me/tokens/{id}: revoke one of the
// caller's OWN tokens. Scoped to their principal at the store, so another
// human's id answers the byte-identical 404 a nonexistent id does — no existence
// leak across principals.
func (s *Server) handleRevokeAPIToken(w http.ResponseWriter, r *http.Request) {
	s.revokeAPIToken(w, r, principalFromRequest(r))
}

// handleAdminRevokeAPIToken is DELETE /api/v1/tokens/{id}: revoke ANYONE's
// token. securityOps (routes.go) — admin OR security_admin, its twin above.
// A revocation only ever SUBTRACTS reach, which is why it sits on the security
// tier rather than the admin one, and it reaches a SUPER admin's tokens too
// (see handleRevokeSessions for that argument in full). This is the remediation
// path for the stamp
// ceiling migration 0045 documents — a demoted admin's outstanding tokens keep
// the role they were minted under until their owner's next login re-stamps it
// (store.RefreshAPITokenRoles, fired from the same OnLogin hook that has
// refreshed SSH keys since 0.6) or until they are revoked here. This route is
// the path that takes effect IMMEDIATELY, and the only one that helps for an
// owner who never signs in again — and for a token
// whose owner has left.
func (s *Server) handleAdminRevokeAPIToken(w http.ResponseWriter, r *http.Request) {
	s.revokeAPIToken(w, r, "")
}

// revokeAPIToken is the shared body of both revoke routes. An EMPTY principal is
// the admin lane (any token); a non-empty one scopes the update to that human's
// own rows. Already-revoked is ErrNotFound at the store, so a second call is a
// 404 and emits no second audit row for an act that did not happen.
func (s *Server) revokeAPIToken(w http.ResponseWriter, r *http.Request, principal string) {
	id, ok := parseIDParam(w, r, "id", "api token")
	if !ok {
		return
	}
	revoked, err := s.cfg.Store.RevokeAPIToken(r.Context(), id, principal, s.cfg.Now().UTC())
	if notFoundIf(w, err, "api token") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "revoke api token: "+err.Error())
		return
	}
	// principal names the token's OWNER, which is the whole point of the row on
	// the admin lane: the actor is the admin, the subject is someone else.
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"token.revoke", revoked.ID.String(), "success",
		mustJSON(map[string]any{"principal": revoked.Principal, "name": revoked.Name})))
	w.WriteHeader(http.StatusNoContent)
}
