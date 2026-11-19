// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/foldersync"
)

// scanFixture makes dirs, each with a .env.example naming its token, and the links, under a resolved
// temp folder it returns.
func scanFixture(t *testing.T, dirs map[string]string, links map[string]string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for dir, token := range dirs {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, dir, ".env.example"), []byte(token+"=\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for link, target := range links {
		if !filepath.IsAbs(target) {
			target = filepath.Join(base, target)
		}
		if err := os.Symlink(target, filepath.Join(base, link)); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

// wantRefused fails unless OpenScanSource refused src with the member refusal and no handle, without
// naming where the folder resolved.
func wantRefused(t *testing.T, p UserMountPolicy, owner, src, hidden string, holders []ScanHolder) {
	t.Helper()
	dir, err := p.OpenScanSource(owner, src, holders, foldersync.OpenFromTop)
	if dir != nil {
		_ = dir.Close()
	}
	if dir != nil || !errors.Is(err, errMemberSourceRefused) || strings.Contains(err.Error(), hidden) {
		t.Errorf("OpenScanSource(%q, %s) = %v, %v; want no handle and the member refusal, without %s", owner, src, dir, err, hidden)
	}
}

// wantOpened fails unless OpenScanSource opened src and reads token's file through the handle.
func wantOpened(t *testing.T, p UserMountPolicy, owner, src, token string, holders []ScanHolder) {
	t.Helper()
	dir, err := p.OpenScanSource(owner, src, holders, foldersync.OpenFromTop)
	if err != nil || dir == nil {
		t.Errorf("OpenScanSource(%q, %s) = %v, %v; want the folder opened", owner, src, dir, err)
		return
	}
	defer dir.Close()
	f, err := dir.Open(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if body, _ := io.ReadAll(f); string(body) != token+"=\n" {
		t.Errorf("OpenScanSource(%q, %s) read %q through the handle, want %s", owner, src, body, token)
	}
}

// A member workspace attaches a folder that none of its owner's roots holds any more: the roots were
// narrowed, or removed, after onboarding. Every scan of that folder is refused, the operator's and
// another member's, with the workspace and its owner named, though the folder is a link out of every
// root and a scan by its path would read through it. A member reads the same refusal without the
// name. The scanning member's own workspace never blocks them this way, and a folder that no root
// holds and no member workspace attaches is still read by its path.
func TestOpenScanSource_AWorkspaceWhoseOwnersRootsNoLongerHoldTheFolderBlocksItsScan(t *testing.T) {
	base := scanFixture(t, map[string]string{"old/plain": "OLD_TOKEN", "elsewhere": "OUTSIDE_TOKEN", "now": "NOW_TOKEN"},
		map[string]string{"old/app": "elsewhere"})
	old, now := filepath.Join(base, "old"), filepath.Join(base, "now")
	alice := ScanHolder{Owner: "alice", Workspace: "payroll"}
	for name, tc := range map[string]struct {
		policy UserMountPolicy
		roots  string
	}{
		"narrowed": {UserMountPolicy{RootsByPrincipal: map[string][]string{"alice": {now}}}, now},
		"removed":  {UserMountPolicy{}, "none set"},
	} {
		dir, err := tc.policy.OpenScanSource("", filepath.Join(old, "app"), []ScanHolder{alice}, foldersync.OpenFromTop)
		var stale *StaleWorkspaceError
		if dir != nil || !errors.As(err, &stale) || !errors.Is(err, errMemberSourceRefused) {
			t.Fatalf("%s: OpenScanSource = %v, %v; want no handle and the stale workspace refusal", name, dir, err)
		}
		for _, want := range []string{`workspace "payroll" of alice`, "(" + tc.roots + ")", "remove the folder from that workspace", "WARDYN_USER_WORKSPACE_ROOTS"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: refusal %q does not name %q", name, err, want)
			}
		}
		if u := stale.Unnamed(); strings.Contains(u, "payroll") || strings.Contains(u, "alice") || strings.Contains(u, now) || !strings.Contains(u, "an operator can see which workspace in the audit log") {
			t.Errorf("%s: unnamed refusal %q names the workspace, its owner or their roots", name, u)
		}
		if strings.Contains(err.Error(), filepath.Join(base, "elsewhere")) {
			t.Errorf("%s: refusal %q names where the folder resolves", name, err)
		}
	}

	// Another member, whose shared roots still hold the folder, is blocked by alice's workspace; their own is not.
	shared := UserMountPolicy{Roots: []string{old}, RootsByPrincipal: map[string][]string{"alice": {now}}}
	bob := ScanHolder{Owner: "bob", Workspace: "bob-ws"}
	if dir, err := shared.OpenScanSource("bob", filepath.Join(old, "plain"), []ScanHolder{bob, alice}, foldersync.OpenFromTop); dir != nil || !errors.As(err, new(*StaleWorkspaceError)) {
		t.Errorf("bob's scan of a folder alice's workspace attaches = %v, %v; want the stale workspace refusal", dir, err)
	}
	wantOpened(t, shared, "bob", filepath.Join(old, "plain"), "OLD_TOKEN", []ScanHolder{bob})
	wantOpened(t, shared, "", filepath.Join(old, "plain"), "OLD_TOKEN", []ScanHolder{bob})
	if dir, err := (UserMountPolicy{}).OpenScanSource("", filepath.Join(old, "app"), nil, foldersync.OpenFromTop); dir != nil || err != nil {
		t.Errorf("a folder no root holds and no member workspace attaches = %v, %v; want no handle and no error", dir, err)
	}
}

// A folder named by another name of a member root, a link to the root or to a folder above it or one
// landing inside it, or another letter case of the root on a volume that ignores case, is held by
// that root. A link out of the root under that name is refused, a plain folder is opened, and an
// operator's folder outside every root keeps no handle.
func TestOpenScanSource_AnotherNameOfAMemberRootHoldsTheFolder(t *testing.T) {
	base := scanFixture(t, map[string]string{"real/members/app": "INSIDE_TOKEN", "elsewhere": "OUTSIDE_TOKEN"}, map[string]string{
		"real/members/out": "elsewhere",        // a member's link out of the root
		"alias":            "real",             // an operator's link above the root
		"second":           "real/members",     // a second name of the root
		"deep":             "real/members/out", // an operator's link landing on the member's link
		"deep-plain":       "real/members/app",
		"linked":           "real/members", // the root as one deployment sets it
	})
	outside := filepath.Join(base, "elsewhere")
	for name, root := range map[string]string{"the root as it resolves": filepath.Join(base, "real", "members"), "the root set through a link": filepath.Join(base, "linked")} {
		t.Run(name, func(t *testing.T) {
			p := UserMountPolicy{Roots: []string{root}}
			for _, src := range []string{"alias/members/out", "second/out", "deep"} {
				wantRefused(t, p, "", filepath.Join(base, src), outside, nil)
			}
			for _, src := range []string{"alias/members/app", "second/app", "deep-plain"} {
				wantOpened(t, p, "", filepath.Join(base, src), "INSIDE_TOKEN", nil)
			}
			if dir, err := p.OpenScanSource("", outside, nil, foldersync.OpenFromTop); dir != nil || err != nil {
				t.Errorf("an operator's folder outside every root = %v, %v; want no handle and no error", dir, err)
			}
		})
	}
	if _, err := os.Lstat(filepath.Join(base, "REAL")); err == nil {
		wantRefused(t, UserMountPolicy{Roots: []string{filepath.Join(base, "real", "members")}}, "", filepath.Join(base, "REAL", "Members", "out"), outside, nil)
	}
}

// A member whose roots are the shared root scans a folder inside another person's own root that is a
// link to a folder of the shared root outside that person's root. That person may not mount it, and
// may read what the scan finds (their workspace attaches it, or can), so it is refused at the wider
// member's door too, naming only the scanning member's roots. A plain folder there is opened.
func TestOpenScanSource_AWiderMemberIsHeldToTheNarrowerRootThatHoldsTheFolder(t *testing.T) {
	base := scanFixture(t, map[string]string{"shared/team": "TEAM_TOKEN", "shared/alice/plain": "ALICE_TOKEN"},
		map[string]string{"shared/alice/app": "shared/team"})
	shared, aliceRoot := filepath.Join(base, "shared"), filepath.Join(base, "shared", "alice")
	p := UserMountPolicy{Roots: []string{shared}, RootsByPrincipal: map[string][]string{"alice": {aliceRoot}}}
	holders := []ScanHolder{{Owner: "alice", Workspace: "alice-ws"}, {Owner: "bob", Workspace: "bob-ws"}}
	for _, h := range [][]ScanHolder{holders, nil} {
		wantRefused(t, p, "bob", filepath.Join(aliceRoot, "app"), filepath.Join(shared, "team"), h)
	}
	wantOpened(t, p, "bob", filepath.Join(aliceRoot, "plain"), "ALICE_TOKEN", holders)
	wantOpened(t, p, "bob", filepath.Join(shared, "team"), "TEAM_TOKEN", holders[1:])
}

// An operator's relative link, or one with a ".." on the way, is walked as EvalSymlinks walks it, and
// the folder it lands on is held to every root that holds it there, a person's own root inside the
// shared one included: alice's link out of her root into the shared root is refused under that
// name too. Plain folders there are opened.
func TestOpenScanSource_AnOperatorsRelativeLinkIsHeldToEveryRootThatHoldsTheFolder(t *testing.T) {
	base := scanFixture(t, map[string]string{"real/x": "X_TOKEN", "real/members/team": "TEAM_TOKEN", "real/members/alice/plain": "ALICE_TOKEN"}, nil)
	for link, target := range map[string]string{
		"alias":                  "real",
		"alias-up":               "real/x/../members",
		"real/members/alice/app": "../team",
	} {
		if err := os.Symlink(target, filepath.Join(base, link)); err != nil {
			t.Fatal(err)
		}
	}
	shared := filepath.Join(base, "real", "members")
	p := UserMountPolicy{Roots: []string{shared}, RootsByPrincipal: map[string][]string{"alice": {filepath.Join(shared, "alice")}}}
	for _, src := range []string{"alias/members/alice/app", "alias-up/alice/app"} {
		wantRefused(t, p, "", filepath.Join(base, src), filepath.Join(shared, "team"), nil)
	}
	wantOpened(t, p, "", filepath.Join(base, "alias", "members", "team"), "TEAM_TOKEN", nil)
	wantOpened(t, p, "", filepath.Join(base, "alias-up", "alice", "plain"), "ALICE_TOKEN", nil)
}

// A member whose own roots no longer hold their folder reads their own refusal, though another
// person's workspace that attaches the folder is stale too: that workspace, or that there is one, is
// not theirs to learn.
func TestOpenScanSource_AMemberWhoseOwnRootsNoLongerHoldTheFolderReadsOnlyTheirOwnRefusal(t *testing.T) {
	base := scanFixture(t, map[string]string{"old/app": "OLD_TOKEN", "now": "NOW_TOKEN"}, nil)
	src, now := filepath.Join(base, "old", "app"), filepath.Join(base, "now")
	p := UserMountPolicy{RootsByPrincipal: map[string][]string{"alice": {now}, "carol": {now}}}
	holders := []ScanHolder{{Owner: "alice", Workspace: "mine"}, {Owner: "carol", Workspace: "theirs"}}
	dir, err := p.OpenScanSource("alice", src, holders, foldersync.OpenFromTop)
	if dir != nil {
		_ = dir.Close()
	}
	if dir != nil || !errors.Is(err, errMemberSourceRefused) || errors.As(err, new(*StaleWorkspaceError)) ||
		!strings.Contains(err.Error(), "outside every member workspace root") || strings.Contains(err.Error(), "attached to") {
		t.Errorf("OpenScanSource(alice, %s) = %v, %v; want alice's own refusal and nothing of carol's workspace", src, dir, err)
	}
}
