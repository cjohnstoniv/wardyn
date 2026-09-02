// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The RUN seam for user drives (0.7, migration 0054): turning
// `drive: {enabled, read_only}` on a create-run request into the mount a runner
// executes, or into the one refusal that explains why there is none.
//
// Its own file, beside runs_create.go rather than inside it, for the reason
// runs_dispatch_ceiling.go and runs_create_validate.go are their own files: the
// 1000-line gate, and the fact that a security seam reads better whole than
// scattered through a handler that is already doing eleven other things.
//
// ─── THE THREE REFUSAL SHAPES, AND WHY THEY DIFFER ─────────────────────────
//
// The whole design of this seam is one table, and each row is a decision that
// could have gone the other way:
//
//	the profile's door is shut       403 + authz.denied   denyMemberField
//	the group snapshot is unreadable 403 groups_snapshot_stale
//	everything else                  422, NO audit        denyMemberRunQuota
//
// The 403/422 split is the one denyMemberRunQuota already draws and it is about
// the CALLER, not the severity: a door refusal says "you asked for something
// you may not have", which is an authorization event a SIEM should see. "No
// drive is allocated to you" says the caller is perfectly authorized and there
// is simply nothing to mount — an unmet precondition. Auditing that as
// authz.denied would fill the denial stream with rows about members who did
// nothing wrong, which is how a real denial stops standing out.
//
// ─── ENABLED BUT UNMOUNTABLE IS AN ERROR, NOT A QUIET LAUNCH ───────────────
//
// Every arm below REFUSES the run rather than launching it driveless. The
// alternative reads friendlier and is worse: a member ticks "mount my drive",
// the run starts, the agent works for an hour and writes its output into a
// container layer that is deleted at teardown. Asking for storage and silently
// not getting it is how work is lost, so the request is a REQUIREMENT.
//
// ─── WHERE THE RESOLVED MOUNT LIVES BETWEEN CREATE AND DISPATCH ────────────
//
// On dispatchParams, exactly as the governance ceiling's own dispatch-time
// inputs do (dispatchParams.CeilingDeny / CeilingProfile) — NOT on the run row,
// and there is no migration for it. Three facts decide that, and the third is
// the one that closes the question:
//
//  1. There is no create-then-dispatch WINDOW. handleCreateRun calls
//     dispatchRun inline, in the same request, a few statements after this
//     function runs; nothing in the tree re-dispatches a run later (reconcile
//     FAILS in-flight pre-dispatch runs, it never resumes one). So the grant
//     that resolved here is the grant in force at bind time.
//  2. Re-resolving at dispatch is IMPOSSIBLE anyway, which is why the ceiling
//     does not do it either. Resolution keys on capabilitySubjects — the
//     caller's OIDC sub, email and group snapshot — and the run row carries
//     only CreatedBy, one principal string. There is no stored identity to
//     re-resolve FROM.
//  3. Persisting the request flag on the run row instead would therefore buy
//     nothing readable: a column whose only consumer would have to re-resolve
//     from an identity the row does not carry.
//
// The consequence is stated rather than hidden: revoking a grant does not reach
// INTO a run that is already dispatching. It takes effect on the next run,
// which is the same guarantee reassertCeilingDenies gives for a ceiling, and
// the same one the run's own persisted policy gives for everything else.
package api

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seedRequestDrive resolves the run request's drive flag into the mount the
// runner will execute, writing its own HTTP error and returning ok=false once
// it has responded.
//
// CALLED AFTER validateWorkspaceSources, in handleCreateRun and again in
// handlePreflightRun, and the placement is deliberate on both counts. AFTER,
// because the onboarding gate is the un-bypassable chokepoint on the resolved
// spec and a drive must not be able to answer before it; in PREFLIGHT too,
// because a dry run that previewed a green Review for a launch that will 422 is
// the one thing the preflight handler exists not to do.
//
// It returns nil, true for "no drive was asked for" — the overwhelmingly common
// case, and a provable no-op: no store read, no ceiling read, nothing.
func (s *Server) seedRequestDrive(w http.ResponseWriter, r *http.Request,
	req createRunRequest, ceiling governanceCeiling) (*types.DriveMount, bool) {
	if req.Drive == nil || !req.Drive.Enabled {
		return nil, true
	}
	if s.denyMemberDrive(w, r, ceiling) {
		return nil, false
	}
	// The SAME resolver /me and the admin preview run. A store failure is an
	// ERROR here and not "no drive" — writeDriveError's 500 arm — because
	// mounting nothing where an admin allocated something loses a member's work
	// silently, while a 500 tells them to try again.
	resolved, err := s.resolveUserDrive(r.Context())
	if err != nil {
		writeDriveError(w, err)
		return nil, false
	}
	if resolved == nil {
		writeError(w, http.StatusUnprocessableEntity,
			driveRefusal("no user drive is allocated to you — ask an admin for an allocation"))
		return nil, false
	}
	if resolved.Paused {
		writeError(w, http.StatusUnprocessableEntity,
			driveRefusal("your allocation is paused by an admin"))
		return nil, false
	}
	return s.driveMountFor(w, req, *resolved)
}

// driveDoorProfile names the governance profile whose DenyUserDrive DOOR is
// shut for this caller, or "" when the door is open. ONE predicate, read by
// the enforcement path (denyMemberDrive's 403) and the display path
// (userDriveDeniedByProfile, the /me field) alike: this is an authz rule, and
// two spellings of one authz rule is one place a widening can hide.
//
// KEYED ON ceiling.Profile != nil, the scoping rule every limit in
// denyMemberGovernance follows: an UNASSIGNED member has no profile, so there
// is no door, and a deployment that has never authored one is unaffected.
//
// AND ON !isOperator, which is belt to that braces. denyMemberRequest already
// short-circuits an operator before it resolves a ceiling at all, so the zero
// governanceCeiling an operator carries here has a nil Profile — but the
// operator exemption is the kind of property that should be readable at the
// site it applies to rather than inferred from a caller three files away. An
// operator's drive still RESOLVES; only the door does not apply to them.
func (s *Server) driveDoorProfile(ctx context.Context, ceiling governanceCeiling) (string, bool) {
	if s.isOperator(ctx) || ceiling.Profile == nil || !ceiling.Limits.DenyUserDrive {
		return "", false
	}
	// The DECISION is the bool, never the name: a profile row's name is TEXT NOT
	// NULL UNIQUE with no non-empty CHECK, so returning the name alone made a
	// blank-named profile with DenyUserDrive set read as "no door" and fail OPEN
	// at the enforcement site. The name is display only.
	return ceiling.Profile.Name, true
}

// denyMemberDrive is the DOOR at the enforcement site: 403 with an authz.denied
// row, target `runs.drive`, reason `governance_profile` — the denyMemberField
// shape the two other profile refusals take, and no new value in the closed
// reason enum.
func (s *Server) denyMemberDrive(w http.ResponseWriter, r *http.Request, ceiling governanceCeiling) bool {
	profile, shut := s.driveDoorProfile(r.Context(), ceiling)
	if !shut {
		return false
	}
	// The mock round's frozen member copy, reproduced byte-exact: the console
	// never rewords a server refusal, so this line is where that string ships.
	return s.denyMemberField(w, r, "runs.drive", "governance_profile", fmt.Sprintf(
		"mounting a user drive is not allowed by your governance profile %q. Launch without `drive`.", profile))
}

// driveMountFor folds a resolved drive and the run request into the mount, or
// writes the 422 that says why it cannot.
//
// TWO REFUSALS, and both are the same class: the caller is authorized, the
// allocation exists, and this particular RUN cannot have it.
//
//  1. BACKEND UNAVAILABLE HERE. A drive's backend names exactly one substrate
//     (types.DriveBackend.RunnerTarget), and the write boundary already refuses
//     to author a mismatched one — so this arm only fires for a row that was
//     valid when written and is not now, i.e. a deployment that re-pointed
//     WARDYN_RUNNER. Re-checked rather than trusted because a stale row must
//     not become a mount attempt the driver has no code path for.
//
//  2. THE SHARE IS NOT THERE (host_path only — see driveShareIsBindable).
//
//  3. WIDENING. read_only:false against a read-only allocation is refused
//     rather than ignored, and that is the choice worth naming: silently
//     honouring the allocation would launch a run the member believes is
//     writable, and they would find out when their work failed to persist. The
//     narrow direction (read_only:true on a writable allocation) is always
//     honoured — that is what NARROW-ONLY means, and it matches
//     WorkspaceSelection.ReadOnly exactly.
func (s *Server) driveMountFor(w http.ResponseWriter, req createRunRequest,
	resolved types.ResolvedDrive) (*types.DriveMount, bool) {
	if target := resolved.Drive.Backend.RunnerTarget(); target != s.cfg.RunnerTarget {
		writeError(w, http.StatusUnprocessableEntity, driveRefusal(fmt.Sprintf(
			"this deployment cannot mount your drive (it is a %q drive and this deployment dispatches to %q)",
			resolved.Drive.Backend, s.cfg.RunnerTarget)))
		return nil, false
	}
	if !s.driveShareIsBindable(w, resolved) {
		return nil, false
	}
	readOnly := !resolved.Writable
	if req.Drive.ReadOnly != nil {
		if !*req.Drive.ReadOnly && readOnly {
			writeError(w, http.StatusUnprocessableEntity,
				driveRefusal("your allocation is read-only; `read_only:false` cannot widen it"))
			return nil, false
		}
		readOnly = readOnly || *req.Drive.ReadOnly
	}
	return &types.DriveMount{
		// The row's id, carried so a driver's labels and an offboarding sweep
		// can group by the DRIVE. ObjectName cannot answer that: it is
		// per-principal by construction.
		DriveID:    resolved.Drive.ID,
		Backend:    resolved.Drive.Backend,
		ObjectName: resolved.ObjectName,
		// The provisioner a managed claim asks for, carried so the k8s substrate
		// never has to read the drive row it was resolved from.
		StorageClass: resolved.Drive.StorageClass,
		HomeName:     resolved.HomeName,
		// The resolver's fingerprint of the principal the home was derived from,
		// carried verbatim rather than recomputed. This seam holds the request
		// and the folded allocation, never the claims — and a second derivation
		// from a differently-picked claim is precisely the drift the label the
		// driver stamps from it exists to catch.
		SubjectHash: resolved.SubjectHash,
		// The reserved constant, carried rather than assumed, so the path the
		// runner binds at and the path validateWorkspaceSources/validatePolicySpec
		// refuse to let anyone else name are the SAME symbol.
		Target:      runner.DriveTarget,
		ReadOnly:    readOnly,
		SizeMiB:     resolved.SizeMiB,
		Enforcement: resolved.Enforcement,
	}, true
}

// driveShareIsBindable is driveMountFor's host_path arm: the two facts a share
// bind depends on that the ROW CANNOT CARRY, re-established at the moment of
// the mount. It writes its own 422 and returns false once it has.
//
// It runs for host_path ONLY. Every other backend is either an object Wardyn
// creates on first use (docker_volume, k8s_pvc) or a claim the k8s driver reads
// and refuses by name (k8s_pvc_static — the driver's own bind failure, which is
// why no console string is frozen for it).
//
// ─── (1) THE ENV CEILING, RE-CHECKED ───────────────────────────────────────
//
// handleUpsertUserDrive already applied UserDriveHostRootCheck when this row
// was authored, and that is exactly why it has to run again: the check is over
// WARDYN_USER_DRIVE_HOST_ROOTS and the host filesystem, and neither is in the
// row. A restart with the variable unset, an operator narrowing the roots, or
// the share itself going away all leave a stored drive whose host_root the
// deployment no longer allows — and this is a path bound into OTHER PEOPLE's
// sandboxes. Trusting the row would let the last admin who authored one hold
// the ceiling open across every later boot.
//
// The refusal takes the REFUSED_BACKEND shape rather than a new one, and that
// is the honest reading: from the member's side "the roots moved" and "this
// deployment dispatches elsewhere" are one fact — this deployment cannot mount
// their drive — and the parenthesised reason is where the admin's diagnosis
// goes.
//
// ─── (2) THE DIRECTORY MUST EXIST, AND BE ONE ──────────────────────────────
//
// WARDYN NEVER mkdir's ON A SHARE (DESIGN §3): the tree belongs to whoever owns
// the share, its permissions and quota are theirs, and a control plane that
// created directories there would be authoring on a filesystem it does not own.
// So a missing home is a REFUSAL — and it has to be raised here rather than
// left to the driver, because a bind mount of a non-existent source is one of
// the few places Docker HELPFULLY CREATES IT: an empty root-owned directory
// appears on the operator's share, the run launches, and the member's work goes
// somewhere no admin allocated.
//
// A NON-DIRECTORY is the same refusal for the same reason: binding a regular
// file at the drive target is not a drive, and the sentence a member needs
// ("that directory is not there") is true of both.
//
// ponytail: one os.Stat, on a path already derived, on the create path only.
func (s *Server) driveShareIsBindable(w http.ResponseWriter, resolved types.ResolvedDrive) bool {
	if resolved.Drive.Backend != types.DriveBackendHostPath {
		return true
	}
	if err := s.userDriveHostRootCheck()(resolved.Drive.HostRoot); err != nil {
		writeError(w, http.StatusUnprocessableEntity, driveRefusal(fmt.Sprintf(
			"this deployment cannot mount your drive (%s)", err)))
		return false
	}
	// The HOME name, never the resolved path: the member is told which
	// directory is missing, and the operator's filesystem layout stays where
	// GET /drives already keeps it (a boolean, not the roots).
	//
	// %s AND NOT %q, and the doc's backticks are not typed here. §7's header
	// note makes a backticked substring a MONO SPAN the console applies, never
	// characters in the string — so a PLACEHOLDER the doc backticks ships bare
	// (this one, and it is what DRIVE_MEMBER.REFUSED_HOME_MISSING carries),
	// while a placeholder the doc QUOTES ships %q (DENIED_DRIVE's profile name)
	// and a backticked LITERAL keeps its backticks (REFUSED_WRITABLE's
	// `read_only:false`, REFUSED_HOME_INVALID's `. _ -`).
	if st, err := os.Stat(resolved.ObjectName); err != nil || !st.IsDir() {
		writeError(w, http.StatusUnprocessableEntity, driveRefusal(fmt.Sprintf(
			"directory %s does not exist on the share — ask an admin to create it", resolved.HomeName)))
		return false
	}
	return true
}
