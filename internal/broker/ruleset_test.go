// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rulesetFake is a stand-in api.github.com: rules/branches bodies keyed by probe
// branch name, ruleset bodies keyed by id, and a record of what was asked. The
// bodies are the real wire shape, taken from live api.github.com reads.
//
// A branch not in rules 404s unless otherwise is set — several tests turn on the
// difference between "no rules" ([]) and "could not read" (404).
type rulesetFake struct {
	rules     map[string]string
	otherwise string
	rulesets  map[int64]string
	// blockRules makes every rules/branches read hang until the CLIENT gives up,
	// so a caller deadline can be exercised without a sleep.
	blockRules bool

	mu      sync.Mutex
	asked   []string
	revoked bool
}

func (f *rulesetFake) askedBranches() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func (f *rulesetFake) tokenRevoked() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.revoked
}

// newRulesetMinter arms a real githubMinter against f.
func newRulesetMinter(t *testing.T, f *rulesetFake) *githubMinter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/installation/token"):
			f.mu.Lock()
			f.revoked = true
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/installation"):
			_, _ = w.Write([]byte(`{"id": 42}`))
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_probe","expires_at":"2099-01-01T00:00:00Z"}`))
		case strings.Contains(r.URL.Path, "/rulesets/"):
			// Measured: with includes_parents=false this endpoint 404s for a
			// ruleset inherited from the ORGANIZATION, which is exactly the kind
			// an operator is most likely relying on. Reject the wrong query here
			// so dropping the flag shows up as a broken grading table.
			if r.URL.Query().Get("includes_parents") != "true" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			body, ok := f.rulesets[atoi64(path.Base(r.URL.Path))]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			_, branch, found := strings.Cut(r.URL.Path, "/rules/branches/")
			if found {
				f.mu.Lock()
				f.asked = append(f.asked, branch)
				f.mu.Unlock()
			}
			if f.blockRules {
				<-r.Context().Done()
				return
			}
			body, known := f.rules[branch]
			if !known {
				body = f.otherwise
			}
			if !found || body == "" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return newMinterAgainst(t, srv.URL+"/")
}

// newMinterAgainst builds a githubMinter with throwaway App credentials pointed
// at base.
func newMinterAgainst(t *testing.T, base string) *githubMinter {
	t.Helper()
	ctx := context.Background()
	store := newMemSecrets()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	_ = store.Put(ctx, "github-app-id", []byte("12345"))
	_ = store.Put(ctx, "github-app-key", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	gm, err := NewGitHubMinter(store, GitHubMinterConfig{AppIDSecret: "github-app-id", PrivateKeySecret: "github-app-key"})
	if err != nil {
		t.Fatalf("NewGitHubMinter: %v", err)
	}
	m := gm.(*githubMinter)
	m.baseURL = base
	return m
}

func atoi64(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

const (
	rulesConfining      = `[{"type":"creation","ruleset_id":1},{"type":"update","parameters":{"update_allows_fetch_and_merge":false},"ruleset_id":1},{"type":"deletion","ruleset_id":1},{"type":"non_fast_forward","ruleset_id":1}]`
	rulesNoUpdate       = `[{"type":"creation","ruleset_id":1},{"type":"deletion","ruleset_id":1}]`
	rulesNoDeletion     = `[{"type":"creation","ruleset_id":1},{"type":"update","ruleset_id":1}]`
	rulesNone           = `[]`
	rulesPartial        = `[{"type":"deletion","ruleset_id":1},{"type":"non_fast_forward","ruleset_id":1}]`
	rulesetNeverBypass  = `{"id":1,"name":"wardyn-agent-ref-confinement","enforcement":"active","current_user_can_bypass":"never"}`
	rulesetAlwaysBypass = `{"id":1,"name":"wardyn-agent-ref-confinement","enforcement":"active","current_user_can_bypass":"always"}`
	rulesetNoBypassInfo = `{"id":1,"name":"wardyn-agent-ref-confinement","enforcement":"active"}`
)

func neverBypass() map[int64]string { return map[int64]string{1: rulesetNeverBypass} }

// refNamespaceGlob must root on the namespace the proxy actually enforces AND
// stay recursive. GitHub's ref_name matching does not let "*" cross "/", and a
// TRAILING "**" behaves the same as "*" (measured against the live evaluator —
// see the constant's comment), while a run pushes wardyn/<run-id>/work, two
// segments deep. So a glob ending in a bare "**" excludes nothing a run pushes:
// the operator's ruleset would refuse every governed push at GitHub, and this
// check would tell them to do the thing they just did.
func TestRefNamespaceGlobCoversTheRunBranch(t *testing.T) {
	if !strings.HasSuffix(refNamespaceGlob, "/**/*") {
		t.Fatalf("refNamespaceGlob = %q; it must end in /**/* or it excludes nothing a run pushes", refNamespaceGlob)
	}
	// The glob's fixed prefix must be the namespace the proxy enforces, or the
	// operator would exclude a namespace runs never push to.
	root := strings.TrimSuffix(refNamespaceGlob, "**/*")
	if got := proxy.BranchNSPrefix(uuid.New()); !strings.HasPrefix(got, root) {
		t.Fatalf("proxy enforces %q, ruleset recipe excludes %q — the exclude does not cover the run branch", got, refNamespaceGlob)
	}
}

// The answers VerifyRefRuleset must distinguish, and the mint gate keys on the
// difference: all three write rules restricted outside + open inside + no
// bypass is the ONLY confined shape.
func TestVerifyRefRuleset_Grading(t *testing.T) {
	cases := []struct {
		name         string
		outside      string
		inside       string
		rulesets     map[int64]string
		wantConfined bool
		wantDetail   []string
	}{
		{
			// The "confined" detail must not overstate: a target:"branch" ruleset
			// bounds branches only, so refs/tags/* stays open to a leaked token.
			name:    "restricted outside, namespace excluded, no bypass => confined",
			outside: rulesConfining, inside: rulesNone, rulesets: neverBypass(),
			wantConfined: true,
			wantDetail:   []string{"creation, update and deletion are restricted", "refs/tags/*"},
		},
		{
			name:    "nothing in force outside => unconfined",
			outside: rulesNone, inside: rulesNone, rulesets: neverBypass(),
			wantConfined: false, wantDetail: []string{"no creation/update/deletion rule in force"},
		},
		{
			// non_fast_forward stops a force-push but not an ordinary push to a
			// new or existing branch, and deletion alone stops neither.
			name:    "deletion+non_fast_forward only => still unconfined",
			outside: rulesPartial, inside: rulesNone, rulesets: neverBypass(),
			wantConfined: false, wantDetail: []string{"no creation/update rule in force"},
		},
		{
			// The outside AND, tested per-conjunct rather than through a substring
			// on the combined message: creation present, update absent.
			name:    "creation without update => unconfined, names update",
			outside: rulesNoUpdate, inside: rulesNone, rulesets: neverBypass(),
			wantConfined: false, wantDetail: []string{"no update rule in force"},
		},
		{
			// deletion is a write ("Only allow users with bypass permissions to
			// delete matching refs"): without it a leaked token still runs
			// `git push --delete origin main`.
			name:    "creation+update without deletion => unconfined, names deletion",
			outside: rulesNoDeletion, inside: rulesNone, rulesets: neverBypass(),
			wantConfined: false, wantDetail: []string{"no deletion rule in force"},
		},
		{
			// A ruleset with no ref_name.exclude also blocks the run's own pushes.
			name:    "namespace not excluded => unconfined, and says why",
			outside: rulesConfining, inside: rulesConfining, rulesets: neverBypass(),
			wantConfined: false, wantDetail: []string{"ref_name.exclude"},
		},
		{
			// A perfect rule list the App is exempt from is not confinement.
			name:    "App can bypass the ruleset => unconfined",
			outside: rulesConfining, inside: rulesNone,
			rulesets:     map[int64]string{1: rulesetAlwaysBypass},
			wantConfined: false, wantDetail: []string{"current_user_can_bypass"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newRulesetMinter(t, &rulesetFake{
				rules:    map[string]string{refProbeOutside: tc.outside, refProbeInside: tc.inside},
				rulesets: tc.rulesets,
			})
			confined, detail, err := m.VerifyRefRuleset(context.Background(), "acme/widgets")
			if err != nil {
				t.Fatalf("VerifyRefRuleset: %v", err)
			}
			if confined != tc.wantConfined {
				t.Fatalf("confined = %v, want %v (detail %q)", confined, tc.wantConfined, detail)
			}
			for _, want := range tc.wantDetail {
				if !strings.Contains(detail, want) {
					t.Fatalf("detail = %q, want it to contain %q", detail, want)
				}
			}
		})
	}
}

// A failed read is UNKNOWN (an error), never a quiet "unconfined" — the callers
// grade the two differently and must be able to tell them apart. Both reads
// count: the bypass read is as load-bearing as the rules read, so an
// unreadable ruleset (or one that returns no bypass field at all, which is what
// an unauthenticated read of a public repo does) must not grade confined.
func TestVerifyRefRuleset_ReadFailureIsAnError(t *testing.T) {
	cases := map[string]*rulesetFake{
		"rules read 404s": {},
		"ruleset read 404s": {
			rules:    map[string]string{refProbeOutside: rulesConfining, refProbeInside: rulesNone},
			rulesets: nil,
		},
		"ruleset omits current_user_can_bypass": {
			rules:    map[string]string{refProbeOutside: rulesConfining, refProbeInside: rulesNone},
			rulesets: map[int64]string{1: rulesetNoBypassInfo},
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			m := newRulesetMinter(t, f)
			confined, _, err := m.VerifyRefRuleset(context.Background(), "acme/widgets")
			if err == nil {
				t.Fatal("an unreadable answer must return an error, not confined=false with a nil error")
			}
			if confined {
				t.Fatal("confined must be false on error")
			}
		})
	}
}

// The outside probe must be a branch nobody would ever name in a ruleset.
// Probing a REAL branch would grade a ruleset covering only that branch as
// confinement while master, release/* and every other ref stayed open to the
// token — so a fake that restricts ONLY main/master must grade UNCONFINED.
func TestVerifyRefRuleset_OutsideProbeIsNotARealBranch(t *testing.T) {
	f := &rulesetFake{
		// Every branch reads as "no rules" except the plausible ones.
		rules: map[string]string{
			"main":   rulesConfining,
			"master": rulesConfining,
			"HEAD":   rulesConfining,
		},
		otherwise: rulesNone,
		rulesets:  neverBypass(),
	}
	m := newRulesetMinter(t, f)
	confined, detail, err := m.VerifyRefRuleset(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("VerifyRefRuleset: %v", err)
	}
	if confined {
		t.Fatalf("a ruleset naming only main/master is NOT ref confinement, but it graded confined: %s", detail)
	}
	asked := f.askedBranches()
	if len(asked) == 0 {
		t.Fatal("no branch was probed")
	}
	for _, b := range asked {
		switch b {
		case "main", "master", "HEAD":
			t.Fatalf("probed the real branch %q; the outside probe must be a synthetic name", b)
		}
	}
}

// The probe must not need a repo permission the App may not hold: metadata:read
// is the documented one and the only one every App holds mandatorily. And the
// live token it mints — GitHub gives installation tokens ~1h whatever TTL is
// asked for, and this one is minted outside Broker.mint so it is unaudited —
// must be handed back rather than left alive.
func TestVerifyRefRuleset_MintsMetadataReadOnlyAndRevokes(t *testing.T) {
	var gotPerms string
	f := &rulesetFake{otherwise: rulesNone, rulesets: neverBypass()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/installation/token"):
			f.mu.Lock()
			f.revoked = true
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/installation"):
			_, _ = w.Write([]byte(`{"id": 42}`))
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			buf := make([]byte, 512)
			n, _ := r.Body.Read(buf)
			gotPerms = string(buf[:n])
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_probe","expires_at":"2099-01-01T00:00:00Z"}`))
		default:
			_, _ = w.Write([]byte(rulesNone))
		}
	}))
	defer srv.Close()

	m := newMinterAgainst(t, srv.URL+"/")
	if _, _, err := m.VerifyRefRuleset(context.Background(), "acme/widgets"); err != nil {
		t.Fatalf("VerifyRefRuleset: %v", err)
	}
	if !strings.Contains(gotPerms, `"metadata":"read"`) {
		t.Fatalf("token request permissions = %s; want metadata:read only", gotPerms)
	}
	if strings.Contains(gotPerms, `"contents"`) {
		t.Fatalf("token request must not ask for contents: %s", gotPerms)
	}
	if !f.tokenRevoked() {
		t.Fatal("the probe token must be revoked; leaving it live is an unaudited ghs_ token for ~1h")
	}
}

// The revoke must survive an EXPIRED caller ctx — the timeout path is exactly
// where an unrevoked token would otherwise linger for the hour, so the deferred
// revoke has to detach from the caller's ctx (context.WithoutCancel).
func TestVerifyRefRuleset_RevokesAfterCallerDeadline(t *testing.T) {
	f := &rulesetFake{blockRules: true, rulesets: neverBypass()}
	m := newRulesetMinter(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := m.VerifyRefRuleset(ctx, "acme/widgets"); err == nil {
		t.Fatal("a read that outlives the caller's deadline must be an error")
	}
	if !f.tokenRevoked() {
		t.Fatal("the deferred revoke must run on a ctx detached from the caller's")
	}
}

// The opt-in gate: off (the default) mints regardless; on, an unconfined repo —
// and an UNVERIFIABLE one — refuse, and the message names the env var and the
// repo so the operator can act on it.
func TestMintGitHub_RequireRefRulesetGate(t *testing.T) {
	mint := func(t *testing.T, gh GitHubMinter) (Minted, error) {
		t.Helper()
		db := newFakeDB()
		b := New(db, nil, &fakeAudit{}, nil, gh)
		runID := uuid.New()
		spec := githubGrantSpec(t, false)
		gid := seedGrant(db, runID, spec)
		return b.MintForGrant(context.Background(), callerFor(runID), gid)
	}

	t.Run("default off: an unconfined repo still mints", func(t *testing.T) {
		if _, err := mint(t, &FakeGitHubMinter{Token: "ghs_x"}); err != nil {
			t.Fatalf("mint with the gate unset must succeed: %v", err)
		}
	})

	t.Setenv(envRequireRefRuleset, "true")

	t.Run("on + unconfined: refused, naming the var and the repo", func(t *testing.T) {
		_, err := mint(t, &FakeGitHubMinter{Token: "ghs_x", RefRulesetDetail: "acme/widgets: no ruleset."})
		if !errors.Is(err, ErrRefRulesetRequired) {
			t.Fatalf("err = %v, want ErrRefRulesetRequired", err)
		}
		for _, want := range []string{envRequireRefRuleset, "acme/widgets", "docs/POLICIES.md"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal %q must name %q", err, want)
			}
		}
	})

	t.Run("on + unverifiable: refused too (fail closed)", func(t *testing.T) {
		_, err := mint(t, &FakeGitHubMinter{Token: "ghs_x", RefRulesetErr: errors.New("dial tcp: i/o timeout")})
		if !errors.Is(err, ErrRefRulesetRequired) {
			t.Fatalf("err = %v, want ErrRefRulesetRequired when the check itself fails", err)
		}
	})

	t.Run("on + confined: mints", func(t *testing.T) {
		gh := &FakeGitHubMinter{Token: "ghs_x", RefRulesetConfined: true}
		got, err := mint(t, gh)
		if err != nil {
			t.Fatalf("mint with a confined repo must succeed: %v", err)
		}
		if got.Token != "ghs_x" {
			t.Fatalf("token = %q", got.Token)
		}
		if len(gh.LastVerifiedRepos) != 1 || gh.LastVerifiedRepos[0] != "acme/widgets" {
			t.Fatalf("verified repos = %v, want every repo in the grant scope", gh.LastVerifiedRepos)
		}
	})

	// The check holds the mint tx's FOR UPDATE row lock and a pooled PG
	// connection while it talks to GitHub, so it must carry its own deadline.
	t.Run("on: the probe gets a deadline", func(t *testing.T) {
		gh := &deadlineRecorder{FakeGitHubMinter: FakeGitHubMinter{Token: "ghs_x", RefRulesetConfined: true}}
		if _, err := mint(t, gh); err != nil {
			t.Fatalf("mint: %v", err)
		}
		if !gh.hadDeadline {
			t.Fatal("VerifyRefRuleset was handed a ctx with no deadline; a blackholed api.github.com would pin the grant row lock and a pooled PG connection for as long as it stayed black")
		}
	})
}

// deadlineRecorder notes whether checkRefRuleset bounded the outbound call.
type deadlineRecorder struct {
	FakeGitHubMinter
	hadDeadline bool
}

func (d *deadlineRecorder) VerifyRefRuleset(ctx context.Context, repo string) (bool, string, error) {
	_, d.hadDeadline = ctx.Deadline()
	return d.FakeGitHubMinter.VerifyRefRuleset(ctx, repo)
}

// The gate must not touch the other grant kinds.
func TestRequireRefRulesetGate_GitHubOnly(t *testing.T) {
	t.Setenv(envRequireRefRuleset, "true")
	db := newFakeDB()
	secrets := newMemSecrets()
	_ = secrets.Put(context.Background(), "pat", []byte("ghp_x"))
	b := New(db, secrets, &fakeAudit{}, nil, &FakeGitHubMinter{})
	runID := uuid.New()
	gid := seedGrant(db, runID, types.GrantSpec{
		Kind:       types.GrantGitPAT,
		Scope:      []byte(`{"host":"github.com","secret_name":"pat"}`),
		TTLSeconds: 600,
	})
	if _, err := b.MintForGrant(context.Background(), callerFor(runID), gid); err != nil {
		t.Fatalf("git_pat mint must be unaffected by the github ruleset gate: %v", err)
	}
}
