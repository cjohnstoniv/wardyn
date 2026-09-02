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
	// The default, and the only sane template for a MANAGED backend, where
	// Wardyn is naming an object nothing else has an opinion about.
	HomeTemplateHash HomeTemplate = "hash"
	// HomeTemplateSub uses the sign-in subject claim verbatim.
	HomeTemplateSub HomeTemplate = "sub"
	// HomeTemplateEmailLocal uses the part of the email claim before the "@" —
	// the usual shape of a corporate home directory (alice@corp -> alice).
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
type ResolvedDrive struct {
	Drive UserDrive             `json:"drive"`
	Grant UserDriveGrant        `json:"grant"`
	Tier  CapabilitySubjectType `json:"tier"`
	// HomeName is the per-user segment: the subdirectory of a share, or the
	// suffix of a managed object's name.
	HomeName string `json:"home_name"`
	// ObjectName is what the runner asks the substrate for — a volume name, a
	// PVC name, or an absolute host path.
	ObjectName  string             `json:"object_name"`
	SizeMiB     int                `json:"size_mib,omitempty"`
	Writable    bool               `json:"writable,omitempty"`
	Enforcement StorageEnforcement `json:"enforcement"`
	// Paused is set when the grant that WON is disabled: Drive and Grant are
	// that row, the size and mode folds still ran, nothing is derived (no
	// HomeName, no ObjectName) and nothing may be mounted. A paused row wins
	// its tier rather than falling through to the wider row beneath it
	// (DESIGN §2.2), so an admin turning one off cannot silently hand that
	// member the everyone drive instead.
	Paused bool `json:"paused,omitempty"`
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
	// StorageClass is the provisioner a managed k8s_pvc claim asks for; "" means
	// the cluster default, and it is meaningless on every other backend. Carried
	// here because a substrate never reads the database — the resolver hands it
	// everything a mount needs (UserDrive.StorageClass).
	StorageClass string `json:"storage_class,omitempty"`
	// HomeName is the per-user segment ObjectName was built from, carried for
	// labels and for the audit row an operator reads when reclaiming.
	HomeName string `json:"home_name"`
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

// driveHomeSegmentRe is the shape a home name may take on a DOCKER backend: a
// single path segment that is also a legal Docker volume-name component, so
// one string can be both a subdirectory of a share and the suffix of a named
// volume.
//
// It is NOT a DNS-1123 name and never was — `_` is not legal in one, and a
// trailing `-` or `.` is not either. That claim used to sit on this comment
// and was the bug driveHomeSegmentK8sRe below exists to close: a k8s drive
// whose home came through here would validate and then be rejected by the
// apiserver at bind time, on somebody's run.
//
// The leading character is [a-z0-9] specifically to exclude a LEADING DOT: a
// dotfile home would put a drive inside the credential deny class the member
// mount rules already refuse by segment, and "..", the traversal, is excluded
// by the same clause.
var driveHomeSegmentRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// driveHomeSegmentK8sRe is the same segment on a KUBERNETES backend, where it
// is concatenated into a PVC NAME (DriveObjectName) and must therefore be a
// DNS-1123 subdomain: lowercase alphanumerics, `-` and `.` in the middle only,
// and at most 63 characters. The `{0,61}` middle plus the two anchored
// alphanumerics is that 63 written as the regex rather than as a second length
// check something could forget.
//
// THE MOTIVATING CASE IS NOT HYPOTHETICAL: an Entra `sub` is base64url and
// routinely carries `_`, so a `k8s_pvc` drive templated on `sub` passes the
// Docker rule, is stored, and then fails at bind time for every member it
// allocates. Refusing it in DriveHomeName makes that a resolve-time
// REFUSED_HOME_INVALID naming the claim an admin has to override, at the
// moment the admin previews the allocation, instead of a cluster error inside
// somebody's run.
var driveHomeSegmentK8sRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)

// driveHomeSegmentOK reports whether seg is a legal home name for backend b,
// picking the rule from the substrate that has to hold the name.
//
// The ".." clause is the one thing the k8s regex above cannot say: consecutive
// dots are not a legal DNS-1123 subdomain (each dot-separated label must be
// non-empty), and the same two characters are the path traversal a share's
// subdirectory bind must never carry. One check, both reasons.
func driveHomeSegmentOK(b DriveBackend, seg string) bool {
	if b.RunnerTarget() != "k8s" {
		return driveHomeSegmentRe.MatchString(seg)
	}
	return driveHomeSegmentK8sRe.MatchString(seg) && !strings.Contains(seg, "..")
}

// driveHomeSegmentRule renders b's rule for the error message that refuses a
// name — the pattern itself, so an admin reading a refusal sees the shape they
// have to satisfy rather than a prose paraphrase of it that can drift.
func driveHomeSegmentRule(b DriveBackend) string {
	if b.RunnerTarget() != "k8s" {
		return driveHomeSegmentRe.String()
	}
	return driveHomeSegmentK8sRe.String() + " (a DNS-1123 subdomain: it becomes part of a PVC name)"
}

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
	// neither and why it is the only template a MANAGED backend should use.
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

// driveObjectPrefix is shared by both managed object names so an operator can
// find every Wardyn-created volume or claim with one glob, and so nothing
// Wardyn did not create can be mistaken for a drive.
const driveObjectPrefix = "wardyn-drive-"

// driveSlugMaxLen bounds the drive-name half of a PVC name. A PVC name is
// capped at 253 characters and the home segment can be 63, so 40 leaves room
// for both plus the prefix with no arithmetic anywhere else.
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
//   - docker_volume  -> wardyn-drive-<home>
//   - k8s_pvc[_static] -> wardyn-drive-<drive-slug>-<home>
//   - host_path      -> <host_root>/<home>, cleaned
//
// The PVC name carries the DRIVE's slug and the volume name does not, and that
// asymmetry is the namespaces they live in: Docker volume names are global to a
// daemon that also holds one drive's worth of state, while PVCs share a
// namespace with every other claim in it — including a second drive's, whose
// home for the same member is a different hash but whose PURPOSE an operator
// reading `kubectl get pvc` has no other way to tell.
func DriveObjectName(d UserDrive, home string) string {
	switch d.Backend {
	case DriveBackendHostPath:
		return filepath.Join(filepath.Clean(d.HostRoot), home)
	case DriveBackendK8sPVC, DriveBackendK8sPVCStatic:
		return driveObjectPrefix + driveSlug(d.Name) + "-" + home
	default:
		return driveObjectPrefix + home
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
	if d.Backend.Kind() == DriveKindShare && d.HomeTemplate == HomeTemplateHash {
		return fmt.Errorf("home_template %q is not allowed on a share backend — a share's directories are named by "+
			"your directory, so pick %s or %s", HomeTemplateHash, HomeTemplateSub, HomeTemplateEmailLocal)
	}
	if d.SizeMiB < 0 {
		return fmt.Errorf("size_mib: must not be negative")
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

// driveTextIsClean reports whether s carries no control characters — the same
// field hygiene the capability-grant and governance-assignment writes apply,
// restated here because this package must not import internal/api.
func driveTextIsClean(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
