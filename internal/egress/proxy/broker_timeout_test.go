// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// hungControlPlane accepts the TLS connection and then never answers — a wedged
// wardynd, a hung DB behind it, or an in-path middlebox. It is the shape F070
// fixed for the egress_domain hold and that the BROKER credential path still
// had no ceiling for at all.
func hungControlPlane(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv
}

// TestBrokerCredentialPathIsBoundedByTheApprovalBudget pins the F070 sibling:
// WARDYN_GIT_APPROVAL_TIMEOUT has to bound the brokered-credential path, not
// only the timer inside waitForGitApproval.
//
// Every HTTP call on that path (callMintGit, pollGitApproval) rides
// forwardToControlPlane -> p.localClient, which carried NO Timeout, and both
// handlers pass r.Context() while the agent-facing listener sets
// ReadTimeout/WriteTimeout to 0. So against a control plane that accepts and
// never answers, a clone blocked forever even with the timeout set to 1s —
// strictly worse than the 785s the egress_domain hold had before F070, and the
// wave routed the git_pat lane into the same unbounded path.
//
// The assertion is the one F070's own pin makes: bounded by the operator's
// budget (plus slack), and still FAILING CLOSED — a 502, no credential, no
// forward.
func TestBrokerCredentialPathIsBoundedByTheApprovalBudget(t *testing.T) {
	const budget = time.Second
	t.Setenv(envGitApprovalTimeout, budget.String())

	newHungProxy := func(t *testing.T) *Proxy {
		t.Helper()
		cp := hungControlPlane(t)
		grant := uuid.New()
		buf := &bytes.Buffer{}
		return newProxy(Options{
			RunID:           uuid.New(),
			Policy:          CompilePolicy(types.RunPolicySpec{}),
			Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
			Resolver:        publicResolver{},
			Dial:            redirectDial(upstreamAddr(cp)),
			ControlPlaneURL: "https://wardynd.test:8080",
			RunToken:        newTokenSource("RUNTOK"),
			TLSClientConfig: testInsecureTLSConfig,
			GitGrants:       map[string]uuid.UUID{"org/repo": grant},
			PATGrants:       map[string]PATGrant{"gitlab.com": {GrantID: grant}},
		})
	}

	for _, tc := range []struct{ name, path string }{
		{"github_token lane", "/wardyn/gh/org/repo.git/info/refs?service=git-upload-pack"},
		{"git_pat lane", "/wardyn/git/gitlab.com/org/repo.git/info/refs?service=git-upload-pack"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newHungProxy(t)
			rec := httptest.NewRecorder()
			done := make(chan time.Duration, 1)
			go func() {
				start := time.Now()
				p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, tc.path, nil))
				done <- time.Since(start)
			}()
			select {
			case elapsed := <-done:
				if elapsed > budget+5*time.Second {
					t.Fatalf("the brokered mint took %s against a hung control plane with %s=%s — "+
						"the documented timeout must bound the whole acquisition, not just the approval timer",
						elapsed, envGitApprovalTimeout, budget)
				}
			case <-time.After(budget + 5*time.Second):
				t.Fatalf("the brokered mint was STILL blocked after %s against a hung control plane with "+
					"%s=%s: nothing in the stack would ever end it", budget+5*time.Second, envGitApprovalTimeout, budget)
			}
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502: an unobtainable credential must fail CLOSED", rec.Code)
			}
		})
	}
}
