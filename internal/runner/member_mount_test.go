// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The adversarial matrix for the MEMBER-safe mount gate
// (docs/design/member-role-desktop.md §c). Every case here is a path a member
// could plausibly supply — or repoint under the daemon — that must NOT reach a
// bind. Unit tests only: no docker, no daemon, matching mount_test.go's shape.

// memberRoot builds a real projects root under t.TempDir() with one project dir
// in it, and returns (root, projectDir). Real directories, not fixtures: the
// whole gate turns on EvalSymlinks, which needs paths that actually exist.
func memberRoot(t *testing.T) (string, string) {
	t.Helper()
	// The tempdir itself is symlinked on macOS (/var -> /private/var); resolve it
	// so the test asserts about the gate, not about the platform.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve tempdir: %v", err)
	}
	root := filepath.Join(base, "projects")
	project := filepath.Join(root, "app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return root, project
}

// TestMemberMount_AllowsProjectUnderRoot is the positive control: without it a
// gate that refuses everything would pass every case below.
func TestMemberMount_AllowsProjectUnderRoot(t *testing.T) {
	root, project := memberRoot(t)
	p := MemberMountPolicy{Roots: []string{root}}
	if err := p.ValidateMemberMount("alice", project, false); err != nil {
		t.Fatalf("read-only project dir under the root = %v, want allowed", err)
	}
	if err := p.ValidateMemberMount("alice", root, false); err != nil {
		t.Fatalf("the root itself = %v, want allowed", err)
	}
}

// TestMemberMount_SymlinkEscape is matrix row 1: a source INSIDE a root that is
// a symlink pointing OUT of every root. A lexical prefix check passes it; the
// canonicalized check must not. This is the case the whole design exists for.
func TestMemberMount_SymlinkEscape(t *testing.T) {
	root, _ := memberRoot(t)
	outside := filepath.Join(filepath.Dir(root), "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "sneaky")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	p := MemberMountPolicy{Roots: []string{root}}
	err := p.ValidateMemberMount("alice", link, false)
	if err == nil {
		t.Fatal("a symlink inside the root pointing outside every root was ALLOWED; the within-root test must run on the EvalSymlinks result, not the lexical path")
	}
	if !strings.Contains(err.Error(), "outside every member workspace root") {
		t.Errorf("error = %v, want the outside-every-root refusal", err)
	}
}

// TestMemberMount_SymlinkToDeniedPrefix pins the belt-and-braces half: a
// symlink inside a root aimed at /etc is caught by the OPERATOR deny-list
// (ValidateMountSource's own real-path re-check), before the root test even
// matters — so the member gate never weakens what already held.
func TestMemberMount_SymlinkToDeniedPrefix(t *testing.T) {
	root, _ := memberRoot(t)
	link := filepath.Join(root, "etc-link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	p := MemberMountPolicy{Roots: []string{root}}
	if err := p.ValidateMemberMount("alice", link, false); err == nil {
		t.Fatal("a symlink to /etc inside a member root was ALLOWED")
	}
}

// TestMemberMount_TraversalEscape is matrix row 2: "<root>/../../etc". Refused
// twice over — the uncleaned-path rule in ValidateMountSource catches the
// literal form, and a pre-cleaned one lands outside every root.
func TestMemberMount_TraversalEscape(t *testing.T) {
	root, _ := memberRoot(t)
	p := MemberMountPolicy{Roots: []string{root}}

	if err := p.ValidateMemberMount("alice", root+"/../../etc", false); err == nil {
		t.Error("uncleaned traversal path was ALLOWED")
	}
	// The lexically-cleaned form of the same intent: a real directory that sits
	// beside the root rather than inside it.
	sibling := filepath.Join(filepath.Dir(root), "notprojects")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := p.ValidateMemberMount("alice", sibling, false); err == nil {
		t.Error("a cleaned path outside every root was ALLOWED")
	}
}

// TestMemberMount_DotfileDenyUnderWideRoot is matrix row 5: with a root set as
// wide as a home directory, every credential dotfile is STILL refused — the
// deny-list is what bounds an operator's misconfiguration. A normal project dir
// under the same wide root stays allowed, so the deny-list is not just
// "refuse everything".
func TestMemberMount_DotfileDenyUnderWideRoot(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve tempdir: %v", err)
	}
	p := MemberMountPolicy{Roots: []string{home}}

	for _, rel := range []string{
		".ssh", ".aws", ".claude", ".wardyn", ".gnupg", ".docker", ".kube",
		".netrc", ".git-credentials", ".config/gh", ".git/config",
		// Traversed, not just named: the credential dir is a PARENT of the source.
		".ssh/keys/work", ".config/gh/hosts",
	} {
		dir := filepath.Join(home, rel)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := p.ValidateMemberMount("alice", dir, false); err == nil {
			t.Errorf("%s under a $HOME-wide root was ALLOWED; the dotfile deny-list is the only thing bounding a too-wide root", rel)
		}
	}

	ok := filepath.Join(home, "code", "app")
	if err := os.MkdirAll(ok, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := p.ValidateMemberMount("alice", ok, false); err != nil {
		t.Errorf("an ordinary project dir under the same root = %v, want allowed", err)
	}
	// ".config" and ".git" alone are ordinary project contents — only the two
	// credential-bearing CHILDREN are denied.
	plain := filepath.Join(home, "code", "app", ".config", "app.d")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := p.ValidateMemberMount("alice", plain, false); err != nil {
		t.Errorf("a non-credential .config subdir = %v, want allowed (only .config/gh is denied)", err)
	}
}

// TestMemberMount_NoRootsFailsClosed is matrix row 6: with nothing configured,
// a member local_dir mount is refused outright rather than falling back to the
// operator deny-list alone.
func TestMemberMount_NoRootsFailsClosed(t *testing.T) {
	_, project := memberRoot(t)
	var p MemberMountPolicy // the zero value IS the default deployment
	if p.Configured() {
		t.Fatal("the zero policy reports Configured()")
	}
	err := p.ValidateMemberMount("alice", project, false)
	if err == nil {
		t.Fatal("a member mount was allowed with NO roots configured; must fail closed")
	}
	if !strings.Contains(err.Error(), "no member workspace roots") {
		t.Errorf("error = %v, want the no-roots refusal", err)
	}
}

// TestMemberMount_AdditiveForOperators is matrix row 7: the BIND-time entry
// point with nil roots is the operator path, which must be untouched — the
// driver only calls it when SandboxSpec.MemberMountRoots is non-nil, and this
// pins that a nil-roots call is a refusal rather than a silent allow, so a
// wiring mistake in the other direction fails closed too.
func TestMemberMount_AdditiveForOperators(t *testing.T) {
	_, project := memberRoot(t)
	if err := ValidateMemberMountSource(project, nil); err == nil {
		t.Error("ValidateMemberMountSource with nil roots ALLOWED the source; it must refuse, leaving nil-means-operator to the CALLER's branch")
	}
	// And the operator's own gate keeps allowing what it always allowed.
	if err := ValidateMountSource(project); err != nil {
		t.Errorf("operator ValidateMountSource(%q) = %v, want allowed — the member gate must not narrow it", project, err)
	}
}

// TestMemberMount_MissingSourceFailsClosed pins the deliberate divergence from
// ValidateMountSource: that one falls through to lexical-only when a path does
// not resolve (the remote-daemon case), which for a member source would mean
// waving through a path the allowlist was never evaluated against.
func TestMemberMount_MissingSourceFailsClosed(t *testing.T) {
	root, _ := memberRoot(t)
	p := MemberMountPolicy{Roots: []string{root}}
	if err := p.ValidateMemberMount("alice", filepath.Join(root, "does-not-exist"), false); err == nil {
		t.Fatal("an unresolvable member source was ALLOWED; the root allowlist cannot be asserted about a path this process cannot see")
	}
}

// TestMemberMount_PerPrincipalReplacesShared pins O1: a principal with a map
// entry uses ONLY that entry. A union would make adding a per-member row WIDEN
// what that member reaches, which inverts the control's purpose.
func TestMemberMount_PerPrincipalReplacesShared(t *testing.T) {
	shared, sharedProject := memberRoot(t)
	own, ownProject := memberRoot(t)
	p := MemberMountPolicy{
		Roots:            []string{shared},
		RootsByPrincipal: map[string][]string{"alice": {own}},
	}

	if err := p.ValidateMemberMount("alice", ownProject, false); err != nil {
		t.Errorf("alice in her own root = %v, want allowed", err)
	}
	if err := p.ValidateMemberMount("alice", sharedProject, false); err == nil {
		t.Error("alice reached the SHARED root while holding her own map entry; per-member must REPLACE, not union")
	}
	// A principal with no entry still gets the shared list.
	if err := p.ValidateMemberMount("bob", sharedProject, false); err != nil {
		t.Errorf("bob (no map entry) in the shared root = %v, want allowed", err)
	}
	// Lookup is case-insensitive on the principal, like every other identity
	// match in this codebase.
	if err := p.ValidateMemberMount("ALICE", sharedProject, false); err == nil {
		t.Error("principal matching is case-SENSITIVE; a differently-cased sub would silently fall back to the shared list")
	}
	// An explicitly-empty entry means "this member mounts nothing".
	p.RootsByPrincipal["carol"] = []string{}
	if err := p.ValidateMemberMount("carol", sharedProject, false); err == nil {
		t.Error("an empty per-member entry fell back to the shared list; it must mean 'no roots'")
	}
}

// TestMemberMount_WritableAllowlist is O3: writable is a SEPARATE, narrower
// allowlist, default-empty, with a deny carve-out that wins.
func TestMemberMount_WritableAllowlist(t *testing.T) {
	root, project := memberRoot(t)
	vendored := filepath.Join(project, "vendor")
	if err := os.MkdirAll(vendored, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Default (no writable roots): read-only is fine, writable is refused.
	base := MemberMountPolicy{Roots: []string{root}}
	if err := base.ValidateMemberMount("alice", project, false); err != nil {
		t.Fatalf("read-only = %v, want allowed", err)
	}
	err := base.ValidateMemberMount("alice", project, true)
	if err == nil {
		t.Fatal("writable was allowed with NO writable roots configured; the default is no writable member mounts at all")
	}
	if !strings.Contains(err.Error(), "no member writable roots") {
		t.Errorf("error = %v, want the no-writable-roots refusal", err)
	}

	// Writable inside the allowlist: allowed.
	allowed := MemberMountPolicy{Roots: []string{root}, WritableRoots: []string{root}}
	if err := allowed.ValidateMemberMount("alice", project, true); err != nil {
		t.Errorf("writable inside the writable root = %v, want allowed", err)
	}

	// Writable OUTSIDE the allowlist (readable, but not writable): refused.
	other, otherProject := memberRoot(t)
	split := MemberMountPolicy{Roots: []string{root, other}, WritableRoots: []string{root}}
	if err := split.ValidateMemberMount("alice", otherProject, false); err != nil {
		t.Errorf("read-only in the second root = %v, want allowed", err)
	}
	if err := split.ValidateMemberMount("alice", otherProject, true); err == nil {
		t.Error("writable outside every writable root was ALLOWED")
	}

	// DENY WINS: a carve-out inside a writable root pins that subtree read-only.
	carved := MemberMountPolicy{
		Roots: []string{root}, WritableRoots: []string{root}, WritableDeny: []string{vendored},
	}
	if err := carved.ValidateMemberMount("alice", project, true); err != nil {
		t.Errorf("writable on the parent = %v, want allowed (the deny covers only the carve-out)", err)
	}
	derr := carved.ValidateMemberMount("alice", vendored, true)
	if derr == nil {
		t.Fatal("writable inside a writable-DENY carve-out was allowed; deny must win over allow")
	}
	if !strings.Contains(derr.Error(), "writable-deny") {
		t.Errorf("error = %v, want the deny-carve-out refusal", derr)
	}
	if err := carved.ValidateMemberMount("alice", vendored, false); err != nil {
		t.Errorf("READ-ONLY inside the writable-deny carve-out = %v, want allowed — the deny bounds writability, not readability", err)
	}
}

// TestParseMemberMountPolicy_FailsClosed pins the boot parse: a malformed root
// stops the daemon rather than starting it with a half-understood allowlist.
func TestParseMemberMountPolicy_FailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, roots, rootsMap, writable, deny string }{
		{name: "relative root", roots: "projects"},
		{name: "uncleaned root", roots: "/srv/projects/"},
		{name: "traversal root", roots: "/srv/../etc"},
		{name: "bad json map", rootsMap: "{"},
		{name: "empty principal", rootsMap: `{"  ": ["/srv/p"]}`},
		{name: "relative root in map", rootsMap: `{"alice": ["p"]}`},
		{name: "relative writable root", writable: "rw"},
		{name: "relative deny root", deny: "rw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ParseMemberMountPolicy(tc.roots, tc.rootsMap, tc.writable, tc.deny); err == nil {
				t.Error("parse accepted a malformed value; boot must fail closed")
			}
		})
	}

	p, warns, err := ParseMemberMountPolicy("/srv/projects, /srv/scratch", `{"Alice@corp.example": ["/srv/alice"]}`, "/srv/projects", "/srv/projects/vendor")
	if err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none for narrow roots", warns)
	}
	if got := p.RootsFor("alice@corp.example"); len(got) != 1 || got[0] != "/srv/alice" {
		t.Errorf("RootsFor = %v, want the lowercased map entry", got)
	}
	if got := p.RootsFor("bob"); len(got) != 2 {
		t.Errorf("RootsFor(bob) = %v, want the two shared roots", got)
	}
}

// TestParseMemberMountPolicy_WarnsOnWideRoots is O4: "/" and $HOME are WARNED
// about, not refused (matching the LocalMode unspecified-bind precedent). The
// warning is the only signal an operator gets, so its absence would be silent.
func TestParseMemberMountPolicy_WarnsOnWideRoots(t *testing.T) {
	_, warns, err := ParseMemberMountPolicy("/", "", "", "")
	if err != nil {
		t.Fatalf("root=/ must WARN, not refuse: %v", err)
	}
	if len(warns) == 0 {
		t.Error("root=/ produced no warning")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	_, warns, err = ParseMemberMountPolicy(home, "", "", "")
	if err != nil {
		t.Fatalf("root=$HOME must WARN, not refuse: %v", err)
	}
	if len(warns) == 0 {
		t.Errorf("root=$HOME (%s) produced no warning", home)
	}

	// A per-member entry gets the same scrutiny as the shared list.
	_, warns, err = ParseMemberMountPolicy("", `{"alice": ["/"]}`, "", "")
	if err != nil {
		t.Fatalf("per-member root=/ must WARN, not refuse: %v", err)
	}
	if len(warns) == 0 {
		t.Error("a per-member root of / produced no warning")
	}
}
