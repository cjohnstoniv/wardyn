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

// maxUserDriveNameLen bounds a drive name on write, matched to the governance
// profile-name cap so the two admin objects behave alike.
const maxUserDriveNameLen = 128

// maxUserDriveFieldLen bounds the free-text columns (host_root, storage_class,
// subject, home_override) so a row cannot be used as storage.
const maxUserDriveFieldLen = 512

// ValidateUserDrive normalizes d in place and validates it: trim, length, no
// control characters. runnerTarget must match d.Backend.RunnerTarget(), refused
// here rather than left to fail at run time; host_root is not checked against
// the deployment's env ceiling (see UserDriveHostRootCheck), since this package
// cannot read the environment.
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
	// one.
	if ShareBackendRejectsTemplate(d.Backend, d.HomeTemplate) {
		return fmt.Errorf("home_template %q is not allowed on a share backend — a share's directories are named by "+
			"your directory, so pick %s or %s", HomeTemplateHash, HomeTemplateSub, HomeTemplateEmailLocal)
	}
	// A managed object's name ends in the home segment and is readable via
	// `docker volume ls` / `kubectl get pvc` without inspecting anything; under
	// `email_local` two addresses sharing the part before "@" collide onto one
	// object, silently sharing a writable drive. `hash` is unique and reveals
	// nothing; a share backend keeps every template.
	if ManagedBackendRejectsTemplate(d.Backend, d.HomeTemplate) {
		if d.Backend.Kind() == DriveKindShare {
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
	// A k8s_pvc drive's size becomes the PVC's resources.requests.storage, and
	// the apiserver rejects a zero-byte claim, so 0 is refused here instead of
	// failing at bind time on the cluster.
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
// absolute and already cleaned for a host_path drive, and empty for every
// other backend (required cleaned rather than cleaned silently, since an
// operator compares the stored string against the env allowlist by eye).
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

// ValidateUserDriveGrant normalizes g in place and validates it: trim, length,
// no control characters, and for a GROUP subject the shared
// CanonicalGroupSubject the login-time snapshot is built with, so the subject
// vocabulary matches capabilitySubjects exactly.
func ValidateUserDriveGrant(g *UserDriveGrant) error {
	if !g.SubjectType.Valid() {
		return fmt.Errorf("subject_type: invalid %q", g.SubjectType)
	}
	if g.DriveID == uuid.Nil {
		return fmt.Errorf("drive_id: required")
	}
	if g.SubjectType == CapabilitySubjectAll {
		// subject is always '' for "all"; a caller-supplied value is dropped
		// rather than trusted, to avoid a second, unreachable everyone row.
		g.Subject = ""
	} else {
		g.Subject = strings.TrimSpace(g.Subject)
		if g.Subject == "" {
			return fmt.Errorf("subject: required for subject_type %q", g.SubjectType)
		}
		if g.SubjectType == CapabilitySubjectGroup {
			// Matched by exact equality against the login-time snapshot
			// (printable ASCII), via CanonicalGroupSubject rather than a plain
			// ToLower: ToLower would fold U+212A/U+0130 onto an operator-authored
			// ASCII group, binding the drive to a group the author never named.
			subject, ok := CanonicalGroupSubject(g.Subject)
			if !ok {
				return fmt.Errorf("subject: must be printable ASCII — a group subject is matched against the login-time group snapshot, which carries printable ASCII only, so this value can never match anyone")
			}
			g.Subject = subject
		} else if g.SubjectType == CapabilitySubjectUserType {
			// Kept verbatim: a type id is matched exactly against the stamped
			// type and never folded. Its shape and existence are checked by
			// internal/api, which can read the user_types table.
		} else {
			// CanonicalUserSubject, not a bare lowercase: ToLower folds U+212A/
			// U+0130 onto ASCII look-alikes, misdirecting the allocation. Unlike
			// the group arm, a non-ASCII subject is kept rather than refused,
			// since a `sub` claim is whatever the identity provider issues.
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
	// User tier only: a home override on a group or all row would hand every
	// member the SAME directory, removing the isolation a per-user subdirectory
	// buys.
	if g.SubjectType != CapabilitySubjectUser {
		return fmt.Errorf("home_override is accepted on a %s-tier allocation only — a group cannot share one directory",
			CapabilitySubjectUser)
	}
	// This row holds a drive_id, not the drive, so the backend that will hold
	// the name is not knowable here; DriveHomeName re-checks against the real
	// backend at resolve time.
	if !driveHomeSegmentRe.MatchString(g.HomeOverride) {
		return fmt.Errorf("home_override: %q is not a valid home name (%s)", g.HomeOverride, driveHomeSegmentRe)
	}
	return nil
}

// maxUserDriveInt / minUserDriveInt are the range EVERY integer column on
// these two tables actually has: migration 0054 declares size_mib, priority
// and size_mib_override as PostgreSQL INT (32-bit) while Go's int is 64-bit,
// so a value in the gap would otherwise reach the database and fail there as
// a raw 500 instead of a clean validation error. Priority is bounded both
// ways since negative priorities are legitimate and int32 is asymmetric.
const (
	maxUserDriveInt = math.MaxInt32
	minUserDriveInt = math.MinInt32
)

// userDriveIntColumnReason is the shared WHY text for those three refusals.
const userDriveIntColumnReason = "the column is a 32-bit integer (migration 0054), and a larger value is refused by the database rather than stored"

// driveTextIsClean reports whether s carries no control characters — restated
// here (rather than imported from internal/api) since this package must not
// import that one. Covers C0, DEL, and the C1 range U+0080-U+009F.
func driveTextIsClean(s string) bool {
	return !strings.ContainsFunc(s, unicode.IsControl)
}
