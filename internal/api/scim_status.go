// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// scimStatusListCap caps each list GET /scim/status returns: the card is a glance, and a
	// backlog past this is read in Audit.
	scimStatusListCap = 200
	// scimStatusAuditScan is how many of the newest audit rows are read to find the token slot that
	// last matched and the purges that listed drives.
	scimStatusAuditScan = 200
)

// scimStatus is GET /scim/status: the state of leaver deprovisioning for the console's SCIM card.
// Names, steps and errors only; never a token, a digest, or a record's content.
type scimStatus struct {
	// Configured is whether wardynd mounts the SCIM routes. When false every other field is empty.
	Configured bool `json:"configured"`
	// LastTokenSlot is the bearer slot of the newest SCIM write ("primary" or "next"), or "" before
	// the first one.
	LastTokenSlot string `json:"last_token_slot"`
	// PurgeAfterSeconds is WARDYN_SCIM_PURGE_AFTER; zero means only a DELETE purges.
	PurgeAfterSeconds int64 `json:"purge_after_seconds"`
	// KeepWorkspaces is WARDYN_SCIM_LEAVER_WORKSPACES set to keep.
	KeepWorkspaces bool                    `json:"keep_workspaces"`
	Deactivated    []scimStatusDeactivated `json:"deactivated"`
	Pending        []scimStatusPending     `json:"pending"`
	Drives         []scimStatusDrive       `json:"drives"`
}

type scimStatusDeactivated struct {
	Person        string     `json:"person"`
	DeactivatedAt time.Time  `json:"deactivated_at"`
	PurgeAfter    *time.Time `json:"purge_after,omitempty"`
}

type scimStatusPending struct {
	Person    string `json:"person"`
	Step      string `json:"step"`
	LastError string `json:"last_error"`
}

type scimStatusDrive struct {
	Person   string    `json:"person"`
	Drive    string    `json:"drive"`
	PurgedAt time.Time `json:"purged_at"`
}

// mountSCIMStatusRoute mounts the read on securityOps: the security admins who run leavers must see a
// stuck deprovisioning, and the read returns names, steps and errors, never a credential.
func (s *Server) mountSCIMStatusRoute(securityOps chi.Router) {
	securityOps.Get("/scim/status", s.handleSCIMStatus)
}

func (s *Server) handleSCIMStatus(w http.ResponseWriter, r *http.Request) {
	out := scimStatus{Deactivated: []scimStatusDeactivated{}, Pending: []scimStatusPending{}, Drives: []scimStatusDrive{}}
	st, ok := s.cfg.Store.(scimStore)
	if !ok || s.cfg.SCIM == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	ctx := r.Context()
	out.Configured = true
	out.PurgeAfterSeconds = int64(s.cfg.SCIM.PurgeAfter / time.Second)
	out.KeepWorkspaces = s.cfg.SCIM.KeepWorkspaces

	labels := map[uuid.UUID]string{}
	label := func(id uuid.UUID) (string, error) {
		if l, ok := labels[id]; ok {
			return l, nil
		}
		ident, err := st.GetIdentity(ctx, id)
		if err != nil {
			return "", err
		}
		labels[id] = identityLabel(ident)
		return labels[id], nil
	}

	deactivated, err := st.ListDeactivatedIdentities(ctx, scimStatusListCap)
	if err != nil {
		writeServerError(w, r, "list deactivated identities", err)
		return
	}
	for _, ident := range deactivated {
		labels[ident.ID] = identityLabel(ident)
		out.Deactivated = append(out.Deactivated, scimStatusDeactivated{
			Person: labels[ident.ID], DeactivatedAt: ident.DeactivatedAt.UTC(), PurgeAfter: utcPtr(ident.PurgeAfter),
		})
	}

	failures, err := st.ListDeprovisionFailures(ctx, scimStatusListCap)
	if err != nil {
		writeServerError(w, r, "list deprovision failures", err)
		return
	}
	for _, f := range failures {
		person, err := label(f.IdentityID)
		if err != nil {
			writeServerError(w, r, "read an unfinished identity", err)
			return
		}
		out.Pending = append(out.Pending, scimStatusPending{Person: person, Step: f.Step, LastError: f.LastError})
	}

	if out.LastTokenSlot, err = s.scimLastTokenSlot(ctx); err != nil {
		writeServerError(w, r, "read the last SCIM token slot", err)
		return
	}
	if out.Drives, err = s.scimDrivesToReclaim(ctx, st, label); err != nil {
		writeServerError(w, r, "read the drives to reclaim", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// identityLabel is how the card names an identity: the SCIM user name, else the login email, else the
// bound principal, else the object id.
func identityLabel(i store.PrincipalIdentity) string {
	for _, v := range []string{i.ScimUserName, i.EmailLower, i.Principal, i.ObjectID} {
		if v != "" {
			return v
		}
	}
	return i.ID.String()
}

// scimAuditRows is the newest audit rows wardynd itself wrote as the SCIM caller that match f.
func (s *Server) scimAuditRows(ctx context.Context, f store.AuditFilter) ([]types.AuditEvent, error) {
	pager, ok := s.cfg.Store.(store.Pager)
	if !ok {
		return nil, nil
	}
	f.Origin, f.ActorType, f.Actor = store.AuditOriginOrganisation, types.ActorSystem, scimActor
	return pager.QueryAuditEventsFilteredPage(ctx, nil, f, store.Page{Limit: scimStatusAuditScan})
}

// scimLastTokenSlot is the bearer slot of the newest scim.user.* row that names one; the sweeper's own
// rows name no bearer.
func (s *Server) scimLastTokenSlot(ctx context.Context) (string, error) {
	rows, err := s.scimAuditRows(ctx, store.AuditFilter{ActionPrefix: "scim.user."})
	if err != nil {
		return "", err
	}
	for _, ev := range rows { // newest first
		var d struct {
			Slot string `json:"slot"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && (d.Slot == scimSlotPrimary || d.Slot == scimSlotNext) {
			return d.Slot, nil
		}
	}
	return "", nil
}

// scimDrivesToReclaim is the drives the purges listed that still need their purged person's storage
// reclaimed, newest purge first. A purge lists a person's drives in its person.deprovision row and leaves
// them in place; a drive leaves the list when it is deleted or when a drive.reclaim succeeded for that
// person after the purge.
func (s *Server) scimDrivesToReclaim(ctx context.Context, st scimStore, label func(uuid.UUID) (string, error)) ([]scimStatusDrive, error) {
	out := []scimStatusDrive{}
	rows, err := s.scimAuditRows(ctx, store.AuditFilter{
		Action: "person.deprovision", DataContains: string(mustJSON(map[string]any{"kind": store.JobKindPurge})),
	})
	if err != nil || len(rows) == 0 {
		return out, err
	}
	live, err := s.cfg.Store.ListUserDrives(ctx)
	if err != nil {
		return nil, err
	}
	var reclaims []types.AuditEvent
	if pager, ok := s.cfg.Store.(store.Pager); ok {
		// Not scimAuditRows: an operator wrote these, not the SCIM caller.
		reclaims, err = pager.QueryAuditEventsFilteredPage(ctx, nil, store.AuditFilter{
			Action: "drive.reclaim", Outcome: "success", Origin: store.AuditOriginOrganisation,
		}, store.Page{Limit: scimStatusAuditScan})
		if err != nil {
			return nil, err
		}
	}
	for _, ev := range rows {
		var d struct {
			Drives []string `json:"drives"`
		}
		id, perr := uuid.Parse(ev.Target)
		if perr != nil || json.Unmarshal(ev.Data, &d) != nil {
			continue
		}
		var person string
		var ident store.PrincipalIdentity
		for _, name := range d.Drives {
			if !slices.ContainsFunc(live, func(l types.UserDriveListItem) bool { return l.Name == name }) {
				continue
			}
			if person == "" {
				if person, err = label(id); err != nil {
					return nil, err
				}
				if ident, err = st.GetIdentity(ctx, id); err != nil {
					return nil, err
				}
			}
			if scimReclaimedSince(reclaims, ev.Time, name, ident) {
				continue
			}
			out = append(out, scimStatusDrive{Person: person, Drive: name, PurgedAt: ev.Time.UTC()})
		}
	}
	return out, nil
}

// scimReclaimedSince reports whether a successful drive.reclaim of drive for ident's user grant is
// recorded after since.
func scimReclaimedSince(reclaims []types.AuditEvent, since time.Time, drive string, ident store.PrincipalIdentity) bool {
	return slices.ContainsFunc(reclaims, func(ev types.AuditEvent) bool {
		var d struct {
			Drive       string `json:"drive"`
			Subject     string `json:"subject"`
			SubjectType string `json:"subject_type"`
		}
		if !ev.Time.After(since) || json.Unmarshal(ev.Data, &d) != nil {
			return false
		}
		return d.SubjectType == "user" && d.Drive == drive && d.Subject != "" &&
			(strings.EqualFold(d.Subject, ident.EmailLower) || strings.EqualFold(d.Subject, ident.Principal))
	})
}
