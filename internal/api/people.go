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
//
// Entra ID's `sub` is pairwise, unknowable before the first sign-in, so there a
// person may instead be set up by tenant and object id (#1195): their principal
// is "entra:<tid>:<oid>", and a sign-in becomes it only when its issuer, `tid`
// and `oid` claims equal the row's exactly (peopleKeying).
package api

import (
	"context"
	"errors"
	"log/slog"
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
	// The sign-in half of the family: both sign-in doors (the callback and a
	// portal's token exchange) resolve an Entra person through it.
	if ps, ok := s.cfg.Store.(store.PersonStore); ok && s.cfg.OIDC != nil {
		s.cfg.OIDC.AttachPersonKeying(peopleKeying{s: s, ps: ps})
	}
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
	TenantID  string `json:"tenant_id"`
	ObjectID  string `json:"object_id"`
}

// entraPrincipalPrefix begins the principal of a person keyed by Entra object id.
const entraPrincipalPrefix = "entra:"

// objectIDPerson is the Entra-keyed person req asks for, or the 422 message
// refusing it. The ids are GUIDs, stored lowercase as Entra's claims carry them.
func (s *Server) objectIDPerson(req createPersonRequest) (types.Person, string) {
	if s.cfg.OIDC == nil || !s.cfg.OIDC.KeysPeopleByObjectID() {
		return types.Person{}, "tenant_id, object_id: a person is keyed by object id only when this deployment signs in with Microsoft Entra ID — give principal (the subject) instead"
	}
	if req.Principal != "" {
		return types.Person{}, "principal: give either principal or tenant_id and object_id, not both"
	}
	tid, terr := uuid.Parse(req.TenantID)
	oid, oerr := uuid.Parse(req.ObjectID)
	if terr != nil || oerr != nil || len(req.TenantID) != 36 || len(req.ObjectID) != 36 {
		return types.Person{}, "tenant_id, object_id: both are required, each a GUID (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx)"
	}
	return types.Person{
		Principal: entraPrincipalPrefix + tid.String() + ":" + oid.String(),
		Issuer:    s.cfg.OIDC.Issuer(), TenantID: tid.String(), ObjectID: oid.String(),
	}, ""
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
	want := types.Person{Principal: req.Principal}
	if req.TenantID != "" || req.ObjectID != "" {
		var refusal string
		if want, refusal = s.objectIDPerson(req); refusal != "" {
			writeErrorReason(w, http.StatusUnprocessableEntity, reasonPersonPrincipalInvalid, refusal)
			return
		}
		req.Principal = want.Principal
	}
	switch {
	case !validSubject(req.Principal):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonPersonPrincipalInvalid, "principal: the identity provider's subject (sub) exactly, 1-255 printable characters, no spaces")
		return
	case s.isReservedPrincipal(req.Principal):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonPersonPrincipalReserved, "principal: that subject is reserved for a non-person identity")
		return
	case want.ObjectID == "" && s.cfg.OIDC != nil && s.cfg.OIDC.KeysPeopleByObjectID() &&
		strings.HasPrefix(strings.ToLower(req.Principal), entraPrincipalPrefix):
		// No sign-in can become such a subject (PrincipalFor refuses it).
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonPersonPrincipalReserved, "principal: the entra: namespace is set by tenant_id and object_id, never as a subject")
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
	want.Email, want.CreatedBy = email, principalFromRequest(r)
	p, created, err := ps.CreatePerson(ctx, want)
	if errors.Is(err, store.ErrConflict) {
		writeErrorReason(w, http.StatusConflict, reasonPersonEmailTaken, personEmailTakenMsg)
		return
	}
	if err != nil {
		writeServerError(w, r, "create person", err)
		return
	}
	if !created && (p.Issuer != want.Issuer || p.TenantID != want.TenantID || p.ObjectID != want.ObjectID) && want.ObjectID != "" {
		// Same tenant and object id, recorded under another issuer or as a subject.
		s.auditOwnerRefusal(r, "person.create", req.Principal, "collision")
		writeErrorReason(w, http.StatusConflict, reasonPersonCollision, "principal: this object id is already recorded under a different issuer, or as a subject")
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, p)
		return
	}
	data := map[string]any{"email": p.Email}
	if p.ObjectID != "" {
		data["issuer"] = p.Issuer
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"person.create", p.Principal, "success", mustJSON(data)))
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
		return http.StatusConflict, reasonPersonMintNoSignIn,
			"this person's email derives no sign-in on this deployment (" + d.Denial + "), so there is no role to mint a token under"
	case elevated && slices.ContainsFunc(d.Matches, func(m oidc.Match) bool { return m.Source == oidc.MatchSourceDefaultRole }):
		return http.StatusConflict, reasonPersonMintDefaultRoleUnknownGroups,
			"this person's role would come from the elevated default role, which their groups could narrow once known — they must sign in once first"
	case elevated && !s.isOperator(ctx):
		return http.StatusForbidden, reasonPersonMintElevatedTarget,
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

// peopleKeying is oidc.PersonKeying over the person store: how an Entra
// sign-in finds a person set up by object id. The principal is derived from the
// tenant and object id, so one exact-key read answers it, and the row must
// agree on all three of issuer, tid and oid. The email is never read.
type peopleKeying struct {
	s  *Server
	ps store.PersonStore
}

func (k peopleKeying) PrincipalFor(ctx context.Context, subj oidc.Subject) (string, bool, error) {
	// The entra: namespace is object-id people's. No real Entra sub has a
	// colon, so a sub in it (any case: grants match subjects lowercased) is
	// refused rather than let it name one without their tid and oid.
	if strings.HasPrefix(strings.ToLower(subj.Sub), entraPrincipalPrefix) {
		return "", true, nil
	}
	if subj.TenantID == "" || subj.ObjectID == "" {
		return subj.Sub, false, nil
	}
	p, err := k.ps.GetPerson(ctx, entraPrincipalPrefix+subj.TenantID+":"+subj.ObjectID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return subj.Sub, false, nil
	case err != nil:
		return "", false, err
	case !samePersonKey(p, subj):
		return subj.Sub, false, nil
	}
	// Someone already known here by their pairwise sub keeps it: re-keying
	// them would leave everything they own behind, with no way back.
	known, err := k.subKnown(ctx, subj.Sub)
	if err != nil {
		return "", false, err
	}
	if known {
		k.attachIgnored(ctx, subj, p.Principal)
		return subj.Sub, false, nil
	}
	return p.Principal, false, nil
}

// subKnown reports whether sub already names someone here: a person record,
// or an API token, SSH key, run or workspace they own. Read only when a sign-in
// matches an object-id person, so an ordinary sign-in pays nothing.
func (k peopleKeying) subKnown(ctx context.Context, sub string) (bool, error) {
	if _, err := k.ps.GetPerson(ctx, sub); !errors.Is(err, store.ErrNotFound) {
		return err == nil, err
	}
	st := k.s.cfg.Store
	if toks, err := st.ListAPITokensByPrincipal(ctx, sub); err != nil || len(toks) > 0 {
		return len(toks) > 0, err
	}
	if keys, err := st.ListSSHKeysByPrincipal(ctx, sub); err != nil || len(keys) > 0 {
		return len(keys) > 0, err
	}
	if rp, ok := st.(store.RunsFilteredPager); ok {
		runs, err := rp.ListRunsFiltered(ctx, store.RunFilter{Owner: sub, IncludeKilled: true}, store.Page{Limit: 1})
		if err != nil || len(runs) > 0 {
			return len(runs) > 0, err
		}
	}
	wss, err := st.ListWorkspaces(ctx)
	return slices.ContainsFunc(wss, func(w types.Workspace) bool { return w.OwnedBy == sub }), err
}

// attachIgnored writes the denied person.attach for a sign-in that matched an
// object-id person but kept its own, already-known sub.
func (k peopleKeying) attachIgnored(ctx context.Context, subj oidc.Subject, principal string) {
	slog.WarnContext(ctx, "api: an Entra sign-in matched a person set up by object id, but its sub already names someone here; it keeps its sub",
		"sub", subj.Sub, "person", principal)
	k.s.recordAudit(ctx, k.s.auditEvent(nil, types.ActorHuman, subj.Sub, "person.attach", principal, "denied",
		mustJSON(map[string]any{"reason": "sub_known", "sub": subj.Sub, "issuer": subj.Issuer, "tenant_id": subj.TenantID, "object_id": subj.ObjectID})))
}

func samePersonKey(p types.Person, subj oidc.Subject) bool {
	return p.ObjectID != "" && p.Issuer == subj.Issuer && p.TenantID == subj.TenantID && p.ObjectID == subj.ObjectID
}

// Attached writes person.attach: the person is actor and target, and the
// identity provider's pairwise sub the other party.
func (k peopleKeying) Attached(r *http.Request, subj oidc.Subject, principal string) {
	k.s.recordAudit(r.Context(), k.s.auditEvent(nil, types.ActorHuman, principal, "person.attach", principal, "success",
		mustJSON(map[string]any{"sub": subj.Sub, "issuer": subj.Issuer, "tenant_id": subj.TenantID, "object_id": subj.ObjectID})))
}
