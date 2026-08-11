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
// PRIMARY source ONLY — unioning them would let a devcontainer that lives in
// a non-primary source get built as if it were the primary repo's own.
// primaryIdentity names that source (matched against identities, NOT
// profiles[0] — the caller's attachment order and this function's scan order
// can diverge whenever the first attachment is ephemeral or not yet scanned;
// see hydrateWorkspace in internal/store/store_sources.go, which derives it
// from the exact same ws.Sources[0] the consumer reads as "primary"). A
// primaryIdentity matching no profile (primary is ephemeral or unscanned)
// yields false/false, never another source's values. identities[i] names
// profiles[i]'s source (its locator, unique per source): SecretFilesPresent
// entries are prefixed "identity/path" so a merged finding still says which
// source it came from, while a LeakFinding carries the same attribution in
// LeakFinding.Source and keeps its Path scan-root-relative — Path is
// path-CLASSIFIED by the client (fixture vs hot), which a locator prefix
// would corrupt. Empty input returns the zero profile.
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
			// FoldWorkspaceContract's rule 5 (workspace_contract.go) — a
			// first-wins keep here let attachment ORDER decide whether a
			// secret both surfaces agree exists reads as optional or required
			// (WSPIPE-10).
			if prev, dup := secretByName[n.Name]; !dup || (prev.Optional && !n.Optional) {
				secretByName[n.Name] = n
			}
		}
		for _, f := range p.LeakFindings {
			// Attribution rides its OWN field here, not a Path prefix: Path is
			// path-classified downstream (fixture vs hot), so prefixing it with a
			// locator that itself contains a testdata/fixtures segment would
			// silently reclassify every leak in that source. Source still keeps
			// two sources' identical relative paths distinct in leakSeen.
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
// whose identity equals primaryIdentity — i.e. the SAME attachment
// hydrateWorkspace put at ws.Sources[0], which resolveWorkspaceImage
// (internal/api/workspace_run.go) reads as "primary". Deliberately NOT
// profiles[0]: identities/profiles only contain attachments that actually
// decoded a profile, in scan order, which can diverge from attachment order.
// No match (the primary attachment is ephemeral or unscanned) returns
// false/false — a devcontainer belonging to some OTHER attached source must
// never be attributed to a primary we know nothing about.
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

// attributePath prefixes a per-source-root-relative path with its source's
// identity so a merged finding still says which attached source it came
// from — two sources' identical relative paths ("config/.env") would
// otherwise be indistinguishable once concatenated. "/" is the separator
// (not ": ") so the result reads as a plausible full path (source root /
// file). Empty identity leaves path as-is (unreachable via hydrateWorkspace,
// which always has a source locator, but fail-safe rather than producing a
// stray leading "/").
//
// SecretFilesPresent only: it is a bare []string with nowhere to put a
// separate attribution field, and nothing path-CLASSIFIES it. LeakFinding
// carries its attribution in LeakFinding.Source instead — see the loop above
// for why a prefix there is wrong.
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
