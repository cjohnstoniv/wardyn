// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newEgressEditFixture is newReadbackFixture's run, live: its proxy runs and nothing has lost it.
func newEgressEditFixture(t *testing.T) *readbackFixture {
	t.Helper()
	f := newReadbackFixture(t)
	f.st.run.LostAt, f.st.run.LostReason = nil, ""
	f.run = f.st.run
	poll, wait := egressEditPoll, egressEditWait
	egressEditPoll, egressEditWait = time.Millisecond, 10*time.Second
	t.Cleanup(func() { egressEditPoll, egressEditWait = poll, wait })
	return f
}

func (f *readbackFixture) editEgress(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.srv, http.MethodPatch, "/api/v1/runs/"+f.run.ID.String()+"/egress", adminToken, body)
}

// confirmEachReplace has each new proxy report the bytes it was handed, under its own token, while
// its driver is still answering: the report is held and the driver's hash then enforces the start.
func (f *readbackFixture) confirmEachReplace(t *testing.T) {
	f.rr.onReplace = func(cfg []byte) {
		if code, _ := postLoaded(t, f.srv, f.run.ID, f.tokenJTI(t, cfg), hashHex(cfg)); code != http.StatusAccepted {
			t.Errorf("the new proxy's report: %d, want 202 (held)", code)
		}
	}
}

func (f *readbackFixture) tokenJTI(t *testing.T, cfg []byte) string {
	t.Helper()
	var c struct {
		RunToken string `json:"run_token"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Errorf("the handed-over config: %v", err)
		return ""
	}
	claims, err := f.srv.cfg.Identity.Verify(context.Background(), c.RunToken, internalAudience)
	if err != nil {
		t.Errorf("the new proxy's token does not verify: %v", err)
		return ""
	}
	return claims.JTI
}

// answeredStart waits until the run's active start has its driver's answer (a deadline), so a test
// can move the database clock past it.
func (f *readbackFixture) answeredStart(t *testing.T, after int) int {
	t.Helper()
	for range 5000 {
		if p, ok, _ := f.rb.ActiveProxyStart(context.Background(), f.run.ID); ok && p.Generation > after && p.Deadline != nil {
			return p.Generation
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the edit's start was never answered")
	return 0
}

// The edit removes an allowlist entry and denies it, adds a deny, and answers only once the new
// proxy confirmed the config it loaded, with that start's generation.
func TestEgressEdit_NarrowsAndAnswersAfterTheReadback(t *testing.T) {
	f := newEgressEditFixture(t)
	f.confirmEachReplace(t)
	w := f.editEgress(t, `{"remove_hosts":["API.Anthropic.com"],"deny_hosts":["evil.example"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s, want 200", w.Code, w.Body.String())
	}
	var out egressEditResult
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	p := f.active(t)
	if p.State != types.ProxyStartEnforced || out.PolicyGeneration != p.Generation || p.Generation < 1 {
		t.Fatalf("answer generation %d, start %d %s; want the enforced start's generation", out.PolicyGeneration, p.Generation, p.State)
	}
	cfg := f.newConfig(t)
	if slices.Contains(cfg.Policy.AllowedDomains, "api.anthropic.com") {
		t.Errorf("allowed_domains = %v; the removed entry must be gone", cfg.Policy.AllowedDomains)
	}
	for _, h := range []string{"api.anthropic.com", "evil.example"} {
		if !slices.Contains(cfg.Policy.DeniedDomains, h) {
			t.Errorf("denied_domains = %v; want %s denied", cfg.Policy.DeniedDomains, h)
		}
	}
	ev := f.audit.eventsFor(f.run.ID, "run.egress.edit")
	if len(ev) != 1 || ev[0].Outcome != "success" {
		t.Fatalf("run.egress.edit rows = %+v, want one success", ev)
	}
	if d := leaseAuditData(t, ev[0]); d["policy_generation"] != float64(p.Generation) {
		t.Errorf("row data = %v, want policy_generation %d", d, p.Generation)
	}
}

// A second edit while the first has not answered is refused 409 egress_edit_in_progress, and only the
// first replaces the proxy.
func TestEgressEdit_ConcurrentEditIsRefused(t *testing.T) {
	f := newEgressEditFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.rr.onReplace = func(cfg []byte) {
		close(entered)
		<-release
		postLoaded(t, f.srv, f.run.ID, f.tokenJTI(t, cfg), hashHex(cfg))
	}
	first := make(chan int, 1)
	go func() { first <- f.editEgress(t, `{"deny_hosts":["evil.example"]}`).Code }()
	<-entered
	w := f.editEgress(t, `{"deny_hosts":["other.example"]}`)
	if w.Code != http.StatusConflict || errorReason(w) != reasonEgressEditInProgress {
		t.Errorf("second edit: %d %s, want 409 %s", w.Code, w.Body.String(), reasonEgressEditInProgress)
	}
	close(release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first edit: %d, want 200", code)
	}
	if n := len(f.rr.replaced); n != 1 {
		t.Errorf("ReplaceProxy calls = %d, want 1", n)
	}
}

// No report by the start's deadline: the edit's new proxy is stopped, the run has no proxy (lost),
// and the answer is 503 egress_edit_unconfirmed.
func TestEgressEdit_NoReadbackLeavesNoProxyAnd503(t *testing.T) {
	f := newEgressEditFixture(t)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.editEgress(t, `{"deny_hosts":["evil.example"]}`) }()
	f.answeredStart(t, 0)
	stops := f.lr.proxyStopCount()
	f.rb.advance(proxyReportGrace)
	w := <-done
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonEgressEditUnconfirmed {
		t.Fatalf("edit: %d %s, want 503 %s", w.Code, w.Body.String(), reasonEgressEditUnconfirmed)
	}
	if got := f.active(t).State; got != types.ProxyStartFailed {
		t.Errorf("start = %s, want failed", got)
	}
	if lostAt, _ := f.st.lost(); lostAt == nil || f.lr.proxyStopCount() <= stops {
		t.Error("the run must be left with no proxy: stopped and lost")
	}
	if ev := f.audit.eventsFor(f.run.ID, "run.egress.edit"); len(ev) != 1 || ev[0].Outcome != "failure" {
		t.Errorf("run.egress.edit rows = %+v, want one failure", ev)
	}
}

// The pre-edit proxy keeps reporting what it loaded, under its own token, before and after the edit's
// driver answers: each report is stale for the edit's start, so it never confirms the edit, and the
// edit ends 503 with no proxy.
func TestEgressEdit_StaleReportFromThePreEditProxyDoesNotConfirm(t *testing.T) {
	f := newEgressEditFixture(t)
	f.confirmEachReplace(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive for the pre-edit proxy: %d, want 200", code)
	}
	_, oldSHA, oldJTI := f.lastHandover(t)
	before := f.active(t)
	if before.State != types.ProxyStartEnforced {
		t.Fatalf("pre-edit start = %s, want enforced", before.State)
	}
	f.now = f.now.Add(reviveLiveEvery) // past the live-revive bound
	f.rr.onReplace = func([]byte) {
		if _, state := postLoaded(t, f.srv, f.run.ID, oldJTI, oldSHA); state != "stale" {
			t.Errorf("the pre-edit proxy's report during the edit: %q, want stale", state)
		}
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.editEgress(t, `{"deny_hosts":["evil.example"]}`) }()
	f.answeredStart(t, before.Generation)
	if _, state := postLoaded(t, f.srv, f.run.ID, oldJTI, oldSHA); state != "stale" {
		t.Errorf("the pre-edit proxy's report after the answer: %q, want stale", state)
	}
	f.rb.advance(proxyReportGrace)
	w := <-done
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonEgressEditUnconfirmed {
		t.Fatalf("edit: %d %s, want 503 %s", w.Code, w.Body.String(), reasonEgressEditUnconfirmed)
	}
	if n := len(f.audit.eventsFor(f.run.ID, "run.policy.enforced")); n != 1 {
		t.Errorf("run.policy.enforced rows = %d, want only the pre-edit start's", n)
	}
	if len(f.audit.eventsFor(f.run.ID, "run.policy.report_stale")) < 2 {
		t.Error("want each pre-edit report audited stale")
	}
	if lostAt, _ := f.st.lost(); lostAt == nil {
		t.Error("the run must be left with no proxy")
	}
}

// Every refusal before the claim leaves the run's proxy untouched, including a body that tries to
// widen: the edit has no field that adds a host.
func TestEgressEdit_RefusedBeforeAnyChange(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		code               int
		setup              func(f *readbackFixture)
	}{
		{"a widening field", `{"add_hosts":["new.example"]}`, reasonInvalidRequestBody, http.StatusBadRequest, nil},
		{"an allowlist field", `{"allowed_domains":["new.example"]}`, reasonInvalidRequestBody, http.StatusBadRequest, nil},
		{"no host", `{"remove_hosts":[],"deny_hosts":[]}`, reasonEgressEditInvalid, http.StatusBadRequest, nil},
		{"a URL, not a host", `{"deny_hosts":["https://evil.example/x"]}`, reasonEgressEditInvalid, http.StatusBadRequest, nil},
		{"a remove of a host not listed", `{"remove_hosts":["other.example"]}`, reasonEgressEditHostNotListed, http.StatusUnprocessableEntity, nil},
		{"a lost run", `{"deny_hosts":["evil.example"]}`, reasonEgressEditRunNotLive, http.StatusConflict, func(f *readbackFixture) {
			lostAt := f.now
			f.st.run.LostAt, f.st.run.LostReason = &lostAt, types.LostOutage
		}},
		{"a substrate that cannot hash", `{"deny_hosts":["evil.example"]}`, reasonEgressEditUnsupported, http.StatusConflict, func(f *readbackFixture) {
			f.srv.cfg.Runner = f.rr
		}},
		{"a legacy proxy image", `{"deny_hosts":["evil.example"]}`, reasonEgressEditLegacyProxy, http.StatusConflict, func(f *readbackFixture) {
			f.srv.cfg.ProxyImage = "ghcr.io/example/wardyn-proxy:0.10.0-rc.17"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEgressEditFixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			w := f.editEgress(t, tc.body)
			if w.Code != tc.code || errorReason(w) != tc.reason {
				t.Fatalf("edit: %d %s, want %d %s", w.Code, w.Body.String(), tc.code, tc.reason)
			}
			if n := len(f.rr.replaced); n != 0 {
				t.Errorf("ReplaceProxy calls = %d, want 0", n)
			}
		})
	}
}

// applyEgressEdits narrows only: the removed entry leaves the allowlist, every edited host joins the
// ceiling's denies, and nothing is added to the allowlist.
func TestApplyEgressEdits_NarrowsOnly(t *testing.T) {
	policy := types.RunPolicySpec{AllowedDomains: []string{"a.example", "B.example", "*.c.example"}}
	c := ownerCeiling{deny: []string{"profile.example"}}
	if rerr := applyEgressEdits(&c, &policy, []liveEgressEdit{{remove: []string{"b.example"}, deny: []string{"d.example"}}}); rerr != nil {
		t.Fatal(rerr)
	}
	if !slices.Equal(policy.AllowedDomains, []string{"a.example", "*.c.example"}) {
		t.Errorf("allowed_domains = %v", policy.AllowedDomains)
	}
	if !slices.Equal(c.deny, []string{"profile.example", "b.example", "d.example"}) {
		t.Errorf("ceiling denies = %v", c.deny)
	}
}

// failPutStore fails the run's proxy-config write while fail is set, as a database blip would.
type failPutStore struct {
	*readbackStore
	fail atomic.Bool
}

func (s *failPutStore) PutRunProxyConfig(ctx context.Context, id uuid.UUID, sealed []byte) error {
	if s.fail.Load() {
		return errors.New("injected config write failure")
	}
	return s.readbackStore.PutRunProxyConfig(ctx, id, sealed)
}

// handedPolicy is the policy in the newest config a proxy was handed.
func (f *readbackFixture) handedPolicy(t *testing.T) types.RunPolicySpec {
	t.Helper()
	raw, _, _ := f.lastHandover(t)
	cfg, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatalf("the handed-over config does not load: %v", err)
	}
	return cfg.Policy
}

// An edit whose config cannot be stored never answers 200: a later revive starts from the stored
// config, so the edit fails closed (503, the new proxy stopped, the run lost). The ordinary revive
// after it then starts from the pre-edit config, which is right only because no edit was confirmed.
func TestEgressEdit_ConfigWriteFailureFailsClosed(t *testing.T) {
	f := newEgressEditFixture(t)
	fs := &failPutStore{readbackStore: f.rb}
	fs.fail.Store(true)
	f.srv.cfg.Store = fs
	f.confirmEachReplace(t)
	stops := f.lr.proxyStopCount()
	w := f.editEgress(t, `{"remove_hosts":["api.anthropic.com"],"deny_hosts":["evil.example"]}`)
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonEgressEditUnconfirmed {
		t.Fatalf("edit with a failed config write: %d %s, want 503 %s", w.Code, w.Body.String(), reasonEgressEditUnconfirmed)
	}
	if lostAt, _ := f.st.lost(); lostAt == nil || f.lr.proxyStopCount() <= stops {
		t.Fatal("the edit's new proxy must be stopped and the run lost")
	}
	fs.fail.Store(false)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("ordinary revive after the failed edit: %d, want 200", code)
	}
	if !slices.Contains(f.handedPolicy(t).AllowedDomains, "api.anthropic.com") {
		t.Error("the ordinary revive must start from the stored, pre-edit config")
	}
	for _, ev := range f.audit.eventsFor(f.run.ID, "run.egress.edit") {
		if ev.Outcome == "success" {
			t.Fatal("an edit whose config was never stored must not answer success")
		}
	}
}

// An edit that answered 200 is in the stored config, so an ordinary revive after it keeps the removed
// host off the allowlist and both hosts denied.
func TestEgressEdit_ConfirmedEditSurvivesAnOrdinaryRevive(t *testing.T) {
	f := newEgressEditFixture(t)
	f.confirmEachReplace(t)
	if w := f.editEgress(t, `{"remove_hosts":["api.anthropic.com"],"deny_hosts":["evil.example"]}`); w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s, want 200", w.Code, w.Body.String())
	}
	f.now = f.now.Add(reviveLiveEvery) // past the live-revive bound
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("ordinary revive after the edit: %d, want 200", code)
	}
	p := f.handedPolicy(t)
	if slices.Contains(p.AllowedDomains, "api.anthropic.com") {
		t.Errorf("allowed_domains = %v: a confirmed removal must survive a later revive", p.AllowedDomains)
	}
	for _, h := range []string{"api.anthropic.com", "evil.example"} {
		if !slices.Contains(p.DeniedDomains, h) {
			t.Errorf("denied_domains = %v: want %s still denied after a later revive", p.DeniedDomains, h)
		}
	}
}
