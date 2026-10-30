// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// macOSCandidate is one eligible runner of a pool as poolMacOSVM judges it: its row, its last caps and
// the macos_vm runs the org counts on it.
type macOSCandidate struct {
	rn       types.Runner
	caps     runnerwire.Caps
	haveCaps bool
	live     int
}

// poolMacOSVMOf is the pool list's macOS VM offer for a pool whose eligible targets are eligible: each
// self-hosted one's row and macos_vm runs from fits (runnerFits, read once per list) and its last caps
// from this replica's endpoint, so it costs no query of its own. A remote-hosted target is no
// candidate: placement never starts a macOS VM on one (macOSVMOnRemote, kindTargets). Nor is any of the
// person's own when their profile denies them (denied, deny_placements), so such a pool offers none.
func (s *Server) poolMacOSVMOf(ctx context.Context, eligible []poolTarget, fits map[string]runnerFit, denied []types.Placement) *client.PoolMacOSVM {
	var cands []macOSCandidate
	for _, t := range eligible {
		if t.runnerID == nil || slices.Contains(denied, types.PlacementLocal) {
			continue
		}
		f := fits[t.id]
		c := macOSCandidate{rn: f.runner, live: f.used.MacOSVMRuns}
		c.rn.ID = *t.runnerID
		if c.rn.Limits == nil {
			c.rn.Limits = f.limits
		}
		c.caps, c.haveCaps = s.runnerLastCaps(*t.runnerID)
		cands = append(cands, c)
	}
	// A site config that cannot be read sets no maximum here; placement reads it again and refuses on the error.
	limit, _ := s.macOSBaseLimitOf(ctx)
	return poolMacOSVM(cands, s.isSecurityOperator(ctx), limit)
}

// poolMacOSVM is what a pool offers a macOS VM run now, by the rule placement holds a pooled one to
// (placementCapabilityRefusalFor): the run lands on any of the pool's eligible runners, so it is offered
// only when every one of them offers it (macOSVMOffer). Offered, a run must fit whichever runner takes
// it: the largest minimum, and only the harnesses every base serves.
func poolMacOSVM(cands []macOSCandidate, operator bool, limit ...macOSBaseLimit) *client.PoolMacOSVM {
	out := &client.PoolMacOSVM{}
	if len(cands) == 0 {
		return out
	}
	slices.SortFunc(cands, func(a, b macOSCandidate) int { return cmp.Compare(macRunnerName(a.rn), macRunnerName(b.rn)) })
	if len(cands) == 1 {
		out.Runner = macRunnerName(cands[0].rn)
	}
	var offered []*runnerwire.MacVMCaps
	for _, c := range cands {
		offer := macOSVMOffer(c.rn, c.caps, c.haveCaps, c.live, operator, limit...)
		if offer.Offered {
			offered = append(offered, c.caps.MacOSVM)
			continue
		}
		ref := client.PoolMacOSVMRefusal{Runner: macRunnerName(c.rn), Why: offer.Why, Detail: offer.Refusal.Detail,
			Reason: offer.Refused, Have: offer.Have, Need: offer.Need}
		if mc := c.caps.MacOSVM; mc != nil && offer.Why == client.MacOSVMWhyPreparing {
			ref.Progress = mc.Base.Progress
		} else if mc != nil && offer.Why == client.MacOSVMWhyNoBase {
			ref.Error = mc.Base.Error
		}
		if operator && offer.Why == client.MacOSVMWhyNotManaged {
			ref.RunnerID = c.rn.ID.String()
		}
		out.Refusals = append(out.Refusals, ref)
	}
	if len(out.Refusals) > 0 {
		return out
	}
	out.Offered, out.GuestOS, out.Harnesses = true, offered[0].Base.GuestOS, slices.Clone(offered[0].Harnesses)
	for _, mc := range offered {
		if mc.Base.GuestOS != out.GuestOS {
			out.GuestOS = ""
		}
		out.Harnesses = slices.DeleteFunc(out.Harnesses, func(h string) bool { return !slices.Contains(mc.Harnesses, h) })
		out.MinCPUs, out.MinMemoryMiB = max(out.MinCPUs, mc.MinCPUs), max(out.MinMemoryMiB, mc.MinMemoryMiB)
	}
	return out
}
