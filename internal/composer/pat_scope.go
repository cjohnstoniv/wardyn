// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The git_pat dominance contract, beside GitHubScopeWithin/clampGitHubScope: ONE
// definition of "this proposal stays inside that ceiling grant" and "the
// intersection of this proposal and those ceiling grants" for the four scope axes
// (repos, access, api, forge), called at every seam that bounds a grant, so that
// accept-here cannot drift from narrow-there.
//
// Omission defaults are the contract (types.GitPATScope): an omitted field never
// means "whatever the ceiling says". repos omitted = every repository; access
// omitted = write; api omitted = false; forge omitted = generic.
//
// A SINGLE ceiling grant must dominate every axis. Two same-host ceiling grants,
// one wide on repos and one wide on access, do not combine into a proposal wide
// on both; callers select the dominating grant by pairing identity first.

// patForgeConstrained reports whether a ceiling grant's repos or api axis makes
// the forge part of what it bounds: a forge change changes the path identity
// (repos) or the API operation table (api). A ceiling that leaves repos out and
// has api false bounds neither, so the forge axis is free there. Every ceiling
// written before 0.8.6 has no forge key, which is what lets a proposal narrow
// below such a ceiling on any forge.
func patForgeConstrained(cg types.GitPATScope) bool {
	return cg.Repos != nil || cg.API
}

func patAccessRank(a string) int {
	if a == types.PATAccessRead {
		return 1
	}
	return 2
}

func decodePATScopes(proposed, ceiling json.RawMessage) (g, cg types.GitPATScope, err error) {
	if g, err = types.DecodeGitPATScope(proposed); err != nil {
		return g, cg, fmt.Errorf("git_pat scope is not decodable: %w", err)
	}
	if cg, err = types.DecodeGitPATScope(ceiling); err != nil {
		return g, cg, fmt.Errorf("the deployment ceiling's git_pat scope is not decodable: %w", err)
	}
	return g, cg, nil
}

// PATScopeWithin reports whether ceiling grant cg dominates proposal g on every
// PAT axis, naming the first axis that does not:
//
//   - repos: g.repos is a subset of cg.repos under the forge's identity rule
//     (types.PATRepoKey). An omitted cg.repos dominates anything; an omitted
//     g.repos is dominated only by an omitted cg.repos; a "group/*" in g is
//     covered only by the same "group/*", or by an omitted cg.repos.
//   - access: read is below write.
//   - api: false is below true.
//   - forge: equal, but only when cg sets repos or api: true.
//
// An undecodable scope on either side is an error, not "no constraint": a
// comparator reading nothing would fail open.
func PATScopeWithin(proposed, ceiling json.RawMessage) error {
	g, cg, err := decodePATScopes(proposed, ceiling)
	if err != nil {
		return err
	}
	if patForgeConstrained(cg) && g.Forge != cg.Forge {
		return fmt.Errorf("git_pat forge %q differs from the deployment ceiling's %q, which sets repos or api "+
			"(the forge decides how a repository path is read and which API operations exist)", g.Forge, cg.Forge)
	}
	if cg.Repos != nil {
		if g.Repos == nil {
			return fmt.Errorf("git_pat repos is omitted (every repository), but the deployment ceiling narrows it to a list")
		}
		for _, e := range *g.Repos {
			if !patEntryWithin(g.Forge, e, *cg.Repos) {
				return fmt.Errorf("git_pat repos entry %q is outside the deployment ceiling's repos", e)
			}
		}
	}
	if patAccessRank(g.Access) > patAccessRank(cg.Access) {
		return fmt.Errorf("git_pat access %q is above the deployment ceiling's %q", g.Access, cg.Access)
	}
	if g.API && !cg.API {
		return fmt.Errorf("git_pat api: true is above the deployment ceiling's false")
	}
	return nil
}

// patEntryWithin reports whether one proposed repos entry is covered by some
// entry of ceiling. A malformed entry on either side covers and is covered by
// nothing.
func patEntryWithin(forge, entry string, ceiling []string) bool {
	key, ok := types.PATRepoKey(forge, entry)
	if !ok {
		return false
	}
	return slices.ContainsFunc(ceiling, func(c string) bool {
		ck, cok := types.PATRepoKey(forge, c)
		return cok && types.PATRepoCovers(ck, key)
	})
}

// patIntersectRepos is the set intersection of two repos lists under forge's
// identity rule: an entry survives when the other list covers it, so "group/*"
// against "group/app" leaves "group/app". The result is canonical (an entry that
// another entry covers is dropped, then sorted), which is what makes the meet
// across several ceiling grants independent of their order.
func patIntersectRepos(forge string, a, b []string) []string {
	var out []string
	for _, e := range a {
		if patEntryWithin(forge, e, b) {
			out = append(out, e)
		}
	}
	for _, e := range b {
		if patEntryWithin(forge, e, a) {
			out = append(out, e)
		}
	}
	slices.Sort(out)
	out = slices.Compact(out)
	return slices.DeleteFunc(slices.Clone(out), func(e string) bool {
		return slices.ContainsFunc(out, func(o string) bool { return o != e && patEntryWithin(forge, e, []string{o}) })
	})
}

// PATScopeMeet intersects proposal g with every bounding ceiling grant's scope
// and reports whether the grant survives. It never widens: repos are
// intersected, access takes the lower, api the lower, and the forge is g's own.
// The result does not depend on the order of bounds.
//
// The grant is DROPPED (keep=false, with a warning) when:
//   - the repos intersection is empty, because an empty list reads as "none" only
//     where it was authored, and one left behind by the meet must never become
//     an unnarrowed grant;
//   - a bound that sets repos or api: true has a different forge from g;
//   - any scope is undecodable (fail closed).
//
// An empty repos list that g itself authored is not an intersection result and
// stays as written.
func PATScopeMeet(g json.RawMessage, bounds []json.RawMessage, warns *[]string) (json.RawMessage, bool) {
	drop := func(why string) (json.RawMessage, bool) {
		*warns = append(*warns, "dropped git_pat grant: "+why)
		return nil, false
	}
	var raw types.GitPATScope
	if err := json.Unmarshal(g, &raw); err != nil {
		return drop("its scope is not decodable")
	}
	s, err := types.DecodeGitPATScope(g)
	if err != nil {
		return drop("its scope is not decodable")
	}
	out := raw
	authoredEmpty := s.Repos != nil && len(*s.Repos) == 0
	repos := s.Repos
	access, api := s.Access, s.API
	var notes []string
	for _, b := range bounds {
		cg, err := types.DecodeGitPATScope(b)
		if err != nil {
			return drop("a deployment ceiling's git_pat scope is not decodable")
		}
		if patForgeConstrained(cg) && cg.Forge != s.Forge {
			return drop(fmt.Sprintf("its forge %q differs from the deployment ceiling's %q, which sets repos or api", s.Forge, cg.Forge))
		}
		if cg.Repos != nil {
			if repos == nil {
				cp := slices.Clone(*cg.Repos)
				slices.Sort(cp)
				repos = &cp
			} else {
				x := patIntersectRepos(s.Forge, *repos, *cg.Repos)
				repos = &x
			}
		}
		if patAccessRank(cg.Access) < patAccessRank(access) {
			access = cg.Access
		}
		api = api && cg.API
	}
	if repos != nil && len(*repos) == 0 && !authoredEmpty {
		return drop("no repository is left after intersecting its repos with the deployment ceiling's")
	}
	if repos != s.Repos && !patReposWithin(s.Forge, s.Repos, repos) {
		notes = append(notes, "git_pat grant: repos narrowed to the operator scope")
		out.Repos = repos
	}
	if access != s.Access {
		notes = append(notes, fmt.Sprintf("git_pat grant: access clamped %s→%s", s.Access, access))
		out.Access = access
	}
	if api != s.API {
		notes = append(notes, "git_pat grant: api disabled (operator policy)")
		out.API = api
	}
	if len(notes) == 0 {
		return g, true
	}
	*warns = append(*warns, notes...)
	b, err := json.Marshal(out)
	if err != nil {
		return drop("its narrowed scope could not be encoded")
	}
	return b, true
}

// patReposWithin reports whether every entry of a is covered by b, i.e. the
// meet left a's reach unchanged. A nil list is "all", so nil is within only nil.
func patReposWithin(forge string, a, b *[]string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return !slices.ContainsFunc(*a, func(e string) bool { return !patEntryWithin(forge, e, *b) })
}
