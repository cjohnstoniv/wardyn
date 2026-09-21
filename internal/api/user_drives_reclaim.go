// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// POST /drives/{id}/reclaim — the one verb in the product that DESTROYS a
// member's stored bytes.
//
// Its own file rather than a ninth handler in user_drives.go, for the reason
// user_drives_preview.go is its own file: that one is at the 1000-line ceiling
// scripts/check-file-size.sh holds, and the allowlist is for pre-existing files
// only. The seam is also the right one — everything here is the destroy path
// and its refusals, and nothing else in the family shares them.
//
// WHY A VERB EXISTS AT ALL. Deleting a drive removed its row and left the
// volume or the claim on the substrate with nothing in the product able to name
// it afterwards, so the storage a departing person left behind was reclaimed by
// hand off a `POST /drives/preview` answer or not at all (#166). The verb takes
// the guess out of it.
//
// WHY IT IS FENCED THE WAY IT IS. Four rails, and each one is load-bearing:
//
//  1. On Kubernetes wardynd does not even hold the verb unless an operator
//     grants it: the chart's Role carries `persistentvolumeclaims: [get,
//     create]` and adds `delete` only under `userDrives.reclaim.enabled`
//     (deploy/helm/wardyn/templates/rbac.yaml), so a stock install answers the
//     apiserver's own 403 underneath everything below.
//  2. SUPER-ADMIN ONLY, like every other /drives route and for a sharper
//     reason: this one is irreversible.
//  3. A `drive.reclaim` audit row per attempt — success, refusal and failure
//     alike — naming the drive, the person, the backend, the object and what
//     happened to it. A destructive act with no durable record of WHICH bytes
//     went is not auditable after the fact, because the row it describes is
//     gone from the database too.
//  4. NO CONSOLE BUTTON. A destructive confirmation is a screen, and this one
//     has no approved mock (docs/design/CONSOLE-RULES.md's mock-first rule), so
//     the surface is the API and the CLI.
//
// The org switch (`storage.user_drive.disabled`) is deliberately NOT a gate
// here, unlike on every mount-shaped surface in this family. Turning drives off
// deployment-wide is exactly when an operator needs to clean the objects up,
// and a switch that blocked the cleanup would strand the bytes it was flipped
// to stop using.
package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mountUserDriveFamily registers the WHOLE /drives family on the super-admin
// group: the seven routes user_drives.go owns, plus the destroy verb below.
//
// Not a ninth line in mountUserDriveRoutes, because user_drives.go is at its
// size ceiling (scripts/check-file-size.sh) and the handler lives here, where
// its registration belongs beside it. Not a second call in routes() either:
// that function is at its own funlen ceiling, so the family gets one call site
// and this is where the family's two halves are joined.
func (s *Server) mountUserDriveFamily(operatorOnly chi.Router) {
	s.mountUserDriveRoutes(operatorOnly)
	operatorOnly.Post("/drives/{id}/reclaim", s.handleReclaimUserDrive)
}

// userDriveReclaimRequest names the ALLOCATION whose object is to be
// destroyed, on the same natural key `drive.grant.delete` records:
// (subject_type, subject).
//
// The pair rather than the subject alone, because the pair is what an operator
// reads back out of the offboarding trail — the grant-delete row that recorded
// the product-side half of the same offboarding carries exactly these two
// fields, and a reclaim row that carried only one of them could not be joined
// to it.
type userDriveReclaimRequest struct {
	SubjectType types.CapabilitySubjectType `json:"subject_type"`
	Subject     string                      `json:"subject"`
}

// userDriveReclaimResponse is what the caller gets back: the same seven facts
// the audit row carries, so a CLI can print what happened without a second
// read, and so the two can never describe the act differently.
type userDriveReclaimResponse struct {
	Drive       string                      `json:"drive"`
	DriveID     uuid.UUID                   `json:"drive_id"`
	SubjectType types.CapabilitySubjectType `json:"subject_type"`
	Subject     string                      `json:"subject"`
	Backend     types.DriveBackend          `json:"backend"`
	Object      string                      `json:"object"`
	Outcome     string                      `json:"outcome"`
}

// The four outcomes a reclaim attempt can record. The first two are the
// substrate's own answers (runner.DriveReclaimOutcome) and ride an event whose
// Outcome is `success`; the last two ride a `failure` event.
//
// They are a DATA field as well as the event's own outcome because the event's
// outcome answers "did the call work" and this answers "what happened to the
// bytes" — and `deleted` versus `already_absent` is the difference between a
// row that proves a person's storage was destroyed and one that proves it was
// already gone when we looked. An operator auditing an offboarding needs the
// second question answered, and only this field answers it.
const (
	driveReclaimOutcomeRefused = "refused"
	driveReclaimOutcomeFailed  = "failed"
)

// driveReclaimTimeout bounds the ONE substrate call this handler makes. A
// request thread must not hang on a wedged Docker daemon or an unreachable
// apiserver, and an operator who does not get an answer in this long has an
// unhealthy substrate rather than a slow delete — both calls are a single
// object write.
const driveReclaimTimeout = 30 * time.Second

// handleReclaimUserDrive destroys the storage object one allocation resolved
// to, and records what it destroyed. 200 with the outcome, or:
//
//	400  the request cannot name an object (a bad subject, a tier that has no
//	     single object, a home the drive's template cannot derive)
//	404  no such drive
//	409  the substrate refused: the object is not this drive's, a reclaim is
//	     already in flight, or a run still holds it
//	422  this drive's backend allocates nothing this deployment may destroy
//	501  the wired runner cannot reclaim at all
//	500  anything else
//
// operatorOnly (routes.go).
func (s *Server) handleReclaimUserDrive(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "user drive")
	if !ok {
		return
	}
	var req userDriveReclaimRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	// The WRITE BOUNDARY's own validator, not a second copy of subject
	// hygiene: ValidateUserDriveGrant is what folded the subject when the
	// allocation was stored (CanonicalUserSubject / CanonicalGroupSubject, and
	// never a bare ToLower — see its own comment for the fold-escalation that
	// rule exists to stop). Reusing it is what makes the subject looked up here
	// the subject stored there; hand-folding would be the third spelling of a
	// rule that has already drifted twice.
	g := types.UserDriveGrant{SubjectType: req.SubjectType, Subject: req.Subject, DriveID: id}
	if err := types.ValidateUserDriveGrant(&g); err != nil {
		writeError(w, http.StatusBadRequest, "invalid reclaim request: "+err.Error())
		return
	}
	if g.SubjectType != types.CapabilitySubjectUser {
		// A group or `all` allocation gives EVERY person it matches their own
		// home and their own object — that is the whole point of a per-user
		// drive — so the tier names no single object to destroy. Refused rather
		// than resolved to something: the two ways a destructive verb could
		// answer this are "guess one" and "delete them all", and neither is a
		// thing a machine may decide. Reclaim the people one at a time.
		writeError(w, http.StatusBadRequest, "subject_type: a reclaim names one person's storage — a "+
			string(g.SubjectType)+" allocation gives each person their own object, so reclaim them one subject at a time")
		return
	}
	d, err := s.cfg.Store.GetUserDrive(r.Context(), id)
	if notFoundIf(w, err, "user drive") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get user drive: "+err.Error())
		return
	}
	if code, msg := driveReclaimableHere(d, s.cfg.RunnerTarget); msg != "" {
		writeError(w, code, msg)
		return
	}
	object, code, msg := s.driveReclaimObject(r.Context(), d, g)
	if msg != "" {
		writeError(w, code, msg)
		return
	}
	reclaimer, ok := s.cfg.Runner.(runner.DriveReclaimer)
	if !ok {
		// 501, the same verdict handleRunFiles gives for a capability the wired
		// runner does not have: a permanent fact about this deployment, not a
		// blip to retry. Nothing is audited — no attempt reached the storage.
		writeError(w, http.StatusNotImplemented, "this deployment's runner cannot reclaim drive storage; "+
			"remove the object with the substrate command in docs/OPERATIONS.md instead")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), driveReclaimTimeout)
	defer cancel()
	outcome, err := reclaimer.ReclaimDrive(ctx, types.DriveMount{
		DriveID:      d.ID,
		Backend:      d.Backend,
		ObjectName:   object,
		HostRoot:     d.HostRoot,
		DriveName:    d.Name,
		StorageClass: d.StorageClass,
		HomeName:     driveHomeOf(object, d),
		SubjectHash:  types.DriveSubjectHash(g.Subject),
		Target:       runner.DriveTarget,
	})
	if err != nil {
		status, recorded := http.StatusInternalServerError, driveReclaimOutcomeFailed
		if errors.Is(err, runner.ErrDriveInUse) || errors.Is(err, runner.ErrDriveNotReclaimable) {
			status, recorded = http.StatusConflict, driveReclaimOutcomeRefused
		}
		s.auditDriveReclaim(r, d, g, object, recorded, false)
		writeError(w, status, "reclaim refused: "+err.Error())
		return
	}
	s.auditDriveReclaim(r, d, g, object, string(outcome), true)
	writeJSON(w, http.StatusOK, userDriveReclaimResponse{
		Drive: d.Name, DriveID: d.ID, SubjectType: g.SubjectType, Subject: g.Subject,
		Backend: d.Backend, Object: object, Outcome: string(outcome),
	})
}

// driveReclaimableHere answers whether THIS deployment may destroy anything for
// THIS drive's backend, before a subject is ever resolved. Empty msg is yes.
//
// Two refusals, and the first is the one that matters most:
//
//   - A SHARE backend allocates nothing. A host_path home is a directory inside
//     a tree the operator mounted and named, and a k8s_pvc_static claim is an
//     admin's pre-provisioned handle on a real corporate export — Wardyn
//     created neither and must delete neither. There is no recursive delete in
//     this product, at any privilege, for any backend, and this is where that
//     is enforced rather than merely intended.
//   - A backend this deployment does not dispatch to has no substrate here to
//     ask. The same check the run path makes (DriveBackend.RunnerTarget), for
//     the same reason: a k8s drive on a Docker install names an object no local
//     daemon has ever heard of, and the honest answer is "not here", never a
//     delete aimed at a name that might match something else.
func driveReclaimableHere(d types.UserDrive, runnerTarget string) (int, string) {
	if d.Backend.Kind() != types.DriveKindManaged {
		return http.StatusUnprocessableEntity, "this drive's storage is not Wardyn's to destroy: a " +
			string(d.Backend) + " drive binds an object an administrator provisioned, so reclaiming it is a change " +
			"on the share itself (docs/OPERATIONS.md), never a call to this API"
	}
	if runnerTarget != "" && d.Backend.RunnerTarget() != runnerTarget {
		return http.StatusUnprocessableEntity, "this drive's backend (" + string(d.Backend) +
			") is not mounted by this deployment's runner (" + runnerTarget + "), so there is no substrate here to reclaim it from"
	}
	return 0, ""
}

// driveReclaimObject derives the object name one allocation resolved to, which
// is the string the substrate is then asked to destroy. Empty msg is success.
//
// THE HOME OVERRIDE IS THE WHOLE DIFFICULTY. An object name is
// `wardyn-drive-<slug>-<home>`, and the home is either derived from the person
// (the drive's template) or PINNED on their allocation (`home_override`) — so
// deriving one without reading the other names a different object, and this is
// the one caller where naming the wrong object destroys somebody's work.
//
// The grants read is therefore FAIL-CLOSED: a read error is a 500, never a
// fall-through to the template's answer. "I could not find out whether a
// directory name was pinned" must never become "there wasn't one".
//
// A MISSING allocation is not an error, and that is the case the verb exists
// for: offboarding deletes the allocation (`drive.grant.delete`) and leaves the
// object behind, so by the time anyone reclaims it the row is routinely gone.
// What that costs is stated rather than hidden — with the allocation gone, a
// PINNED home cannot be recovered from the database and the derived name is the
// template's. docs/OPERATIONS.md's runbook says to reclaim BEFORE deleting the
// allocation for exactly that reason, and the object name the response and the
// audit row carry is what an operator checks against the preview.
func (s *Server) driveReclaimObject(ctx context.Context, d types.UserDrive, g types.UserDriveGrant) (string, int, string) {
	grants, err := s.cfg.Store.ListUserDriveGrants(ctx)
	if err != nil {
		return "", http.StatusInternalServerError, "list user drive allocations: " + err.Error()
	}
	override := ""
	for _, existing := range grants {
		if existing.DriveID == d.ID && existing.SubjectType == g.SubjectType && existing.Subject == g.Subject {
			override = existing.HomeOverride
			break
		}
	}
	home, err := types.DriveHomeName(d, g.Subject, override)
	if err != nil {
		return "", http.StatusBadRequest, "this allocation resolves to no directory name, so there is no object to reclaim: " + err.Error()
	}
	return types.DriveObjectName(d, home), 0, ""
}

// driveHomeOf recovers the home segment from an object name for the mount the
// substrate is handed, so the driver's own label/log lines name the directory
// rather than an empty string. types.DriveObjectName is
// `wardyn-drive-<slug>-<home>` on a managed backend, and this handler refuses
// every other kind, so the prefix is always present.
func driveHomeOf(object string, d types.UserDrive) string {
	prefix := types.DriveObjectName(d, "")
	if len(object) > len(prefix) {
		return object[len(prefix):]
	}
	return ""
}

// auditDriveReclaim writes the `drive.reclaim` row — on every attempt, not only
// the ones that destroyed something.
//
// A REFUSED reclaim is as much a governance fact as a successful one: it is a
// super-admin asking for a named person's storage to be destroyed, and the row
// is what says whether it was. Recording only successes would leave the one
// case an incident review cares about — somebody tried, repeatedly, and the
// substrate said no — invisible.
//
// The fields mirror `drive.grant.delete`'s (the product-side half of the same
// offboarding) so the two rows join on (drive_id, subject_type, subject), plus
// the three this act needs and that one does not have: which substrate object
// was addressed, on which backend, and what became of it.
func (s *Server) auditDriveReclaim(r *http.Request, d types.UserDrive, g types.UserDriveGrant, object, outcome string, ok bool) {
	result := "failure"
	if ok {
		result = "success"
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"drive.reclaim", d.ID.String(), result, mustJSON(map[string]any{
			"backend":      d.Backend,
			"drive":        d.Name,
			"drive_id":     d.ID,
			"object":       object,
			"outcome":      outcome,
			"subject":      g.Subject,
			"subject_type": g.SubjectType,
		})))
}
