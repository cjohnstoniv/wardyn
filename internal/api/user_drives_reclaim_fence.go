// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The reclaim fence (drive_reclaims): the destroy verb claims the one object it is about to destroy before it asks
// the storage, and a run attaches no claimed object.

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reclaimableObject reports whether a mount's object is one POST /drives/{id}/reclaim may destroy: only those are
// fenced and held.
func reclaimableObject(b types.DriveBackend) bool {
	return b == types.DriveBackendDockerVolume || b == types.DriveBackendK8sPVC
}

// reclaimFenced is the reclaim from its claim on: claim the object, re-check who may hold it, make the one substrate
// call, settle the claim by what the call proved, and answer. It runs to its end even if the caller goes away (the
// context is detached first, so a disconnect cannot cancel the claim's own statement), so the claim is always settled
// or left fencing, and the attempt is audited. A settle that fails leaves the object fenced and the answer says so.
func (s *Server) reclaimFenced(w http.ResponseWriter, r *http.Request, fs store.DriveReclaimFenceStore, rc runner.DriveReclaimer,
	d types.UserDrive, g types.UserDriveGrant, object, homeSubject string) {
	r = r.WithContext(context.WithoutCancel(r.Context()))
	ctx := r.Context()
	started, claimN, err := fs.BeginDriveReclaim(ctx, d.Backend, object, d.ID)
	if errors.Is(err, store.ErrDriveReclaimBusy) {
		s.auditDriveReclaim(r, d, g, object, driveReclaimOutcomeRefused, false)
		writeErrorReason(w, http.StatusConflict, reasonUserDriveReclaimConflict, "reclaim refused: another reclaim of this object started less than a minute ago")
		return
	}
	if err != nil {
		s.auditDriveReclaim(r, d, g, object, driveReclaimOutcomeFailed, false)
		writeServerError(w, r, "claim the object to reclaim", err)
		return
	}
	// settled reports whether the claim's settle statement left the fence as intended; any error but ErrNotFound
	// (the claim was taken over, and its taker settles it) leaves the object fenced, and the answer must say so.
	settled := func(what string, err error) bool {
		if err == nil || errors.Is(err, store.ErrNotFound) {
			return true
		}
		slog.ErrorContext(ctx, "api: "+what+" a drive reclaim's claim", slog.String("object", object), slog.String("error", err.Error()))
		return false
	}
	fenceLeft := func() {
		writeErrorReason(w, http.StatusInternalServerError, reasonUserDriveReclaimFailed,
			"the reclaim's mark on the object could not be updated, so runs are still refused it: run the same reclaim again after a minute")
	}
	// SECURITY: a run that re-reads its drives after the claim meets it (holdReclaimableDrives, driveReclaimFailure);
	// one that re-read before it is seen here. One of this person's runs still STARTING may have checked the object and
	// not yet mounted it, as a member drive's delete counts it (deleteMemberDrive). A run of anyone's that has not ended
	// and was dispatched with this object holds it, whoever's allocation named it. Each runner also refuses, at the
	// mount, an object that is no longer the one it checked. A store that cannot answer refuses the reclaim.
	starting, err := true, error(nil)
	if st, ok := s.cfg.Store.(interface {
		MemberDriveOwnerStarting(ctx context.Context, owner string, leaseFresh time.Duration) (bool, error)
	}); ok {
		starting, err = st.MemberDriveOwnerStarting(ctx, homeSubject, watcherLeaseStaleAfter)
	}
	held := false
	if err == nil && !starting {
		held, err = fs.DriveObjectHeld(ctx, d.Backend, object)
	}
	if err != nil || starting || held {
		cleared := settled("clear", fs.ClearDriveReclaim(ctx, d.Backend, object, started, claimN))
		auditOutcome := driveReclaimOutcomeRefused
		if err != nil {
			auditOutcome = driveReclaimOutcomeFailed
		}
		s.auditDriveReclaim(r, d, g, object, auditOutcome, false)
		switch {
		case !cleared:
			fenceLeft()
		case err != nil:
			writeServerError(w, r, "read the runs that may hold the object", err)
		case starting:
			writeErrorReason(w, http.StatusConflict, reasonUserDriveReclaimConflict, "reclaim refused: one of this person's runs is starting")
		default:
			writeErrorReason(w, http.StatusConflict, reasonUserDriveReclaimConflict, "reclaim refused: a run that has not ended holds this object")
		}
		return
	}
	issued, err := fs.IssueDriveReclaim(ctx, d.Backend, object, started, claimN)
	if errors.Is(err, store.ErrNotFound) {
		// Another reclaim took the claim over after a minute: it makes the call, this one makes none.
		s.auditDriveReclaim(r, d, g, object, driveReclaimOutcomeRefused, false)
		writeErrorReason(w, http.StatusConflict, reasonUserDriveReclaimConflict, "reclaim refused: another reclaim of this object took it over")
		return
	}
	if err != nil {
		cleared := settled("clear", fs.ClearDriveReclaim(ctx, d.Backend, object, started, claimN))
		s.auditDriveReclaim(r, d, g, object, driveReclaimOutcomeFailed, false)
		if !cleared {
			fenceLeft()
			return
		}
		writeServerError(w, r, "record the reclaim's delete", err)
		return
	}
	sctx, cancel := context.WithTimeout(ctx, driveReclaimTimeout)
	defer cancel()
	outcome, rerr := rc.ReclaimDrive(sctx, types.DriveMount{
		DriveID:      d.ID,
		Backend:      d.Backend,
		ObjectName:   object,
		HostRoot:     d.HostRoot,
		DriveName:    d.Name,
		StorageClass: d.StorageClass,
		HomeName:     driveHomeOf(object, d),
		SubjectHash:  types.DriveSubjectHash(homeSubject),
		Target:       runner.DriveTarget,
	})
	switch {
	case rerr == nil && (outcome == runner.DriveReclaimDeleted || outcome == runner.DriveReclaimAlreadyAbsent):
		finished := settled("finish", fs.FinishDriveReclaim(ctx, d.Backend, object, started, issued))
		s.auditDriveReclaim(r, d, g, object, string(outcome), true)
		if !finished {
			fenceLeft()
			return
		}
		writeJSON(w, http.StatusOK, userDriveReclaimResponse{
			Drive: d.Name, DriveID: d.ID, SubjectType: g.SubjectType, Subject: g.Subject,
			Backend: d.Backend, Object: object, Outcome: string(outcome),
		})
	case rerr == nil:
		// An outcome this server does not know is not evidence that the object is gone: it stays fenced.
		s.auditDriveReclaim(r, d, g, object, driveReclaimOutcomeFailed, false)
		writeServerError(w, r, "reclaim drive", fmt.Errorf("the substrate answered outcome %q", outcome))
	case errors.Is(rerr, runner.ErrDriveInUse) || errors.Is(rerr, runner.ErrDriveUntouched):
		// Refused before anything changed: the claim goes, unless it was taken over a call that may still be
		// outstanding. These wrap OUR OWN sentinel errors; the drivers add the object's name and, on Kubernetes, the
		// holding pod's name (wardyn-agent-<run id>); both are already in the 200 body and the audit row, and the
		// route is super-admin only, so the body crosses no trust boundary.
		cleared := settled("clear", fs.ClearDriveReclaimIssued(ctx, d.Backend, object, started, claimN, issued))
		s.auditDriveReclaim(r, d, g, object, driveReclaimOutcomeRefused, false)
		if !cleared {
			fenceLeft()
			return
		}
		writeErrorReason(w, http.StatusConflict, reasonUserDriveReclaimConflict, "reclaim refused: "+rerr.Error())
	case errors.Is(rerr, runner.ErrDriveNotReclaimable):
		// A bare one (a Kubernetes claim already Terminating) does not say nothing was touched: the object stays fenced.
		s.auditDriveReclaim(r, d, g, object, driveReclaimOutcomeRefused, false)
		writeErrorReason(w, http.StatusConflict, reasonUserDriveReclaimConflict, "reclaim refused: "+rerr.Error())
	default:
		// The call was made and its outcome is not known (the time-out, a daemon or API error): the object stays fenced
		// until a later reclaim answers deleted or already_absent.
		s.auditDriveReclaim(r, d, g, object, driveReclaimOutcomeFailed, false)
		writeServerError(w, r, "reclaim drive", rerr)
	}
}
