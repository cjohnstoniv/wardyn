// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Kept machines, the organisation's side: a person lists and deletes their own
// (/me/machines), and a super admin lists and deletes everyone's
// (/machines/kept), in the member drives' pattern. owner_subject is the
// authority: another person's machine answers as one that does not exist.
// The delete protocol, the sweeps, erasure and the kill hook are in
// kept_machine_delete.go.
package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// keptMachineNewWorstBytes is the most disk one new kept machine can take on its Mac: the base disk's
// logical size (macvm.BaseDiskBytes) plus what a run reserves beside it (admission.DiskExtraBytes), the
// sum the runner checks its own limit with. TestKeptMachineNewWorstBytesIsTheRunners pins the two.
const keptMachineNewWorstBytes int64 = 50<<30 + 1<<30

func keptMachineNotYourMacMsg() string {
	return "Wardyn keeps a machine only on a Mac whose runner you registered, for runs you start yourself."
}

func keptMachineNotFoundMsg(id string) string { return fmt.Sprintf("You have no kept machine %s.", id) }

func keptMachineInUseDeleteMsg() string {
	return "This machine is in use by a run. End that run first, then delete it."
}

func keptMachineDeletingMsg(name string) string {
	return fmt.Sprintf("Your machine %s is being deleted.", name)
}

func keptMachineDeleteUnconfirmedMsg() string {
	return "Wardyn couldn't confirm the delete. It will keep trying; the machine can't be resumed meanwhile."
}

func keptMachineDeleteFailedMsg(mac string) string {
	return fmt.Sprintf("The machine couldn't be deleted from %s. Nothing was removed; try again.", mac)
}

// mountKeptMachineRoutes registers the person's two routes on r and the super admin's two on
// operatorOnly: deleting a kept machine destroys a person's files, the same rail as a member drive's.
func (s *Server) mountKeptMachineRoutes(r, operatorOnly chi.Router) {
	r.Get("/me/machines", s.handleListMyMachines)
	r.Delete("/me/machines/{id}", s.handleDeleteMyMachine)
	operatorOnly.Get("/machines/kept", s.handleListKeptMachines)
	operatorOnly.Delete("/machines/kept/{id}", s.handleDeleteKeptMachineAdmin)
}

// keptMachineOwner is the person a /me/machines route acts for: the canonical subject of a session or of
// the person's own API token. A device, a delegated caller, SCIM, a service identity, the admin token and
// local mode have no person here and are refused.
func keptMachineOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	owner := types.CanonicalUserSubject(oidcHumanFromContext(r.Context()))
	if owner == "" || neverOperator(r.Context()) {
		writeErrorReason(w, http.StatusForbidden, reasonKeptMachineNotYourMac, keptMachineNotYourMacMsg())
		return "", false
	}
	return owner, true
}

// handleListMyMachines is GET /me/machines: the caller's own kept machines, deleting ones included, and
// whether they may keep another (client.MyMachines). New Run reads Allowed to show its machine section;
// the run's create checks again. A store that keeps no machines answers none, with the site switch's
// reason.
func (s *Server) handleListMyMachines(w http.ResponseWriter, r *http.Request) {
	owner, ok := keptMachineOwner(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	out := client.MyMachines{Machines: []client.KeptMachine{}, WorstBytes: keptMachineNewWorstBytes}
	ks, ok := s.cfg.Store.(store.KeptMachineStore)
	if !ok {
		out.Reason, out.Limit = reasonKeptMachineSiteOff, types.MaxKeptMachinesPerPersonDefault
		writeJSON(w, http.StatusOK, out)
		return
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeServerError(w, r, "read site config", err)
		return
	}
	mine, err := ks.ListKeptMachinesOf(ctx, owner)
	if err != nil {
		writeServerError(w, r, "list kept machines", err)
		return
	}
	if out.Machines, err = s.keptMachineViews(ctx, ks, mine); err != nil {
		writeServerError(w, r, "read kept machine deletes", err)
		return
	}
	out.Count, out.Limit, out.IdleDays = len(mine), sc.MacOSVM.MaxKeptMachinesPerPersonOrDefault(), sc.MacOSVM.KeptMachineIdleDaysOrDefault()
	if !sc.MacOSVM.KeptMachinesAllowed() {
		out.Reason = reasonKeptMachineSiteOff
	} else {
		ceiling, cerr := s.effectiveCeiling(ctx)
		if cerr != nil {
			ceilingRunRefusal(cerr).write(s, w, r)
			return
		}
		if out.Reason, err = s.keptMachineKeepRefusal(ctx, ks, owner, ceiling, out.Count, out.Limit); err != nil {
			writeServerError(w, r, "read the caller's Macs", err)
			return
		}
	}
	out.Allowed = out.Reason == ""
	writeJSON(w, http.StatusOK, out)
}

// keptMachineKeepRefusal is the reason the caller, under the site switch, may not keep another machine,
// "" when they may, in the create fold's order: the governance profile's deny_kept_machines (no role
// exempt), a Mac of their own outside every other person's pool, then the per-person limit, which still
// lets them resume one.
func (s *Server) keptMachineKeepRefusal(ctx context.Context, ks store.KeptMachineStore, owner string, ceiling governanceCeiling, count, limit int) (string, error) {
	if ceiling.Limits.DenyKeptMachines {
		return string(authz.ReasonGovernanceUnit), nil
	}
	own, err := s.keptMachineOwnMac(ctx, ks, owner)
	switch {
	case err != nil:
		return "", err
	case !own:
		return reasonKeptMachineNotYourMac, nil
	case count >= limit:
		return reasonKeptMachinePersonLimit, nil
	}
	return "", nil
}

// keptMachineOwnMac reports whether owner has a Mac a kept machine may live on: a runner registered to
// them as a person, claimed and not revoked, that no pool but one of their own holds
// (ListRunnerPoolsWithMember), so no other person's run can reach it.
func (s *Server) keptMachineOwnMac(ctx context.Context, ks store.KeptMachineStore, owner string) (bool, error) {
	rs, ok := s.cfg.Store.(interface {
		ListRunnersByOwner(ctx context.Context, owner string) ([]types.Runner, error)
	})
	human := oidcHumanFromContext(ctx)
	if !ok || human == "" {
		return false, nil
	}
	runners, err := rs.ListRunnersByOwner(ctx, human)
	if err != nil {
		return false, err
	}
	for _, rn := range runners {
		if rn.OwnerKind != types.RunnerOwnerPerson || rn.State != types.RunnerClaimed || rn.RevokedAt != nil ||
			types.CanonicalUserSubject(rn.Owner) != owner {
			continue
		}
		pools, err := ks.ListRunnerPoolsWithMember(ctx, rn.ID)
		if err != nil {
			return false, err
		}
		if !slices.ContainsFunc(pools, func(p types.RunnerPool) bool {
			return p.Scope != types.RunnerPoolUser || types.CanonicalUserSubject(p.Owner) != owner
		}) {
			return true, nil
		}
	}
	return false, nil
}

// keptMachineDeleteReason is client.KeptMachine.DeleteReason for a delete's by: an admin's kill deletes
// as an admin does; a by the person never sees (a revoked runner, a delete on the Mac) has none.
func keptMachineDeleteReason(by string) string {
	switch by {
	case keptMachineByOwner:
		return client.KeptMachineDeletedByPerson
	case keptMachineByExpiry:
		return client.KeptMachineDeletedByExpired
	case keptMachineByAdmin, keptMachineByNonOwnerKill:
		return client.KeptMachineDeletedByAdmin
	case keptMachineByErasure:
		return client.KeptMachineDeletedByErasure
	}
	return ""
}

// keptMachineViews is ms on the wire, each with its Mac's name and, while it is being deleted, why. The
// tombstones are read only when one of ms has one.
func (s *Server) keptMachineViews(ctx context.Context, ks store.KeptMachineStore, ms []types.KeptMachine) ([]client.KeptMachine, error) {
	reasons := map[uuid.UUID]string{}
	if slices.ContainsFunc(ms, func(m types.KeptMachine) bool { return m.State == types.KeptMachineDeleting }) {
		deletes, err := ks.ListKeptMachineDeletes(ctx)
		if err != nil {
			return nil, err
		}
		for _, d := range deletes {
			reasons[d.Machine.ID] = keptMachineDeleteReason(d.By)
		}
	}
	names := map[uuid.UUID]string{}
	out := make([]client.KeptMachine, 0, len(ms))
	for _, m := range ms {
		name, ok := names[m.RunnerID]
		if !ok {
			name = s.keptMachineMacName(ctx, m.RunnerID)
			names[m.RunnerID] = name
		}
		v := client.KeptMachine{
			ID: m.ID, Name: m.Name, OwnerSubject: m.OwnerSubject, RunnerID: m.RunnerID, Base: m.Base,
			BasePreparedAt: m.BasePreparedAt, State: m.State, RunID: m.RunID, Scrubbed: m.Scrubbed, DiskBytes: m.DiskBytes,
			CreatedAt: m.CreatedAt, LastUsedAt: m.LastUsedAt, ExpiresAt: m.ExpiresAt, DeleteStartedAt: m.DeleteStartedAt,
			RunnerName: name,
		}
		if m.State == types.KeptMachineDeleting {
			v.DeleteReason = reasons[m.ID]
		}
		out = append(out, v)
	}
	return out, nil
}

// keptMachineMacName is the name of the Mac runnerID, "" when it can't be read.
func (s *Server) keptMachineMacName(ctx context.Context, runnerID uuid.UUID) string {
	rn, err := s.keptMachineRunner(ctx, runnerID)
	if err != nil {
		return ""
	}
	return cmp.Or(rn.Name, rn.MachineName)
}

// handleDeleteMyMachine is DELETE /me/machines/{id}: 204 once the Mac has deleted the machine (or is no
// longer registered, and erases it on its side), 202 with the machine while the delete waits for its Mac
// to come back online, 503 kept_machine_delete_unconfirmed while the Mac's answer is unknown (the delete
// is retried) and 503 kept_machine_delete_failed when the Mac refused it. A machine a run holds is
// refused, 409. Another person's machine and an id that
// names nothing answer the same 404.
func (s *Server) handleDeleteMyMachine(w http.ResponseWriter, r *http.Request) {
	owner, ok := keptMachineOwner(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "kept machine")
	if !ok {
		return
	}
	ks, ok := s.cfg.Store.(store.KeptMachineStore)
	if !ok {
		writeErrorReason(w, http.StatusNotFound, reasonKeptMachineNotFound, keptMachineNotFoundMsg(id.String()))
		return
	}
	d, ok := s.beginKeptMachineDelete(w, r, ks, owner, id, keptMachineByOwner)
	if !ok {
		return
	}
	res := s.deleteKeptMachine(withRequestActor(r), ks, d)
	switch {
	case res.err != nil:
		writeServerError(w, r, "finish kept machine delete", res.err)
	case keptMachineDeleteConfirmed(res.outcome):
		w.WriteHeader(http.StatusNoContent)
	case res.outcome == driveReclaimOutcomeDeferred:
		views, err := s.keptMachineViews(r.Context(), ks, []types.KeptMachine{d.Machine})
		if err != nil {
			writeServerError(w, r, "read kept machine deletes", err)
			return
		}
		writeJSON(w, http.StatusAccepted, views[0])
	case res.outcome == keptMachineOutcomeUnconfirmed:
		writeErrorReason(w, http.StatusServiceUnavailable, reasonKeptMachineDeleteUnconfirmed, keptMachineDeleteUnconfirmedMsg())
	default:
		writeErrorReason(w, http.StatusServiceUnavailable, reasonKeptMachineDeleteFailed,
			keptMachineDeleteFailedMsg(cmp.Or(s.keptMachineMacName(r.Context(), d.Machine.RunnerID), macDiskNoName)))
	}
}

// beginKeptMachineDelete tombstones machine id for by, owner's only (an empty owner: anyone's), and
// writes the refusal when it can't: 404 when there is no such machine, 409 kept_machine_in_use (audited
// as refused) when a run holds it, 409 kept_machine_deleting when a delete has started.
func (s *Server) beginKeptMachineDelete(w http.ResponseWriter, r *http.Request, ks store.KeptMachineStore, owner string, id uuid.UUID, by string) (store.KeptMachineDeletion, bool) {
	d, err := ks.BeginKeptMachineDelete(r.Context(), owner, id, by)
	switch {
	case errors.Is(err, store.ErrNotFound) && owner == "":
		notFoundIf(w, err, "kept machine", reasonKeptMachineNotFound)
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonKeptMachineNotFound, keptMachineNotFoundMsg(id.String()))
	case errors.Is(err, store.ErrKeptMachineInUse):
		if m, gerr := ks.GetKeptMachine(r.Context(), id); gerr == nil {
			s.auditKeptMachineDelete(withRequestActor(r), store.KeptMachineDeletion{Machine: m, By: by}, driveReclaimOutcomeRefused)
		}
		writeErrorReason(w, http.StatusConflict, reasonKeptMachineInUse, keptMachineInUseDeleteMsg())
	case errors.Is(err, store.ErrKeptMachineDeleting):
		name := id.String()
		if m, gerr := ks.GetKeptMachine(r.Context(), id); gerr == nil {
			name = m.Name
		}
		writeErrorReason(w, http.StatusConflict, reasonKeptMachineDeleting, keptMachineDeletingMsg(name))
	case err != nil:
		writeServerError(w, r, "begin kept machine delete", err)
	default:
		return d, true
	}
	return store.KeptMachineDeletion{}, false
}

// handleListKeptMachines is GET /machines/kept: one page of every person's kept machines, by owner then
// name, narrowed by ?owner= (a person's subject) and ?runner= (a runner id). operatorOnly.
func (s *Server) handleListKeptMachines(w http.ResponseWriter, r *http.Request) {
	page, ok := parseListPage(w, r, maxListLimit)
	if !ok {
		return
	}
	f := store.KeptMachineFilter{Owner: types.CanonicalUserSubject(r.URL.Query().Get("owner"))}
	if raw := strings.TrimSpace(r.URL.Query().Get("runner")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			writeErrorReason(w, http.StatusBadRequest, reasonInvalidIDParam, "invalid runner id")
			return
		}
		f.RunnerID = id
	}
	out := client.KeptMachineList{Machines: []client.KeptMachine{}}
	ks, ok := s.cfg.Store.(store.KeptMachineStore)
	if !ok {
		writeJSON(w, http.StatusOK, out)
		return
	}
	ms, total, err := ks.ListKeptMachines(r.Context(), f, page)
	if err != nil {
		writeServerError(w, r, "list kept machines", err)
		return
	}
	if out.Machines, err = s.keptMachineViews(r.Context(), ks, ms); err != nil {
		writeServerError(w, r, "read kept machine deletes", err)
		return
	}
	out.Total = total
	if page.Offset+len(ms) < total {
		w.Header().Set("X-Wardyn-Truncated", "true")
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteKeptMachineAdmin is DELETE /machines/kept/{id}: a super admin deletes a person's kept
// machine and its files. 200 with what the Mac did, 202 while the delete waits for the Mac; a machine a
// run holds is refused, 409. operatorOnly.
func (s *Server) handleDeleteKeptMachineAdmin(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "kept machine")
	if !ok {
		return
	}
	ks, ok := s.cfg.Store.(store.KeptMachineStore)
	if !ok {
		notFoundIf(w, store.ErrNotFound, "kept machine", reasonKeptMachineNotFound)
		return
	}
	d, ok := s.beginKeptMachineDelete(w, r, ks, "", id, keptMachineByAdmin)
	if !ok {
		return
	}
	res := s.deleteKeptMachine(withRequestActor(r), ks, d)
	m := d.Machine
	switch {
	case res.err != nil:
		writeServerError(w, r, "finish kept machine delete", res.err)
	case keptMachineDeleteConfirmed(res.outcome) || res.outcome == driveReclaimOutcomeDeferred:
		status := http.StatusOK
		if res.outcome == driveReclaimOutcomeDeferred {
			status = http.StatusAccepted
		}
		writeJSON(w, status, client.KeptMachineDeleteResult{ID: m.ID, Name: m.Name, Owner: m.OwnerSubject, RunnerID: m.RunnerID, Outcome: res.outcome})
	case res.outcome == keptMachineOutcomeUnconfirmed:
		writeErrorReason(w, http.StatusServiceUnavailable, reasonKeptMachineDeleteUnconfirmed, keptMachineDeleteUnconfirmedMsg())
	default:
		writeErrorReason(w, http.StatusServiceUnavailable, reasonKeptMachineDeleteFailed,
			keptMachineDeleteFailedMsg(cmp.Or(s.keptMachineMacName(r.Context(), m.RunnerID), macDiskNoName)))
	}
}
