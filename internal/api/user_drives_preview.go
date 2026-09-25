// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// POST /drives/preview — the ADMIN's answer to "whose drive resolves for these
// claims, and would it actually mount".
//
// Split out of user_drives_resolve.go when that file crossed the 1000-line
// ceiling scripts/check-file-size.sh holds, at the seam its own banner already
// drew: everything here is the preview ENDPOINT (its response shape, its
// warning, its three gates), and everything left behind is the resolver the
// launch path and GET /me share with it. The parity guard
// (TestDriveDoorsAnswerOverTheSameSentinels) reads writeDriveError and
// driveUnavailableReason out of the resolver file, which is why those two stayed
// there rather than travelling with the surface that writes one of them.
package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// POST /drives/preview

// userDrivePreviewResponse names the drive a principal carrying those claims
// would mount, the TIER of the grant that won, and — the field an admin
// actually came for — the exact OBJECT NAME, so the offboarding runbook's
// `docker volume rm` / `kubectl delete pvc` can be copied rather than computed
// from a hash by hand.
//
// Every field is omitempty and an empty object is the answer for "no grant
// matched", the same additive/absent doctrine governancePreviewResponse
// follows: an absent key already decodes as "no drive" in the TS mirror, while
// "" would be a value the console then has to special-case.
type userDrivePreviewResponse struct {
	DriveName   string                      `json:"drive_name,omitempty"`
	MatchedTier types.CapabilitySubjectType `json:"matched_tier,omitempty"`
	HomeName    string                      `json:"home_name,omitempty"`
	ObjectName  string                      `json:"object_name,omitempty"`
	SizeMiB     int                         `json:"size_mib,omitempty"`
	Writable    bool                        `json:"writable,omitempty"`
	Enforcement types.StorageEnforcement    `json:"enforcement,omitempty"`
	// Paused: the matched allocation is disabled, so nothing above it is derived
	// and nothing would mount.
	Paused bool `json:"paused,omitempty"`
	// HomeSubject is WHICH of the submitted claims the home above was derived
	// from — driveHomeSubject's positional pick, made visible.
	//
	// It exists because this endpoint's request cannot label a claim: the
	// console sends one kind-less box, and the resolver reads position (the
	// caller's stable subject first, their email last). So an admin who pasted
	// only an address against a `hash` or `sub` drive gets a perfectly
	// well-formed object name that no run will ever mount — derived from the
	// address, where the run derives from the sign-in subject. Naming the claim
	// the answer keys on is what makes that visible instead of confidently
	// wrong. Absent on a paused row, where nothing is derived at all.
	HomeSubject string `json:"home_subject,omitempty"`
	// Warning is a SERVER-composed hint about the REQUEST, not a refusal and not
	// canon: the preview is a typing surface, and the shape above is the mistake
	// it is easiest to make and hardest to notice. It is deliberately NOT one of
	// §7's frozen strings — those are the member's doors, and this is an
	// admin's typo — so the console may render it verbatim without a new key.
	Warning string `json:"warning,omitempty"`
}

// drivePreviewWarning is the one request-shape hint the preview offers: the
// answer keys on the SIGN-IN SUBJECT and the admin appears to have pasted an
// address first.
//
// Keyed on `hash`/`sub` only, because those are the two templates
// driveHomeSubject answers with users[0]; `email_local` reads the LAST claim,
// so an address-first preview is exactly right for it and a warning there would
// train an admin to ignore the field.
//
// It looks at the claim's SHAPE, which the resolver itself refuses to do — and
// that difference is the point rather than an inconsistency. A shape guess
// inside the resolver would be a second opinion about identity competing with
// the ordering it ranks by; a shape guess on an advisory string competes with
// nothing. It never changes the answer above it.
func drivePreviewWarning(tmpl types.HomeTemplate, users []string) string {
	if tmpl == types.HomeTemplateEmailLocal || len(users) == 0 || !strings.Contains(users[0], "@") {
		return ""
	}
	return "the directory name keys on the sign-in subject; paste it first"
}

// handlePreviewUserDrive answers "which drive would a principal carrying these
// claims mount, and what would it be called" by running THE resolver — the
// identical resolveUserDriveFor call the enforcement path takes.
//
// That single call is the whole point, and it is the lesson the governance
// preview already paid for: its first cut re-implemented the ORDER BY in
// TypeScript, and a second implementation of the precedence rule is a second
// implementation of the answer. A preview that drifts from enforcement is worse
// than no preview — it is confidently wrong at the moment an admin is deciding
// whether an allocation is right. So there is no ORDER BY in Go here, none in
// TS, and no second copy of the home derivation either.
//
// It reuses governancePreviewRequest and normalizeGovernancePreviewClaims for
// the same reason: the two endpoints take the identical two claim lists and
// must fold them identically (grants are stored lowercased, and both
// enforcement inputs arrive already folded), so a second struct and a second
// normalizer would be two chances to differ by one strings.ToLower.
//
// Not audited, for the reason the governance preview states: it is typed into,
// mints nothing and changes nothing, and a row per keystroke would turn the
// append-only log into a record of every claim an admin tried.
//
// ErrNotFound is a RESULT, not a failure — answered as the empty object. A
// grant that matches but yields no usable home is a 422 naming the reason,
// because that IS the answer the admin needs (their template does not fit this
// person's claims); only a real store failure is a 500.
//
// The resolver is not the whole enforcement path.
//
// resolveUserDriveFor alone is the store read and the fold. Three of the
// launch path's gates run above and below it, so each is run here too — in
// the enforcement path's own order and words:
//
//  0. The org switch (storage.user_drive.disabled), raised by
//     driveSizeCeilingFor as errDrivesDisabled and answered 422 in the
//     REFUSED_BACKEND family with the launch door's own bytes. It runs ahead of
//     the door because the two answer different questions and only the second
//     is about a member: with drives off nobody was denied, so a 403 here would
//     tell an admin a profile refused somebody it did not.
//  1. THE DOOR, resolved for the PREVIEWED claims rather than for the admin
//     doing the previewing (driveDoorShut over ResolveGovernanceProfile).
//     seedRequestDrive runs it after the switch and before the resolver, so a
//     403 beats a 500 or an allocation-shaped 422; so does this.
//  2. The unanswerable group tier. A request that carries no `groups` at all
//     has not evaluated the group tier, which is the same condition a nil
//     snapshot creates at launch — and answering it from the `all` row is how
//     an admin gets a confident preview of a drive no run will mount. So the
//     no-groups form takes driveWithUnusableGroups, the identical function:
//     a user-tier row is fully determined and served (paused included), a
//     deployment with no group-tier grants is served, and anything else is the
//     403 the launch gives. The console always sends both lists (previewClaims
//     splits one box into both), so this arm answers the hand-made request.
//  3. Would it actually bind here (driveIsMountableHere): the backend/runner
//     mismatch and, for a share, driveShareIsBindable's ceiling re-check and
//     the home's existence. The share arm is what TOP RISK 5 was about — a
//     drive whose /srv/homes/bob does not exist previewed as "nas via the user
//     allocation" and 422'd the moment Bob ticked the box.
//
// The NARROWING arm of driveMountFor has no counterpart and is deliberately
// absent: it folds a run request's read_only, and a preview has no run request.
//
// Still not audited. Running the door here does not make this an authorization
// event: nothing is minted and nothing changes, and denyMemberDrive's
// authz.denied row is about a member's own attempt to launch.
//
// And that is enforced, not merely stated. Both of this handler's
// steps reach a site that DOES record — drivePreviewDoorIsOpen calls
// ceilingWithUnusableGroups, previewResolveUserDrive calls
// driveWithUnusableGroups — so once those sites began emitting authz.denied for
// the stale-snapshot refusal, an admin previewing a member's drive wrote a
// denial row of their own. Executed: one preview of carol's drive produced
// `authz.denied target=governance.ceiling actor="sub-admin-alice"` — the ADMIN
// named as the refused principal, for a question they merely asked about
// somebody else. That is worse than the silence before this fix: a denial
// stream with the wrong person in it cannot be read at all.
//
// The whole request is marked as a DISPLAY READ, once, here rather than at each
// step: every line of this endpoint is a display, so a step added later inherits
// the right posture instead of having to remember it.
//
// Routing is SUPER-only, beside the /drives CRUD family; this handler is
// deliberately complete so that registration is one line.
func (s *Server) handlePreviewUserDrive(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(withDisplayRead(r.Context()))
	var req governancePreviewRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	users, msg := normalizeGovernancePreviewClaims(req.UserSubjects, "user_subjects")
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// A claims-less preview is a 400 about the REQUEST, and it must not be
	// allowed to fall through: the resolver's step 2 answers "no subjects, no
	// drive", which would render here as the empty object — "nobody is
	// allocated this" — for a question nobody actually asked. Groups alone
	// cannot be previewed either, because a home name is derived from a USER
	// claim and a group-tier match with no claim to name a directory after has
	// no object name to show.
	//
	// The ADMIN's voice, not §7.7's: these refusals are the member's doors, and
	// answering an admin's empty form with one would put a member-facing
	// sentence on a screen no member can reach.
	if len(users) == 0 {
		writeError(w, http.StatusBadRequest, "user_subjects: at least one claim is needed to derive the directory name")
		return
	}
	groups, msg := normalizeGovernancePreviewClaims(req.Groups, "groups")
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// (0) the org switch, ahead of the door — the launch path's own order, and
	// the order is the argument: storage.user_drive.disabled is a 422 about the
	// DEPLOYMENT and DenyUserDrive is a 403 about one member, so a deployment
	// with drives switched off must answer 422 rather than tell an admin that a
	// profile denied somebody. driveSizeCeilingFor raises it (errDrivesDisabled)
	// from the one read of the org half all three surfaces share; the deployment
	// half of the ceiling comes back with it, and the previewed principal's half
	// is folded in below once the door has named it. Two site-config reads on a
	// display endpoint would be the alternative, and this is the cheaper one.
	ceiling, err := s.driveSizeCeilingFor(r.Context(), 0)
	if err != nil {
		writeDriveError(w, r, err)
		return
	}
	// (1) The door, for the claims that were TYPED — the launch path's second
	// gate, in the launch path's own words.
	userType := strings.TrimSpace(req.UserType)
	previewed, open := s.drivePreviewDoorIsOpen(w, r, users, groups, userType)
	if !open {
		return
	}
	// (1b) The PREVIEWED principal's half, read off the door's OWN resolve — the
	// number the launch path clamps with, so the two surfaces print one size.
	ceiling.ProfileMiB = previewed.Limits.MaxDriveSizeMiB
	// (2) The resolver, taking the unusable-groups arm when the request cannot
	// answer the group tier at all.
	resolved, err := s.previewResolveUserDrive(r.Context(), users, groups, userType, ceiling)
	if err != nil {
		writeDriveError(w, r, err)
		return
	}
	if resolved == nil {
		writeJSON(w, http.StatusOK, userDrivePreviewResponse{})
		return
	}
	// (3) And would it bind here — driveIsMountableHere's own DECISION
	// (driveBindFailureHere), written here WITHOUT its refusal writer. Skipped
	// for a PAUSED row, where nothing above it was derived: there is no object
	// name to stat and nothing would mount anyway, which is the answer the
	// response already carries.
	//
	// The decision, not the refusal, and this was the last unconverted caller of
	// that split. A preview is a DISPLAY READ by an admin about somebody else:
	// routing it through refuseDrive counted wardyn_drive_refusals_total and
	// logged "a run was refused its drive" for a run that never existed, so an
	// operator reading either one saw members being turned away from their drives
	// whenever an admin opened the drives screen. The BODY is unchanged —
	// driveBindFailure.body() is the one spelling both audiences write — because
	// an admin checking why a member cannot mount a drive must read the sentence
	// that member reads.
	if !resolved.Paused {
		if f := s.driveBindFailureHere(r.Context(), *resolved); f != nil {
			writeError(w, f.status, f.body())
			return
		}
	}
	// The SAME positional pick newResolvedDrive made, read back rather than
	// re-derived — a second copy of driveHomeSubject's rule here would be the
	// preview drifting from enforcement in the one field that exists to say
	// what enforcement did.
	homeSubject := driveHomeSubject(resolved.Drive.HomeTemplate, users)
	if resolved.Paused {
		homeSubject = ""
	}
	writeJSON(w, http.StatusOK, userDrivePreviewResponse{
		DriveName:   resolved.Drive.Name,
		MatchedTier: resolved.Tier,
		HomeName:    resolved.HomeName,
		ObjectName:  resolved.ObjectName,
		SizeMiB:     resolved.SizeMiB,
		Writable:    resolved.Writable,
		Enforcement: resolved.Enforcement,
		Paused:      resolved.Paused,
		HomeSubject: homeSubject,
		Warning:     drivePreviewWarning(resolved.Drive.HomeTemplate, users),
	})
}

// drivePreviewDoorIsOpen runs the profile DOOR against the claims an admin
// typed, writing the launch path's own 403 and returning false once it has.
//
// The door at launch keys on the CALLER (denyMemberDrive → driveDoorProfile,
// which exempts an operator). Here the caller is always an operator — every
// /drives route is SUPER — so asking about them would answer "open" for every
// previewed principal and the preview would keep saying "this person mounts
// their drive" about somebody whose profile forbids it. So the ceiling is
// resolved for the PREVIEWED claims and only the shared predicate
// (driveDoorShut) is asked.
//
// SAME BYTES as the launch refusal (driveDeniedByProfileMsg): an admin checking
// why a member cannot mount a drive should read the sentence that member reads,
// not a paraphrase they then have to match up with a support ticket.
//
// A store failure is a 500 and never "the door is open" — ceilingFromProfile's
// own ordering rule, reused rather than restated: a resolve that failed also
// returns a nil profile, and treating that as no-door would fail open on
// exactly the deployment whose database is unhappy.
//
// And an empty `groups` is the unanswerable group tier here too, which this
// gate did not say and its own resolver two functions down does
// (previewResolveUserDrive → driveWithUnusableGroups). The launch path resolves
// its ceiling through effectiveCeiling, which takes ceilingWithUnusableGroups on
// a stale or truncated snapshot; a preview has no snapshot, and an empty list is
// the same condition. Without the arm the DOOR answered from the `all` tier
// while the launch refused groups_snapshot_stale — a hand-made preview said 200
// for a principal every launch bounces, which is a confidently wrong answer at
// the moment an admin is deciding whether an allocation is right. Same function
// as the launch, so the two cannot drift.
//
// It hands the ceiling back, and that is the whole reason it returns two values:
// it resolves the PREVIEWED principal's ceiling already, and the preview's size
// has to be clamped to the same MaxDriveSizeMiB the launch path applies
// (driveSizeCeiling). Discarding it here and resolving a second time is how the
// preview would come to print a number no launch agrees with — which is the one
// thing this endpoint exists not to do.
func (s *Server) drivePreviewDoorIsOpen(w http.ResponseWriter, r *http.Request, users, groups []string, userType string) (governanceCeiling, bool) {
	// A build with no store holds no profiles, so there is no door — the same
	// short-circuit resolveUserDrive's step 1 makes, and it has to be here too
	// because this gate runs BEFORE the resolver.
	if s.cfg.Store == nil {
		return governanceCeiling{}, true
	}
	deployment := governanceCeiling{Spec: s.cfg.DefaultPolicy.Clone()}
	ceiling, err := deployment, error(nil)
	if len(groups) == 0 {
		ceiling, err = s.ceilingWithUnusableGroups(r.Context(), users, userType, deployment)
	} else {
		p, _, rerr := s.cfg.Store.ResolveGovernanceProfile(r.Context(), users, groups, userType)
		ceiling, err = s.ceilingFromProfile(p, rerr, deployment)
	}
	if err != nil {
		writeCeilingError(w, r, err)
		return governanceCeiling{}, false
	}
	if name, shut := driveDoorShut(ceiling); shut {
		writeError(w, http.StatusForbidden, driveDeniedByProfileMsg(name))
		return governanceCeiling{}, false
	}
	return ceiling, true
}

// previewResolveUserDrive is the preview's entrance to the resolver, and the
// one place it differs from resolveUserDrive: the enforcement path decides
// "can this caller's group tier be evaluated" from their SNAPSHOT, and a
// preview has no snapshot — it has the two lists an admin typed.
//
// An empty `groups` is therefore the preview's version of an unusable snapshot:
// the group tier was not evaluated, so a group-tier grant could outrank
// whatever the user and `all` tiers matched. driveWithUnusableGroups is the
// identical function the launch path takes, with the identical scoping — a
// user-tier row settles the question whatever the groups are (paused included),
// a deployment with no group-tier grants has nothing to hide, and anything else
// is the 403 rather than a guess decided by alphabetical luck.
//
// It is NOT the same as "this person is in no groups". A caller who really is
// in none sends `groups: []` and gets the refusal — which is the honest answer
// to a request that cannot tell the two apart, and the console never sends it
// (previewClaims splits one box into both lists, so an admin who typed anything
// has typed groups too).
func (s *Server) previewResolveUserDrive(ctx context.Context, users, groups []string, userType string, ceiling driveSizeCeiling) (*types.ResolvedDrive, error) {
	// The nil-store guard resolveUserDriveFor already keeps, repeated for the
	// arm below it: driveWithUnusableGroups is written for the enforcement path,
	// where resolveUserDrive has already short-circuited a store-less build, and
	// the ~30 nil-store doubles in this package must not start panicking because
	// the preview grew a second entrance to it.
	if s.cfg.Store == nil {
		return nil, nil
	}
	if len(groups) == 0 {
		return s.driveWithUnusableGroups(ctx, users, userType, ceiling)
	}
	return s.resolveUserDriveFor(ctx, users, groups, userType, ceiling)
}
