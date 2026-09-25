// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/gitpack"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pushCP is the control plane a held push talks to: it records every raise
// and answers every poll with the state the test sets. Every raise gets the
// same id, which is what the real control plane's PENDING dedup does for one
// push's scope.
type pushCP struct {
	srv *httptest.Server
	id  uuid.UUID

	mu     sync.Mutex
	state  types.ApprovalState
	kinds  []string
	raises []types.PushContentScope
	lists  []*types.PushPathList
	polls  int
}

func newPushCP(t *testing.T, state types.ApprovalState) *pushCP {
	t.Helper()
	c := &pushCP{id: uuid.New(), state: state}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/internal/approvals" {
			var body struct {
				Kind           string              `json:"kind"`
				RequestedScope json.RawMessage     `json:"requested_scope"`
				PathList       *types.PushPathList `json:"path_list"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var scope types.PushContentScope
			dec := json.NewDecoder(bytes.NewReader(body.RequestedScope))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&scope); err != nil || scope.Validate() != nil {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			c.kinds = append(c.kinds, body.Kind)
			c.raises = append(c.raises, scope)
			c.lists = append(c.lists, body.PathList)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: c.id, State: types.ApprovalPending})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/internal/approvals/"+c.id.String() {
			c.polls++
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: c.id, State: c.state})
			return
		}
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *pushCP) set(s types.ApprovalState) { c.mu.Lock(); c.state = s; c.mu.Unlock() }

func (c *pushCP) raised() ([]string, []types.PushContentScope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.kinds...), append([]types.PushContentScope(nil), c.raises...)
}

func (c *pushCP) counts() (raises, polls int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.raises), c.polls
}

// wire points p's approval client at the fake control plane and shortens the
// poll so a hold settles quickly.
func (c *pushCP) wire(t *testing.T, p *Proxy) {
	t.Helper()
	p.approval = newApprovalClient(c.srv.URL, newTokenSource("tok"), p.runID, c.srv.Client())
	prev := holdPollInterval
	holdPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { holdPollInterval = prev })
}

// reviewSpec is a run policy with review rules (and optionally deny rules).
func reviewSpec(holdSeconds int, review []string, deny ...string) types.RunPolicySpec {
	return types.RunPolicySpec{PushRules: &types.PushRulesSpec{
		DenyPaths: deny, RequireReviewPaths: review, HoldSeconds: holdSeconds,
	}}
}

// newAppLaneHold is the App-lane broker with review rules, a fake control
// plane and a forge, for one repo.
func newAppLaneHold(t *testing.T, spec types.RunPolicySpec, state types.ApprovalState) (*Proxy, *bytes.Buffer, *gitBrokerUpstream, *pushCP, uuid.UUID) {
	t.Helper()
	grant := uuid.New()
	up := newGitBrokerUpstream(t, "gh-inst-token")
	p, sink := newGitBrokerProxyWithSpec(t, map[string]uuid.UUID{"octocat/hello-world": grant}, upstreamAddr(up.srv), spec)
	cp := newPushCP(t, state)
	cp.wire(t, p)
	return p, sink, up, cp, grant
}

var workflowPush = map[string]string{".github/workflows/ci.yml": "on: push\n", "README.md": "hello\n"}

// newCommit is the object id the push sets its ref to.
func newCommit(t *testing.T, body []byte) string {
	t.Helper()
	res, err := gitpack.Inspect(body)
	if err != nil || len(res.Commands) != 1 {
		t.Fatalf("inspect recorded push: %v (%d commands)", err, len(res.Commands))
	}
	return res.Commands[0].New
}

// TestPushHoldForwardsOnApprove is the feature: a push touching a review path
// is held, an approval is raised with the wire contract the console renders,
// and once an admin approves it the buffered bytes go to the forge unchanged.
func TestPushHoldForwardsOnApprove(t *testing.T) {
	p, sink, up, cp, grant := newAppLaneHold(t, reviewSpec(5, []string{".github/workflows/**"}), types.ApprovalPending)
	ref := BranchNSPrefix(p.runID) + "work"
	body := recordedPush(t, ref, workflowPush)

	go func() {
		// Decide only once the push is parked: approve after the raise lands.
		for range 400 {
			if n, _ := cp.counts(); n > 0 {
				cp.set(types.ApprovalApproved)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	rec := postPush(t, p, string(body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(up.gitBody, body) {
		t.Errorf("the forge received %d bytes, want the %d held bytes verbatim", len(up.gitBody), len(body))
	}
	kinds, raises := cp.raised()
	if len(raises) != 1 || kinds[0] != string(types.ApprovalPushContent) {
		t.Fatalf("raises = %d kinds %v, want one push_content", len(raises), kinds)
	}
	got := raises[0]
	want := types.PushContentScope{
		Repo:        "github.com/octocat/hello-world",
		Branch:      ref,
		ActsAs:      "github_token:" + grant.String(),
		Paths:       []string{".github/workflows/ci.yml"},
		PathsTotal:  1,
		Commits:     []string{newCommit(t, body)},
		PathsDigest: got.PathsDigest,
	}
	if gj, wj := mustJSON(t, got), mustJSON(t, want); gj != wj {
		t.Errorf("requested_scope =\n %s\nwant\n %s", gj, wj)
	}
	if strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPushHeld) {
		t.Errorf("an approved push wrote a held refusal row: %q", sink.String())
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPushHoldRefusesOnDeny: a denied push never reaches the forge, and the
// denial sticks — the same commits are refused again without a second
// question.
func TestPushHoldRefusesOnDeny(t *testing.T) {
	p, sink, up, cp, _ := newAppLaneHold(t, reviewSpec(5, []string{".github/workflows/**"}), types.ApprovalDenied)
	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)

	rec := postPush(t, p, string(body))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "an admin denied this push") {
		t.Fatalf("status = %d body %q, want 403 naming the denial", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ".github/workflows/ci.yml") {
		t.Errorf("the refusal does not name the path: %q", rec.Body.String())
	}
	if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPushHeld+`"`) {
		t.Errorf("decision log = %q, want a %s row", sink.String(), ruleSourceGitPushHeld)
	}
	if strings.Contains(sink.String(), ".github/workflows") {
		t.Errorf("the decision log carries the path: %q", sink.String())
	}
	raises, polls := cp.counts()

	rec = postPush(t, p, string(body))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("retry of a denied push: status = %d, want 403", rec.Code)
	}
	if r2, p2 := cp.counts(); r2 != raises || p2 != polls {
		t.Errorf("a retry of a denied push asked again (raises %d->%d, polls %d->%d)", raises, r2, polls, p2)
	}
	if up.gitHits != 0 || up.mintCalls != 0 {
		t.Errorf("a denied push reached the forge %d times and minted %d times", up.gitHits, up.mintCalls)
	}
}

// TestPushHoldRefusesOnTimeout: nobody decided within hold_seconds, so the
// push is refused and its row left PENDING; a retry waits on that same row
// rather than raising a second, and is forwarded once it is approved.
func TestPushHoldRefusesOnTimeout(t *testing.T) {
	p, sink, up, cp, _ := newAppLaneHold(t, reviewSpec(1, []string{".github/workflows/**"}), types.ApprovalPending)
	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)

	start := time.Now()
	rec := postPush(t, p, string(body))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "nobody decided within 1s") {
		t.Fatalf("status = %d body %q, want 403 naming the timeout", rec.Code, rec.Body.String())
	}
	if d := time.Since(start); d < time.Second || d > 10*time.Second {
		t.Errorf("the hold lasted %s, want about hold_seconds (1s)", d)
	}
	if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPushHeld+`"`) {
		t.Errorf("decision log = %q, want a %s row", sink.String(), ruleSourceGitPushHeld)
	}
	if up.gitHits != 0 {
		t.Fatalf("a timed-out push reached the forge")
	}

	cp.set(types.ApprovalApproved)
	if rec = postPush(t, p, string(body)); rec.Code != http.StatusOK {
		t.Fatalf("retry after a late approval: status = %d body %q, want 200", rec.Code, rec.Body.String())
	}
	if raises, _ := cp.counts(); raises != 1 {
		t.Errorf("raises = %d, want 1: the retry must rejoin the PENDING row", raises)
	}
}

// TestPushHoldUnattendedRefusesWithoutARow: a run nobody drives cannot be
// asked, so a review match is refused at once and no approval is raised.
func TestPushHoldUnattendedRefusesWithoutARow(t *testing.T) {
	grant := uuid.New()
	up := newGitBrokerUpstream(t, "gh-inst-token")
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(reviewSpec(5, []string{".github/workflows/**"})),
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(up.srv)),
		ControlPlaneURL: "https://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		GitGrants:       map[string]uuid.UUID{"octocat/hello-world": grant},
		Unattended:      true,
	})
	cp := newPushCP(t, types.ApprovalApproved)
	cp.wire(t, p)

	rec := postPush(t, p, string(recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "unattended") {
		t.Fatalf("status = %d body %q, want 403 naming the unattended run", rec.Code, rec.Body.String())
	}
	if !strings.Contains(buf.String(), `"rule_source":"`+ruleSourceGitPushHeldUnattended+`"`) {
		t.Errorf("decision log = %q, want a %s row", buf.String(), ruleSourceGitPushHeldUnattended)
	}
	if raises, polls := cp.counts(); raises != 0 || polls != 0 {
		t.Errorf("an unattended run asked the control plane (raises %d, polls %d), want nothing", raises, polls)
	}
	if up.gitHits != 0 {
		t.Error("an unattended review-path push reached the forge")
	}
}

// TestPushHoldDenyBeatsReview: a path both lists match is refused, never
// held — a deny rule is not something an admin can click past.
func TestPushHoldDenyBeatsReview(t *testing.T) {
	p, sink, _, cp, _ := newAppLaneHold(t,
		reviewSpec(5, []string{".github/workflows/**"}, ".github/**"), types.ApprovalApproved)

	rec := postPush(t, p, string(recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitRules+`"`) {
		t.Errorf("decision log = %q, want the %s deny row", sink.String(), ruleSourceGitRules)
	}
	if raises, _ := cp.counts(); raises != 0 {
		t.Errorf("a denied path raised %d approvals, want 0", raises)
	}
}

// TestPushHoldAdmitsARepackedRetry: an approval covers the push's commits, not
// its pack bytes. git repacks on retry, and a pack built at another
// compression level carries the same commits in different bytes — admitted on
// the dedup key, forwarded verbatim, with no second question.
func TestPushHoldAdmitsARepackedRetry(t *testing.T) {
	p, _, up, cp, _ := newAppLaneHold(t, reviewSpec(5, []string{".github/workflows/**"}), types.ApprovalApproved)
	ref := BranchNSPrefix(p.runID) + "work"
	first := recordedPushArgs(t, ref, workflowPush)
	repacked := recordedPushArgs(t, ref, workflowPush, "-c", "pack.compression=0")
	if bytes.Equal(first, repacked) {
		t.Fatal("the repacked body is byte-identical; this test would prove nothing")
	}
	if newCommit(t, first) != newCommit(t, repacked) {
		t.Fatal("the two recordings carry different commits; this test would prove nothing")
	}

	if rec := postPush(t, p, string(first)); rec.Code != http.StatusOK {
		t.Fatalf("first push: status = %d body %q", rec.Code, rec.Body.String())
	}
	cp.set(types.ApprovalPending) // a second ask would now hold and time out
	if rec := postPush(t, p, string(repacked)); rec.Code != http.StatusOK {
		t.Fatalf("repacked retry: status = %d body %q, want 200", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(up.gitBody, repacked) {
		t.Error("the repacked retry was not forwarded verbatim")
	}
	if raises, _ := cp.counts(); raises != 1 {
		t.Errorf("raises = %d, want 1", raises)
	}
}

// TestPushHoldLeavesOtherPushesAlone: review rules set, nothing matched —
// forwarded unchanged, nothing asked.
func TestPushHoldLeavesOtherPushesAlone(t *testing.T) {
	p, _, up, cp, _ := newAppLaneHold(t, reviewSpec(5, []string{".github/workflows/**"}), types.ApprovalDenied)
	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", map[string]string{"src/main.go": "package main\n"})
	if rec := postPush(t, p, string(body)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %q, want 200", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(up.gitBody, body) {
		t.Error("an unmatched push was not forwarded verbatim")
	}
	if raises, polls := cp.counts(); raises != 0 || polls != 0 {
		t.Errorf("an unmatched push asked the control plane (raises %d, polls %d)", raises, polls)
	}
}

// TestPushHoldAbsentRulesAskNothing: nil push_rules is today's behaviour even
// with an approval channel wired — nothing buffered, nothing asked.
func TestPushHoldAbsentRulesAskNothing(t *testing.T) {
	p, _, up, cp, _ := newAppLaneHold(t, types.RunPolicySpec{}, types.ApprovalDenied)
	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)
	if rec := postPush(t, p, string(body)); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Equal(up.gitBody, body) {
		t.Error("nil push_rules did not forward verbatim")
	}
	if raises, polls := cp.counts(); raises != 0 || polls != 0 {
		t.Errorf("nil push_rules asked the control plane (raises %d, polls %d)", raises, polls)
	}
}

// TestPushHoldOnTheTokenLane runs the same cases on the git_pat lane, with its
// branch-namespace switch ON, for a GitLab host and an Azure DevOps one: a
// review path must hold wherever a deny path would refuse.
func TestPushHoldOnTheTokenLane(t *testing.T) {
	t.Setenv(envEnforcePATBranchNS, "true")
	for _, c := range []struct {
		host, path, repo string
	}{
		{"gitlab.com", "org/repo.git", "gitlab.com/org/repo.git"},
		{"dev.azure.com", "org/project/_git/repo", "dev.azure.com/org/project/_git/repo"},
	} {
		t.Run(c.host, func(t *testing.T) {
			lane := func(t *testing.T, spec types.RunPolicySpec, state types.ApprovalState) (*Proxy, *bytes.Buffer, *gitBrokerUpstream, *pushCP, uuid.UUID) {
				grant := uuid.New()
				up := newPATBrokerUpstream(t, "pat-token", "oauth2")
				p, sink := newPATBrokerProxySpec(t, spec,
					map[string]PATGrant{c.host: {GrantID: grant, Username: "oauth2"}}, upstreamAddr(up.srv))
				cp := newPushCP(t, state)
				cp.wire(t, p)
				return p, sink, up, cp, grant
			}
			post := func(p *Proxy, body []byte) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
					"/wardyn/git/"+c.host+"/"+c.path+"/git-receive-pack", bytes.NewReader(body)))
				return rec
			}
			review := reviewSpec(1, []string{".github/workflows/**"})

			t.Run("approve forwards verbatim", func(t *testing.T) {
				p, _, up, cp, grant := lane(t, review, types.ApprovalApproved)
				body := recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)
				if rec := post(p, body); rec.Code != http.StatusOK {
					t.Fatalf("status = %d body %q, want 200", rec.Code, rec.Body.String())
				}
				if !bytes.Equal(up.gitBody, body) {
					t.Error("the approved push was not forwarded verbatim")
				}
				if _, raises := cp.raised(); len(raises) != 1 || raises[0].Repo != c.repo || raises[0].ActsAs != "git_pat:"+grant.String() {
					t.Errorf("raises = %+v, want one naming repo %q and acts_as git_pat:%s", raises, c.repo, grant)
				}
			})
			t.Run("deny refuses", func(t *testing.T) {
				p, sink, up, _, _ := lane(t, review, types.ApprovalDenied)
				if rec := post(p, recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)); rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", rec.Code)
				}
				if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPushHeld+`"`) || up.gitHits != 0 {
					t.Errorf("decision log %q / forge hits %d, want a %s row and no forward", sink.String(), up.gitHits, ruleSourceGitPushHeld)
				}
			})
			t.Run("timeout refuses", func(t *testing.T) {
				p, _, up, _, _ := lane(t, review, types.ApprovalPending)
				rec := post(p, recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush))
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "nobody decided") || up.gitHits != 0 {
					t.Fatalf("status = %d body %q hits %d, want a 403 timeout and no forward", rec.Code, rec.Body.String(), up.gitHits)
				}
			})
			t.Run("unattended refuses without a row", func(t *testing.T) {
				p, sink, _, cp, _ := lane(t, review, types.ApprovalApproved)
				p.pushHolds.unattended = true
				if rec := post(p, recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)); rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", rec.Code)
				}
				if raises, _ := cp.counts(); raises != 0 || !strings.Contains(sink.String(), ruleSourceGitPushHeldUnattended) {
					t.Errorf("raises = %d, log %q; want none and a %s row", raises, sink.String(), ruleSourceGitPushHeldUnattended)
				}
			})
			t.Run("deny beats review", func(t *testing.T) {
				p, sink, _, cp, _ := lane(t, reviewSpec(1, []string{".github/workflows/**"}, ".github/**"), types.ApprovalApproved)
				if rec := post(p, recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)); rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", rec.Code)
				}
				if raises, _ := cp.counts(); raises != 0 || !strings.Contains(sink.String(), ruleSourceGitRules) {
					t.Errorf("raises = %d, log %q; want none and a %s row", raises, sink.String(), ruleSourceGitRules)
				}
			})
		})
	}
}

// TestPushRulesCompileHold: hold_seconds defaults to 120 and is clamped to the
// proxy's hold ceiling for a policy that reached the sidecar unvalidated.
func TestPushRulesCompileHold(t *testing.T) {
	for _, c := range []struct {
		seconds int
		want    time.Duration
	}{{0, 120 * time.Second}, {30, 30 * time.Second}, {600, maxHoldTimeout}, {100000, maxHoldTimeout}} {
		rs := compilePushRules(&types.PushRulesSpec{RequireReviewPaths: []string{"a/**"}, HoldSeconds: c.seconds})
		if rs.hold != c.want {
			t.Errorf("hold_seconds %d compiles to %s, want %s", c.seconds, rs.hold, c.want)
		}
	}
}

// recordedPushArgs is recordedPush with extra git options on the push itself,
// against a fresh forge each call.
func recordedPushArgs(t *testing.T, ref string, files map[string]string, pushArgs ...string) []byte {
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
	runGit(t, work, append(pushArgs, "push", "-q", f.srv.URL+"/octocat/hello-world.git", "HEAD:"+ref)...)
	return f.pushBody()
}

// TestPushHoldApprovalDoesNotTravel: an approval covers the commits it named
// for the repository and branch it named. The same commits pushed to another
// branch, or to another repository the run can reach, are a new question —
// raised as their own row and held, never forwarded on the first approval.
func TestPushHoldApprovalDoesNotTravel(t *testing.T) {
	t.Run("another branch on the App lane", func(t *testing.T) {
		p, _, up, cp, _ := newAppLaneHold(t, reviewSpec(1, []string{".github/workflows/**"}), types.ApprovalApproved)
		work := recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)
		other := recordedPush(t, BranchNSPrefix(p.runID)+"other", workflowPush)
		if newCommit(t, work) != newCommit(t, other) {
			t.Fatal("the two recordings carry different commits; this test would prove nothing")
		}
		if rec := postPush(t, p, string(work)); rec.Code != http.StatusOK {
			t.Fatalf("approved push: status = %d body %q", rec.Code, rec.Body.String())
		}
		hits := up.gitHits
		cp.set(types.ApprovalPending)
		if rec := postPush(t, p, string(other)); rec.Code != http.StatusForbidden {
			t.Fatalf("same commits to another branch: status = %d, want 403 (held, then timed out)", rec.Code)
		}
		if raises, _ := cp.counts(); raises != 2 || up.gitHits != hits {
			t.Errorf("raises = %d, forge hits %d->%d; want a second row and no forward", raises, hits, up.gitHits)
		}
	})
	t.Run("another repository on the token lane", func(t *testing.T) {
		up := newPATBrokerUpstream(t, "pat-token", "oauth2")
		p, _ := newPATBrokerProxySpec(t, reviewSpec(1, []string{".github/workflows/**"}),
			map[string]PATGrant{"gitlab.com": {GrantID: uuid.New(), Username: "oauth2"}}, upstreamAddr(up.srv))
		cp := newPushCP(t, types.ApprovalApproved)
		cp.wire(t, p)
		body := recordedPush(t, "refs/heads/main", workflowPush)
		post := func(repo string) int {
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
				"/wardyn/git/gitlab.com/org/"+repo+".git/git-receive-pack", bytes.NewReader(body)))
			return rec.Code
		}
		if got := post("repo-a"); got != http.StatusOK {
			t.Fatalf("approved push to repo-a: status = %d", got)
		}
		hits := up.gitHits
		cp.set(types.ApprovalPending)
		if got := post("repo-b"); got != http.StatusForbidden {
			t.Fatalf("same commits to repo-b: status = %d, want 403 (held, then timed out)", got)
		}
		_, raises := cp.raised()
		if len(raises) != 2 || raises[1].Repo != "gitlab.com/org/repo-b.git" || up.gitHits != hits {
			t.Errorf("raises = %+v, forge hits %d->%d; want a second row for repo-b and no forward", raises, hits, up.gitHits)
		}
	})
}
