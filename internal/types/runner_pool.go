// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// The runner pool contract (0.9). A pool is a stable set of runners: an
// organization pool of remote-hosted runners, or one person's own user pool of
// their own claimed self-hosted runners and the remote-hosted runners an
// administrator made available to people's pools. Choosing one constrains where
// a run may go; it never grants a runner, a credential, a drive or a
// confinement class, and it never makes another person's runner a candidate.

// RunnerPoolScope says whose a pool is. It is fixed for the life of a pool:
// an organization pool is the administrators', a user pool its owner's.
type RunnerPoolScope string

const (
	// RunnerPoolOrganization pools hold remote-hosted runners only, and have no owner.
	RunnerPoolOrganization RunnerPoolScope = "organization"
	// RunnerPoolUser pools belong to one person.
	RunnerPoolUser RunnerPoolScope = "user"
)

// Valid reports whether s is one of the two scopes.
func (s RunnerPoolScope) Valid() bool {
	return s == RunnerPoolOrganization || s == RunnerPoolUser
}

// Placement is the run placement a pool of this scope serves today: an
// organization pool places on its remote-hosted runners, a user pool on its
// owner's own self-hosted runners (a user pool's remote-hosted members are
// listed but not yet candidates). Empty for a value outside the set.
func (s RunnerPoolScope) Placement() Placement {
	switch s {
	case RunnerPoolOrganization:
		return PlacementRemote
	case RunnerPoolUser:
		return PlacementLocal
	}
	return ""
}

// RunnerKind is the kind of a runner: a person's registered computer, or a
// Docker host or Kubernetes cluster this server is set up with.
type RunnerKind string

const (
	RunnerKindSelfHosted   RunnerKind = "self_hosted"
	RunnerKindRemoteHosted RunnerKind = "remote_hosted"
)

// RunnerPoolState is a pool's lifecycle. A deleted pool is a tombstone: runs
// that were bound to it keep their history, and nothing new can choose it.
type RunnerPoolState string

const (
	RunnerPoolActive   RunnerPoolState = "active"
	RunnerPoolDisabled RunnerPoolState = "disabled"
	RunnerPoolDeleted  RunnerPoolState = "deleted"
)

// Valid reports whether s is one of the three lifecycle states.
func (s RunnerPoolState) Valid() bool {
	return s == RunnerPoolActive || s == RunnerPoolDisabled || s == RunnerPoolDeleted
}

// RunnerPool is one pool of the catalogue: an organization pool or one person's
// own user pool.
type RunnerPool struct {
	ID uuid.UUID `json:"id"`
	// Name is a display label a pool's administrator may change. It is never an
	// identity: every reference is by ID.
	Name  string          `json:"name"`
	Scope RunnerPoolScope `json:"scope"`
	// Owner is the person a user pool belongs to (their subject, as runner
	// ownership names them): only they change it or put runners in it, and only
	// they use it. An organization pool has none: administrators keep it.
	Owner string          `json:"owner,omitempty"`
	State RunnerPoolState `json:"state"`
	// Revision starts at 1 and increases by one on every change to the pool, its
	// membership or its use policy. A run records the revision it was admitted
	// against, and a writer that names a stale revision is refused.
	Revision int64 `json:"revision"`
	// Limits are the pool's own limits per run type. Absent, the pool adds no
	// bound of its own (a pool migrated from before limits existed).
	Limits    *RunnerPoolLimits `json:"limits,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// RunnerPoolNameMax is the longest pool name, in characters.
const RunnerPoolNameMax = 64

// ValidateRunnerPoolName accepts a printable, trimmed, single-line name.
func ValidateRunnerPoolName(name string) error {
	if name == "" || name != strings.TrimSpace(name) {
		return errors.New("name a pool in 1 to 64 characters, without leading or trailing spaces")
	}
	n := 0
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return errors.New("a pool name is one printable line")
		}
		n++
	}
	if n > RunnerPoolNameMax {
		return fmt.Errorf("a pool name is at most %d characters", RunnerPoolNameMax)
	}
	return nil
}

// remoteRunnerIDRE is a remote-hosted runner's stable ID, the name of the
// substrate it is configured as: a short lowercase token, never a host, a socket
// path or a URL. remoteRunnerUUIDRE refuses one that reads as a UUID, so a
// self-hosted runner's id and a remote-hosted runner's never collide.
var (
	remoteRunnerIDRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	remoteRunnerUUIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// ValidRemoteRunnerID reports whether id can name a remote-hosted runner.
func ValidRemoteRunnerID(id string) bool {
	return remoteRunnerIDRE.MatchString(id) && !remoteRunnerUUIDRE.MatchString(id)
}

// OrgPoolSelfHostedMsg refuses a self-hosted runner in an organization pool.
const OrgPoolSelfHostedMsg = "Organization pools take remote-hosted runners only. A self-hosted runner can join its owner's user pools."

// RunnerPoolMember is one runner of a pool: exactly one of RunnerID (a
// self-hosted runner) and RemoteRunnerID (a remote-hosted runner) is set, and
// Kind says which. A self-hosted membership is added by the runner's own owner
// into their own user pool and never by anyone on their behalf.
type RunnerPoolMember struct {
	PoolID uuid.UUID  `json:"pool_id"`
	Kind   RunnerKind `json:"kind"`
	// RunnerID is a claimed self-hosted runner (user pools only).
	RunnerID *uuid.UUID `json:"runner_id,omitempty"`
	// RemoteRunnerID is a remote-hosted runner (either scope).
	RemoteRunnerID string    `json:"remote_runner_id,omitempty"`
	AddedAt        time.Time `json:"added_at"`
}

// Validate refuses a member that names no runner, two runners, or a
// self-hosted runner in an organization pool.
func (m RunnerPoolMember) Validate(scope RunnerPoolScope) error {
	hasRunner, hasRemote := m.RunnerID != nil, m.RemoteRunnerID != ""
	switch {
	case hasRunner == hasRemote:
		return errors.New("a pool member names one runner")
	case hasRunner && scope != RunnerPoolUser:
		return errors.New(OrgPoolSelfHostedMsg)
	case hasRunner && *m.RunnerID == uuid.Nil:
		return errors.New("runner_id is not a runner id")
	case hasRemote && !ValidRemoteRunnerID(m.RemoteRunnerID):
		return errors.New("remote_runner_id is not a remote-hosted runner id")
	}
	return nil
}

// RunnerPoolDefaultPrefKey is the principal_prefs key of a person's own default
// pool. A person's document sits under their authenticated principal only.
const RunnerPoolDefaultPrefKey = "runner_pool_default.v2"

// RunnerPoolDefaults is the default pool, for a person or for the organization:
// the pool a run that names none starts on. A person's own wins over the
// organization's; it is never a way to widen what a person may use, because a
// default only ever seeds a choice the catalogue must still permit. The
// organization's names an organization pool only.
//
// Barrier is the organisation's alone: the class an untouched launch gets when it is at or above the
// floor and the pool offers it. A person's own document never carries one.
type RunnerPoolDefaults struct {
	PoolID  *uuid.UUID       `json:"pool_id,omitempty"`
	Barrier ConfinementClass `json:"barrier,omitempty"`
}

// Validate checks the document's shape. Whether the pool exists, is permitted
// and may be this kind of default is the writer's and the resolver's, against
// the live catalogue.
func (d RunnerPoolDefaults) Validate() error {
	if d.PoolID != nil && *d.PoolID == uuid.Nil {
		return errors.New("a default pool is a pool id")
	}
	if d.Barrier != "" && d.Barrier.Rank() == 0 {
		return fmt.Errorf("barrier %q is not CC1, CC2 or CC3", d.Barrier)
	}
	return nil
}

// RunnerPoolSelection says how a run's pool was chosen.
type RunnerPoolSelection string

const (
	// RunnerPoolSelectedExplicit: the request named the pool.
	RunnerPoolSelectedExplicit RunnerPoolSelection = "explicit"
	// RunnerPoolSelectedPersonal: the person's own saved default.
	RunnerPoolSelectedPersonal RunnerPoolSelection = "personal_default"
	// RunnerPoolSelectedOrg: the organisation's default.
	RunnerPoolSelectedOrg RunnerPoolSelection = "organisation_default"
)

// Valid reports whether s is one of the three.
func (s RunnerPoolSelection) Valid() bool {
	return s == RunnerPoolSelectedExplicit || s == RunnerPoolSelectedPersonal || s == RunnerPoolSelectedOrg
}

// ResolvedRunnerPool is the pool a request resolves to: the pool, the revision
// it was read at and how it was chosen. It is a label for the choice and never
// admission: a run is admitted against the persisted pool and the exact target.
type ResolvedRunnerPool struct {
	ID        uuid.UUID           `json:"id"`
	Name      string              `json:"name"`
	Scope     RunnerPoolScope     `json:"scope"`
	Revision  int64               `json:"revision"`
	Selection RunnerPoolSelection `json:"selection"`
}

// RunnerPoolSubject is one person, group or role a pool-use policy admits.
// It reuses the capability subject vocabulary: a role is a user_type, and an
// identity provider's roles arrive as groups.
type RunnerPoolSubject struct {
	SubjectType CapabilitySubjectType `json:"subject_type"`
	Subject     string                `json:"subject"`
}

// RunnerPoolUsePolicy narrows who may use one organization pool. With no
// policy, everyone who may launch remote runs may use the pool; with one, only
// the listed subjects may. It never grants launch rights, so it can only narrow.
// A policy with no subjects is refused rather than stored: "nobody" and
// "unrestricted" must not look alike, and a disabled pool already closes one.
type RunnerPoolUsePolicy struct {
	PoolID   uuid.UUID           `json:"pool_id"`
	Subjects []RunnerPoolSubject `json:"subjects"`
	// Revision is the pool's revision this policy was written at.
	Revision  int64     `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// Validate checks a policy before it is stored against the pool it narrows: a
// use policy narrows an organization pool only. On a user pool it would either
// be stored and ignored, reading as a restriction that does not exist, or become
// a sharing ACL over people's own runners, which 0.9 does not have.
func (p RunnerPoolUsePolicy) Validate(scope RunnerPoolScope) error {
	if scope != RunnerPoolOrganization {
		return errors.New("a use policy narrows an organization pool only")
	}
	return ValidateRunnerPoolUseSubjects(p.Subjects)
}

// RunnerPoolUseSubjectsMax bounds a policy's subject list.
const RunnerPoolUseSubjectsMax = 64

// ValidateRunnerPoolUseSubjects checks a policy's subjects: one to
// RunnerPoolUseSubjectsMax, each a user, group or user_type with a printable
// name, none repeated. The "all" subject is refused: it is no narrowing.
func ValidateRunnerPoolUseSubjects(subjects []RunnerPoolSubject) error {
	if len(subjects) == 0 || len(subjects) > RunnerPoolUseSubjectsMax {
		return fmt.Errorf("a use policy lists 1 to %d people, groups or roles", RunnerPoolUseSubjectsMax)
	}
	seen := map[RunnerPoolSubject]bool{}
	for _, s := range subjects {
		switch s.SubjectType {
		case CapabilitySubjectUser, CapabilitySubjectGroup, CapabilitySubjectUserType:
		default:
			return fmt.Errorf("subject_type %q is not user, group or user_type", s.SubjectType)
		}
		if s.Subject == "" || len(s.Subject) > 256 || s.Subject != strings.TrimSpace(s.Subject) ||
			strings.IndexFunc(s.Subject, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
			return errors.New("a subject is one printable line of at most 256 bytes")
		}
		if seen[s] {
			return fmt.Errorf("%s %q is listed twice", s.SubjectType, s.Subject)
		}
		seen[s] = true
	}
	return nil
}
