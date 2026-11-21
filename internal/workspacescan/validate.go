// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"maps"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/gitremote"
	"github.com/cjohnstoniv/wardyn/internal/hostrules"
)

// --- DeriveProfile-side validation (facts are untrusted input) ---

// validateSecretNeeds drops non-conforming names, coerces unknown kinds to
// "generic", dedupes by name (required beats optional; a real config kind
// beats a weak ci/code/generic sighting), caps and sorts for determinism.
// omitted counts the valid names the cap left out.
func validateSecretNeeds(raw []SecretNeed) (out []SecretNeed, omitted int) {
	byName := make(map[string]SecretNeed, len(raw))
	for _, n := range raw {
		name := strings.TrimSpace(n.Name)
		if !needNameRE.MatchString(name) {
			continue
		}
		// Filtered here, not at collection time, since this is the one boundary
		// both scan lanes cross: host-side Scan and the repo-lane's re-derivation
		// of untrusted uploaded facts via DeriveProfile. A collection-site filter
		// would miss the latter.
		if isPlatformEnvName(name) {
			continue
		}
		kind := n.Kind
		if _, ok := knownSecretKinds[kind]; !ok {
			kind = "generic"
		}
		if prev, seen := byName[name]; seen {
			if prev.Optional && !n.Optional {
				prev.Optional = false
			}
			// A concrete config classification supersedes a weak ci/code/generic
			// one, so the badge reflects the strongest evidence.
			if _, prevWeak := weakKinds[prev.Kind]; prevWeak {
				if _, newWeak := weakKinds[kind]; !newWeak {
					prev.Kind = kind
				}
			}
			byName[name] = prev
			continue
		}
		byName[name] = SecretNeed{Name: name, Kind: kind, Optional: n.Optional}
	}
	if len(byName) == 0 {
		return nil, 0
	}
	// Priority cap: credential-DECLARING secrets rank ahead of advisory code/CI
	// references, so source-grep noise can never evict a real credential.
	// Output re-sorted alpha for a stable ProfileHash.
	var strong, weak []SecretNeed
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		if s := byName[name]; s.Kind == "code" || s.Kind == "ci" {
			weak = append(weak, s)
		} else {
			strong = append(strong, s)
		}
	}
	out = append(strong, weak...)
	if len(out) > maxSecretNeeds {
		omitted = len(out) - maxSecretNeeds
		out = out[:maxSecretNeeds]
	}
	slices.SortFunc(out, func(a, b SecretNeed) int { return strings.Compare(a.Name, b.Name) })
	return out, omitted
}

// validateServices keeps only ids from the fixed service map, plus those
// implied by validated secret kinds; deduped/sorted/capped.
func validateServices(raw []string, needs []SecretNeed) []string {
	allowed := map[string]struct{}{}
	for _, v := range serviceByImagePrefix {
		allowed[v] = struct{}{}
	}
	set := map[string]struct{}{}
	for _, s := range raw {
		if _, ok := allowed[s]; ok {
			set[s] = struct{}{}
		}
	}
	for _, n := range needs {
		// Derive from the NAME, not the stored kind (which carries provenance,
		// e.g. deploy/code/ci) so MINIO_ROOT_PASSWORD still implies its service.
		if svc, ok := serviceForKind[classifySecretKind(n.Name)]; ok {
			set[svc] = struct{}{}
		}
	}
	out := gitremote.ToSorted(set)
	if len(out) > maxServices {
		out = out[:maxServices]
	}
	return out
}

// validateSuggestedHosts normalizes/validates hosts, drops ones already in
// the filename-keyed allowed set, and caps + sorts; cut reports a cap drop.
func validateSuggestedHosts(raw []string, allowedEgress map[string]struct{}) (out []string, cut bool) {
	set := map[string]struct{}{}
	for _, h := range raw {
		host := hostrules.HostOf(h) // normalize + validate; "" if invalid
		if host == "" {
			continue
		}
		if _, already := allowedEgress[host]; already {
			continue
		}
		set[host] = struct{}{}
	}
	out = gitremote.ToSorted(set)
	if len(out) > maxSuggested {
		return out[:maxSuggested], true
	}
	return out, false
}

// validateSecretFilePaths keeps pathSafe, bounded-length relative paths;
// cut reports a cap drop.
func validateSecretFilePaths(raw []string) (out []string, cut bool) {
	set := map[string]struct{}{}
	for _, p := range raw {
		if p == "" || len(p) > maxSecretPathLn || !pathSafe(p) || strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			continue
		}
		set[p] = struct{}{}
	}
	out = gitremote.ToSorted(set)
	if len(out) > maxSecretFiles {
		return out[:maxSecretFiles], true
	}
	return out, false
}

// validateBuildMemoryMiB bounds an untrusted build heap to a sane ceiling
// (0 when unset/garbage).
func validateBuildMemoryMiB(v int) int {
	if v < 0 {
		return 0
	}
	if v > maxBuildMemMiB {
		return maxBuildMemMiB
	}
	return v
}

// validateLeakFindings keeps only known-kind findings with a safe relative
// path, dedupes, then sorts riskiest-first and caps (capLeakFindings). Never
// carries a value. truncated reports a cap drop, which the caller turns into
// NeedsReview rather than a silent drop.
func validateLeakFindings(raw []LeakFinding) (out []LeakFinding, truncated bool) {
	type key struct {
		path, kind string
		line       int
	}
	seen := map[key]struct{}{}
	for _, f := range raw {
		if _, ok := leakKinds[f.Kind]; !ok {
			continue
		}
		if f.Path == "" || len(f.Path) > maxSecretPathLn || !pathSafe(f.Path) || strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "..") {
			continue
		}
		line := f.Line
		if line < 0 {
			line = 0
		}
		k := key{f.Path, f.Kind, line}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, LeakFinding{Path: f.Path, Kind: f.Kind, Line: line})
	}
	return capLeakFindings(out)
}
