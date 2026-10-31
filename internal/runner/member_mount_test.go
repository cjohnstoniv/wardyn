// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	p := UserMountPolicy{Roots: []string{root}}
	if err := p.ValidateUserMount("alice", project, false); err != nil {
		t.Fatalf("read-only project dir under the root = %v, want allowed", err)
	}
	if err := p.ValidateUserMount("alice", root, false); err != nil {
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
	p := UserMountPolicy{Roots: []string{root}}
	err := p.ValidateUserMount("alice", link, false)
	if err == nil {
		t.Fatal("a symlink inside the root pointing outside every root was ALLOWED; the within-root test must run on the EvalSymlinks result, not the lexical path")
	}
	if !errors.Is(err, errMemberSourceRefused) || strings.Contains(err.Error(), outside) {
		t.Errorf("error = %v, want the member refusal, without the path the link resolves to", err)
	}
}

// TestMemberMount_RefusalsAreNoFilesystemOracle pins #2273: a member reads the same refusal whether a
// path outside their roots exists or not, and whether a link inside them points at something that
// exists or not, and never the place a link points to.
func TestMemberMount_RefusalsAreNoFilesystemOracle(t *testing.T) {
	root, _ := memberRoot(t)
	base := filepath.Dir(root)
	outside := filepath.Join(base, "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	present, missing := filepath.Join(root, "present"), filepath.Join(root, "missing")
	if err := os.Symlink(outside, present); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	if err := os.Symlink(filepath.Join(base, "gone"), missing); err != nil {
		t.Fatal(err)
	}
	p := UserMountPolicy{Roots: []string{root}}
	refusal := func(src string) string {
		t.Helper()
		err := p.ValidateUserMount("alice", src, false)
		if !errors.Is(err, errMemberSourceRefused) {
			t.Fatalf("%s = %v, want the member refusal", src, err)
		}
		return strings.ReplaceAll(err.Error(), strconv.Quote(src), "SRC")
	}
	if a, b := refusal(outside), refusal(filepath.Join(base, "gone")); a != b {
		t.Errorf("outside the roots, an existing and a missing path read differently:\n%s\n%s", a, b)
	}
	a, b := refusal(present), refusal(missing)
	if a != b {
		t.Errorf("inside the roots, a link to an existing and to a missing target read differently:\n%s\n%s", a, b)
	}
	if strings.Contains(a, outside) || strings.Contains(b, "gone") {
		t.Errorf("a refusal names where a link points: %s / %s", a, b)
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
	p := UserMountPolicy{Roots: []string{root}}
	if err := p.ValidateUserMount("alice", link, false); err == nil {
		t.Fatal("a symlink to /etc inside a member root was ALLOWED")
	}
}

// TestMemberMount_TraversalEscape is matrix row 2: "<root>/../../etc". Refused
// twice over — the uncleaned-path rule in ValidateMountSource catches the
// literal form, and a pre-cleaned one lands outside every root.
func TestMemberMount_TraversalEscape(t *testing.T) {
	root, _ := memberRoot(t)
	p := UserMountPolicy{Roots: []string{root}}

	if err := p.ValidateUserMount("alice", root+"/../../etc", false); err == nil {
		t.Error("uncleaned traversal path was ALLOWED")
	}
	// The lexically-cleaned form of the same intent: a real directory that sits
	// beside the root rather than inside it.
	sibling := filepath.Join(filepath.Dir(root), "notprojects")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := p.ValidateUserMount("alice", sibling, false); err == nil {
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
	p := UserMountPolicy{Roots: []string{home}}

	for _, rel := range []string{
		".ssh", ".aws", ".azure", ".claude", ".wardyn", ".gnupg", ".docker", ".kube",
		".netrc", ".git-credentials", ".config/gh", ".config/gcloud", ".git/config", "Library/Keychains",
		".codex", ".gemini", ".npmrc", ".pypirc", ".azure/cache",
		// Traversed, not just named: the credential dir is a PARENT of the source.
		".ssh/keys/work", ".config/gh/hosts", ".config/gcloud/legacy_credentials",
	} {
		dir := filepath.Join(home, rel)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := p.ValidateUserMount("alice", dir, false); err == nil {
			t.Errorf("%s under a $HOME-wide root was ALLOWED; the dotfile deny-list is the only thing bounding a too-wide root", rel)
		}
	}

	ok := filepath.Join(home, "code", "app")
	if err := os.MkdirAll(ok, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := p.ValidateUserMount("alice", ok, false); err != nil {
		t.Errorf("an ordinary project dir under the same root = %v, want allowed", err)
	}
	// ".config" and ".git" alone are ordinary project contents — only the two
	// credential-bearing CHILDREN are denied.
	plain := filepath.Join(home, "code", "app", ".config", "app.d")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := p.ValidateUserMount("alice", plain, false); err != nil {
		t.Errorf("a non-credential .config subdir = %v, want allowed (only .config/gh is denied)", err)
	}
}

// The dotfile deny-list compares case-folded: on a case-insensitive filesystem
// ".SSH" is ~/.ssh, and EvalSymlinks keeps the spelling it was given. A Unicode
// look-alike that folds to the same name (the Kelvin sign for "k") is the same
// name to the deny-list.
func TestMemberMount_DotfileDenyIgnoresCase(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve tempdir: %v", err)
	}
	p := UserMountPolicy{Roots: []string{home}}
	for _, rel := range []string{".SSH", ".Aws/sso", ".KUBE", ".Kube", ".Git/Config", ".CONFIG/GH/hosts", ".Git-Credentials"} {
		dir := filepath.Join(home, rel)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := p.ValidateUserMount("alice", dir, false); err == nil || !strings.Contains(err.Error(), "credential path") {
			t.Errorf("%s under a $HOME-wide root = %v, want the credential-path refusal", rel, err)
		}
		if err := ValidateUserMountSource(dir, p.Roots, nil, nil); err == nil {
			t.Errorf("%s at bind time was ALLOWED", rel)
		}
	}
	ok := filepath.Join(home, "Code", ".Configs", "Git")
	if err := os.MkdirAll(ok, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := p.ValidateUserMount("alice", ok, false); err != nil {
		t.Errorf("an ordinary dir = %v, want allowed", err)
	}
}

// The bind-time gate holds a read-write member bind to the writable ceiling on
// the path it resolves to NOW: a source that was writable at authoring and was
// re-pointed since, into a readable-only root or a writable-deny subtree, is
// refused. A read-only bind, and a spec that carries no ceiling, are checked as
// before.
func TestMemberMount_BindTimeHoldsWritableCeiling(t *testing.T) {
	root, project := memberRoot(t)
	other, otherProject := memberRoot(t)
	vendored := filepath.Join(project, "vendor")
	src := filepath.Join(project, "src")
	for _, d := range []string{vendored, src} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	p := UserMountPolicy{Roots: []string{root, other}, WritableRoots: []string{root}, WritableDeny: []string{vendored}}
	link := filepath.Join(root, "work")
	if err := os.Symlink(src, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	if err := p.ValidateUserMount("alice", link, true); err != nil {
		t.Fatalf("authoring: writable inside the writable root = %v, want allowed", err)
	}
	ceiling := p.Writable()
	if err := ValidateUserMountSource(link, p.Roots, &ceiling, nil); err != nil {
		t.Fatalf("bind time, unchanged source = %v, want allowed", err)
	}
	for name, target := range map[string]string{"a readable-only root": otherProject, "a writable-deny subtree": vendored} {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := ValidateUserMountSource(link, p.Roots, &ceiling, nil); err == nil || !strings.Contains(err.Error(), "may not be mounted writable") {
			t.Errorf("re-pointed into %s: bind-time = %v, want the writable refusal", name, err)
		}
		if err := ValidateUserMountSource(link, p.Roots, nil, nil); err != nil {
			t.Errorf("re-pointed into %s: read-only bind = %v, want allowed", name, err)
		}
	}
}

// TestMemberMount_NoRootsFailsClosed is matrix row 6: with nothing configured,
// a member local_dir mount is refused outright rather than falling back to the
// operator deny-list alone.
func TestMemberMount_NoRootsFailsClosed(t *testing.T) {
	_, project := memberRoot(t)
	var p UserMountPolicy // the zero value IS the default deployment
	if p.Configured() {
		t.Fatal("the zero policy reports Configured()")
	}
	err := p.ValidateUserMount("alice", project, false)
	if err == nil {
		t.Fatal("a member mount was allowed with NO roots configured; must fail closed")
	}
	if !strings.Contains(err.Error(), "no member workspace roots") {
		t.Errorf("error = %v, want the no-roots refusal", err)
	}
}

// TestMemberMount_AdditiveForOperators is matrix row 7: the BIND-time entry
// point with nil roots is the operator path, which must be untouched — the
// driver only calls it when SandboxSpec.UserMountRoots is non-nil, and this
// pins that a nil-roots call is a refusal rather than a silent allow, so a
// wiring mistake in the other direction fails closed too.
func TestMemberMount_AdditiveForOperators(t *testing.T) {
	_, project := memberRoot(t)
	if err := ValidateUserMountSource(project, nil, nil, nil); err == nil {
		t.Error("ValidateUserMountSource with nil roots ALLOWED the source; it must refuse, leaving nil-means-operator to the CALLER's branch")
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
	p := UserMountPolicy{Roots: []string{root}}
	if err := p.ValidateUserMount("alice", filepath.Join(root, "does-not-exist"), false); err == nil {
		t.Fatal("an unresolvable member source was ALLOWED; the root allowlist cannot be asserted about a path this process cannot see")
	}
}

// TestMemberMount_PerPrincipalReplacesShared pins O1: a principal with a map
// entry uses ONLY that entry. A union would make adding a per-member row WIDEN
// what that member reaches, which inverts the control's purpose.
func TestMemberMount_PerPrincipalReplacesShared(t *testing.T) {
	shared, sharedProject := memberRoot(t)
	own, ownProject := memberRoot(t)
	p := UserMountPolicy{
		Roots:            []string{shared},
		RootsByPrincipal: map[string][]string{"alice": {own}},
	}

	if err := p.ValidateUserMount("alice", ownProject, false); err != nil {
		t.Errorf("alice in her own root = %v, want allowed", err)
	}
	if err := p.ValidateUserMount("alice", sharedProject, false); err == nil {
		t.Error("alice reached the SHARED root while holding her own map entry; per-member must REPLACE, not union")
	}
	// A principal with no entry still gets the shared list.
	if err := p.ValidateUserMount("bob", sharedProject, false); err != nil {
		t.Errorf("bob (no map entry) in the shared root = %v, want allowed", err)
	}
	// Lookup is case-insensitive on the principal, like every other identity
	// match in this codebase.
	if err := p.ValidateUserMount("ALICE", sharedProject, false); err == nil {
		t.Error("principal matching is case-SENSITIVE; a differently-cased sub would silently fall back to the shared list")
	}
	// An explicitly-empty entry means "this member mounts nothing".
	p.RootsByPrincipal["carol"] = []string{}
	if err := p.ValidateUserMount("carol", sharedProject, false); err == nil {
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
	base := UserMountPolicy{Roots: []string{root}}
	if err := base.ValidateUserMount("alice", project, false); err != nil {
		t.Fatalf("read-only = %v, want allowed", err)
	}
	err := base.ValidateUserMount("alice", project, true)
	if err == nil {
		t.Fatal("writable was allowed with NO writable roots configured; the default is no writable member mounts at all")
	}
	if !strings.Contains(err.Error(), "no member writable roots") {
		t.Errorf("error = %v, want the no-writable-roots refusal", err)
	}

	// Writable inside the allowlist: allowed.
	allowed := UserMountPolicy{Roots: []string{root}, WritableRoots: []string{root}}
	if err := allowed.ValidateUserMount("alice", project, true); err != nil {
		t.Errorf("writable inside the writable root = %v, want allowed", err)
	}

	// Writable OUTSIDE the allowlist (readable, but not writable): refused.
	other, otherProject := memberRoot(t)
	split := UserMountPolicy{Roots: []string{root, other}, WritableRoots: []string{root}}
	if err := split.ValidateUserMount("alice", otherProject, false); err != nil {
		t.Errorf("read-only in the second root = %v, want allowed", err)
	}
	if err := split.ValidateUserMount("alice", otherProject, true); err == nil {
		t.Error("writable outside every writable root was ALLOWED")
	}

	// DENY WINS: a carve-out inside a writable root pins that subtree read-only.
	carved := UserMountPolicy{
		Roots: []string{root}, WritableRoots: []string{root}, WritableDeny: []string{vendored},
	}
	if err := carved.ValidateUserMount("alice", project, true); err == nil || !strings.Contains(err.Error(), "contains the member writable-deny path") {
		t.Errorf("writable on the parent = %v, want refused: a writable bind of the parent covers the carve-out", err)
	}
	derr := carved.ValidateUserMount("alice", vendored, true)
	if derr == nil {
		t.Fatal("writable inside a writable-DENY carve-out was allowed; deny must win over allow")
	}
	if !strings.Contains(derr.Error(), "writable-deny") {
		t.Errorf("error = %v, want the deny-carve-out refusal", derr)
	}
	if err := carved.ValidateUserMount("alice", vendored, false); err != nil {
		t.Errorf("READ-ONLY inside the writable-deny carve-out = %v, want allowed — the deny bounds writability, not readability", err)
	}
}

// TestMemberMount_WritableDenyBothDirections: a writable mount may neither sit
// under a WARDYN_USER_WRITABLE_DENY path nor contain one. A writable bind of a
// directory covers everything below it, so the parent of a denied subtree is
// as refused as the subtree; each direction answers its own sentence, and the
// second names the path. Read-only mounts are untouched.
func TestMemberMount_WritableDenyBothDirections(t *testing.T) {
	root, project := memberRoot(t)
	pkg := filepath.Join(project, "pkg")
	denied := filepath.Join(pkg, "vendor")
	child, sibling := filepath.Join(denied, "lib"), filepath.Join(pkg, "src")
	for _, d := range []string{child, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	link := filepath.Join(root, "link") // resolves to pkg, the deny path's parent
	if err := os.Symlink(pkg, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	p := UserMountPolicy{Roots: []string{root}, WritableRoots: []string{root}, WritableDeny: []string{denied}}
	const contains, under = "contains the member writable-deny path", "under a member writable-deny path"
	for _, tc := range []struct {
		name, src string
		writable  bool
		want      string // "" = allowed
	}{
		{"the writable root itself", root, true, contains},
		{"a middle ancestor", project, true, contains},
		{"the deny path's parent", pkg, true, contains},
		{"a symlinked ancestor", link, true, contains},
		{"the deny path", denied, true, under},
		{"a child of the deny path", child, true, under},
		{"a sibling of the deny path", sibling, true, ""},
		{"an ancestor, read-only", project, false, ""},
		{"the writable root itself, read-only", root, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := p.ValidateUserMount("alice", tc.src, tc.writable)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("%s (writable=%v) = %v, want allowed", tc.src, tc.writable, err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("%s (writable=%v) = %v, want the %q refusal", tc.src, tc.writable, err, tc.want)
			case tc.want == contains && !strings.Contains(err.Error(), denied):
				t.Errorf("error = %v, want it to name the deny path %s", err, denied)
			}
		})
	}
}

// A deny entry that does not exist yet and is spelled through a symlinked parent still sits where
// the resolved parent does, so a writable mount of that parent is refused.
func TestMemberMount_WritableDenyBehindASymlinkedParentThatDoesNotExistYet(t *testing.T) {
	root, project := memberRoot(t)
	pkg := filepath.Join(project, "pkg")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(pkg, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	p := UserMountPolicy{Roots: []string{root}, WritableRoots: []string{root}, WritableDeny: []string{filepath.Join(link, "ghost", "vendor")}}
	if err := p.ValidateUserMount("alice", pkg, true); err == nil || !strings.Contains(err.Error(), "contains the member writable-deny path") {
		t.Errorf("writable mount of the resolved parent = %v, want the contains refusal", err)
	}
	if err := p.ValidateUserMount("alice", pkg, false); err != nil {
		t.Errorf("read-only = %v, want allowed", err)
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
			if _, _, err := ParseUserMountPolicy(tc.roots, tc.rootsMap, tc.writable, tc.deny); err == nil {
				t.Error("parse accepted a malformed value; boot must fail closed")
			}
		})
	}

	p, warns, err := ParseUserMountPolicy("/srv/projects, /srv/scratch", `{"Alice@corp.example": ["/srv/alice"]}`, "/srv/projects", "/srv/projects/vendor")
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
	_, warns, err := ParseUserMountPolicy("/", "", "", "")
	if err != nil {
		t.Fatalf("root=/ must WARN, not refuse: %v", err)
	}
	if len(warns) == 0 {
		t.Error("root=/ produced no warning")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	_, warns, err = ParseUserMountPolicy(home, "", "", "")
	if err != nil {
		t.Fatalf("root=$HOME must WARN, not refuse: %v", err)
	}
	if len(warns) == 0 {
		t.Errorf("root=$HOME (%s) produced no warning", home)
	}

	// A per-member entry gets the same scrutiny as the shared list.
	_, warns, err = ParseUserMountPolicy("", `{"alice": ["/"]}`, "", "")
	if err != nil {
		t.Fatalf("per-member root=/ must WARN, not refuse: %v", err)
	}
	if len(warns) == 0 {
		t.Error("a per-member root of / produced no warning")
	}
}

// R5: bootWarnings' sentence for a root must describe what withinAnyRoot
// actually does with that root — the two "/"-and-$HOME cases were merged into
// one wide-open sentence, and for "/" it stated the OPPOSITE of the code.
// withinAnyRoot matches `real == root || strings.HasPrefix(real, root+"/")`, so
// a root of "/" compares against the prefix "//" and matches nothing but the
// literal "/" — the same dead ceiling ParseUserDriveHostRoots already words as
// "matches NOTHING". This drives the parser and withinAnyRoot together, so the
// wording cannot drift from the behaviour again.
func TestMemberBootWarnings_MatchWithinAnyRootReality(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	under := filepath.Join(home, "proj")
	if err := os.MkdirAll(under, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		root string
		// probe is a real path under root that a member might mount.
		probe string
	}{
		{"slash-is-dead", "/", under},
		{"home-is-wide-open", home, under},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, warns, err := ParseUserMountPolicy(tc.root, "", "", "")
			if err != nil {
				t.Fatalf("ParseUserMountPolicy(%q): %v", tc.root, err)
			}
			if len(warns) == 0 {
				t.Fatalf("ParseUserMountPolicy(%q) warned about nothing", tc.root)
			}
			got := strings.Join(warns, "\n")
			if !strings.Contains(got, "WARDYN_USER_WORKSPACE_ROOTS") {
				t.Errorf("warning does not name the variable that is set wrong:\n  %s", got)
			}

			// The ground truth the sentence has to agree with.
			reaches := withinAnyRoot(tc.probe, []string{tc.root})
			wideOpen := strings.Contains(got, "bounded only by the credential dotfile deny-list")
			dead := strings.Contains(got, "matches NOTHING")

			if reaches && !wideOpen {
				t.Errorf("withinAnyRoot(%q, [%q]) = true (the root really is wide open) but the warning does not say so:\n  %s", tc.probe, tc.root, got)
			}
			if !reaches && wideOpen {
				t.Errorf("withinAnyRoot(%q, [%q]) = false — the root refuses every mount under it — yet the warning claims a member's mounts are bounded only by the dotfile deny-list, the OPPOSITE:\n  %s", tc.probe, tc.root, got)
			}
			if !reaches && !dead {
				t.Errorf("withinAnyRoot(%q, [%q]) = false, so this ceiling is DEAD; the warning must say so in ParseUserDriveHostRoots' words (\"matches NOTHING\"):\n  %s", tc.probe, tc.root, got)
			}
			if reaches && dead {
				t.Errorf("withinAnyRoot(%q, [%q]) = true but the warning calls the ceiling dead:\n  %s", tc.probe, tc.root, got)
			}
		})
	}
}

// A local_dir folder bound to a person's runner lives on that runner, never on the control plane's
// host, so its authoring gate reads text only: a member folder that does not exist here passes, and
// the runner checks it on its own disk at bind.
func TestValidateUserMountLexical_FolderOnlyOnTheRunnerPasses(t *testing.T) {
	const root, onLaptopOnly = "/Users/alice/src", "/Users/alice/src/payments-api"
	if _, err := os.Lstat(root); err == nil {
		t.Skipf("%s exists on this host; the case needs a folder only a runner holds", root)
	}
	p := UserMountPolicy{
		RootsByPrincipal: map[string][]string{"alice@example.com": {root}},
		WritableRoots:    []string{root},
	}
	// The host gate refuses it: the folder is not on this host.
	if err := p.ValidateUserMount("alice@example.com", onLaptopOnly, false); !errors.Is(err, errMemberSourceRefused) {
		t.Fatalf("host gate = %v, want the member refusal", err)
	}
	for _, tc := range []struct {
		principal, src string
		writable       bool
	}{
		{"alice@example.com", onLaptopOnly, false},
		{"alice@example.com", onLaptopOnly, true},
		{"alice@example.com", root, false},
		{"Alice@Example.com", onLaptopOnly, false}, // RootsFor folds the principal
	} {
		if err := p.ValidateUserMountLexical(tc.principal, tc.src, tc.writable); err != nil {
			t.Errorf("ValidateUserMountLexical(%q, %q, writable=%v) = %v, want allowed", tc.principal, tc.src, tc.writable, err)
		}
	}
}

// Everything text can show is still refused for a runner-bound folder, and refused for what the text
// says: never the host-lookup sentence, since nothing on this host was read.
func TestValidateUserMountLexical_RefusesWhatTextCanShow(t *testing.T) {
	const root = "/Users/alice/src"
	p := UserMountPolicy{
		Roots:         []string{root},
		WritableRoots: []string{root + "/w"},
		WritableDeny:  []string{root + "/w/secrets", root + "/w/app/keys"},
	}
	wide := UserMountPolicy{Roots: []string{"/"}} // only the operator deny-list can refuse here
	for _, tc := range []struct {
		name     string
		p        UserMountPolicy
		src      string
		writable bool
		refused  bool
	}{
		{"a folder under the root", p, root + "/app", false, false},
		{"writable under the writable root", p, root + "/w/lib", true, false},
		{"a sibling of a deny path", p, root + "/w/secrets-docs", true, false},
		{"relative", p, "src/app", false, true},
		{"a .. segment", p, root + "/../bob/app", false, true},
		{"outside the roots", p, "/Users/bob/src/app", false, true},
		{"a root's prefix sibling", p, root + "2/app", false, true},
		{".ssh", p, root + "/.ssh", false, true},
		{".config/gh", p, root + "/app/.config/gh", false, true},
		{"/etc/x", wide, "/etc/x", false, true},
		{"docker.sock", wide, "/var/run/docker.sock", false, true},
		{"no roots", UserMountPolicy{}, root + "/app", false, true},
		{"writable outside the writable roots", p, root + "/app", true, true},
		{"writable under a deny path in another case", p, root + "/w/SECRETS/x", true, true},
		{"writable over a deny path in another case", p, root + "/w/APP", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.ValidateUserMountLexical("alice", tc.src, tc.writable)
			switch {
			case !tc.refused && err != nil:
				t.Fatalf("%s (writable=%v) = %v, want allowed", tc.src, tc.writable, err)
			case tc.refused && err == nil:
				t.Fatalf("%s (writable=%v) was ALLOWED", tc.src, tc.writable)
			case tc.refused && strings.Contains(err.Error(), "must resolve on this host"):
				t.Fatalf("%s (writable=%v) = %v, want a refusal by its text, not a host lookup", tc.src, tc.writable, err)
			}
		})
	}
}

// What exists on the control plane's host never changes a runner-bound folder's answer: the same
// paths answer the same before and after this host grows a symlinked root and a link out of it.
func TestValidateUserMountLexical_ReadsNoServerFile(t *testing.T) {
	base := realTemp(t)
	root, elsewhere := filepath.Join(base, "projects"), filepath.Join(base, "elsewhere")
	p := UserMountPolicy{Roots: []string{root}, WritableRoots: []string{root}}
	srcs := []string{filepath.Join(root, "app"), filepath.Join(root, "escape"), filepath.Join(elsewhere, "app")}
	want := []bool{true, true, false} // allowed by text: under the root, under the root, outside it
	answers := func() (got []bool) {
		for _, writable := range []bool{false, true} {
			for _, src := range srcs {
				got = append(got, p.ValidateUserMountLexical("alice", src, writable) == nil)
			}
		}
		return got
	}
	before := answers()
	if !slices.Equal(before, append(slices.Clone(want), want...)) {
		t.Fatalf("with nothing on this host: allowed = %v for %v (read-only, then writable), want %v twice", before, srcs, want)
	}
	// The root becomes a link to elsewhere, and root/escape a link out of every root.
	mkdirs(t, filepath.Join(elsewhere, "app"), filepath.Join(base, "secret"))
	if err := os.Symlink(elsewhere, root); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	if err := os.Symlink(filepath.Join(base, "secret"), filepath.Join(elsewhere, "escape")); err != nil {
		t.Fatal(err)
	}
	if after := answers(); !slices.Equal(after, before) {
		t.Fatalf("after this host's links: allowed = %v, want %v: the answer read this host", after, before)
	}
}

// On a Mac, /r/APP and /r/app are one folder, so a writable deny holds whatever the spelling: in
// either direction, as a path under the deny entry or a folder that contains it.
func TestUserWritableCeiling_DenyIgnoresCase(t *testing.T) {
	r := filepath.Join(realTemp(t), "r")
	c := UserWritableCeiling{Roots: []string{r}, Deny: []string{filepath.Join(r, "app", "secrets")}}
	for _, rel := range []string{"APP", "App/Secrets", "app/SECRETS/x"} {
		real := filepath.Join(r, rel)
		if err := c.allows(real, real); err == nil || !strings.Contains(err.Error(), "writable-deny") {
			t.Errorf("writable %s = %v, want the writable-deny refusal", rel, err)
		}
	}
	for _, rel := range []string{"APP/src", "app/secrets-docs", "other"} {
		real := filepath.Join(r, rel)
		if err := c.allows(real, real); err != nil {
			t.Errorf("writable %s = %v, want allowed", rel, err)
		}
	}
}

// On a runner the bind is also held to the runner owner's own roots (--drive-root): a runner with
// none refuses, and a folder inside the org's roots but outside the owner's, or a link out of them,
// is refused. No own roots (nil) is wardynd's host, checked as before.
func TestValidateUserMountSource_HoldsOwnRoots(t *testing.T) {
	root, project := memberRoot(t)
	own := filepath.Join(root, "shared")
	inOwn, link := filepath.Join(own, "app"), filepath.Join(own, "link")
	mkdirs(t, inOwn)
	if err := os.Symlink(project, link); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	roots, ceiling := []string{root}, &UserWritableCeiling{Roots: []string{root}}
	for _, tc := range []struct {
		name     string
		src      string
		writable *UserWritableCeiling
		ownRoots []string
		refused  bool
	}{
		{"inside the owner's roots", inOwn, nil, []string{own}, false},
		{"inside the owner's roots, writable", inOwn, ceiling, []string{own}, false},
		{"a runner with no own roots", inOwn, nil, []string{}, true},
		{"inside the org's roots, outside the owner's", project, nil, []string{own}, true},
		{"a link out of the owner's roots", link, nil, []string{own}, true},
		{"wardynd's host", project, nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateUserMountSource(tc.src, roots, tc.writable, tc.ownRoots)
			if tc.refused != (err != nil) {
				t.Fatalf("ValidateUserMountSource(%s, own=%v) = %v, want refused=%v", tc.src, tc.ownRoots, err, tc.refused)
			}
		})
	}
}
