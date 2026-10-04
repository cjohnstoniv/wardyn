// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPATRepoKey(t *testing.T) {
	for _, tc := range []struct {
		name, forge, path string
		want              string
		ok                bool
	}{
		{"plain", "generic", "team/app", "team/app", true},
		{"empty forge is generic", "", "team/app", "team/app", true},
		{"generic keeps case", "generic", "Team/App.git", "Team/App.git", true},
		{"generic keeps .git", "generic", "team/app.git", "team/app.git", true},
		{"gitlab keeps path exactly", "gitlab", "Team/App.git", "Team/App.git", true},
		{"gitea keeps path exactly", "gitea", "team/app", "team/app", true},
		{"bitbucket keeps path exactly", "bitbucket_server", "PROJ/repo", "PROJ/repo", true},
		{"subgroup", "gitlab", "g/sub/app", "g/sub/app", true},
		{"one trailing wildcard", "generic", "group/*", "group/*", true},
		{"two trailing wildcards", "generic", "group/*/*", "", false},
		{"bare wildcard", "generic", "/*", "", false},
		{"empty", "generic", "", "", false},
		{"dotdot segment", "generic", "team/../app", "", false},
		{"dotdot inside a name", "generic", "team/a..b", "", false},
		{"dot segment", "generic", "team/./app", "", false},
		{"query", "generic", "team/app?x=1", "", false},
		{"fragment", "generic", "team/app#x", "", false},
		{"backslash", "generic", `team\app`, "", false},
		{"control character", "generic", "team/app\n", "", false},
		{"del", "generic", "team/app\x7f", "", false},
		{"leading slash", "generic", "/team/app", "", false},
		{"trailing slash", "generic", "team/app/", "", false},
		{"empty segment", "generic", "team//app", "", false},
		{"unknown forge", "svn", "team/app", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PATRepoKey(tc.forge, tc.path)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("PATRepoKey(%q, %q) = (%q, %v), want (%q, %v)", tc.forge, tc.path, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// The generic collision pairs: distinct repositories on a git-http-backend, so
// distinct keys. A key function that folded either pair would let a ceiling
// entry for one cover the other.
func TestPATRepoKeyGenericCollisionPairsStayDistinct(t *testing.T) {
	for _, pair := range [][2]string{
		{"Team/App.git", "team/app.git"},
		{"team/foo", "team/foo.git"},
		{"Team/App", "team/app"},
	} {
		a, aok := PATRepoKey(PATForgeGeneric, pair[0])
		b, bok := PATRepoKey(PATForgeGeneric, pair[1])
		if !aok || !bok {
			t.Fatalf("%v: both must be valid keys", pair)
		}
		if a == b {
			t.Errorf("%q and %q collapse to one key %q", pair[0], pair[1], a)
		}
		if PATRepoCovers(a, b) || PATRepoCovers(b, a) {
			t.Errorf("%q and %q cover each other", pair[0], pair[1])
		}
	}
}

func TestPATRepoCovers(t *testing.T) {
	for _, tc := range []struct {
		name, entry, key string
		want             bool
	}{
		{"equal", "team/app", "team/app", true},
		{"different", "team/app", "team/other", false},
		{"prefix is not coverage", "team/app", "team/app-x", false},
		{"child is not coverage", "team/app", "team/app/sub", false},
		{"wildcard covers one segment", "group/*", "group/app", true},
		{"wildcard does not cover two segments", "group/*", "group/sub/app", false},
		{"wildcard does not cover its own group", "group/*", "group", false},
		{"wildcard does not cover a sibling group", "group/*", "groupx/app", false},
		{"wildcard covers itself", "group/*", "group/*", true},
		{"a name does not cover a wildcard", "group/app", "group/*", false},
		{"a different wildcard does not cover a wildcard", "other/*", "group/*", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PATRepoCovers(tc.entry, tc.key); got != tc.want {
				t.Fatalf("PATRepoCovers(%q, %q) = %v, want %v", tc.entry, tc.key, got, tc.want)
			}
		})
	}
}

func TestDecodeGitPATScopeDefaults(t *testing.T) {
	sc, err := DecodeGitPATScope(json.RawMessage(`{"host":"gitlab.example.com","secret_name":"pat"}`))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Repos != nil || sc.Access != PATAccessWrite || sc.API || sc.Forge != PATForgeGeneric {
		t.Fatalf("omission defaults wrong: %+v", sc)
	}
	if sc.Narrowed() || sc.SetsAnyAxis() {
		t.Fatalf("an unnarrowed scope reads as narrowed: %+v", sc)
	}
}

// An absent repos is "all"; an empty one is "none". Reading the second as the
// first would turn an empty intersection into an unnarrowed grant.
func TestDecodeGitPATScopeAbsentAndEmptyReposStayDistinct(t *testing.T) {
	absent, err := DecodeGitPATScope(json.RawMessage(`{"host":"h","secret_name":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := DecodeGitPATScope(json.RawMessage(`{"host":"h","secret_name":"s","repos":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if absent.Repos != nil {
		t.Fatal("absent repos decoded as set")
	}
	if empty.Repos == nil || len(*empty.Repos) != 0 {
		t.Fatalf("empty repos decoded as %v", empty.Repos)
	}
	if absent.Narrowed() || !empty.Narrowed() {
		t.Fatal("absent repos must be unnarrowed and an empty list narrowed")
	}
	b, err := json.Marshal(empty)
	if err != nil || !strings.Contains(string(b), `"repos":[]`) {
		t.Fatalf("an empty list must survive a round trip, got %s (%v)", b, err)
	}
	b, _ = json.Marshal(absent)
	if strings.Contains(string(b), "repos") {
		t.Fatalf("an absent list must stay absent, got %s", b)
	}
}

func TestDecodeGitPATScopeNarrowedAndFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		narrowed  bool
		anyAxis   bool
		access    string
		forge     string
	}{
		{"read", `{"host":"h","secret_name":"s","access":"read"}`, true, true, "read", "generic"},
		{"explicit write", `{"host":"h","secret_name":"s","access":"write"}`, false, false, "write", "generic"},
		{"api", `{"host":"h","secret_name":"s","api":true,"forge":"gitlab"}`, true, true, "write", "gitlab"},
		{"forge only", `{"host":"h","secret_name":"s","forge":"gitea"}`, false, true, "write", "gitea"},
		{"explicit generic", `{"host":"h","secret_name":"s","forge":"generic"}`, false, false, "write", "generic"},
		{"unknown access reads as read", `{"host":"h","secret_name":"s","access":"admin"}`, true, true, "read", "generic"},
		{"unknown forge reads as generic", `{"host":"h","secret_name":"s","forge":"svn"}`, false, false, "write", "generic"},
		{"stray key is ignored", `{"host":"h","secret_name":"s","repo":["a/b"]}`, false, false, "write", "generic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc, err := DecodeGitPATScope(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if sc.Narrowed() != tc.narrowed || sc.SetsAnyAxis() != tc.anyAxis || sc.Access != tc.access || sc.Forge != tc.forge {
				t.Fatalf("got %+v narrowed=%v any=%v", sc, sc.Narrowed(), sc.SetsAnyAxis())
			}
		})
	}
}

func TestDecodeGitPATScopeRequiresHostAndSecret(t *testing.T) {
	for _, raw := range []string{`{}`, `{"host":"h"}`, `{"secret_name":"s"}`, `[]`, `not json`} {
		if _, err := DecodeGitPATScope(json.RawMessage(raw)); err == nil {
			t.Errorf("lenient decode accepted %s", raw)
		}
		if _, err := DecodeGitPATScopeStrict(json.RawMessage(raw)); err == nil {
			t.Errorf("strict decode accepted %s", raw)
		}
	}
}

func TestDecodeGitPATScopeStrict(t *testing.T) {
	const base = `"host":"h","secret_name":"s"`
	for _, tc := range []struct {
		name, raw string
		wantErr   string // substring; empty = accepted
	}{
		{"minimal", `{` + base + `}`, ""},
		{"username", `{` + base + `,"username":"u"}`, ""},
		{"all axes", `{` + base + `,"repos":["team/app","group/*"],"access":"read","forge":"gitlab"}`, ""},
		{"empty repos is allowed", `{` + base + `,"repos":[]}`, ""},
		{"unknown key", `{` + base + `,"repo":["a/b"]}`, `unknown field "repo"`},
		{"unknown key naming a case variant", `{` + base + `,"Repos":["a/b"],"extra":1}`, `unknown field "extra"`},
		{"access out of enum", `{` + base + `,"access":"admin"}`, "access"},
		{"access wrong case", `{` + base + `,"access":"Read"}`, "access"},
		{"forge out of enum", `{` + base + `,"forge":"svn"}`, "forge"},
		{"empty repos entry", `{` + base + `,"repos":[""]}`, "malformed"},
		{"dotdot entry", `{` + base + `,"repos":["a/../b"]}`, "malformed"},
		{"query entry", `{` + base + `,"repos":["a/b?x"]}`, "malformed"},
		{"fragment entry", `{` + base + `,"repos":["a/b#x"]}`, "malformed"},
		{"backslash entry", `{` + base + `,"repos":["a\\b"]}`, "malformed"},
		{"control entry", `{` + base + `,"repos":["a/b\u0001"]}`, "malformed"},
		{"double wildcard", `{` + base + `,"repos":["a/*/*"]}`, "malformed"},
		{"api on generic", `{` + base + `,"api":true}`, "generic"},
		{"api on gitlab", `{` + base + `,"api":true,"forge":"gitlab"}`, ""},
		{"api on gitea", `{` + base + `,"api":true,"forge":"gitea"}`, ""},
		{"api false is fine", `{` + base + `,"api":false}`, ""},
		{"trailing data", `{` + base + `} {}`, "unexpected data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeGitPATScopeStrict(json.RawMessage(tc.raw))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("accepted, want an error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
