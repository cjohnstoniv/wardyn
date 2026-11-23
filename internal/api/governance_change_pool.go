// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The pool of people who can approve a held governance change, and the one check of a person against an
// approver predicate from their latest sign-in record. Both read the same source: the record of each
// person's last sign-in to Wardyn itself (principal_groups, door ""), joined to the people directory for the
// inactive rule and the email. A person who never signed in has no record and is not eligible now; a record
// that cannot establish a role (a truncated one) is counted apart, so the pool reads "can't confirm" where it
// would otherwise read "can't reach".
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The tiers an approval is given at and a person is rebuilt at, lowest first.
const (
	tierUnitAdmin     = "unit_admin"
	tierSecurityAdmin = "security_admin"
	tierSuperAdmin    = "super_admin"
)

// tierRank orders the tiers; an unknown one is below them all.
func tierRank(t string) int {
	switch t {
	case tierSuperAdmin:
		return 3
	case tierSecurityAdmin:
		return 2
	case tierUnitAdmin:
		return 1
	}
	return 0
}

// lowerTier is the lower of two tiers.
func lowerTier(a, b string) string {
	if tierRank(a) <= tierRank(b) {
		return a
	}
	return b
}

// tierIsSecurity reports whether a tier is a security admin's or a super admin's.
func tierIsSecurity(t string) bool { return tierRank(t) >= tierRank(tierSecurityAdmin) }

// tierOf is the tier of the person on ctx: a super admin, else a security admin, else (for anyone an approver
// predicate admitted) a unit admin. It is never asked of the admin token.
func (s *Server) tierOf(ctx context.Context) string {
	switch {
	case s.isOperator(ctx):
		return tierSuperAdmin
	case s.isSecurityOperator(ctx):
		return tierSecurityAdmin
	}
	return tierUnitAdmin
}

// poolCandidateCap bounds the sign-in records a pool counts: the read asks for one more, and a longer read
// says the directory is larger than the pool can count.
const poolCandidateCap = store.SignInRecordCap

// The reasons a pool cannot count some people.
const (
	uncountableUnconfirmed = "unconfirmed_people"
	uncountableTooMany     = "too_many_people"
)

// recordInputs is what judging a person against a predicate from their record needs, read once: the role
// mappings and user types the sign-in derives a role against, and the context every rebuilt identity starts
// from.
type recordInputs struct {
	// base is the context a rebuilt identity is layered on. It carries no caller: it starts from the
	// background (with the request's deadline), so none of the request's identity, token, delegation or
	// device markers reach a predicate. A decision's base reads the unit scope on the decision's own
	// transaction; a pool's carries the one shared scope memo.
	base      context.Context
	mappings  []oidc.RoleMapping
	userTypes []types.UserType
}

// poolInputs is a pool's reads: the bounded sign-in records, and every unit-admin grant.
type poolInputs struct {
	recordInputs
	records []store.SignInRecord
	tooMany bool
	grants  []types.GovernanceUnitAdmin
}

// grantSet is the loaded unit-admin grants a pool consults to ask a scoped predicate only of holders; a nil
// *grantSet asks every candidate (a decision's check of one person).
type grantSet struct{ grants []types.GovernanceUnitAdmin }

// poolPerson is one person who can approve a change: the principal that stands for them, the email the
// people directory lists them under (none when it has none), and their highest tier.
type poolPerson struct {
	Principal string `json:"principal"`
	Email     string `json:"email,omitempty"`
	Tier      string `json:"tier"`
}

// poolSource is a reason some people cannot be counted: unconfirmed_people with how many, or too_many_people.
type poolSource struct {
	Why   string `json:"why"`
	Count int    `json:"count,omitempty"`
}

// poolResult is who can approve (people, not principals) and who cannot be counted.
type poolResult struct {
	Eligible    []poolPerson
	Uncountable []poolSource
}

// countedPerson is the stored identity of a counting approval: the principal and the email on its row.
type countedPerson struct{ Principal, Email string }

// detachedContext is a context with none of parent's values: the background, carrying parent's deadline when
// it has one and released when parent ends.
func detachedContext(parent context.Context) context.Context {
	base := context.Background()
	if dl, ok := parent.Deadline(); ok {
		var cancel context.CancelFunc
		base, cancel = context.WithDeadline(base, dl)
		context.AfterFunc(parent, cancel)
	}
	return base
}

var errEmptyPrincipal = errors.New("api: a sign-in record names no principal")

// recordIdentity is one person's request context, built from their record and the derivation of their role:
// base with the person as the human, never a predicate asked about an empty principal.
func recordIdentity(base context.Context, rec store.SignInRecord, d oidc.Derivation) (context.Context, error) {
	if rec.Principal == "" {
		return nil, errEmptyPrincipal
	}
	return withHumanIdentity(base, rec.Principal, rec.Email, d.Role, d.UserType, rec.Groups, rec.Truncated), nil
}

// requestOn is a request that carries ctx, for an approver predicate, which reads only its context.
func requestOn(ctx context.Context) *http.Request { return (&http.Request{}).WithContext(ctx) }

// derivationInputs reads the role mappings and user types on q.
func (s *Server) derivationInputs(ctx context.Context, q store.Querier) ([]oidc.RoleMapping, []types.UserType, error) {
	rows, err := store.ListRoleMappingsQ(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	userTypes, err := store.ListUserTypesQ(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	return toOIDCRoleMappings(rows), userTypes, nil
}

// loadRecordInputs is the reads a decision's check of one person needs, with the unit scope read on q.
func (s *Server) loadRecordInputs(ctx context.Context, q store.Querier) (*recordInputs, error) {
	mappings, userTypes, err := s.derivationInputs(ctx, q)
	if err != nil {
		return nil, err
	}
	return &recordInputs{base: withUnitScopeTx(detachedContext(ctx), q), mappings: mappings, userTypes: userTypes}, nil
}

// loadPoolInputs reads, once per request, what every pool of the request is built from: the bounded sign-in
// records with the directory's inactive rule and email, the role mappings, the user types and every
// unit-admin grant, and creates the one unit-scope memo the request's predicates share. That is four reads.
func (s *Server) loadPoolInputs(ctx context.Context, q store.Querier) (*poolInputs, error) {
	mappings, userTypes, err := s.derivationInputs(ctx, q)
	if err != nil {
		return nil, err
	}
	records, err := store.ListSignInRecordsQ(ctx, q)
	if err != nil {
		return nil, err
	}
	grants, err := store.ListUnitAdminsQ(ctx, q)
	if err != nil {
		return nil, err
	}
	in := &poolInputs{records: records, grants: grants}
	if len(records) > poolCandidateCap {
		in.records, in.tooMany = records[:poolCandidateCap], true
	}
	memo := &unitScopeMemo{in: map[unitScopeKey]bool{}, grants: map[string][]types.GovernanceUnitAdmin{}}
	in.recordInputs = recordInputs{
		base:     context.WithValue(detachedContext(ctx), unitScopeMemoKey{}, memo),
		mappings: mappings, userTypes: userTypes,
	}
	return in, nil
}

// deriveFromRecord is the role a record derives against the current mappings; the zero (refusing)
// derivation when sign-in is not SSO, so a deployment without it can establish no role.
func (s *Server) deriveFromRecord(in *recordInputs, rec store.SignInRecord) oidc.Derivation {
	if s.cfg.OIDC == nil {
		return oidc.Derivation{}
	}
	return s.deriveRecordRole(in.mappings, in.userTypes, rec.Roles, rec.Groups, rec.Email)
}

// recordOutcome is what one person's record says about an approver predicate.
type recordOutcome int

const (
	// recordPasses: sign-in would admit them and the predicate passes on the record.
	recordPasses recordOutcome = iota
	// recordNone: they have no sign-in record, or none that names a principal.
	recordNone
	// recordRoleGone: sign-in would now refuse them, or give no role, on a complete record.
	recordRoleGone
	// recordUnconfirmed: the predicate fails and the record is a partial snapshot, so the failure may be a
	// group the cut dropped.
	recordUnconfirmed
	// recordFails: the predicate fails on a complete record.
	recordFails
)

// recordVerdict is the outcome of checking one record, with the tier recomputed on the rebuilt identity when
// the predicate passes.
type recordVerdict struct {
	outcome recordOutcome
	tier    string
}

// judgeRecord asks approver of the person rec describes, as the record says they are now. gs, when not nil,
// is every loaded unit-admin grant: a scoped predicate is then asked only of a security-tier person or a
// grant holder, so a pool reads the unit scope for holders and never for every candidate.
func (s *Server) judgeRecord(in *recordInputs, gs *grantSet, approver govApprover, rec store.SignInRecord) recordVerdict {
	d := s.deriveFromRecord(in, rec)
	if !d.OK() {
		if rec.Truncated {
			// The groups were cut, so a group that would have given a role may be the one that was dropped.
			return recordVerdict{outcome: recordUnconfirmed}
		}
		return recordVerdict{outcome: recordRoleGone}
	}
	ctx, err := recordIdentity(in.base, rec, d)
	if err != nil {
		return recordVerdict{outcome: recordNone}
	}
	passes := false
	if approver.scoped && gs != nil && !s.isSecurityOperator(ctx) {
		passes = holdsGrant(s, ctx, gs.grants) && approver.allowed(s, requestOn(ctx))
	} else {
		passes = approver.allowed(s, requestOn(ctx))
	}
	switch {
	case passes:
		return recordVerdict{outcome: recordPasses, tier: s.tierOf(ctx)}
	case rec.Truncated:
		return recordVerdict{outcome: recordUnconfirmed}
	}
	return recordVerdict{outcome: recordFails}
}

// holdsGrant reports whether the person on ctx is named by one of grants: a user subject equal to a user grant's
// subject, or a group subject equal to a group grant's, the match ListUnitAdminOf makes.
func holdsGrant(s *Server, ctx context.Context, grants []types.GovernanceUnitAdmin) bool {
	users, groups := s.unitAdminSubjects(ctx)
	return slices.ContainsFunc(grants, func(g types.GovernanceUnitAdmin) bool {
		switch g.SubjectType {
		case types.CapabilitySubjectUser:
			return slices.Contains(users, g.Subject)
		case types.CapabilitySubjectGroup:
			return slices.Contains(groups, g.Subject)
		}
		return false
	})
}

// poolForKind is who can approve a change of one kind (at one unit, for the kinds a unit scopes): the
// kind's approver predicate asked of every loaded candidate, people merged by the distinct-human rule. A
// unit kind with a unit uses the unit-scope predicate its changes are decided under; a nil unit uses the
// kind's table approver.
func (s *Server) poolForKind(in *poolInputs, kind string, unit uuid.UUID) (poolResult, error) {
	gk, ok := governanceChangeKinds[kind]
	if !ok || gk.approver.allowed == nil {
		return poolResult{}, fmt.Errorf("api: no approver pool for target kind %q", kind)
	}
	approver := gk.approver
	if unit != uuid.Nil {
		switch kind {
		case govKindMembership:
			approver = unitScopeApprover(false, unit)
		case govKindUnit, govKindUnitAdmin, govKindUnitDeciders:
			approver = unitScopeApprover(true, unit)
		}
	}
	return s.evalPool(in, approver, nil), nil
}

// poolForChange is who else can approve one held change: the change's own approver predicate, minus every
// alias of the proposer and of every counted approver. counted is the stored identity of each counting
// approval, so a counted person outside the bounded candidate read is still left out.
func (s *Server) poolForChange(in *poolInputs, ch types.GovernanceChange, counted []countedPerson) (poolResult, error) {
	gk, ok := governanceChangeKinds[ch.TargetKind]
	if !ok {
		return poolResult{}, fmt.Errorf("api: no approver pool for target kind %q", ch.TargetKind)
	}
	approver := gk.forChange(ch).approver
	if approver.allowed == nil {
		return poolResult{}, fmt.Errorf("api: no approver pool for target kind %q", ch.TargetKind)
	}
	excluded := append([]countedPerson{{Principal: ch.ProposedBy, Email: ch.ProposedByEmail}}, counted...)
	return s.evalPool(in, approver, excluded), nil
}

// evalPool asks approver of every candidate that is active and not one of excluded, and merges the eligible
// into people: two principals that are the same human are one person, carrying the highest tier of the two
// and the lowest principal of that tier. A candidate whose role a partial record cannot establish is counted,
// as one person for each email, unless that person is already eligible.
func (s *Server) evalPool(in *poolInputs, approver govApprover, excluded []countedPerson) poolResult {
	var people []poolPerson
	gs := &grantSet{grants: in.grants}
	byEmail := map[string]int{}
	unconfirmed := map[string]bool{}
	anonymous := 0 // unconfirmed candidates with no email, never merged
	for _, rec := range in.records {
		if rec.Inactive() || isExcluded(rec, excluded) {
			continue
		}
		v := s.judgeRecord(&in.recordInputs, gs, approver, rec)
		email := strings.ToLower(rec.Email)
		switch v.outcome {
		case recordPasses:
			p := poolPerson{Principal: rec.Principal, Email: rec.DirectoryEmail, Tier: v.tier}
			if i, seen := byEmail[email]; seen && email != "" {
				if tierRank(p.Tier) > tierRank(people[i].Tier) {
					people[i] = p
				}
				continue
			}
			if email != "" {
				byEmail[email] = len(people)
			}
			people = append(people, p)
		case recordUnconfirmed:
			if email == "" {
				anonymous++
			} else {
				unconfirmed[email] = true
			}
		}
	}
	res := poolResult{Eligible: people}
	if res.Eligible == nil {
		res.Eligible = []poolPerson{}
	}
	count := anonymous
	for email := range unconfirmed {
		if _, eligible := byEmail[email]; !eligible {
			count++
		}
	}
	if count > 0 {
		res.Uncountable = append(res.Uncountable, poolSource{Why: uncountableUnconfirmed, Count: count})
	}
	if in.tooMany {
		res.Uncountable = append(res.Uncountable, poolSource{Why: uncountableTooMany})
	}
	if res.Uncountable == nil {
		res.Uncountable = []poolSource{}
	}
	return res
}

// isExcluded reports whether rec is the same human as one of excluded.
func isExcluded(rec store.SignInRecord, excluded []countedPerson) bool {
	return slices.ContainsFunc(excluded, func(c countedPerson) bool {
		return sameHuman(rec.Principal, rec.Email, c.Principal, c.Email)
	})
}
