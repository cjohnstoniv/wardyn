// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/adler32"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/gitpack"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// contentRulesSpec is a run policy carrying nothing but the content rules
// under test.
func contentRulesSpec(deny ...string) types.RunPolicySpec {
	return types.RunPolicySpec{PushRules: &types.PushRulesSpec{DenyPaths: deny}}
}

// recordedPush drives a real `git push` at a throwaway forge over plain HTTP
// and returns the git-receive-pack REQUEST BODY git produced.
//
// The packs below are therefore ones git actually built, not hand-framed
// bytes: the whole question this feature answers is what a real push contains,
// and a fixture that encoded the answer would prove nothing about it.
func recordedPush(t *testing.T, ref string, files map[string]string) []byte {
	t.Helper()
	f := forgeOn(t, httptest.NewServer)
	bare := filepath.Join(f.root, "octocat", "hello-world.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.root, "init", "-q", "--bare", "--initial-branch=main", bare)

	work := t.TempDir()
	runGit(t, work, "init", "-q", "--initial-branch=main", work)
	writeFiles(t, work, files)
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-qm", "work")
	runGit(t, work, "push", "-q", f.srv.URL+"/octocat/hello-world.git", "HEAD:"+ref)

	body := f.pushBody()
	if len(body) == 0 {
		t.Fatal("the forge received no receive-pack request")
	}
	return body
}

// recordedThinPush records the body a SHALLOW clone's push sends when nothing
// advertised no-thin: the delta bases stayed on the forge, so the request
// cannot be read on its own. It is the shape #178 exists to prevent and the
// one an ignore-the-advertisement client can still produce.
func recordedThinPush(t *testing.T, ref string) []byte {
	t.Helper()
	f := forgeOn(t, httptest.NewServer)
	bare := filepath.Join(f.root, "octocat", "hello-world.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, f.root, "init", "-q", "--bare", "--initial-branch=main", bare)

	seed := t.TempDir()
	runGit(t, seed, "init", "-q", "--initial-branch=main", seed)
	writeFiles(t, seed, map[string]string{"data.txt": bigFile(4000)})
	runGit(t, seed, "add", "data.txt")
	runGit(t, seed, "commit", "-qm", "seed")
	runGit(t, seed, "push", "-q", bare, "HEAD:refs/heads/main")

	repoURL := f.srv.URL + "/octocat/hello-world.git"
	work := filepath.Join(t.TempDir(), "work")
	runGit(t, t.TempDir(), "clone", "-q", "--depth", "1", repoURL, work)
	writeFiles(t, work, map[string]string{
		"data.txt": strings.Replace(bigFile(4000), "line 02000:", "line 02000! edited", 1),
	})
	runGit(t, work, "commit", "-qam", "edit one line")
	runGit(t, work, "push", "-q", repoURL, "HEAD:"+ref)

	body := f.pushBody()
	if _, err := gitpack.Inspect(body); !errors.Is(err, gitpack.ErrUninspectable) {
		t.Fatalf("recorded push inspects as %v, want ErrUninspectable — "+
			"if git stopped sending thin packs by default this fixture proves nothing", err)
	}
	return body
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// postPush replays a recorded receive-pack body at the App-lane broker.
func postPush(t *testing.T, p *Proxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
		"/wardyn/gh/octocat/hello-world/git-receive-pack", strings.NewReader(body)))
	return rec
}

// TestPushRulesRefuseADeniedPath is the feature: a push that edits a workflow
// file under a policy that denies workflow files is refused, the offending
// path is named to the person, and nothing else about the push is.
func TestPushRulesRefuseADeniedPath(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, sink := newGitBrokerProxyWithSpec(t,
		map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv),
		contentRulesSpec(".github/workflows/**"))

	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", map[string]string{
		".github/workflows/ci.yml": "on: push\n",
		"README.md":                "hello\n",
	})
	rec := postPush(t, p, string(body))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ".github/workflows/ci.yml") {
		t.Errorf("refusal does not name the offending path: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "README.md") {
		t.Errorf("refusal names a path no rule matched: %q", rec.Body.String())
	}
	if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitRules+`"`) {
		t.Errorf("decision log = %q, want a %s deny row", sink.String(), ruleSourceGitRules)
	}
	if up.mintCalls != 0 || up.gitHits != 0 {
		t.Errorf("a refused push minted %d credentials and reached the forge %d times, want 0 and 0",
			up.mintCalls, up.gitHits)
	}
}

// TestPushRulesEnterIndependentlyOfBranchConfinement is the failure this issue
// was most likely to ship: content rules wired INSIDE the branch-namespace
// block would let either WHERE opt-out — the per-run git_push_any_branch or
// the deployment-wide env switch — silently disable them. A WHERE opt-out must
// never switch off a WHAT control, so both cases must still be refused.
func TestPushRulesEnterIndependentlyOfBranchConfinement(t *testing.T) {
	cases := []struct {
		name string
		spec types.RunPolicySpec
		env  string // WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS, "" = leave default
	}{
		{name: "git_push_any_branch opts out of WHERE only",
			spec: types.RunPolicySpec{
				GitPushAnyBranch: true,
				PushRules:        &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}},
			}},
		{name: "the deployment-wide switch opts out of WHERE only",
			spec: contentRulesSpec(".github/workflows/**"), env: "false"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.env != "" {
				t.Setenv(envEnforceBranchNS, c.env)
			}
			up := newGitBrokerUpstream(t, "gh-inst-token")
			p, sink := newGitBrokerProxyWithSpec(t,
				map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv), c.spec)

			// refs/heads/main is OUTSIDE the run's namespace on purpose: the
			// push is one branch confinement would have refused and this
			// posture forwards, which is exactly when content rules are the
			// only control left.
			body := recordedPush(t, "refs/heads/main", map[string]string{
				".github/workflows/ci.yml": "on: push\n",
			})
			rec := postPush(t, p, string(body))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: a WHERE opt-out disabled a WHAT control (body %q)",
					rec.Code, rec.Body.String())
			}
			if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitRules+`"`) {
				t.Errorf("decision log = %q, want a %s deny row", sink.String(), ruleSourceGitRules)
			}
			if up.mintCalls != 0 {
				t.Errorf("a refused push minted %d credentials, want 0", up.mintCalls)
			}
		})
	}
}

// TestPushRulesRefuseAnOversizePush pins the ceiling AND its boundary: the
// buffer is the ceiling plus one byte, so a body exactly at the ceiling is not
// the too-large refusal — it fails on what it is, not on how big it is.
func TestPushRulesRefuseAnOversizePush(t *testing.T) {
	const mib = 1 << 20
	spec := types.RunPolicySpec{
		PushRules: &types.PushRulesSpec{DenyPaths: []string{"x"}, MaxInspectPackMiB: 1},
	}
	cases := []struct {
		name string
		size int
		want int
		src  string
	}{
		{"one byte over the ceiling", mib + 1, http.StatusRequestEntityTooLarge, ruleSourceGitPackBig},
		{"exactly at the ceiling", mib, http.StatusUnsupportedMediaType, ruleSourceGitPackBlind},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := newGitBrokerUpstream(t, "gh-inst-token")
			p, sink := newGitBrokerProxyWithSpec(t,
				map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv), spec)

			// A real, in-namespace command section, so branch confinement
			// passes and the SIZE is the only thing left to refuse on. The
			// ceiling covers the whole buffered request, command section
			// included, because that is what the inspector is handed.
			section := pkt(someOID+" "+otherOID+" "+BranchNSPrefix(p.runID)+"work"+firstCaps) + "0000"
			rec := postPush(t, p, section+strings.Repeat("A", c.size-len(section)))

			if rec.Code != c.want {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, c.want, rec.Body.String())
			}
			if !strings.Contains(sink.String(), `"rule_source":"`+c.src+`"`) {
				t.Errorf("decision log = %q, want a %s deny row", sink.String(), c.src)
			}
			if up.mintCalls != 0 || up.gitHits != 0 {
				t.Errorf("a refused push minted %d credentials and reached the forge %d times",
					up.mintCalls, up.gitHits)
			}
		})
	}
}

// TestPushRulesRefuseWhatCannotBeInspected covers the three ways the inspector
// declines to answer: a real thin pack from a shallow clone, a ref update with
// no pack behind it, and a body in a content-coding this proxy cannot read
// past. The encoded case runs with git_push_any_branch set so confinePush's
// own encoding refusal is out of the way and this step is the only thing left.
func TestPushRulesRefuseWhatCannotBeInspected(t *testing.T) {
	anyBranch := types.RunPolicySpec{
		GitPushAnyBranch: true,
		PushRules:        &types.PushRulesSpec{DenyPaths: []string{".github/workflows/**"}},
	}
	t.Run("a thin pack from a shallow clone", func(t *testing.T) {
		up := newGitBrokerUpstream(t, "gh-inst-token")
		p, sink := newGitBrokerProxyWithSpec(t,
			map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv), anyBranch)

		rec := postPush(t, p, string(recordedThinPush(t, "refs/heads/topic")))

		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415 (body %q)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPackBlind+`"`) {
			t.Errorf("decision log = %q, want a %s deny row", sink.String(), ruleSourceGitPackBlind)
		}
		if up.mintCalls != 0 {
			t.Errorf("a refused push minted %d credentials, want 0", up.mintCalls)
		}
	})

	t.Run("a ref update with no packfile", func(t *testing.T) {
		up := newGitBrokerUpstream(t, "gh-inst-token")
		p, sink := newGitBrokerProxyWithSpec(t,
			map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv), anyBranch)

		rec := postPush(t, p, pkt(someOID+" "+otherOID+" refs/heads/main"+firstCaps)+"0000")

		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415 (body %q)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPackBlind+`"`) {
			t.Errorf("decision log = %q, want a %s deny row", sink.String(), ruleSourceGitPackBlind)
		}
		if up.mintCalls != 0 {
			t.Errorf("a refused push minted %d credentials, want 0", up.mintCalls)
		}
	})

	t.Run("a body in a content-coding", func(t *testing.T) {
		up := newGitBrokerUpstream(t, "gh-inst-token")
		p, sink := newGitBrokerProxyWithSpec(t,
			map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv), anyBranch)

		rec := httptest.NewRecorder()
		req := mustLocalReq(t, http.MethodPost,
			"/wardyn/gh/octocat/hello-world/git-receive-pack", strings.NewReader("compressed"))
		req.Header.Set("Content-Encoding", "gzip")
		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415 (body %q)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "gzip-encoded push body") {
			t.Errorf("refusal = %q, want it worded like the branch-namespace encoding refusal", rec.Body.String())
		}
		if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPackBlind+`"`) {
			t.Errorf("decision log = %q, want a %s deny row", sink.String(), ruleSourceGitPackBlind)
		}
		if up.mintCalls != 0 {
			t.Errorf("a refused push minted %d credentials, want 0", up.mintCalls)
		}
	})
}

// storedZlib frames payload as one uncompressed ("stored") deflate block, so
// 200,001 objects build in a fraction of a second rather than 200,001
// compressor runs. payload must be under 64 KiB. Copied from
// internal/gitpack/pack_limits_test.go rather than shared across packages.
func storedZlib(payload []byte) []byte {
	n := uint16(len(payload))
	out := []byte{0x78, 0x01, 0x01, byte(n), byte(n >> 8), byte(^n), byte(^n >> 8)}
	out = append(out, payload...)
	return binary.BigEndian.AppendUint32(out, adler32.Checksum(payload))
}

// packObjHeader is git's pack object header: type in the top 3 bits, size
// base-128 from there. Copied from internal/gitpack/pack_test.go's objHeader.
func packObjHeader(typ byte, size int64) []byte {
	b := []byte{typ<<4 | byte(size&0x0f)}
	for size >>= 4; size > 0; size >>= 7 {
		b[len(b)-1] |= 0x80
		b = append(b, byte(size&0x7f))
	}
	return b
}

func gitObjectID(typ string, payload []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d", typ, len(payload))
	h.Write([]byte{0})
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// oversizeObjectCountPush is issue #250's own fixture, one object past
// internal/gitpack's maxObjects ceiling (200,000): a commit, its empty tree
// (so the walker would report no paths even if it ran), and n-2 distinct
// 4-byte blobs the tree names don't cover. gitpack.Inspect refuses it with
// ErrTooLarge before any tree is walked, which is what this test is for —
// distinguishing that refusal from ErrUninspectable at the proxy.
func oversizeObjectCountPush(ref string, n int) []byte {
	const objCommit, objTree, objBlob byte = 1, 2, 3
	tree := gitObjectID("tree", nil)
	commit := fmt.Appendf(nil, "tree %s\nauthor T <t@example.com> 1767225600 +0000\n"+
		"committer T <t@example.com> 1767225600 +0000\n\nx\n", tree)
	pack := []byte("PACK")
	pack = binary.BigEndian.AppendUint32(pack, 2)
	pack = binary.BigEndian.AppendUint32(pack, uint32(n))
	pack = append(pack, packObjHeader(objCommit, int64(len(commit)))...)
	pack = append(pack, storedZlib(commit)...)
	pack = append(pack, packObjHeader(objTree, 0)...)
	pack = append(pack, storedZlib(nil)...)
	for i := range n - 2 {
		payload := binary.BigEndian.AppendUint32(nil, uint32(i))
		pack = append(pack, packObjHeader(objBlob, int64(len(payload)))...)
		pack = append(pack, storedZlib(payload)...)
	}
	sum := sha1.Sum(pack)
	cmd := zeroOID + " " + gitObjectID("commit", commit) + " " + ref + firstCaps
	return append([]byte(pkt(cmd)+"0000"), append(pack, sum[:]...)...)
}

// TestPushRulesRefuseAGitpackCeiling covers the ceiling gitpack.Inspect hits
// on a pack it can otherwise read in full — as opposed to
// TestPushRulesRefuseWhatCannotBeInspected's thin packs and missing refs,
// which it cannot read at all. Both used to be wrapped in the same
// --unshallow hint and filed under the same rule source, which told the
// person the wrong fix for this one: an honest push over a size ceiling is
// fixed by pushing fewer commits, not by a more complete clone.
func TestPushRulesRefuseAGitpackCeiling(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, sink := newGitBrokerProxyWithSpec(t,
		map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv),
		contentRulesSpec("nonexistent/**"))

	body := oversizeObjectCountPush(BranchNSPrefix(p.runID)+"work", 200_001)
	rec := postPush(t, p, string(body))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "push fewer commits") {
		t.Errorf("refusal = %q, want the push-fewer-commits hint, not the --unshallow one",
			rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "unshallow") {
		t.Errorf("refusal = %q, tells the person to reclone for a push that was fully read",
			rec.Body.String())
	}
	if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPackBig+`"`) {
		t.Errorf("decision log = %q, want a %s deny row", sink.String(), ruleSourceGitPackBig)
	}
	if up.mintCalls != 0 || up.gitHits != 0 {
		t.Errorf("a refused push minted %d credentials and reached the forge %d times, want 0 and 0",
			up.mintCalls, up.gitHits)
	}
}

// TestPushRulesForwardAnAllowedPushUnchanged: the buffered bytes go onward
// byte for byte, and this POST mints only once the push has passed. A real
// push's discovery request mints before it (see
// TestPushRulesDiscoveryMintsBeforeAnyRefusal).
func TestPushRulesForwardAnAllowedPushUnchanged(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, _ := newGitBrokerProxyWithSpec(t,
		map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv),
		contentRulesSpec(".github/workflows/**"))

	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", map[string]string{"README.md": "hello\n"})
	rec := postPush(t, p, string(body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if string(up.gitBody) != string(body) {
		t.Errorf("the forge received %d bytes, want the %d buffered bytes verbatim", len(up.gitBody), len(body))
	}
	if up.mintCalls != 1 {
		t.Errorf("mintCalls = %d, want 1", up.mintCalls)
	}
}

// TestPushRulesAbsentBehaveExactlyAsToday: a run with no content rules buffers
// nothing and forwards what it always forwarded, workflow file and all.
func TestPushRulesAbsentBehaveExactlyAsToday(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, _ := newGitBrokerProxy(t,
		map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv))

	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", map[string]string{
		".github/workflows/ci.yml": "on: push\n",
	})
	rec := postPush(t, p, string(body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if string(up.gitBody) != string(body) {
		t.Error("a run with no content rules did not forward the push verbatim")
	}
}

// TestPushRulesBodyNamesAtMostTenPaths: the person is told what to fix without
// the refusal becoming the push itself, and the count is honest about the rest.
func TestPushRulesBodyNamesAtMostTenPaths(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, _ := newGitBrokerProxyWithSpec(t,
		map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv),
		contentRulesSpec("**"))

	files := map[string]string{}
	for i := range 25 {
		files[fmt.Sprintf("f%02d.txt", i)] = "x\n"
	}
	rec := postPush(t, p, string(recordedPush(t, BranchNSPrefix(p.runID)+"work", files)))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	if n := strings.Count(rec.Body.String(), ".txt"); n != maxDeniedPathsInBody {
		t.Errorf("refusal names %d paths, want %d: %q", n, maxDeniedPathsInBody, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "and 15 more") {
		t.Errorf("refusal does not state how many more matched: %q", rec.Body.String())
	}
}

// TestPushRulesKeepPathsOutOfTheDecisionLog: the decision stream is SIEM-fanned
// and its free-text fields (cause, via) are reserved for dial-shaped refusals.
// An offending path belongs in the structured log and the response body, and
// nowhere in the row.
func TestPushRulesKeepPathsOutOfTheDecisionLog(t *testing.T) {
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, sink := newGitBrokerProxyWithSpec(t,
		map[string]uuid.UUID{"octocat/hello-world": uuid.New()}, upstreamAddr(up.srv),
		contentRulesSpec("secrets/**"))

	rec := postPush(t, p, string(recordedPush(t, BranchNSPrefix(p.runID)+"work",
		map[string]string{"secrets/prod.env": "TOKEN=1\n"})))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	row := sink.String()
	if strings.Contains(row, "secrets/prod.env") {
		t.Errorf("the decision log carries the offending path: %q", row)
	}
	for _, field := range []string{`"cause"`, `"via"`} {
		if strings.Contains(row, field) {
			t.Errorf("the decision log carries %s, a field reserved for dial-shaped refusals: %q", field, row)
		}
	}
}

// TestPushRulesOnTheTokenLane: the git_pat lane enforces the same rules under
// the same trigger, with its branch-namespace switch at its default OFF — the
// asymmetry this issue had to resolve. Content rules are not a WHERE control
// and do not wait on one.
func TestPushRulesOnTheTokenLane(t *testing.T) {
	if PATBranchNSEnforced() {
		t.Fatal("WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS is set in this environment; " +
			"this test's premise is that the lane's WHERE switch is OFF")
	}
	up := newPATBrokerUpstream(t, "pat-token", "oauth2")
	p, sink := newPATBrokerProxySpec(t, contentRulesSpec(".github/workflows/**"),
		map[string]PATGrant{"gitlab.com": {GrantID: uuid.New(), Username: "oauth2"}},
		upstreamAddr(up.srv))

	body := recordedPush(t, "refs/heads/main", map[string]string{
		".github/workflows/ci.yml": "on: push\n",
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
		"/wardyn/git/gitlab.com/org/repo.git/git-receive-pack", strings.NewReader(string(body))))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitRules+`"`) {
		t.Errorf("decision log = %q, want a %s deny row", sink.String(), ruleSourceGitRules)
	}
	if up.mintCalls != 0 || up.gitHits != 0 {
		t.Errorf("a refused push minted %d PATs and reached the forge %d times", up.mintCalls, up.gitHits)
	}
}

// TestPushRulesAdvertiseNoThinOnTheTokenLane is the other half of that answer:
// a lane that enforces content rules must ask for a pack it can read, or its
// rules refuse every push from the shallow clone the agent images make.
func TestPushRulesAdvertiseNoThinOnTheTokenLane(t *testing.T) {
	for _, c := range []struct {
		name string
		spec types.RunPolicySpec
		want bool
	}{
		{"content rules set", contentRulesSpec(".github/workflows/**"), true},
		{"no content rules", types.RunPolicySpec{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			up := newAdvertUpstream(t, advert(realCaps))
			// The advert upstream answers the mint route with a github_token
			// body; patToken only needs a token and an expiry from it.
			p, _ := newPATBrokerProxySpec(t, c.spec,
				map[string]PATGrant{"gitlab.com": {GrantID: uuid.New(), Username: "oauth2"}},
				upstreamAddr(up.srv))

			rec := httptest.NewRecorder()
			req := mustLocalReq(t, http.MethodGet,
				"/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-receive-pack", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
			}
			if got := strings.Contains(rec.Body.String(), " no-thin"); got != c.want {
				t.Errorf("no-thin advertised = %v, want %v (body %q)", got, c.want, rec.Body.String())
			}
			wantAE := "gzip"
			if c.want {
				wantAE = "identity"
			}
			if got := up.acceptEncoding(); got != wantAE {
				t.Errorf("forge saw Accept-Encoding %q, want %q", got, wantAE)
			}
		})
	}
}

// TestPushRulesGlobMatching pins the pattern language: ** crosses separators,
// * does not, and a pattern is anchored at the repository root.
func TestPushRulesGlobMatching(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{".github/workflows/**", ".github/workflows/ci.yml", true},
		{".github/workflows/**", ".github/workflows/nested/ci.yml", true},
		{".github/workflows/**", ".github/dependabot.yml", false},
		{"/infra/**", "infra/main.tf", true}, // a leading separator is trimmed
		{"infra/**", "infrastructure/main.tf", false},
		{"*.pem", "server.pem", true},
		{"*.pem", "certs/server.pem", false},
		{"**/*.pem", "certs/a/server.pem", true},
		{"**/*.pem", "server.pem", true},
		{"Makefile", "Makefile", true},
		{"Makefile", "sub/Makefile", false},
		{"**", "anything/at/all", true},
		{"a/**/z", "a/z", true},
		{"a/**/z", "a/b/c/z", true},
		{"a/**/z", "a/b/c", false},
		{"co[nfig.yml", "co[nfig.yml", true}, // not a Go pattern: compared literally
		{"infra/", "infra/main.tf", true},    // a trailing separator means everything beneath
		{"infra/", "infra/prod/main.tf", true},
		{"infra/", "infrastructure/main.tf", false},
	}
	for _, c := range cases {
		t.Run(c.pattern+" vs "+c.path, func(t *testing.T) {
			rs := compilePushRules(&types.PushRulesSpec{DenyPaths: []string{c.pattern}})
			_, total, _, err := rs.match([]gitpack.Change{{Path: c.path, Mode: "100644"}})
			if err != nil {
				t.Fatalf("match: %v", err)
			}
			if got := total > 0; got != c.want {
				t.Errorf("matched = %v, want %v", got, c.want)
			}
		})
	}
}

// TestPushRulesBoundTheirOwnWork is the hostile case #265's removal of the
// deny_paths count cap left reachable: a member authoring a deny list as long
// as a 1 MiB policy body allows, against a push carrying as many changed paths
// as the inspector allows. The matcher must refuse rather than grind — an
// unevaluated rule may not read as a pass — and it must do so in bounded time.
func TestPushRulesBoundTheirOwnWork(t *testing.T) {
	deny := make([]string, 131_000)
	for i := range deny {
		deny[i] = fmt.Sprintf("vendor/pkg%06d/**", i)
	}
	rs := compilePushRules(&types.PushRulesSpec{DenyPaths: deny})
	// An opaque entry is matched twice over — itself, then what could lie
	// beneath it — so it is bounded on the same budget or not at all.
	for _, mode := range []string{"100644", gitpack.ModeUncarried} {
		t.Run(mode, func(t *testing.T) {
			changes := make([]gitpack.Change, 200_000)
			for i := range changes {
				changes[i] = gitpack.Change{Path: fmt.Sprintf("src/mod%06d/file.go", i), Mode: mode}
			}
			start := time.Now()
			_, _, _, err := rs.match(changes)
			elapsed := time.Since(start)

			if !errors.Is(err, errGlobBudget) {
				t.Fatalf("match = %v after %s, want the work ceiling to refuse", err, elapsed)
			}
			if elapsed > 60*time.Second {
				t.Errorf("the bounded matcher took %s, which is not a bound anyone would call one", elapsed)
			}
		})
	}
}

// TestPushRulesDefaultInspectionCeilingLeavesRoomToRaise: raising
// max_inspect_pack_mib is the stated remedy for a too-large refusal, so the
// default an unset field gets has to sit below what an operator may author.
func TestPushRulesDefaultInspectionCeilingLeavesRoomToRaise(t *testing.T) {
	rs := compilePushRules(&types.PushRulesSpec{DenyPaths: []string{"x"}})
	if rs.inspectMax != int64(defaultInspectPackMiB)<<20 {
		t.Errorf("default ceiling = %d bytes, want %d MiB", rs.inspectMax, defaultInspectPackMiB)
	}
	// The bound internal/api's validatePushRules admits.
	if defaultInspectPackMiB >= 64 {
		t.Errorf("default ceiling %d MiB is not below the 64 MiB an operator may author, "+
			"so the remedy for a too-large refusal does not exist", defaultInspectPackMiB)
	}
	if compilePushRules(&types.PushRulesSpec{}) != nil {
		t.Error("an all-zero push_rules compiled to a rule set; it must read as absent")
	}
	if compilePushRules(nil) != nil {
		t.Error("a nil push_rules compiled to a rule set")
	}
}

// pushThroughBroker drives the shape a real governed run takes — a shallow
// clone, one edited file, a push onto the run's own branch — all the way
// through the broker under the given deny list, and reports whether git
// succeeded.
//
// The seed repository is deliberately ordinary: a workflow file in a
// subdirectory, a file at the root, and the source tree the run edits.
func pushThroughBroker(t *testing.T, deny ...string) (string, error) {
	t.Helper()
	forge := newGitForge(t)
	bare := filepath.Join(forge.root, "octocat", "hello-world.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, forge.root, "init", "-q", "--bare", "--initial-branch=main", bare)

	seed := t.TempDir()
	runGit(t, seed, "init", "-q", "--initial-branch=main", seed)
	writeFiles(t, seed, map[string]string{
		".github/workflows/ci.yml": "on: push\n",
		"Makefile":                 "all:\n\techo hi\n",
		"src/app.go":               bigFile(2000),
	})
	runGit(t, seed, "add", "-A")
	runGit(t, seed, "commit", "-qm", "seed")
	runGit(t, seed, "push", "-q", bare, "HEAD:refs/heads/main")

	p, sink := newGitBrokerProxyWithSpec(t,
		map[string]uuid.UUID{"octocat/hello-world": uuid.New()},
		upstreamAddr(forge.srv), contentRulesSpec(deny...))
	broker := httptest.NewServer(p)
	t.Cleanup(broker.Close)

	work := filepath.Join(t.TempDir(), "work")
	runGit(t, t.TempDir(), "clone", "-q", "--depth", "1",
		broker.URL+"/wardyn/gh/octocat/hello-world", work)
	writeFiles(t, work, map[string]string{
		"src/app.go": strings.Replace(bigFile(2000), "line 01000:", "line 01000! edited", 1),
	})
	runGit(t, work, "commit", "-qam", "edit one line")

	cmd := exec.Command("git", "push", "origin", "HEAD:"+BranchNSPrefix(p.runID)+"work")
	cmd.Dir, cmd.Env = work, gitEnv()
	out, err := cmd.CombinedOutput()
	return string(out) + "\n--- decision log ---\n" + sink.String(), err
}

// TestPushRulesSeeWhatThePackCarriesAndNoMore pins what the rules actually
// match against on a real governed run, in BOTH directions, because both are
// load-bearing and neither is obvious.
//
// Under branch-namespace confinement every governed push lands on the run's
// own branch, so the pushed commit's parent stays on the forge and the
// inspector has no pre-image in the pack to diff against: it enumerates the
// new tree instead. A pack carries only objects the forge lacks, so that
// enumeration descends into changed directories and names every file at the
// ROOT, changed or not — and every directory the push did not change arrives
// as a tree the forge already stores, which is exactly what a directory moved
// or restored onto that path looks like.
//
// What the pack carries is judged from the pack: the edited path is refused.
// What it does not carry — the root-level file and the directory this push
// left alone — is compared with the same path in the commit the push builds
// on, read from the forge, and passes because it is unchanged.
func TestPushRulesSeeWhatThePackCarriesAndNoMore(t *testing.T) {
	cases := []struct {
		name      string
		deny      string
		wantRefus bool
	}{
		{"the edited path is matched", "src/**", true},
		{"a file at the root this push did not touch is compared with the base and passes", "Makefile", false},
		{"a directory this push did not carry is compared with the base and passes",
			".github/workflows/**", false},
		{"a pattern that cannot reach into an uncarried directory lets the push through",
			"docs/**", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := pushThroughBroker(t, c.deny)
			if c.wantRefus {
				if err == nil {
					t.Fatalf("git push succeeded under deny %q, want a refusal\n%s", c.deny, out)
				}
				if !strings.Contains(out, `"rule_source":"`+ruleSourceGitRules+`"`) {
					t.Errorf("git push failed for some other reason under deny %q:\n%s", c.deny, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("git push was refused under deny %q, which this push leaves unchanged:\n%s", c.deny, out)
			}
		})
	}
}
