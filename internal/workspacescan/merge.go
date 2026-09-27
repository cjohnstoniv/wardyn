// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strings"
)

// merge.go lives in workspacescan, not api, because store's hydrate pass
// calls MergeProfiles and store cannot import api.

// MergeProfiles combines N sources' individually-scanned profiles into ONE
// profile for the workspace: unions the set-like fields, concatenates leak
// findings and setup commands (deduped and re-capped), takes the largest
// build-memory hint and the LOWEST confidence, and folds ContextHash into a
// digest-of-digests. HasDevcontainer/HasDockerfile come from the PRIMARY
// source ONLY, matched against identities (not profiles[0], since attachment
// order and scan order can diverge) — no match yields false/false, never
// another source's values. SecretFilesPresent entries are prefixed
// "identity/path"; a LeakFinding instead carries its attribution in
// LeakFinding.Source and keeps Path scan-root-relative, since Path is
// path-CLASSIFIED downstream and a locator prefix would corrupt that. Empty
// input returns the zero profile.
func MergeProfiles(profiles []WorkspaceProfile, identities []string, primaryIdentity string) WorkspaceProfile {
	if len(profiles) == 0 {
		return WorkspaceProfile{}
	}
	hasDevcontainer, hasDockerfile := primaryBuildFacts(profiles, identities, primaryIdentity)
	if len(profiles) == 1 {
		p := profiles[0]
		p.HasDevcontainer, p.HasDockerfile = hasDevcontainer, hasDockerfile
		return p
	}
	langs, pkgMgrs, egress, tools := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	services, suggested, secretFiles := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	github, otherHosts := map[string]struct{}{}, map[string]struct{}{}
	secretByName := map[string]SecretNeed{}
	leakSeen := map[LeakFinding]struct{}{}
	var leaks []LeakFinding
	setupSeen := map[string]struct{}{}
	var setupCmds []SetupCommand
	var contextHashes []string
	needsReview := false
	buildMemMiB := 0
	confidenceRank := map[string]int{
		ConfidenceHigh: 3, ConfidenceMedium: 2, ConfidenceLow: 1,
	}
	lowest := ConfidenceHigh

	addAll := func(dst map[string]struct{}, xs []string) {
		for _, x := range xs {
			dst[x] = struct{}{}
		}
	}
	for i, p := range profiles {
		var identity string
		if i < len(identities) {
			identity = identities[i]
		}
		addAll(langs, p.Languages)
		addAll(pkgMgrs, p.PackageManagers)
		addAll(egress, p.EgressDomains)
		addAll(tools, p.Tools)
		addAll(services, p.ServicesNeeded)
		addAll(suggested, p.SuggestedEgress)
		for _, f := range p.SecretFilesPresent {
			secretFiles[attributePath(identity, f)] = struct{}{}
		}
		addAll(github, p.GitRemotes.GitHub)
		addAll(otherHosts, p.GitRemotes.OtherHosts)
		for _, n := range p.RequiredSecrets {
			// Strongest-wins (required beats optional), matching
			// FoldWorkspaceContract's rule 5 — a first-wins keep here would
			// let attachment order decide optional-vs-required.
			if prev, dup := secretByName[n.Name]; !dup || (prev.Optional && !n.Optional) {
				secretByName[n.Name] = n
			}
		}
		for _, f := range p.LeakFindings {
			// Attribution rides its own field, not a Path prefix, since Path
			// is path-classified downstream and a locator prefix would
			// silently reclassify it.
			f.Source = identity
			if _, dup := leakSeen[f]; !dup {
				leakSeen[f] = struct{}{}
				leaks = append(leaks, f)
			}
		}
		for _, c := range p.SetupCommands {
			k := c.Stage + "\x00" + c.Command // dedupe key mirrors deriveSetupCommands
			if _, dup := setupSeen[k]; !dup {
				setupSeen[k] = struct{}{}
				setupCmds = append(setupCmds, c)
			}
		}
		needsReview = needsReview || p.NeedsReview
		if p.BuildMemoryMiB > buildMemMiB {
			buildMemMiB = p.BuildMemoryMiB
		}
		if p.ContextHash != "" {
			contextHashes = append(contextHashes, p.ContextHash)
		}
		if confidenceRank[p.Confidence] < confidenceRank[lowest] {
			lowest = p.Confidence
		}
	}
	// Riskiest-first, then capped; a drop sets NeedsReview.
	leaks, leaksTruncated := capLeakFindings(leaks)
	needsReview = needsReview || leaksTruncated
	requiredSecrets := make([]SecretNeed, 0, len(secretByName))
	for _, n := range secretByName {
		requiredSecrets = append(requiredSecrets, n)
	}
	slices.SortFunc(requiredSecrets, func(a, b SecretNeed) int { return strings.Compare(a.Name, b.Name) })

	return WorkspaceProfile{
		Languages:          sortedSetKeys(langs),
		PackageManagers:    sortedSetKeys(pkgMgrs),
		EgressDomains:      sortedSetKeys(egress),
		Tools:              sortedSetKeys(tools),
		GitRemotes:         GitRemotes{GitHub: sortedSetKeys(github), OtherHosts: sortedSetKeys(otherHosts)},
		HasDevcontainer:    hasDevcontainer,
		HasDockerfile:      hasDockerfile,
		RequiredSecrets:    requiredSecrets,
		ServicesNeeded:     sortedSetKeys(services),
		SuggestedEgress:    sortedSetKeys(suggested),
		SecretFilesPresent: sortedSetKeys(secretFiles),
		BuildMemoryMiB:     buildMemMiB,
		LeakFindings:       leaks,
		SetupCommands:      setupCmds,
		ContextHash:        mergeContextHash(contextHashes),
		Confidence:         lowest,
		NeedsReview:        needsReview,
		Source:             SourceDeterministic,
	}
}

// primaryBuildFacts returns the HasDevcontainer/HasDockerfile of the profile
// whose identity equals primaryIdentity, not profiles[0] — identities and
// profiles are in scan order, which can diverge from attachment order. No
// match returns false/false rather than attributing another source's values.
func primaryBuildFacts(profiles []WorkspaceProfile, identities []string, primaryIdentity string) (hasDevcontainer, hasDockerfile bool) {
	for i, id := range identities {
		if i >= len(profiles) {
			break
		}
		if id == primaryIdentity {
			return profiles[i].HasDevcontainer, profiles[i].HasDockerfile
		}
	}
	return false, false
}

// attributePath prefixes a path with its source's identity so two sources'
// identical relative paths stay distinguishable once concatenated. Used only
// for SecretFilesPresent, a bare []string with no separate attribution field;
// LeakFinding carries its attribution in LeakFinding.Source instead. Empty
// identity leaves path as-is.
func attributePath(identity, path string) string {
	if identity == "" {
		return path
	}
	return identity + "/" + path
}

// mergeContextHash folds N sources' own ContextHash digests into ONE: sorted
// (deterministic regardless of attachment order) then sha256'd together, so
// the built-image cache still busts when ANY attached source's build inputs
// change. Empty when no source contributed a hash (nothing to bust on).
func mergeContextHash(hashes []string) string {
	if len(hashes) == 0 {
		return ""
	}
	slices.Sort(hashes)
	sum := sha256.Sum256([]byte(strings.Join(hashes, "")))
	return hex.EncodeToString(sum[:])
}

// sortedSetKeys returns a set's keys sorted — deterministic merges so equal
// inputs marshal byte-equal.
func sortedSetKeys(m map[string]struct{}) []string {
	return slices.Sorted(maps.Keys(m))
}
