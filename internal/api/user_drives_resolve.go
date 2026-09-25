// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// User-drive resolution (migration 0054): which drive — if any — belongs
// to the caller, what its per-user object is called, and what the size beside
// it actually means.
//
// It sits beside effectiveCeiling (governance.go) deliberately: both answer
// "what does this deployment say about THIS principal", both resolve through
// the same capabilitySubjects identity and the same subject vocabulary, and
// both must fail CLOSED in the same three shapes. Sharing the shapes is what
// keeps a drive from becoming the one governed surface that quietly falls open
// when the database hiccups.
//
// The member never names a path. A run request carries only
// `drive: {enabled, read_only}`; everything below is derived from the
// authenticated identity, so "mount /home/someone-else" is not a request this
// surface can express.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errDriveUnmountable is the 422-class resolver failure: a grant DID match, and
// the drive it names cannot be turned into a mount for this principal — the
// home template derived nothing usable from their claims, or an override on the
// row is no longer a valid segment.
//
// It is a distinct sentinel from errGroupsSnapshotStale because the two are
// different answers to the caller: the stale one is "we cannot tell whether you
// have a drive" (403, sign in again), this one is "you are authorized and there
// is simply nothing mountable" (422, no audit) — the split
// denyMemberRunQuota already draws between a refusal and an unmet precondition.
// The wrapped cause names the field an admin has to fix.
var errDriveUnmountable = errors.New("drive_unmountable")

// errDrivesDisabled is the ORG SWITCH (storage.user_drive.disabled) reaching the
// resolver, so the three surfaces a drive is answered on cannot disagree about
// whether this deployment offers drives at all.
//
// It is a sentinel rather than a third read of provider.Disabled because the
// switch was already read in three places and asked at only two of them: the
// launch door (seedRequestDrive) and the admin write boundary
// (userDriveWriteRefusal). GET /me shipped an affirmative allocation — name,
// size, writable, home_name, and an empty user_drive_unavailable — and POST
// /drives/preview answered 200 with the same, for a mount the create path
// refuses 422. The New Run card drew the checkbox and its writable sentence for
// a drive no run on this deployment can have. Raising it from
// driveSizeCeilingFor — the ONE read of the org half every surface already
// passes through — is what makes forgetting it impossible rather than
// discouraged.
//
// The launch door still asks first, and must: the org switch is a 422 in the
// REFUSED_BACKEND family while the profile door is a 403 with an authz.denied
// row, so the switch has to be settled BEFORE the door or a deployment with
// drives switched off would log a denial naming a member nobody denied. The
// preview keeps the same order for the same reason. This sentinel is the floor
// under both, not a replacement for either.
var errDrivesDisabled = errors.New("drives_disabled")

// writeDriveError answers a resolver failure at an HTTP site: 403 for the
// stale/truncated group snapshot (reusing the governance resolver's own
// sentinel and message — it is the same snapshot, the same remedy, and a second
// wording would make one deployment problem read as two), 422 for an
// unmountable drive, 500 for everything else.
//
// 500 is the point for the everything-else arm, for the reason writeCeilingError
// states: a store failure means the answer is unknown, and carrying on with the
// zero value would silently mean "you have no drive" for a member who does —
// mounting nothing where an admin allocated something.
// driveUnavailableReason names WHY /me could not answer for a caller's drive, in
// the one shape a wire field may carry it: a closed token, never a sentence.
//
// It is writeDriveError's switch, in the same order and over the same sentinels,
// because the two answer ONE question at two doors. writeDriveError is what a
// member meets when they launch; this is what /me says before they try. A
// deployment where those two disagree is one where the console shows a member a
// state the launch path does not have, which is the whole defect this exists to
// close — so they are written adjacent and a new arm in one is a missing arm in
// the other rather than a silent divergence.
//
// The tokens are for a CLIENT to branch on, not for a human to read. The
// sentence a member gets is still the server-composed one writeDriveError
// writes at the door that refuses them.
func driveUnavailableReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errDrivesDisabled):
		// The existing closed token, deliberately not a fifth one. `unavailable`
		// is what the default arm would have answered anyway, and the console
		// already renders it (NR_UNAVAILABLE) — a new member sentence is a copy
		// change that belongs to the canon sitting, not to this switch, and
		// shipping a token no client branches on yet would only mean the card
		// renders nothing. SITTING ITEM: a drives-are-off member sentence of its
		// own, which is the one thing this arm cannot say today.
		return driveUnavailableUnknown
	case errors.Is(err, errGroupsSnapshotStale):
		return driveUnavailableGroups
	case errors.Is(err, errDriveUnmountable):
		return driveUnavailableUnmountable
	default:
		return driveUnavailableUnknown
	}
}

// ceilingUnavailableReason is driveUnavailableReason's twin for the OTHER half
// of /me's answer: why the DOOR could not be decided.
//
// It exists because a bare bool cannot carry which remedy applies. A truncated
// group snapshot fails BOTH resolves, and on a deployment that assigns
// governance by group while allocating drives per USER the drive resolver
// succeeds — so only the ceiling error knows the remedy is the member's own
// ("sign in again"), and a bool cannot express that. Collapsing the two would
// have /me say governance_unavailable (wait for an operator) while POST /runs
// says 403 groups_snapshot_stale (sign in again) — showing the member the one
// remedy that is not theirs.
//
// Two arms only, and deliberately not driveUnavailableReason's three: what
// failed here is the CEILING, so "the allocation could not be read" is not one
// of the answers. Everything that is not the stale snapshot is
// governance_unavailable — the token whose documented meaning is "nothing is
// wrong with the allocation; what is unknown is permission".
func ceilingUnavailableReason(err error) string {
	if errors.Is(err, errGroupsSnapshotStale) {
		return driveUnavailableGroups
	}
	return driveUnavailableGovernance
}

// The closed reason set. `user_drive_unavailable` carries exactly one of these,
// and "" is the ordinary answer: /me could answer, and user_drive says what it
// answered (an allocation, or null for none).
const (
	// driveUnavailableGroups: the caller's group snapshot cannot answer the
	// group tier, so an allocation may exist and be invisible. 403 at launch.
	driveUnavailableGroups = "groups_snapshot_stale"
	// driveUnavailableUnmountable: an allocation EXISTS and cannot be mounted —
	// a home name that cannot name a directory, a share that is not there. 422
	// at launch, and the one state whose remedy is an admin's, not the member's.
	driveUnavailableUnmountable = "unmountable"
	// driveUnavailableUnknown: the allocation could not be READ. 500 at launch.
	driveUnavailableUnknown = "unavailable"
	// driveUnavailableGovernance: the caller's CEILING could not be resolved, so
	// whether the door is open is unknown. Distinct from the three above because
	// nothing is wrong with the allocation — what is unknown is permission.
	driveUnavailableGovernance = "governance_unavailable"
)

func writeDriveError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errDrivesDisabled):
		// The same bytes the launch door refuses with (seedRequestDrive composes
		// driveRefusedBackendMsg over driveDisabledMsg), not a paraphrase: an
		// admin previewing why a member cannot mount a drive reads the sentence
		// that member reads. 422, never a 403 — nobody was denied by a profile.
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(driveRefusedBackendMsg, driveDisabledMsg))
	case errors.Is(err, errGroupsSnapshotStale):
		writeError(w, http.StatusForbidden, groupsSnapshotStaleMsg)
	case errors.Is(err, errDriveUnmountable):
		// The sentinel's own name is stripped: what is left is the frozen
		// MEMBER sentence (docs/design/user-drives-prompt.md's DRIVE_MEMBER
		// table), and the console renders a server refusal verbatim. The
		// sentinel exists to be matched with errors.Is by the callers above, not
		// to be read by the human this refuses.
		writeError(w, http.StatusUnprocessableEntity,
			strings.TrimPrefix(err.Error(), errDriveUnmountable.Error()+": "))
	default:
		writeServerError(w, r, "resolve user drive", err)
	}
}

// the resolver

// resolveUserDrive returns the drive that applies to the caller on ctx, or nil
// when they have none. Resolution order, and each step is a decision rather
// than a fallback:
//
//  1. NO STORE ⇒ no drive. A build with no store holds no grants, so there is
//     nothing to resolve and the answer is the pre-feature answer: no mount.
//  2. NO SUBJECTS ⇒ no drive. An admin token and local mode carry no per-human
//     identity, so there is no principal to name a home after and an `all`-tier
//     grant would hand every identity-less caller ONE shared directory. An SSO
//     ADMIN is not in this arm and resolves like anyone else — the drive door
//     (GovernanceLimits.DenyUserDrive) is what keys on isOperator, not this.
//  3. Unanswerable group snapshot ⇒ user-tier only, and a refusal in exactly
//     one shape (see driveWithUnusableGroups).
//  4. STORE ERROR ⇒ ERROR. Never "no drive": a database hiccup would silently
//     drop a member's storage out of a run that then writes its work into a
//     container layer nobody keeps.
//  5. ErrNotFound ⇒ no drive. Absent row, absent behaviour — the same
//     back-compat doctrine an absent governance assignment follows.
//
// ponytail: no cache, matching effectiveCeiling's own note. One indexed read
// per resolve; a stale allocation is a correctness bug, not a slow page.
// profileMaxDriveMiB is this caller's GovernanceLimits.MaxDriveSizeMiB, which
// every caller already holds (the launch path from the ceiling it resolved for
// the request, /me the way its door check does) — passed rather than resolved
// here so the PREVIEW, whose subject is not its caller, can hand in the previewed
// principal's instead of the admin's.
func (s *Server) resolveUserDrive(ctx context.Context, profileMaxDriveMiB int) (*types.ResolvedDrive, error) {
	if s.cfg.Store == nil {
		return nil, nil
	}
	users, groups, stale := capabilitySubjects(ctx)
	if len(users) == 0 {
		return nil, nil
	}
	ceiling, err := s.driveSizeCeilingFor(ctx, profileMaxDriveMiB)
	if err != nil {
		return nil, err
	}
	// Truncated counts as stale, the same rule effectiveCeiling applies:
	// the group snapshot is sorted and cut at the cookie byte cap, so a member
	// in enough groups holds one that is present, non-nil and INCOMPLETE — and
	// the group whose grant carries their drive is exactly as likely to be
	// missing as any other.
	if stale || oidcGroupsTruncatedFromContext(ctx) {
		return s.driveWithUnusableGroups(ctx, users, ceiling)
	}
	return s.resolveUserDriveFor(ctx, users, groups, ceiling)
}

// driveWithUnusableGroups is step 3: the caller's group identity cannot be
// evaluated, so resolve on their user subjects alone and decide whether that
// answer is trustworthy anyway.
//
// It is trustworthy in exactly two shapes, and the scoping is the whole point —
// a blanket 403 here would lock every pre-0.6 cookie out of every deployment,
// including the ones that have never allocated a drive:
//
//   - A USER-TIER row matched. user > group > all, so an explicitly named
//     principal's drive is FULLY determined whatever their groups are —
//     INCLUDING when that row is paused, which is an answer and not an absence:
//     no group-tier grant can outrank it, so nothing the snapshot is hiding
//     could change it.
//   - no group-tier grant exists at all. Nothing an unknown group could have
//     matched, so nothing a nil snapshot could be hiding.
//
// Otherwise it refuses, and the refusal is the honest answer: with the group
// tier unreadable, an `all`-tier grant would win by default and could hand this
// member a WRITABLE drive where their group's row says read-only — a widening
// decided by alphabetical luck.
//
// HasGroupTierDriveGrants stays a SEPARATE read, deliberately: the case that
// most needs it is the one where the resolver matched NOTHING, and a zero-row
// result carries no columns to have piggybacked the answer on.
func (s *Server) driveWithUnusableGroups(ctx context.Context, users []string, ceiling driveSizeCeiling) (*types.ResolvedDrive, error) {
	d, g, tier, err := s.cfg.Store.ResolveUserDrive(ctx, users, nil)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("api: resolve user drive: %w", err)
	}
	if err == nil && tier == types.CapabilitySubjectUser {
		return newResolvedDrive(d, g, tier, users, ceiling)
	}
	hasGroupTier, herr := s.cfg.Store.HasGroupTierDriveGrants(ctx)
	if herr != nil {
		return nil, fmt.Errorf("api: resolve user drive: %w", herr)
	}
	if hasGroupTier {
		// Audited, at the SECOND site that decides this refusal.
		//
		// This branch is the mirror image of ceilingWithUnusableGroups' own, and
		// it was the silent one: docs/AUDIT-ACTIONS.md and OPERATIONS.md both
		// described groups_snapshot_stale as emitted "at the ONE site that
		// decides it", naming the governance resolver — while the DRIVES
		// resolver raised the identical 403 from here and recorded nothing. On
		// the deployment shape that has group-tier DRIVE grants and no
		// group-tier governance assignment, that made the whole denial stream
		// empty: executed, 0 authz.denied rows out of 0 events for a member the
		// launch door refuses 403.
		//
		// HERE rather than at writeDriveError, for the reason the governance
		// twin gives: writeDriveError is a free function with no server and no
		// context, and auditing at the write sites would mean one emit per seam.
		// This is the only place the drive refusal is DECIDED.
		//
		// runs.drive is the target, matching denyMemberDrive — the other refusal
		// this seam writes — rather than governance.ceiling. The two rows say
		// different things: one is "your profile shuts the drive door", the
		// other "nobody can tell whether it is shut", and an operator filtering
		// by target is asking about the drive either way.
		if !isDisplayRead(ctx) {
			s.recordRefusal(ctx, nil, authz.Deny(authz.ReasonGroupsSnapshotStale, "runs.drive", ""))
		}
		return nil, errGroupsSnapshotStale
	}
	if err != nil {
		return nil, nil // ErrNotFound, and no group tier could have been hiding one
	}
	return newResolvedDrive(d, g, tier, users, ceiling)
}

// displayReadCtxKey marks a resolve made to DISPLAY a state, not to enforce one.
//
// It exists because the two groups_snapshot_stale deciding sites write an
// authz.denied row, and GET /me reaches BOTH of them on every console poll. A
// member with an unanswerable group snapshot therefore produced denial rows on a
// TIMER — two per poll on a deployment that assigns governance by group and
// allocates drives by group — for a member who never asked for a run. Executed
// before the mark existed: three /me polls, three rows.
//
// That is the same argument resolveMeUserDrive already makes one layer up about
// the metric and the WARN ("The decision, not the refusal: a /me poll is a
// display read on a timer, so running the writer here would inflate
// wardyn_user_drive_refused_total and fill the log with WARNs for a member who
// never asked for a run"), and an audit row is a STRONGER artefact than a WARN:
// it is the operator's count of who was refused, and page views are not
// refusals.
//
// Set by the display callers, never by a middleware, so the default is
// "enforcing" and a new enforcement seam cannot silently inherit the
// suppression: seedRequestDrive, the launch and preflight paths and every other
// caller reach the deciding sites unmarked and keep recording. There are two
// display callers — GET /me, and POST /drives/preview, whose handler doc has
// always said it is "still not audited" and whose steps reach both deciding
// sites; the preview is the sharper case, because the row it wrote named the
// ADMIN asking about somebody else as the refused principal. The DECISION is
// unchanged either way — /me still refuses to answer, and still reports
// groups_snapshot_stale on the wire; what the mark removes is only the
// operator-facing ROW for a request nobody was refused by.
type displayReadCtxKey struct{}

func withDisplayRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, displayReadCtxKey{}, true)
}

func isDisplayRead(ctx context.Context) bool {
	v, _ := ctx.Value(displayReadCtxKey{}).(bool)
	return v
}

// resolveUserDriveFor is the store read plus the absent-row doctrine, shared by
// the enforcement path above and by the preview endpoint below so the two
// cannot answer differently for the same claims.
//
// Order matters, and it is the same rule ceilingFromProfile states: the
// REAL-ERROR check runs FIRST, ahead of the nil-row one, because a failed
// resolve also returns nil rows — checking nil first would turn every store
// failure into "no grant matched" and mount nothing, which is the fail-quiet
// this file exists to refuse, arriving through the back door of a convenience
// guard.
//
// The nil-store guard is repeated here rather than left to resolveUserDrive
// because THIS is the door the preview handler enters through, and the ~30
// nil-store doubles in this package must not start panicking on it.
func (s *Server) resolveUserDriveFor(ctx context.Context, users, groups []string, ceiling driveSizeCeiling) (*types.ResolvedDrive, error) {
	if s.cfg.Store == nil {
		return nil, nil
	}
	d, g, tier, err := s.cfg.Store.ResolveUserDrive(ctx, users, groups)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("api: resolve user drive: %w", err)
	}
	if err != nil {
		return nil, nil // ErrNotFound: no grant matched, so no drive
	}
	return newResolvedDrive(d, g, tier, users, ceiling)
}

// driveSizeCeiling is the TWO numbers that bound a resolved drive's size, folded
// at one site so launch, GET /me and POST /drives/preview cannot disagree about
// how big a drive is.
//
// Both are 0 = UNLIMITED, and they are kept apart until the fold because the
// warning has to say which one bit: "this deployment caps every drive" and "the
// profile you are assigned caps yours" are different facts with different
// remedies, and an operator reading one clamped number cannot tell them apart.
//
// DeploymentMiB is storage.user_drive.max_size_mib — the same ceiling the admin
// write boundary refuses 422 against (userDriveWriteRefusal). It is re-applied
// HERE because a ceiling LOWERED after allocations exist binds nothing at that
// boundary: no row is being written, and every stored size is already above it.
//
// ProfileMiB is GovernanceLimits.MaxDriveSizeMiB, which is PER PRINCIPAL and
// therefore cannot be applied at a grant write at all — the profile binding a
// subject is claims-resolved and unreadable from the grant row. This resolver is
// the only scope holding both facts, which is why the clamp lives here.
//
// A clamp, never a refusal: the size is an allocation an admin already made, and
// refusing the mount would take a member's storage away over a number. And it
// clamps a NUMBER — on `external` backends (a share, a static claim) that number
// binds no bytes at all, which is what the frozen honesty sentence says.
type driveSizeCeiling struct {
	DeploymentMiB int
	ProfileMiB    int
}

// bound is the ONE expression both ceilings meet: the smaller of the two, with a
// zero standing for "no ceiling" rather than for "nothing allowed". It answers
// (0, "") when neither binds, and otherwise the bound and which half set it.
func (c driveSizeCeiling) bound() (int, string) {
	orUnlimited := func(v int) int {
		if v <= 0 {
			return math.MaxInt
		}
		return v
	}
	bound := min(orUnlimited(c.DeploymentMiB), orUnlimited(c.ProfileMiB))
	switch {
	case bound == math.MaxInt:
		return 0, ""
	case bound == c.ProfileMiB:
		// The profile wins a TIE, deliberately: the number is the same either
		// way, and the per-principal half is the one an admin can change for this
		// one member.
		return bound, driveBoundByProfile
	default:
		return bound, driveBoundByDeployment
	}
}

// The two values driveSizeCeiling.bound names, and the two the clamp's log line
// carries — an operator filtering on `bound_by` is asking which ceiling to edit.
const (
	driveBoundByProfile    = "governance_profile"
	driveBoundByDeployment = "deployment"
)

// driveSizeCeilingFor folds a principal's governance MaxDriveSizeMiB together
// with the deployment's own storage.user_drive.max_size_mib — the ONE read of the
// org half, so the three surfaces share a spelling instead of each taking their
// own.
//
// A site-config read that FAILS is an ERROR, never a zero: a ceiling that could
// not be read is not "no ceiling", and treating it as one is the fail-open this
// file's ordering rules exist to refuse.
//
// And the switch is answered HERE (errDrivesDisabled), because this is the one
// read of the org half every surface passes through. The block it reads carries
// two facts, not one — `disabled` and `max_size_mib` — and a function that took
// the number while dropping the switch is how /me and the preview came to offer
// an allocation the launch path refuses 422. There is no ceiling to fold for a
// deployment that mounts no drives at all, so the switch is raised before the
// number is composed.
func (s *Server) driveSizeCeilingFor(ctx context.Context, profileMaxMiB int) (driveSizeCeiling, error) {
	provider, err := s.userDriveProvider(ctx)
	if err != nil {
		return driveSizeCeiling{}, fmt.Errorf("api: resolve user drive: %w", err)
	}
	if provider.Disabled {
		return driveSizeCeiling{}, errDrivesDisabled
	}
	return driveSizeCeiling{DeploymentMiB: provider.MaxSizeMiB, ProfileMiB: profileMaxMiB}, nil
}

// newResolvedDrive folds one winning (drive, grant) pair into everything a
// runner or a preview needs. Four folds, each of which could have gone the
// other way:
//
//   - a disabled winner is paused, and it returns before anything is derived.
//     The store hands back the row that won its tier whether or not it is
//     enabled (DESIGN §2.2), so this is the single place that tells the two
//     apart — and a paused drive mounts nothing, which makes a home name a
//     directory no consumer will ever ask for. Deriving one anyway is worse
//     than useless: DriveHomeName can FAIL, and a failure here would answer a
//     paused member with the wrong refusal entirely (422 "your claim cannot
//     name a directory") for a drive that was never going to mount.
//
//   - HOME OVERRIDE applies on the user tier only. types.ValidateUserDriveGrant
//     already refuses to store one on a group or all row; the tier gate is
//     repeated here because a row written by an older binary — or by hand —
//     would otherwise hand an entire group ONE directory, which is precisely
//     the isolation the per-user subdirectory buys.
//
//   - SIZE is the override when the admin set one, else the drive's. 0 means
//     "unset" on both, so a grant that never mentions size inherits rather than
//     zeroing the allocation — and the fold is then CLAMPED to the deployment's
//     and this principal's ceilings (driveSizeCeiling), which is why every caller
//     hands one in. The clamp lives here, per RESOLVED drive, rather than in any
//     singleton helper: "one principal has at most one drive" is today's limit,
//     not the ceiling's shape.
//
//   - WRITABLE is the override when present, else the drive's — a COALESCE, not
//     an intersection, because both values are admin-authored and the grant is
//     the more specific statement of the two. The narrowing rule applies one
//     layer up instead: the RUN REQUEST may only narrow this, never widen it.
//
// A home that cannot be derived is errDriveUnmountable (422) and never a guess:
// a fabricated segment either collides with another member's home or escapes
// it, and both are worse than a refusal naming the field to fix.
func newResolvedDrive(d *types.UserDrive, g *types.UserDriveGrant,
	tier types.CapabilitySubjectType, users []string, ceiling driveSizeCeiling) (*types.ResolvedDrive, error) {
	// A store answering (nil, nil, "", nil) means the absent-row doctrine
	// rather than a dereference, so no caller has to have checked on its
	// behalf — the same guard ceilingFromProfile keeps for a nil profile.
	if d == nil || g == nil {
		return nil, nil
	}
	size := d.SizeMiB
	if g.SizeMiBOverride > 0 {
		size = g.SizeMiBOverride
	}
	// One line, naming which ceiling bit: the member's number is bounded, and the
	// operator's log says which of the two to edit. Not an audit row — nobody was
	// denied anything (composer.Clamp's own doctrine for the sibling ephemeral
	// cap), and not a refusal, for the reason driveSizeCeiling states.
	if bound, by := ceiling.bound(); bound > 0 && size > bound {
		slog.Warn("wardynd: user drive: a drive's size was clamped to a ceiling",
			slog.String("drive", d.Name), slog.Int("allocated_mib", size),
			slog.Int("ceiling_mib", bound), slog.String("bound_by", by))
		size = bound
	}
	writable := d.Writable
	if g.WritableOverride != nil {
		writable = *g.WritableOverride
	}
	resolved := &types.ResolvedDrive{
		Drive: *d, Grant: *g, Tier: tier,
		SizeMiB:     size,
		Writable:    writable,
		Enforcement: types.EnforcementFor(d.Backend),
	}
	// The size and mode folds run for a paused row too: what the member is
	// shown is the allocation that is off, and reading the drive's own size
	// where the grant overrode it would name a different one.
	if !g.Enabled {
		resolved.Paused = true
		return resolved, nil
	}
	// The managed non-hash refusal, repeated — and repeated for the same reason
	// the home-override tier gate below is, on a row written by an older binary
	// or by hand. A managed object is named by the HOME alone, and neither
	// non-hash template can safely name one: under `email_local` two principals
	// whose addresses share the part before the "@" resolve to ONE volume or PVC
	// (the driver's own collision check cannot see it — the object IS labelled
	// with this drive), so deriving the home anyway would hand the second
	// principal the first one's storage; under `sub` the name is unique but it
	// publishes the sign-in subject (below). Hence the member sentence names the
	// sign-in IDENTITY the home is derived from rather than the email claim: on
	// a `sub` row the email had no part in it, so a member told to look at their
	// email address — and the admin they forward that to — cannot act on it.
	//
	// It runs BEFORE the derivation, not after: the home it would derive is
	// exactly the colliding one, and a check downstream of it would be reasoning
	// about a value it had already accepted.
	//
	// EVERY non-hash template, not just `email_local`: the write boundary was
	// widened to the full rule (types.ManagedBackendRejectsTemplate) and this
	// site was not, so a managed row carrying `sub` — authorable by any older
	// binary, or by hand — was refused on write and still resolved and mounted,
	// publishing the sign-in subject in an object name `docker volume ls` and
	// `kubectl get pvc` print without inspecting anything.
	if types.ManagedBackendRejectsTemplate(d.Backend, d.HomeTemplate) {
		slog.Warn("wardynd: user drive: a managed drive carries a non-hash home template, which cannot safely name one object per person",
			slog.String("drive", d.Name), slog.String("backend", string(d.Backend)),
			slog.String("home_template", string(d.HomeTemplate)))
		// REFUSED_BACKEND's frozen shape, whose parenthesised half is where the
		// admin's diagnosis goes — the same reuse driveShareIsBindable makes for
		// the roots ceiling, and the same honest reading: from the member's side
		// "the roots moved" and "this row could not be authored today" are one
		// fact, which is that this deployment cannot mount their drive.
		return nil, fmt.Errorf("%w: drive: this deployment cannot mount your drive "+
			"(its directory name is derived from your sign-in identity, which cannot safely name one %s object per person — "+
			"ask an admin to change how this drive names directories)", errDriveUnmountable, d.Backend)
	}
	// The mirror refusal, and repeated here for the same reason and on the same
	// class of row: a share's directories are named by whoever owns the share
	// and Wardyn never mkdir's on one, so a `hash` home names a directory that
	// cannot exist. The write boundary has refused that shape since it was
	// written; a row that PREDATES the rule, or one written by hand, resolved
	// cleanly and then met the MISSING-HOME refusal — "directory
	// d-00e23f375d35be941331 does not exist on the share — ask an admin to
	// create it", which asks an admin to make a directory named after a digest
	// and names the wrong remedy for the row they actually have. The right
	// remedy is the template, and it is an admin's to change.
	//
	// BEFORE the derivation, like the managed arm above: the home it would
	// derive is exactly the unmakeable one, and a check downstream of it would
	// be reasoning about a value it had already accepted. The share rule keys
	// on who NAMES the object (types.DriveObjectNamedByWardyn):
	// host_path refuses `hash`, and k8s_pvc_static — a share Wardyn names —
	// refuses `email_local` instead, so this arm fires for both share backends.
	if types.ShareBackendRejectsTemplate(d.Backend, d.HomeTemplate) {
		slog.Warn("wardynd: user drive: a share drive is templated on a hash, which names a directory nobody created",
			slog.String("drive", d.Name), slog.String("backend", string(d.Backend)),
			slog.String("home_template", string(d.HomeTemplate)))
		// REFUSED_BACKEND's frozen shape again, whose parenthesised half is
		// where the admin's diagnosis goes — the same reuse the managed arm and
		// driveShareIsBindable both make, and the same honest reading: from the
		// member's side this deployment cannot mount their drive.
		return nil, fmt.Errorf("%w: drive: this deployment cannot mount your drive "+
			"(its directory name comes from a hash, and a share's directories are named by whoever owns the share — "+
			"ask an admin to change how this drive names directories)", errDriveUnmountable)
	}
	override := ""
	if tier == types.CapabilitySubjectUser {
		override = g.HomeOverride
	}
	subject := driveHomeSubject(d.HomeTemplate, users)
	home, err := types.DriveHomeName(*d, subject, override)
	if err != nil {
		// The BODY is the frozen member sentence and NOTHING ELSE. What this
		// member needs is which of their claims could not name a directory and
		// who can fix it; types.DriveHomeName's own message names a template and
		// a regex, which is an operator's diagnostic and reads to a member as
		// gibberish appended to the sentence that was meant for them. So it goes
		// to the LOG, where the operator who has to act on it already looks —
		// and the member's 422 stays byte-identical to the mock.
		//
		// Whose value failed is in the log, because the log is where the person
		// who can fix it looks. DriveHomeName SHORT-CIRCUITS on the override —
		// a non-empty one is checked and returned before any template is read —
		// so home_override_set true means the invalid value is the ADMIN's
		// stored one and the template had no part in it. Without this field the
		// operator's own diagnostic named home_template for a failure the
		// template did not cause, and the admin looked at the wrong setting.
		//
		// The flag, never the value: a directory name is a person's username as
		// often as not, the same reason drive.grant.write audits
		// home_override_set rather than what it was set to.
		slog.Warn("wardynd: user drive: a home name could not be derived for this principal",
			slog.String("drive", d.Name), slog.String("backend", string(d.Backend)),
			slog.String("home_template", string(d.HomeTemplate)),
			slog.Bool("home_override_set", strings.TrimSpace(override) != ""),
			slog.String("err", err.Error()))
		// Filed, not fixed here — the sentence blames the member for an admin's
		// value. When an override is set it is the ONLY thing that can have
		// failed (see the short-circuit above), yet this interpolates
		// d.HomeTemplate unconditionally, so a member is told "your hash cannot
		// name a directory" about a machine-generated name they never supplied
		// and cannot change. The remedy clause is already right — an admin does
		// fix it — but the subject is not, and the member is left with nothing
		// to ask for by name.
		//
		// It is not fixed here because §7.7 declares its table COMPLETE and no
		// row covers "an administrator's setting for your drive is invalid":
		// REFUSED_HOME_INVALID says "your {claim}", which is the wrong subject,
		// and REFUSED_BACKEND is the deployment-capability sentence, which
		// carries no remedy for a case that has one. That is new member copy, so
		// it is FILED (local/FILED-COPY.md) rather than invented at a call site.
		//
		// The FROZEN sentence first, byte-for-byte, then the substrate's own
		// clause when the substrate is stricter than the sentence describes.
		//
		// The canon describes driveHomeSegmentRe, which is the DOCKER rule; a
		// k8s backend enforces driveHomeSegmentK8sRe, which also forbids `_` and
		// a trailing `-`/`.`. The motivating case is not hypothetical — an Entra
		// `sub` is base64url and routinely carries `_` — and the member was being
		// told the character that refused them was allowed, so they asked their
		// admin for nothing. The canon is frozen, so it is not reworded; the
		// clause is APPENDED (types.DriveHomeStricterRuleClause, which lives
		// beside the regex it describes), the §7.1 server-composed class.
		return nil, fmt.Errorf("%w: %s", errDriveUnmountable,
			strings.TrimSpace(fmt.Sprintf("drive: your %s cannot name a directory "+
				"(lowercase letters and digits, then . _ -, up to 63 characters) — "+
				"ask an admin to set your directory name %s",
				d.HomeTemplate, types.DriveHomeStricterRuleClause(d.Backend))))
	}
	resolved.HomeName = home
	resolved.ObjectName = types.DriveObjectName(*d, home)
	// The fingerprint of the principal the home was derived FROM, which the home
	// cannot answer for once a template or an override has folded two subjects
	// onto one segment. Derived from the SAME subject DriveHomeName was handed —
	// never from users[0] again — so the label the driver stamps and the object
	// it stamps it on were decided by one claim.
	resolved.SubjectHash = types.DriveSubjectHash(subject)
	return resolved, nil
}

// driveHomeSubject picks WHICH of the caller's identities the drive's home
// template names — the one selection types.DriveHomeName cannot make, because
// by the time the claims reach it they are a flat list with no labels.
//
// It is POSITIONAL, and that is the contract rather than a shortcut:
// capabilitySubjects builds the list as [lowercased sub, email] and both
// entrances to this resolver preserve that order (the enforcement path by
// construction, the preview endpoint by its documented request contract). So
// the LAST entry is the email whenever the caller presented both, and the first
// is their stable primary identity.
//
// Deliberately not a shape guess. There is no "an @ makes it an email" rule
// here for the same reason the governance preview refuses one: a second opinion
// about which claim is which, living beside the ordering the resolver already
// ranks by, is how two surfaces start disagreeing about one person. A caller
// who presented only a sub and a drive templated on `email_local` gets a
// refusal naming the remedy (a home override on their grant), not a guess.
func driveHomeSubject(tmpl types.HomeTemplate, users []string) string {
	if len(users) == 0 {
		return ""
	}
	switch tmpl {
	case types.HomeTemplateEmailLocal:
		return users[len(users)-1]
	default:
		return users[0]
	}
}
