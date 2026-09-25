// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package gitremote deterministically detects the git remotes configured in a
// local directory tree, so Wardyn can ground a composed run's GitHub grant on the
// workspace's ACTUAL remotes rather than an LLM guess.
//
// It is read-only and runs NO subprocess: it parses .git/config and .gitmodules
// as plain files (so it can never trigger a git hook or a malicious
// include.path / core.fsmonitor in a repo's config). The walk is bounded (depth,
// .git count, file size), never follows symlinks, reads REGULAR files only (a
// FIFO named .gitmodules would otherwise block the scan's goroutine forever),
// and fails safe — any error yields fewer/zero detected repos, never a grant on
// uncertainty.
package gitremote

import (
	"errors"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode"
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

	root = ResolveRoot(root)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil // skip unreadable entries; keep walking siblings
		}
		// Never descend or follow symlinks (a symlinked dir is reported as a
		// non-dir by WalkDir's lstat, so this also prevents escaping `root`).
		// The ROOT is exempt: WalkDir lstats it like every other entry, so a
		// root that is ITSELF a link (~/work -> /mnt/d/work) ended the walk on
		// its very first callback and detection reported zero repos — which
		// the caller cannot tell from a tree with no git remotes, and which
		// costs the run its GitHub grant (B11b-F1). ResolveRoot has already
		// canonicalised it; this arm is what still holds when that failed.
		if p != root && d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !d.IsDir() {
			if d.Name() == ".gitmodules" {
				scanRemotes(readCapped(p), ghSet, otherSet)
			}
			return nil
		}
		// Never descend a repo's .git internals: detection stats <parent>/.git
		// directly (below), so re-walking objects/refs/logs is pure waste that
		// makes the scan O(git-objects) instead of O(repo-roots).
		if d.Name() == ".git" {
			return fs.SkipDir
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

// ResolveRoot canonicalises a scan root before a walk: cleaned, and with every
// symlink in it resolved. It is the ONE line that keeps a walk's own view of a
// workspace and the sandbox's mount of it talking about the same directory —
// dispatch bind-mounts the RESOLVED tree, so a scanner that stops at the link
// describes a tree the run never sees.
//
// Resolution failure (a broken link, a directory the daemon cannot traverse)
// falls back to the cleaned path: the walk then finds nothing, which is the
// fail-safe answer both callers already treat as "no evidence", never a claim
// about a tree nobody read. Shared with internal/workspacescan, whose
// CollectFacts had the identical bug on the identical locator.
func ResolveRoot(root string) string {
	clean := filepath.Clean(root)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return clean
	}
	return resolved
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

// readCapped reads at most maxConfigBytes from a REGULAR file, failing safe to
// nil for anything else. It is the single chokepoint behind all three read
// sites (the .gitmodules scan, the .git/config scan, and resolveConfigPath's
// gitdir pointer), which is why the whole of B11a-F5 is fixed here.
//
// Three properties the bare os.Open + single Read did not have:
//
//   - O_NOFOLLOW. The walk skips symlinks everywhere EXCEPT this final open, so
//     a .git/config symlink was the one place the package doc's "never follows
//     symlinks" promise did not hold; it read whatever the scanning uid could
//     reach.
//   - O_NONBLOCK + IsRegular. A FIFO named .gitmodules (or .git/config) blocks
//     open(2) forever waiting for a writer, and CollectFacts runs on an HTTP
//     handler goroutine with no ctx — one such file wedged that request
//     permanently. O_NONBLOCK makes the open return; the fstat is what decides
//     nothing is read from it. Doing it in that order is also race-free: the
//     flags pick what gets opened, the fstat judges the thing actually opened,
//     so there is no path-based window between the two.
//   - io.ReadFull over a LimitReader. A single Read is documented to return
//     FEWER bytes than the buffer holds, which silently truncated a large
//     config mid-line and dropped every remote after the break.
//
// The first two properties are OpenRegular's, which internal/workspacescan
// shares — its own walk had four bare os.Open calls on the same handler path.
func readCapped(p string) []byte {
	f, err := OpenRegular(p)
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

// OpenRegular opens a file for reading that is PROVABLY an ordinary file and
// is never reached through a symlink. It is the shared door for every read a
// workspace walk performs, because every such walk runs on an HTTP handler
// goroutine with no ctx and over a tree the scanned party controls.
//
// O_NOFOLLOW refuses a symlinked final component (the walk skips links
// everywhere else, so this is the one remaining place a tree could point the
// scanner at a file outside itself). O_NONBLOCK makes open(2) RETURN on a FIFO
// instead of waiting forever for a writer — one such file wedged the request
// permanently — and the fstat is what then decides nothing is read from it.
// That order is also race-free: the flags pick what gets opened and the fstat
// judges the thing actually opened, so there is no path-based window between
// the two.
//
// The caller closes. The caller also bounds what it reads: this says WHAT may
// be opened, never HOW MUCH comes back.
func OpenRegular(p string) (*os.File, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, errNotRegular
	}
	return f, nil
}

// errNotRegular is what OpenRegular returns for a FIFO, device, socket or
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
	// takes the URL arm while "O/R" survives as written. Before this, an
	// upper-case scheme fell through to the scp arm and the SCHEME ITSELF came
	// back as the host — "https" and "file" really appeared in the operator's
	// "other hosts" warning (B11a-F8).
	switch lower := strings.ToLower(s); {
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "http://"),
		strings.HasPrefix(lower, "ssh://"), strings.HasPrefix(lower, "git://"):
		s = s[strings.Index(s, "://")+3:]
		// LastIndexByte, not IndexByte: userinfo ends at the LAST "@" (git and
		// net/url both read it that way), so "ssh://a@b@github.com/o/r" is
		// github.com — IndexByte alone would take "b@github.com" as the host.
		if at := strings.LastIndexByte(s, '@'); at >= 0 {
			s = s[at+1:]
		}
		slash := strings.IndexByte(s, '/')
		if slash < 0 {
			return "", ""
		}
		return hostOnly(s[:slash]), twoSegments(s[slash+1:])
	default:
		// A value carrying "://" is a URL, never an scp target — that is git's
		// own rule. A scheme this package does not handle (file://, ftp://, a
		// typo) is therefore DROPPED rather than scp-parsed into the host
		// "file" (B11a-F8).
		if strings.Contains(s, "://") {
			return "", ""
		}
		// scp-like: [user@]host:owner/repo
		if at := strings.LastIndexByte(s, '@'); at >= 0 {
			s = s[at+1:]
		}
		// The separating colon is the one AFTER the closing bracket when the
		// host is a bracketed IPv6 literal — git's own scp-like syntax accepts
		// `git@[2001:db8::1]:acme/web.git`. Taking the FIRST colon split that
		// host into the bogus "[2001", which is precisely the string B11a-F8/F11
		// exist to keep out of the operator's "other hosts" warning; the URL arm
		// was fixed and this one was not.
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
// bracketed IPv6 literal. Splitting at the first ":" truncated
// "[2001:db8::1]:2222" to "[2001" — the same class as the git helper's
// B11a-F11, and it is the string the operator reads in the "other hosts"
// warning.
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

// FieldSafe reports whether s carries no control character and no whitespace.
// It is THE predicate for both doors that judge an attacker-influenceable repo
// slug or remote URL: this package's classify(), and internal/api's
// repoFieldSafe, which delegates here.
//
// One predicate, deliberately (B11a-F10). The previous local copy carried a
// FIXED whitespace list and a comment claiming to mirror repoFieldSafe — which
// had since moved to unicode.IsSpace, so every Unicode space separator
// (U+2000..U+200A, U+3000, …) passed here while the api door rejected it. Two
// doors judging one value by different rules is how a detected remote reaches a
// grant the authoring door would have refused.
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
