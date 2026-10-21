// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memberCapOf is the CPU and memory a governed member may get under a resolved profile: the member
// cap (runnerpool.MemberCap) over the profile's AUTHORED size, its maximums and the member default
// under its size, as composer.ClampMember applies it at create (authoredSize, memberSize). It is the
// answer the narrowing proofs compare, because a maximum set where no size was authored IS the size,
// so composer.Leq's reading of a new maximum as tighter is not the run's.
func (s *Server) memberCapOf(authored types.ResourceLimits, limits types.GovernanceLimits, ceiling *types.ResourceLimits) types.ResourceLimits {
	def := s.memberDefaultFor(ceiling)
	return types.ResourceLimits{
		CPUMillis: runnerpool.MemberCap(authored.CPUMillis, limits.MaxCPUMillis, def.CPUMillis),
		MemoryMiB: runnerpool.MemberCap(authored.MemoryMiB, limits.MaxMemoryMiB, def.MemoryMiB),
	}
}

// resolvedMemberCap is memberCapOf for a resolved profile.
func (s *Server) resolvedMemberCap(p *ResolvedUnit) types.ResourceLimits {
	return s.memberCapOf(p.AuthoredResources, p.Limits, p.Ceiling.Resources)
}

// authoredAfter is the authored size once row's overlay is applied over a base that authored base:
// a size the overlay names is authored by the chain, and the meet's answer for that field.
func authoredAfter(base types.ResourceLimits, row types.GovernanceUnit, res composer.Authority) types.ResourceLimits {
	if row.Overlay == nil || row.Overlay.Resources == nil || res.Ceiling.Resources == nil {
		return base
	}
	if v := row.Overlay.Resources.CPUMillis; v != nil && *v > 0 {
		base.CPUMillis = res.Ceiling.Resources.CPUMillis
	}
	if v := row.Overlay.Resources.MemoryMiB; v != nil && *v > 0 {
		base.MemoryMiB = res.Ceiling.Resources.MemoryMiB
	}
	if v := row.Overlay.Resources.PidsLimit; v != nil && *v > 0 {
		base.PidsLimit = res.Ceiling.Resources.PidsLimit
	}
	return base
}

// memberCapRaises names each field where cap is above within, with both values; empty when none is.
func memberCapRaises(cap, within types.ResourceLimits) []string {
	var out []string
	if cap.CPUMillis > within.CPUMillis {
		out = append(out, fmt.Sprintf("max_cpu_millis (a member cap of %d over %d)", cap.CPUMillis, within.CPUMillis))
	}
	if cap.MemoryMiB > within.MemoryMiB {
		out = append(out, fmt.Sprintf("max_memory_mib (a member cap of %d over %d)", cap.MemoryMiB, within.MemoryMiB))
	}
	return out
}
