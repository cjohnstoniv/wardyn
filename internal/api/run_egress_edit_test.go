// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
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

// k8sLikeHashedRunner answers CanReplaceProxy the way the orchestrator does for a Kubernetes
// sandbox, while still passing the edit's own support check (a store with readback, a hashing runner).
type k8sLikeHashedRunner struct{ *hashedReviveRunner }

func (k8sLikeHashedRunner) CanReplaceProxy(context.Context, string) error {
	return runner.ErrReviveUnsupported
}

// A live edit on a substrate that cannot replace a sandbox's proxy (Kubernetes) is refused 409
// egress_edit_unsupported, not the revive's revive_unsupported, and its run.egress.edit row carries
// the same reason. Catches a production change that drops egressEditRefusal's reviveUnsupported
// mapping, or maps only the response and leaves the audit row on revive_unsupported.
func TestEgressEdit_UnsupportedSubstrateAnswersEgressEditUnsupported(t *testing.T) {
	f := newEgressEditFixture(t)
	f.srv.cfg.Runner = k8sLikeHashedRunner{f.hr}
	w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
	if w.Code != http.StatusConflict || errorReason(w) != reasonEgressEditUnsupported {
		t.Fatalf("edit: %d %s, want 409 %s", w.Code, w.Body.String(), reasonEgressEditUnsupported)
	}
	if n := len(f.rr.replaced); n != 0 {
		t.Errorf("ReplaceProxy calls = %d, want 0", n)
	}
	ev := f.audit.eventsFor(f.run.ID, "run.egress.edit")
	if len(ev) != 1 || ev[0].Outcome != "denied" {
		t.Fatalf("run.egress.edit rows = %+v, want one denied", ev)
	}
	if d := leaseAuditData(t, ev[0]); d["reason"] != reasonEgressEditUnsupported {
		t.Errorf("row reason = %v, want %s", d["reason"], reasonEgressEditUnsupported)
	}
}

// unhashableRunner has a ReplaceProxyHashed that refuses at once with runner.ErrReviveUnsupported, as a
// substrate that cannot answer the hash of the bytes it hands a new proxy does.
type unhashableRunner struct{ *hashedReviveRunner }

func (unhashableRunner) ReplaceProxyHashed(context.Context, string, []byte) (string, error) {
	return "", runner.ErrReviveUnsupported
}

// A live edit whose substrate refuses the proxy replace as unable to confirm its policy (the revive's
// revive_readback_unsupported) is also 409 egress_edit_unsupported, with the same reason on its
// run.egress.edit row, and the proxy is not replaced. Catches egressEditRefusal mapping only
// revive_unsupported, which would leave docs/sdk.md's egress_edit_unsupported row inexact.
func TestEgressEdit_ReadbackUnsupportedAnswersEgressEditUnsupported(t *testing.T) {
	f := newEgressEditFixture(t)
	f.srv.cfg.Runner = unhashableRunner{f.hr}
	w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
	if w.Code != http.StatusConflict || errorReason(w) != reasonEgressEditUnsupported {
		t.Fatalf("edit: %d %s, want 409 %s", w.Code, w.Body.String(), reasonEgressEditUnsupported)
	}
	if n := len(f.rr.replaced); n != 0 {
		t.Errorf("ReplaceProxy calls = %d, want 0", n)
	}
	ev := f.audit.eventsFor(f.run.ID, "run.egress.edit")
	if len(ev) != 1 || ev[0].Outcome != "denied" {
		t.Fatalf("run.egress.edit rows = %+v, want one denied", ev)
	}
	if d := leaseAuditData(t, ev[0]); d["reason"] != reasonEgressEditUnsupported {
		t.Errorf("row reason = %v, want %s", d["reason"], reasonEgressEditUnsupported)
	}
}

// An edit whose substrate cannot apply it answers what to do, not the runner's own error text.
// Catches the revive's text leaking back, a change that maps the reason but not the text, and "start a
// new run" given where updating the runner is the fix. The handler cases run a run with no runner; the
// runner cases call egressEditRefusal with egressEditRunnerToUpdate, because a check that an older
// runner can replace a proxy at all may refuse its edit before any replace.
func TestEgressEdit_UnsupportedSaysWhatToDo(t *testing.T) {
	for name, rn := range map[string]func(*readbackFixture) runner.Runner{
		"substrate that cannot replace": func(f *readbackFixture) runner.Runner { return k8sLikeHashedRunner{f.hr} },
		"readback not confirmable":      func(f *readbackFixture) runner.Runner { return unhashableRunner{f.hr} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newEgressEditFixture(t)
			f.srv.cfg.Runner = rn(f)
			w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
			var b errorBody
			if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusConflict || b.Reason != reasonEgressEditUnsupported {
				t.Fatalf("edit: %d %s, want 409 %s", w.Code, w.Body.String(), reasonEgressEditUnsupported)
			}
			if b.Error != egressEditUnsupportedMsg || b.Error == runner.ErrReviveUnsupported.Error() {
				t.Errorf("error = %q, want %q", b.Error, egressEditUnsupportedMsg)
			}
		})
	}

	seed := func(srv *Server, id uuid.UUID, caps runnerwire.Caps) {
		data, err := json.Marshal(caps)
		if err != nil {
			t.Fatal(err)
		}
		sub := remote.New(id.String(), &capsTransport{online: true}, remote.Options{})
		sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
		srv.runnerStreams.subsMu.Lock()
		if srv.runnerStreams.subs == nil {
			srv.runnerStreams.subs = map[uuid.UUID]runnerEndpoint{}
		}
		srv.runnerStreams.subs[id] = runnerEndpoint{sub: sub, link: &remote.Link{}}
		srv.runnerStreams.subsMu.Unlock()
	}
	srv := &Server{}
	kept, unkept, unnamed := uuid.New(), uuid.New(), uuid.New()
	seed(srv, kept, runnerwire.Caps{MachineName: "build-mac"})
	seed(srv, unkept, runnerwire.Caps{MachineName: "build-mac", EndsUnkept: true})
	seed(srv, unnamed, runnerwire.Caps{})
	held := uuid.New() // a runner id no caps were seeded for
	for _, tc := range []struct {
		name string
		run  types.AgentRun
		want string
	}{
		{"container run on a runner that can keep a sandbox", types.AgentRun{RunnerID: &kept}, fmt.Sprintf(egressEditUpdateRunnerMsg, "build-mac")},
		{"the same runner with no name", types.AgentRun{RunnerID: &unnamed}, fmt.Sprintf(egressEditUpdateRunnerMsg, "its computer")},
		{"runner that ends unkept", types.AgentRun{RunnerID: &unkept}, egressEditUnsupportedMsg},
		{"a VM run on that runner", types.AgentRun{RunnerID: &kept, MachineKind: types.MachineMacOSVM}, egressEditUnsupportedMsg},
		{"no runner id", types.AgentRun{}, egressEditUnsupportedMsg},
		{"runner with no caps held", types.AgentRun{RunnerID: &held}, egressEditUnsupportedMsg},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := egressEditRefusal(reviveUnsupported(), srv.egressEditRunnerToUpdate(tc.run))
			if got.msg != tc.want || got.reason != reasonEgressEditUnsupported || got.status != http.StatusConflict {
				t.Errorf("refusal = %d %s %q, want 409 %s %q", got.status, got.reason, got.msg, reasonEgressEditUnsupported, tc.want)
			}
		})
	}
}

// applyEgressEdits narrows only: the removed entry leaves the allowlist, every edited host joins the
// ceiling's denies, and nothing is added to the allowlist.
func TestApplyEgressEdits_NarrowsOnly(t *testing.T) {
	policy := types.RunPolicySpec{AllowedDomains: []string{"a.example", "B.example", "*.c.example"}}
	c := ownerCeiling{deny: []string{"profile.example"}}
	g1, g2, g3, g4 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	cfg := &proxy.Config{Policy: policy, ViaOrgRoutes: []proxy.ViaOrgRoute{
		{GrantID: g1, Host: "a.example"}, {GrantID: g2, Host: "B.example"}, {GrantID: g3, Host: "x.d.example"}, {GrantID: g4, Host: "port.example"},
	}}
	edits := []liveEgressEdit{{remove: []string{"b.example"}, deny: []string{"*.d.example", "port.example:8443"}}}
	if rerr := applyEgressEdits(&c, cfg, edits); rerr != nil {
		t.Fatal(rerr)
	}
	policy = cfg.Policy
	if !slices.Equal(cfg.ViaOrgRoutes, []proxy.ViaOrgRoute{{GrantID: g1, Host: "a.example"}}) {
		t.Errorf("via_org routes = %v: every route on an edited host (exact, wildcard or port-scoped) must go", cfg.ViaOrgRoutes)
	}
	if !slices.Equal(policy.AllowedDomains, []string{"a.example", "*.c.example"}) {
		t.Errorf("allowed_domains = %v", policy.AllowedDomains)
	}
	if !slices.Equal(c.deny, []string{"profile.example", "b.example", "*.d.example", "port.example:8443"}) {
		t.Errorf("ceiling denies = %v", c.deny)
	}
}

// failPutStore fails the run's proxy-config write while fail is set, as a database blip would.
type failPutStore struct {
	*readbackStore
	fail   atomic.Bool
	onFail func() // runs at each failed write, before it answers
}

// AdvanceRunPolicyGeneration fails the config write the advance carries, which takes the generation with it.
func (s *failPutStore) AdvanceRunPolicyGeneration(ctx context.Context, id uuid.UUID, expected store.PolicySlot, digest string, retired []string, fresh string, sealed []byte) (int, error) {
	if sealed != nil && s.fail.Load() {
		if s.onFail != nil {
			s.onFail()
		}
		return 0, errors.New("injected config write failure")
	}
	return s.readbackStore.AdvanceRunPolicyGeneration(ctx, id, expected, digest, retired, fresh, sealed)
}

func (s *failPutStore) PutRunProxyConfig(ctx context.Context, id uuid.UUID, sealed []byte) error {
	if s.fail.Load() {
		if s.onFail != nil {
			s.onFail()
		}
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

// An edit whose config cannot be stored changes nothing: an edit's config, like an ordinary revive's, is
// stored in its generation's own write, before the replace, so a write that fails takes no generation and
// no start, replaces and stops no proxy, and the run keeps its current proxy (502 revive_proxy_kept_current).
// The ordinary revive after it then starts from the pre-edit config, right because no edit took effect.
func TestEgressEdit_ConfigWriteFailureChangesNothing(t *testing.T) {
	f := newEgressEditFixture(t)
	fs := &failPutStore{readbackStore: f.rb}
	fs.fail.Store(true)
	f.srv.cfg.Store = fs
	f.confirmEachReplace(t)
	ctx := context.Background()
	stops, replaced, stored := f.lr.proxyStopCount(), len(f.rr.replaced), string(f.rs.cfg)
	before, _, _ := f.rb.RunPolicyState(ctx, f.run.ID)
	startBefore, hadStart, _ := f.rb.ActiveProxyStart(ctx, f.run.ID)
	w := f.editEgress(t, `{"remove_hosts":["api.anthropic.com"],"deny_hosts":["evil.example"]}`)
	if w.Code != http.StatusBadGateway || errorReason(w) != reasonReviveProxyKeptCurrent {
		t.Fatalf("edit with a failed config write: %d %s, want 502 %s", w.Code, w.Body.String(), reasonReviveProxyKeptCurrent)
	}
	if f.lr.proxyStopCount() != stops || len(f.rr.replaced) != replaced {
		t.Fatal("an edit whose config was never stored must replace and stop no proxy")
	}
	if lostAt, _ := f.st.lost(); lostAt != nil {
		t.Error("an edit whose config was never stored must not lose the run")
	}
	after, _, _ := f.rb.RunPolicyState(ctx, f.run.ID)
	startAfter, hasStart, _ := f.rb.ActiveProxyStart(ctx, f.run.ID)
	if store.NewestPolicyGeneration(after) != store.NewestPolicyGeneration(before) || hasStart != hadStart || startAfter.Generation != startBefore.Generation {
		t.Errorf("generation %d -> %d, start %v %d -> %v %d: a failed config write must take no generation and no start",
			store.NewestPolicyGeneration(before), store.NewestPolicyGeneration(after), hadStart, startBefore.Generation, hasStart, startAfter.Generation)
	}
	if string(f.rs.cfg) != stored {
		t.Error("the stored config changed though its write failed")
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

// An edit retires this replica's cached via_org engine before it answers, and the stored config it
// leaves has no via_org route on the edited host: the next via_org request rebuilds from that config,
// whose engine has no route for the grant (admitViaOrg refuses it), and the new proxy, whose config has
// no route either, never forwards one: the host goes to its own host check, which denies it.
func TestEgressEdit_RetiresTheViaOrgEngineBeforeTheAnswer(t *testing.T) {
	f := newEgressEditFixture(t)
	grant := uuid.New()
	var stored map[string]any
	if err := json.Unmarshal(f.rs.cfg, &stored); err != nil {
		t.Fatal(err)
	}
	stored["via_org_routes"] = []proxy.ViaOrgRoute{{GrantID: grant, Host: "api.anthropic.com"}}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	f.rs.cfg = raw
	// The warm cache: an engine built from the pre-edit config, as a first via_org request leaves it.
	eng, err := proxy.NewViaOrgEngine(context.Background(),
		&proxy.Config{RunID: f.run.ID, ControlPlaneURL: "http://127.0.0.1:9", RunToken: "run-token", DecisionBufferSize: 16},
		proxy.ViaOrgEngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	vr := &viaOrgRun{id: f.run.ID, runner: uuid.New(), done: make(chan struct{}), streams: map[*viaOrgStream]struct{}{},
		bytes: new(atomic.Int64), engine: &viaOrgGen{engine: eng}, routes: []proxy.ViaOrgRoute{{GrantID: grant, Host: "api.anthropic.com"}}}
	f.srv.viaOrg.mu.Lock()
	f.srv.viaOrg.runs = map[uuid.UUID]*viaOrgRun{f.run.ID: vr}
	f.srv.viaOrg.mu.Unlock()
	f.confirmEachReplace(t)
	if w := f.editEgress(t, `{"remove_hosts":["api.anthropic.com"]}`); w.Code != http.StatusOK {
		t.Fatalf("edit: %d %s, want 200", w.Code, w.Body.String())
	}
	vr.mu.Lock()
	retired := vr.engine == nil
	vr.mu.Unlock()
	if !retired {
		t.Fatal("the cached via_org engine built from the pre-edit config is still served after the edit's 200")
	}
	raw, _, _ = f.lastHandover(t)
	handed, err := proxy.LoadConfigBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(handed.ViaOrgRoutes) != 0 {
		t.Errorf("the new proxy still routes %v to the organisation", handed.ViaOrgRoutes)
	}
	stored2, err := proxy.LoadConfigBytes(f.rs.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored2.ViaOrgRoutes) != 0 {
		t.Errorf("the stored config the next engine is built from still routes %v to the organisation", stored2.ViaOrgRoutes)
	}
}

// blockingStopRunner holds every proxy stop while block is set, until gate closes, as a runner whose
// stop is slow to land would.
type blockingStopRunner struct {
	*hashedReviveRunner
	block   atomic.Bool
	gate    chan struct{}
	entered chan struct{} // one signal per stop that reached the gate
}

func (r *blockingStopRunner) StopProxy(ctx context.Context, ref string) error {
	if r.block.Load() {
		select {
		case r.entered <- struct{}{}:
		default:
		}
		<-r.gate
	}
	return r.hashedReviveRunner.StopProxy(ctx, ref)
}

// mismatchAfterAnswer has the edit's new proxy report bytes other than it was handed, after the
// driver answered: the start fails by its report, not by the revive.
func (f *readbackFixture) mismatchAfterAnswer(t *testing.T) {
	t.Helper()
	f.answeredStart(t, 0)
	_, _, jti := f.lastHandover(t)
	go func() { postLoaded(t, f.srv, f.run.ID, jti, hashHex([]byte("other bytes"))) }()
}

// A start the readback failed is not yet a stopped proxy: while its stop has not landed the edit does
// not answer, and once it lands the edit answers 503 egress_edit_unconfirmed (stopped).
func TestEgressEdit_FailedStartAnswersOnlyOnceItsStopLands(t *testing.T) {
	f := newEgressEditFixture(t)
	br := &blockingStopRunner{hashedReviveRunner: f.hr, gate: make(chan struct{}), entered: make(chan struct{}, 4)}
	f.srv.cfg.Runner = br
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.editEgress(t, `{"deny_hosts":["evil.example"]}`) }()
	// The report's handler takes the run's lock and holds the stop; the edit's first read of its failed
	// start waits for that, so the edit never wins the lock and holds the stop itself.
	saw, held := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	hook := egressEditSawUncontained
	egressEditSawUncontained = func() {
		once.Do(func() { <-br.entered; close(held) })
		select {
		case saw <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { egressEditSawUncontained = hook })
	f.answeredStart(t, 0)
	br.block.Store(true)
	f.mismatchAfterAnswer(t)
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the failed start's stop was never held")
	}
	// The barrier: each read of the failed start is the edit going round its wait again (its own
	// containment finds the lock busy), so it did not answer on the reads before it.
	for range 3 {
		select {
		case <-saw:
		case w := <-done:
			t.Fatalf("the edit answered %d %s before its failed start's stop landed", w.Code, w.Body.String())
		case <-time.After(10 * time.Second):
			t.Fatal("the edit stopped reading its start while the stop was held")
		}
	}
	select {
	case w := <-done:
		t.Fatalf("the edit answered %d %s before its failed start's stop landed", w.Code, w.Body.String())
	default:
	}
	close(br.gate)
	w := <-done
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonEgressEditUnconfirmed {
		t.Fatalf("edit: %d %s, want 503 %s", w.Code, w.Body.String(), reasonEgressEditUnconfirmed)
	}
	if p := f.active(t); p.State != types.ProxyStartFailed || p.ContainedAt == nil {
		t.Errorf("start = %s contained_at = %v, want failed and contained", p.State, p.ContainedAt)
	}
}

// A failed start whose stop does not land is answered as unresolved, never as stopped.
func TestEgressEdit_UnconfirmedStopIsUnresolvedNotStopped(t *testing.T) {
	f := newEgressEditFixture(t)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.editEgress(t, `{"deny_hosts":["evil.example"]}`) }()
	f.answeredStart(t, 0)
	f.setProxyStopErr(errors.New("the runner did not answer the stop"))
	f.mismatchAfterAnswer(t)
	w := <-done
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonEgressEditContainmentUnresolved {
		t.Fatalf("edit: %d %s, want 503 %s", w.Code, w.Body.String(), reasonEgressEditContainmentUnresolved)
	}
	if strings.Contains(w.Body.String(), "was stopped") {
		t.Errorf("an unresolved stop must not be answered as stopped: %s", w.Body.String())
	}
}

// A start another revive superseded is that start's to settle: the edit answers without stopping the
// run's newer proxy.
func TestEgressEdit_SupersededStartStopsNothing(t *testing.T) {
	f := newEgressEditFixture(t)
	f.confirmEachReplace(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: %d, want 200", code)
	}
	gen := f.active(t).Generation
	if _, err := f.rb.BeginProxyStart(context.Background(), f.run.ID, gen+1, "newer-jti", store.ProxyStartExpected); err != nil {
		t.Fatal(err)
	}
	stops := f.lr.proxyStopCount()
	rerr := f.srv.awaitEgressEditReadback(context.Background(), f.run.ID, gen, true)
	if rerr == nil || rerr.reason != reasonEgressEditSuperseded || strings.Contains(rerr.msg, "was stopped") {
		t.Fatalf("superseded start: %+v, want egress_edit_superseded that does not claim a stop", rerr)
	}
	if f.lr.proxyStopCount() != stops {
		t.Error("a superseded edit start must not stop the run's newer proxy")
	}
}

// A new proxy that reports other bytes while its driver answers fails the revive itself: the edit
// answers stopped only once that start is contained, and unresolved while its stop does not land.
func TestEgressEdit_RevivesOwnFailedStartAnswersByItsContainment(t *testing.T) {
	for _, stopErr := range []error{nil, errors.New("the runner did not answer the stop")} {
		f := newEgressEditFixture(t)
		f.rr.onReplace = func(cfg []byte) { // once the proxy is replaced: its stop is the containment's
			f.setProxyStopErr(stopErr)
			postLoaded(t, f.srv, f.run.ID, f.tokenJTI(t, cfg), hashHex([]byte("other bytes")))
		}
		w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
		want := reasonEgressEditUnconfirmed
		if stopErr != nil {
			want = reasonEgressEditContainmentUnresolved
		}
		if w.Code != http.StatusServiceUnavailable || errorReason(w) != want {
			t.Fatalf("stop error %v: %d %s, want 503 %s", stopErr, w.Code, w.Body.String(), want)
		}
		if stopErr != nil && strings.Contains(w.Body.String(), "was stopped") {
			t.Errorf("an unresolved stop must not be answered as stopped: %s", w.Body.String())
		}
	}
}

// The edited config is stored before the settle: an edit whose settle fails (its new proxy reports
// other bytes) answers 503, and the revive that brings the run back starts from the edited config.
func TestEgressEdit_ReviveAfterAFailedSettleStartsFromTheEdit(t *testing.T) {
	f := newEgressEditFixture(t)
	f.rr.onReplace = func(cfg []byte) { postLoaded(t, f.srv, f.run.ID, f.tokenJTI(t, cfg), hashHex([]byte("other bytes"))) }
	if w := f.editEgress(t, `{"remove_hosts":["api.anthropic.com"],"deny_hosts":["evil.example"]}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("edit with a failed settle: %d %s, want 503", w.Code, w.Body.String())
	}
	f.confirmEachReplace(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive after the failed edit: %d, want 200", code)
	}
	p := f.handedPolicy(t)
	if slices.Contains(p.AllowedDomains, "api.anthropic.com") || !slices.Contains(p.DeniedDomains, "evil.example") {
		t.Errorf("the revive after a failed settle started from the pre-edit config: allowed %v denied %v", p.AllowedDomains, p.DeniedDomains)
	}
}

// An ordinary revive now stores its config before the replace, in its generation's own write, as an edit
// does: while the new proxy is being started, the stored config already carries that proxy's token. A
// write that fails is the revive's: it replaces nothing and the live run keeps its current proxy.
func TestEgressEdit_OrdinaryReviveStoresItsConfigBeforeTheReplace(t *testing.T) {
	f := newEgressEditFixture(t)
	var storedToken, handedToken string
	f.rr.onReplace = func(cfg []byte) {
		handedToken = f.tokenJTI(t, cfg)
		f.rs.mu.Lock()
		raw := slices.Clone(f.rs.cfg)
		f.rs.mu.Unlock()
		storedToken = f.tokenJTI(t, raw)
		postLoaded(t, f.srv, f.run.ID, handedToken, hashHex(cfg))
	}
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: %d, want 200", code)
	}
	if handedToken == "" || storedToken != handedToken {
		t.Fatalf("during the replace the stored config's token was %q and the new proxy's %q: the config must be stored before the replace", storedToken, handedToken)
	}
	fs := &failPutStore{readbackStore: f.rb}
	fs.fail.Store(true)
	f.srv.cfg.Store = fs
	f.now = f.now.Add(reviveLiveEvery) // past the live-revive bound
	stops, replaced := f.lr.proxyStopCount(), len(f.rr.replaced)
	w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
	if w.Code != http.StatusBadGateway || errorReason(w) != reasonReviveProxyKeptCurrent {
		t.Fatalf("revive with a failed config write: %d %s, want 502 %s", w.Code, w.Body.String(), reasonReviveProxyKeptCurrent)
	}
	if f.lr.proxyStopCount() != stops || len(f.rr.replaced) != replaced {
		t.Error("a revive whose config write failed must replace and stop no proxy")
	}
}

// A replace that fails puts the run back to lost; the edit answers from its own confirmed stop, never
// from the lost mark's wording, and unresolved when that stop does not land.
func TestEgressEdit_ReplaceFailureAnswersFromTheConfirmedStop(t *testing.T) {
	for _, stopErr := range []error{nil, errors.New("the runner did not answer the stop")} {
		f := newEgressEditFixture(t)
		f.rr.replaceErr = runner.ErrProxyReplaceFailed
		f.setProxyStopErr(stopErr)
		w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
		want := reasonEgressEditUnconfirmed
		if stopErr != nil {
			want = reasonEgressEditContainmentUnresolved
		}
		if w.Code != http.StatusServiceUnavailable || errorReason(w) != want {
			t.Fatalf("stop error %v: %d %s, want 503 %s", stopErr, w.Code, w.Body.String(), want)
		}
	}
}

// A stopped edit names the fate the store holds, never one it guessed; an unresolved one that recorded
// none says the run will be lost until revived where it can be kept, and ended where it cannot.
func TestEgressEdit_AnswerNamesTheRunsFate(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name        string
		stored      func(*readbackFixture)
		interactive bool
		rerr        *reviveError
		want, never string
	}{
		{"stored lost", func(f *readbackFixture) { f.st.run.LostAt = &now }, true, egressEditUnconfirmed("start_failed"), "is lost until revived", "ended"},
		{"stored ended", func(f *readbackFixture) { f.st.state = types.RunFailed }, false, egressEditUnconfirmed("start_failed"), "is ended", "lost until revived"},
		{"stop confirmed, nothing recorded", func(*readbackFixture) {}, false, egressEditUnconfirmed("start_failed"), "", "ended"},
		{"unresolved, kept", func(*readbackFixture) {}, true, egressEditUnresolved("start_failed", nil), "will be lost until revived", "is lost"},
		{"unresolved, not kept", func(*readbackFixture) {}, false, egressEditUnresolved("start_failed", nil), "will be ended", "is ended"},
	} {
		f := newEgressEditFixture(t)
		c.stored(f)
		run := types.AgentRun{ID: f.run.ID, State: types.RunRunning, Interactive: c.interactive, SandboxRef: "sbx"}
		got := f.srv.withRunFate(context.Background(), run, c.rerr).msg
		if !strings.Contains(got, c.want) || strings.Contains(got, c.never) {
			t.Errorf("%s: %q, want it to say %q and never %q", c.name, got, c.want, c.never)
		}
	}
	f := newEgressEditFixture(t)
	if got := f.srv.withRunFate(context.Background(), f.run, egressEditSuperseded(true)).msg; strings.Contains(got, "ended") {
		t.Errorf("a superseded edit stopped nothing, so it names no fate: %q", got)
	}
}

// failMarkStore is readbackStore whose cleared sandbox ref, when st is set, is recorded on st as the store does once a
// teardown succeeds, and whose lost mark and run end writes fail or do not apply on request.
type failMarkStore struct {
	*readbackStore
	st             *leaseStore
	fail           atomic.Bool
	notApplied     atomic.Bool // the mark answers not applied, as when another writer moved the run first
	endNotApplied  atomic.Bool // a kept run's teardown write does not apply
	finalizeRefuse atomic.Bool // a FAILED transition does not apply, as when another writer moved the run first
}

func (s *failMarkStore) SetSandboxRef(ctx context.Context, id uuid.UUID, ref string) error {
	if s.st == nil {
		return s.readbackStore.SetSandboxRef(ctx, id, ref)
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	s.st.run.SandboxRef = ref
	return nil
}

func (s *failMarkStore) StopKeptRunIf(ctx context.Context, id uuid.UUID, to types.RunState, lostAt *time.Time, reason types.LostReason, endsAt *time.Time) (bool, error) {
	if s.endNotApplied.Load() {
		return false, nil
	}
	return s.readbackStore.StopKeptRunIf(ctx, id, to, lostAt, reason, endsAt)
}

func (s *failMarkStore) UpdateRunStateIf(ctx context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	if s.finalizeRefuse.Load() && to == types.RunFailed {
		return false, nil
	}
	return s.readbackStore.UpdateRunStateIf(ctx, id, from, to)
}

func (s *failMarkStore) MarkRunLost(ctx context.Context, id uuid.UUID, reason types.LostReason, at time.Time, life time.Duration) (bool, error) {
	if s.fail.Load() {
		return false, errors.New("injected lost-mark failure")
	}
	if s.notApplied.Load() {
		return false, nil
	}
	return s.readbackStore.MarkRunLost(ctx, id, reason, at, life)
}

func (s *failMarkStore) MarkRunLostAtGeneration(ctx context.Context, id uuid.UUID, gen int, reason types.LostReason, at time.Time, life time.Duration) (bool, error) {
	if s.fail.Load() {
		return false, errors.New("injected lost-mark failure")
	}
	if s.notApplied.Load() {
		return false, nil
	}
	return s.readbackStore.MarkRunLostAtGeneration(ctx, id, gen, reason, at, life)
}

// A failed replace whose proxy stop also fails is never answered as stopped, whether the lost mark is
// written or not. (The egress edit's own answers are pinned by TestEgressEdit_ReplaceFailureAnswersFromTheConfirmedStop.)
func TestRevive_FailedReplaceWithAFailedStopIsNotStopped(t *testing.T) {
	for _, markFails := range []bool{false, true} {
		f := newReadbackFixture(t)
		fs := &failMarkStore{readbackStore: f.rb}
		fs.fail.Store(markFails)
		f.srv.cfg.Store = fs
		f.rr.replaceErr = runner.ErrProxyReplaceFailed
		f.setProxyStopErr(errors.New("the runner did not answer the stop"))
		want := reasonReviveProxyReplaceFailedLost
		if markFails {
			want = reasonReviveRecoveryUnresolved
		}
		w := do(t, f.srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/revive", adminToken, "")
		body := w.Body.String()
		if errorReason(w) != want || !strings.Contains(body, "not confirmed") ||
			strings.Contains(body, "was stopped") || strings.Contains(body, "has no egress") {
			t.Errorf("mark fails=%v: %d %s, want %s saying the stop is not confirmed", markFails, w.Code, body, want)
		}
	}
}

// failSandboxStopRunner fails every sandbox stop, as a daemon that does not answer would.
type failSandboxStopRunner struct{ *hashedReviveRunner }

func (r *failSandboxStopRunner) StopSandbox(context.Context, string) error {
	return errors.New("the daemon did not answer the stop")
}

// failedAfterClaim makes the fixture's run live, with a store and runner that persist fate when a revive fails after
// its claim: lost (the mark written), unknown (the mark not applied), ended (a headless run on a remote runner,
// which the mark cannot keep, finalized; "ended, stop fails" also fails its sandbox stops), or torn down (the mark
// written on a substrate that cannot keep a sandbox, so the run is torn down). "torn down, end not applied" keeps
// the run lost when that teardown's write does not apply, and "ended, end refused" leaves it running when the
// finalize's transition does not.
func (f *readbackFixture) failedAfterClaim(fate string) {
	fs := &failMarkStore{readbackStore: f.rb, st: f.st}
	f.srv.cfg.Store = fs
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	f.st.run.LostAt, f.st.run.LostReason = nil, ""
	switch fate {
	case "unknown":
		fs.notApplied.Store(true)
	case "torn down, end not applied":
		fs.endNotApplied.Store(true)
		fallthrough
	case "torn down":
		f.setProxyStopErr(runner.ErrEndUnsupported)
	case "ended, end refused":
		fs.finalizeRefuse.Store(true)
		f.st.run.Interactive, f.st.run.Placement = false, types.PlacementRemote
	case "ended, stop fails":
		f.srv.cfg.Runner = &failSandboxStopRunner{hashedReviveRunner: f.hr}
		fallthrough
	case "ended":
		f.st.run.Interactive, f.st.run.Placement = false, types.PlacementRemote
	}
	f.run = f.st.run
}

// A bulk restart that fails after its claim answers the run's fate as persisted, through a failed replace and through
// a failed readback (whose stop clause never replaces that fate), and its lost_again is true only once the lost mark
// was written and the run kept, as the audit row's is. Ended is claimed only once the end landed, and a replace's
// stop not confirmed is named so, with containment unresolved on its row.
func TestRestart_FailureAfterTheClaimAnswersThePersistedFate(t *testing.T) {
	for _, readback := range []bool{false, true} {
		for _, c := range []struct {
			fate      string
			lostAgain bool
			say       string
			replaceNC bool // a failed replace's stop is not confirmed
			ended     bool
		}{
			{"lost", true, "lost until revived", false, false},
			{"unknown", false, "being lost or ended by another operation", true, false},
			{"ended", false, "being ended", false, true},
			{"ended, stop fails", false, "being ended", true, true},
			{"ended, end refused", false, "being lost or ended by another operation", true, false},
			{"torn down", false, "being ended", false, true},
			{"torn down, end not applied", true, "lost until revived", true, false},
		} {
			f := newReadbackFixture(t)
			f.failedAfterClaim(c.fate)
			want := reasonReviveProxyReplaceFailedLost
			if readback {
				f.hr.hash, want = func([]byte) string { return "" }, reasonRevivePolicyUnconfirmedLost
			} else {
				f.rr.replaceErr = runner.ErrProxyReplaceFailed
			}
			res := f.restart(t)
			if res.OK || res.Reason != want || res.LostAgain != c.lostAgain {
				t.Errorf("readback=%v %s: %+v, want %s and lost_again %v", readback, c.fate, res, want, c.lostAgain)
			}
			if readback {
				// A failed readback words its fate from the store (storedRunFate) and its stop from its own
				// containment, so only the lost claim is pinned here.
				if strings.Contains(res.Error, "lost until revived") != c.lostAgain {
					t.Errorf("readback=%v %s: %q, want it to say lost until revived only for a lost run", readback, c.fate, res.Error)
				}
			} else {
				if !strings.Contains(res.Error, c.say) || (c.say != "lost until revived" && strings.Contains(res.Error, "lost until revived")) ||
					strings.Contains(res.Error, "not confirmed") != c.replaceNC {
					t.Errorf("readback=%v %s: %q, want it to say %q and name the stop not confirmed only when it did not land (%v)",
						readback, c.fate, res.Error, c.say, c.replaceNC)
				}
			}
			// A readback failure's stop is reported on its run.policy.unconfirmed row, not on run.revive.
			ev := f.audit.eventsFor(f.run.ID, "run.revive")
			if len(ev) != 1 {
				t.Fatalf("readback=%v %s: run.revive rows %+v, want one", readback, c.fate, ev)
			}
			data := leaseAuditData(t, ev[0])
			if _, unresolved := data["containment"]; data["lost_again"] != c.lostAgain || unresolved != (!readback && c.replaceNC) {
				t.Errorf("readback=%v %s: run.revive data %v, want lost_again %v and containment only for an unconfirmed replace stop",
					readback, c.fate, data, c.lostAgain)
			}
			if ended := isTerminalRunState(f.st.State()); ended != c.ended {
				t.Errorf("readback=%v %s: state %s, want terminal only for an ended fate", readback, c.fate, f.st.State())
			}
		}
	}
}

// A failed start of a run torn down with its lost_at kept is contained by a sandbox stop, never retried through a
// proxy stop its substrate refuses, so it leaves the overdue sweep's list.
func TestReadback_TornDownRunWithLostAtIsContained(t *testing.T) {
	f := newReadbackFixture(t)
	f.failedAfterClaim("torn down")
	f.hr.hash = func([]byte) string { return "" }
	if res := f.restart(t); res.OK || res.LostAgain || strings.Contains(res.Error, "not confirmed") {
		t.Fatalf("restart: %+v, want refused, not lost again, with its stop confirmed", res)
	}
	if lostAt, _ := f.st.lost(); lostAt == nil || !isTerminalRunState(f.st.State()) {
		t.Fatalf("lost_at %v, state %s: want the run torn down with its lost_at kept", lostAt, f.st.State())
	}
	if p := f.active(t); p.State != types.ProxyStartFailed || p.ContainedAt == nil {
		t.Errorf("start = %s contained_at = %v, want failed and contained", p.State, p.ContainedAt)
	}
	stops := f.lr.proxyStopCount()
	if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if overdue, err := f.rb.OverdueProxyStarts(context.Background()); err != nil || len(overdue) != 0 {
		t.Errorf("overdue starts = %+v (%v), want none: a contained start is not retried", overdue, err)
	}
	if n := f.lr.proxyStopCount() - stops; n != 0 {
		t.Errorf("the sweep asked for %d more proxy stops, want none", n)
	}
}

// A failed start whose runner refuses a proxy stop alone (runner.ErrEndUnsupported, as a Kubernetes server does) has
// its run torn down and its sandbox stopped on the report's own pass, and is contained there, never left unresolved
// for the sweep as an unconfirmed proxy stop.
func TestReadback_FailedStartOfATornDownRunIsContainedAtOnce(t *testing.T) {
	f := newReadbackFixture(t)
	f.srv.cfg.Store = &failMarkStore{readbackStore: f.rb, st: f.st}
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("revive: code %d, want 200", code)
	}
	_, _, jti := f.lastHandover(t)
	f.setProxyStopErr(runner.ErrEndUnsupported)
	sandboxStops := f.lr.stopCount()
	if code, state := postLoaded(t, f.srv, f.run.ID, jti, hashHex([]byte("other bytes"))); code != http.StatusOK || state != string(types.ProxyStartFailed) {
		t.Fatalf("mismatched report: %d %q, want 200 failed", code, state)
	}
	if n := f.lr.stopCount() - sandboxStops; n < 1 {
		t.Errorf("sandbox stops +%d on the first pass, want the sandbox stopped", n)
	}
	if p := f.active(t); p.ContainedAt == nil || p.ContainmentError != "" {
		t.Errorf("start contained %v, error %q; want contained on the report's own pass", p.ContainedAt, p.ContainmentError)
	}
	if !isTerminalRunState(f.st.State()) {
		t.Errorf("state %s, want the run torn down", f.st.State())
	}
}

// failEndStore fails every write ending the run, as a database blip would.
type failEndStore struct {
	*readbackStore
}

func (s *failEndStore) UpdateRunStateIf(ctx context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	if to.IsTerminal() {
		return false, errors.New("injected end-write failure")
	}
	return s.readbackStore.UpdateRunStateIf(ctx, id, from, to)
}

// A headless run's edit whose replace fails has its proxy stopped; with the end never written, the
// edit's retries run out and the answer says so, never that the run is or will be ended.
func TestEgressEdit_AnUnwrittenEndClaimsNoFate(t *testing.T) {
	f := newEgressEditFixture(t)
	f.st.run.Interactive, f.run.Interactive = false, false
	f.srv.cfg.Store = &failEndStore{readbackStore: f.rb}
	f.rr.replaceErr = runner.ErrProxyReplaceFailed
	w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
	body := w.Body.String()
	if errorReason(w) != reasonEgressEditUnconfirmed || !strings.Contains(body, "end could not be recorded yet") || !strings.Contains(body, "once its token lapses") ||
		strings.Contains(body, "is ended") || strings.Contains(body, "will be ended") || strings.Contains(body, "lost until revived") {
		t.Errorf("%d %s: want %s saying the end could not be recorded and when the lapsed-token sweep ends the run", w.Code, body, reasonEgressEditUnconfirmed)
	}
}

// blindAfterEndStore fails every write ending the run and, from the second (the edit's own end, after
// its proxy stop; the revive's failure tried the first), every read of it.
type blindAfterEndStore struct {
	*readbackStore
	ends  atomic.Int32
	blind atomic.Bool
}

func (s *blindAfterEndStore) UpdateRunStateIf(ctx context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	if to.IsTerminal() {
		if s.ends.Add(1) >= 2 {
			s.blind.Store(true)
		}
		return false, errors.New("injected end-write failure")
	}
	return s.readbackStore.UpdateRunStateIf(ctx, id, from, to)
}

func (s *blindAfterEndStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if s.blind.Load() {
		return types.AgentRun{}, errors.New("injected read failure")
	}
	return s.readbackStore.GetRun(ctx, id)
}

// An edit's retry whose read of the run fails never loses the proxy stop already confirmed, and
// never promises an end.
func TestEgressEdit_AFailedRetryReadKeepsTheConfirmedStop(t *testing.T) {
	f := newEgressEditFixture(t)
	f.st.run.Interactive, f.run.Interactive = false, false
	bs := &blindAfterEndStore{readbackStore: f.rb}
	f.srv.cfg.Store = bs
	f.rr.replaceErr = runner.ErrProxyReplaceFailed
	stops := f.lr.proxyStopCount()
	w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
	body := w.Body.String()
	if n := f.lr.proxyStopCount(); n <= stops {
		t.Errorf("StopProxy = %d, want more than %d: the edit's own stop", n, stops)
	}
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonEgressEditUnconfirmed ||
		!strings.Contains(body, "so it was stopped and the run has no egress") || !strings.Contains(body, "end could not be recorded yet") ||
		strings.Contains(body, "not confirmed yet") || strings.Contains(body, "will be ended") || strings.Contains(body, "is ended") {
		t.Errorf("%d %s: want 503 %s naming the confirmed stop and the unrecorded end, and no end", w.Code, body, reasonEgressEditUnconfirmed)
	}
}

// failLostMarkStore fails the run's lost mark, as a database blip would.
type failLostMarkStore struct {
	*readbackStore
}

func (s *failLostMarkStore) MarkRunLost(context.Context, uuid.UUID, types.LostReason, time.Time, time.Duration) (bool, error) {
	return false, errors.New("injected lost-mark failure")
}

func (s *failLostMarkStore) MarkRunLostAtGeneration(context.Context, uuid.UUID, int, types.LostReason, time.Time, time.Duration) (bool, error) {
	return false, errors.New("injected lost-mark failure")
}

// An edit whose replace fails and whose lost mark is not written, though its proxy stop lands, is
// unresolved and never says the run is lost: that outcome was not recorded.
func TestEgressEdit_AnUnwrittenLostMarkClaimsNoFate(t *testing.T) {
	f := newEgressEditFixture(t)
	f.srv.cfg.Store = &failLostMarkStore{readbackStore: f.rb}
	f.rr.replaceErr = runner.ErrProxyReplaceFailed
	w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
	body := w.Body.String()
	if errorReason(w) != reasonEgressEditContainmentUnresolved || strings.Contains(body, "is lost until revived") || strings.Contains(body, "is ended") {
		t.Errorf("%d %s: want %s that names no recorded fate", w.Code, body, reasonEgressEditContainmentUnresolved)
	}
}

// supersedeOnAbandonStore has another revive begin a newer start the moment the edit's start is
// abandoned, as a revive that raced a failed replace would.
type supersedeOnAbandonStore struct {
	*readbackStore
}

func (s *supersedeOnAbandonStore) AbandonProxyStart(ctx context.Context, runID uuid.UUID, gen int) error {
	if err := s.readbackStore.AbandonProxyStart(ctx, runID, gen); err != nil {
		return err
	}
	_, err := s.BeginProxyStart(ctx, runID, gen+1, "newer-jti", store.ProxyStartExpected)
	return err
}

// An edit stores its config in its generation's own write, before the replace. So one whose replace failed
// and that another revive then superseded was stored, and that revive started from it: the answer says so,
// never "send it again". One whose config write failed took no generation and no start, so nothing can
// supersede it: it answers that the run keeps its current proxy.
func TestEgressEdit_SupersededAfterAFailedReplaceStartsFromTheEditedConfig(t *testing.T) {
	for _, failure := range []string{"replace", "config write"} {
		f := newEgressEditFixture(t)
		switch failure {
		case "replace":
			f.srv.cfg.Store = &supersedeOnAbandonStore{readbackStore: f.rb}
			f.rr.replaceErr = runner.ErrProxyReplaceFailed
		default:
			fs := &failPutStore{readbackStore: f.rb}
			fs.fail.Store(true)
			f.srv.cfg.Store = fs
		}
		w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
		body := w.Body.String()
		switch failure {
		case "replace":
			if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonEgressEditSuperseded ||
				!strings.Contains(body, "edited config") || strings.Contains(body, "not applied") {
				t.Errorf("superseded after a failed replace: %d %s, want %s saying the run starts from the edited config", w.Code, body, reasonEgressEditSuperseded)
			}
		default:
			if w.Code != http.StatusBadGateway || errorReason(w) != reasonReviveProxyKeptCurrent {
				t.Errorf("a failed config write: %d %s, want 502 %s", w.Code, body, reasonReviveProxyKeptCurrent)
			}
		}
	}
}

// A live edit of a No network run is refused 409 egress_edit_no_network before anything else: it has nothing to
// narrow, so no proxy is replaced or reloaded, and the run.egress.edit row carries the reason. A run with
// another preset still reaches the substrate check. Catches the check placed after the revive.
func TestEgressEdit_NoNetworkRunIsRefused(t *testing.T) {
	const msg = "This run has no network, so an edit has nothing to change. Start a new run with the policy you want."
	f := newEgressEditFixture(t)
	f.st.run.WallPreset = &types.WallPresetOnRun{ID: types.WallPresetNoNetwork, Name: "No network", Scope: types.WallPresetScopeBuiltin}
	w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
	if w.Code != http.StatusConflict || errorReason(w) != reasonEgressEditNoNetwork || !strings.Contains(w.Body.String(), msg) {
		t.Fatalf("edit: %d %s, want 409 %s saying %q", w.Code, w.Body.String(), reasonEgressEditNoNetwork, msg)
	}
	if n := len(f.rr.replaced); n != 0 {
		t.Errorf("ReplaceProxy calls = %d, want 0", n)
	}
	ev := f.audit.eventsFor(f.run.ID, "run.egress.edit")
	if len(ev) != 1 || ev[0].Outcome != "denied" {
		t.Fatalf("run.egress.edit rows = %+v, want one denied", ev)
	}
	if d := leaseAuditData(t, ev[0]); d["reason"] != reasonEgressEditNoNetwork {
		t.Errorf("row reason = %v, want %s", d["reason"], reasonEgressEditNoNetwork)
	}

	// Another preset goes on to the substrate check, which a runner that cannot hash refuses.
	g := newEgressEditFixture(t)
	g.st.run.WallPreset = &types.WallPresetOnRun{ID: "other", Name: "Other", Scope: types.WallPresetScopeBuiltin}
	g.srv.cfg.Runner = g.rr
	if w := g.editEgress(t, `{"deny_hosts":["evil.example"]}`); w.Code != http.StatusConflict || errorReason(w) != reasonEgressEditUnsupported {
		t.Errorf("another preset: %d %s, want 409 %s from the substrate check", w.Code, w.Body.String(), reasonEgressEditUnsupported)
	}
}
