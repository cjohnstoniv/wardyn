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
		if err := ValidateUserMountSource(dir, p.Roots, nil); err == nil {
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
	if err := ValidateUserMountSource(link, p.Roots, &ceiling); err != nil {
		t.Fatalf("bind time, unchanged source = %v, want allowed", err)
	}
	for name, target := range map[string]string{"a readable-only root": otherProject, "a writable-deny subtree": vendored} {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := ValidateUserMountSource(link, p.Roots, &ceiling); err == nil || !strings.Contains(err.Error(), "may not be mounted writable") {
			t.Errorf("re-pointed into %s: bind-time = %v, want the writable refusal", name, err)
		}
		if err := ValidateUserMountSource(link, p.Roots, nil); err != nil {
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
	if err := ValidateUserMountSource(project, nil, nil); err == nil {
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
