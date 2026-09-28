// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// People an admin sets up before their first sign-in, and API tokens an admin
// mints for them (#1157) — the interim path for an install whose members never
// open the console, until delegation (#1142) lands.
//
// The keying rule is the one sign-in already uses: a person's principal IS the
// identity provider's `sub`, exact and case-sensitive (oidc's CallbackHandler
// publishes idToken.Subject and nothing else). So a pre-created person is keyed
// by that subject, a sign-in attaches to it by subject equality alone, and the
// email the admin records is an assertion used only to derive the role before
// the person's own claims exist. An email never attaches anyone: a different
// person signing in with the same email carries a different subject and lands
// on a different principal.
package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mountPeopleRoutes is the pre-created identity and admin-mint family, all on
// securityOps: the issue's tier. The guard against a security admin minting an
// admin's credential lives in the mint handler, because it depends on WHO the
// token is for, not on the route.
func (s *Server) mountPeopleRoutes(securityOps chi.Router) {
	securityOps.Post("/people", s.handleCreatePerson)
	securityOps.Post("/people/{principal}/tokens", s.handleMintPersonAPIToken)
	securityOps.Get("/people/{principal}/tokens", s.handleListPersonAPITokens)
}

func (s *Server) personStoreOr501(w http.ResponseWriter) (store.PersonStore, bool) {
	ps, ok := s.cfg.Store.(store.PersonStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonPeopleStoreUnavailable, "pre-created people require the Postgres store backend")
	}
	return ps, ok
}

// principalParam is the {principal} path segment, unescaped when the client
// percent-encoded it: a subject may carry a '/' or a '|'.
func principalParam(r *http.Request) string {
	raw := chi.URLParam(r, "principal")
	if r.URL.RawPath != "" {
		if u, err := url.PathUnescape(raw); err == nil {
			raw = u
		}
	}
	return raw
}

// validSubject is the shape an OIDC `sub` may take (OIDC Core §2: at most 255
// ASCII characters): printable, no spaces.
func validSubject(p string) bool {
	return p != "" && len(p) <= 255 && !strings.ContainsFunc(p, func(r rune) bool { return r <= ' ' || r > '~' })
}

func validPersonEmail(e string) bool {
	local, domain, ok := strings.Cut(e, "@")
	return ok && local != "" && domain != "" && !strings.Contains(domain, "@") && validSubject(e)
}

type createPersonRequest struct {
	Principal string `json:"principal"`
	Email     string `json:"email"`
}

// handleCreatePerson is POST /api/v1/people: create the person keyed by their
// IdP subject, or confirm the one already there (200, same body). The refusals
// are what keep the keying unambiguous — see personCollision.
func (s *Server) handleCreatePerson(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.personStoreOr501(w)
	if !ok {
		return
	}
	var req createPersonRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	switch {
	case !validSubject(req.Principal):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonPersonPrincipalInvalid, "principal: the identity provider's subject (sub) exactly, 1-255 printable characters, no spaces")
		return
	case s.isReservedPrincipal(req.Principal):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonPersonPrincipalReserved, "principal: that subject is reserved for a non-person identity")
		return
	case email != "" && !validPersonEmail(email):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonPersonEmailInvalid, "email: invalid")
		return
	}
	ctx := r.Context()
	people, err := ps.ListPeople(ctx)
	if err != nil {
		writeServerError(w, r, "list people", err)
		return
	}
	directory := s.knownPrincipals(ctx)
	for _, p := range people {
		directory = append(directory, principalIdentity{principal: p.Principal, email: p.Email})
	}
	if refusal := personCollision(directory, req.Principal, email); refusal != "" {
		s.auditOwnerRefusal(r, "person.create", req.Principal, "collision")
		writeErrorReason(w, http.StatusConflict, reasonPersonCollision, refusal)
		return
	}
	p, created, err := ps.CreatePerson(ctx, types.Person{Principal: req.Principal, Email: email, CreatedBy: principalFromRequest(r)})
	if errors.Is(err, store.ErrConflict) {
		writeErrorReason(w, http.StatusConflict, reasonPersonEmailTaken, personEmailTakenMsg)
		return
	}
	if err != nil {
		writeServerError(w, r, "create person", err)
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, p)
		return
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"person.create", p.Principal, "success", mustJSON(map[string]any{"email": p.Email})))
	writeJSON(w, http.StatusCreated, p)
}

const personEmailTakenMsg = "email: another subject is already known by this email — a sign-in attaches by subject, so two subjects sharing one email would share every email-keyed role mapping and grant"

// personCollision refuses a (subject, email) pair that would make an identity
// ambiguous against the directory of subjects this deployment already knows,
// or "" when it would not. Capability grants and role mappings match a caller
// by lowercased subject OR email, so the ambiguity that matters is
// case-insensitive in both columns:
//
//   - the subject is known under a DIFFERENT email (the admin's assertion
//     contradicts the one its own sign-in or token recorded);
//   - another subject differs from this one only by case;
//   - the email, or the subject itself, already names another subject.
func personCollision(directory []principalIdentity, principal, email string) string {
	names := func(v string) bool {
		return v != "" && (strings.EqualFold(v, principal) || strings.EqualFold(v, email))
	}
	for _, d := range directory {
		if d.principal == principal {
			if d.email != "" && email != "" && !strings.EqualFold(d.email, email) {
				return "principal: this subject is already known by a different email"
			}
			continue
		}
		if strings.EqualFold(d.principal, principal) {
			return "principal: another subject differs from this one only by case"
		}
		if names(d.email) || (email != "" && strings.EqualFold(d.principal, email)) {
			return personEmailTakenMsg
		}
	}
	return ""
}

// personMintNoHumanRefusal is the 403 for a caller with no signed-in human
// behind them: minted_by must name a person.
const personMintNoHumanRefusal = "minting a token for a person needs a signed-in admin — the admin token and local mode name no one to record as its minter"

// handleMintPersonAPIToken is POST /api/v1/people/{principal}/tokens: mint a
// wdn_ token OWNED by a pre-created person, stamped with the role and user type
// their sign-in would derive from what is knowable before it, and return the
// plaintext once.
//
// The stamp. Before a first sign-in the person's roles and groups claims do not
// exist, so the role is derived from their recorded email alone, through the
// same PreviewRole a login's derivation uses, and the group snapshot is stamped
// UNKNOWN (nil, truncated): every group-tier ceiling, drive and deny grant
// fails closed for the token exactly as it does for a truncated session. The
// person's sign-in re-stamps it (store.RefreshAPITokenIdentity), and revokes it
// instead if the real role differs — see that method.
//
// The guards, in order: a signed-in human caller who is not itself a token
// (the /me/tokens rules, for the same reasons); a person record for exactly
// this subject; a derivation that admits a sign-in at all and does not rest on
// an elevated default role their unseen groups might have narrowed; and a
// target on the admin or security-admin tier only for a super admin caller.
func (s *Server) handleMintPersonAPIToken(w http.ResponseWriter, r *http.Request) {
	// Admission time, before the body is read — handleCreateAPIToken says why.
	authorizedAt := s.cfg.Now().UTC()
	var req createAPITokenRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	ctx := r.Context()
	caller := oidcHumanFromContext(ctx)
	if caller == "" || s.cfg.OIDC == nil {
		writeErrorReason(w, http.StatusForbidden, reasonPersonMintNoHuman, personMintNoHumanRefusal)
		return
	}
	if apiTokenIDFromContext(ctx) != uuid.Nil {
		writeErrorReason(w, http.StatusForbidden, reasonAPITokenFromAPIToken, "an API token cannot create another API token — sign in to the console to mint one")
		return
	}
	ps, ok := s.personStoreOr501(w)
	if !ok {
		return
	}
	name, ok := apiTokenName(w, req.Name)
	if !ok {
		return
	}
	principal := principalParam(r)
	p, err := ps.GetPerson(ctx, principal)
	if errors.Is(err, store.ErrNotFound) || (err == nil && s.isReservedPrincipal(p.Principal)) {
		writeErrorReason(w, http.StatusNotFound, reasonPersonNotFound, "no person is recorded under this subject — create or confirm them with POST /api/v1/people first")
		return
	}
	if err != nil {
		writeServerError(w, r, "get person", err)
		return
	}
	d, err := s.cfg.OIDC.PreviewRole(ctx, nil, nil, p.Email)
	if err != nil {
		writeServerError(w, r, "derive role", err)
		return
	}
	if status, reason, msg := s.personMintRefusal(ctx, d); status != 0 {
		s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), caller,
			"person.token.create", p.Principal, "denied", mustJSON(map[string]any{"reason": reason, "role": d.Role})))
		writeErrorReason(w, status, reason, msg)
		return
	}
	if s.apiTokenCapReached(w, r, p.Principal) {
		return
	}
	// The caller's own session may have been cut off while this was in flight
	// (handleCreateAPIToken's late re-check, for the same instant).
	if s.cfg.SessionRevocations != nil {
		revoked, rerr := s.cfg.SessionRevocations.IsSessionRevoked(ctx, caller, oidcEmailFromContext(ctx), authorizedAt)
		if rerr != nil {
			writeServerError(w, r, "create api token", rerr)
			return
		}
		if revoked {
			writeErrorReason(w, http.StatusForbidden, reasonPersonMintNoHuman, personMintNoHumanRefusal)
			return
		}
	}
	unknown := true
	created, ok := s.insertAPIToken(w, r, types.APIToken{
		Principal: p.Principal, Email: p.Email, Role: d.Role, UserType: d.UserType,
		GroupsTruncated: &unknown, Name: name, CreatedAt: authorizedAt, MintedBy: caller,
	})
	if !ok {
		return
	}
	// Both parties: the actor is the minter, principal the owner.
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), caller,
		"person.token.create", created.ID.String(), "success",
		mustJSON(map[string]any{"principal": created.Principal, "name": created.Name, "role": created.Role, "user_type": created.UserType})))
	writeJSON(w, http.StatusCreated, created)
}

// personMintRefusal is the derivation half of the mint guards: status 0 when
// the mint may proceed. The tiers are asked through roleSnapshotCtx, never
// re-derived from a role comparison (http.go's rule).
func (s *Server) personMintRefusal(ctx context.Context, d oidc.Derivation) (status int, reason, msg string) {
	elevated := s.isSecurityOperator(roleSnapshotCtx(d.Role))
	switch {
	case !d.OK():
		return http.StatusConflict, "no_sign_in",
			"this person's email derives no sign-in on this deployment (" + d.Denial + "), so there is no role to mint a token under"
	case elevated && slices.ContainsFunc(d.Matches, func(m oidc.Match) bool { return m.Source == oidc.MatchSourceDefaultRole }):
		return http.StatusConflict, "default_role_unknown_groups",
			"this person's role would come from the elevated default role, which their groups could narrow once known — they must sign in once first"
	case elevated && !s.isOperator(ctx):
		return http.StatusForbidden, "elevated_target",
			"only a super admin may mint a token for an admin or a security admin"
	}
	return 0, "", ""
}

// handleListPersonAPITokens is GET /api/v1/people/{principal}/tokens: one
// person's tokens, revoked included, exactly as they see them at /me/tokens.
// Revoking one is DELETE /tokens/{id}.
func (s *Server) handleListPersonAPITokens(w http.ResponseWriter, r *http.Request) {
	s.listAPITokensFor(w, r, principalParam(r))
}
