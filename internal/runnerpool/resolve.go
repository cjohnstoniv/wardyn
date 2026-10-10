// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"cmp"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Choice is what a request itself says: an explicit pool, and an explicit
// hosting type when the person picked one.
type Choice struct {
	PoolID  *uuid.UUID
	Hosting types.RunnerPoolHosting
}

// Refusal is a pool selection that cannot proceed, and the sentence to show.
type Refusal struct {
	Reason Reason
	// Source is whose default was refused, for ReasonDefaultUnavailable.
	Source types.RunnerPoolSelection
	// Name is the pool the sentence is about; empty when there is none to name.
	Name string
	// Hosting is the hosting type the sentence is about.
	Hosting types.RunnerPoolHosting
	// Runner names the runner a ReasonMemberMismatch is about: the caller's own
	// runner's name, or the id as the caller sent it. A runner that is not the
	// caller's is never named, it is answered like an unknown one.
	Runner string
	// RunType, Barrier, Allowed and Max carry what the pool-limit reasons name: the
	// run type or barrier asked for, the barriers the pool allows and its cap.
	RunType types.RunnerPoolRunType
	Barrier types.ConfinementClass
	Allowed []types.ConfinementClass
	Max     int
}

func (r *Refusal) Error() string { return r.Message() }

// Message is the refusal's sentence.
func (r *Refusal) Message() string {
	switch r.Reason {
	case ReasonRequired:
		return RequiredMsg(HostingLabel(r.Hosting))
	case ReasonNotFound:
		return NotFoundMsg()
	case ReasonUnavailable:
		return UnavailableMsg(r.Name)
	case ReasonDefaultUnavailable:
		if r.Source == types.RunnerPoolSelectedOrg {
			return DefaultUnavailableOrgMsg()
		}
		return DefaultUnavailablePersonalMsg()
	case ReasonStale:
		return StaleMsg(r.Name)
	case ReasonMemberMismatch:
		if r.Runner != "" {
			return MemberMismatchMsg(r.Runner, r.Name)
		}
		return HostingMismatchMsg(r.Name, HostingLabel(r.Hosting))
	case ReasonNoEligibleMember:
		if r.Hosting == types.RunnerPoolSelfHosted {
			return NoOwnRunnerMsg(r.Name)
		}
		return NoEligibleMemberMsg(r.Name)
	case ReasonPoolsUnavailable:
		return UnavailableServerMsg()
	case ReasonRunTypeNotAllowed:
		return RunTypeNotAllowedMsg(r.Name, r.RunType)
	case ReasonBarrierNotAllowed:
		return BarrierNotAllowedMsg(r.Name, r.Barrier, r.Allowed)
	case ReasonAtCapacity:
		return AtCapacityMsg(r.Name, r.Max)
	}
	return InvalidMsg()
}

// Resolve picks the pool a request runs against, from the pools the caller may
// use. permitted is the caller's own catalogue: the caller of Resolve has
// already dropped every pool the person may not use, so a pool absent from it
// is indistinguishable from one that does not exist.
//
// An explicit pool wins. Otherwise the hosting type is the request's, then the
// person's preferred one, then the organisation's; within that type the
// person's own default pool wins over the organisation's. A default that is
// gone, switched off or of the wrong hosting type is refused, never replaced by
// another default, another pool or the other hosting type: the person must
// choose.
func Resolve(permitted []types.RunnerPool, c Choice, personal, org types.RunnerPoolDefaults) (types.ResolvedRunnerPool, *Refusal) {
	if c.Hosting != "" && !c.Hosting.Valid() {
		return types.ResolvedRunnerPool{}, &Refusal{Reason: ReasonInvalid}
	}
	if c.PoolID != nil {
		return resolveExplicit(permitted, c)
	}
	hosting := cmp.Or(c.Hosting, personal.PreferredHosting, org.PreferredHosting)
	if hosting == "" {
		return types.ResolvedRunnerPool{}, &Refusal{Reason: ReasonRequired}
	}
	source, id := types.RunnerPoolSelectedPersonal, personal.PoolFor(hosting)
	if id == nil {
		source, id = types.RunnerPoolSelectedOrg, org.PoolFor(hosting)
	}
	if id == nil {
		return types.ResolvedRunnerPool{}, &Refusal{Reason: ReasonRequired, Hosting: hosting}
	}
	pool, ok := find(permitted, *id)
	if !ok || pool.State != types.RunnerPoolActive || pool.HostingType != hosting {
		return types.ResolvedRunnerPool{}, &Refusal{Reason: ReasonDefaultUnavailable, Source: source, Hosting: hosting}
	}
	return resolved(pool, source), nil
}

func resolveExplicit(permitted []types.RunnerPool, c Choice) (types.ResolvedRunnerPool, *Refusal) {
	pool, ok := find(permitted, *c.PoolID)
	if !ok {
		return types.ResolvedRunnerPool{}, &Refusal{Reason: ReasonNotFound}
	}
	switch {
	case c.Hosting != "" && c.Hosting != pool.HostingType:
		return types.ResolvedRunnerPool{}, &Refusal{Reason: ReasonMemberMismatch, Name: pool.Name, Hosting: pool.HostingType}
	case pool.State != types.RunnerPoolActive:
		return types.ResolvedRunnerPool{}, &Refusal{Reason: ReasonUnavailable, Name: pool.Name}
	}
	return resolved(pool, types.RunnerPoolSelectedExplicit), nil
}

// find skips tombstones: a deleted pool is not in anyone's catalogue.
func find(permitted []types.RunnerPool, id uuid.UUID) (types.RunnerPool, bool) {
	i := slices.IndexFunc(permitted, func(p types.RunnerPool) bool { return p.ID == id && p.State != types.RunnerPoolDeleted })
	if i < 0 {
		return types.RunnerPool{}, false
	}
	return permitted[i], true
}

func resolved(p types.RunnerPool, sel types.RunnerPoolSelection) types.ResolvedRunnerPool {
	return types.ResolvedRunnerPool{ID: p.ID, Name: p.Name, HostingType: p.HostingType, Revision: p.Revision, Selection: sel}
}
