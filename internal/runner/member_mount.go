// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	".codex":           true, // the Codex harness login
	".azure":           true, // Azure CLI token cache
	".gemini":          true, // another agent CLI's login
	".npmrc":           true, // registry auth tokens
	".pypirc":          true,
}

// memberDeniedPairs are two-segment denials — a bare ".config", ".git" or
// "Library" is an ordinary directory a project legitimately contains, but these
// children hold credentials (gh's token, gcloud's, the macOS keychains) and
// remote/credential config.
var memberDeniedPairs = [][2]string{
	{".config", "gh"},
	{".config", "gcloud"}, // gcloud's credential database
	{".git", "config"},
	{"Library", "Keychains"}, // macOS keychains
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

// ScanHolder is a member whose workspace attaches the folder a scan reads, and that workspace's name.
type ScanHolder struct{ Owner, Workspace string }

// OpenScanSource opens src for a folder scan on this host. What the scan finds lands on the folder's
// one source row, which every workspace attaching it shows, so the folder is held, whoever scans, to:
//
//   - every member workspace root that holds the folder under the name memberLocation gives it: a
//     member can replace any folder in their root with a link, and a person's own root often lies
//     inside the shared one, so it must resolve inside each of them, not any one;
//   - owner's own roots, at the door of a workspace a member owns ("" is an operator's door), with
//     ValidateUserMount's refusals, checked first: a folder the member's own roots refuse tells them
//     nothing of another person's workspace;
//   - each holder's roots: one of them must still hold the folder, or the scan is refused with a
//     StaleWorkspaceError naming that workspace, since its owner could not onboard the folder today.
//
// The resolved folder is then opened with open(real). The caller passes foldersync.OpenFromTop, which
// opens it one folder at a time from the top of its volume with no link followed (this package cannot
// import foldersync: foldersync's tests import this one), and reads only through the handle, so a
// folder swapped for a link after the check is never read through. An open that fails is the member
// refusal, its cause in the operator's log. At an operator's door a folder no root holds and no member
// workspace attaches gets no handle and no error, and the caller reads it by its path.
// One the walk cannot follow (a link loop, more than memberWalkLinks links, or a name on its way
// that cannot be read) while a member workspace attaches it is refused for that, never as that
// person's narrowed roots.
func (p UserMountPolicy) OpenScanSource(owner, src string, holders []ScanHolder, open func(real string) (*os.Root, error)) (*os.Root, error) {
	at, held, gaveUp := p.memberLocation(src)
	roots := held // the roots a refusal names
	var real string
	var err error
	if owner != "" {
		roots = p.RootsFor(owner)
		if real, err = validateUserSource(src, roots); err != nil {
			return nil, err
		}
	} else if len(held) == 0 && len(holders) == 0 {
		return nil, nil
	} else if gaveUp != nil {
		return nil, walkGaveUp(src, gaveUp)
	}
	for _, h := range holders {
		own := p.RootsFor(h.Owner)
		if h.Owner != owner && !slices.ContainsFunc(own, func(r string) bool { return slices.Contains(held, r) }) {
			return nil, &StaleWorkspaceError{Source: src, Holder: h, Roots: own}
		}
	}
	if owner == "" {
		if real, err = validateUserSource(at, roots); err != nil {
			return nil, err
		}
	}
	for _, root := range held {
		if !withinAnyRoot(real, []string{root}) {
			return nil, memberSourceRefused(src, real, roots, fmt.Errorf("resolves outside the member workspace root %q that holds it", root))
		}
	}
	userSourceChecked()
	dir, err := open(real)
	if err != nil {
		return nil, memberSourceRefused(src, real, roots, err)
	}
	return dir, nil
}

// userSourceChecked runs in OpenScanSource between the check and the open; tests swap the source there.
var userSourceChecked = func() {}

// StaleWorkspaceError refuses a folder scan because a member workspace attaches the folder while none
// of its owner's roots holds it any more (narrowed or removed since onboarding). The scan would land
// on the row that person reads, so every scan of the folder waits until it is taken out of that
// workspace or the roots cover it again. Error names the workspace and its owner for an operator;
// Unnamed is the same refusal for a member, who may not learn another person's workspace.
type StaleWorkspaceError struct {
	Source string
	Holder ScanHolder
	Roots  []string // the owner's roots now; empty when none is set
}

func (e *StaleWorkspaceError) Error() string {
	roots := "none set"
	if len(e.Roots) > 0 {
		roots = strings.Join(e.Roots, ", ")
	}
	return fmt.Sprintf("%s: the folder %q is attached to the workspace %q of %s, and none of that person's member workspace roots (%s) holds it now; "+
		"remove the folder from that workspace, or set that person's roots to cover it again (WARDYN_USER_WORKSPACE_ROOTS / _MAP), then scan again",
		errMemberSourceRefused, e.Source, e.Holder.Workspace, e.Holder.Owner, roots)
}

// Unnamed is the refusal without the workspace, its owner or their roots.
func (e *StaleWorkspaceError) Unnamed() string {
	return fmt.Sprintf("%s: the folder %q is attached to a workspace whose owner's member workspace roots no longer hold it, so no scan of it can run; "+
		"an operator can see which workspace in the audit log", errMemberSourceRefused, e.Source)
}

func (e *StaleWorkspaceError) Unwrap() error { return errMemberSourceRefused }

// walkGaveUp refuses a folder scan at an operator's door when memberLocation could not follow src
// while a member workspace attaches it: no root can be said to hold the folder, and that person's
// roots are not the cause. The cause goes to the operator's log; the refusal names no workspace.
func walkGaveUp(src string, cause error) error {
	slog.Warn("runner: member mount source refused", slog.String("source", src), slog.Any("err", cause))
	return errMemberSourceRefused
}

// memberWalkLinks is how many links memberLocation follows before it gives up: filepath.EvalSymlinks'
// own limit, so a folder the walk gives up on is one EvalSymlinks refuses too. Never lower it: a name
// EvalSymlinks resolves would then lose the root that holds it.
const memberWalkLinks = 255

// memberLocation is where src lies among the member workspace roots: at is the name to check it by
// and held every root that holds at by its text. A src whose text a root holds is itself. Any other
// is walked one name at a time from the top, following each link on the way as EvalSymlinks does,
// up to the first folder that is a root, compared by identity (os.SameFile), so another name of the
// root (a link to it or above it, another letter case on a volume that ignores case) is that root;
// at is then src named from that root as configured. The walk stops at the first root and never
// looks below it, where a member can make any folder a link; a bind or second mount of a folder
// below a root is not that root, and the walk goes on through it. held is empty when the walk meets
// no root or cannot go on; gaveUp is set only in the second case, with why: a name on the way that
// cannot be read, or more than memberWalkLinks links (a loop among them).
func (p UserMountPolicy) memberLocation(src string) (at string, held []string, gaveUp error) {
	all := p.allRoots()
	holding := func(at string) []string {
		var out []string
		for _, root := range all {
			if rootHolds(root, at) {
				out = append(out, root)
			}
		}
		return out
	}
	if held := holding(src); len(held) > 0 {
		return src, held, nil
	}
	type rootDir struct {
		name string
		fi   os.FileInfo
	}
	var dirs []rootDir
	for _, root := range all {
		if fi, err := os.Stat(root); err == nil && root != string(filepath.Separator) {
			dirs = append(dirs, rootDir{root, fi})
		}
	}
	if len(dirs) == 0 {
		return src, nil, nil
	}
	cur, rest := string(filepath.Separator), strings.Split(src, "/") // a stored folder is "/"-rooted and clean
	for links := 0; len(rest) > 0; {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		fi, err := os.Lstat(next)
		if err != nil {
			return src, nil, err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			for _, d := range dirs {
				if os.SameFile(fi, d.fi) {
					// A name left that is not plain (a link's own "..") leaves at unclean,
					// which validateUserSource refuses: nothing below a root is walked.
					at := strings.Join(append([]string{d.name}, slices.DeleteFunc(rest, func(n string) bool { return n == "" || n == "." })...), string(filepath.Separator))
					return at, holding(at), nil
				}
			}
			cur = next
			continue
		}
		target, err := os.Readlink(next)
		if err != nil {
			return src, nil, err
		}
		if links++; links > memberWalkLinks {
			return src, nil, fmt.Errorf("more than %d links on the way to %s", memberWalkLinks, src)
		}
		if filepath.IsAbs(target) {
			cur = filepath.VolumeName(target) + string(filepath.Separator)
			target = target[len(filepath.VolumeName(target)):]
		}
		rest = append(strings.Split(filepath.ToSlash(target), "/"), rest...)
	}
	return src, nil, nil
}

// rootHolds reports whether src, by its text alone, is root or lies under it,
// with root taken as configured or as it resolves. A root of "/" holds nothing
// but itself, as in withinAnyRoot: it admits no member folder, so it makes no
// operator folder a member's.
func rootHolds(root, src string) bool {
	return withinAnyCanon(src, []string{root}, filepath.Clean) || withinAnyRoot(src, []string{root})
}

// allRoots is every member root the policy sets, the shared list and each
// person's own, sorted and without repeats.
func (p UserMountPolicy) allRoots() []string {
	roots := slices.Clone(p.Roots)
	for _, own := range p.RootsByPrincipal {
		roots = append(roots, own...)
	}
	slices.Sort(roots)
	return slices.Compact(roots)
}

// ValidateUserMountLexical is the AUTHORING-time gate for a folder bound to a
// person's runner: the folder lives on that machine, so nothing on this host is
// read. Everything the path's text can show is still refused: the operator
// deny-list's string half, a path outside every root of principal's, a
// credential path, and, writable, a path outside the writable roots or over a
// writable-deny entry in any letter case. The runner checks the folder itself
// at bind (ValidateUserMountSource with its own roots).
func (p UserMountPolicy) ValidateUserMountLexical(principal, src string, writable bool) error {
	if err := ValidateMountSourceLexical(src); err != nil {
		return err
	}
	roots := p.RootsFor(principal)
	if len(roots) == 0 {
		return fmt.Errorf("mount source %q is refused: this deployment configures no member workspace roots (WARDYN_USER_WORKSPACE_ROOTS / _MAP), so a member may not mount a host directory", src)
	}
	if !withinAnyCanon(src, roots, filepath.Clean) {
		return fmt.Errorf("%w: mount source %q is outside every member workspace root (%s); an operator sets these roots with WARDYN_USER_WORKSPACE_ROOTS", errMemberSourceRefused, src, strings.Join(roots, ", "))
	}
	if seg := DeniedUserSegment(src); seg != "" {
		return fmt.Errorf("mount source %q is or traverses the credential path %q; denied", src, seg)
	}
	if !writable {
		return nil
	}
	return p.Writable().allowsAt(src, src, filepath.Clean)
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
	return c.allowsAt(src, real, resolveEntry)
}

// allowsAt is allows with each configured entry made comparable by canon:
// resolveEntry on the host that binds, filepath.Clean where only the text is
// known. The deny half compares names case-folded: on a Mac /r/APP and /r/app
// are one folder, and EvalSymlinks keeps the spelling it was given, so a deny
// holds whatever the spelling. It only ever denies more.
func (c UserWritableCeiling) allowsAt(src, real string, canon func(string) string) error {
	for _, entry := range c.Deny {
		if foldWithin(canon(entry), real) {
			return fmt.Errorf("mount source %q may not be mounted writable: it is under a member writable-deny path (WARDYN_USER_WRITABLE_DENY)", src)
		}
	}
	// The other direction: a writable bind of a directory covers everything
	// under it, the denied subtree included, and nothing downstream re-covers
	// that subtree read-only.
	for _, entry := range c.Deny {
		if foldWithin(real, canon(entry)) {
			return fmt.Errorf("mount source %q may not be mounted writable: it contains the member writable-deny path %q (WARDYN_USER_WRITABLE_DENY)", src, entry)
		}
	}
	if len(c.Roots) == 0 {
		return fmt.Errorf("mount source %q may not be mounted writable: no member writable roots are configured (WARDYN_USER_WRITABLE_ROOTS) — member mounts are read-only by default", src)
	}
	if !withinAnyCanon(real, c.Roots, canon) {
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
//
// ownRoots, non-nil on a runner, are the roots its owner shares (--drive-root):
// the resolved path must lie inside one of them as well, and none refuses. Nil
// is wardynd's host, where the member's roots alone hold the bind.
func ValidateUserMountSource(src string, roots []string, writable *UserWritableCeiling, ownRoots []string) error {
	real, err := validateUserSource(src, roots)
	if err != nil {
		return err
	}
	if err := withinOwnRoots(src, real, ownRoots); err != nil {
		return err
	}
	if writable == nil {
		return nil
	}
	return writable.allows(src, real)
}

// withinOwnRoots holds the ALREADY-RESOLVED real path of src to a runner's own
// roots; nil roots (wardynd's host) hold nothing.
func withinOwnRoots(src, real string, ownRoots []string) error {
	switch {
	case ownRoots == nil:
		return nil
	case len(ownRoots) == 0:
		return fmt.Errorf("%w: mount source %q is refused: this runner shares no folder (start it with a --drive-root that covers it)", errMemberSourceRefused, src)
	case !withinAnyRoot(real, ownRoots):
		slog.Warn("runner: member mount source outside the runner's --drive-root", slog.String("source", src), slog.String("resolved", real))
		return fmt.Errorf("%w: mount source %q must resolve inside a folder this runner shares (--drive-root: %s)", errMemberSourceRefused, src, strings.Join(ownRoots, ", "))
	}
	return nil
}

// validateUserSource runs the three additive checks and returns the resolved
// real path, which the writable check then matches against so the two can
// never disagree about WHICH path they are deciding about.
//
// Nothing about the filesystem reaches the caller: a source outside every root
// is refused on its text before anything is read, and every refusal after a
// read (a path that does not resolve, one that resolves out of every root, a
// symlink onto the deny-list) is the one errMemberSourceRefused sentence, so
// a member cannot learn whether a path exists or where a symlink points. The
// resolved path and the error go to the operator's log.
func validateUserSource(src string, roots []string) (string, error) {
	// The operator deny-list's string half first, unchanged and unweakened.
	if err := ValidateMountSourceLexical(src); err != nil {
		return "", err
	}
	if len(roots) == 0 {
		return "", fmt.Errorf("mount source %q is refused: this deployment configures no member workspace roots (WARDYN_USER_WORKSPACE_ROOTS / _MAP), so a member may not mount a host directory", src)
	}
	refused := func(real string, cause error) (string, error) {
		return "", memberSourceRefused(src, real, roots, cause)
	}
	if !withinAnyRootLexical(src, roots) {
		return "", fmt.Errorf("%w: mount source %q is outside every member workspace root (%s); an operator sets these roots with WARDYN_USER_WORKSPACE_ROOTS", errMemberSourceRefused, src, strings.Join(roots, ", "))
	}
	if err := denySourceSymlink(src); err != nil {
		return refused("", err)
	}
	// Fail CLOSED on any resolve error, "does not exist" included: unlike the
	// operator path, there is no lexical-only fallback here — an allowlist
	// that can't be evaluated is not an allowlist.
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return refused("", err)
	}
	if !withinAnyRoot(real, roots) {
		return refused(real, errors.New("resolves outside every member workspace root"))
	}
	if seg := DeniedUserSegment(real); seg != "" {
		return "", fmt.Errorf("mount source %q is or traverses the credential path %q; denied", src, seg)
	}
	return real, nil
}

// memberSourceRefused logs cause for the operator and returns the refusal the
// member reads, which says nothing about what the filesystem held.
func memberSourceRefused(src, real string, roots []string, cause error) error {
	slog.Warn("runner: member mount source refused", slog.String("source", src), slog.String("resolved", real), slog.Any("err", cause))
	return fmt.Errorf("%w: mount source %q must resolve on this host to a directory inside your member workspace roots (%s); an operator sets these roots with WARDYN_USER_WORKSPACE_ROOTS", errMemberSourceRefused, src, strings.Join(roots, ", "))
}

// errMemberSourceRefused is the one refusal a member reads for a source that
// is not inside their roots, whatever the filesystem said about it.
var errMemberSourceRefused = errors.New("member mount source refused")

// withinAnyRootLexical reports whether src, by its text alone, is one of roots
// or nested under one, each root taken as configured or as it resolves, so a
// source spelled through a symlinked root's real path still passes to the
// resolved check.
func withinAnyRootLexical(src string, roots []string) bool {
	for _, root := range roots {
		for _, r := range []string{filepath.Clean(root), resolveEntry(root)} {
			if src == r || strings.HasPrefix(src, strings.TrimSuffix(r, string(filepath.Separator))+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
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
	return withinAnyCanon(real, roots, resolveEntry)
}

// withinAnyCanon is withinAnyRoot with each root made comparable by canon.
func withinAnyCanon(real string, roots []string, canon func(string) string) bool {
	for _, root := range roots {
		r := canon(root)
		if real == r || strings.HasPrefix(real, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
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
