// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// MEMBER-authored host bind mounts (SECURITY-CRITICAL — the one place a
// NON-operator supplies a host path that gets bound into a sandbox).
//
// Everything in mount.go assumes the mount source was authored by an
// operator. A member-owned workspace (types.Workspace.OwnedBy) breaks that
// assumption on purpose — a desktop developer onboards their own project
// directory without an admin authoring the mount — so a member source runs
// the operator deny-list (ValidateMountSource) AND, additively:
//
//  1. ROOT ALLOWLIST. The source's canonicalized real path must sit inside
//     one of the operator/MDM-set roots (WARDYN_USER_WORKSPACE_ROOTS, or that
//     member's own entry in WARDYN_USER_WORKSPACE_ROOTS_MAP, which REPLACES
//     the shared list rather than adding to it). No roots configured => a
//     member local_dir mount is refused outright (fail closed).
//  2. CANONICALIZE, THEN MATCH. The within-root test runs on the EvalSymlinks
//     result, so a symlink INSIDE a root pointing OUT of every root is
//     refused — the escape a lexical prefix check misses. Unlike
//     ValidateMountSource (which falls through to lexical-only for the
//     remote-daemon case), an unresolvable member source is REFUSED: the
//     allowlist can't be asserted about a path this process can't see.
//  3. DOTFILE DENY. The real path may neither BE nor TRAVERSE a credential
//     dotfile dir (~/.ssh, ~/.aws, ~/.claude, ...), belt-and-braces with (1):
//     an operator who points a root at $HOME still can't let a member mount
//     their own ~/.ssh.
//
// WRITABLE is a separate, narrower allowlist (WARDYN_USER_WRITABLE_ROOTS
// minus WARDYN_USER_WRITABLE_DENY, deny winning) because a writable bind
// widens the residual; both unset means NO writable member mounts. None of
// this narrows an operator mount.
//
// RESIDUAL: the bind-time re-check resolves the real path as late as this
// process can, but validate and ContainerCreate are still not atomic — the
// same TOCTOU window ValidateMountSource's own residual note documents.
// Bounded by three things: the roots are operator-set, so a member can only
// race WITHIN the declared roots, never enlarge them; the dotfile deny-list
// matches the post-EvalSymlinks real path, so a won race landing on ~/.ssh is
// still refused; and a writable bind is held to the writable roots and deny on
// that same real path.

// memberDeniedSegments are path SEGMENTS a member mount's resolved real path
// may neither end in nor traverse. Modeled on deniedSourcePrefixes (mount.go)
// but segment-matched rather than prefix-matched: these live under a member's
// home, not at fixed absolute paths.
var memberDeniedSegments = map[string]bool{
	".ssh":             true, // SSH private keys
	".aws":             true, // AWS SSO cache / static credentials
	".claude":          true, // the harness subscription OAuth creds
	".wardyn":          true, // staged claude-creds
	".gnupg":           true,
	".docker":          true, // registry auth (and config that can name a socket)
	".kube":            true, // cluster credentials
	".netrc":           true,
	".git-credentials": true,
}

// memberDeniedPairs are two-segment denials — a bare ".config" or ".git" is an
// ordinary directory a project legitimately contains, but these two children
// hold credentials (gh's token) and remote/credential config.
var memberDeniedPairs = [][2]string{
	{".config", "gh"},
	{".git", "config"},
}

// UserMountPolicy is the operator/MDM-set posture for MEMBER-authored
// local_dir binds. The zero value means "no member local_dir mounts at all",
// the fail-closed default.
//
// Every field comes from operator/MDM-set ENV (cmd/wardynd), never
// SiteConfig — site config is a full-replace row a single bad PUT can blank,
// and this is a security ceiling.
type UserMountPolicy struct {
	// Roots is the shared allowlist (WARDYN_USER_WORKSPACE_ROOTS): absolute,
	// cleaned host prefixes any member's local_dir source must resolve into.
	Roots []string
	// RootsByPrincipal (WARDYN_USER_WORKSPACE_ROOTS_MAP) is the per-member
	// override. A principal with an entry uses ONLY that entry — per-member
	// REPLACES shared, since it exists to be the more restrictive control,
	// and a union would make adding a row widen rather than narrow. Keys are
	// lowercased principals (OIDC sub or email).
	RootsByPrincipal map[string][]string
	// WritableRoots (WARDYN_USER_WRITABLE_ROOTS) is where a member may mark
	// their own mount writable. Empty = nowhere.
	WritableRoots []string
	// WritableDeny (WARDYN_USER_WRITABLE_DENY) carves holes in WritableRoots: a
	// member may write neither inside an entry nor to a directory that
	// contains one. Deny WINS over allow, and is checked first.
	WritableDeny []string
}

// Configured reports whether any member root is set at all — i.e. whether
// member local_dir onboarding is available on this deployment.
func (p UserMountPolicy) Configured() bool {
	return len(p.Roots) > 0 || len(p.RootsByPrincipal) > 0
}

// RootsFor returns the roots that bound one member's mounts: their own map
// entry when they have one (REPLACING the shared list), otherwise the shared
// list. An empty result means this member may not mount a host dir at all.
func (p UserMountPolicy) RootsFor(principal string) []string {
	if own, ok := p.RootsByPrincipal[strings.ToLower(strings.TrimSpace(principal))]; ok {
		return own
	}
	return p.Roots
}

// ValidateUserMount is the AUTHORING-time gate (workspace onboarding and
// run-create): the full source check for principal, plus the writable
// allowlist when the source asks to be mounted read-write. A nil error means
// this member may bind this source with this writability.
func (p UserMountPolicy) ValidateUserMount(principal, src string, writable bool) error {
	real, err := validateUserSource(src, p.RootsFor(principal))
	if err != nil {
		return err
	}
	if !writable {
		return nil
	}
	return p.Writable().allows(src, real)
}

// UserWritableCeiling is the writable half of UserMountPolicy, carried on a
// member run's spec (SandboxSpec.UserWritable) so the bind-time gate decides
// writability again on the path it is about to bind.
type UserWritableCeiling struct {
	Roots []string // WARDYN_USER_WRITABLE_ROOTS
	Deny  []string // WARDYN_USER_WRITABLE_DENY
}

// Writable is p's writable ceiling.
func (p UserMountPolicy) Writable() UserWritableCeiling {
	return UserWritableCeiling{Roots: p.WritableRoots, Deny: p.WritableDeny}
}

// allows reports whether the ALREADY-RESOLVED real path of src may be bound
// read-write under c.
func (c UserWritableCeiling) allows(src, real string) error {
	if withinAnyRoot(real, c.Deny) {
		return fmt.Errorf("mount source %q may not be mounted writable: it is under a member writable-deny path (WARDYN_USER_WRITABLE_DENY)", src)
	}
	// The other direction: a writable bind of a directory covers everything
	// under it, the denied subtree included, and nothing downstream re-covers
	// that subtree read-only.
	if deny := denyPathUnder(real, c.Deny); deny != "" {
		return fmt.Errorf("mount source %q may not be mounted writable: it contains the member writable-deny path %q (WARDYN_USER_WRITABLE_DENY)", src, deny)
	}
	if len(c.Roots) == 0 {
		return fmt.Errorf("mount source %q may not be mounted writable: no member writable roots are configured (WARDYN_USER_WRITABLE_ROOTS) — member mounts are read-only by default", src)
	}
	if !withinAnyRoot(real, c.Roots) {
		return fmt.Errorf("mount source %q may not be mounted writable: it is outside every member writable root (WARDYN_USER_WRITABLE_ROOTS)", src)
	}
	return nil
}

// ValidateUserMountSource is the BIND-time gate, run against the roots already
// resolved for the run (SandboxSpec.UserMountRoots) as the last thing before
// the driver hands the bind to the container runtime. writable, non-nil for a
// read-write bind, is the run's writable ceiling (SandboxSpec.UserWritable): a
// source re-pointed since authoring is held to it on the path it resolves to
// now. Nil skips that half, as a spec from a control plane that sends no
// ceiling asks.
func ValidateUserMountSource(src string, roots []string, writable *UserWritableCeiling) error {
	real, err := validateUserSource(src, roots)
	if err != nil || writable == nil {
		return err
	}
	return writable.allows(src, real)
}

// validateUserSource runs the three additive checks and returns the resolved
// real path, which the writable check then matches against so the two can
// never disagree about WHICH path they are deciding about.
func validateUserSource(src string, roots []string) (string, error) {
	// The operator deny-list first, unchanged and unweakened.
	if err := ValidateMountSource(src); err != nil {
		return "", err
	}
	if len(roots) == 0 {
		return "", fmt.Errorf("mount source %q is refused: this deployment configures no member workspace roots (WARDYN_USER_WORKSPACE_ROOTS / _MAP), so a member may not mount a host directory", src)
	}
	// Fail CLOSED on any resolve error, "does not exist" included: unlike the
	// operator path, there is no lexical-only fallback here — an allowlist
	// that can't be evaluated is not an allowlist.
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return "", fmt.Errorf("mount source %q could not be resolved (member mounts must resolve on this host): %w", src, err)
	}
	if !withinAnyRoot(real, roots) {
		return "", fmt.Errorf("mount source %q resolves to %q, which is outside every member workspace root (%s)", src, real, strings.Join(roots, ", "))
	}
	if seg := DeniedUserSegment(real); seg != "" {
		return "", fmt.Errorf("mount source %q resolves to %q, which is or traverses the credential path %q; denied", src, real, seg)
	}
	return real, nil
}

// resolveEntry is a configured path made comparable with an already-resolved one: cleaned, with
// symlinks resolved through its longest existing prefix. An entry that does not exist yet keeps
// its remaining names, so one behind a symlinked parent still lands where a resolved spelling of
// that parent does, and not at its lexical text.
func resolveEntry(entry string) string {
	p := filepath.Clean(entry)
	var rest []string
	for {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{resolved}, rest...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Clean(entry)
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
}

// withinAnyRoot reports whether the ALREADY-RESOLVED real path is one of roots
// or nested under one. Each root is canonicalized the same way before the
// comparison (falling back to its cleaned lexical form when it does not
// resolve), so a root behind a symlinked parent — /home -> /mnt/home — still
// matches a resolved source under it.
func withinAnyRoot(real string, roots []string) bool {
	for _, root := range roots {
		r := resolveEntry(root)
		if real == r || strings.HasPrefix(real, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// denyPathUnder returns the first of deny that lies strictly under the
// ALREADY-RESOLVED real path, as configured, or "" when real contains none of
// them. It is withinAnyRoot read the other way, and canonicalizes each entry
// exactly as that does, so the two can never disagree about where an entry is.
func denyPathUnder(real string, deny []string) string {
	prefix := strings.TrimSuffix(real, string(filepath.Separator)) + string(filepath.Separator)
	for _, entry := range deny {
		d := resolveEntry(entry)
		if strings.HasPrefix(d, prefix) {
			return entry
		}
	}
	return ""
}

// DeniedUserSegment returns the offending path element when real IS or
// TRAVERSES a credential dotfile path, or "" when it is clean. Elements compare
// case-folded: on a case-insensitive filesystem EvalSymlinks keeps the
// spelling it was given, so ".SSH" there is ~/.ssh.
func DeniedUserSegment(real string) string {
	parts := strings.Split(strings.TrimPrefix(filepath.ToSlash(real), "/"), "/")
	for i, part := range parts {
		for seg := range memberDeniedSegments {
			if strings.EqualFold(part, seg) {
				return part
			}
		}
		if i+1 < len(parts) {
			for _, pair := range memberDeniedPairs {
				if strings.EqualFold(part, pair[0]) && strings.EqualFold(parts[i+1], pair[1]) {
					return pair[0] + "/" + pair[1]
				}
			}
		}
	}
	return ""
}

// ParseUserMountPolicy builds the policy from its four raw operator/MDM env
// values (see UserMountPolicy for what each one means). roots, writable and
// writableDeny are comma-separated absolute paths; rootsMap is JSON
// {"<principal>": ["/abs/root", ...]}.
//
// FAIL CLOSED on a malformed value — a typo'd root that silently matched
// nothing (or, worse, prefix-matched something else) is exactly the config
// bug this ceiling cannot afford — so boot stops rather than starting with a
// half-understood allowlist.
//
// warnings are the posture notices the caller logs at boot: a root of "/" or
// of $HOME is permitted but leaves the dotfile-deny residual unbounded, so it
// must never be silent.
func ParseUserMountPolicy(roots, rootsMap, writable, writableDeny string) (p UserMountPolicy, warnings []string, err error) {
	if p.Roots, err = parseRootList("WARDYN_USER_WORKSPACE_ROOTS", roots); err != nil {
		return UserMountPolicy{}, nil, err
	}
	if p.WritableRoots, err = parseRootList("WARDYN_USER_WRITABLE_ROOTS", writable); err != nil {
		return UserMountPolicy{}, nil, err
	}
	if p.WritableDeny, err = parseRootList("WARDYN_USER_WRITABLE_DENY", writableDeny); err != nil {
		return UserMountPolicy{}, nil, err
	}
	if strings.TrimSpace(rootsMap) != "" {
		var raw map[string][]string
		if uerr := json.Unmarshal([]byte(rootsMap), &raw); uerr != nil {
			return UserMountPolicy{}, nil, fmt.Errorf("WARDYN_USER_WORKSPACE_ROOTS_MAP: %w (want {\"<principal>\": [\"/abs/root\", ...]})", uerr)
		}
		p.RootsByPrincipal = make(map[string][]string, len(raw))
		for principal, list := range raw {
			key := strings.ToLower(strings.TrimSpace(principal))
			if key == "" {
				return UserMountPolicy{}, nil, fmt.Errorf("WARDYN_USER_WORKSPACE_ROOTS_MAP: empty principal key")
			}
			parsed, perr := parseRootList("WARDYN_USER_WORKSPACE_ROOTS_MAP["+key+"]", strings.Join(list, ","))
			if perr != nil {
				return UserMountPolicy{}, nil, perr
			}
			// An entry REPLACES the shared list, so an empty one is "this member
			// mounts nothing" — a meaningful, deliberately-expressible posture.
			p.RootsByPrincipal[key] = parsed
		}
	}
	return p, p.bootWarnings(), nil
}

// parseRootList splits and validates one comma-separated root list. Each
// entry must be an absolute, already-cleaned path — the same shape
// ValidateMountSource demands, so a root can never carry a traversal segment
// the prefix test would then honor.
func parseRootList(name, raw string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !filepath.IsAbs(part) || filepath.Clean(part) != part {
			return nil, fmt.Errorf("%s: %q must be an absolute, cleaned path", name, part)
		}
		out = append(out, part)
	}
	return out, nil
}

// bootWarnings returns notices for the two roots that are allowed (WARN, not
// refuse) but don't bound what an operator setting them expects. They're
// warned about for OPPOSITE reasons and get OPPOSITE sentences — a merged
// sentence would send an operator hunting the wrong failure:
//
//   - $HOME is far too WIDE: withinAnyRoot really does admit everything under
//     it, so the credential dotfile deny-list is the only thing left between
//     a member and the operator's credentials.
//   - "/" is DEAD: withinAnyRoot compares real == root || HasPrefix(real,
//     root+"/"), which for "/" is the prefix "//" — so a root of "/" matches
//     nothing but the literal path "/" and REFUSES every member mount under
//     it. Saying it is bounded only by the deny-list states the opposite of
//     what the code does.
//
// internal/runner/member_mount_test.go drives both against withinAnyRoot so
// the wording cannot drift from the behaviour.
func (p UserMountPolicy) bootWarnings() []string {
	home := filepath.Clean(strings.TrimSpace(os.Getenv("HOME")))
	seen := map[string]bool{}
	var out []string
	check := func(name string, roots []string) {
		for _, r := range roots {
			var msg string
			switch {
			case r == "/":
				msg = fmt.Sprintf("%s contains %q, which matches NOTHING: a root of \"/\" bounds only the literal path \"/\", so every member mount under it is refused rather than allowed; point it at a dedicated projects directory instead", name, r)
			case home != "" && home != "." && r == home:
				msg = fmt.Sprintf("%s contains %q, this daemon's own home directory — a member's mounts are then bounded only by the credential dotfile deny-list; point it at a dedicated projects directory instead", name, r)
			default:
				continue
			}
			if !seen[msg] {
				seen[msg] = true
				out = append(out, msg)
			}
		}
	}
	check("WARDYN_USER_WORKSPACE_ROOTS", p.Roots)
	check("WARDYN_USER_WRITABLE_ROOTS", p.WritableRoots)
	for _, principal := range slices.Sorted(maps.Keys(p.RootsByPrincipal)) {
		check("WARDYN_USER_WORKSPACE_ROOTS_MAP["+principal+"]", p.RootsByPrincipal[principal])
	}
	return out
}
