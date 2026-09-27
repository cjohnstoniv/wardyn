// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newEntraLane is the Azure DevOps Entra lane (/wardyn/git/ on a host the
// run's per-person grant covers) with the person's bearer injected, a forge
// recording what reaches it, and a fake control plane for held pushes. caps
// is what the grant holds; with code_write a push to the run's own branch
// needs no capability hold, so anything refused is the content rules'.
func newEntraLane(t *testing.T, spec types.RunPolicySpec, state types.ApprovalState,
	caps ...adoscope.Capability) (*Proxy, *bytes.Buffer, *gitBrokerUpstream, *pushCP, uuid.UUID) {
	t.Helper()
	up := newGitBrokerUpstream(t, "unused")
	p, sink, grantID := newEntraProxy(t, spec, upstreamAddr(up.srv), caps...)
	cp := newPushCP(t, state)
	cp.wire(t, p)
	return p, sink, up, cp, grantID
}

// newEntraProxy is the lane's proxy alone, dialing addr for Azure DevOps.
func newEntraProxy(t *testing.T, spec types.RunPolicySpec, addr string, caps ...adoscope.Capability) (*Proxy, *bytes.Buffer, uuid.UUID) {
	t.Helper()
	grantID := uuid.New()
	inj := &injector{byHost: map[string]*injEntry{
		"dev.azure.com": {grantID: grantID, header: injectedHeader{name: "Authorization", value: "Bearer entra-" + uuid.NewString()}, requireTLS: true},
	}}
	sink := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(spec),
		Injector:        inj,
		Sink:            &decisionSink{out: sink, ch: make(chan egress.DecisionLog, 256)},
		Resolver:        publicResolver{},
		Dial:            redirectDial(addr),
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		ADOGrants:       adoGrantsByHost{"dev.azure.com": ADOGrant{Organization: "acme", Capabilities: caps}},
	})
	return p, sink, grantID
}

func postEntraPush(t *testing.T, p *Proxy, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost,
		"/wardyn/git/dev.azure.com/acme/proj/_git/app/git-receive-pack", bytes.NewReader(body)))
	return rec
}

// TestPushHoldOnTheEntraLane runs TestPushHoldOnTheTokenLane's cases on the
// per-person Azure DevOps lane: a review path holds and a deny path refuses
// here exactly as on the other two lanes, and the approval names the lane's
// own credential.
func TestPushHoldOnTheEntraLane(t *testing.T) {
	review := reviewSpec(1, []string{".github/workflows/**"})
	write := adoscope.CapCodeWrite

	t.Run("approve forwards verbatim", func(t *testing.T) {
		p, _, up, cp, grant := newEntraLane(t, review, types.ApprovalApproved, write)
		body := recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)
		if rec := postEntraPush(t, p, body); rec.Code != http.StatusOK {
			t.Fatalf("status = %d body %q, want 200", rec.Code, rec.Body.String())
		}
		if !bytes.Equal(up.gitBody, body) {
			t.Error("the approved push was not forwarded verbatim")
		}
		_, raises := cp.raised()
		if len(raises) != 1 || raises[0].Repo != "dev.azure.com/acme/proj/_git/app" ||
			raises[0].ActsAs != "api_key:"+grant.String() || raises[0].ActsAsKind != "" || raises[0].ActsAsLabel != "" {
			t.Errorf("raises = %+v, want one naming dev.azure.com/acme/proj/_git/app and acts_as api_key:%s, "+
				"with no label of the sidecar's own", raises, grant)
		}
	})
	t.Run("deny refuses", func(t *testing.T) {
		p, sink, up, _, _ := newEntraLane(t, review, types.ApprovalDenied, write)
		if rec := postEntraPush(t, p, recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)); rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitPushHeld+`"`) || up.gitHits != 0 {
			t.Errorf("decision log %q / forge hits %d, want a %s row and no forward", sink.String(), up.gitHits, ruleSourceGitPushHeld)
		}
	})
	t.Run("timeout refuses", func(t *testing.T) {
		p, _, up, _, _ := newEntraLane(t, review, types.ApprovalPending, write)
		rec := postEntraPush(t, p, recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush))
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "nobody decided") || up.gitHits != 0 {
			t.Fatalf("status = %d body %q hits %d, want a 403 timeout and no forward", rec.Code, rec.Body.String(), up.gitHits)
		}
	})
	t.Run("unattended refuses without a row", func(t *testing.T) {
		p, sink, up, cp, _ := newEntraLane(t, review, types.ApprovalApproved, write)
		p.pushHolds.unattended = true
		if rec := postEntraPush(t, p, recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)); rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if raises, _ := cp.counts(); raises != 0 || !strings.Contains(sink.String(), ruleSourceGitPushHeldUnattended) || up.gitHits != 0 {
			t.Errorf("raises = %d, log %q, hits %d; want none, a %s row, no forward", raises, sink.String(), up.gitHits, ruleSourceGitPushHeldUnattended)
		}
	})
	t.Run("deny beats review", func(t *testing.T) {
		p, sink, up, cp, _ := newEntraLane(t, reviewSpec(1, []string{".github/workflows/**"}, ".github/**"), types.ApprovalApproved, write)
		if rec := postEntraPush(t, p, recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)); rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if raises, _ := cp.counts(); raises != 0 || !strings.Contains(sink.String(), ruleSourceGitRules) || up.gitHits != 0 {
			t.Errorf("raises = %d, log %q, hits %d; want none, a %s row, no forward", raises, sink.String(), up.gitHits, ruleSourceGitRules)
		}
	})
	// The content rules come BEFORE the capability check: a run without
	// code_write pushing a denied path gets the content refusal, and nobody is
	// asked for a capability to push something the rules refuse anyway.
	t.Run("content rules before the capability check", func(t *testing.T) {
		p, sink, up, _, _ := newEntraLane(t, contentRulesSpec(".github/**"), types.ApprovalApproved)
		if rec := postEntraPush(t, p, recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)); rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if !strings.Contains(sink.String(), `"rule_source":"`+ruleSourceGitRules+`"`) ||
			strings.Contains(sink.String(), ruleSourceADOGitDenied) || up.gitHits != 0 {
			t.Errorf("decision log %q, hits %d; want the %s row alone and no forward", sink.String(), up.gitHits, ruleSourceGitRules)
		}
	})
	t.Run("nil push_rules forwards verbatim and asks nothing", func(t *testing.T) {
		p, _, up, cp, _ := newEntraLane(t, types.RunPolicySpec{}, types.ApprovalDenied, write)
		body := recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)
		if rec := postEntraPush(t, p, body); rec.Code != http.StatusOK {
			t.Fatalf("status = %d body %q, want 200", rec.Code, rec.Body.String())
		}
		if !bytes.Equal(up.gitBody, body) {
			t.Error("nil push_rules did not forward verbatim")
		}
		if raises, polls := cp.counts(); raises != 0 || polls != 0 {
			t.Errorf("nil push_rules asked the control plane (raises %d, polls %d)", raises, polls)
		}
	})
}

// TestPushRulesAdvertiseNoThinOnTheEntraLane: a lane that enforces content
// rules must ask for a pack it can read, or a shallow clone's every push is
// refused as uninspectable. Same trigger as the other two lanes.
func TestPushRulesAdvertiseNoThinOnTheEntraLane(t *testing.T) {
	for _, c := range []struct {
		name string
		spec types.RunPolicySpec
		want bool
	}{
		{"content rules set", reviewSpec(0, []string{".github/workflows/**"}), true},
		{"no content rules", types.RunPolicySpec{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			up := newAdvertUpstream(t, advert(realCaps))
			p, _, _ := newEntraProxy(t, c.spec, upstreamAddr(up.srv), adoscope.CapRead, adoscope.CapCodeWrite)
			rec := httptest.NewRecorder()
			req := mustLocalReq(t, http.MethodGet,
				"/wardyn/git/dev.azure.com/acme/proj/_git/app/info/refs?service=git-receive-pack", nil)
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
