// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Composed governance profiles (migration 0125): a profile may name a base (another profile, or the
// deployment default) and carry an overlay that can only narrow it. This file is the ONLY reader of a
// profile's authority. A composed row stores no ceiling or limits of its own, so a reader that
// took the stored row as authority would read it as empty (every limit unset) and fail OPEN;
// the resolver below composes from the deployment down, and a source guard
// (governance_compose_census_test.go) fails any other file in this package that calls the store's
// raw profile reads. types.GovernanceProfile is the stored row and ResolvedProfile is what
// every reader takes, so a reader left on the row stops compiling.
//
// A chain that cannot be read or composed returns an error and never a partial chain: every caller
// takes the branch it already has for a store failure, and a composition the meet cannot satisfy
// (overlayUnsatisfiableError) is the explicit deny-all state, refused with the authz reason
// governance_overlay_unsatisfiable.
package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxProfileDepth is the longest chain: the profile, its base, that base's base. The deployment
// default under the root is not counted.
const maxProfileDepth = 3

var (
	errProfileCycle = errors.New("api: a governance profile chain repeats a profile")
	errProfileDepth = errors.New("api: a governance profile chain is deeper than 3 profiles")
)

// ResolvedProfile is one profile as authority: the leaf's identity and contact beside the EFFECTIVE
// content composed from its chain. It carries nothing a member may not see except through
// AdminWarnings, which names bases and is never put on a member surface.
type ResolvedProfile struct {
	ID       uuid.UUID
	Name     string
	Contact  *policyref.Contact // the leaf's own: never inherited from a base
	Composed bool
	// Chain is the leaf first, then each base: admin surfaces only.
	Chain   []profileLink
	Ceiling types.RunPolicySpec
	Limits  types.GovernanceLimits
	// Warnings name the LEAF only and never a base's name, id, overlay or contact: they reach a
	// member's 201 clamp_warnings.
	Warnings []string
	// AdminWarnings carry the base-level detail for GET /governance and the 409 bodies.
	AdminWarnings []string
}

// profileLink names one profile of a chain.
type profileLink struct {
	ID   uuid.UUID
	Name string
}

// overlayUnsatisfiableError is a composition the meet refuses: no value satisfies both the base and
// the overlay. Error() names the LEAF only and says nothing about the base, because it reaches a
// member's refusal; Detail is for an admin surface.
type overlayUnsatisfiableError struct {
	Leaf   string
	Detail string
}

func (e *overlayUnsatisfiableError) Error() string {
	return fmt.Sprintf("governance profile %q cannot be applied: nothing satisfies it and the profile it builds on together, "+
		"so runs under it are refused until an administrator fixes it", e.Leaf)
}

func isOverlayUnsatisfiable(err error) (*overlayUnsatisfiableError, bool) {
	var u *overlayUnsatisfiableError
	return u, errors.As(err, &u)
}

// resolveProfileByID is the resolver for a run's captured profile: the leaf and its ancestors in
// ONE statement, composed from the deployment down. store.ErrNotFound when the leaf is gone, which
// each caller reads its own way (a door has nothing to bind; revive refuses); any other error is a
// failed read and the caller fails closed.
func (s *Server) resolveProfileByID(ctx context.Context, id uuid.UUID) (*ResolvedProfile, error) {
	chain, err := s.cfg.Store.GetGovernanceProfileChain(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("api: read the governance profile chain: %w", err)
	}
	return s.composeChain(chain)
}

// resolveAssignedProfile is the resolver for a caller's assignment: the store's precedence read,
// then the chain of the profile it names when that profile is composed. store.ErrNotFound means no
// assignment matched (the deployment ceiling). A composition nothing satisfies comes back as an
// *overlayUnsatisfiableError, audited by ceilingFromProfile, the one place every ceiling passes.
func (s *Server) resolveAssignedProfile(ctx context.Context, users, groups []string, userType string) (*ResolvedProfile, types.CapabilitySubjectType, error) {
	row, tier, err := s.cfg.Store.ResolveGovernanceProfile(ctx, users, groups, userType)
	if err != nil {
		return nil, "", err
	}
	if row == nil {
		return nil, "", store.ErrNotFound
	}
	chain := []types.GovernanceProfile{*row}
	if row.Composed() {
		if chain, err = s.cfg.Store.GetGovernanceProfileChain(ctx, row.ID); err != nil {
			// Never ErrNotFound here: an assigned profile cannot be deleted, and reading "gone" as
			// "no assignment" would hand back the deployment ceiling.
			return nil, "", fmt.Errorf("api: read the governance profile chain: %v", err)
		}
	}
	p, err := s.composeChain(chain)
	if err != nil {
		return nil, "", err
	}
	return p, tier, nil
}

// assignedProfileIdentity answers which profile binds the claims, for the admin preview: identity
// and tier only, never authority, so it composes nothing.
func (s *Server) assignedProfileIdentity(ctx context.Context, users, groups []string, userType string) (id uuid.UUID, name string, tier types.CapabilitySubjectType, err error) {
	row, tier, err := s.cfg.Store.ResolveGovernanceProfile(ctx, users, groups, userType)
	if err != nil {
		return uuid.Nil, "", "", err
	}
	if row == nil {
		return uuid.Nil, "", "", store.ErrNotFound
	}
	return row.ID, row.Name, tier, nil
}

// profileLeafName is the name of the profile id names and nothing else: no chain is composed and no
// base is returned, so it is safe for a member surface. store.ErrNotFound when the profile is gone.
func (s *Server) profileLeafName(ctx context.Context, id uuid.UUID) (string, error) {
	chain, err := s.cfg.Store.GetGovernanceProfileChain(ctx, id)
	if err != nil {
		return "", err
	}
	return chain[0].Name, nil
}

// profileResolution is one profile of the whole list with what composing it gave. Err is why it
// has no effective content (a cycle, a depth overflow, an unsatisfiable overlay), shown to admins
// rather than failing the list.
type profileResolution struct {
	Row      types.GovernanceProfile
	Resolved *ResolvedProfile
	Err      error
}

// resolveAllProfiles composes every profile from ONE list read, for the readers that show the
// whole list.
func (s *Server) resolveAllProfiles(ctx context.Context) ([]profileResolution, error) {
	rows, err := s.cfg.Store.ListGovernanceProfiles(ctx)
	if err != nil {
		return nil, err
	}
	return s.composeAll(rows), nil
}

// composeAll composes each row of an already-read list independently.
func (s *Server) composeAll(rows []types.GovernanceProfile) []profileResolution {
	byID := profilesByID(rows)
	out := make([]profileResolution, len(rows))
	for i, row := range rows {
		out[i] = profileResolution{Row: row}
		chain, err := chainFromRows(byID, row.ID)
		if err == nil {
			out[i].Resolved, err = s.composeChain(chain)
		}
		out[i].Err = err
	}
	return out
}

func profilesByID(rows []types.GovernanceProfile) map[uuid.UUID]types.GovernanceProfile {
	byID := make(map[uuid.UUID]types.GovernanceProfile, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	return byID
}

// chainFromRows walks id's bases through an in-memory list, leaf first, refusing a repeat
// (errProfileCycle), a chain past maxProfileDepth (errProfileDepth) and a base the list lacks.
func chainFromRows(byID map[uuid.UUID]types.GovernanceProfile, id uuid.UUID) ([]types.GovernanceProfile, error) {
	var chain []types.GovernanceProfile
	for cur := &id; cur != nil; {
		row, ok := byID[*cur]
		if !ok {
			return nil, fmt.Errorf("api: governance profile %s names a base that does not exist", id)
		}
		if slices.ContainsFunc(chain, func(r types.GovernanceProfile) bool { return r.ID == row.ID }) {
			return nil, errProfileCycle
		}
		if len(chain) == maxProfileDepth {
			return nil, errProfileDepth
		}
		chain = append(chain, row)
		cur = row.BaseProfileID
	}
	return chain, nil
}

// checkProfileChain refuses a chain that is not a clean line to a root: an id that repeats, a link
// that does not point at the next row, a standalone row with a base above it, or a last row that
// still names a base (the bound cut the chain short, or the base is missing).
func checkProfileChain(chain []types.GovernanceProfile) error {
	seen := make(map[uuid.UUID]bool, len(chain))
	for i, r := range chain {
		if seen[r.ID] {
			return errProfileCycle
		}
		seen[r.ID] = true
		if r.BaseProfileID != nil && !r.Composed() {
			return fmt.Errorf("api: governance profile %s has a base but no overlay", r.ID)
		}
		if i+1 < len(chain) && (r.BaseProfileID == nil || *r.BaseProfileID != chain[i+1].ID) {
			return fmt.Errorf("api: governance profile chain is not contiguous at %s", r.ID)
		}
	}
	last := chain[len(chain)-1]
	if last.BaseProfileID != nil {
		switch {
		case seen[*last.BaseProfileID]:
			return errProfileCycle
		case len(chain) >= maxProfileDepth:
			return errProfileDepth
		}
		// The base was not read: only reachable from a store that returned a short chain.
		return fmt.Errorf("api: governance profile %s names a base that was not read", last.ID)
	}
	if len(chain) > maxProfileDepth {
		return errProfileDepth
	}
	return nil
}

// composeChain composes a chain (leaf first) from the deployment down.
func (s *Server) composeChain(chain []types.GovernanceProfile) (*ResolvedProfile, error) {
	if len(chain) == 0 {
		return nil, errors.New("api: an empty governance profile chain")
	}
	if err := checkProfileChain(chain); err != nil {
		return nil, err
	}
	leaf := chain[0]
	out := &ResolvedProfile{ID: leaf.ID, Name: leaf.Name, Contact: leaf.Contact, Composed: leaf.Composed()}
	for _, r := range chain {
		out.Chain = append(out.Chain, profileLink{ID: r.ID, Name: r.Name})
	}
	root := len(chain) - 1
	var base composer.Authority
	next := root
	if chain[root].Composed() {
		base = s.deploymentAuthority()
	} else {
		var warns []string
		base, warns = s.standaloneAuthority(chain[root])
		out.AdminWarnings = append(out.AdminWarnings, warns...)
		if root == 0 {
			out.Warnings = warns
		}
		next = root - 1
	}
	for i := next; i >= 0; i-- {
		var err error
		if base, err = s.applyStep(out, base, chain[i], i == 0); err != nil {
			return nil, err
		}
	}
	out.Ceiling, out.Limits = base.Ceiling, base.Limits
	if out.Composed {
		out.Ceiling.Resources = s.inheritDeploymentResources(out.Ceiling.Resources)
	}
	return out, nil
}

// deploymentAuthority is the root of a chain whose root names no base: the deployment default, with
// the deployment's own size filled in for a field it leaves unset, and no limits.
func (s *Server) deploymentAuthority() composer.Authority {
	spec := s.cfg.DefaultPolicy.Clone()
	spec.Resources = s.inheritDeploymentResources(spec.Resources)
	return composer.Authority{Ceiling: spec}
}

// standaloneAuthority is a standalone row's authority exactly as the resolver has always read it:
// a size it omits inherited from the deployment, its grants re-intersected against the deployment's
// (a redeploy may have dropped one), and a dropped push-rules warning.
func (s *Server) standaloneAuthority(p types.GovernanceProfile) (composer.Authority, []string) {
	spec := p.Ceiling.Clone()
	spec.Resources = s.inheritDeploymentResources(spec.Resources)
	kept, warns := reintersectGovernanceGrants(spec.EligibleGrants, s.cfg.DefaultPolicy.EligibleGrants, p.Name)
	spec.EligibleGrants = kept
	warns = append(warns, droppedPushRulesWarning(spec, s.cfg.DefaultPolicy, p.Name)...)
	return composer.Authority{Ceiling: spec, Limits: p.Limits}, warns
}

// overlayOf is a composed row's overlay as the composer takes it.
func overlayOf(p types.GovernanceProfile) composer.Overlay {
	ov := composer.Overlay{}
	if p.Overlay != nil {
		ov.Ceiling = *p.Overlay
	}
	if p.OverlayLimits != nil {
		ov.Limits = *p.OverlayLimits
	}
	return ov
}

// applyStep narrows base by one composed row's overlay: the meet, then the row's own grants
// re-intersected against that resolved base. leaf says the row is the profile being resolved, whose
// warnings are the only ones a member sees.
func (s *Server) applyStep(out *ResolvedProfile, base composer.Authority, row types.GovernanceProfile, leaf bool) (composer.Authority, error) {
	res, warns, err := composer.ApplyOverlay(base, overlayOf(row))
	if err == nil {
		if msg := governanceLimitsRefusal(res.Limits); msg != "" {
			err = &composer.OverlayError{Reason: composer.ReasonOverlayUnsatisfiable, Field: "limits", Detail: msg}
		}
	}
	if err != nil {
		return composer.Authority{}, &overlayUnsatisfiableError{Leaf: out.Name,
			Detail: fmt.Sprintf("governance profile %q: %v", row.Name, err)}
	}
	for _, w := range warns {
		out.AdminWarnings = append(out.AdminWarnings, fmt.Sprintf("governance profile %q: overlay %s", row.Name, w))
		if leaf {
			field, _, _ := strings.Cut(w, ": ")
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"governance profile %q: its overlay names %s beyond what the profile it builds on allows, so it was narrowed", row.Name, field))
		}
	}
	// The meet passes the overlay's own grants through unchanged: bound them by the resolved base.
	if row.Overlay != nil && row.Overlay.EligibleGrants != nil {
		kept := res.Ceiling.EligibleGrants[:0:0]
		for _, g := range res.Ceiling.EligibleGrants {
			if err := governanceGrantWithinCeiling(g, base.Ceiling.EligibleGrants); err != nil {
				out.AdminWarnings = append(out.AdminWarnings, fmt.Sprintf(
					"governance profile %q: dropped %s grant no longer within the profile it builds on (%v)", row.Name, g.Kind, err))
				if leaf {
					out.Warnings = append(out.Warnings, fmt.Sprintf(
						"governance profile %q: dropped %s grant the profile it builds on no longer allows", row.Name, g.Kind))
				}
				continue
			}
			kept = append(kept, g)
		}
		res.Ceiling.EligibleGrants = kept
	}
	return res, nil
}
