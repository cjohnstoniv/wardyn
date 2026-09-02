// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// User-drive resolution (0.7, migration 0054): which drive — if any — belongs
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
// THE MEMBER NEVER NAMES A PATH. A run request carries only
// `drive: {enabled, read_only}`; everything below is derived from the
// authenticated identity, so "mount /home/someone-else" is not a request this
// surface can express.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

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

// writeDriveError answers a resolver failure at an HTTP site: 403 for the
// stale/truncated group snapshot (reusing the governance resolver's own
// sentinel and message — it is the same snapshot, the same remedy, and a second
// wording would make one deployment problem read as two), 422 for an
// unmountable drive, 500 for everything else.
//
// 500 IS THE POINT for the everything-else arm, for the reason writeCeilingError
// states: a store failure means the answer is unknown, and carrying on with the
// zero value would silently mean "you have no drive" for a member who does —
// mounting nothing where an admin allocated something.
func writeDriveError(w http.ResponseWriter, err error) {
	switch {
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
		writeError(w, http.StatusInternalServerError, "resolve user drive: "+err.Error())
	}
}

// ─── the resolver ──────────────────────────────────────────────────────────

// resolveUserDrive returns the drive that applies to the caller on ctx, or nil
// when they have none. Resolution order, and each step is a decision rather
// than a fallback:
//
//  1. NO STORE ⇒ no drive. A build with no store holds no grants, so there is
//     nothing to resolve and the answer is 0.6's answer: no mount.
//  2. NO SUBJECTS ⇒ no drive. An admin token and local mode carry no per-human
//     identity, so there is no principal to name a home after and an `all`-tier
//     grant would hand every identity-less caller ONE shared directory. An SSO
//     ADMIN is not in this arm and resolves like anyone else — the drive door
//     (GovernanceLimits.DenyUserDrive) is what keys on isOperator, not this.
//  3. UNANSWERABLE GROUP SNAPSHOT ⇒ user-tier only, and a refusal in exactly
//     one shape (see driveWithUnusableGroups).
//  4. STORE ERROR ⇒ ERROR. Never "no drive": a database hiccup would silently
//     drop a member's storage out of a run that then writes its work into a
//     container layer nobody keeps.
//  5. ErrNotFound ⇒ no drive. Absent row, absent behaviour — the same
//     back-compat doctrine an absent governance assignment follows.
//
// ponytail: no cache, matching effectiveCeiling's own note. One indexed read
// per resolve; a stale allocation is a correctness bug, not a slow page.
func (s *Server) resolveUserDrive(ctx context.Context) (*types.ResolvedDrive, error) {
	if s.cfg.Store == nil {
		return nil, nil
	}
	users, groups, stale := capabilitySubjects(ctx)
	if len(users) == 0 {
		return nil, nil
	}
	// TRUNCATED COUNTS AS STALE, the same PF-26 rule effectiveCeiling applies:
	// the group snapshot is sorted and cut at the cookie byte cap, so a member
	// in enough groups holds one that is present, non-nil and INCOMPLETE — and
	// the group whose grant carries their drive is exactly as likely to be
	// missing as any other.
	if stale || oidcGroupsTruncatedFromContext(ctx) {
		return s.driveWithUnusableGroups(ctx, users)
	}
	return s.resolveUserDriveFor(ctx, users, groups)
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
//   - NO GROUP-TIER GRANT EXISTS AT ALL. Nothing an unknown group could have
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
func (s *Server) driveWithUnusableGroups(ctx context.Context, users []string) (*types.ResolvedDrive, error) {
	d, g, tier, err := s.cfg.Store.ResolveUserDrive(ctx, users, nil)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("api: resolve user drive: %w", err)
	}
	if err == nil && tier == types.CapabilitySubjectUser {
		return newResolvedDrive(d, g, tier, users)
	}
	hasGroupTier, herr := s.cfg.Store.HasGroupTierDriveGrants(ctx)
	if herr != nil {
		return nil, fmt.Errorf("api: resolve user drive: %w", herr)
	}
	if hasGroupTier {
		return nil, errGroupsSnapshotStale
	}
	if err != nil {
		return nil, nil // ErrNotFound, and no group tier could have been hiding one
	}
	return newResolvedDrive(d, g, tier, users)
}

// resolveUserDriveFor is the store read plus the absent-row doctrine, shared by
// the enforcement path above and by the preview endpoint below so the two
// cannot answer differently for the same claims.
//
// ORDER MATTERS, and it is the same rule ceilingFromProfile states: the
// REAL-ERROR check runs FIRST, ahead of the nil-row one, because a failed
// resolve also returns nil rows — checking nil first would turn every store
// failure into "no grant matched" and mount nothing, which is the fail-quiet
// this file exists to refuse, arriving through the back door of a convenience
// guard.
//
// The nil-store guard is repeated here rather than left to resolveUserDrive
// because THIS is the door the preview handler enters through, and the ~30
// nil-store doubles in this package must not start panicking on it.
func (s *Server) resolveUserDriveFor(ctx context.Context, users, groups []string) (*types.ResolvedDrive, error) {
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
	return newResolvedDrive(d, g, tier, users)
}

// newResolvedDrive folds one winning (drive, grant) pair into everything a
// runner or a preview needs. Four folds, each of which could have gone the
// other way:
//
//   - A DISABLED WINNER IS PAUSED, and it returns before anything is derived.
//     The store hands back the row that won its tier whether or not it is
//     enabled (DESIGN §2.2), so this is the single place that tells the two
//     apart — and a paused drive mounts nothing, which makes a home name a
//     directory no consumer will ever ask for. Deriving one anyway is worse
//     than useless: DriveHomeName can FAIL, and a failure here would answer a
//     paused member with the wrong refusal entirely (422 "your claim cannot
//     name a directory") for a drive that was never going to mount.
//
//   - HOME OVERRIDE applies on the USER TIER ONLY. types.ValidateUserDriveGrant
//     already refuses to store one on a group or all row; the tier gate is
//     repeated here because a row written by an older binary — or by hand —
//     would otherwise hand an entire group ONE directory, which is precisely
//     the isolation the per-user subdirectory buys.
//
//   - SIZE is the override when the admin set one, else the drive's. 0 means
//     "unset" on both, so a grant that never mentions size inherits rather than
//     zeroing the allocation.
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
	tier types.CapabilitySubjectType, users []string) (*types.ResolvedDrive, error) {
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
	// THE MANAGED/`email_local` REFUSAL, REPEATED — and repeated for the same
	// reason the home-override tier gate below is, on a row written by an older
	// binary or by hand. A managed object is named by the HOME alone, so under
	// `email_local` two principals whose addresses share the part before the "@"
	// resolve to ONE volume or PVC, and the driver's own collision check cannot
	// see it (the object IS labelled with this drive). Deriving the home anyway
	// would hand the second principal the first one's storage.
	//
	// It runs BEFORE the derivation, not after: the home it would derive is
	// exactly the colliding one, and a check downstream of it would be reasoning
	// about a value it had already accepted.
	if d.Backend.Kind() == types.DriveKindManaged && d.HomeTemplate == types.HomeTemplateEmailLocal {
		slog.Warn("wardynd: user drive: a managed drive is templated on the email local part, which cannot name one object per person",
			slog.String("drive", d.Name), slog.String("backend", string(d.Backend)),
			slog.String("home_template", string(d.HomeTemplate)))
		// REFUSED_BACKEND's frozen shape, whose parenthesised half is where the
		// admin's diagnosis goes — the same reuse driveShareIsBindable makes for
		// the roots ceiling, and the same honest reading: from the member's side
		// "the roots moved" and "this row could not be authored today" are one
		// fact, which is that this deployment cannot mount their drive.
		return nil, fmt.Errorf("%w: drive: this deployment cannot mount your drive "+
			"(its directory name comes from your email address, which cannot name one %s object per person — "+
			"ask an admin to change how this drive names directories)", errDriveUnmountable, d.Backend)
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
		slog.Warn("wardynd: user drive: a home name could not be derived for this principal",
			slog.String("drive", d.Name), slog.String("backend", string(d.Backend)),
			slog.String("home_template", string(d.HomeTemplate)), slog.String("err", err.Error()))
		return nil, fmt.Errorf("%w: drive: your %s cannot name a directory "+
			"(lowercase letters and digits, then `. _ -`, up to 63 characters) — "+
			"ask an admin to set your directory name", errDriveUnmountable, d.HomeTemplate)
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
// DELIBERATELY NOT a shape guess. There is no "an @ makes it an email" rule
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

// ─── POST /drives/preview ──────────────────────────────────────────────────

// userDrivePreviewResponse names the drive a principal carrying those claims
// would mount, the TIER of the grant that won, and — the field an admin
// actually came for — the exact OBJECT NAME, so the offboarding runbook's
// `docker volume rm` / `kubectl delete pvc` can be copied rather than computed
// from a hash by hand.
//
// EVERY FIELD IS omitempty and an empty object is the answer for "no grant
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
	// well-formed object name that NO RUN WILL EVER MOUNT — derived from the
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
// THAT SINGLE CALL IS THE WHOLE POINT, and it is the lesson the governance
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
// NOT AUDITED, for the reason the governance preview states: it is typed into,
// mints nothing and changes nothing, and a row per keystroke would turn the
// append-only log into a record of every claim an admin tried.
//
// ErrNotFound is a RESULT, not a failure — answered as the empty object. A
// grant that matches but yields no usable home is a 422 naming the reason,
// because that IS the answer the admin needs (their template does not fit this
// person's claims); only a real store failure is a 500.
//
// Routing is D2's (SUPER-only, beside the /drives CRUD family); this handler is
// deliberately complete so that registration is one line.
func (s *Server) handlePreviewUserDrive(w http.ResponseWriter, r *http.Request) {
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
	resolved, err := s.resolveUserDriveFor(r.Context(), users, groups)
	if err != nil {
		writeDriveError(w, err)
		return
	}
	if resolved == nil {
		writeJSON(w, http.StatusOK, userDrivePreviewResponse{})
		return
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
