// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// ─── write-boundary validation ────────────────────────────────────────────────

// maxUserDriveNameLen bounds a drive name on write — a human handle rendered in
// a picker and folded into an object name, not a description. Matched to the
// governance profile-name cap so the two admin objects behave alike.
const maxUserDriveNameLen = 128

// maxUserDriveFieldLen bounds the free-text columns (host_root, storage_class,
// subject, home_override) so a row cannot be used as storage.
const maxUserDriveFieldLen = 512

// ValidateUserDrive normalizes d in place and validates it, in the same shape
// validateGovernanceAssignment applies to an assignment: trim, lowercase where
// the value is matched case-insensitively downstream, length, no control
// characters.
//
// runnerTarget is the deployment's own Config.RunnerTarget, and the match is
// REQUIRED rather than advisory: a k8s drive on a Docker deployment is a row
// whose every resolution ends in a runtime refusal, so it is a 400 at the write
// instead of a 422 on somebody's run three days later. An empty or unknown
// target refuses every backend — a control plane that dispatches nowhere has no
// business registering storage for it.
//
// It deliberately does NOT check host_root against the deployment's env ceiling
// (see UserDriveHostRootCheck): this package cannot read the environment, and a
// ceiling that lived in two places would be a ceiling one of them could forget.
func ValidateUserDrive(d *UserDrive, runnerTarget string) error {
	d.Name = strings.TrimSpace(d.Name)
	if d.Name == "" {
		return fmt.Errorf("name: required")
	}
	if len(d.Name) > maxUserDriveNameLen || !driveTextIsClean(d.Name) {
		return fmt.Errorf("name: invalid")
	}
	if DriveSlug(d.Name) == "" {
		return fmt.Errorf("name: must contain at least one letter or digit (it names the drive's storage objects)")
	}
	if !d.Backend.Valid() {
		return fmt.Errorf("backend: invalid %q", d.Backend)
	}
	if d.Backend.RunnerTarget() != runnerTarget {
		return fmt.Errorf("backend %q cannot be mounted by this deployment's runner (%s)", d.Backend, runnerTarget)
	}
	if err := validateDriveHostRoot(d); err != nil {
		return err
	}
	if d.StorageClass = strings.TrimSpace(d.StorageClass); d.StorageClass != "" {
		if d.Backend != DriveBackendK8sPVC {
			return fmt.Errorf("storage_class: only a %q drive provisions a volume; %q does not",
				DriveBackendK8sPVC, d.Backend)
		}
		if len(d.StorageClass) > maxUserDriveFieldLen || !driveTextIsClean(d.StorageClass) {
			return fmt.Errorf("storage_class: invalid")
		}
	}
	if d.HomeTemplate == "" {
		d.HomeTemplate = HomeTemplateHash
	}
	if !d.HomeTemplate.Valid() {
		return fmt.Errorf("home_template: invalid %q", d.HomeTemplate)
	}
	// A host_path drive's directories are named by whoever owns the tree, so a
	// hash would name a directory that does not exist and Wardyn does not create
	// one: a missing home on a share is a refusal, not a mkdir. It is the ONLY
	// backend this applies to — a k8s_pvc_static claim's name is Wardyn's to
	// mint, so `hash` is allowed there and is the default.
	if ShareBackendRejectsTemplate(d.Backend, d.HomeTemplate) {
		return fmt.Errorf("home_template %q is not allowed on a share backend — a share's directories are named by "+
			"your directory, so pick %s or %s", HomeTemplateHash, HomeTemplateSub, HomeTemplateEmailLocal)
	}
	// AND THE MIRROR IMAGE, for a reason that is security rather than symmetry.
	// A MANAGED object's name ENDS in the HOME segment, and everything before it
	// is fixed for the drive (DriveObjectName: `wardyn-drive-<drive-slug>-<home>`),
	// so under `email_local` two principals whose addresses share the part before
	// the "@" — alice@corp.example and alice@acquired.example, the ordinary
	// shape of a merged tenant — resolve to ONE object name. On a SHARE that is
	// an admin's problem with a filesystem they own and can see; on a managed
	// backend Wardyn ALLOCATES the object, and the Docker driver adopts a volume
	// labelled with this same drive rather than refusing it (ensureDriveVolume's
	// inspect-hit arm keys on the drive id, which matches). The result is two
	// people silently sharing one drive, with write access to each other's files
	// whenever the allocation is writable.
	//
	// REFUSED, not warned: the collision is invisible from the admin surface
	// (both allocations preview a perfectly well-formed object name), and the
	// the remedy is cheap — `hash`, which puts the subject in the digest and is
	// already the default.
	//
	// SCOPE WIDENED (2026-09-03): the refusal covers EVERY non-hash template on a
	// managed backend, not just email_local. `sub` was previously allowed here as
	// the collision-free alternative, which answered collision but never asked the
	// EXPOSURE question DriveSubjectHash settles for labels: the home segment is
	// concatenated into the object name (DriveObjectName ->
	// `wardyn-drive-<drive-slug>-<home>`), and an object name is read by
	// `docker volume ls` / `kubectl get pvc` WITHOUT
	// the inspect or describe a label needs. So `sub` was refused in the LESS
	// exposed place and permitted in the MORE exposed one. `hash` is unique AND
	// reveals nothing, so nothing is lost: a managed volume's name is not a thing
	// a human navigates, which is precisely what a share backend is for — and
	// share backends keep every template.
	if ManagedBackendRejectsTemplate(d.Backend, d.HomeTemplate) {
		if d.Backend.Kind() == DriveKindShare {
			// k8s_pvc_static: only the collision half applies, so the sentence
			// names the collision and the two templates that remain.
			return fmt.Errorf("home_template %q is not allowed on a %s drive — Wardyn mints the claim name, so "+
				"%s folds two people whose addresses share the part before the \"@\" onto ONE claim and they bind each "+
				"other's volume. Use %s (recommended: the preview endpoint prints the exact claim name to pre-provision) "+
				"or %s",
				d.HomeTemplate, d.Backend, HomeTemplateEmailLocal, HomeTemplateHash, HomeTemplateSub)
		}
		return fmt.Errorf("home_template %q is not allowed on a managed backend — Wardyn names the object after the "+
			"directory, so the template lands in a %s object name that `docker volume ls` and `kubectl get pvc` show "+
			"without inspecting anything; %s also collides two people whose addresses share the part before the \"@\". "+
			"Use %s, which is unique and reveals nothing; a share backend keeps every template",
			d.HomeTemplate, d.Backend, HomeTemplateEmailLocal, HomeTemplateHash)
	}
	if d.SizeMiB < 0 {
		return fmt.Errorf("size_mib: must not be negative")
	}
	if d.SizeMiB > maxUserDriveInt {
		return fmt.Errorf("size_mib: must be at most %d — %s", maxUserDriveInt, userDriveIntColumnReason)
	}
	// A k8s_pvc drive is the ONE backend where the size is not a display value:
	// it becomes the PVC's resources.requests.storage, and a claim requesting
	// zero bytes is rejected by the apiserver. So 0 — which every other backend
	// reads as "no allocation shown" — is a row whose every member's run would
	// fail at bind time on the cluster, and it is refused at authoring instead.
	if d.Backend == DriveBackendK8sPVC && d.SizeMiB <= 0 {
		return fmt.Errorf("size_mib must be above 0 for a %s drive — it is the volume request", DriveBackendK8sPVC)
	}
	if d.Reclaim == "" {
		d.Reclaim = DriveReclaimRetain
	}
	if !d.Reclaim.Valid() {
		return fmt.Errorf("reclaim: invalid %q", d.Reclaim)
	}
	return nil
}

// validateDriveHostRoot is ValidateUserDrive's host_path arm: the root must be
// absolute and already cleaned for a host_path drive, and EMPTY for every other
// backend.
//
// Requiring the cleaned form rather than cleaning it silently is deliberate:
// the stored string is the one an operator compares against the env allowlist
// by eye, and "/srv/homes/../homes" and "/srv/homes" reading as the same root
// while LOOKING different is how an allowlist review misses a row.
func validateDriveHostRoot(d *UserDrive) error {
	d.HostRoot = strings.TrimSpace(d.HostRoot)
	if d.Backend != DriveBackendHostPath {
		if d.HostRoot != "" {
			return fmt.Errorf("host_root: only a %q drive binds a host tree", DriveBackendHostPath)
		}
		return nil
	}
	if d.HostRoot == "" {
		return fmt.Errorf("host_root: required for a %q drive", DriveBackendHostPath)
	}
	if len(d.HostRoot) > maxUserDriveFieldLen || !driveTextIsClean(d.HostRoot) {
		return fmt.Errorf("host_root: invalid")
	}
	if !filepath.IsAbs(d.HostRoot) {
		return fmt.Errorf("host_root: must be an absolute path")
	}
	if filepath.Clean(d.HostRoot) != d.HostRoot {
		return fmt.Errorf("host_root: must be a cleaned path (%q)", filepath.Clean(d.HostRoot))
	}
	return nil
}

// ValidateUserDriveGrant normalizes g in place and validates it, applying the
// SAME subject hygiene validateGovernanceAssignment applies — trim, length, no
// control characters, and for a GROUP subject the shared CanonicalGroupSubject
// the login-time snapshot itself is built with — because the three tables are
// written against the identical subject vocabulary and resolved through the
// identical capabilitySubjects call. A subject normalized one way here and
// another way there is a row that silently never matches.
func ValidateUserDriveGrant(g *UserDriveGrant) error {
	if !g.SubjectType.Valid() {
		return fmt.Errorf("subject_type: invalid %q", g.SubjectType)
	}
	if g.DriveID == uuid.Nil {
		return fmt.Errorf("drive_id: required")
	}
	if g.SubjectType == CapabilitySubjectAll {
		// "all" names every signed-in human; the migration is explicit that
		// subject is '' for this type, so a caller-supplied value is dropped
		// rather than trusted — honouring it would create a second,
		// unreachable everyone row.
		g.Subject = ""
	} else {
		g.Subject = strings.TrimSpace(g.Subject)
		if g.Subject == "" {
			return fmt.Errorf("subject: required for subject_type %q", g.SubjectType)
		}
		if g.SubjectType == CapabilitySubjectGroup {
			// The group half of that hygiene is CanonicalGroupSubject, not a
			// lowercase. A group subject is matched by EXACT equality against
			// the login-time snapshot, which carries printable ASCII guarded
			// before the Unicode fold — so a plain ToLower both ACCEPTS a
			// subject no session can ever carry (an allocation that matches
			// nobody, while HasGroupTierDriveGrants counts the dead row as a
			// group tier that exists) and, worse, folds U+212A / U+0130 ONTO an
			// operator-authored ASCII group, binding a drive to a group the
			// author never named. This was the one write boundary of the three
			// left on the loose rule.
			subject, ok := CanonicalGroupSubject(g.Subject)
			if !ok {
				return fmt.Errorf("subject: must be printable ASCII — a group subject is matched against the login-time group snapshot, which carries printable ASCII only, so this value can never match anyone")
			}
			g.Subject = subject
		} else {
			// The USER half is CanonicalUserSubject, not a bare lowercase, and
			// for the escalating half of the same reason the group arm states.
			// strings.ToLower folds U+212A onto ASCII 'k' and U+0130 onto 'i',
			// so an admin who typed a look-alike spelling of a real human's
			// address had the allocation stored against THAT human's subject —
			// a drive handed to somebody nobody named, while the audit row shows
			// the exotic string that was actually typed. This was the third and
			// last write boundary left on the loose rule; the other two are
			// internal/api's permission and governance-assignment validators.
			//
			// It KEEPS a non-ASCII subject rather than refusing it, unlike the
			// group arm: a `sub` claim is whatever the identity provider issues,
			// and a boundary that refused one would refuse a real person. See
			// CanonicalUserSubject for what that costs and where it is settled.
			g.Subject = CanonicalUserSubject(g.Subject)
		}
		if len(g.Subject) > maxUserDriveFieldLen || !driveTextIsClean(g.Subject) {
			return fmt.Errorf("subject: invalid")
		}
	}
	if g.SizeMiBOverride < 0 {
		return fmt.Errorf("size_mib_override: must not be negative")
	}
	if g.SizeMiBOverride > maxUserDriveInt {
		return fmt.Errorf("size_mib_override: must be at most %d — %s", maxUserDriveInt, userDriveIntColumnReason)
	}
	if g.Priority > maxUserDriveInt || g.Priority < minUserDriveInt {
		return fmt.Errorf("priority: must be between %d and %d — %s", minUserDriveInt, maxUserDriveInt, userDriveIntColumnReason)
	}
	g.HomeOverride = strings.ToLower(strings.TrimSpace(g.HomeOverride))
	if g.HomeOverride == "" {
		return nil
	}
	// USER TIER ONLY. A home override on a group or all row would hand every
	// member of that group the SAME directory — the isolation a per-user
	// subdirectory buys, removed by a field that reads like a convenience.
	if g.SubjectType != CapabilitySubjectUser {
		return fmt.Errorf("home_override is accepted on a %s-tier allocation only — a group cannot share one directory",
			CapabilitySubjectUser)
	}
	// The DOCKER rule, and deliberately the looser of the two: this row holds a
	// drive_id, not the drive, so the backend that will have to hold the name
	// is not knowable here. DriveHomeName re-checks against the real backend at
	// resolve time — the same shape ValidateUserDrive takes with the host-root
	// ceiling it also cannot see.
	if !driveHomeSegmentRe.MatchString(g.HomeOverride) {
		return fmt.Errorf("home_override: %q is not a valid home name (%s)", g.HomeOverride, driveHomeSegmentRe)
	}
	return nil
}

// maxUserDriveInt / minUserDriveInt are the range EVERY integer column on these
// two tables actually has, and it is the COLUMN'S rather than a policy: 0054
// declares user_drives.size_mib, user_drive_grants.priority and
// user_drive_grants.size_mib_override as INT — PostgreSQL's 4-byte signed
// integer — while Go's int is 64-bit on every platform wardynd ships on. So a
// value in the gap validated here, was written, and the DATABASE refused it
// with SQLSTATE 22003 ("integer out of range"), which is neither ErrConflict
// nor ErrNotFound and therefore reached the admin as a 500 carrying the raw
// driver string. That is the shape the CreatePolicy contract exists to keep off
// the wire, and the row was the caller's to fix all along.
//
// A CEILING, NOT A SIZE OPINION. 2 PiB of size_mib is not a number this package
// has grounds to argue with; a number the column cannot hold is. Priority is
// bounded in BOTH directions because negative priorities are legitimate (a
// deliberate de-prioritised group row) and int32 is asymmetric.
const (
	maxUserDriveInt = math.MaxInt32
	minUserDriveInt = math.MinInt32
)

// userDriveIntColumnReason is the half of those three refusals that says WHY,
// written once so the three cannot drift into three different explanations of
// one column type.
const userDriveIntColumnReason = "the column is a 32-bit integer (migration 0054), and a larger value is refused by the database rather than stored"

// driveTextIsClean reports whether s carries no control characters — the same
// field hygiene the capability-grant and governance-assignment writes apply,
// restated here because this package must not import internal/api.
//
// unicode.IsControl covers C0 and DEL exactly as the hand-rolled loop did, and
// ALSO the C1 range U+0080-U+009F — strictly tighter at every call site, never
// looser. Same expression, same stance as internal/api's controlCharFree.
func driveTextIsClean(s string) bool {
	return !strings.ContainsFunc(s, unicode.IsControl)
}
