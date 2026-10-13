// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The admin side of member drives: a super admin lists and deletes people's
// drives here, and every admin drive, grant and reclaim route refuses a
// person's drive before it writes or reaches the substrate. Destroying a
// person's bytes stays super admin or owner only, the same rail as reclaim.
package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// userDriveMemberOwnedMsg is the admin routes' refusal of a person's drive.
const userDriveMemberOwnedMsg = "This drive belongs to a person: delete it with DELETE /drives/member/{id}. " +
	"It can't be edited, allocated or reclaimed here."

// memberOwnedRefusal writes the 409 when d is a person's drive.
func memberOwnedRefusal(w http.ResponseWriter, d types.UserDrive) bool {
	if d.OwnerSubject == "" {
		return false
	}
	writeErrorReason(w, http.StatusConflict, reasonUserDriveMemberOwned, userDriveMemberOwnedMsg)
	return true
}

// refuseMemberOwned reads drive id and refuses it when it is a person's. An id
// that names nothing passes, so the handler answers it as before; a failed
// read is a 500, never a pass.
func (s *Server) refuseMemberOwned(w http.ResponseWriter, r *http.Request, id uuid.UUID) bool {
	d, err := s.cfg.Store.GetUserDrive(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		writeServerError(w, r, "get user drive", err)
		return true
	}
	return memberOwnedRefusal(w, d)
}

// adminDrivesOnly drops people's drives from GET /drives, which lists the
// drives admins register and allocate.
func adminDrivesOnly(drives []types.UserDriveListItem) []types.UserDriveListItem {
	out := drives[:0:0]
	for _, d := range drives {
		if d.OwnerSubject == "" {
			out = append(out, d)
		}
	}
	return out
}

// handleListMemberDrives is GET /drives/member: one page of every person's
// drives with their owner and storage object. operatorOnly.
func (s *Server) handleListMemberDrives(w http.ResponseWriter, r *http.Request) {
	page, ok := parseListPage(w, r, maxListLimit)
	if !ok {
		return
	}
	out := client.MemberDriveList{Drives: []client.MemberDriveAdminItem{}}
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	if !ok {
		writeJSON(w, http.StatusOK, out)
		return
	}
	drives, total, err := ms.ListMemberDrives(r.Context(), page)
	if err != nil {
		writeServerError(w, r, "list member drives", err)
		return
	}
	for _, d := range drives {
		mount, _ := memberDriveMount(d)
		out.Drives = append(out.Drives, client.MemberDriveAdminItem{
			ID: d.ID, Name: d.Name, Owner: d.OwnerSubject, Backend: d.Backend, SizeMiB: d.SizeMiB,
			StorageClass: d.StorageClass, Object: mount.ObjectName, CreatedAt: d.CreatedAt, Deleting: d.DeleteStartedAt != nil,
		})
	}
	out.Total = total
	if page.Offset+len(drives) < total {
		w.Header().Set("X-Wardyn-Truncated", "true")
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteMemberDriveAdmin is DELETE /drives/member/{id}: a super admin
// deletes a person's drive and its files. 200 with what happened, or 409 with
// the substrate's own refusal, as the reclaim route answers. operatorOnly.
func (s *Server) handleDeleteMemberDriveAdmin(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "member drive")
	if !ok {
		return
	}
	ms, ok := s.cfg.Store.(store.MemberDriveStore)
	if !ok {
		writeErrorReason(w, http.StatusNotFound, reasonUserDriveNotFound, "member drive not found")
		return
	}
	d, started, err := ms.BeginMemberDriveDeleteAdmin(r.Context(), id)
	if notFoundIf(w, err, "member drive", reasonUserDriveNotFound) {
		return
	}
	if errors.Is(err, store.ErrMemberDriveBusy) {
		writeErrorReason(w, http.StatusConflict, reasonMemberDriveBusy, memberDriveBusyMsg())
		return
	}
	if err != nil {
		writeServerError(w, r, "begin member drive delete", err)
		return
	}
	res := s.deleteMemberDrive(r, ms, d, started, memberDriveByAdmin)
	switch {
	case res.err != nil:
		writeServerError(w, r, "finish member drive delete", res.err)
	case res.driverErr != nil:
		writeErrorReason(w, http.StatusConflict, reasonUserDriveReclaimConflict, "delete refused: "+res.driverErr.Error())
	case res.reason == reasonMemberDriveDeleteUnavailable:
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonMemberDriveDeleteUnavailable, memberDriveDeleteUnavailableMsg())
	case res.reason != "":
		writeErrorReason(w, http.StatusServiceUnavailable, reasonMemberDriveDeleteFailed, memberDriveDeleteFailedMsg())
	default:
		writeJSON(w, http.StatusOK, client.MemberDriveDeleteResult{
			Drive: d.Name, DriveID: d.ID, Owner: d.OwnerSubject, Backend: d.Backend, Object: res.object, Outcome: res.outcome,
		})
	}
}
