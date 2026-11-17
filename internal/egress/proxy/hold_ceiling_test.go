// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A via_org engine's holds end under its ceiling, so a held request gets the hold's own sentence
// before the relay's idle cut would reset the stream. A sidecar sets none.

const holdCeilingTest = 2 * time.Second

// serveWithin runs serve and fails the test if it outlasts limit: a ceiling that is not applied
// leaves the hold on its own 240 s to 600 s window.
func serveWithin(t *testing.T, limit time.Duration, serve func() *httptest.ResponseRecorder) (*httptest.ResponseRecorder, time.Duration) {
	t.Helper()
	start := time.Now()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serve() }()
	select {
	case rec := <-done:
		return rec, time.Since(start)
	case <-time.After(limit):
		t.Fatalf("the hold outlasted %s: the ceiling was not applied", limit)
		return nil, 0
	}
}

func TestHoldCeiling_CapabilityHoldEndsAtTheCeiling(t *testing.T) {
	cp := newCapControlPlane(t)
	h := newADOHoldHarness(t, cp, &fakeApprovalReader{steps: steps(types.ApprovalPending)})
	h.p.inject.reauth.ceiling = holdCeilingTest
	rec, took := serveWithin(t, 3*holdCeilingTest, func() *httptest.ResponseRecorder { return h.patchWorkItem(t) })
	h.mustRefuse(t, rec, adoHoldTimedOutRefusal)
	if took < holdCeilingTest || took > 3*holdCeilingTest {
		t.Errorf("the capability hold lasted %s, want about %s", took, holdCeilingTest)
	}
}

func TestHoldCeiling_ReauthHoldEndsAtTheCeiling(t *testing.T) {
	fastPolls(t, 5*time.Millisecond)
	h := newADOHarness(t, adoscope.CapProjectRead)
	cp := newSignInControlPlane(t, adoToken)
	lapse(t, h.p.inject, adoHost, cp, &fakeApprovalReader{steps: steps(types.ApprovalPending)})
	h.p.inject.reauth.ceiling = holdCeilingTest
	rec, took := serveWithin(t, 3*holdCeilingTest, func() *httptest.ResponseRecorder {
		return h.do(t, http.MethodGet, "/acme/_apis/projects?api-version=7.1", "", nil)
	})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), adoSignInTimedOutRefusal) {
		t.Fatalf("status %d body %s, want the sign-in hold's own timeout sentence", rec.Code, rec.Body.String())
	}
	if took < holdCeilingTest || took > 3*holdCeilingTest {
		t.Errorf("the re-auth hold lasted %s, want about %s", took, holdCeilingTest)
	}
}

func TestHoldCeiling_PushApprovalEndsAtTheCeiling(t *testing.T) {
	p, _, up, _, _ := newAppLaneHold(t, reviewSpec(60, []string{".github/workflows/**"}), types.ApprovalPending)
	p.approval.capHolds(holdCeilingTest)
	body := recordedPush(t, BranchNSPrefix(p.runID)+"work", workflowPush)
	rec, took := serveWithin(t, 3*holdCeilingTest, func() *httptest.ResponseRecorder { return postPush(t, p, string(body)) })
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "nobody decided within 2s") {
		t.Fatalf("status %d body %q, want the push hold's own timeout sentence at the ceiling", rec.Code, rec.Body.String())
	}
	if took < holdCeilingTest || up.gitHits != 0 {
		t.Errorf("the push hold lasted %s and the forge saw %d hits, want about %s and none", took, up.gitHits, holdCeilingTest)
	}
}

// A sidecar sets no ceiling: its re-auth hold keeps the knob's default window.
func TestHoldCeiling_SidecarKeepsTheDefaultReauthWindow(t *testing.T) {
	t.Setenv(envCredentialReauthTimeout, "")
	fastPolls(t, 5*time.Millisecond)
	h := newADOHarness(t, adoscope.CapProjectRead)
	cp := newSignInControlPlane(t, adoToken)
	lapse(t, h.p.inject, adoHost, cp, &fakeApprovalReader{steps: steps(types.ApprovalPending)})
	coord := h.p.inject.reauth
	if coord.ceiling != 0 {
		t.Fatalf("a sidecar's coordinator has ceiling %s, want none", coord.ceiling)
	}
	go func() { h.do(t, http.MethodGet, "/acme/_apis/projects?api-version=7.1", "", nil) }()
	var wf *reauthWorkflow
	for range 400 {
		coord.mu.Lock()
		for _, w := range coord.workflows {
			wf = w
		}
		coord.mu.Unlock()
		if wf != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if wf == nil {
		t.Fatal("the lapsed request opened no re-auth hold")
	}
	if left := time.Until(wf.deadline); left < defaultCredentialReauthTimeout-10*time.Second || left > defaultCredentialReauthTimeout {
		t.Errorf("the hold has %s left, want about defaultCredentialReauthTimeout (%s)", left, defaultCredentialReauthTimeout)
	}
}

// The ceiling is applied where the engine builds its hold owners, and only for an engine.
func TestHoldCeiling_EngineWiresEveryHoldOwner(t *testing.T) {
	build := func(ceiling time.Duration) *ViaOrgEngine {
		up := newOrgUpstream(t)
		cfg := &Config{
			RunID: uuid.New(), ControlPlaneURL: "https://wardynd.test", RunToken: "RUNTOK", DecisionBufferSize: defaultBufferSize,
			Policy: types.RunPolicySpec{AllowedDomains: []string{viaOrgHost}, FirstUseHoldSeconds: 300},
			Injection: []InjectionConfig{{GrantID: uuid.New(),
				InjectionRule: egress.InjectionRule{Host: viaOrgHost, Header: "Authorization", SecretName: "corp-secret"}}},
			MITMHosts:    []string{viaOrgHost},
			TrustedCAPEM: string(up.caPEM),
		}
		e, err := NewViaOrgEngine(context.Background(), cfg, ViaOrgEngineOptions{
			Client: &http.Client{Transport: &fakeOrg{}}, Dial: redirectDial(upstreamAddr(up.srv)),
			LookupIP: publicResolver{}.LookupIP, HoldCeiling: ceiling,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close(context.Background()) })
		return e
	}
	capped := build(holdCeilingTest).srv.proxy
	if capped.inject.reauth.ceiling != holdCeilingTest || capped.approval.holdCeiling != holdCeilingTest ||
		capped.approval.holdTimeout != holdCeilingTest {
		t.Errorf("a capped engine has reauth %s, push %s, first-use hold %s; want %s each",
			capped.inject.reauth.ceiling, capped.approval.holdCeiling, capped.approval.holdTimeout, holdCeilingTest)
	}
	open := build(0).srv.proxy
	if open.inject.reauth.ceiling != 0 || open.approval.holdCeiling != 0 || open.approval.holdTimeout != 300*time.Second {
		t.Errorf("an engine with no ceiling has reauth %s, push %s, first-use hold %s; want 0, 0, 300s",
			open.inject.reauth.ceiling, open.approval.holdCeiling, open.approval.holdTimeout)
	}
}
