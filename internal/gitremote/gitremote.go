// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package gitremote deterministically detects the git remotes configured in
// a local directory tree, so Wardyn can ground a composed run's GitHub grant
// on the workspace's ACTUAL remotes rather than a guess.
//
// It is read-only and runs NO subprocess: it parses .git/config and
// .gitmodules as plain files (never triggering a git hook or a malicious
// include.path / core.fsmonitor). The walk is bounded (depth, .git count,
// file size), never follows symlinks, reads REGULAR files only, and fails
// safe — any error yields fewer/zero detected repos, never a grant on
// uncertainty.
package gitremote

import (
	"errors"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode"

	"github.com/cjohnstoniv/wardyn/internal/foldersync"
)

// Bounds keep detection fast and safe on large/hostile trees.
const (
	maxDepth       = 4
	maxGitDirs     = 50
	maxConfigBytes = 1 << 20 // 1 MiB per config/.gitmodules file
)

// skipDirNames are package-manager trees the walk never descends: dependencies
// the workspace did not author, which can hold thousands of directories and
// whose own repos are not the workspace's remotes. A subset of workspacescan's
// list: vendor, target, dist and build can hold a checked-out repo or submodule.
var skipDirNames = map[string]struct{}{
	"node_modules": {}, ".venv": {}, "venv": {}, "__pycache__": {}, ".tox": {},
	".yarn": {}, ".pnpm-store": {}, "bower_components": {},
}

// DetectGitHubRepos walks root (bounded, no symlink following) and returns the
// sorted, de-duplicated set of github.com "owner/repo" remotes found in
// .git/config and .gitmodules files, plus the sorted set of non-GitHub remote
// HOSTS (for an operator warning). It never returns an error: detection is
// best-effort and fail-safe. Canonicalised first (ResolveRoot), so a root that
// is itself a link is walked; then read only through the opened directory.
func DetectGitHubRepos(root string) (github []string, otherHosts []string) {
	dir, err := os.OpenRoot(ResolveRoot(root))
	if err != nil {
		return nil, nil
	}
	defer dir.Close()
	return DetectGitHubReposIn(dir)
}

// DetectGitHubReposIn is DetectGitHubRepos over an open directory: it lists and
// reads only through dir and follows no link at any component (foldersync.FS,
// OpenRegularIn), so a folder swapped for a link after the walk saw it is not
// read through.
func DetectGitHubReposIn(dir *os.Root) (github []string, otherHosts []string) {
	ghSet := map[string]struct{}{}
	otherSet := map[string]struct{}{}
	gitDirs := 0

	fsys := foldersync.FS(dir)
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil // skip unreadable entries; keep walking siblings
		}
		// Never descend or follow symlinks (also prevents escaping the root).
		if p != "." && d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !d.IsDir() {
			if d.Name() == ".gitmodules" {
				scanRemotes(readCapped(dir, p), ghSet, otherSet)
			}
			return nil
		}
		// Never descend a repo's .git internals: it is stat'd directly below, so
		// re-walking it would make the scan O(git-objects) instead of O(repo-roots).
		// The root itself is never skipped by name: a workspace folder called "node_modules" is the workspace.
		if _, skip := skipDirNames[d.Name()]; (skip && p != ".") || d.Name() == ".git" {
			return fs.SkipDir
		}
		if depthUnder(p) > maxDepth {
			return fs.SkipDir
		}
		// A repo's .git is a directory (normal) or a "gitdir:" pointer file
		// (submodules / linked worktrees). Parse its config either way.
		gitPath := path.Join(p, ".git")
		fi, statErr := fs.Stat(fsys, gitPath) // FS.Stat looks at the name without following it
		if statErr != nil {
			return nil
		}
		if gitDirs >= maxGitDirs {
			return fs.SkipDir
		}
		gitDirs++
		if cfg := resolveConfigPath(dir, gitPath, fi); cfg != "" {
			scanRemotes(readCapped(dir, cfg), ghSet, otherSet)
		}
		return nil
	})

	return ToSorted(ghSet), ToSorted(otherSet)
}

// ResolveRoot canonicalises a scan root before a walk: cleaned, with every
// symlink resolved. Dispatch bind-mounts the RESOLVED tree, so a scanner
// stopping at the link would describe a tree the run never sees. Resolution
// failure falls back to the cleaned path — the walk then finds nothing, the
// fail-safe "no evidence" answer both callers already expect. Shared with
// internal/workspacescan.
func ResolveRoot(root string) string {
	clean := filepath.Clean(root)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return clean
	}
	return resolved
}

// depthUnder returns how many path segments the slash-separated rel is below the walk's root.
func depthUnder(rel string) int {
	if rel == "." {
		return 0
	}
	return strings.Count(rel, "/") + 1
}

// resolveConfigPath returns the slash-separated path inside dir of the config
// file for a .git entry: a directory yields <gitPath>/config; a "gitdir: X"
// pointer file is resolved to <X>/config but ONLY if X stays inside dir and
// opens with no link at any component (else skipped). An absolute X is taken
// against the path dir was opened at.
func resolveConfigPath(dir *os.Root, gitPath string, fi fs.FileInfo) string {
	if fi.IsDir() {
		return path.Join(gitPath, "config")
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	data := readCapped(dir, gitPath)
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	target := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if filepath.IsAbs(target) {
		rel, err := filepath.Rel(filepath.Clean(dir.Name()), filepath.Clean(target))
		if err != nil {
			return ""
		}
		target = filepath.ToSlash(rel)
	} else {
		target = path.Join(path.Dir(gitPath), filepath.ToSlash(target))
	}
	if !fs.ValidPath(target) {
		return "" // pointer escapes the workspace — ignore
	}
	// A lexically inner path can still run through a symlink out of the root, and
	// a target that does not exist has no config to read.
	sub, err := foldersync.OpenDir(dir, target)
	if err != nil {
		return ""
	}
	_ = sub.Close()
	return path.Join(target, "config")
}

// readCapped reads at most maxConfigBytes from the REGULAR file rel inside dir,
// failing safe to nil for anything else. It is the single chokepoint behind all
// three read sites: OpenRegularIn follows no link at any component and gives
// O_NONBLOCK+IsRegular (a FIFO can't block open(2) forever), wrapped with
// io.ReadFull over a LimitReader since a single Read can silently truncate
// mid-line.
func readCapped(dir *os.Root, rel string) []byte {
	f, err := OpenRegularIn(dir, rel)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, maxConfigBytes)
	n, err := io.ReadFull(io.LimitReader(f, maxConfigBytes), buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil // a genuine read error: fail safe, as everything here does
	}
	return buf[:n]
}

// OpenRegularIn opens the file rel (slash-separated, inside dir) for reading
// when it is PROVABLY an ordinary file reached through no symlink — the shared
// door for every read a workspace walk performs, since such walks run on an
// HTTP handler goroutine over a tree the scanned party controls. Each folder
// on the way is opened by foldersync.OpenDir, which follows no link at any
// component, and the file opened must be the entry its folder holds under that
// name (a link there is refused). O_NONBLOCK makes open(2) return on a FIFO
// instead of waiting forever, and the fstat then decides nothing is read from
// it. The caller closes and bounds what it reads; this only says WHAT may be
// opened.
func OpenRegularIn(dir *os.Root, rel string) (*os.File, error) {
	if !fs.ValidPath(rel) || rel == "." {
		return nil, errNotRegular
	}
	parent, leaf := path.Split(rel)
	if parent != "" {
		sub, err := foldersync.OpenDir(dir, strings.TrimSuffix(parent, "/"))
		if err != nil {
			return nil, err
		}
		defer sub.Close()
		dir = sub
	}
	f, err := dir.OpenFile(leaf, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, ferr := f.Stat()
	li, lerr := dir.Lstat(leaf)
	if ferr != nil || lerr != nil || !fi.Mode().IsRegular() || !os.SameFile(fi, li) {
		f.Close()
		return nil, errNotRegular
	}
	return f, nil
}

// errNotRegular is what OpenRegularIn returns for a FIFO, device, socket or
// directory. Callers here all fail safe on ANY error, so it is deliberately
// not distinguishable from an unreadable file: neither one is evidence.
var errNotRegular = errors.New("gitremote: not a regular file")

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
	if url == "" || !FieldSafe(url) {
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
	// The SCHEME is case-insensitive (RFC 3986); the path is not. Match on a
	// lowered copy and keep slicing the original, so "HTTPS://github.com/O/R"
	// takes the URL arm while "O/R" survives as written.
	switch lower := strings.ToLower(s); {
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "http://"),
		strings.HasPrefix(lower, "ssh://"), strings.HasPrefix(lower, "git://"):
		s = s[strings.Index(s, "://")+3:]
		// LastIndexByte, not IndexByte: userinfo ends at the LAST "@" (git and
		// net/url both read it that way), so "ssh://a@b@github.com/o/r" is
		// github.com.
		if at := strings.LastIndexByte(s, '@'); at >= 0 {
			s = s[at+1:]
		}
		slash := strings.IndexByte(s, '/')
		if slash < 0 {
			return "", ""
		}
		return hostOnly(s[:slash]), twoSegments(s[slash+1:])
	default:
		// A value carrying "://" is a URL, never an scp target (git's own rule),
		// so an unhandled scheme (file://, ftp://, a typo) is DROPPED rather
		// than scp-parsed into the host itself.
		if strings.Contains(s, "://") {
			return "", ""
		}
		// scp-like: [user@]host:owner/repo
		if at := strings.LastIndexByte(s, '@'); at >= 0 {
			s = s[at+1:]
		}
		// The separating colon is the one AFTER the closing bracket when the
		// host is a bracketed IPv6 literal — git's scp-like syntax accepts
		// `git@[2001:db8::1]:acme/web.git`, and the FIRST colon would split
		// that host into the bogus "[2001".
		colon := strings.IndexByte(s, ':')
		if strings.HasPrefix(s, "[") {
			end := strings.IndexByte(s, ']')
			if end < 0 {
				return "", "" // unterminated bracket: fail safe, as everything here does
			}
			rest := strings.IndexByte(s[end+1:], ':')
			if rest < 0 {
				return "", ""
			}
			colon = end + 1 + rest
		}
		if colon < 0 {
			return "", ""
		}
		return hostOnly(s[:colon]), twoSegments(s[colon+1:])
	}
}

// hostOnly lowercases a URL authority and strips an optional port, unwrapping a
// bracketed IPv6 literal (splitting at the first ":" would truncate
// "[2001:db8::1]:2222" to "[2001").
func hostOnly(authority string) string {
	h := strings.ToLower(authority)
	if bare, _, err := net.SplitHostPort(h); err == nil {
		return bare
	}
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		return h[1 : len(h)-1]
	}
	return h
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

// FieldSafe reports whether s carries no control character and no
// whitespace. It is THE predicate for both doors that judge an
// attacker-influenceable repo slug or remote URL: this package's classify(),
// and internal/api's repoFieldSafe, which delegates here — one predicate, so
// the two doors can never judge the same value by different rules.
func FieldSafe(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	})
}

// ToSorted dedupes and sorts a set into a slice, nil if empty. Shared with
// internal/workspacescan, which already imports this package.
func ToSorted(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	return slices.Sorted(maps.Keys(m))
}
