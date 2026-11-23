// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Choice is what a request itself says: an explicit pool. The placement a
// request asks for narrows the resolved pool's runners afterwards (the api's
// kindTargets), so it plays no part here.
//
// SiteDefault is the default pool of the one site every homed drive of the run
// is at, nil for none. Pinned says something already holds the run to one
// runner (a drive or workspace folder bound to it, a kept machine, a request
// that names its runner): the site step is then skipped, so a site pool that
// does not hold that runner never refuses a run that would otherwise place.
type Choice struct {
	PoolID      *uuid.UUID
	SiteDefault *uuid.UUID
	Pinned      bool
}

// SitePreference is what the site step made of the run's drives' site, for
// run.create's audit row. Empty: the step did not run, because the request
// named its pool or no drive of the run is homed at a site.
type SitePreference string

const (
	// SiteMet: the site's default pool was chosen.
	SiteMet SitePreference = "met"
	// SiteUnusable: the run's drives share a site whose default pool the
	// caller cannot use now (none set, gone, switched off, or not in the
	// caller's catalogue), so the next default was tried.
	SiteUnusable SitePreference = "unusable"
	// SiteAmbiguous: the run's drives are homed at different sites, so no site
	// is preferred. The caller decides it from the drives; Resolve never does.
	SiteAmbiguous SitePreference = "ambiguous"
	// SitePinned: the run is held to one runner, so the site step was skipped.
	SitePinned SitePreference = "pinned"
)

// Refusal is a pool selection that cannot proceed, and the sentence to show.
type Refusal struct {
	Reason Reason
	// Source is whose default was refused, for ReasonDefaultUnavailable.
	Source types.RunnerPoolSelection
	// Name is the pool the sentence is about; empty when there is none to name.
	Name string
	// Scope is the scope of the pool the sentence is about.
	Scope types.RunnerPoolScope
	// Runner names the runner a ReasonMemberMismatch is about: the caller's own
	// runner's name, or the id as the caller sent it. A runner that is not the
	// caller's is never named, it is answered like an unknown one.
	Runner string
	// RunType, Barrier and Allowed carry what the pool-limit reasons name: the run
	// type or barrier asked for and the barriers the pool allows.
	RunType types.RunnerPoolRunType
	Barrier types.ConfinementClass
	Allowed []types.ConfinementClass
	// Count is the number the queue reasons name: the waiting runs a person holds (ReasonQueueFull) or
	// the pool's concurrent-run cap (ReasonFull).
	Count int
}

func (r *Refusal) Error() string { return r.Message() }

// Message is the refusal's sentence.
func (r *Refusal) Message() string {
	switch r.Reason {
	case ReasonRequired:
		return RequiredMsg()
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
		return KindMismatchMsg(r.Name)
	case ReasonNoEligibleMember:
		if r.Scope == types.RunnerPoolUser {
			return NoOwnRunnerMsg(r.Name)
		}
		return NoEligibleMemberMsg(r.Name)
	case ReasonPoolsUnavailable:
		return UnavailableServerMsg()
	case ReasonRunTypeMissing:
		return RunTypeMissingMsg()
	case ReasonRunTypeNotAllowed:
		return RunTypeNotAllowedMsg(r.Name, r.RunType)
	case ReasonBarrierNotAllowed:
		return BarrierNotAllowedMsg(r.Name, r.Barrier, r.Allowed)
	case ReasonQueueFull:
		return QueueFullMsg(r.Name, r.Count)
	case ReasonFull:
		return FullMsg(r.Name, r.Count)
	}
	return InvalidMsg()
}

// Resolve picks the pool a request runs against, from the pools the caller may
// use. permitted is the caller's own catalogue: the caller of Resolve has
// already dropped every pool the person may not use, so a pool absent from it
// is indistinguishable from one that does not exist.
//
// An explicit pool wins, then the default pool of the site the run's drives
// are at, then the person's own default pool, then the organisation's. A
// person's or organisation's default that is gone or switched off is refused,
// never replaced by another default or another pool: the person must choose.
// A site default the caller cannot use is passed over instead, and never
// refuses: a site only prefers, it grants nothing.
func Resolve(permitted []types.RunnerPool, c Choice, personal, org types.RunnerPoolDefaults) (types.ResolvedRunnerPool, *Refusal) {
	pool, _, ref := ResolveSite(permitted, c, personal, org)
	return pool, ref
}

// ResolveSite is Resolve, also answering what the site step made of c
// (SiteMet, SiteUnusable or SitePinned; empty when it did not run).
func ResolveSite(permitted []types.RunnerPool, c Choice, personal, org types.RunnerPoolDefaults) (types.ResolvedRunnerPool, SitePreference, *Refusal) {
	if c.PoolID != nil {
		pool, ref := resolveExplicit(permitted, c)
		return pool, "", ref
	}
	var pref SitePreference
	switch {
	case c.Pinned:
		pref = SitePinned
	case c.SiteDefault != nil:
		if pool, ok := find(permitted, *c.SiteDefault); ok && pool.State == types.RunnerPoolActive {
			return resolved(pool, types.RunnerPoolSelectedSite), SiteMet, nil
		}
		pref = SiteUnusable
	}
	source, id := types.RunnerPoolSelectedPersonal, personal.PoolID
	if id == nil {
		source, id = types.RunnerPoolSelectedOrg, org.PoolID
	}
	if id == nil {
		return types.ResolvedRunnerPool{}, pref, &Refusal{Reason: ReasonRequired}
	}
	pool, ok := find(permitted, *id)
	if !ok || pool.State != types.RunnerPoolActive {
		return types.ResolvedRunnerPool{}, pref, &Refusal{Reason: ReasonDefaultUnavailable, Source: source}
	}
	return resolved(pool, source), pref, nil
}

func resolveExplicit(permitted []types.RunnerPool, c Choice) (types.ResolvedRunnerPool, *Refusal) {
	pool, ok := find(permitted, *c.PoolID)
	if !ok {
		return types.ResolvedRunnerPool{}, &Refusal{Reason: ReasonNotFound}
	}
	if pool.State != types.RunnerPoolActive {
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
	return types.ResolvedRunnerPool{ID: p.ID, Name: p.Name, Scope: p.Scope, Revision: p.Revision, Selection: sel}
}
