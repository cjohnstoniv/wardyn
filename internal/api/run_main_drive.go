// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Every run has a sized main drive. The size is a soft REQUEST unless the run's policy writes it:
// a requested or defaulted size is cut to the effective cap and degrades to uncapped on a runner
// that cannot cap disk, while a size the policy writes stays a requirement, which the request may
// only lower. mainDriveFor is the one expression, read by the create fold, both dry doors and the
// quota fit, and dispatch is handed its answer rather than deciding again.

// mainDrive is the answer: the size, where it came from and what cut it.
type mainDrive struct {
	MiB          int
	RequestedMiB int
	Basis        client.MainDriveBasis
	// DefaultSource names the limit whose default sized the run, with basis default.
	DefaultSource types.LimitSource
	ClampedBy     client.MainDriveClampSource
	// Soft is true when no policy wrote the size: a runner that cannot cap disk runs uncapped.
	Soft bool
}

// mainDriveFor sizes the main drive. policyMiB is the size the run's policy writes (0: none),
// requestMiB the request's, and lim the effective limit of the pool, governance and deployment.
//
//  1. The base is the first positive of the request, the policy's size, then the limit's default;
//     with none the run has no size.
//  2. A policy's size is a ceiling for the request: it may lower it, never raise it.
//  3. The effective cap cuts what remains.
//
// A size is soft exactly when no policy wrote one.
func mainDriveFor(policyMiB, requestMiB int, lim types.EffectiveLimit) mainDrive {
	md := mainDrive{RequestedMiB: requestMiB, Basis: client.MainDriveBasisNone}
	switch {
	case requestMiB > 0:
		md.MiB, md.Basis = requestMiB, client.MainDriveBasisRequest
	case policyMiB > 0:
		md.MiB, md.Basis = policyMiB, client.MainDriveBasisPolicy
	case !lim.Default.Unlimited && lim.Default.Value > 0:
		md.MiB, md.Basis, md.DefaultSource = lim.Default.Value, client.MainDriveBasisDefault, defaultSourceOf(lim.DefaultSource)
	}
	if policyMiB > 0 && md.MiB > policyMiB {
		md.MiB, md.ClampedBy = policyMiB, client.MainDriveClampPolicy
	}
	if !lim.Cap.Unlimited && md.MiB > lim.Cap.Value {
		md.MiB, md.ClampedBy = lim.Cap.Value, mainDriveClampOf(lim.CapSource)
	}
	md.Soft = policyMiB == 0 && md.MiB > 0
	return md
}

// defaultSourceOf keeps default_source to the two layers that name a default (spec 3.5). A cap
// that stands in as the default (a profile's maximum, a runner's capacity) reads as the server's.
func defaultSourceOf(src types.LimitSource) types.LimitSource {
	if src == types.LimitSourcePool {
		return src
	}
	return types.LimitSourceDeployment
}

func mainDriveClampOf(src types.LimitSource) client.MainDriveClampSource {
	switch src {
	case types.LimitSourcePool:
		return client.MainDriveClampPool
	case types.LimitSourceGovernance:
		return client.MainDriveClampGovernance
	case types.LimitSourceDeployment:
		return client.MainDriveClampDeployment
	}
	return ""
}

// applied is the dry doors' fact.
func (m mainDrive) applied() *client.MainDriveApplied {
	return &client.MainDriveApplied{RequestedMiB: m.RequestedMiB, AppliedMiB: m.MiB, Basis: m.Basis,
		DefaultSource: m.DefaultSource, ClampedBy: m.ClampedBy, Soft: m.Soft}
}

// mainDriveDecision is what create hands dispatch: the size and whether it is soft. Dispatch never
// reads the size back out of the launch spec, so a requested or defaulted size is never taken for
// one the policy wrote.
type mainDriveDecision struct {
	MiB           int
	Soft          bool
	Basis         client.MainDriveBasis
	DefaultSource types.LimitSource
}

func (m mainDrive) decision() *mainDriveDecision {
	return &mainDriveDecision{MiB: m.MiB, Soft: m.Soft, Basis: m.Basis, DefaultSource: m.DefaultSource}
}

// basis is the decision's basis for the audit envelope: "" when the lane has no decision.
func (d *mainDriveDecision) basis() client.MainDriveBasis {
	if d == nil {
		return ""
	}
	return d.Basis
}

// defaultSource is the layer whose default sized the run, for the envelope: "" unless the basis is default.
func (d *mainDriveDecision) defaultSource() types.LimitSource {
	if d == nil || d.Basis != client.MainDriveBasisDefault {
		return ""
	}
	return d.DefaultSource
}

// apply writes the size into policy as a FRESH block, never in place: policy is a shallow copy of
// the caller's spec, so its Resources pointer may still be the process-global default's.
func (d *mainDriveDecision) apply(policy *types.RunPolicySpec) {
	rl := types.ResourceLimits{}
	if policy.Resources != nil {
		rl = *policy.Resources
	}
	rl.DiskMiB = d.MiB
	policy.Resources = &rl
}

// authoredDiskMiB is the main-drive size the ceiling's policy writes, the number composer.Clamp
// holds an inline policy's disk to: 0 when it names none.
func (c governanceCeiling) authoredDiskMiB() int {
	if c.Spec.Resources == nil {
		return 0
	}
	return c.Spec.Resources.DiskMiB
}

// runnerCannotCapDisk reports whether the run can land where disk is not capped. A pooled run is
// judged by its eligible targets (the pool's executors or runners): ANY target reporting `none`
// refuses, because placement picks a target by run id and never by disk, so a hard size could bind
// there (DS-3 section 2, the weakest word across the targets). A pool whose targets all enforce is
// accepted even when another substrate of the deployment does not. An online target that reports no
// word is unknown, never `none`. An unpooled run has only the deployment's word.
// A runner that cannot answer is not asked to refuse here: the placement steps own that.
func (s *Server) runnerCannotCapDisk(ctx context.Context, req createRunRequest, place runPlacement) bool {
	if place.pooled && len(place.eligible) > 0 {
		return slices.ContainsFunc(place.eligible, func(t poolTarget) bool {
			return t.facts.EphemeralDiskEnforcement == types.StorageEnforcementNone
		})
	}
	p := requestPlacement(req)
	if p == "" {
		p = types.PlacementRemote
	}
	caps, err := s.placementCapabilities(ctx, p, requestRunnerID(req))
	return err == nil && caps.EphemeralDiskEnforcement == types.StorageEnforcementNone
}

// mainDriveUnenforceableMsg is the 422 sentence. It says "this run's policy", not an admin: an
// inline or overlay policy can be written by the person who launches the run.
func mainDriveUnenforceableMsg() string {
	return "This run's policy sets a disk size that this pool's runners can't cap. Choose another pool, or ask your admin."
}

// mainDriveUnenforceableUnpooledMsg is the same refusal for a run in no pool, which has no pool to
// choose instead.
//
// DRAFT (M2 canon pending)
func mainDriveUnenforceableUnpooledMsg() string {
	return "This run's policy sets a disk size that this server's runner can't cap. Ask your admin."
}

// warnDiskPolicyCut is the clamp warning when the run's own policy, not an operator maximum, cut a
// requested disk size; composer.WarnResourcesCapped would blame an operator for a policy the person
// may have written.
//
// DRAFT (M2 canon pending)
const warnDiskPolicyCut = "disk size reduced to the size this run's policy sets"
