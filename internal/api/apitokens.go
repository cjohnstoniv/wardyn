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
	"slices"
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
//
// apiTokenNoHumanRefusal is the ONE sentence this handler gives a caller with
// no live signed-in human behind the request — no verified human on the
// context at all, or a session the revocation lever cut off while the mint was
// in flight. Shared verbatim so the two arms cannot drift into an oracle that
// distinguishes "you were never signed in" from "your session was just
// revoked".
const apiTokenNoHumanRefusal = "an API token belongs to a signed-in human — sign in to the console and create one from Account, or keep using the admin token directly"

func (s *Server) handleCreateAPIToken(w http.ResponseWriter, r *http.Request) {
	// THE CREDENTIAL'S AUTHORITY TIME, stamped HERE — before a single byte of
	// the request body is read.
	//
	// created_at is not a display field: apiTokenAuth compares it against the
	// POST /sessions/revoke cutoff, so it means "the moment from which this
	// credential's authority runs". The authority was established upstream, by
	// the session gate that admitted this request (oidc.Middleware's
	// IsSessionRevoked). Stamping the row with s.cfg.Now() at INSERT time
	// instead put the field minutes or hours after the moment it stands for,
	// and the API server sets no ReadTimeout by design: a caller who holds the
	// mint request's body open across the lever got created_at AFTER the
	// cutoff, escaped the sweep's ListAPITokens snapshot exactly as before, and
	// passed the read-side check too — a permanent wdn_ credential surviving
	// the 204 the incident lever answered with.
	//
	// The whole client-controlled window lives between request admission and
	// this handler's store write; taking the timestamp at the top removes it,
	// because everything an attacker can stretch happens after this line.
	authorizedAt := s.cfg.Now().UTC()
	var req createAPITokenRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	ctx := r.Context()
	sub := oidcHumanFromContext(ctx)
	if sub == "" {
		writeError(w, http.StatusForbidden, apiTokenNoHumanRefusal)
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
	// THE LEVER MAY HAVE FIRED WHILE THIS REQUEST WAS IN FLIGHT. The gate that
	// admitted it ran in oidc.Middleware before the handler was entered, and
	// everything since — the body read, the quota list — is time an attacker
	// can stretch. Ask the same question the gate asked, as late as possible
	// and about the same instant the row will carry: is a session admitted at
	// authorizedAt now cut off?
	//
	// This does not replace the read-side check in apiTokenAuth above; the two
	// close different halves. A cutoff committed BEFORE this line refuses the
	// mint outright, so no dead credential is minted and no success audit row
	// claims one was. A cutoff committed AFTER it postdates authorizedAt, so
	// the row is born at-or-before the cutoff and never authenticates.
	//
	// The refusal is BYTE-IDENTICAL to the no-verified-human arm above: a
	// caller whose session the lever just killed is exactly a caller with no
	// signed-in human behind them, and answering differently would make this
	// handler an oracle for "that revoke has landed".
	//
	// An unanswerable check fails closed with the same 500 the store errors
	// below give — never as "not revoked".
	if s.cfg.SessionRevocations != nil {
		revoked, rerr := s.cfg.SessionRevocations.IsSessionRevoked(ctx, sub, oidcEmailFromContext(ctx), authorizedAt)
		if rerr != nil {
			writeError(w, http.StatusInternalServerError, "create api token: "+rerr.Error())
			return
		}
		if revoked {
			writeError(w, http.StatusForbidden, apiTokenNoHumanRefusal)
			return
		}
	}
	created, err := s.cfg.Store.CreateAPIToken(ctx, types.APIToken{
		ID:              uuid.New(),
		Principal:       sub,
		Email:           oidcEmailFromContext(ctx),
		Role:            role,
		Groups:          oidcGroupsFromContext(ctx),
		GroupsTruncated: &groupsTruncated,
		Name:            name,
		CreatedAt:       authorizedAt,
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

// staleRoleSnapshotCount reports how many UNREVOKED wdn_ tokens still carry a
// frozen role snapshot that a role-mapping edit naming `value` cannot reach.
//
// WHY THIS EXISTS. An api_token's role is stamped at mint (handleCreateAPIToken)
// and read verbatim on every request (apiTokenAuth) — since 0.7 that stamp can
// be security_admin. The stamp is BOUNDED-STALE, NOT FROZEN, and the ceiling is
// the owner's own next login: store.RefreshAPITokenRoles re-stamps role on
// every unrevoked token that principal holds, fired from the same OnLogin hook
// that has refreshed SSH keys since 0.6 (the SSH lane's bound is a TTL instead
// — sshgateway.go's sshRoleFresh against WARDYN_SSH_ROLE_TTL). What the login
// hook does NOT refresh is the GROUP snapshot, and what it cannot bound at all
// is a human who is demoted and never signs in again. Both are why this count,
// and the revoke beside it, exist.
//
// WHAT IT IS NOT: it is not a TTL. It is the INFORMATIONAL half — how many
// mint-time snapshots name this value at all, elevated or not — and it revokes
// nothing itself. The acting half is revokeDemotedRoleSnapshots below, which
// the owner adjudication for F112 requires: a demotion must be effective
// IMMEDIATELY, not merely announced and not deferred to whenever the demoted
// human happens to sign in next. Telling the admin was this counter's whole
// remedy and it was the wrong one — the login refresh is a real bound, but it
// is the owner's schedule, not the operator's, and it never arrives for someone
// who has left.
//
// The self-DoS this used to fear is answered by SCOPE, not by inaction: the
// revoke fires only when the edit actually takes a tier away from the edited
// value, and then only for snapshots that lose one, so an ordinary member CI
// credential naming the same group is never touched. See
// revokeDemotedRoleSnapshots.
//
// THE MATCH IS THE SNAPSHOT'S OWN VOCABULARY. api_tokens carries the principal,
// the email and the login-time GROUP snapshot (migration 0045 + the 0.7 groups
// column), and a role-mapping value is a group/App-Role key or — where the org
// opted in — an email. Both are canonicalized lowercase at their write
// boundaries (canonicalRoleMapValue, oidc.CanonicalGroupSubject), so groups
// compare exactly and the email compares case-insensitively, exactly as
// IsSessionRevoked's email arm does.
//
// BEST EFFORT BY CONTRACT: a store failure returns the error for the caller to
// log, never to fail the write with — the mapping edit is already durable by the
// time this runs, and refusing it afterwards would be a lie about what happened.
func (s *Server) staleRoleSnapshotCount(ctx context.Context, value string) (int, error) {
	if s.cfg.Store == nil || value == "" {
		return 0, nil
	}
	toks, err := s.cfg.Store.ListAPITokens(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range toks {
		if t.RevokedAt != nil {
			continue
		}
		if t.Principal == value || (t.Email != "" && strings.EqualFold(t.Email, value)) || slices.Contains(t.Groups, value) {
			n++
		}
	}
	return n, nil
}

// apiTokenSnapshotAnswerable reports whether a token's login-time GROUP snapshot
// can be re-derived from at all. NULL groups_truncated reads as TRUNCATED, the
// same PF-26 rule apiTokenAuth applies: a pre-0.7 row's completeness was never
// recorded, so "no groups" and "the groups we kept" are indistinguishable.
func apiTokenSnapshotAnswerable(t types.APIToken) bool {
	return t.Groups != nil && t.GroupsTruncated != nil && !*t.GroupsTruncated
}

// roleSnapshotCtx is the minimal context the two tier predicates read: a
// verified human (so neither takes its shared-credential arm) carrying exactly
// this role.
func roleSnapshotCtx(role string) context.Context {
	return withOIDCRole(withOIDCHuman(context.Background(), "role-snapshot"), role)
}

// roleSnapshotDrops reports whether re-deriving a human's role as `derived`
// takes authority AWAY from a frozen snapshot that says `stamped`.
//
// THE TIERS ARE ASKED, NEVER RE-DERIVED. This builds the context each predicate
// reads and calls s.isOperator / s.isSecurityOperator themselves, because
// http.go states that a predicate there is the tier's ONLY definition and
// nothing in this package may re-derive one from a role comparison of its own.
//
// BOTH are asked because the tiers DO NOT NEST — RoleSecurityAdmin is beside
// RoleAdmin, not below it. security_admin ⇒ admin takes nothing away (a super
// admin is a security admin too); admin ⇒ security_admin takes away the run
// reach an admin stamp carries and IS a drop. A ladder would get exactly one of
// those two backwards.
func (s *Server) roleSnapshotDrops(stamped, derived string) bool {
	was, now := roleSnapshotCtx(stamped), roleSnapshotCtx(derived)
	return (s.isOperator(was) && !s.isOperator(now)) ||
		(s.isSecurityOperator(was) && !s.isSecurityOperator(now))
}

// revokeDemotedRoleSnapshots makes a role-mapping demotion EFFECTIVE: it
// revokes the outstanding wdn_ tokens of every principal whose derived role
// this edit takes a tier away from. Returns how many rows it revoked.
//
// WHY IT REVOKES AT ALL (owner adjudication, F112). An api_token's role is
// stamped at mint and read verbatim on every request until something re-stamps
// it, and the only thing that does is the owner's own next login
// (store.RefreshAPITokenRoles). So removing someone's admin through the People
// screen took effect on THEIR schedule — and never at all for someone who has
// left, which is the case a demotion is most often about. "Removing admin
// removes admin" is the contract; a bound that waits for the demoted human to
// come back is not it, and neither is a count in an audit row.
//
// TWO SCOPES KEEP THE BLAST RADIUS AT THE DEMOTION, which is what makes this
// safe where a blanket sweep would not be:
//
//  1. THE EDIT ITSELF must take a tier away. before/after are the real merged
//     derivations for the edited value (PreviewRoleAgainst over the same
//     candidate row set the posture-flip guard uses), so a promotion, a
//     no-op re-save, and a delete of a row the chart still grants revoke
//     nothing.
//  2. THIS EDIT must be what takes it. Each live token is re-derived from its
//     OWN login-time claims TWICE — against the pre-edit rows and the post-edit
//     rows — so a snapshot the edit does not move is left alone even if it is
//     stale for some other reason. An admin-stamped token naming a different
//     group is untouched.
//  3. THE STAMP must still carry what was lost. A member-stamped CI credential
//     that names the demoted group keeps working: member ⇒ member drops
//     nothing, so there is nothing stale to revoke.
//
// AN UNANSWERABLE SNAPSHOT FAILS CLOSED, and this is the count's old
// undercount corrected: a token whose group snapshot is nil or PARTIAL (PF-26)
// cannot be re-derived, so it cannot be shown to have kept its tier. Elevated
// stamps in that state are revoked; member stamps are not, since there is
// nothing for them to lose. The old slices.Contains(t.Groups, value) test
// reported 1 of 3 live admin-stamped tokens for exactly this reason.
//
// BEST EFFORT BY CONTRACT, like the counter: the mapping edit is already
// durable when this runs, so a store failure is logged at WARN and reported as
// zero — never turned into a 500 that would misdescribe what happened.
func (s *Server) revokeDemotedRoleSnapshots(ctx context.Context, value string, before, after []oidc.RoleMapping) int {
	if s.cfg.Store == nil || s.cfg.OIDC == nil || value == "" {
		return 0
	}
	was, _ := s.cfg.OIDC.PreviewRoleAgainst(before, nil, []string{value}, "")
	now, _ := s.cfg.OIDC.PreviewRoleAgainst(after, nil, []string{value}, "")
	if !s.roleSnapshotDrops(was, now) {
		return 0
	}
	toks, err := s.cfg.Store.ListAPITokens(ctx)
	if err != nil {
		slog.WarnContext(ctx, "api: could not list api tokens to revoke a demoted role snapshot; the demotion is NOT yet effective for outstanding tokens",
			"value", value, "error", err)
		return 0
	}
	var principals []string
	for _, t := range toks {
		if t.RevokedAt != nil {
			continue
		}
		if apiTokenSnapshotAnswerable(t) {
			wasT, _ := s.cfg.OIDC.PreviewRoleAgainst(before, nil, t.Groups, t.Email)
			nowT, _ := s.cfg.OIDC.PreviewRoleAgainst(after, nil, t.Groups, t.Email)
			if !s.roleSnapshotDrops(wasT, nowT) || !s.roleSnapshotDrops(t.Role, nowT) {
				continue
			}
		} else if !s.roleSnapshotDrops(t.Role, oidc.RoleMember) {
			continue
		}
		if !slices.Contains(principals, t.Principal) {
			principals = append(principals, t.Principal)
		}
	}
	revoked := 0
	for _, p := range principals {
		n, rerr := s.revokeAPITokensFor(ctx, p)
		revoked += n
		if rerr != nil {
			slog.WarnContext(ctx, "api: could not revoke every api token of a demoted principal",
				"principal", p, "value", value, "revoked", n, "error", rerr)
		}
	}
	if revoked > 0 {
		slog.WarnContext(ctx, "api: role mapping demoted a value; the affected principals' api tokens were revoked",
			"value", value, "was", was, "now", now, "tokens_revoked", revoked, "principals", len(principals))
	}
	return revoked
}

// roleSnapshotWarnNote / roleSnapshotWarnRemedy are the operator-facing halves
// of the role-mapping WARN, kept as named constants because they are a CLAIM
// about system behaviour that has now drifted twice.
//
// The line used to end "a token's role is frozen at mint and no sign-in
// refreshes it", which the same release had already made false: the token lane
// gained the login hook the key lane had since 0046 (store.RefreshAPITokenRoles,
// and CHANGELOG 0.7 says so in plain words). Then the demotion path itself began
// revoking what it demotes (F112). An operator acting on a stale remedy line
// either does unnecessary work or assumes a bound that is not there, so the two
// strings say exactly the three things that are true at once: what this edit
// already did, what the owner's next login will do, and when the human still has
// to reach for the lever.
const (
	roleSnapshotWarnNote = "counted BEFORE this edit acted; the snapshots this edit demotes were revoked with it (see the revoke line), and the rest keep a role this edit did not change"

	roleSnapshotWarnRemedy = "nothing further is needed for the principals this edit demoted — they were revoked. For the rest, the owner's next sign-in re-stamps the role on every unrevoked token they hold (store.RefreshAPITokenRoles, the same OnLogin hook that has refreshed SSH keys since 0.6); POST /api/v1/sessions/revoke {\"sub\":\"<principal>\"} or DELETE /api/v1/tokens/{id} is the lever when a change has to take effect immediately or the owner will not sign in again"
)

// noteStaleRoleSnapshots counts the mint-time token snapshots a role-mapping write
// does not reach and says so at WARN, naming the lever. Returns the count for
// the handler to publish; 0 on any failure, which is the quiet direction — a
// count this could not take must not read as "none outstanding" in the audit
// row, so the error is logged where an operator sees it.
func (s *Server) noteStaleRoleSnapshots(ctx context.Context, value, what string) int {
	n, err := s.staleRoleSnapshotCount(ctx, value)
	if err != nil {
		slog.WarnContext(ctx, "api: could not count api tokens carrying a frozen role snapshot for this mapping",
			"value", value, "error", err)
		return 0
	}
	if n > 0 {
		slog.WarnContext(ctx, "api: role mapping changed; outstanding api tokens carry a role stamped at mint",
			"value", value, "change", what, "tokens", n,
			"note", roleSnapshotWarnNote, "remedy", roleSnapshotWarnRemedy)
	}
	return n
}
