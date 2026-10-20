// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/gitpack"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// chmodX makes a work-tree file executable on disk, where the commit's add -A reads it.
func chmodX(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.Chmod(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
}

func newExecSpec(on bool) types.RunPolicySpec {
	return types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyNewExecutables: on}}
}

// newExecRun is a governed run whose repository holds tool.sh executable and
// notes.txt not, on main.
func newExecRun(t *testing.T, patHost string, spec types.RunPolicySpec) *forgeRun {
	t.Helper()
	r := newForgeRunSpec(t, patHost, spec, map[string]string{"tool.sh": "#!/bin/sh\n", "notes.txt": "notes\n"}, nil)
	s := t.TempDir()
	runGit(t, s, "clone", "-q", r.bare, s)
	runGit(t, s, "update-index", "--chmod=+x", "tool.sh")
	runGit(t, s, "commit", "-qm", "executable")
	runGit(t, s, "push", "-q", "origin", "HEAD:refs/heads/main")
	runGit(t, r.work, "pull", "-q", "--ff-only")
	return r
}

// TestPushRulesDenyNewExecutables drives real pushes through the GitHub App lane,
// whose forge the broker can read: an added executable and a file made
// executable are refused, an edit to a file that was executable already passes.
func TestPushRulesDenyNewExecutables(t *testing.T) {
	cases := []struct {
		name    string
		change  func(t *testing.T, r *forgeRun)
		refused string // the path the forge must not hold, "" when the push passes
	}{
		{"an added executable is refused", func(t *testing.T, r *forgeRun) {
			writeFiles(t, r.work, map[string]string{"bin/new.sh": "#!/bin/sh\n"})
			chmodX(t, r.work, "bin/new.sh")
		}, "bin/new.sh"},
		{"a file made executable is refused", func(t *testing.T, r *forgeRun) {
			chmodX(t, r.work, "notes.txt")
		}, ""},
		{"an edit to an executable passes", func(t *testing.T, r *forgeRun) {
			writeFiles(t, r.work, map[string]string{"tool.sh": "#!/bin/sh\necho edited\n"})
			chmodX(t, r.work, "tool.sh")
		}, ""},
		{"an added plain file passes", func(t *testing.T, r *forgeRun) {
			writeFiles(t, r.work, map[string]string{"docs/a.md": "a\n"})
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newExecRun(t, "", newExecSpec(true))
			c.change(t, r)
			r.commit(t, c.name)
			out, err := r.push()
			switch {
			case c.refused != "":
				r.wantRefused(t, out, err, c.refused)
			case strings.HasPrefix(c.name, "a file made executable"):
				if err == nil || r.landed(t) {
					t.Fatalf("git push of a chmod +x landed, want a refusal\n%s", out)
				}
				if want := "  notes.txt\n"; !strings.Contains(r.lastRefusal(), want) {
					t.Errorf("refusal %q does not name notes.txt", r.lastRefusal())
				}
			default:
				r.wantAllowed(t, out, err)
			}
		})
	}
	// The second commit is diffed against the first, which the pack carries, so
	// the refusal comes from the pack alone.
	t.Run("an executable added in the second of two commits is refused", func(t *testing.T) {
		r := newExecRun(t, "", newExecSpec(true))
		writeFiles(t, r.work, map[string]string{"a.txt": "a\n"})
		r.commit(t, "one")
		writeFiles(t, r.work, map[string]string{"b.sh": "#!/bin/sh\n"})
		chmodX(t, r.work, "b.sh")
		r.commit(t, "two")
		out, err := r.push()
		r.wantRefused(t, out, err, "b.sh")
	})
	t.Run("false lets an added executable through", func(t *testing.T) {
		r := newExecRun(t, "", newExecSpec(false))
		writeFiles(t, r.work, map[string]string{"new.sh": "#!/bin/sh\n"})
		chmodX(t, r.work, "new.sh")
		r.commit(t, "add")
		out, err := r.push()
		r.wantAllowed(t, out, err)
	})
}

// TestPushRulesDenyNewExecutablesRefusalText pins both refusals word for word:
// an executable the pack shows is new, and on a lane whose forge cannot be read,
// an edit to an executable the broker cannot show was executable before.
func TestPushRulesDenyNewExecutablesRefusalText(t *testing.T) {
	const headline = "wardyn: this push is refused by the run's push content rules: new executable files\n"
	t.Run("added", func(t *testing.T) {
		r := newExecRun(t, "", newExecSpec(true))
		writeFiles(t, r.work, map[string]string{"x.txt": "x\n"})
		r.commit(t, "one")
		writeFiles(t, r.work, map[string]string{"new.sh": "#!/bin/sh\n"})
		chmodX(t, r.work, "new.sh")
		r.commit(t, "two")
		if _, err := r.push(); err == nil {
			t.Fatal("git push succeeded, want a refusal")
		}
		want := headline + "  new.sh\n" +
			"clear the executable bit on these files (git update-index --chmod=-x), " +
			"or ask an operator to set push_rules.deny_new_executables to false\n"
		if got := r.lastRefusal(); got != want {
			t.Errorf("refusal =\n%q\nwant\n%q", got, want)
		}
	})
	t.Run("not GitHub", func(t *testing.T) {
		r := newExecRun(t, "gitlab.com", newExecSpec(true))
		writeFiles(t, r.work, map[string]string{"tool.sh": "#!/bin/sh\necho edited\n"})
		chmodX(t, r.work, "tool.sh")
		r.commit(t, "edit")
		if _, err := r.push(); err == nil {
			t.Fatal("git push succeeded, want a refusal")
		}
		want := headline + "  tool.sh\n" + whyNotGitHub + "\n" +
			"clear the executable bit on these files (git update-index --chmod=-x), " +
			"or ask an operator to set push_rules.deny_new_executables to false. " +
			"Wardyn can't read this repository's history here, so it can't tell which files are new.\n"
		if got := r.lastRefusal(); got != want {
			t.Errorf("refusal =\n%q\nwant\n%q", got, want)
		}
	})
}

func TestPushRulesNewExecutablesClaim(t *testing.T) {
	ch := func(path, mode, base string) gitpack.Change {
		return gitpack.Change{Path: path, Mode: mode, BaseMode: base}
	}
	hits, unknown, err := newExecutables([]gitpack.Change{
		ch("added.sh", "100755", gitpack.ModeAbsent),
		ch("chmod.sh", "100755", "100644"),
		ch("was-a-dir.sh", "100755", "40000"),
		ch("edited.sh", "100755", "100755"),
		ch("no-base.sh", "100755", ""),
		ch("plain.txt", "100644", gitpack.ModeAbsent),
		ch("link", "120000", gitpack.ModeAbsent),
		ch("sub", "160000", gitpack.ModeAbsent),
		ch("dir", gitpack.ModeUncarried, ""),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"added.sh", "chmod.sh", "was-a-dir.sh"}; !reflect.DeepEqual(hits, want) {
		t.Errorf("hits = %v, want %v", hits, want)
	}
	var got []string
	for _, c := range unknown {
		got = append(got, c.Path)
	}
	if want := []string{"no-base.sh", "dir"}; !reflect.DeepEqual(got, want) {
		t.Errorf("unknown = %v, want %v", got, want)
	}
}

// recordedExecPush records a first push whose one file, new.sh, is executable.
func recordedExecPush(t *testing.T, ref string) []byte {
	t.Helper()
	f := forgeOn(t, httptest.NewServer)
	bare := filepath.Join(f.root, "octocat", "hello-world.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.root, "init", "-q", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGit(t, work, "init", "-q", "--initial-branch=main", work)
	writeFiles(t, work, map[string]string{"new.sh": "#!/bin/sh\n"})
	runGit(t, work, "add", "new.sh")
	runGit(t, work, "update-index", "--chmod=+x", "new.sh")
	runGit(t, work, "commit", "-qm", "add")
	runGit(t, work, "push", "-q", f.srv.URL+"/octocat/hello-world.git", "HEAD:"+ref)
	return f.pushBody()
}

// TestPushRulesDenyNewExecutablesOffIsToday: false compiles to exactly what an
// absent field does, so a push the rule refuses goes onward byte for byte.
func TestPushRulesDenyNewExecutablesOffIsToday(t *testing.T) {
	if compilePushRules(&types.PushRulesSpec{DenyNewExecutables: false}) != nil {
		t.Error("deny_new_executables false compiles to a rule")
	}
	without := types.PushRulesSpec{DenyPaths: []string{"nothing/**"}}
	withFalse, withTrue := without, without
	withTrue.DenyNewExecutables = true
	if !reflect.DeepEqual(compilePushRules(&withFalse), compilePushRules(&without)) {
		t.Error("deny_new_executables false compiles differently from an absent field")
	}
	for _, c := range []struct {
		spec types.PushRulesSpec
		want int
	}{{withTrue, http.StatusForbidden}, {withFalse, http.StatusOK}} {
		up := newGitBrokerUpstream(t, "gh-inst-token")
		p, _ := newGitBrokerProxyWithSpec(t, map[string]uuid.UUID{"octocat/hello-world": uuid.New()},
			upstreamAddr(up.srv), types.RunPolicySpec{PushRules: &c.spec})
		body := recordedExecPush(t, BranchNSPrefix(p.runID)+"work")
		rec := postPush(t, p, string(body))
		if rec.Code != c.want {
			t.Errorf("deny_new_executables %v: status = %d, want %d (body %q)", c.spec.DenyNewExecutables, rec.Code, c.want, rec.Body.String())
		}
		if c.want == http.StatusOK && string(up.gitBody) != string(body) {
			t.Error("deny_new_executables false did not forward the push verbatim")
		}
	}
}

// TestADORESTPushRulesDenyNewExecutables: a REST push names no modes, and a
// rename carries a file's mode to a new path, so with the rule on the REST door
// refuses a content push it would otherwise pass, and off it passes as before.
func TestADORESTPushRulesDenyNewExecutables(t *testing.T) {
	caps := []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapCodeWrite}
	h, _ := newADORESTRules(t, newExecSpec(true), types.ApprovalApproved, caps...)
	rec := h.do(t, http.MethodPost, restPushTarget, restPush(h.branch(), "/src/main.go"), nil)
	if rec.Code != http.StatusForbidden || len(h.fake.Requests()) != 0 ||
		!strings.Contains(rec.Body.String(), "does not say which files it leaves executable") {
		t.Fatalf("status = %d body %s upstream %d; want a 403 naming why and nothing forwarded",
			rec.Code, rec.Body.String(), len(h.fake.Requests()))
	}
	h, _ = newADORESTRules(t, newExecSpec(false), types.ApprovalApproved, caps...)
	if rec := h.do(t, http.MethodPost, restPushTarget, restPush(h.branch(), "/src/main.go"), nil); rec.Code/100 != 2 {
		t.Fatalf("status = %d body %s, want it forwarded as before", rec.Code, rec.Body.String())
	}
}
