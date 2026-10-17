// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// queuePass holds, for one promotion pass, the facts about an owner that a promotion cannot change: the
// pools the owner may use and their ceiling. A pass judges many waiting runs of few owners, and each
// judgement would read the same two facts again. What a promotion does change (the runners' room and
// usage) is never held here. A pass judges one entry at a time, so nothing locks it, and it is dropped
// with the pass: no fact outlives the pass that read it.
type queuePass struct {
	owners map[string]*queueOwnerFacts
}

type queuePassKey struct{}

// queueOwnerFacts is one owner's facts, each read at most once per pass. An owner is keyed by the whole
// stored authority, not the name: two runs of one person queued under different group snapshots resolve
// different pools and ceilings.
type queueOwnerFacts struct {
	poolsRead   bool
	pools       []store.RunnerPoolCatalogueRow
	poolRefusal *runRefusal
	ceilingRead bool
	ceiling     governanceCeiling
	ceilingErr  error
}

type queueOwnerKey struct{}

// withQueuePass starts a pass's cache on ctx.
func withQueuePass(ctx context.Context) context.Context {
	return context.WithValue(ctx, queuePassKey{}, &queuePass{owners: map[string]*queueOwnerFacts{}})
}

// inheritQueuePass carries the caller's pass, or starts one when it has none, onto the pass's own context.
func inheritQueuePass(base, caller context.Context) context.Context {
	if p, ok := caller.Value(queuePassKey{}).(*queuePass); ok {
		return context.WithValue(base, queuePassKey{}, p)
	}
	return withQueuePass(base)
}

// withQueueOwner binds the entry's owner facts to ctx; without a pass on ctx it binds none, and every
// read goes to the store.
func withQueueOwner(ctx context.Context, owner string, authority []byte) context.Context {
	p, ok := ctx.Value(queuePassKey{}).(*queuePass)
	if !ok {
		return ctx
	}
	key := owner + "\x00" + string(authority)
	f := p.owners[key]
	if f == nil {
		f = &queueOwnerFacts{}
		p.owners[key] = f
	}
	return context.WithValue(ctx, queueOwnerKey{}, f)
}

// queuePermittedPools is permittedPools for the owner on ctx, read once per pass.
func (s *Server) queuePermittedPools(ctx context.Context, ps store.RunnerPoolStore) ([]store.RunnerPoolCatalogueRow, *runRefusal) {
	f, ok := ctx.Value(queueOwnerKey{}).(*queueOwnerFacts)
	if !ok {
		return s.permittedPools(ctx, ps, true)
	}
	if !f.poolsRead {
		f.pools, f.poolRefusal = s.permittedPools(ctx, ps, true)
		f.poolsRead = true
	}
	return f.pools, f.poolRefusal
}

// queueCeiling is effectiveCeiling for the owner on ctx, read once per pass.
func (s *Server) queueCeiling(ctx context.Context) (governanceCeiling, error) {
	f, ok := ctx.Value(queueOwnerKey{}).(*queueOwnerFacts)
	if !ok {
		return s.effectiveCeiling(ctx)
	}
	if !f.ceilingRead {
		f.ceiling, f.ceilingErr = s.effectiveCeiling(ctx)
		f.ceilingRead = true
	}
	return f.ceiling, f.ceilingErr
}
