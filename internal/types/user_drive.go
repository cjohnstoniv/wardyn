// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// User drives (migration 0054): the wire + store types for an admin-registered
// per-user storage allocation and the row that grants one to a subject.
//
// Named `drive` throughout, and the noun/verb split is deliberate: a DRIVE is
// what an admin registers and allocates, MOUNT is what a run does with one. The
// member's run request carries only `{enabled, read_only}` and NEVER a path —
// the server derives the home name from the authenticated identity, so a member
// cannot name someone else's directory by typing it.
//
// Two kinds fall out of the backend rather than being stored beside it:
//   - MANAGED — Wardyn allocates the object (a per-user Docker named volume, a
//     per-user dynamic PVC).
//   - SHARE — the platform already has the tree (a host path the operator
//     mounted from NFS/SMB, an admin-precreated PVC) and Wardyn binds one
//     per-user subdirectory of it.
//
// Everything here is a CLOSED enum validated at the write boundary, which is
// what lets migration 0054 carry three CHECKs pinned against these constants by
// internal/db's TestClosedEnumChecksMatchConstants.
package types

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

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
	// mounted host-side (fstab/systemd cifs or nfs). Wardyn never performs the
	// share mount itself and therefore never holds a share credential. Share.
	// Docker only.
	DriveBackendHostPath DriveBackend = "host_path"
	// DriveBackendK8sPVC is a per-user dynamically-provisioned PVC. Managed.
	// Kubernetes only.
	DriveBackendK8sPVC DriveBackend = "k8s_pvc"
	// DriveBackendK8sPVCStatic is an admin-precreated PVC (typically an NFS/SMB
	// CSI volume). Wardyn only ever READS it: a missing claim is a refusal, not
	// a create. Share. Kubernetes only.
	DriveBackendK8sPVCStatic DriveBackend = "k8s_pvc_static"
)

// DriveBackends is the closed set in the order the admin surface shows them.
var DriveBackends = []DriveBackend{
	DriveBackendDockerVolume, DriveBackendHostPath,
	DriveBackendK8sPVC, DriveBackendK8sPVCStatic,
}

// Valid reports whether b is one of the four backends, mirroring
// CapabilitySubjectType.Valid — the API write boundary uses it in place of a
// second opinion about the CHECK.
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
// reads as a SHARE — the conservative half, since a share is never created and
// never deleted by Wardyn, so a bad row cannot make the control plane
// provision storage.
func (b DriveBackend) Kind() DriveKind {
	switch b {
	case DriveBackendDockerVolume, DriveBackendK8sPVC:
		return DriveKindManaged
	default:
		return DriveKindShare
	}
}

// RunnerTarget is the ONE Config.RunnerTarget value that can mount this
// backend, or "" for an unknown backend. It exists so the write boundary can
// refuse a k8s drive on a Docker deployment (a 400) rather than storing a row
// whose every resolution ends in a runtime refusal.
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
	// subject: deterministic, DNS-1123-safe for a PVC name, and it leaks no
	// identity into an object name an operator will read in `docker volume ls`.
	// The default, and the ONLY template a MANAGED backend may use — where
	// Wardyn is naming an object nothing else has an opinion about, a name
	// derived from a claim buys nothing and can collide (see the two claim
	// templates below); ValidateUserDrive refuses the others there.
	HomeTemplateHash HomeTemplate = "hash"
	// HomeTemplateSub uses the sign-in subject claim verbatim, lowercased.
	// SHARE backends only: it is not folded with the drive id, so one member's
	// two drives would be one object, and the lowercasing makes two subjects
	// differing only in case one home.
	HomeTemplateSub HomeTemplate = "sub"
	// HomeTemplateEmailLocal uses the part of the email claim before the "@" —
	// the usual shape of a corporate home directory (alice@corp -> alice).
	// SHARE backends only: the domain is dropped, so alice@corp.example and
	// alice@partner.example name ONE directory. On a share that directory was
	// minted by somebody else and the answer is a home_override on the
	// colliding person's allocation; on a managed backend Wardyn would be
	// minting the collision itself, so ValidateUserDrive refuses it there.
	//
	// THERE IS DELIBERATELY NO WHOLE-EMAIL TEMPLATE. An address carries an "@",
	// which driveHomeSegmentRe excludes and a DNS-1123 label could not hold
	// either, so such a template could only ever RESOLVE for a claim that was
	// not an address — a value that validates and then refuses every real
	// caller is a dead enum member, not an option. The corporate case it looked
	// like it served (a home directory named by the person's username) is
	// exactly this template; a home named anything else is a per-user
	// home_override on that person's grant.
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

// DriveReclaim is the DECLARED INTENT for a drive's objects when a grant goes
// away. v1 executes it by documented operator command, not by code: nothing in
// the control plane deletes a volume or a PVC, and the RBAC the k8s runner asks
// for deliberately carries no `delete` verb.
type DriveReclaim string

const (
	// DriveReclaimRetain keeps the object after the grant is removed — the
	// default, because the alternative default is data loss on an admin's
	// routine unassign.
	DriveReclaimRetain DriveReclaim = "retain"
	// DriveReclaimDelete records that the object SHOULD be reclaimed. It is a
	// note to the offboarding runbook, not an action.
	DriveReclaimDelete DriveReclaim = "delete"
)

// DriveReclaims is the closed set in admin-surface order.
var DriveReclaims = []DriveReclaim{DriveReclaimRetain, DriveReclaimDelete}

// Valid reports whether r is one of the two reclaim intents.
func (r DriveReclaim) Valid() bool {
	return r == DriveReclaimRetain || r == DriveReclaimDelete
}

// StorageEnforcement names WHAT ACTUALLY BINDS BYTES for a drive, and it exists
// because a size that nothing enforces must not be rendered as a limit.
//
// The honesty sentence this vocabulary carries, verbatim, wherever a size is
// shown: "Wardyn never enforces a drive's size itself. On Kubernetes the size
// is the volume request and the storage class decides whether it binds — block
// disks do, network-share provisioners do not. On Docker a managed drive has no
// byte cap, the same gap disk_mib has. A share is bounded by its own quota. The
// size you see is the allocation, not a guarantee."
//
// It is introduced ONCE here and is the vocabulary the two existing DiskMiB
// warn sites (the docker driver's storage-opt warning and the k8s sandbox's)
// adopt when they are consolidated — the third instance is the trigger, and
// this is the second.
type StorageEnforcement string

const (
	// StorageEnforcementFilesystem: the filesystem itself refuses the write
	// (XFS project quota). NOTHING in v1 reports this — it is the value the
	// documented operator recipe earns, and naming it here is what keeps a
	// future quota lane from inventing a fifth word.
	StorageEnforcementFilesystem StorageEnforcement = "filesystem"
	// StorageEnforcementRequest: the size is a scheduling REQUEST (a PVC's
	// resources.requests.storage). A block storage class binds it; an NFS/EFS
	// provisioner accepts it and enforces nothing.
	StorageEnforcementRequest StorageEnforcement = "request"
	// StorageEnforcementExternal: something outside Wardyn binds it — the NAS's
	// own quota. Wardyn displays the allocation and claims nothing.
	StorageEnforcementExternal StorageEnforcement = "external"
	// StorageEnforcementNone: nothing binds it. A Docker named volume has no
	// cap at all (--storage-opt size caps only the writable layer, never a
	// volume, and project quota needs a capability the control plane must not
	// gain).
	StorageEnforcementNone StorageEnforcement = "none"
)

// EnforcementFor maps a backend to what actually binds its bytes. An unknown
// backend reads as `none`: claiming enforcement for a row this binary does not
// understand is the one answer that could mislead an admin into trusting a cap.
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
// Name is the UNIQUE human handle: what an admin allocates by, what the console
// lists, the source of a PVC's slug, and the last tie-break in the resolver's
// ORDER BY — so LIMIT 1 stays deterministic when priority ties, exactly as
// GovernanceProfile.Name does for a ceiling.
//
// Writable is the drive's DEFAULT posture and it defaults to FALSE. A grant may
// narrow it and a run may narrow it again; neither may widen it.
type UserDrive struct {
	ID      uuid.UUID    `json:"id"`
	Name    string       `json:"name"`
	Backend DriveBackend `json:"backend"`
	// HostRoot is the operator-mounted tree a host_path drive binds a
	// subdirectory of. Absolute and cleaned, empty for every other backend. It
	// is authored in the DB by an admin and bound into OTHER PEOPLE's
	// sandboxes, so the security ceiling over it is the env allowlist
	// (UserDriveHostRootCheck), never this column alone.
	HostRoot string `json:"host_root,omitempty"`
	// StorageClass is the k8s_pvc provisioner to request; "" means the
	// cluster default. Meaningless for every other backend (a static PVC was
	// provisioned by someone else, and Docker has no such concept).
	StorageClass string       `json:"storage_class,omitempty"`
	HomeTemplate HomeTemplate `json:"home_template"`
	// SizeMiB is the ALLOCATION, not a guarantee — see StorageEnforcement. 0
	// means "no allocation shown" on every backend but k8s_pvc, where the value
	// IS the PVC's resources.requests.storage and 0 is therefore refused at the
	// write boundary rather than stored as a claim no cluster would bind.
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
// SubjectType REUSES CapabilitySubjectType — the same user/group/all vocabulary
// capability_grants and governance_assignments are written against, and the
// same one capabilitySubjects resolves a caller into. A second enum meaning the
// same three things is the dual-matcher drift this tree refuses everywhere: two
// definitions of "who" that disagree by one case is how a deny stops biting.
//
// Every override is a column on the BINDING row rather than a second table,
// because an override is a property of "this subject on this drive" and has no
// meaning apart from the pair.
type UserDriveGrant struct {
	ID          uuid.UUID             `json:"id"`
	SubjectType CapabilitySubjectType `json:"subject_type"`
	Subject     string                `json:"subject"`
	DriveID     uuid.UUID             `json:"drive_id"`
	// Priority breaks ties WITHIN a tier (higher wins) — the group tier's
	// working lever, since a member is typically in several groups at once. It
	// does NOT cross tiers.
	Priority int `json:"priority"`
	// SizeMiBOverride replaces the drive's allocation for this subject. 0 means
	// "use the drive's".
	SizeMiBOverride int `json:"size_mib_override,omitempty"`
	// WritableOverride is TRI-STATE on purpose: nil is "use the drive's
	// posture", and an explicit false is an admin saying "this subject reads
	// only" even on a writable drive. A plain bool could not tell the two
	// apart, so an unset override would silently mean read-only.
	WritableOverride *bool `json:"writable_override,omitempty"`
	// HomeOverride names this subject's directory verbatim (Bob's NAS home is
	// `bsmith`, whatever the template would derive). USER TIER ONLY: on a group
	// or all row it would give every member of the group ONE home, which is the
	// opposite of the isolation binding a per-user subdirectory buys.
	HomeOverride string `json:"home_override,omitempty"`
	// Enabled is the admin's off switch that keeps the row (and its overrides)
	// intact. A disabled row is IN the resolver's query and can win its tier:
	// the winner's bit is what folds to ResolvedDrive.Paused, so an admin
	// turning one off tells that member "your allocation is paused" instead of
	// silently dropping them through to the wider row beneath it.
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// UserDriveListItem is one row of the admin Drives table: the drive plus how
// many grants bind it.
//
// The count is a READ-SIDE derivation and lives here rather than on UserDrive
// so the write type stays exactly the row: a client that GETs a list and PUTs
// an item back cannot resurrect a stale count as data. It is what makes the
// delete affordance honest — a drive with grants answers 409 (ON DELETE
// RESTRICT), and an admin should see why before clicking.
type UserDriveListItem struct {
	UserDrive
	GrantCount int `json:"grant_count"`
}

// ResolvedDrive is THE answer for one principal: the winning drive, the grant
// that won, the tier it won at, and everything derived from the pair that a
// runner or a preview needs. Built once, by the API resolver, so no consumer
// re-derives a home name or an override.
//
// Writable is already folded (grant override over drive default) but NOT yet
// narrowed by the run request — a request may only narrow, and that fold
// belongs to the run-create seam that has the request in hand.
//
// NO json tags: this type crosses no wire. The preview and /me each compose
// their own response struct from it, deliberately — Grant is a whole admin
// grant row, and a struct that looks marshal-ready is a struct somebody
// marshals.
type ResolvedDrive struct {
	Drive UserDrive
	Grant UserDriveGrant
	Tier  CapabilitySubjectType
	// HomeName is the per-user segment: the subdirectory of a share, or the
	// suffix of a managed object's name.
	HomeName string
	// SubjectHash fingerprints the principal HomeName was derived FROM
	// (DriveSubjectHash), which the home itself cannot answer for: a managed
	// object's name carries the home and nothing else, so two principals whose
	// template collapses onto one home produce one object name and no way to
	// tell them apart. Carried here so the runner can stamp it on the object it
	// allocates and refuse one stamped for somebody else.
	SubjectHash string `json:"subject_hash,omitempty"`
	// ObjectName is what the runner asks the substrate for — a volume name, a
	// PVC name, or an absolute host path.
	ObjectName  string
	SizeMiB     int
	Writable    bool
	Enforcement StorageEnforcement
	// Paused is set when the grant that WON is disabled: Drive and Grant are
	// that row, the size and mode folds still ran, nothing is derived (no
	// HomeName, no ObjectName) and nothing may be mounted. A paused row wins
	// its tier rather than falling through to the wider row beneath it
	// (DESIGN §2.2), so an admin turning one off cannot silently hand that
	// member the everyone drive instead.
	Paused bool
}

// DriveMount is the RESOLVED answer the runner acts on: one principal's drive,
// folded with their grant's overrides and narrowed by their run request, in the
// shape a substrate can execute without re-reading a row or re-deriving a name.
//
// It is a SEPARATE type from ResolvedDrive on purpose. ResolvedDrive is the
// admin-facing answer ("who gets what, and why") and carries the whole drive
// and grant rows; this is the run-facing one and carries only what a mount
// needs. Handing the runner the grant row would put an admin's authoring
// fields — priority, subject, the override tri-state — inside the sandbox
// wiring, where nothing may branch on them and a later field could.
//
// ReadOnly rather than Writable, and the inversion is deliberate: every mount
// type the substrates already speak (runner.Mount, a k8s VolumeMount, a Docker
// mount) says ReadOnly, and a field that flips sense at the boundary is how a
// narrowing becomes a widening in a refactor.
//
// It does NOT ride RunPolicySpec.WorkspaceMounts. A drive is per-PRINCIPAL and
// a workspace mount is per-ROW; keeping them apart is what keeps composer.Clamp,
// validateWorkspaceSources, primaryWorkspacePath and the k8s blanket host-bind
// refusal from each needing a drive exemption.
type DriveMount struct {
	// DriveID is the user_drives row this mount came from. Labels and reclaim
	// sweeps group by it: an object name is per-PRINCIPAL, so it cannot answer
	// "every object this drive allocated" — the question an offboarding sweep
	// and a `kubectl get pvc -l` both ask.
	DriveID uuid.UUID    `json:"drive_id"`
	Backend DriveBackend `json:"backend"`
	// ObjectName is what the substrate is asked for: a Docker volume name, a
	// PVC name, or the absolute host path of this principal's subdirectory.
	// Derived once by the resolver (DriveObjectName) so no runner re-computes a
	// hash.
	ObjectName string `json:"object_name"`
	// HostRoot is THIS drive's own share root (UserDrive.HostRoot), carried on a
	// host_path mount so the driver can assert the bind stays inside the tree
	// this drive was authored against. Empty on every other backend, which has
	// no host tree at all.
	//
	// It does not replace the deployment's WARDYN_USER_DRIVE_HOST_ROOTS ceiling
	// and is not redundant with it. The ceiling is the OPERATOR's outer bound
	// over every drive at once, so on a deployment with two share drives it
	// cannot tell one drive's tree from the other's: a home replaced host-side
	// by a link to the same-named home under the OTHER drive's root is inside
	// the ceiling, is not a root, and still carries this principal's name. Only
	// the drive's own root refuses that — and only the ceiling stops an
	// admin-authored row from naming a tree the operator never allowed. The
	// driver asserts BOTH (runner.UserDriveHomeWithinItsRoot).
	HostRoot string `json:"host_root,omitempty"`
	// DriveName is the drive object's human name (UserDrive.Name), carried for
	// the AUDIT row alone: run.drive.mount's Target is "<drive>/<home>" on a
	// share, so the member reading their own run's rows learns which drive and
	// which directory without being handed the operator's absolute host path —
	// which stays in the payload's `object`, where an operator reads it.
	DriveName string `json:"drive_name,omitempty"`
	// StorageClass is the provisioner a managed k8s_pvc claim asks for; "" means
	// the cluster default, and it is meaningless on every other backend. Carried
	// here because a substrate never reads the database — the resolver hands it
	// everything a mount needs (UserDrive.StorageClass).
	StorageClass string `json:"storage_class,omitempty"`
	// HomeName is the per-user segment ObjectName was built from, carried for
	// labels and for the audit row an operator reads when reclaiming.
	HomeName string `json:"home_name"`
	// SubjectHash is the non-PII fingerprint of the principal this mount was
	// resolved for (types.DriveSubjectHash). It is DEFENCE IN DEPTH over the
	// managed-backend collision the write boundary already refuses: an object
	// name is per-HOME, so a stored row that predates that refusal can still
	// resolve two principals onto one volume, and the driver stamps this on the
	// object it creates so the second one is refused instead of adopted.
	//
	// A digest, never the claim: a Docker label is echoed by `docker volume
	// inspect` to anybody who can reach the daemon, so the subject itself must
	// not be written there.
	SubjectHash string
	// Target is the reserved in-container path (runner.DriveTarget). Carried
	// rather than assumed so a runner never hard-codes the string, and so the
	// reserved-target refusal and the mount agree by construction.
	Target      string             `json:"target"`
	ReadOnly    bool               `json:"read_only,omitempty"`
	SizeMiB     int                `json:"size_mib,omitempty"`
	Enforcement StorageEnforcement `json:"enforcement"`
}

// UserDriveHostRootCheck is the signature of the deployment's ENV CEILING over
// admin-authored host_path drives (WARDYN_USER_DRIVE_HOST_ROOTS), returning nil
// when hostRoot is inside an allowed root.
//
// It is a hook rather than a rule inside ValidateUserDrive because this package
// cannot read the environment and must not: the ceiling is operator/MDM-set,
// exactly like the member-mount roots, and site config (a full-replace row one
// bad PUT can blank) is the wrong home for a security ceiling. The API write
// boundary composes the two — shape here, ceiling there — and an UNSET env
// means no host_path drive may be authored at all, the same fail-closed posture
// WARDYN_MEMBER_WORKSPACE_ROOTS takes.
type UserDriveHostRootCheck func(hostRoot string) error

// ─── derivation ───────────────────────────────────────────────────────────────

// driveHomeHashLen is how many hex characters of the sha256 a `hash` home
// carries. 20 hex = 80 bits, which is collision-free for any plausible member
// count and leaves the whole name (d- + 20) at 22 characters — short enough
// that `wardyn-drive-<slug>-<home>` stays well inside a PVC's 253-character
// name limit.
const driveHomeHashLen = 20

// DriveHomeName derives the per-user home segment for one principal.
//
// PRECEDENCE, and the first rule is the whole point of the override existing:
//
//  1. override (a user-tier grant's home_override) WINS. An admin who has
//     written down "Bob's directory on the NAS is bsmith" is stating a fact
//     about a filesystem Wardyn does not own, and no template may out-vote it.
//  2. hash — `d-` + the first 20 hex of sha256(drive id + "\n" + subject). The
//     drive id is in the digest so one member's two drives never collide, and
//     the "\n" separator keeps a concatenation from being ambiguous.
//  3. sub / email_local — the CLAIM, lowercased, and email_local truncated at
//     the first "@".
//
// EVERY non-hash path is then checked against driveHomeSegmentRe and an
// unusable claim is an ERROR, NEVER A GUESS. Guessing here is not a cosmetic
// failure: a fabricated segment either collides with another member's home (two
// people sharing a directory neither was granted) or escapes it. The remedy the
// error points at is a home_override on that member's grant.
//
// `subject` is the identity the home is derived FROM, and the caller selects it
// because only the caller holds the labelled claims: the caller's stable
// primary subject for `hash` and `sub`, the email claim for `email_local`. It
// is never the GRANT's subject — a group grant's subject would give an entire
// group one home, and re-pointing a grant would move a member's data.
func DriveHomeName(d UserDrive, subject, override string) (string, error) {
	// THE OVERRIDE IS NOT SUBJECT TO THE MANAGED HASH-ONLY RULE, deliberately.
	// That rule (ValidateUserDrive) refuses a TEMPLATE on a managed backend
	// because a template derives the home from the sign-in subject AUTOMATICALLY
	// — every allocation publishes its principal into an object name as a
	// mechanical consequence, with nobody deciding per person. An override is the
	// opposite: one literal an admin typed for one named allocation, the same
	// decision they make naming a directory on a share. Asked and answered here
	// so the next audit does not have to re-derive it: automatic derivation from
	// the subject is refused; an operator's explicit choice remains theirs.
	if seg := strings.ToLower(strings.TrimSpace(override)); seg != "" {
		// Re-checked against THIS drive's backend rule, not just the write
		// boundary's: ValidateUserDriveGrant holds only the grant row and
		// cannot see which substrate the drive it points at lands on, so an
		// override legal for Docker can be stored against a k8s drive. An
		// admin's fact about a filesystem still cannot out-vote what the
		// apiserver will accept.
		if !driveHomeSegmentOK(d.Backend, seg) {
			return "", fmt.Errorf("home_override %q is not a valid home name (%s)", override, driveHomeSegmentRule(d.Backend))
		}
		return seg, nil
	}
	subject = strings.ToLower(strings.TrimSpace(subject))
	if subject == "" {
		return "", fmt.Errorf("no subject to derive a home name from")
	}
	// `d-` + hex satisfies BOTH backend rules by construction — no `_`, no dot,
	// no trailing `-`, 22 characters — which is why it is checked against
	// neither and why it is the only template a MANAGED backend MAY use
	// (ValidateUserDrive refuses the claim templates on one).
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

// DriveSubjectHash fingerprints the principal a drive object was allocated to,
// in the one shape a substrate LABEL may carry it: the first 20 hex of
// sha256(subject), lowercased and trimmed exactly as DriveHomeName folds it.
//
// NON-PII IS THE WHOLE REQUIREMENT. A label is echoed verbatim by `docker volume
// inspect` and `kubectl describe` to anybody who can reach the daemon or the
// namespace, so the sign-in subject itself — or an address — must not be written
// there. A digest answers the only question the driver asks ("is the object I
// found the object THIS principal was allocated?") and answers no other.
//
// It reuses driveHomeHashLen deliberately: the same 80 bits, collision-free for
// any plausible member count, and one number to reason about rather than two.
// The drive id is NOT in this digest, unlike DriveHomeName's — the label lives
// beside `wardyn.drive`, which already carries the id, and folding it in would
// make one principal's fingerprint differ per drive for no reader's benefit.
//
// An EMPTY subject returns "" rather than the digest of "": the caller then
// writes no label at all, which is the label-less state the restore path
// already tolerates. Hashing the empty string would mint a fingerprint every
// identity-less caller shares — the one value that could make two principals
// look like one.
func DriveSubjectHash(subject string) string {
	subject = strings.ToLower(strings.TrimSpace(subject))
	if subject == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:])[:driveHomeHashLen]
}

// driveObjectPrefix is shared by every minted object name so an operator can
// find every Wardyn-created volume or claim with one glob, and so nothing
// Wardyn did not create can be mistaken for a drive.
const driveObjectPrefix = "wardyn-drive-"

// driveSlugMaxLen bounds the drive-name half of a minted object name. A PVC
// name is capped at 253 characters (a Docker volume name at 255) and the home
// segment can be 63, so 40 leaves room for both plus the prefix with no
// arithmetic anywhere else.
const driveSlugMaxLen = 40

// driveSlugUnsafeRe matches every run of characters a DNS-1123 name may not
// carry. Collapsed to a single "-" so "Corp NAS (eng)" and "corp-nas-eng"
// cannot produce two different objects for one drive.
var driveSlugUnsafeRe = regexp.MustCompile(`[^a-z0-9]+`)

// driveSlug folds a drive's human name into the DNS-1123 fragment a PVC name
// carries. ValidateUserDrive refuses a name that folds to nothing, so this
// never returns "" for a stored drive.
func driveSlug(name string) string {
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
// EVERY name Wardyn MINTS carries the DRIVE's slug, because the home alone does
// not identify a drive. It reads as though it did — a `hash` home folds the
// drive id, so one member's two drives are already two homes — but a
// home_override does not: the admin writes "Bob's directory is bsmith" on the
// GRANT, and the grant is the row that gets re-pointed from one drive to
// another (user_drive_grants is UNIQUE on (subject_type, subject), so
// re-pointing IS how a member moves between drives). With no slug both sides of
// that re-point named one volume, so Bob mounted the old drive's contents under
// the new drive's name, size and writable posture, and an offboarding command
// naming `wardyn-drive-bsmith` could not say which drive it was reclaiming.
//
// The volume arm carried no slug on the reasoning that Docker volume names are
// global to a daemon holding one drive's worth of state — an assumption nothing
// enforces: user_drives has no cardinality constraint and a deployment may
// register any number of docker_volume drives. The slug costs a longer name and
// buys the same (drive, home) scoping the PVC arm always had, which is also
// what lets an operator reading `docker volume ls` tell two drives apart, as
// `kubectl get pvc` already could.
//
// A SHARE keeps its own shape: <host_root> already scopes it, and the directory
// under it was named by whoever owns the tree, not by Wardyn — a slug there
// would name a directory that does not exist.
func DriveObjectName(d UserDrive, home string) string {
	switch d.Backend {
	case DriveBackendHostPath:
		return filepath.Join(filepath.Clean(d.HostRoot), home)
	default:
		return driveObjectPrefix + driveSlug(d.Name) + "-" + home
	}
}

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
	if driveSlug(d.Name) == "" {
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
	// A SHARE's directories are named by whoever owns the share, so a hash
	// would name a directory that does not exist and Wardyn does not create
	// one: a missing home on a share is a refusal, not a mkdir.
	if ShareBackendRejectsTemplate(d.Backend, d.HomeTemplate) {
		return fmt.Errorf("home_template %q is not allowed on a share backend — a share's directories are named by "+
			"your directory, so pick %s or %s", HomeTemplateHash, HomeTemplateSub, HomeTemplateEmailLocal)
	}
	// AND THE MIRROR IMAGE, for a reason that is security rather than symmetry.
	// A MANAGED object is named by the HOME and by nothing else
	// (DriveObjectName: `wardyn-drive-<home>`, `wardyn-drive-<slug>-<home>`), so
	// under `email_local` two principals whose addresses share the part before
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
	// concatenated into the object name (DriveObjectName -> `wardyn-drive-<home>`),
	// and an object name is read by `docker volume ls` / `kubectl get pvc` WITHOUT
	// the inspect or describe a label needs. So `sub` was refused in the LESS
	// exposed place and permitted in the MORE exposed one. `hash` is unique AND
	// reveals nothing, so nothing is lost: a managed volume's name is not a thing
	// a human navigates, which is precisely what a share backend is for — and
	// share backends keep every template.
	if ManagedBackendRejectsTemplate(d.Backend, d.HomeTemplate) {
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
// SAME subject hygiene validateGovernanceAssignment applies — trim, lowercase,
// length, no control characters — because the two tables are written against
// the identical subject vocabulary and resolved through the identical
// capabilitySubjects call. A subject normalized one way here and another way
// there is a row that silently never matches.
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
		g.Subject = strings.ToLower(strings.TrimSpace(g.Subject))
		if g.Subject == "" {
			return fmt.Errorf("subject: required for subject_type %q", g.SubjectType)
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

// ManagedBackendRejectsTemplate is THE managed-backend home-template rule: a
// managed object is NAMED by the home segment (DriveObjectName ->
// `wardyn-drive-<home>`), and an object name is printed by `docker volume ls`
// and `kubectl get pvc` without the inspect or describe a label needs — so on a
// managed backend only `hash`, which is unique and reveals nothing, may name
// one. A share backend keeps every template.
//
// It is a function, and exported, because the rule has TWO enforcement points
// and they had drifted apart. The write boundary (Validate, above) was widened
// from `email_local` to every non-hash template; the RUN-TIME resolver
// (newResolvedDrive) was not, so a legacy or hand-written `sub` row was refused
// at authoring and still mounted — putting the sign-in subject in the object
// name, which is the exposure the widening exists to prevent. One predicate, so
// a third site cannot diverge again.
func ManagedBackendRejectsTemplate(backend DriveBackend, tmpl HomeTemplate) bool {
	return backend.Kind() == DriveKindManaged && tmpl != "" && tmpl != HomeTemplateHash
}

// ShareBackendRejectsTemplate is the MIRROR rule, and it is exported for the
// same reason its twin above is: it had exactly one enforcement point.
//
// A SHARE's directories are named by whoever owns the share, and Wardyn never
// mkdir's on one — so a `hash` home names a directory that does not exist and
// nothing will create it. The write boundary refused that row; the resolver did
// not, so a row written before the rule (or by hand) resolved cleanly and the
// member met the MISSING-HOME refusal instead — "directory
// d-00e23f375d35be941331 does not exist on the share — ask an admin to create
// it", which names a directory nobody could ever have made and asks the admin
// to create a digest. The remedy the member was handed was the wrong one for
// the row they actually had.
//
// The empty template is excluded on both sides because ValidateUserDrive
// defaults it before this is asked, and a resolver looking at a legacy row with
// no template must fall through to the derivation rather than refuse a shape
// this rule has no opinion about.
func ShareBackendRejectsTemplate(backend DriveBackend, tmpl HomeTemplate) bool {
	return backend.Kind() == DriveKindShare && tmpl == HomeTemplateHash
}
