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
// Everything in mount.go assumes the mount source was authored by an operator:
// a policy write or an inline policy from an admin / SSO-gated human operator.
// A member-owned workspace (types.Workspace.OwnedBy) breaks that assumption on
// purpose — the desktop developer onboards their own project directory without
// an admin authoring the mount — so a member source runs the operator
// deny-list (ValidateMountSource) AND, additively, everything below:
//
//  1. ROOT ALLOWLIST. The source's canonicalized real path must sit inside one
//     of the operator/MDM-set roots (WARDYN_MEMBER_WORKSPACE_ROOTS, or that
//     member's own entry in WARDYN_MEMBER_WORKSPACE_ROOTS_MAP, which REPLACES
//     the shared list rather than adding to it). No roots configured => a
//     member local_dir mount is refused outright (fail closed); repos and
//     operator-owned workspaces still work.
//  2. CANONICALIZE, THEN MATCH. The within-root test runs on the EvalSymlinks
//     result, so a symlink INSIDE a root pointing OUT of every root is refused —
//     the escape a lexical prefix check misses. Unlike ValidateMountSource
//     (which falls through to lexical-only when the path does not resolve, for
//     the remote-daemon case), an unresolvable member source is REFUSED: the
//     allowlist is the whole guarantee and cannot be asserted about a path this
//     process cannot see.
//  3. DOTFILE DENY. The real path may neither BE nor TRAVERSE a credential
//     dotfile dir (~/.ssh, ~/.aws, ~/.claude, ...). This is the compose file's
//     own warning ("DO NOT set it to your home directory") turned into code,
//     and it is belt-and-braces with (1): an operator who does point a root at
//     $HOME still cannot let a member mount their own ~/.ssh.
//
// WRITABLE is a separate, narrower allowlist (WARDYN_MEMBER_WRITABLE_ROOTS,
// minus WARDYN_MEMBER_WRITABLE_DENY, deny winning) because a writable bind
// widens the residual; both unset means NO writable member mounts at all.
// Operators keep the unrestricted per-source Writable opt-in they have today —
// none of this narrows an operator mount, since a run with no member-owned
// workspace carries no roots and takes exactly today's path.
//
// RESIDUAL, stated honestly: the bind-time re-check (docker driver's
// agentMounts, via SandboxSpec.MemberMountRoots) resolves the real path as late
// as this process can, but validate and ContainerCreate are still not atomic —
// the same TOCTOU window ValidateMountSource's own residual note already
// documents for operator mounts. Two things bound it: the roots are operator-set, so a member can only
// race WITHIN the declared roots, never enlarge them; and the dotfile deny-list
// matches the post-EvalSymlinks real path, so a won race that lands on ~/.ssh is
// still refused.

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

// MemberMountPolicy is the operator/MDM-set posture for MEMBER-authored
// local_dir binds. The zero value means "no member local_dir mounts at all",
// which is the fail-closed default: a deployment that configures none of these
// vars behaves exactly as it does today, minus the ability for a member to
// onboard a host directory.
//
// Every field comes from operator/MDM-set ENV (cmd/wardynd), never SiteConfig —
// site config is a full-replace row a single bad PUT can blank (the 0042
// hazard), and this is a security ceiling.
type MemberMountPolicy struct {
	// Roots is the shared allowlist (WARDYN_MEMBER_WORKSPACE_ROOTS): absolute,
	// cleaned host prefixes any member's local_dir source must resolve into.
	Roots []string
	// RootsByPrincipal (WARDYN_MEMBER_WORKSPACE_ROOTS_MAP) is the per-member
	// override. A principal with an entry uses ONLY that entry — per-member
	// REPLACES shared, because per-member exists to be the more restrictive
	// control, and a union would make adding a row widen rather than narrow.
	// Keys are lowercased principals (OIDC sub or email, the same dual-key
	// identity a capability_grants `user` subject carries).
	RootsByPrincipal map[string][]string
	// WritableRoots (WARDYN_MEMBER_WRITABLE_ROOTS) is where a member may mark
	// their own mount writable. Empty = nowhere.
	WritableRoots []string
	// WritableDeny (WARDYN_MEMBER_WRITABLE_DENY) carves holes in WritableRoots.
	// Deny WINS over allow, and is checked first.
	WritableDeny []string
}

// Configured reports whether any member root is set at all — i.e. whether
// member local_dir onboarding is available on this deployment.
func (p MemberMountPolicy) Configured() bool {
	return len(p.Roots) > 0 || len(p.RootsByPrincipal) > 0
}

// RootsFor returns the roots that bound one member's mounts: their own map
// entry when they have one (REPLACING the shared list), otherwise the shared
// list. An empty result means this member may not mount a host dir at all.
func (p MemberMountPolicy) RootsFor(principal string) []string {
	if own, ok := p.RootsByPrincipal[strings.ToLower(strings.TrimSpace(principal))]; ok {
		return own
	}
	return p.Roots
}

// ValidateMemberMount is the AUTHORING-time gate (workspace onboarding and
// run-create): the full source check for principal, plus the writable
// allowlist when the source asks to be mounted read-write. A nil error means
// this member may bind this source with this writability.
func (p MemberMountPolicy) ValidateMemberMount(principal, src string, writable bool) error {
	real, err := validateMemberSource(src, p.RootsFor(principal))
	if err != nil {
		return err
	}
	if !writable {
		return nil
	}
	if withinAnyRoot(real, p.WritableDeny) {
		return fmt.Errorf("mount source %q may not be mounted writable: it is under a member writable-deny path (WARDYN_MEMBER_WRITABLE_DENY)", src)
	}
	if len(p.WritableRoots) == 0 {
		return fmt.Errorf("mount source %q may not be mounted writable: no member writable roots are configured (WARDYN_MEMBER_WRITABLE_ROOTS) — member mounts are read-only by default", src)
	}
	if !withinAnyRoot(real, p.WritableRoots) {
		return fmt.Errorf("mount source %q may not be mounted writable: it is outside every member writable root (WARDYN_MEMBER_WRITABLE_ROOTS)", src)
	}
	return nil
}

// ValidateMemberMountSource is the BIND-time gate: the source half only, run
// against the roots already resolved for the run (SandboxSpec.MemberMountRoots)
// as the last thing before the driver hands the bind to the container runtime.
// Writability is not re-derived here — it was decided at authoring time and
// rides on the mount itself, so a path swap cannot change it.
func ValidateMemberMountSource(src string, roots []string) error {
	_, err := validateMemberSource(src, roots)
	return err
}

// validateMemberSource runs the three additive checks and returns the resolved
// real path (which the writable check then matches against, so the two can
// never disagree about WHICH path they are deciding about).
func validateMemberSource(src string, roots []string) (string, error) {
	// The operator deny-list first, unchanged and unweakened: absolute, cleaned
	// (so lexical ".." is already refused), not host-root/proc/etc/docker.sock.
	if err := ValidateMountSource(src); err != nil {
		return "", err
	}
	if len(roots) == 0 {
		return "", fmt.Errorf("mount source %q is refused: this deployment configures no member workspace roots (WARDYN_MEMBER_WORKSPACE_ROOTS / _MAP), so a member may not mount a host directory", src)
	}
	// Fail CLOSED on any resolve error, "does not exist" included: unlike the
	// operator path, there is no lexical-only fallback here — an allowlist we
	// cannot evaluate is not an allowlist.
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return "", fmt.Errorf("mount source %q could not be resolved (member mounts must resolve on this host): %w", src, err)
	}
	if !withinAnyRoot(real, roots) {
		return "", fmt.Errorf("mount source %q resolves to %q, which is outside every member workspace root (%s)", src, real, strings.Join(roots, ", "))
	}
	if seg := deniedMemberSegment(real); seg != "" {
		return "", fmt.Errorf("mount source %q resolves to %q, which is or traverses the credential path %q; denied", src, real, seg)
	}
	return real, nil
}

// withinAnyRoot reports whether the ALREADY-RESOLVED real path is one of roots
// or nested under one. Each root is canonicalized the same way before the
// comparison (falling back to its cleaned lexical form when it does not
// resolve), so a root behind a symlinked parent — /home -> /mnt/home — still
// matches a resolved source under it.
func withinAnyRoot(real string, roots []string) bool {
	for _, root := range roots {
		r := filepath.Clean(root)
		if resolved, err := filepath.EvalSymlinks(r); err == nil {
			r = resolved
		}
		if real == r || strings.HasPrefix(real, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// deniedMemberSegment returns the offending path element when real IS or
// TRAVERSES a credential dotfile path, or "" when it is clean.
func deniedMemberSegment(real string) string {
	parts := strings.Split(strings.TrimPrefix(filepath.ToSlash(real), "/"), "/")
	for i, part := range parts {
		if memberDeniedSegments[part] {
			return part
		}
		if i+1 < len(parts) {
			for _, pair := range memberDeniedPairs {
				if part == pair[0] && parts[i+1] == pair[1] {
					return pair[0] + "/" + pair[1]
				}
			}
		}
	}
	return ""
}

// ParseMemberMountPolicy builds the policy from its four raw operator/MDM env
// values (see MemberMountPolicy for what each one means). roots, writable and
// writableDeny are comma-separated absolute paths; rootsMap is JSON
// {"<principal>": ["/abs/root", ...]}.
//
// FAIL CLOSED on a malformed value — a typo'd root that silently matched
// nothing (or, worse, prefix-matched something else) is exactly the config bug
// this ceiling cannot afford — so boot stops rather than starting with a
// half-understood allowlist.
//
// warnings are the O4 posture notices the caller logs at boot: a root of "/" or
// of $HOME is permitted (matching the LocalMode unspecified-bind WARN precedent
// documented on humanOrAdminAuth) but leaves the section-(c) residual
// unbounded, so it must never be silent.
func ParseMemberMountPolicy(roots, rootsMap, writable, writableDeny string) (p MemberMountPolicy, warnings []string, err error) {
	if p.Roots, err = parseRootList("WARDYN_MEMBER_WORKSPACE_ROOTS", roots); err != nil {
		return MemberMountPolicy{}, nil, err
	}
	if p.WritableRoots, err = parseRootList("WARDYN_MEMBER_WRITABLE_ROOTS", writable); err != nil {
		return MemberMountPolicy{}, nil, err
	}
	if p.WritableDeny, err = parseRootList("WARDYN_MEMBER_WRITABLE_DENY", writableDeny); err != nil {
		return MemberMountPolicy{}, nil, err
	}
	if strings.TrimSpace(rootsMap) != "" {
		var raw map[string][]string
		if uerr := json.Unmarshal([]byte(rootsMap), &raw); uerr != nil {
			return MemberMountPolicy{}, nil, fmt.Errorf("WARDYN_MEMBER_WORKSPACE_ROOTS_MAP: %w (want {\"<principal>\": [\"/abs/root\", ...]})", uerr)
		}
		p.RootsByPrincipal = make(map[string][]string, len(raw))
		for principal, list := range raw {
			key := strings.ToLower(strings.TrimSpace(principal))
			if key == "" {
				return MemberMountPolicy{}, nil, fmt.Errorf("WARDYN_MEMBER_WORKSPACE_ROOTS_MAP: empty principal key")
			}
			parsed, perr := parseRootList("WARDYN_MEMBER_WORKSPACE_ROOTS_MAP["+key+"]", strings.Join(list, ","))
			if perr != nil {
				return MemberMountPolicy{}, nil, perr
			}
			// An entry REPLACES the shared list, so an empty one is "this member
			// mounts nothing" — a meaningful, deliberately-expressible posture.
			p.RootsByPrincipal[key] = parsed
		}
	}
	return p, p.bootWarnings(), nil
}

// parseRootList splits and validates one comma-separated root list. Each entry
// must be an absolute, already-cleaned path — the same shape ValidateMountSource
// demands of a mount source, so a root can never carry a traversal segment that
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

// bootWarnings returns the O4 notices for the two roots that are allowed (WARN,
// not refuse) but do not bound what an operator setting them expects. They are
// warned about for OPPOSITE reasons and so get OPPOSITE sentences — the same
// split ParseUserDriveHostRoots keeps, and for the same reason (a merged
// sentence sends an operator hunting the wrong failure):
//
//   - $HOME is far too WIDE: withinAnyRoot really does admit everything under
//     it, so the credential dotfile deny-list is the only thing left between a
//     member and the operator's credentials.
//   - "/" is DEAD: withinAnyRoot compares real == root || HasPrefix(real,
//     root+"/"), which for "/" is the prefix "//" — so a root of "/" matches
//     nothing but the literal path "/" and REFUSES every member mount under it.
//     Saying it is bounded only by the deny-list states the opposite of what
//     the code does.
//
// Fail-closed is the right behaviour for "/" and stays; only the sentence was
// wrong. internal/runner/member_mount_test.go drives both against withinAnyRoot
// so the wording cannot drift from the behaviour again.
func (p MemberMountPolicy) bootWarnings() []string {
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
	check("WARDYN_MEMBER_WORKSPACE_ROOTS", p.Roots)
	check("WARDYN_MEMBER_WRITABLE_ROOTS", p.WritableRoots)
	for _, principal := range slices.Sorted(maps.Keys(p.RootsByPrincipal)) {
		check("WARDYN_MEMBER_WORKSPACE_ROOTS_MAP["+principal+"]", p.RootsByPrincipal[principal])
	}
	return out
}
