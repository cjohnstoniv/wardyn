// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/scim"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The SCIM Groups routes: how an identity provider tells Wardyn a person left a group. A group created
// and a member added are stored and grant nothing; no authorisation decision reads them. A member
// removed is a mover: the person's sessions are cut and their API tokens whose group snapshot holds the
// group are revoked, recorded as a group_remove job in deprovision_jobs that the request answers 5xx
// until it is done.

func (s *Server) mountSCIMGroupRoutes(r chi.Router) {
	r.Get("/Groups", s.handleSCIMListGroups)
	r.Get("/Groups/{id}", s.handleSCIMGetGroup)
	r.Post("/Groups", s.handleSCIMCreateGroup)
	r.Patch("/Groups/{id}", s.handleSCIMPatchGroup)
	r.Delete("/Groups/{id}", s.handleSCIMDeleteGroup)
}

// scimGroupLoad loads the group a {id} path names.
func (s *Server) scimGroupLoad(w http.ResponseWriter, r *http.Request) (scimStore, store.ScimGroup, bool) {
	st, ok := s.scimStore(w)
	if !ok {
		return nil, store.ScimGroup{}, false
	}
	notFound := scim.NewError(http.StatusNotFound, "", "no such group")
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeSCIMError(w, notFound)
		return nil, store.ScimGroup{}, false
	}
	g, err := st.GetScimGroup(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeSCIMError(w, notFound)
		return nil, store.ScimGroup{}, false
	}
	if err != nil {
		s.scimServerError(w, r, "read the group", err)
		return nil, store.ScimGroup{}, false
	}
	return st, g, true
}

// scimGroupResource renders a group, with the members Wardyn has recorded, as a SCIM Group.
func scimGroupResource(ctx context.Context, st scimStore, g store.ScimGroup) (scim.Group, error) {
	ids, err := st.ScimGroupMembers(ctx, g.ID)
	if err != nil {
		return scim.Group{}, err
	}
	members := make([]scim.Member, 0, len(ids))
	for _, id := range ids {
		members = append(members, scim.Member{Value: id.String()})
	}
	return scim.Group{
		Schemas: []string{scim.SchemaGroup}, ID: g.ID.String(), ExternalID: g.ExternalID, DisplayName: g.DisplayName,
		Members: members, Meta: &scim.Meta{ResourceType: "Group", Created: g.CreatedAt.UTC().Format(time.RFC3339)},
	}, nil
}

func (s *Server) handleSCIMListGroups(w http.ResponseWriter, r *http.Request) {
	st, ok := s.scimStore(w)
	if !ok {
		return
	}
	raw := r.URL.Query().Get("filter")
	if strings.TrimSpace(raw) == "" {
		writeSCIMError(w, scim.NewError(http.StatusBadRequest, scim.TypeInvalidFilter,
			`a filter is required: displayName or externalId eq "value"`))
		return
	}
	f, perr := scim.ParseFilter(raw, store.SearchDisplayName, store.SearchExternalID)
	if perr != nil {
		writeSCIMError(w, perr)
		return
	}
	rows, err := st.SearchScimGroups(r.Context(), f.Attr, f.Value, scimListLimit)
	if err != nil {
		s.scimServerError(w, r, "search groups", err)
		return
	}
	groups := make([]scim.Group, 0, len(rows))
	for _, row := range rows {
		g, err := scimGroupResource(r.Context(), st, row)
		if err != nil {
			s.scimServerError(w, r, "read the group", err)
			return
		}
		groups = append(groups, g)
	}
	writeSCIM(w, http.StatusOK, scim.NewListResponse(groups))
}

func (s *Server) handleSCIMGetGroup(w http.ResponseWriter, r *http.Request) {
	st, g, ok := s.scimGroupLoad(w, r)
	if !ok {
		return
	}
	s.writeSCIMGroup(w, r, st, g, http.StatusOK)
}

func (s *Server) writeSCIMGroup(w http.ResponseWriter, r *http.Request, st scimStore, g store.ScimGroup, status int) {
	res, err := scimGroupResource(r.Context(), st, g)
	if err != nil {
		s.scimServerError(w, r, "read the group", err)
		return
	}
	writeSCIM(w, status, res)
}

// memberIdentities are the identity ids a members value names. A SCIM member's value is the id Wardyn
// returned for the user; a value that is not one names nobody Wardyn knows.
func memberIdentities(ms []scim.Member) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ms))
	for _, m := range ms {
		if id, err := uuid.Parse(strings.TrimSpace(m.Value)); err == nil && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func (s *Server) handleSCIMCreateGroup(w http.ResponseWriter, r *http.Request) {
	st, ok := s.scimStore(w)
	if !ok {
		return
	}
	body, ok := readSCIMBody(w, r)
	if !ok {
		return
	}
	in, perr := scim.ParseGroup(body)
	if perr != nil {
		writeSCIMError(w, perr)
		return
	}
	ctx := r.Context()
	now := s.cfg.Now().UTC()
	g, err := st.CreateScimGroup(ctx, in.ExternalID, in.DisplayName, now)
	if errors.Is(err, store.ErrScimGroupExists) {
		writeSCIMError(w, scim.NewError(http.StatusConflict, "uniqueness", "a group with this externalId already exists"))
		return
	}
	if err == nil {
		err = st.AddScimGroupMembers(ctx, g.ID, memberIdentities(in.Members), now)
	}
	if err != nil {
		s.scimServerError(w, r, "create the group", err)
		return
	}
	s.writeSCIMGroup(w, r, st, g, http.StatusCreated)
}

func (s *Server) handleSCIMPatchGroup(w http.ResponseWriter, r *http.Request) {
	st, g, ok := s.scimGroupLoad(w, r)
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
	ctx := r.Context()
	slot := scimSlot(ctx)
	// The whole request is read before any of it is applied, except that removals are never held back
	// by a refusal: a refused externalId change answers 400 after the members have been removed.
	externalIDRefused := false
	displayName := ""
	for _, op := range ops {
		if v, _ := op.Value.(string); op.Kind != scim.OpRemove {
			switch op.Attr {
			case scim.AttrDisplayName:
				displayName = v
			case scim.AttrExternalID:
				externalIDRefused = externalIDRefused || !strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(g.ExternalID))
			}
		}
	}
	var errs []error
	for _, op := range ops {
		if op.Attr != scim.AttrMembers {
			continue
		}
		members, _ := op.Value.([]scim.Member)
		switch op.Kind {
		case scim.OpAdd:
			if err := st.AddScimGroupMembers(ctx, g.ID, memberIdentities(members), s.cfg.Now().UTC()); err != nil {
				errs = append(errs, err)
			}
		case scim.OpRemove:
			ids := memberIdentities(members)
			if members == nil {
				var err error
				if ids, err = st.GroupRemovalIdentities(ctx, g.ID); err != nil {
					errs = append(errs, err)
				}
			}
			errs = append(errs, s.removeGroupMembers(ctx, st, g, ids, slot))
		}
	}
	if err := errors.Join(errs...); err != nil {
		s.scimServerError(w, r, "update the group", err)
		return
	}
	if externalIDRefused {
		writeSCIMError(w, scim.NewError(http.StatusBadRequest, scim.TypeInvalidValue, "externalId cannot change on a group"))
		return
	}
	if displayName != "" {
		if err := st.RenameScimGroup(ctx, g.ID, displayName); err != nil {
			s.scimServerError(w, r, "rename the group", err)
			return
		}
		g.DisplayName = displayName
	}
	s.writeSCIMGroup(w, r, st, g, http.StatusOK)
}

func (s *Server) handleSCIMDeleteGroup(w http.ResponseWriter, r *http.Request) {
	st, g, ok := s.scimGroupLoad(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	ids, err := st.GroupRemovalIdentities(ctx, g.ID)
	if err == nil {
		err = s.removeGroupMembers(ctx, st, g, ids, scimSlot(ctx))
	}
	if err == nil {
		err = st.DeleteScimGroup(ctx, g.ID)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.scimServerError(w, r, "delete the group", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeGroupMembers removes each identity from the group as a mover. Every one is attempted whether or
// not another failed; the joined error wraps errDeprovisionIncomplete.
func (s *Server) removeGroupMembers(ctx context.Context, st scimStore, g store.ScimGroup, ids []uuid.UUID, slot string) error {
	var errs []error
	for _, id := range ids {
		if err := s.removeGroupMember(ctx, st, g, id, slot); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	slog.WarnContext(ctx, "api: a group removal step failed and stays pending", slog.String("group", g.ID.String()), slog.Any("err", errors.Join(errs...)))
	return fmt.Errorf("%w: %w", errDeprovisionIncomplete, errors.Join(errs...))
}

// A group removal is three steps, each idempotent and recorded in deprovision_jobs: the person's
// browser sessions cut, their own API tokens that the group's removal can affect revoked, and the
// scim.group.member_remove row written once both are done.
//
// The sessions are cut without touching the person's other credentials (oidc.CutSessions), so a token
// whose group snapshot does not hold the group keeps working. A token is revoked when its snapshot
// holds the group's external id (compared lower-cased and trimmed, as the snapshot was normalised at
// sign-in; never the display name) or when it cannot say, because the snapshot was truncated or never
// recorded. Only the removed person's tokens are looked at.
func (s *Server) removeGroupMember(ctx context.Context, st scimStore, g store.ScimGroup, identityID uuid.UUID, slot string) error {
	ctx = withActor(ctx, types.ActorSystem, scimActor)
	ident, err := st.GetIdentity(ctx, identityID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := st.StartGroupRemoval(ctx, g.ID, identityID); err != nil {
		return err
	}
	forms, err := s.leaverForms(ctx, st, ident)
	if err != nil {
		return err
	}
	all, err := st.ListDeprovisionJobs(ctx, identityID, store.JobKindGroupRemove)
	if err != nil {
		return err
	}
	jobs := slices.DeleteFunc(all, func(j store.DeprovisionJob) bool { return j.Target != g.ID.String() })
	pending := func(step string) bool {
		j, ok := jobByKey(jobs, step, g.ID.String())
		return ok && !j.Done
	}
	finish := func(step string, runErr error, detail map[string]int) error {
		key := store.JobKey{Step: step, Target: g.ID.String()}
		if runErr != nil {
			return errors.Join(runErr, st.FailDeprovisionJob(ctx, identityID, store.JobKindGroupRemove, key, runErr))
		}
		return st.FinishDeprovisionJob(ctx, identityID, store.JobKindGroupRemove, key, detail)
	}

	var errs []error
	if pending(store.GroupStepSessions) {
		var cutErr error
		for _, t := range forms.targets {
			cutErr = errors.Join(cutErr, oidc.CutSessions(ctx, s.cfg.SessionRevocations, t))
		}
		errs = append(errs, finish(store.GroupStepSessions, cutErr, map[string]int{"sessions_cut": len(forms.targets)}))
	}
	if pending(store.GroupStepTokens) {
		revoked, revokeErr := 0, error(nil)
		match := groupSnapshotMatch(g.ExternalID)
		for _, t := range forms.targets {
			n, err := s.revokeAPITokensMatching(ctx, t, match)
			revoked += n
			revokeErr = errors.Join(revokeErr, err)
		}
		errs = append(errs, finish(store.GroupStepTokens, revokeErr, map[string]int{"tokens_revoked": revoked}))
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if !pending(store.GroupStepAudit) {
		return nil
	}
	if jobs, err = st.ListDeprovisionJobs(ctx, identityID, store.JobKindGroupRemove); err != nil {
		return err
	}
	counts := map[string]int{}
	for _, j := range jobs {
		if j.Target == g.ID.String() {
			for k, v := range j.Detail {
				counts[k] += v
			}
		}
	}
	ev := s.auditEvent(nil, types.ActorSystem, scimActor, "scim.group.member_remove", identityID.String(), "success", mustJSON(map[string]any{
		"slot": slot, "group": g.ID.String(), "group_external_id": g.ExternalID,
		"sessions_cut": counts["sessions_cut"], "tokens_revoked": counts["tokens_revoked"],
	}))
	return finish(store.GroupStepAudit, s.recordAuditStrict(ctx, ev), nil)
}

// groupSnapshotMatch selects the tokens a removal from the group with this external id affects: those
// whose login-time group snapshot holds it, and those whose snapshot cannot prove it absent (truncated,
// or never recorded; apiTokenSnapshotAnswerable).
func groupSnapshotMatch(externalID string) func(types.APIToken) bool {
	want := strings.ToLower(strings.TrimSpace(externalID))
	return func(t types.APIToken) bool {
		return !apiTokenSnapshotAnswerable(t) ||
			slices.ContainsFunc(t.Groups, func(g string) bool { return strings.ToLower(strings.TrimSpace(g)) == want })
	}
}
