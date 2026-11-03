// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strconv"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// ungovernedHeadlineMax is how many limits the notice names before saying there are more.
const ungovernedHeadlineMax = 3

// ungovernedNotice is the preview's `ungoverned` block for a caller whose runs stand outside
// governance (ceiling.Operator, which resolveChainCeiling sets from runUngoverned). With a person
// signed in it resolves what that person's stamped type would get as a member: resolveContext is
// the member path, called directly so the ungoverned short-circuit above it is bypassed. With no
// person (the admin token, local mode) there is no profile to resolve. The resolve is preview-only: its
// result is read for the block and dropped, never stored on the request or fed to any launch check, and
// a failed one is reported as unavailable, never as an empty limits list, and never fails the preview.
func (s *Server) ungovernedNotice(ctx context.Context, ceiling governanceCeiling) *client.UngovernedNotice {
	if !ceiling.Operator {
		return nil
	}
	if oidcHumanFromContext(ctx) == "" {
		return &client.UngovernedNotice{NoPerson: true}
	}
	if s.cfg.Store == nil {
		return &client.UngovernedNotice{NoProfile: true}
	}
	deployment := governanceCeiling{Spec: s.cfg.DefaultPolicy.Clone(), MemberDefault: s.memberDefaultFor(s.cfg.DefaultPolicy.Resources)}
	member, err := s.resolveContext(withDisplayRead(ctx), deployment)
	if err != nil {
		return &client.UngovernedNotice{Unavailable: true}
	}
	if member.Unit == nil {
		return &client.UngovernedNotice{NoProfile: true}
	}
	limits := headlineLimits(member.Limits)
	out := &client.UngovernedNotice{Unit: member.Unit.Name, Limits: limits}
	if len(limits) > ungovernedHeadlineMax {
		out.Limits, out.More = limits[:ungovernedHeadlineMax], true
	}
	return out
}

// headlineLimits are the limits a member would most plainly feel, in a fixed order: size first,
// then how many runs, then the doors. Only limits the unit sets appear.
func headlineLimits(l types.GovernanceLimits) []string {
	var out []string
	if l.MaxCPUMillis > 0 {
		cpus := strconv.FormatFloat(float64(l.MaxCPUMillis)/1000, 'f', -1, 64)
		if l.MaxCPUMillis == 1000 {
			out = append(out, "1 CPU")
		} else {
			out = append(out, cpus+" CPUs")
		}
	}
	if l.MaxMemoryMiB > 0 {
		if l.MaxMemoryMiB%1024 == 0 {
			out = append(out, strconv.Itoa(l.MaxMemoryMiB/1024)+" GiB of memory")
		} else {
			out = append(out, strconv.Itoa(l.MaxMemoryMiB)+" MiB of memory")
		}
	}
	if l.MaxConcurrentRuns > 0 {
		out = append(out, strconv.Itoa(l.MaxConcurrentRuns)+" runs at once")
	}
	if l.DenyInteractive {
		out = append(out, "no interactive runs")
	}
	if l.DenyUIApps {
		out = append(out, "no web apps")
	}
	if l.DenyTaskModeExec {
		out = append(out, "no command runs")
	}
	return out
}
