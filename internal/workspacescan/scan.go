// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/gitremote"
)

// Bounds keep detection fast and safe on large/hostile trees (mirrors
// internal/gitremote's maxDepth/maxConfigBytes).
const (
	// maxDepth allows deep monorepo layouts (e.g. apps/<x>/overlays/prod/*
	// SealedSecrets sit ~5 levels down). The real DoS guard is maxManifestHits
	// + the skipDirNames vendor-tree exclusions, not depth.
	maxDepth               = 6
	maxManifestHits        = 200 // generous for a real monorepo, bounds a hostile tree
	maxUnrecognizedSamples = 20
	maxSampleBytes         = 2048 // stored sample prefix, well under the read cap below
	maxFileBytes           = 1 << 20
)

// skipDirNames are dependency/vendor directories we never descend into.
// Unlike gitremote (which only ever looks for a `.git` entry), this package
// walks every file for marker matching, so without this a single node_modules
// tree would bury the real manifests under thousands of nested
// dependency-internal package.json files.
var skipDirNames = map[string]struct{}{
	"node_modules":     {},
	"vendor":           {},
	".git":             {},
	"target":           {},
	"dist":             {},
	"build":            {},
	".venv":            {},
	"venv":             {},
	"__pycache__":      {},
	".tox":             {},
	".yarn":            {},
	".pnpm-store":      {},
	"bower_components": {},
}

// Scan walks root (bounded, read-only, no symlink following) and derives a
// WorkspaceProfile. It never returns an error: a scan that hits a bound or an
// unrecognized build system yields a lower-confidence profile, never a crash.
func Scan(root string) WorkspaceProfile {
	return DeriveProfile(CollectFacts(root))
}

// DeriveProfile is the control-plane re-derivation of a WorkspaceProfile from
// previously-emitted ScanFacts — the SAME logic Scan uses internally, so a
// local Scan(root) and a facts round trip (Scan → marshal → unmarshal →
// DeriveProfile) always agree. Untrusted-input safe: an unknown Marker id
// (e.g. from a future scanner version) is silently ignored, never trusted.
func DeriveProfile(facts ScanFacts) WorkspaceProfile {
	langs := map[string]struct{}{}
	pkgMgrs := map[string]struct{}{}
	egress := map[string]struct{}{}
	tools := map[string]struct{}{}
	hasDevcontainer := false
	hasDockerfile := false
	sawUnresolved := false

	for _, hit := range facts.ManifestsFound {
		row, ok := markersByID[hit.Marker]
		if !ok {
			continue // unknown marker id — ignore, fail safe
		}
		for _, l := range row.languages {
			langs[l] = struct{}{}
		}
		for _, pm := range row.packageManagers {
			pkgMgrs[pm] = struct{}{}
		}
		for _, e := range row.egress {
			egress[e] = struct{}{}
		}
		for _, t := range row.tools {
			tools[t] = struct{}{}
		}
		if row.isDevcontainer {
			hasDevcontainer = true
		}
		if row.isDockerfile {
			hasDockerfile = true
		}
		if row.unresolved {
			sawUnresolved = true
		}
	}

	p := WorkspaceProfile{
		Languages:       gitremote.ToSorted(langs),
		PackageManagers: gitremote.ToSorted(pkgMgrs),
		EgressDomains:   gitremote.ToSorted(egress),
		Tools:           gitremote.ToSorted(tools),
		GitRemotes:      facts.GitRemotes,
		HasDevcontainer: hasDevcontainer,
		HasDockerfile:   hasDockerfile,
		Source:          SourceDeterministic,
	}

	// Content-lane facts are untrusted (a sandboxed repo scan controls them):
	// validate charset/caps, coerce unknown kinds, and subtract suggested
	// hosts already allowed by the filename-keyed table. See detect.go.
	p.RequiredSecrets = validateSecretNeeds(facts.SecretRequirements)
	p.ServicesNeeded = validateServices(facts.ServicesFound, p.RequiredSecrets)
	p.SuggestedEgress = validateSuggestedHosts(facts.SuggestedEgress, egress)
	p.SecretFilesPresent = validateSecretFilePaths(facts.SecretFilesPresent)
	p.BuildMemoryMiB = validateBuildMemoryMiB(facts.BuildMemoryMiB)
	p.LeakFindings = validateLeakFindings(facts.LeakFindings)
	// Setup commands are synthesized from FIXED templates keyed on the (trusted,
	// marker-derived) package managers + which conventional script/target keys
	// exist — never copied from file content, so a hostile scripts.build can't
	// become a command. Advisory: operator-approved before they ever run.
	p.SetupCommands = deriveSetupCommands(pkgMgrs, tools, facts.ScriptKeys, facts.MakeTargets,
		has(tools, "maven-wrapper"), has(tools, "gradle-wrapper"))
	// Build-input content digest → busts the image cache on a devcontainer/
	// Dockerfile change even when the detected profile is otherwise identical.
	p.ContextHash = contextHashOf(facts.BuildInputHashes)

	lowConfidence := facts.Truncated || len(facts.UnrecognizedSamples) > 0
	switch {
	case lowConfidence:
		p.Confidence = ConfidenceLow
		p.NeedsReview = true
	case sawUnresolved:
		p.Confidence = ConfidenceMedium
		p.NeedsReview = true
	default:
		p.Confidence = ConfidenceHigh
	}
	return p
}

// CollectFacts walks root and collects the raw, bounded ScanFacts. This is the
// entry point the in-sandbox wardyn-scan binary calls: it emits the untrusted
// facts a governed repo scan ships back over the brokered scan-result route,
// which the control plane re-derives into a WorkspaceProfile (never trusting
// the facts on faith — see DeriveProfile).
func CollectFacts(root string) ScanFacts {
	var facts ScanFacts
	st := &collectState{facts: &facts} // carries per-scan content-lane budgets
	root = filepath.Clean(root)
	seen := map[string]struct{}{} // dedup guard for ManifestsFound

	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil // skip unreadable entries; keep walking siblings
		}
		// Never descend or follow symlinks.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if _, skip := skipDirNames[d.Name()]; skip && p != root {
				return fs.SkipDir
			}
			if depthUnder(root, p) > maxDepth {
				return fs.SkipDir
			}
			return nil
		}

		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !pathSafe(rel) {
			return nil // a hostile control-char filename — drop, fail safe
		}
		name := d.Name()

		// Content lane (detect.go): names-only extraction from a fixed set of
		// well-known files. Runs BEFORE the marker lookup because compose and
		// Dockerfile are both marker rows AND detector targets — the marker
		// lane's `return nil` below must not starve the detector.
		detectContent(rel, name, p, st)

		// Build-input content hash (cache-key hardening): hash the CONTENT of a
		// repo's own devcontainer.json / Dockerfile so a build-input change busts
		// the image cache even when the detected profile is unchanged.
		if isBuildInputFile(rel, name) {
			if h := hashFileContent(p); h != "" {
				if facts.BuildInputHashes == nil {
					facts.BuildInputHashes = map[string]string{}
				}
				if len(facts.BuildInputHashes) < 16 {
					facts.BuildInputHashes[rel] = h
				}
			}
		}

		if m, ok := lookupMarker(rel, name); ok {
			if m.isDevcontainer {
				facts.HasDevcontainer = true
			}
			if m.isDockerfile {
				facts.HasDockerfile = true
			}
			if _, dup := seen[rel]; !dup {
				if len(facts.ManifestsFound) >= maxManifestHits {
					facts.Truncated = true
				} else {
					seen[rel] = struct{}{}
					facts.ManifestsFound = append(facts.ManifestsFound, ManifestHit{Path: rel, Marker: m.id})
				}
			}
			return nil
		}

		if _, candidate := unmappedBuildFiles[name]; candidate {
			if len(facts.UnrecognizedSamples) >= maxUnrecognizedSamples {
				facts.Truncated = true
				return nil
			}
			facts.UnrecognizedSamples = append(facts.UnrecognizedSamples, UnrecognizedSample{
				Path:    rel,
				Content: scrub(string(readCapped(p))),
			})
		}
		return nil
	})

	gh, other := gitremote.DetectGitHubRepos(root)
	facts.GitRemotes = GitRemotes{GitHub: gh, OtherHosts: other}

	slices.SortFunc(facts.ManifestsFound, func(a, b ManifestHit) int {
		if a.Path != b.Path {
			return strings.Compare(a.Path, b.Path)
		}
		return strings.Compare(a.Marker, b.Marker)
	})
	return facts
}

// lookupMarker resolves a walked file (rel: slash-separated path relative to
// the scan root; name: base filename) to its marker row, if any. Path/suffix
// patterns are checked first since they're more specific than a bare
// filename; a plain markersByID[name] lookup handles every other row.
func lookupMarker(rel, name string) (marker, bool) {
	if name == "devcontainer.json" && strings.Contains(rel, ".devcontainer/") {
		return markersByID[idDevcontainerNested], true
	}
	if strings.HasSuffix(rel, ".cargo/config.toml") || strings.HasSuffix(rel, ".cargo/config") {
		return markersByID[idCargoConfig], true
	}
	if strings.HasSuffix(rel, "gradle/wrapper/gradle-wrapper.properties") {
		return markersByID[idGradleWrapper], true
	}
	if strings.HasSuffix(rel, ".mvn/wrapper/maven-wrapper.properties") {
		return markersByID[idMavenWrapper], true
	}
	if strings.HasPrefix(rel, ".github/workflows/") && (strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
		return markersByID[idGithubWorkflow], true
	}
	if rel == idSphinxConf || strings.HasSuffix(rel, "/"+idSphinxConf) {
		return markersByID[idSphinxConf], true
	}
	if strings.HasSuffix(name, ".gemspec") {
		return markersByID[idGemspec], true
	}
	if strings.HasSuffix(name, ".csproj") || strings.HasSuffix(name, ".fsproj") || strings.HasSuffix(name, ".vbproj") {
		return markersByID[idDotnetProj], true
	}
	if m, ok := markersByID[name]; ok {
		return m, true
	}
	return marker{}, false
}

// depthUnder returns how many path segments p is below root (see
// gitremote.depthUnder — duplicated rather than exported, it's three lines).
func depthUnder(root, p string) int {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." {
		return 0
	}
	return len(strings.Split(rel, string(filepath.Separator)))
}

// maxHashBytes bounds streaming content-hashing of a build-input file.
const maxHashBytes = 16 << 20

// isBuildInputFile reports whether a walked file is a build INPUT whose content
// changing should bust the built-image cache (the repo's own devcontainer /
// Dockerfile). The generated-devcontainer path is already profile-derived.
func isBuildInputFile(rel, name string) bool {
	if name == "Dockerfile" || name == "Containerfile" || name == ".devcontainer.json" {
		return true
	}
	return strings.HasSuffix(rel, ".devcontainer/devcontainer.json")
}

// hashFileContent streams a hex sha256 of p's content (bounded), "" on error.
func hashFileContent(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, maxHashBytes)); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// contextHashOf combines the per-file build-input hashes into one deterministic
// digest (sorted by path so order is stable), "" when there are none.
func contextHashOf(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	h := sha256.New()
	for _, rel := range slices.Sorted(maps.Keys(m)) {
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write([]byte(m[rel]))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// readCapped reads at most maxFileBytes from p, failing safe to nil.
func readCapped(p string) []byte {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, maxFileBytes)
	n, _ := f.Read(buf)
	if n > maxSampleBytes {
		n = maxSampleBytes // only a short prefix is ever kept as a "sample"
	}
	return buf[:n]
}

// pathSafe rejects control characters (mirrors gitremote.safe(), but — unlike
// a single-line remote URL — a real filesystem path may legitimately contain
// a space, so only true control/DEL bytes are rejected, not whitespace).
func pathSafe(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return false
		}
	}
	return true
}

// scrub strips control characters (other than \t\n\r, which are normal in
// multi-line file content) from a bounded content sample before it's stored
// in an UnrecognizedSample.
func scrub(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			// drop
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
