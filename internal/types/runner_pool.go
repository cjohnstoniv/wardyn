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

// The runner pool contract (0.9). A pool is a stable, organisation-defined set
// of execution targets. Choosing one constrains where a run may go; it never
// grants a runner, a credential, a drive or a confinement class, and it never
// makes another person's runner a candidate: a self-hosted pool lists many
// people's runners, but a person's run only ever considers their own.
//
// Nothing here is stored yet. These are the shapes the storage, the admission
// resolver and the console build on, and the wire the server refuses to honour
// until they do (internal/api's stepRunContract).

// RunnerPoolHosting says what kind of target a pool holds. It is fixed for the
// life of a pool and is the console's "Remote Provided" / "Self-Hosted" choice.
// Placement is the transport value a run records; hosting is the vocabulary a
// person sees, and each hosting type has exactly one placement.
type RunnerPoolHosting string

const (
	// RunnerPoolRemoteProvided pools hold executors the organisation configures.
	RunnerPoolRemoteProvided RunnerPoolHosting = "remote_provided"
	// RunnerPoolSelfHosted pools hold personally claimed runners (H1).
	RunnerPoolSelfHosted RunnerPoolHosting = "self_hosted"
)

// Valid reports whether h is one of the two hosting types.
func (h RunnerPoolHosting) Valid() bool {
	return h == RunnerPoolRemoteProvided || h == RunnerPoolSelfHosted
}

// Placement is the run placement a pool of this hosting type serves; empty for
// a value outside the set.
func (h RunnerPoolHosting) Placement() Placement {
	switch h {
	case RunnerPoolRemoteProvided:
		return PlacementRemote
	case RunnerPoolSelfHosted:
		return PlacementLocal
	}
	return ""
}

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

// RunnerPool is one pool of the organisation's catalogue.
type RunnerPool struct {
	ID uuid.UUID `json:"id"`
	// Name is a display label a pool's administrator may change. It is never an
	// identity: every reference is by ID.
	Name        string            `json:"name"`
	HostingType RunnerPoolHosting `json:"hosting_type"`
	State       RunnerPoolState   `json:"state"`
	// Revision starts at 1 and increases by one on every change to the pool, its
	// membership or its use policy. A run records the revision it was admitted
	// against, and a writer that names a stale revision is refused.
	Revision  int64     `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
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

// executorIDRE is a configured executor's stable ID: a short lowercase token,
// never a host, a socket path or a URL.
var executorIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

// RunnerPoolMember is one target of a pool. Exactly one of RunnerID and
// ExecutorID is set, and it must be the kind the pool's hosting type holds.
// A self-hosted membership is added by the runner's own owner and never by
// anyone on their behalf; the pool only records it.
type RunnerPoolMember struct {
	PoolID uuid.UUID `json:"pool_id"`
	// RunnerID is a claimed runner (self_hosted pools).
	RunnerID *uuid.UUID `json:"runner_id,omitempty"`
	// ExecutorID is a server-configured executor (remote_provided pools).
	ExecutorID string    `json:"executor_id,omitempty"`
	AddedAt    time.Time `json:"added_at"`
}

// Validate refuses a member that names no target, two targets, or a target of
// the wrong kind for hosting. Mixed kinds are never stored.
func (m RunnerPoolMember) Validate(hosting RunnerPoolHosting) error {
	hasRunner, hasExecutor := m.RunnerID != nil, m.ExecutorID != ""
	switch {
	case hasRunner == hasExecutor:
		return errors.New("a pool member names one runner or one executor")
	case hasRunner && hosting != RunnerPoolSelfHosted:
		return errors.New("only a self-hosted pool holds runners")
	case hasExecutor && hosting != RunnerPoolRemoteProvided:
		return errors.New("only a remote-provided pool holds executors")
	case hasRunner && *m.RunnerID == uuid.Nil:
		return errors.New("runner_id is not a runner id")
	case hasExecutor && !executorIDRE.MatchString(m.ExecutorID):
		return errors.New("executor_id is not a configured executor id")
	}
	return nil
}

// RunnerPoolDefaultsPrefKey is the principal_prefs key of a person's own pool
// defaults. A person's document sits under their authenticated principal only.
const RunnerPoolDefaultsPrefKey = "runner_pool_defaults.v1"

// RunnerPoolDefaults is the same shape for a person and for the organisation:
// which hosting type a run starts on and which pool each type starts on. An
// absent field inherits (a person's from the organisation's, the organisation's
// from nothing); it is never a way to widen what a person may use, because a
// default only ever seeds a choice the catalogue must still permit.
type RunnerPoolDefaults struct {
	PreferredHosting RunnerPoolHosting `json:"preferred_hosting,omitempty"`
	RemoteProvided   *uuid.UUID        `json:"remote_provided,omitempty"`
	SelfHosted       *uuid.UUID        `json:"self_hosted,omitempty"`
}

// PoolFor is the default pool for a hosting type, nil when none is set.
func (d RunnerPoolDefaults) PoolFor(h RunnerPoolHosting) *uuid.UUID {
	switch h {
	case RunnerPoolRemoteProvided:
		return d.RemoteProvided
	case RunnerPoolSelfHosted:
		return d.SelfHosted
	}
	return nil
}

// Validate checks the document's shape. Whether each pool exists, is permitted
// and has the right hosting type is the writer's and the resolver's, against
// the live catalogue.
func (d RunnerPoolDefaults) Validate() error {
	if d.PreferredHosting != "" && !d.PreferredHosting.Valid() {
		return fmt.Errorf("preferred_hosting %q is not remote_provided or self_hosted", d.PreferredHosting)
	}
	for _, id := range []*uuid.UUID{d.RemoteProvided, d.SelfHosted} {
		if id != nil && *id == uuid.Nil {
			return errors.New("a default pool is a pool id")
		}
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
	ID          uuid.UUID           `json:"id"`
	Name        string              `json:"name"`
	HostingType RunnerPoolHosting   `json:"hosting_type"`
	Revision    int64               `json:"revision"`
	Selection   RunnerPoolSelection `json:"selection"`
}

// RunnerPoolSubject is one person, group or role a pool-use policy admits.
// It reuses the capability subject vocabulary: a role is a user_type, and an
// identity provider's roles arrive as groups.
type RunnerPoolSubject struct {
	SubjectType CapabilitySubjectType `json:"subject_type"`
	Subject     string                `json:"subject"`
}

// RunnerPoolUsePolicy narrows who may use one remote-provided pool. With no
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
