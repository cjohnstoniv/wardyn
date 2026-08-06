// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"maps"
	"slices"
	"strings"
)

// merge.go — moved here VERBATIM from internal/api (workspace_run.go) in the
// three-tier split: the store's hydrate pass computes a workspace's profile as
// the merge of its attached sources' profiles, and store cannot import api.
// api keeps a one-line alias so its call sites don't churn.

// MergeProfiles combines N local_dir sources' individually-scanned
// profiles into ONE profile for the workspace: union the set-like fields
// (languages, package managers, egress, tools, required secrets, services,
// suggested egress, secret-file paths), concatenate leak findings and setup
// commands (never drop a suspected secret or an install step), take the
// largest build-memory hint, and take the LOWEST confidence (one ambiguous
// source makes the whole workspace's profile suspect). Empty input returns
// the zero profile; a single profile is returned unchanged.
func MergeProfiles(profiles []WorkspaceProfile) WorkspaceProfile {
	if len(profiles) == 0 {
		return WorkspaceProfile{}
	}
	if len(profiles) == 1 {
		return profiles[0]
	}
	langs, pkgMgrs, egress, tools := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	services, suggested, secretFiles := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	github, otherHosts := map[string]struct{}{}, map[string]struct{}{}
	secretByName := map[string]SecretNeed{}
	var leaks []LeakFinding
	var setupCmds []SetupCommand
	hasDevcontainer, hasDockerfile, needsReview := false, false, false
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
	for _, p := range profiles {
		addAll(langs, p.Languages)
		addAll(pkgMgrs, p.PackageManagers)
		addAll(egress, p.EgressDomains)
		addAll(tools, p.Tools)
		addAll(services, p.ServicesNeeded)
		addAll(suggested, p.SuggestedEgress)
		addAll(secretFiles, p.SecretFilesPresent)
		addAll(github, p.GitRemotes.GitHub)
		addAll(otherHosts, p.GitRemotes.OtherHosts)
		for _, n := range p.RequiredSecrets {
			if _, dup := secretByName[n.Name]; !dup {
				secretByName[n.Name] = n
			}
		}
		leaks = append(leaks, p.LeakFindings...)
		setupCmds = append(setupCmds, p.SetupCommands...)
		hasDevcontainer = hasDevcontainer || p.HasDevcontainer
		hasDockerfile = hasDockerfile || p.HasDockerfile
		needsReview = needsReview || p.NeedsReview
		if p.BuildMemoryMiB > buildMemMiB {
			buildMemMiB = p.BuildMemoryMiB
		}
		if confidenceRank[p.Confidence] < confidenceRank[lowest] {
			lowest = p.Confidence
		}
	}
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
		Confidence:         lowest,
		NeedsReview:        needsReview,
		Source:             SourceDeterministic,
	}
}

// sortedSetKeys returns a set's keys sorted — deterministic merges so equal
// inputs marshal byte-equal.
func sortedSetKeys(m map[string]struct{}) []string {
	return slices.Sorted(maps.Keys(m))
}
