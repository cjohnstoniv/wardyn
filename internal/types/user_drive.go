// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// User drives (migration 0054): the wire + store types for an admin-registered
// per-user storage allocation and the row that grants one to a subject.
//
// Named `drive` throughout: a DRIVE is what an admin registers and allocates,
// MOUNT is what a run does with one. The member's run request carries only
// `{enabled, read_only}` and NEVER a path — the server derives the home name
// from the authenticated identity.
//
// Two kinds fall out of the backend rather than being stored beside it:
//   - MANAGED — Wardyn allocates the object (a per-user Docker named volume, a
//     per-user dynamic PVC).
//   - SHARE — the platform already has the tree and Wardyn binds one
//     per-user subdirectory of it.
//
// Everything here is a CLOSED enum validated at the write boundary, pinned by
// migration 0054's three CHECKs against internal/db's
// TestClosedEnumChecksMatchConstants.
package types

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ─── the closed enums ─────────────────────────────────────────────────────────

// DriveBackend names WHERE a drive's bytes live and therefore WHICH runner can
// mount it. Closed and complete; migration 0054's user_drives.backend CHECK is
// pinned against these constants.
type DriveBackend string

const (
	// DriveBackendDockerVolume is a per-user Docker named volume, created by
	// Wardyn on first use. Managed. Docker only.
	DriveBackendDockerVolume DriveBackend = "docker_volume"
	// DriveBackendHostPath binds <host_root>/<home> from a tree the OPERATOR
	// mounted host-side. Wardyn never performs the share mount itself and
	// therefore never holds a share credential. Share. Docker only.
	DriveBackendHostPath DriveBackend = "host_path"
	// DriveBackendK8sPVC is a per-user dynamically-provisioned PVC. Managed.
	// Kubernetes only.
	DriveBackendK8sPVC DriveBackend = "k8s_pvc"
	// DriveBackendK8sPVCStatic is an admin-precreated PVC. Wardyn only ever
	// READS it: a missing claim is a refusal, not a create. Share. Kubernetes
	// only.
	DriveBackendK8sPVCStatic DriveBackend = "k8s_pvc_static"
)

// DriveBackends is the closed set in the order the admin surface shows them.
var DriveBackends = []DriveBackend{
	DriveBackendDockerVolume, DriveBackendHostPath,
	DriveBackendK8sPVC, DriveBackendK8sPVCStatic,
}

// Valid reports whether b is one of the four backends, mirroring
// CapabilitySubjectType.Valid.
func (b DriveBackend) Valid() bool {
	switch b {
	case DriveBackendDockerVolume, DriveBackendHostPath, DriveBackendK8sPVC, DriveBackendK8sPVCStatic:
		return true
	default:
		return false
	}
}

// DriveKind is the managed-vs-share split. DERIVED from the backend and never
// stored: two columns that must agree are two columns that can disagree, and
// the disagreement would decide whether Wardyn CREATES an object or refuses
// because it is missing.
type DriveKind string

const (
	// DriveKindManaged: Wardyn allocates the object for the member.
	DriveKindManaged DriveKind = "managed"
	// DriveKindShare: the object exists already and Wardyn only binds it.
	DriveKindShare DriveKind = "share"
)

// Kind derives the managed/share split from the backend. An unknown backend
// reads as a SHARE — the conservative half, since Wardyn never creates or
// deletes a share.
func (b DriveBackend) Kind() DriveKind {
	switch b {
	case DriveBackendDockerVolume, DriveBackendK8sPVC:
		return DriveKindManaged
	default:
		return DriveKindShare
	}
}

// RunnerTarget is the ONE Config.RunnerTarget value that can mount this
// backend, or "" for an unknown backend — lets the write boundary refuse a
// k8s drive on a Docker deployment (400) rather than store a row that can
// never resolve.
func (b DriveBackend) RunnerTarget() string {
	switch b {
	case DriveBackendDockerVolume, DriveBackendHostPath:
		return "docker"
	case DriveBackendK8sPVC, DriveBackendK8sPVCStatic:
		return "k8s"
	default:
		return ""
	}
}

// HomeTemplate names WHICH identity a drive's per-user home name is derived
// from. Closed; pinned against user_drives.home_template's CHECK.
type HomeTemplate string

const (
	// HomeTemplateHash is `d-` + a truncated sha256 over the drive id and the
	// subject: deterministic, DNS-1123-safe, and leaks no identity into an
	// object name an operator reads. The default, and the ONLY template a
	// MANAGED backend may use (a derived name buys nothing there and can
	// collide — see the claim templates below); ValidateUserDrive refuses the
	// others on managed.
	HomeTemplateHash HomeTemplate = "hash"
	// HomeTemplateSub uses the sign-in subject claim verbatim, lowercased.
	// SHARE backends only: not folded with the drive id, so one member's two
	// drives would be one object, and two subjects differing only in case
	// collapse to one home.
	HomeTemplateSub HomeTemplate = "sub"
	// HomeTemplateEmailLocal uses the part of the email claim before the "@"
	// (alice@corp -> alice). SHARE backends only: the domain is dropped, so
	// alice@corp.example and alice@partner.example name ONE directory — the
	// answer is a home_override on the colliding person's allocation; a
	// managed backend would mint the collision itself, so it's refused there.
	//
	// THERE IS DELIBERATELY NO WHOLE-EMAIL TEMPLATE: an address carries an
	// "@", which driveHomeSegmentRe excludes and a DNS-1123 label cannot
	// hold, so it could only ever resolve for a claim that was not an
	// address. The corporate case it looked like it served is exactly this
	// template; anything else is a per-user home_override.
	HomeTemplateEmailLocal HomeTemplate = "email_local"
)

// HomeTemplates is the closed set in admin-surface order.
var HomeTemplates = []HomeTemplate{HomeTemplateHash, HomeTemplateSub, HomeTemplateEmailLocal}

// Valid reports whether t is one of the three templates.
func (t HomeTemplate) Valid() bool {
	switch t {
	case HomeTemplateHash, HomeTemplateSub, HomeTemplateEmailLocal:
		return true
	default:
		return false
	}
}

// DriveObjectScheme names which half of DriveObjectName's minted form carries
// the drive — the variable-width slug (migration 0054,
// `wardyn-drive-<drive-slug>-<home>`) or the drive's own fixed-width id
// (migration 0067, `wardyn-drive-<drive-id-hex>-<home>`). Closed; pinned
// against user_drives.object_scheme's CHECK.
//
// IT IS NOT CLIENT-AUTHORED. store.UpsertUserDrive derives it entirely ('id'
// on every INSERT, carried through unchanged on every UPDATE) and never reads
// this field off the request — neither substrate can rename a storage object,
// so a row keeps whatever scheme it was created under for as long as it
// exists. It rides the wire (json:"object_scheme") only so a GET and its
// round-tripped PUT agree; driveIdentityFields compares it like any other
// identity field, so a forged flip answers 409 rather than being silently
// dropped.
type DriveObjectScheme string

const (
	// DriveObjectSchemeSlug is the original form: DriveSlug at a variable
	// offset before <home>. The column's DEFAULT, so every pre-0067 row reads
	// as this and stays this permanently (DriveSlug is not injective, so only
	// rewriting the object itself could change what a stored row binds).
	DriveObjectSchemeSlug DriveObjectScheme = "slug"
	// DriveObjectSchemeID is the drive's own UUID, dashless (DriveObjectID),
	// at a FIXED offset before <home> — nothing about the drive's name can
	// shift the boundary. Every row created from 0067 onward gets this
	// unconditionally.
	DriveObjectSchemeID DriveObjectScheme = "id"
)

// DriveObjectSchemes is the closed set, in the order a row can only ever move
// through (a legacy row stays slug forever; a new row is always id).
var DriveObjectSchemes = []DriveObjectScheme{DriveObjectSchemeSlug, DriveObjectSchemeID}

// Valid reports whether s is one of the two schemes.
func (s DriveObjectScheme) Valid() bool {
	return s == DriveObjectSchemeSlug || s == DriveObjectSchemeID
}

// DriveReclaim is the DECLARED INTENT for a drive's objects when a grant goes
// away. v1 executes it by documented operator command, not by code: nothing
// in the control plane deletes a volume or a PVC, and the k8s runner's RBAC
// deliberately carries no `delete` verb.
type DriveReclaim string

const (
	// DriveReclaimRetain keeps the object after the grant is removed — the
	// default, since the alternative default is data loss on a routine unassign.
	DriveReclaimRetain DriveReclaim = "retain"
	// DriveReclaimDelete records that the object SHOULD be reclaimed — a note
	// to the offboarding runbook, not an action.
	DriveReclaimDelete DriveReclaim = "delete"
)

// DriveReclaims is the closed set in admin-surface order.
var DriveReclaims = []DriveReclaim{DriveReclaimRetain, DriveReclaimDelete}

// Valid reports whether r is one of the two reclaim intents.
func (r DriveReclaim) Valid() bool {
	return r == DriveReclaimRetain || r == DriveReclaimDelete
}

// StorageEnforcement names WHAT ACTUALLY BINDS BYTES for a drive, so a size
// nothing enforces is never rendered as a limit.
//
// The honesty sentence this vocabulary carries, verbatim, wherever a size is
// shown: "Wardyn never enforces a drive's size itself. On Kubernetes the size
// is the volume request and the storage class decides whether it binds. On
// Docker a managed drive has no byte cap. A share is bounded by its own
// quota. The size you see is the allocation, not a guarantee."
//
// Surfaced, not just logged: runner.Capabilities.EphemeralDiskEnforcement
// carries it to the admin setup status.
type StorageEnforcement string

const (
	// StorageEnforcementFilesystem: the filesystem itself refuses the write
	// (XFS project quota). NOTHING in v1 reports this yet — naming it here
	// keeps a future quota lane from inventing a fifth word.
	StorageEnforcementFilesystem StorageEnforcement = "filesystem"
	// StorageEnforcementEviction: the kubelet enforces it, MEASURED
	// PERIODICALLY — over the limit the POD IS KILLED, not the write. The
	// agent never sees ENOSPC; in-flight work is lost. A burst between two
	// housekeeping ticks can overshoot before the kubelet looks.
	StorageEnforcementEviction StorageEnforcement = "eviction"
	// StorageEnforcementRequest: the size is a scheduling REQUEST. A block
	// storage class binds it; an NFS/EFS provisioner accepts it and enforces
	// nothing.
	StorageEnforcementRequest StorageEnforcement = "request"
	// StorageEnforcementExternal: something outside Wardyn binds it (the
	// NAS's own quota). Wardyn displays the allocation and claims nothing.
	StorageEnforcementExternal StorageEnforcement = "external"
	// StorageEnforcementNone: nothing binds it (a Docker named volume has no
	// cap at all).
	StorageEnforcementNone StorageEnforcement = "none"
)

// EnforcementFor maps a backend to what actually binds its bytes. An unknown
// backend reads as `none` — the one answer that cannot mislead an admin into
// trusting a cap.
func EnforcementFor(b DriveBackend) StorageEnforcement {
	switch b {
	case DriveBackendK8sPVC:
		return StorageEnforcementRequest
	case DriveBackendHostPath, DriveBackendK8sPVCStatic:
		return StorageEnforcementExternal
	default:
		return StorageEnforcementNone
	}
}

// ─── the rows ─────────────────────────────────────────────────────────────────

// UserDrive is one admin-registered drive (migration 0054's user_drives row).
//
// Name is the UNIQUE human handle: what an admin allocates by, what the
// console lists, the source of a PVC's slug, and the last tie-break in the
// resolver's ORDER BY.
//
// Writable is the drive's DEFAULT posture and defaults to FALSE. A grant may
// narrow it and a run may narrow it again; neither may widen it.
type UserDrive struct {
	ID      uuid.UUID    `json:"id"`
	Name    string       `json:"name"`
	Backend DriveBackend `json:"backend"`
	// HostRoot is the operator-mounted tree a host_path drive binds a
	// subdirectory of. Absolute and cleaned, empty for every other backend.
	// Authored by an admin and bound into OTHER PEOPLE's sandboxes, so the
	// security ceiling is the env allowlist (UserDriveHostRootCheck), never
	// this column alone.
	HostRoot string `json:"host_root,omitempty"`
	// StorageClass is the k8s_pvc provisioner to request; "" means the
	// cluster default. Meaningless for every other backend.
	StorageClass string       `json:"storage_class,omitempty"`
	HomeTemplate HomeTemplate `json:"home_template"`
	// ObjectScheme names which half of a minted object name carries the drive
	// — see DriveObjectScheme. NOT client-authored: exists on the wire only
	// so a GET and a round-tripped PUT agree.
	ObjectScheme DriveObjectScheme `json:"object_scheme,omitempty"`
	// SizeMiB is the ALLOCATION, not a guarantee — see StorageEnforcement. 0
	// means "no allocation shown" except on k8s_pvc, where it IS the PVC's
	// requests.storage and is refused at the write boundary.
	SizeMiB   int          `json:"size_mib,omitempty"`
	Writable  bool         `json:"writable,omitempty"`
	Reclaim   DriveReclaim `json:"reclaim"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	CreatedBy string       `json:"created_by,omitempty"`
}

// UserDriveGrant allocates one drive to one subject (migration 0054's
// user_drive_grants row).
//
// SubjectType REUSES CapabilitySubjectType, the same vocabulary
// capability_grants and governance_assignments use — a second enum meaning
// the same thing is the dual-matcher drift this tree refuses everywhere.
//
// Every override is a column on the BINDING row rather than a second table,
// since an override has no meaning apart from the (subject, drive) pair.
type UserDriveGrant struct {
	ID          uuid.UUID             `json:"id"`
	SubjectType CapabilitySubjectType `json:"subject_type"`
	Subject     string                `json:"subject"`
	DriveID     uuid.UUID             `json:"drive_id"`
	// Priority breaks ties WITHIN a tier (higher wins) — the group tier's
	// working lever. Does NOT cross tiers.
	Priority int `json:"priority"`
	// SizeMiBOverride replaces the drive's allocation for this subject. 0
	// means "use the drive's".
	SizeMiBOverride int `json:"size_mib_override,omitempty"`
	// WritableOverride is TRI-STATE on purpose: nil is "use the drive's
	// posture", explicit false is "read only even on a writable drive" — a
	// plain bool could not tell the two apart.
	WritableOverride *bool `json:"writable_override,omitempty"`
	// HomeOverride names this subject's directory verbatim. USER TIER ONLY:
	// on a group/all row it would give every member ONE home, the opposite of
	// per-user isolation.
	HomeOverride string `json:"home_override,omitempty"`
	// Enabled is the admin's off switch that keeps the row (and overrides)
	// intact. A disabled row still wins its tier: the winner's bit folds to
	// ResolvedDrive.Paused, so the member sees "paused" rather than silently
	// falling through to the wider row beneath it.
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// UserDriveListItem is one row of the admin Drives table: the drive plus how
// many grants bind it.
//
// The count is a READ-SIDE derivation kept off UserDrive so a client that GETs
// a list and PUTs an item back cannot resurrect a stale count. It's what makes
// the delete affordance honest — a drive with grants answers 409.
type UserDriveListItem struct {
	UserDrive
	GrantCount int `json:"grant_count"`
}

// ResolvedDrive is THE answer for one principal: the winning drive, the grant
// that won, the tier it won at, and everything derived from the pair. Built
// once by the API resolver so no consumer re-derives a home name or override.
//
// Writable is already folded (grant override over drive default) but NOT yet
// narrowed by the run request — that fold belongs to the run-create seam.
//
// NO json tags: this type crosses no wire. The preview and /me each compose
// their own response struct from it — Grant is a whole admin grant row, and a
// struct that looks marshal-ready is a struct somebody marshals.
type ResolvedDrive struct {
	Drive UserDrive
	Grant UserDriveGrant
	Tier  CapabilitySubjectType
	// HomeName is the per-user segment: the subdirectory of a share, or the
	// suffix of a managed object's name.
	HomeName string
	// SubjectHash fingerprints the principal HomeName was derived FROM
	// (DriveSubjectHash) — the home itself can't answer for this, since two
	// principals whose template collapses onto one home produce one object
	// name. Carried so the runner can stamp it and refuse one stamped for
	// somebody else.
	SubjectHash string `json:"subject_hash,omitempty"`
	// ObjectName is what the runner asks the substrate for — a volume name, a
	// PVC name, or an absolute host path.
	ObjectName  string
	SizeMiB     int
	Writable    bool
	Enforcement StorageEnforcement
	// Paused is set when the grant that WON is disabled: nothing is derived
	// (no HomeName, no ObjectName) and nothing may be mounted. A paused row
	// still wins its tier (DESIGN §2.2) rather than falling through to the
	// wider row beneath it.
	Paused bool
}

// DriveMount is the RESOLVED answer the runner acts on: one principal's
// drive, folded with grant overrides and narrowed by their run request, in
// the shape a substrate can execute without re-reading a row.
//
// A SEPARATE type from ResolvedDrive on purpose: that one is admin-facing and
// carries the whole drive/grant rows; this is run-facing and carries only
// what a mount needs — an admin's authoring fields have no business inside
// the sandbox wiring.
//
// ReadOnly rather than Writable, deliberately: every mount type the
// substrates already speak says ReadOnly, and a field that flips sense at the
// boundary is how a narrowing becomes a widening in a refactor.
//
// Does NOT ride RunPolicySpec.WorkspaceMounts: a drive is per-PRINCIPAL and a
// workspace mount is per-ROW, which keeps composer.Clamp and the k8s blanket
// host-bind refusal from needing a drive exemption.
type DriveMount struct {
	// DriveID is the user_drives row this mount came from. Labels and reclaim
	// sweeps group by it, since an object name is per-PRINCIPAL and cannot
	// answer "every object this drive allocated".
	DriveID uuid.UUID    `json:"drive_id"`
	Backend DriveBackend `json:"backend"`
	// ObjectName is what the substrate is asked for — a Docker volume name, a
	// PVC name, or the absolute host path. Derived once by the resolver.
	ObjectName string `json:"object_name"`
	// HostRoot is THIS drive's own share root, carried on a host_path mount
	// so the driver can assert the bind stays inside it. Does NOT replace the
	// deployment's WARDYN_USER_DRIVE_HOST_ROOTS ceiling: the ceiling is the
	// operator's outer bound over EVERY drive at once and cannot tell one
	// drive's tree from another's, while only the drive's own root refuses a
	// home replaced host-side by a link into a different drive's tree. The
	// driver asserts BOTH.
	HostRoot string `json:"host_root,omitempty"`
	// DriveName is the drive's human name, carried for the AUDIT row alone
	// (run.drive.mount's Target is "<drive>/<home>" on a share) so a member
	// learns which drive without being handed the operator's absolute host
	// path.
	DriveName string `json:"drive_name,omitempty"`
	// StorageClass is the provisioner a managed k8s_pvc claim asks for; ""
	// means the cluster default. Carried here because a substrate never reads
	// the database.
	StorageClass string `json:"storage_class,omitempty"`
	// HomeName is the per-user segment ObjectName was built from, carried for
	// labels and the audit row.
	HomeName string `json:"home_name"`
	// SubjectHash is the non-PII fingerprint of the principal this mount was
	// resolved for. DEFENCE IN DEPTH over the managed-backend collision the
	// write boundary already refuses: a stored row that predates that refusal
	// can still resolve two principals onto one volume, so the driver stamps
	// this and refuses the second instead of adopting it. A digest, never the
	// claim — a Docker label is echoed by `docker volume inspect` to anybody
	// who can reach the daemon.
	SubjectHash string
	// Target is the reserved in-container path (runner.DriveTarget). Carried
	// rather than assumed so a runner never hard-codes the string.
	Target      string             `json:"target"`
	ReadOnly    bool               `json:"read_only,omitempty"`
	SizeMiB     int                `json:"size_mib,omitempty"`
	Enforcement StorageEnforcement `json:"enforcement"`
}

// UserDriveHostRootCheck is the signature of the deployment's ENV CEILING over
// admin-authored host_path drives (WARDYN_USER_DRIVE_HOST_ROOTS), returning
// nil when hostRoot is inside an allowed root.
//
// A hook rather than a rule inside ValidateUserDrive: this package cannot
// read the environment, and site config (a full-replace row one bad PUT can
// blank) is the wrong home for a security ceiling. The API write boundary
// composes the two — shape here, ceiling there. An UNSET env means no
// host_path drive may be authored at all.
type UserDriveHostRootCheck func(hostRoot string) error

// ─── derivation ───────────────────────────────────────────────────────────────

// driveHomeHashLen is how many hex characters of the sha256 a `hash` home
// carries. 20 hex = 80 bits, collision-free for any plausible member count,
// leaving the whole name (d- + 20) well inside a PVC's 253-character limit.
const driveHomeHashLen = 20

// DriveHomeName derives the per-user home segment for one principal.
//
// PRECEDENCE:
//
//  1. override (a user-tier grant's home_override) WINS — an admin's stated
//     fact about a filesystem Wardyn does not own, which no template may
//     out-vote.
//  2. hash — `d-` + the first 20 hex of sha256(drive id + "\n" + subject). The
//     drive id is in the digest so one member's two drives never collide.
//  3. sub / email_local — the CLAIM, lowercased, email_local truncated at the
//     first "@".
//
// EVERY non-hash path is checked against driveHomeSegmentRe; an unusable claim
// is an ERROR, NEVER A GUESS — a fabricated segment either collides with
// another member's home or escapes it. The remedy is a home_override.
//
// `subject` is the identity the home is derived FROM and the caller selects
// it (the caller's stable primary subject for `hash`/`sub`, the email claim
// for `email_local`) — never the GRANT's subject, since a group grant's
// subject would give an entire group one home.
func DriveHomeName(d UserDrive, subject, override string) (string, error) {
	// THE OVERRIDE IS NOT SUBJECT TO THE MANAGED HASH-ONLY RULE, deliberately:
	// that rule refuses a TEMPLATE deriving the home from the sign-in subject
	// automatically; an override is one literal an admin typed for one named
	// allocation, the same decision as naming a directory on a share.
	if seg := strings.ToLower(strings.TrimSpace(override)); seg != "" {
		// Re-checked against THIS drive's backend rule, not just the write
		// boundary's: ValidateUserDriveGrant can't see which substrate the
		// drive lands on, so an override legal for Docker could be stored
		// against a k8s drive.
		if !driveHomeSegmentOK(d.Backend, seg) {
			return "", fmt.Errorf("home_override %q is not a valid home name (%s)", override, driveHomeSegmentRule(d.Backend))
		}
		return seg, nil
	}
	subject = strings.ToLower(strings.TrimSpace(subject))
	if subject == "" {
		return "", fmt.Errorf("no subject to derive a home name from")
	}
	// `d-` + hex satisfies BOTH backend rules by construction, which is why
	// it's the only template a MANAGED backend MAY use.
	if d.HomeTemplate == HomeTemplateHash || d.HomeTemplate == "" {
		sum := sha256.Sum256([]byte(d.ID.String() + "\n" + subject))
		return "d-" + hex.EncodeToString(sum[:])[:driveHomeHashLen], nil
	}
	if !d.HomeTemplate.Valid() {
		return "", fmt.Errorf("home_template %q is not a known template", d.HomeTemplate)
	}
	seg := subject
	if d.HomeTemplate == HomeTemplateEmailLocal {
		at := strings.Index(seg, "@")
		if at < 0 {
			return "", fmt.Errorf("home_template %q needs an email claim, and %q is not one", d.HomeTemplate, subject)
		}
		seg = seg[:at]
	}
	if !driveHomeSegmentOK(d.Backend, seg) {
		return "", fmt.Errorf("home_template %q derived %q, which is not a valid home name (%s)",
			d.HomeTemplate, seg, driveHomeSegmentRule(d.Backend))
	}
	return seg, nil
}

// DriveSubjectHash fingerprints the principal a drive object was allocated
// to, in the one shape a substrate LABEL may carry it: the first 20 hex of
// sha256(subject), lowercased and trimmed exactly as DriveHomeName folds it.
//
// NON-PII IS THE WHOLE REQUIREMENT: a label is echoed verbatim by `docker
// volume inspect`/`kubectl describe` to anybody who can reach the daemon or
// namespace, so the sign-in subject itself must not be written there.
//
// Reuses driveHomeHashLen deliberately. The drive id is NOT in this digest,
// unlike DriveHomeName's — the label lives beside `wardyn.drive`, which
// already carries the id.
//
// An EMPTY subject returns "" rather than the digest of "": the caller then
// writes no label, avoiding a fingerprint every identity-less caller would
// share.
func DriveSubjectHash(subject string) string {
	subject = strings.ToLower(strings.TrimSpace(subject))
	if subject == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:])[:driveHomeHashLen]
}

// driveObjectPrefix is shared by every minted object name so an operator can
// find every Wardyn-created volume or claim with one glob.
const driveObjectPrefix = "wardyn-drive-"

// driveSlugMaxLen bounds the drive-name half of a minted object name. A PVC
// name is capped at 253 characters (Docker volume at 255) and the home
// segment can be 63, so 40 leaves room for both plus the prefix.
const driveSlugMaxLen = 40

// driveSlugUnsafeRe matches every run of characters a DNS-1123 name may not
// carry, collapsed to a single "-" so cosmetic variants fold to one object.
var driveSlugUnsafeRe = regexp.MustCompile(`[^a-z0-9]+`)

// DriveSlug folds a drive's human name into the DNS-1123 fragment a PVC name
// carries. ValidateUserDrive refuses a name that folds to nothing.
//
// EXPORTED BECAUSE THE FOLD IS NOT INJECTIVE AND SOMETHING HAS TO ENFORCE
// THAT: "Corp NAS" and "corp nas" fold to one slug, so UNIQUE(name) alone
// would let two different-looking drive names mint the SAME object for one
// home — caught today only at mount time by the runners' wardyn.drive label
// check. Migration 0061 makes the slug column UNIQUE for every backend Wardyn
// names, the namespace the collision is actually in; this function is what
// decides what that column holds.
func DriveSlug(name string) string {
	s := driveSlugUnsafeRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	s = strings.Trim(s, "-")
	if len(s) > driveSlugMaxLen {
		s = strings.Trim(s[:driveSlugMaxLen], "-")
	}
	return s
}

// DriveObjectName is what the runner asks the substrate for, given a home
// already derived by DriveHomeName:
//
//   - docker_volume    -> wardyn-drive-<drive-slug>-<home>
//   - k8s_pvc[_static] -> wardyn-drive-<drive-slug>-<home>
//   - host_path        -> <host_root>/<home>, cleaned
//
// EVERY name Wardyn MINTS carries the DRIVE's slug, because the home alone
// does not identify a drive: a home_override is a literal an admin typed on
// the GRANT, and the grant is the row that gets re-pointed between drives
// (user_drive_grants is UNIQUE on (subject_type, subject)) — without the
// slug, a re-point would leave two drives naming one volume.
//
// The volume arm carries the slug too even though nothing enforces one
// docker_volume drive per daemon, buying the same (drive, home) scoping the
// PVC arm always had.
//
// A SHARE keeps its own shape: <host_root> already scopes it, and the
// directory under it was named by whoever owns the tree.
//
// THE DRIVE HALF IS d.ObjectScheme, beside DriveSlug rather than replacing it
// (migration 0061's unique index still keys on the slug). DriveObjectSchemeID
// puts DriveObjectID(d.ID) there instead — 32 hex characters, always, so
// <home> starts at a FIXED offset no crafted name can shift (issue #163). The
// empty scheme reads as slug, matching the column's own DEFAULT.
func DriveObjectName(d UserDrive, home string) string {
	if !DriveObjectNamedByWardyn(d.Backend) {
		return filepath.Join(filepath.Clean(d.HostRoot), home)
	}
	if d.ObjectScheme == DriveObjectSchemeID {
		return driveObjectPrefix + DriveObjectID(d.ID) + "-" + home
	}
	return driveObjectPrefix + DriveSlug(d.Name) + "-" + home
}

// DriveObjectID is the drive's own id, dashless — the fixed-width fragment
// DriveObjectSchemeID puts where DriveSlug would otherwise go: always 32
// lowercase hex characters, unlike DriveSlug whose length and content depend
// on caller input.
func DriveObjectID(id uuid.UUID) string {
	return strings.ReplaceAll(id.String(), "-", "")
}

// DriveObjectNamedByWardyn reports whether WARDYN MINTS this backend's
// storage object name rather than joining a path somebody else already
// created.
//
// IT IS NOT THE MANAGED/SHARE SPLIT: DriveKind answers "does Wardyn CREATE
// the object?"; this answers "does Wardyn NAME it?". k8s_pvc_static is a
// SHARE by the first question and a Wardyn-named object by the second (a
// claim is addressed by the name Wardyn asks for). Only host_path is on the
// other side.
//
// The SAME expression DriveObjectName branches on, called rather than
// restated.
func DriveObjectNamedByWardyn(b DriveBackend) bool {
	return b != DriveBackendHostPath
}

// ManagedBackendRejectsTemplate is THE managed-backend home-template rule: a
// managed object's name ENDS in the home segment and is printed by `docker
// volume ls`/`kubectl get pvc` with no inspect/describe needed — so on a
// managed backend only `hash`, unique and revealing nothing, may name one. A
// share backend keeps every template.
//
// A function, and exported, because the rule has TWO enforcement points that
// had drifted apart: the write boundary was widened from `email_local` to
// every non-hash template, but the run-time resolver was not, so a legacy
// `sub` row was refused at authoring and still mounted — putting the sign-in
// subject in the object name. One predicate, so a third site cannot diverge
// again.
func ManagedBackendRejectsTemplate(backend DriveBackend, tmpl HomeTemplate) bool {
	if tmpl == "" || tmpl == HomeTemplateHash {
		return false
	}
	// THE AXIS IS WHO NAMES THE OBJECT, not who creates it — true of every
	// object WARDYN NAMES, which is why keying on DriveKind put
	// k8s_pvc_static on the wrong side of both halves of the rule.
	if !DriveObjectNamedByWardyn(backend) {
		return false
	}
	if backend.Kind() == DriveKindManaged {
		return true
	}
	// k8s_pvc_static: Wardyn names the claim but does NOT create it. The
	// EXPOSURE half can't refuse `sub` here — an admin must pre-provision
	// claims under names it can recognise, and `hash` (now the default) is
	// available for one who'd rather use the preview endpoint. The COLLISION
	// half stays non-negotiable: `email_local` folds two addresses onto ONE
	// claim name, and the static driver's identity check has no per-drive
	// labels to tell them apart.
	return tmpl == HomeTemplateEmailLocal
}

// ShareBackendRejectsTemplate is the MIRROR rule, exported for the same
// reason: it had exactly one enforcement point.
//
// A SHARE's directories are named by whoever owns the share, and Wardyn never
// mkdir's on one — so a `hash` home names a directory that will never exist.
// The write boundary refused that row; the resolver did not, so a legacy row
// resolved cleanly and the member met a MISSING-HOME refusal naming a digest
// nobody could have created — the wrong remedy for the row they had.
//
// The empty template is excluded on both sides: ValidateUserDrive defaults it
// before this is asked, and a resolver looking at a legacy row with no
// template must fall through to derivation.
func ShareBackendRejectsTemplate(backend DriveBackend, tmpl HomeTemplate) bool {
	// KEYED ON WHO NAMES THE OBJECT, not DriveKind — true of host_path (whose
	// object is a path under a share-owner's tree) and NOT true of
	// k8s_pvc_static (whose name Wardyn mints exactly as for a managed
	// claim). Reading it off the managed/share split would have forced every
	// static allocation to publish the member's subject or address in a name
	// Wardyn itself minted.
	return !DriveObjectNamedByWardyn(backend) && tmpl == HomeTemplateHash
}
