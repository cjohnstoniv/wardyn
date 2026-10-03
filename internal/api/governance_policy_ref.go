// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// profilePolicyRef is the member-safe reference to one governance profile: its
// name and the contact it published, each field re-validated on the way out
// (policyref.Project), and nothing else about the profile. The ceiling of one
// profile never reaches a person bound by another, because every caller passes
// the profile that bound THIS person: a leaf, never a composed result.
func profilePolicyRef(p *types.GovernanceProfile) *policyref.Ref {
	if p == nil {
		return nil
	}
	return policyref.Project(policyref.SourceProfile, p.Name, p.Contact)
}

// policyRef is the policy that bounds this principal, with no store read: the
// leaf profile, or the bare deployment arm for a member no profile binds, or nil
// for an operator, who is not clamped. /me serves this directly; a written
// refusal goes through Server.ceilingPolicy, which adds the deployment's
// policy_help.
func (c governanceCeiling) policyRef() *policyref.Ref {
	switch {
	case c.Operator:
		return nil
	case c.Profile != nil:
		return profilePolicyRef(c.Profile)
	default:
		return policyref.Project(policyref.SourceDeployment, "", nil)
	}
}

// ceilingPolicy is the policy a ceiling refusal names. A member bound by the
// deployment gets its policy_help, read through siteConfigSnapshot here and only
// here, so the read happens when a refusal is written and never on a polled read.
// An unreadable site config leaves the bare deployment reference: a refusal is
// never turned into an error by its own help text.
func (s *Server) ceilingPolicy(ctx context.Context, c governanceCeiling) *policyref.Ref {
	ref := c.policyRef()
	if ref == nil || ref.Source != policyref.SourceDeployment {
		return ref
	}
	if sc, ok := s.siteConfigSnapshot(ctx); ok {
		return policyref.Project(policyref.SourceDeployment, "", sc.PolicyHelp)
	}
	return ref
}

// runPolicyRef is the policy a run was launched under, for its detail read: the
// run's leaf profile, else the deployment's policy_help, else nil. A store
// failure omits it and logs, as every other projection on that read does.
func (s *Server) runPolicyRef(ctx context.Context, run types.AgentRun) *policyref.Ref {
	if s.cfg.Store == nil {
		return nil
	}
	p, err := s.runProfile(ctx, run)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: run policy lookup failed", "run_id", run.ID, "err", err)
		return nil
	}
	if p != nil {
		return profilePolicyRef(p)
	}
	if sc, ok := s.siteConfigSnapshot(ctx); ok && sc.PolicyHelp != nil {
		return policyref.Project(policyref.SourceDeployment, "", sc.PolicyHelp)
	}
	return nil
}

// recordCeilingError is errRecordCeilingLimit's refusal carrying the policy that
// raised it. The Record Mode and provider sign-in launch doors answer outside
// refuse (writeErrorReasonPolicy, with the sentinel's own wire reason), so the
// reference travels in the error. Error and Unwrap return the wrapped value
// unchanged: errors.Is keeps matching the sentinel and the member sentence the
// writers trim out of Error() is byte-identical to what it was.
type recordCeilingError struct {
	err error
	ref *policyref.Ref
}

func (e recordCeilingError) Error() string { return e.err.Error() }
func (e recordCeilingError) Unwrap() error { return e.err }

// recordCeilingRef is the policy a launch error carries, nil when it carries none.
func recordCeilingRef(err error) *policyref.Ref {
	var ce recordCeilingError
	if errors.As(err, &ce) {
		return ce.ref
	}
	return nil
}

// meGovernanceContact is /me's governance_contact: the policy that bounds the
// caller, read off the per-request ceiling memo the drive door already fills, so
// /me adds no store read to the console's most-polled route. nil for a caller
// whose runs stand outside governance (runUngoverned: not clamped, and resolved
// before any store read; a governed admin gets their profile's contact like a
// member), on a resolver error and on
// a stale group snapshot: /me is a display read and never fails for this key.
// The deployment arm carries no site-config read here; a member meets the
// deployment's policy_help in a refusal and on a run's detail.
func (s *Server) meGovernanceContact(r *http.Request) *policyref.Ref {
	if s.runUngoverned(r.Context()) {
		return nil
	}
	ceiling, err := s.effectiveCeiling(withDisplayRead(r.Context()))
	if err != nil {
		return nil
	}
	return ceiling.policyRef()
}
