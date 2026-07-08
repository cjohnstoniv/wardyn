// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package gitremote deterministically detects the git remotes configured in a
// local directory tree, so Wardyn can ground a composed run's GitHub grant on the
// workspace's ACTUAL remotes rather than an LLM guess.
//
// It is read-only and runs NO subprocess: it parses .git/config and .gitmodules
// as plain files (so it can never trigger a git hook or a malicious
// include.path / core.fsmonitor in a repo's config). The walk is bounded (depth,
// .git count, file size), never follows symlinks, and fails safe — any error
// yields fewer/zero detected repos, never a grant on uncertainty.
package gitremote

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Bounds keep detection fast and safe on large/hostile trees.
const (
	maxDepth       = 4
	maxGitDirs     = 50
	maxConfigBytes = 1 << 20 // 1 MiB per config/.gitmodules file
)

// DetectGitHubRepos walks root (bounded, no symlink following) and returns the
// sorted, de-duplicated set of github.com "owner/repo" remotes found in
// .git/config and .gitmodules files, plus the sorted set of non-GitHub remote
// HOSTS (for an operator warning). It never returns an error: detection is
// best-effort and fail-safe.
func DetectGitHubRepos(root string) (github []string, otherHosts []string) {
	ghSet := map[string]struct{}{}
	otherSet := map[string]struct{}{}
	gitDirs := 0

	root = filepath.Clean(root)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil // skip unreadable entries; keep walking siblings
		}
		// Never descend or follow symlinks (a symlinked dir is reported as a
		// non-dir by WalkDir's lstat, so this also prevents escaping `root`).
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !d.IsDir() {
			if d.Name() == ".gitmodules" {
				scanRemotes(readCapped(p), ghSet, otherSet)
			}
			return nil
		}
		if depthUnder(root, p) > maxDepth {
			return fs.SkipDir
		}
		// A repo's .git is a directory (normal) or a "gitdir:" pointer file
		// (submodules / linked worktrees). Parse its config either way.
		gitPath := filepath.Join(p, ".git")
		fi, statErr := os.Lstat(gitPath)
		if statErr != nil {
			return nil
		}
		if gitDirs >= maxGitDirs {
			return fs.SkipDir
		}
		gitDirs++
		if cfg := resolveConfigPath(gitPath, fi, root); cfg != "" {
			scanRemotes(readCapped(cfg), ghSet, otherSet)
		}
		return nil
	})

	return ToSorted(ghSet), ToSorted(otherSet)
}

// Classify maps a git remote HOST to a broker-relevant category:
//   - "github"       — github.com, *.github.com
//   - "azure_devops" — dev.azure.com, *.visualstudio.com
//   - "gitlab"       — gitlab.com and self-managed gitlab.* hosts
//   - "other"        — anything else
//
// Matching is case-insensitive and tolerates a trailing dot. Pure string work
// (no subprocess), reusing the host forms parseRemoteURL already yields.
func Classify(host string) string {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	switch {
	case h == "github.com" || strings.HasSuffix(h, ".github.com"):
		return "github"
	case h == "dev.azure.com" || strings.HasSuffix(h, ".visualstudio.com"):
		return "azure_devops"
	case h == "gitlab.com" || strings.HasPrefix(h, "gitlab."):
		return "gitlab"
	default:
		return "other"
	}
}

// depthUnder returns how many path segments p is below root.
func depthUnder(root, p string) int {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." {
		return 0
	}
	return len(strings.Split(rel, string(filepath.Separator)))
}

// resolveConfigPath returns the path to the config file for a .git entry: a
// directory yields <gitPath>/config; a "gitdir: X" pointer file is resolved to
// <X>/config but ONLY if X stays inside root (else skipped, to avoid following a
// pointer outside the operator-selected dir). Non-following on symlinks too.
func resolveConfigPath(gitPath string, fi os.FileInfo, root string) string {
	if fi.IsDir() {
		return filepath.Join(gitPath, "config")
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	data := readCapped(gitPath)
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(gitPath), target)
	}
	target = filepath.Clean(target)
	if !within(root, target) {
		return "" // pointer escapes the workspace — ignore
	}
	return filepath.Join(target, "config")
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func readCapped(p string) []byte {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, maxConfigBytes)
	n, _ := f.Read(buf)
	return buf[:n]
}

// scanRemotes parses an INI-ish git config / .gitmodules body, collecting the
// `url = ...` value of every [remote "..."] and [submodule "..."] section.
func scanRemotes(body []byte, ghSet, otherSet map[string]struct{}) {
	if len(body) == 0 {
		return
	}
	inRemote := false
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			sec := strings.ToLower(strings.Trim(line, "[]"))
			// section header like: remote "origin"  OR  submodule "x"
			inRemote = strings.HasPrefix(sec, "remote ") || strings.HasPrefix(sec, "submodule ")
			continue
		}
		if !inRemote {
			continue
		}
		k, v, ok := splitKV(line)
		if !ok || strings.ToLower(k) != "url" {
			continue
		}
		classify(v, ghSet, otherSet)
	}
}

func splitKV(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, '=')
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

// classify turns one remote URL into either a github "owner/repo" or a
// non-GitHub host. Unsafe/unparseable values are dropped.
func classify(url string, ghSet, otherSet map[string]struct{}) {
	if url == "" || !safe(url) {
		return
	}
	host, ownerRepo := parseRemoteURL(url)
	if host == "" {
		return
	}
	if host == "github.com" {
		if ownerRepo != "" {
			ghSet[ownerRepo] = struct{}{}
		}
		return
	}
	otherSet[host] = struct{}{}
}

// parseRemoteURL extracts (host, "owner/repo") from the common remote URL forms.
// owner/repo is "" when it can't be cleanly extracted (host is still returned).
func parseRemoteURL(url string) (host, ownerRepo string) {
	s := url
	switch {
	case strings.HasPrefix(s, "https://"), strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "ssh://"), strings.HasPrefix(s, "git://"):
		s = s[strings.Index(s, "://")+3:]
		if at := strings.IndexByte(s, '@'); at >= 0 { // strip user@
			s = s[at+1:]
		}
		slash := strings.IndexByte(s, '/')
		if slash < 0 {
			return "", ""
		}
		host = strings.ToLower(s[:slash])
		if c := strings.IndexByte(host, ':'); c >= 0 { // strip :port
			host = host[:c]
		}
		return host, twoSegments(s[slash+1:])
	default:
		// scp-like: [user@]host:owner/repo
		at := strings.IndexByte(s, '@')
		if at >= 0 {
			s = s[at+1:]
		}
		colon := strings.IndexByte(s, ':')
		if colon < 0 {
			return "", ""
		}
		host = strings.ToLower(s[:colon])
		return host, twoSegments(s[colon+1:])
	}
}

// twoSegments returns "owner/repo" from a path tail, stripping a trailing .git
// and surrounding slashes; "" unless exactly two non-empty segments.
func twoSegments(p string) string {
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	parts := strings.Split(p, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

// safe rejects control chars / whitespace (mirrors internal/api repoFieldSafe so
// a detected value can never carry a shell/control payload downstream).
func safe(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return false
		}
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f', 0x85, 0xa0:
			return false
		}
	}
	return true
}

// ToSorted dedupes and sorts a set into a slice, nil if empty. Shared with
// internal/workspacescan, which already imports this package.
func ToSorted(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(m))
}
