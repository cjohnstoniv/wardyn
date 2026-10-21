// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The create-time end (#1319, docs/design/0.9/no-end-runs.md §3). POST /runs takes
// ends_after_sec or no_end; the shared fold resolves the end against the pool, the
// person's governance and the deployment through runnerpool.Effective, and stores
// ends_at and lifetime_cap_at on the run. planCreateEnd is separate from planEnd on
// purpose: planEnd decides a CHANGE to an existing end, and its "no end yet"
// branches would refuse a finite end and pass a null unchecked at create. The two
// share latestEndAhead and noEndBlock, so a finite ask is cut to the same bound at
// both doors.

// runEndShapeRefusal checks ends_after_sec, no_end and max_wait_sec's shape, before any store
// read: both set, below 1, or past the run-limit ceiling.
func runEndShapeRefusal(req createRunRequest) *runRefusal {
	switch {
	case req.EndsAfterSec != nil && req.NoEnd:
		return runError(http.StatusBadRequest, reasonRunEndConflictingFields, "set ends_after_sec or no_end, not both")
	case req.EndsAfterSec != nil && *req.EndsAfterSec < 1:
		return runError(http.StatusBadRequest, reasonRunEndAfterTooSmall, "ends_after_sec must be at least 1")
	case req.EndsAfterSec != nil && *req.EndsAfterSec > maxRunLimitSec:
		return runError(http.StatusBadRequest, reasonRunEndAfterTooLarge, fmt.Sprintf("ends_after_sec must be at most %d", maxRunLimitSec))
	case req.MaxWaitSec != nil && *req.MaxWaitSec < 1:
		return runError(http.StatusUnprocessableEntity, reasonRunMaxWaitInvalid, "max_wait_sec must be at least 1")
	}
	return nil
}

// createEndInput is everything one create's end is decided from.
type createEndInput struct {
	runType types.RunnerPoolRunType
	// pool is the run's resolved pool; nil when it has none.
	pool *types.RunnerPool
	// governed is false when governance is skipped for the caller (a super admin,
	// the admin token, local mode): the pool and the deployment still bind them.
	governed  bool
	gov       types.RunLimits // the captured governance limits
	maxAgeSec int             // WARDYN_RUN_MAX_AGE; 0 is off
	askedSec  int             // 0 when ends_after_sec is absent
	noEnd     bool
}

// createEnd is the decision: the end and the lifetime cap in seconds from
// created_at, and what the run.create row records about how it was reached.
type createEnd struct {
	endSec    *int // nil is No end
	capSec    int  // 0 is no absolute cap
	capSource types.LimitSource
	askedSec  int
	noEnd     bool // No end was asked for
	capped    bool
	// source is what bound the end: the limit that cut an asked end, or the
	// source that named an untouched run's default. Empty when none did.
	source types.LimitSource
	pool   string // the pool's name, for the sentences
}

// runTypeForEnd is the run type the end is decided for: the stored experience. A
// run with none (an older client's) is decided as Background, which keeps today's
// no-end default; it never reads as Interactive, whose default is 24 hours.
func runTypeForEnd(e types.RunExperience) types.RunnerPoolRunType {
	if e == types.ExperienceInteractive {
		return types.RunnerPoolRunInteractive
	}
	return types.RunnerPoolRunBackground
}

// createEndSources are the sources one create's end is folded from, through the one builder
// (buildLimitSources). The pool is the run's own, when it has one; governance is left out when it is
// skipped; the deployment is always there, carrying WARDYN_RUN_MAX_AGE and the Interactive fallback
// default. No size is read: a governance lifetime does not depend on one.
func createEndSources(in createEndInput) ([]runnerpool.SourceLimits, error) {
	sources, refusal, err := buildLimitSources(limitSourceInput{runType: in.runType, pool: in.pool, governed: in.governed,
		limits: types.GovernanceLimits{RunLimits: in.gov}, maxAgeSec: in.maxAgeSec})
	// An older-shape request (no stored experience) reads as Background here. That holds only while
	// admission refuses that shape in every pool that sets limits (resolvePlacement's PoolSource check),
	// so no pool's limits ever decide such a run's end.
	if refusal != nil {
		// Admission (the run-type check) answers this before the end is decided.
		return nil, fmt.Errorf("pool %q does not take a %s run", in.pool.Name, in.runType)
	}
	return sources, err
}

// planCreateEnd decides a new run's end (design §3.3, with O4-B: any finite end
// up to the cap is free; only No end needs the gate). A refusal is a 403 and
// writes no row; an over-ask is cut to the cap, never refused.
func planCreateEnd(in createEndInput) (createEnd, *runRefusal, error) {
	sources, err := createEndSources(in)
	if err != nil {
		return createEnd{}, nil, err
	}
	eff, err := runnerpool.Effective(in.runType, sources...)
	if err != nil {
		return createEnd{}, nil, err
	}
	out := createEnd{askedSec: in.askedSec, noEnd: in.noEnd}
	if in.pool != nil {
		out.pool = in.pool.Name
	}
	var spanAhead *time.Duration
	if span := eff.LifetimeSpan; !span.Cap.Unlimited {
		out.capSec, out.capSource = span.Cap.Value, span.CapSource
		d := time.Duration(span.Cap.Value) * time.Second
		spanAhead = &d
	}
	lease := 0
	if in.governed {
		lease = in.gov.MaxEndAheadSec
	}
	bound := latestEndAhead(lease, spanAhead, out.capSource)

	// A default the person's profile does not name is "no end" (today's rule for
	// a run's end), not the run-limit ceiling its missing maximum is read as.
	defaultNoEnd := eff.Lifetime.Default.Unlimited ||
		(in.governed && in.gov.DefaultEndSec == 0 && in.gov.MaxEndAheadSec == 0 && eff.Lifetime.DefaultSource == types.LimitSourceGovernance)
	switch {
	case in.noEnd && !defaultNoEnd:
		if refusal := createNoEndRefusal(in, eff, out.pool); refusal != nil {
			return createEnd{}, refusal, nil
		}
	case in.noEnd, defaultNoEnd && in.askedSec == 0:
	case in.askedSec > 0:
		sec := in.askedSec
		if limit := int(bound.ahead / time.Second); bound.bounded && sec > limit {
			sec, out.capped, out.source = limit, true, bound.source
		}
		out.endSec = &sec
	default:
		sec := eff.Lifetime.Default.Value
		out.endSec, out.source = &sec, eff.Lifetime.DefaultSource
	}
	return out, nil, nil
}

// createNoEndRefusal is why No end is not on offer, or nil when it is: the pool
// and the deployment must leave the lifetime unlimited, and governance (unless
// skipped) must allow No end and hold the gate, the answers PATCH gives.
func createNoEndRefusal(in createEndInput, eff types.EffectiveRunLimits, pool string) *runRefusal {
	if span := eff.LifetimeSpan; !span.Cap.Unlimited {
		return runError(http.StatusForbidden, reasonRunEndNoEndNotAllowed,
			"No end is not allowed: "+endSourceLabel(span.CapSource, pool)+" limits how long a run may live")
	}
	if in.governed {
		if reason, msg := noEndBlock(in.gov); reason != "" {
			return runError(http.StatusForbidden, reason, msg)
		}
	}
	return nil
}

// endSourceLabel is how a limit's source reads to a member. No profile number
// reaches one; the pool's name is its own.
func endSourceLabel(src types.LimitSource, pool string) string {
	switch src {
	case types.LimitSourcePool:
		if pool != "" {
			return pool
		}
		return "your pool"
	case types.LimitSourceGovernance:
		return "your admin"
	}
	return "this deployment"
}

// resolveCreateEnd is stepRunEnd's check: the run's end, or the refusal. pool is
// the pool the run resolved to (stepPlacement), nil for an unpooled run; its
// lifetime limits the end like the other sources.
func (s *Server) resolveCreateEnd(r *http.Request, req createRunRequest, ceiling governanceCeiling, pool *types.RunnerPool) (createEnd, *runRefusal) {
	in := createEndInput{
		runType: runTypeForEnd(req.Experience),
		pool:    pool,
		// The Wardyn boundary bounds an operator's or a break-glass run's end too, when it names a maximum.
		governed:  !s.runUngoverned(r.Context()) || ceiling.boundaryBoundsEnd(),
		gov:       ceiling.Limits.RunLimits,
		maxAgeSec: max(0, int(s.cfg.RunMaxAge/time.Second)), // a negative WARDYN_RUN_MAX_AGE is off, as 0 is
		noEnd:     req.NoEnd,
	}
	if req.EndsAfterSec != nil {
		in.askedSec = *req.EndsAfterSec
	}
	end, refusal, err := planCreateEnd(in)
	if err != nil {
		return createEnd{}, runServerError("resolve run end", err)
	}
	return end, refusal
}

// audit is the run.create row's `end` record: what was asked, what the run got
// and which limit bound it. A source reads as the member sees it.
func (e createEnd) audit(run types.AgentRun) map[string]any {
	out := map[string]any{"asked_sec": e.askedSec, "no_end": e.noEnd, "applied": run.EndsAt, "capped": e.capped}
	if e.source != "" {
		out["source"] = endSourceLabel(e.source, e.pool)
	}
	return out
}

// warnings is the 201's sentence for an end cut to the cap.
func (e createEnd) warnings(run types.AgentRun) []string {
	if !e.capped || run.EndsAt == nil {
		return nil
	}
	return []string{fmt.Sprintf("the end you asked for is past what %s allows, so the run ends at %s instead",
		endSourceLabel(e.source, e.pool), run.EndsAt.UTC().Format(time.RFC3339))}
}
