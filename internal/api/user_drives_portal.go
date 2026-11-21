// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Allocating a team drive to an issuer portal (types.DriveSubjectPortal). A super admin's write only, additive only,
// held to the drive's and the portal's state inside the store's write transaction. The portal rows themselves are
// read by one door only, run_drives_portal.go.

// portalDriveAllocationAdminMsg is the 403 a security admin gets for a portal allocation.
const portalDriveAllocationAdminMsg = "Only a super admin may allocate a drive to a portal"

// upsertPortalDriveGrant is POST /drives/grants' write for a portal subject, in upsertUserDriveGrantAs' shape: done
// means the response is written. It never takes the replacing or the tier-judged path: a replacing write deletes the
// subject's other rows, which would silently drop every other drive the portal holds, and the tier path judges a
// narrowing the portal rule does not have.
func (s *Server) upsertPortalDriveGrant(w http.ResponseWriter, r *http.Request, g types.UserDriveGrant, _, additional bool) (saved types.UserDriveGrant, removed []uuid.UUID, done bool, err error) {
	switch {
	case !s.isOperator(r.Context()):
		s.refuse(w, r, authz.Deny(authz.ReasonAdminSurface, "drive.grant.portal", portalDriveAllocationAdminMsg))
		return saved, nil, true, nil
	case !additional:
		writeErrorReason(w, http.StatusBadRequest, reasonUserDriveGrantInvalid,
			"invalid allocation: a portal holds one allocation per drive; send additional: true")
		return saved, nil, true, nil
	}
	// Parsed before the write, so nothing after the commit can fail: the grant is committed and audited whatever follows.
	portal, perr := uuid.Parse(g.Subject)
	if perr != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonUserDriveGrantInvalid, "invalid allocation: subject: a portal is named by its id")
		return saved, nil, true, nil
	}
	saved, _, err = s.writeUserDriveGrant(r.Context(), g, false, true)
	if s.portalDriveGrantRefused(w, err) {
		return saved, nil, true, nil
	}
	if err != nil {
		return saved, nil, false, err
	}
	// The folder's object name, so the super admin can create the claim before the first run. It is a function of the
	// portal and the drive's id alone, so no second read of the drive.
	saved.ObjectName = types.DrivePortalObjectName(types.UserDrive{ID: saved.DriveID}, types.DrivePortalHome(types.UserDrive{ID: saved.DriveID}, portal))
	return saved, nil, false, nil
}

// portalDriveGrantRefused answers the store's refusals of a portal allocation, each with its own reason, and reports
// whether it wrote one. Nothing was written.
func (s *Server) portalDriveGrantRefused(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrDrivePortalNotTeam):
		writeErrorReason(w, http.StatusBadRequest, reasonUserDriveGrantInvalid, "invalid allocation: subject_type: a portal allocation is for a team drive only")
	case errors.Is(err, store.ErrDrivePortalBackend):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDrivePortalBackend,
			"a portal's folder needs a drive whose storage Wardyn names (docker_volume or k8s_pvc_static)")
	case errors.Is(err, store.ErrDrivePortalSecondPlace):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDrivePortalSecondPlace,
			"a drive with a second place cannot be allocated to a portal: on the runner that holds the second place the folder would be a host folder, not the portal's folder")
	case errors.Is(err, store.ErrDrivePortalNotIssuer):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDrivePortalNotIssuerPortal,
			"only a portal with its own identity issuer can be allocated a team drive; a console portal's people use group allocations")
	case errors.Is(err, store.ErrDrivePortalNotLive):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDrivePortalNotLive,
			"this portal is unknown or has been revoked; allocate to a portal that is registered and live")
	case errors.Is(err, store.ErrDrivePortalAdditiveOnly):
		writeErrorReason(w, http.StatusBadRequest, reasonUserDriveGrantInvalid,
			"invalid allocation: a portal holds one allocation per drive; send additional: true")
	default:
		return false
	}
	return true
}

// portalDriveEditRefused answers a drive edit the store refused because a portal holds the drive and the edit would
// add a second place, move it off docker_volume or k8s_pvc_static, or unmark it as a team drive, and reports whether it wrote the answer. Nothing
// was written.
func portalDriveEditRefused(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, store.ErrDrivePortalNotTeam):
		writeErrorReason(w, http.StatusConflict, reasonUserDriveRehomeInvalid,
			"a portal is allocated this drive, so it must stay a team drive: remove the portal's allocation first")
	case errors.Is(err, store.ErrDrivePortalBackend):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDrivePortalBackend,
			"a portal is allocated this drive, and a portal's folder needs docker_volume or k8s_pvc_static: remove the portal's allocation first")
	case errors.Is(err, store.ErrDrivePortalSecondPlace):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDrivePortalSecondPlace,
			"a portal is allocated this drive, and a drive with a second place cannot be a portal's folder: remove the portal's allocation first")
	default:
		return false
	}
	return true
}

// driveGrantConflictMsg is the ONE store.ErrConflict this write can raise, told
// apart by the request's own tri-state rather than by a second read.
//
// The two causes are mutually exclusive by construction, which is what makes
// this a decision and not a guess (see PG.UpsertUserDriveGrant): the
// uniqueness guard short-circuits on an EMPTY home_override and an unstated one
// is always empty, while the re-home guard is disabled outright by a stated
// one. So exactly one of them can have fired.
//
// The RE-HOME half takes 409 for driveRehomeGuard's reason — "the state of this
// resource makes that unsafe" — and names the act rather than the field, because
// an admin who sent a priority edit has no idea they were about to move
// somebody's storage. Re-sending the name they want is the confirmation; "" is
// how they clear it deliberately.
//
// The UNIQUENESS half names NO subject, for handleDeleteUserDrive's reason: the
// allocations table the admin is looking at already lists every override, and a
// name read here would be a second, later answer from a read the refusal does
// not need.
func driveGrantConflictMsg(g types.UserDriveGrant, homeOverrideStated bool) string {
	if !homeOverrideStated {
		return "this allocation pins a directory name and your request did not mention home_override — writing it " +
			"would CLEAR that name, so this subject's next run would mount a different object and the one holding " +
			"their work would be left behind with nothing in Wardyn naming it. Re-send with home_override set to " +
			"the name you want kept, or to \"\" to drop it deliberately"
	}
	return fmt.Sprintf("another allocation on this drive already uses the "+
		"directory name %q — a directory name is one person's, which is why a group allocation may not "+
		"carry one; pick a different name or remove the allocation that holds it", g.HomeOverride)
}
