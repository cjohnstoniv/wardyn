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

// merge.go — moved here VERBATIM from internal/api (workspace_run.go) in the
// three-tier split: the store's hydrate pass computes a workspace's profile as
// the merge of its attached sources' profiles, and store cannot import api.
// api keeps a one-line alias so its call sites don't churn.

// MergeProfiles combines N local_dir/repo sources' individually-scanned
// profiles into ONE profile for the workspace: union the set-like fields
// (languages, package managers, egress, tools, required secrets, services,
// suggested egress), concatenate leak findings and setup commands (never drop
// a suspected secret or an install step, deduped so N sources can't repeat
// one, re-capped so N sources can't exceed one source's own bound), take the
// largest build-memory hint, fold ContextHash into a digest-of-digests, and
// take the LOWEST confidence (one ambiguous source makes the whole
// workspace's profile suspect). HasDevcontainer/HasDockerfile come from the
// PRIMARY (profiles[0]) source ONLY — unioning them would let a devcontainer
// that lives in a non-primary source get built as if it were the primary
// repo's own. identities[i] names profiles[i]'s source (its locator, unique
// per source); LeakFinding.Path and SecretFilesPresent entries are prefixed
// "identity: path" so a merged finding still says which source it came from.
// Empty input returns the zero profile; a single profile is returned
// unchanged (nothing ambiguous to resolve for one source).
func MergeProfiles(profiles []WorkspaceProfile, identities []string) WorkspaceProfile {
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
			if _, dup := secretByName[n.Name]; !dup {
				secretByName[n.Name] = n
			}
		}
		for _, f := range p.LeakFindings {
			f.Path = attributePath(identity, f.Path)
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
	if len(leaks) > maxLeakFindings {
		leaks = leaks[:maxLeakFindings]
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
		HasDevcontainer:    profiles[0].HasDevcontainer,
		HasDockerfile:      profiles[0].HasDockerfile,
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

// attributePath prefixes a per-source-root-relative path with its source's
// identity so a merged finding still says which attached source it came
// from — two sources' identical relative paths ("config/.env") would
// otherwise be indistinguishable once concatenated. Empty identity leaves
// path as-is (unreachable via hydrateWorkspace, which always has a source
// locator, but fail-safe rather than producing a stray leading ": ").
func attributePath(identity, path string) string {
	if identity == "" {
		return path
	}
	return identity + ": " + path
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
