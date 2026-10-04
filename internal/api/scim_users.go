// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/scim"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The SCIM Users routes: how an identity provider suspends and reactivates a person. SCIM only ever
// removes access. A user it creates is stored and grants nothing, a projection update never rebinds
// an identity, and no authorisation decision reads anything these routes write.

// scimListLimit caps a filtered list; startIndex and count are ignored.
const scimListLimit = 100

// mountSCIMRoutes registers the Users and Groups routes when Config.SCIM is set. DELETE of a user (purge)
// is not one of them.
func (s *Server) mountSCIMRoutes(r chi.Router) {
	if s.cfg.SCIM == nil {
		return
	}
	r.Route("/scim/v2", func(r chi.Router) {
		r.Use(s.scimAuth)
		r.Get("/Users", s.handleSCIMListUsers)
		r.Get("/Users/{id}", s.handleSCIMGetUser)
		r.Post("/Users", s.handleSCIMCreateUser)
		r.Patch("/Users/{id}", s.handleSCIMPatchUser)
		r.Put("/Users/{id}", s.handleSCIMPutUser)
		s.mountSCIMGroupRoutes(r)
	})
}

func (s *Server) scimStore(w http.ResponseWriter) (scimStore, bool) {
	st, ok := s.cfg.Store.(scimStore)
	if !ok {
		writeSCIMError(w, scim.NewError(http.StatusNotImplemented, "", "SCIM requires the Postgres store backend"))
	}
	return st, ok
}

// scimIdentity loads the identity row a {id} path names. The id is the identity row's own UUID,
// never a principal.
func (s *Server) scimIdentity(w http.ResponseWriter, r *http.Request) (scimStore, store.PrincipalIdentity, bool) {
	st, ok := s.scimStore(w)
	if !ok {
		return nil, store.PrincipalIdentity{}, false
	}
	notFound := scim.NewError(http.StatusNotFound, "", "no such user")
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeSCIMError(w, notFound)
		return nil, store.PrincipalIdentity{}, false
	}
	ident, err := st.GetIdentity(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeSCIMError(w, notFound)
		return nil, store.PrincipalIdentity{}, false
	}
	if err != nil {
		s.scimServerError(w, r, "read the user", err)
		return nil, store.PrincipalIdentity{}, false
	}
	return st, ident, true
}

func (s *Server) scimServerError(w http.ResponseWriter, r *http.Request, what string, err error) {
	slog.ErrorContext(r.Context(), "api: scim: "+what, slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("err", err))
	status, detail := http.StatusInternalServerError, "the server could not complete the request; retry"
	if errors.Is(err, errDeprovisionIncomplete) {
		detail = "deprovisioning is incomplete; retry the request"
	}
	writeSCIMError(w, scim.NewError(status, "", detail))
}

// scimResource renders an identity row as a SCIM User. active is false once the identity is
// deactivated or purged.
func scimResource(ident store.PrincipalIdentity) scim.User {
	u := scim.User{
		Schemas:    []string{scim.SchemaUser},
		ID:         ident.ID.String(),
		ExternalID: cmp.Or(ident.ScimExternalID, ident.ObjectID),
		UserName:   cmp.Or(ident.ScimUserName, ident.EmailLower, ident.ObjectID),
		Active:     ident.DeactivatedAt == nil && ident.PurgedAt == nil,
		Meta:       &scim.Meta{ResourceType: "User", Created: ident.CreatedAt.UTC().Format(time.RFC3339)},
	}
	if ident.EmailLower != "" {
		u.Emails = []scim.Email{{Value: ident.EmailLower, Type: "work", Primary: true}}
	}
	return u
}

func (s *Server) handleSCIMListUsers(w http.ResponseWriter, r *http.Request) {
	st, ok := s.scimStore(w)
	if !ok {
		return
	}
	raw := r.URL.Query().Get("filter")
	if strings.TrimSpace(raw) == "" {
		writeSCIMError(w, scim.NewError(http.StatusBadRequest, scim.TypeInvalidFilter,
			`a filter is required: userName, externalId or emails.value eq "value"`))
		return
	}
	f, perr := scim.ParseFilter(raw, store.SearchUserName, store.SearchExternalID, store.SearchEmail)
	if perr != nil {
		writeSCIMError(w, perr)
		return
	}
	rows, err := st.SearchIdentities(r.Context(), s.cfg.SCIM.Issuer, f.Attr, f.Value, scimListLimit)
	if err != nil {
		s.scimServerError(w, r, "search users", err)
		return
	}
	users := make([]scim.User, 0, len(rows))
	for _, row := range rows {
		users = append(users, scimResource(row))
	}
	writeSCIM(w, http.StatusOK, scim.NewListResponse(users))
}

func (s *Server) handleSCIMGetUser(w http.ResponseWriter, r *http.Request) {
	if _, ident, ok := s.scimIdentity(w, r); ok {
		writeSCIM(w, http.StatusOK, scimResource(ident))
	}
}

// normalizeExternalID is the canonical object id a SCIM externalId names: Entra's objectId, a GUID.
func normalizeExternalID(raw string) (string, bool) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	return id.String(), true
}

// scimNames are the names a SCIM user is known by, lower-cased: the userName and every email.
func scimNames(userName string, emails []string) []string {
	out := make([]string, 0, len(emails)+1)
	for _, v := range append([]string{userName}, emails...) {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func emailValues(in []scim.Email) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		out = append(out, e.Value)
	}
	return out
}

// scimMatch finds the identity a SCIM user stands for: the row holding its object id, then the row
// of the person set up as entra:<tenant>:<oid>, then, as a removal-only link, the one row with no
// object id seen under one of its names. More than one such row links nothing.
func (s *Server) scimMatch(ctx context.Context, st scimStore, oid string, names []string) (store.PrincipalIdentity, bool, error) {
	cfg := s.cfg.SCIM
	ident, err := st.GetIdentityByObject(ctx, cfg.Issuer, cfg.Tenant, oid)
	if err == nil {
		return ident, true, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.PrincipalIdentity{}, false, err
	}
	if ps, ok := s.cfg.Store.(store.PersonStore); ok {
		p, err := ps.GetPerson(ctx, entraPrincipalPrefix+cfg.Tenant+":"+oid)
		switch {
		case err == nil:
			rows, err := st.IdentitiesByPrincipal(ctx, p.Principal)
			if err != nil {
				return store.PrincipalIdentity{}, false, err
			}
			for _, row := range rows {
				if row.Issuer == cfg.Issuer {
					return row, true, nil
				}
			}
		case !errors.Is(err, store.ErrNotFound):
			return store.PrincipalIdentity{}, false, err
		}
	}
	var linked []store.PrincipalIdentity
	for _, name := range names {
		rows, err := st.IdentitiesByAlias(ctx, cfg.Issuer, name)
		if err != nil {
			return store.PrincipalIdentity{}, false, err
		}
		for _, row := range rows {
			if !slices.ContainsFunc(linked, func(l store.PrincipalIdentity) bool { return l.ID == row.ID }) {
				linked = append(linked, row)
			}
		}
	}
	if len(linked) == 1 {
		return linked[0], true, nil
	}
	return store.PrincipalIdentity{}, false, nil
}

// scimAudit records one scim.user.write, written by the SCIM caller and naming its token slot.
func (s *Server) scimAudit(ctx context.Context, outcome string, ident store.PrincipalIdentity, data map[string]any) {
	data["slot"] = scimSlot(ctx)
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, scimActor, "scim.user.write", ident.ID.String(), outcome, mustJSON(data)))
}

func (s *Server) handleSCIMCreateUser(w http.ResponseWriter, r *http.Request) {
	st, ok := s.scimStore(w)
	if !ok {
		return
	}
	body, ok := readSCIMBody(w, r)
	if !ok {
		return
	}
	u, perr := scim.ParseUser(body)
	if perr != nil {
		writeSCIMError(w, perr)
		return
	}
	oid, ok := normalizeExternalID(u.ExternalID)
	if !ok {
		writeSCIMError(w, scim.NewError(http.StatusBadRequest, scim.TypeInvalidValue, "externalId must be the Entra objectId, a GUID"))
		return
	}
	ctx := r.Context()
	upd := store.IdentityUpdate{ExternalID: oid, UserName: u.UserName, Emails: emailValues(u.Emails)}
	ident, found, err := s.scimMatch(ctx, st, oid, scimNames(u.UserName, upd.Emails))
	created := false
	switch {
	case err != nil:
	case !found:
		ident, created, err = st.CreateScimIdentity(ctx, s.cfg.SCIM.Issuer, s.cfg.SCIM.Tenant, oid, upd, s.cfg.Now().UTC())
	case ident.ScimExternalID != "" && !strings.EqualFold(ident.ScimExternalID, oid):
		writeSCIMError(w, scim.NewError(http.StatusConflict, "uniqueness", "this user is already linked to another externalId"))
		return
	default:
		if ident.ScimExternalID != "" {
			upd.ExternalID = ""
		}
		ident, err = st.ApplyIdentityUpdate(ctx, ident.ID, upd, s.cfg.Now().UTC())
	}
	if err != nil {
		s.scimServerError(w, r, "create the user", err)
		return
	}
	op := "link"
	if created {
		op = "create"
	}
	s.scimAudit(ctx, "success", ident, map[string]any{"op": op})
	if !u.Active {
		if err := s.suspendIdentity(ctx, st, ident.ID, scimSlot(ctx)); err != nil {
			s.scimServerError(w, r, "suspend the user", err)
			return
		}
		ident, _ = st.GetIdentity(ctx, ident.ID)
	}
	writeSCIM(w, http.StatusCreated, scimResource(ident))
}

func scimSlot(ctx context.Context) string {
	slot, _ := scimCallerSlot(ctx)
	return slot
}

func (s *Server) handleSCIMPatchUser(w http.ResponseWriter, r *http.Request) {
	st, ident, ok := s.scimIdentity(w, r)
	if !ok {
		return
	}
	body, ok := readSCIMBody(w, r)
	if !ok {
		return
	}
	ops, perr := scim.ParsePatch(body)
	if perr != nil {
		writeSCIMError(w, perr.Error)
		return
	}
	var ch scimChange
	for _, op := range ops {
		if op.Kind == scim.OpRemove {
			continue
		}
		switch v := op.Value.(type) {
		case bool:
			if op.Attr == scim.AttrActive {
				ch.active = &v
			}
		case string:
			switch op.Attr {
			case scim.AttrUserName:
				ch.userName = v
			case scim.AttrEmail:
				ch.emails = append(ch.emails, v)
			case scim.AttrExternalID:
				ch.externalID = v
			}
		}
	}
	s.applySCIMChange(w, r, st, ident, ch)
}

func (s *Server) handleSCIMPutUser(w http.ResponseWriter, r *http.Request) {
	st, ident, ok := s.scimIdentity(w, r)
	if !ok {
		return
	}
	body, ok := readSCIMBody(w, r)
	if !ok {
		return
	}
	u, perr := scim.ParseUser(body)
	if perr != nil {
		writeSCIMError(w, perr)
		return
	}
	s.applySCIMChange(w, r, st, ident, scimChange{
		active: &u.Active, userName: u.UserName, emails: emailValues(u.Emails), externalID: u.ExternalID,
	})
}

// scimChange is what a PATCH or a PUT asks of one identity: the attributes Wardyn acts on.
type scimChange struct {
	active     *bool
	userName   string
	emails     []string
	externalID string
}

// externalIDAllowed reports whether the requested externalId leaves the identity's binding as it is.
// The object id is a binding column and the SCIM externalId its projection; neither may change.
func externalIDAllowed(ident store.PrincipalIdentity, requested string) bool {
	oid, ok := normalizeExternalID(requested)
	if !ok {
		oid = strings.ToLower(strings.TrimSpace(requested))
	}
	if ident.ObjectID != "" && oid != ident.ObjectID {
		return false
	}
	return ident.ScimExternalID == "" || strings.EqualFold(oid, ident.ScimExternalID)
}

// applySCIMChange is the one rule set PATCH and PUT share. The request is validated in full and
// applied in one transaction, with one exception: a refused externalId change beside active=false
// still suspends, because removal outranks atomicity, and the response is then the 400.
func (s *Server) applySCIMChange(w http.ResponseWriter, r *http.Request, st scimStore, ident store.PrincipalIdentity, ch scimChange) {
	ctx := r.Context()
	suspend := ch.active != nil && !*ch.active
	if ch.externalID != "" && !externalIDAllowed(ident, ch.externalID) {
		s.scimAudit(ctx, "denied", ident, map[string]any{"reason": "external_id_immutable"})
		if suspend {
			if err := s.suspendIdentity(ctx, st, ident.ID, scimSlot(ctx)); err != nil {
				s.scimServerError(w, r, "suspend the user", err)
				return
			}
		}
		writeSCIMError(w, scim.NewError(http.StatusBadRequest, scim.TypeInvalidValue, "externalId cannot change on a bound identity"))
		return
	}
	reactivate := ch.active != nil && *ch.active
	before, err := st.IdentityAliasValues(ctx, ident.ID)
	if err != nil {
		s.scimServerError(w, r, "read the user", err)
		return
	}
	upd := store.IdentityUpdate{UserName: ch.userName, Emails: ch.emails, Reactivate: reactivate}
	if ch.externalID != "" && ident.ScimExternalID == "" {
		upd.ExternalID, _ = normalizeExternalID(ch.externalID)
	}
	if reactivate {
		forms, ferr := s.leaverForms(ctx, st, ident)
		if ferr != nil {
			s.scimServerError(w, r, "read the user", ferr)
			return
		}
		upd.PeoplePrincipals = forms.bound
	}
	updated, err := st.ApplyIdentityUpdate(ctx, ident.ID, upd, s.cfg.Now().UTC())
	if errors.Is(err, store.ErrIdentityPurged) {
		s.scimAudit(ctx, "denied", ident, map[string]any{"reason": "purged"})
		writeSCIMError(w, scim.NewError(http.StatusBadRequest, scim.TypeInvalidValue, "this identity was purged and cannot be reactivated; a re-hired person needs a new identity"))
		return
	}
	if err != nil {
		s.scimServerError(w, r, "update the user", err)
		return
	}
	after, _ := st.IdentityAliasValues(ctx, ident.ID)
	reactivated := reactivate && ident.DeactivatedAt != nil && updated.DeactivatedAt == nil
	if reactivated || updated.ScimUserName != ident.ScimUserName || updated.ScimExternalID != ident.ScimExternalID || !slices.Equal(before, after) {
		op := "projection"
		if reactivated {
			op = "reactivate"
		}
		s.scimAudit(ctx, "success", updated, map[string]any{"op": op})
	}
	if suspend {
		if err := s.suspendIdentity(ctx, st, ident.ID, scimSlot(ctx)); err != nil {
			s.scimServerError(w, r, "suspend the user", err)
			return
		}
		updated, _ = st.GetIdentity(ctx, ident.ID)
	}
	writeSCIM(w, http.StatusOK, scimResource(updated))
}
