// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── /me ───────────────────────────────────────────────────────────────────

// meUserDrive is the /me.user_drive object: what the caller would mount if they
// asked for it on their next run, or nil when they would mount nothing.
//
// It is the MEMBER's view of the same resolution the run path takes, and it is
// keyed on the resolver rather than on role: a security admin's own drive
// resolves exactly like a member's, because a drive is per-principal and not
// per-tier. An operator with no per-human subjects (admin token, local mode)
// resolves to nil, which is the resolver's own step 2 and not a special case
// here.
//
// IT CARRIES NO DOOR FIELD, deliberately (owner ruling at the mock gate). This
// object means ONE thing — "what is allocated to you" — and the door is a
// property of the caller's PROFILE, not of the allocation. Folding them would
// make the two states that matter inexpressible: a member who is denied and has
// no allocation would be indistinguishable from a member who simply has none,
// which is exactly the pair the console has to tell apart. The door rides
// beside it as /me.user_drive_denied_by_profile.
type meUserDrive struct {
	Name        string                   `json:"name"`
	Backend     types.DriveBackend       `json:"backend"`
	SizeMiB     int                      `json:"size_mib,omitempty"`
	Writable    bool                     `json:"writable"`
	Enforcement types.StorageEnforcement `json:"enforcement"`
	HomeName    string                   `json:"home_name,omitempty"`
	// Paused: allocated, disabled by an admin — the chip says so and the New Run
	// card renders the paused line where the checkbox would be.
	Paused bool `json:"paused,omitempty"`
}

// resolveMeUserDrive answers the /me.user_drive field, or nil.
//
// EVERY FAILURE IS nil, and that is the one place in this feature where failing
// quiet is right: /me is a display read whose other fields the console needs to
// render the shell at all, so a store hiccup must degrade the drive chip rather
// than 500 the whole endpoint and log the human out of their own console. The
// ENFORCEMENT path (seedRequestDrive) makes the opposite choice for the same
// error, and must: mounting nothing where an admin allocated something is a
// data loss, while showing nothing for a moment is a refresh.
// WHAT WAS WRONG WAS NOT THE nil — IT WAS THAT nil WAS THE WHOLE ANSWER. Three
// distinct states the launch path refuses with 403, 422 and 500 all arrived here
// as the SAME `{"user_drive":null}` a genuinely unallocated member gets, and the
// console renders that as no drive affordance at all — so a member could never
// reach the refusal naming their remedy, and an admin-fixable home name looked
// exactly like "you have no allocation". The reason string is the second half of
// the answer, on the wire, so a client can tell them apart: the same argument
// the door already won as its own sibling key.
func (s *Server) resolveMeUserDrive(r *http.Request) (*meUserDrive, string) {
	// A DISPLAY READ (withDisplayRead, user_drives_resolve.go). The same
	// argument the paragraph above makes about the metric and the WARN, applied
	// to the audit row the stale-snapshot refusal now writes: a poll on a timer
	// is not a denial, and an operator counting denials must not be counting
	// page views. The refusal itself is unchanged — this still answers
	// groups_snapshot_stale on the wire.
	ctx := withDisplayRead(r.Context())
	resolved, err := s.resolveUserDrive(ctx)
	if err != nil {
		return nil, driveUnavailableReason(err)
	}
	if resolved == nil {
		return nil, "" // answered, and the answer is "no allocation"
	}
	// WOULD IT ACTUALLY BIND HERE. driveIsMountableHere ran at the launch door
	// and at the ADMIN preview and never on the member's own surface, so /me
	// offered a mountable-looking allocation — name, size, writable, home_name,
	// user_drive_unavailable "" — for a drive this deployment refuses 422 at
	// launch ("directory carol does not exist on the share — ask an admin to
	// create it"). The New Run card drew the checkbox and its writable sentence
	// for a mount the create path was going to reject.
	//
	// THE DECISION, NOT THE REFUSAL (driveBindFailureHere): a /me poll is a
	// display read on a timer, so running the writer here — even against a
	// throwaway ResponseWriter — would inflate wardyn_user_drive_refused_total
	// and fill the log with WARNs for a member who never asked for a run.
	//
	// SKIPPED FOR A PAUSED ROW, the same scoping the preview uses: nothing was
	// derived above it, there is no object name to stat, and "paused" is already
	// the answer the response carries.
	//
	// THE EXISTING `unmountable` TOKEN, not a new one. Its own doc reads "an
	// allocation EXISTS and cannot be mounted — a home name that cannot name a
	// directory, a share that is not there. 422 at launch, and the one state
	// whose remedy is an admin's" — which is this state exactly. The 503 arm
	// (the runner could not be asked) is not about the drive at all, so it takes
	// `unavailable`, matching writeDriveError's own status mapping.
	if !resolved.Paused {
		if f := s.driveBindFailureHere(ctx, *resolved); f != nil {
			if f.status == http.StatusServiceUnavailable {
				return nil, driveUnavailableUnknown
			}
			return nil, driveUnavailableUnmountable
		}
	}
	return &meUserDrive{
		Name:        resolved.Drive.Name,
		Backend:     resolved.Drive.Backend,
		SizeMiB:     resolved.SizeMiB,
		Writable:    resolved.Writable,
		Enforcement: resolved.Enforcement,
		HomeName:    resolved.HomeName,
		Paused:      resolved.Paused,
	}, ""
}

// userDriveDeniedByProfile names the profile whose DenyUserDrive door is shut
// for this caller, or "" when the door is open — the /me sibling field
// user_drive_denied_by_profile.
//
// A STRING, not a bool, because the member-facing sentence quotes the profile
// by name ("your governance profile %q does not allow…") and a bool would make
// the console invent the rest of it or omit the one word that tells an admin
// which profile to look at.
//
// INDEPENDENT OF THE ALLOCATION, which is the point of the split: a member with
// no drive AND a shut door is a real, distinct state — asking their admin for
// an allocation would not help them, and a field folded into user_drive could
// not have said so.
//
// THE SECOND RETURN IS "I CANNOT ANSWER THE DOOR", and it now covers TWO
// causes, both of which must not read as an open door: a ceiling that could not
// be resolved, and a door that is shut under a profile with no name to quote.
//
// IT IS THE REASON, NOT A BOOL, and that is R1 F273's residue rather than a
// refactor. A bool says "I could not answer" and throws away WHY, so the
// ceiling's own groups_snapshot_stale could never reach the wire: on a
// deployment that assigns governance profiles by group and allocates drives per
// USER, the drive resolver succeeds and the ceiling is the only half that
// failed — /me answered governance_unavailable ("wait for an operator") while
// POST /runs answered 403 groups_snapshot_stale ("sign in again"), showing the
// member the one remedy that is not theirs. The earlier fix compared the DRIVE
// resolver's token in me.go, which closes that for the population where the
// drive resolve also fails and changes nothing for this one.
//
// It reads driveDoorProfile — the SAME predicate denyMemberDrive enforces with,
// whose keying (operator exempt, unassigned member has no door) is documented
// there. A ceiling that cannot be resolved reports "" for resolveMeUserDrive's
// own reason — /me is a display read, and the ENFORCEMENT path answers the same
// failure with a refusal. The operator short-circuit stays HERE too, ahead of
// the resolve: a display read must not cost an operator a ceiling round-trip.
func (s *Server) userDriveDeniedByProfile(r *http.Request) (name, unresolved string) {
	if s.isOperator(r.Context()) {
		return "", ""
	}
	// A DISPLAY READ, for the reason resolveMeUserDrive states: this is the
	// SECOND deciding site /me reaches, so without the mark here a poll still
	// wrote one authz.denied row even after the drive seam stopped.
	ctx := withDisplayRead(r.Context())
	ceiling, err := s.effectiveCeiling(ctx)
	if err != nil {
		// UNKNOWN IS NOT OPEN. "" on this key is an affirmative promise that no
		// profile shuts the door, and serving it for a ceiling that could not be
		// resolved answers an unknown question permissively — the exact thing
		// writeCeilingError refuses to do at the enforcement door, where this
		// same outage refuses the run. Shipped beside a fully populated
		// user_drive it made /me promise a mountable, writable drive for a
		// create the server would then refuse.
		return "", ceilingUnavailableReason(err)
	}
	// THE BOOL IS THE DECISION, and discarding it here was the same fail-open
	// driveDoorShut's own bool was introduced to close, left standing at the
	// sibling call site. governance_profiles.name is TEXT NOT NULL UNIQUE with
	// no non-empty CHECK, so a profile with DenyUserDrive set and a blank name
	// reports ("", true): the name key then shipped "" — which the documented
	// contract reads as "no profile denies you" — beside a fully populated,
	// writable user_drive, while POST /runs with drive.enabled answered 403
	// 'mounting a user drive is not allowed by your governance profile ""'.
	//
	// UNNAMED-BUT-SHUT TAKES THE DOOR-UNKNOWN PATH (the remediation's second
	// option), rather than a new value on either key. The display key cannot say
	// "shut" without a name to quote — that is what it is FOR — so the honest
	// answer is the one /me already has for "I cannot answer the door": suppress
	// the allocation so the card cannot offer a mount the launch refuses, and
	// let the reason key carry it. A NAMED deny is untouched: it keeps shipping
	// the profile name beside the allocation, which is the four-state doctrine
	// working as designed (an allocation and a door are different facts).
	name, shut := s.driveDoorProfile(ctx, ceiling)
	if shut && name == "" {
		// The ceiling RESOLVED here; what cannot be said is which profile shut
		// the door. governance_unavailable is still the honest token — the
		// answer to "may you mount" is unknown — and it is the one this arm has
		// always produced, so F274's behaviour is unchanged by the widening
		// above becoming reason-carrying.
		return "", driveUnavailableGovernance
	}
	return name, ""
}

// driveRefusal composes a 422 body in the frozen member voice: lowercase
// opening, prefixed `drive:`, and it names the remedy. One helper so the four
// refusal sites cannot drift into four different tones for one feature.
func driveRefusal(reason string) string {
	return "drive: " + strings.TrimSpace(reason)
}
