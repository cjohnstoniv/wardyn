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
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// narrowRunner is the fake runner of the egress edit tests that also narrows a proxy in place. Its
// proxy behaves as the real one does: it started from base, holds a deny list that only grows, and
// answers the hash of base with that list. The embedded runner stays the Docker-style one.
type narrowRunner struct {
	*hashedReviveRunner
	mu              sync.Mutex
	base            []byte
	list            []string
	probes, narrows int
	sent            [][]string
	notOffered      bool
	probeErr        map[int]error // by 1-based call number
	narrowErr       error         // the delivery fails with it and the list stays as it was
	wrongHash       bool          // the delivery answers a hash the proxy does not hold
	onProbe         func(call int)
	report          func(hash string) // the proxy's own report of what it now enforces
	afterNarrow     func()
}

var _ runner.ProxyNarrower = (*narrowRunner)(nil)

func (r *narrowRunner) state() runner.ProxyNarrowState {
	base, _ := proxy.LiveDenyBaseSHA256(r.base)
	eff, _ := proxy.WithLiveDenies(r.base, r.list)
	return runner.ProxyNarrowState{ConfigSHA256: proxy.ConfigSHA256(eff), BaseSHA256: base, LiveDenies: slices.Clone(r.list)}
}

func (r *narrowRunner) ProxyNarrowState(context.Context, string) (runner.ProxyNarrowState, error) {
	r.mu.Lock()
	r.probes++
	call, hook, notOffered, perr := r.probes, r.onProbe, r.notOffered, r.probeErr[r.probes]
	r.mu.Unlock()
	if notOffered {
		return runner.ProxyNarrowState{}, runner.ErrNarrowNotOffered
	}
	if hook != nil {
		hook(call)
	}
	if perr != nil {
		return runner.ProxyNarrowState{}, perr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state(), nil
}

func (r *narrowRunner) NarrowProxy(_ context.Context, _ string, denies []string) (runner.ProxyNarrowState, error) {
	r.mu.Lock()
	r.narrows++
	r.sent = append(r.sent, slices.Clone(denies))
	if r.narrowErr != nil {
		err := r.narrowErr
		r.mu.Unlock()
		return runner.ProxyNarrowState{}, err
	}
	next, err := proxy.NormalizeLiveDenies(slices.Concat(r.list, denies))
	if err != nil {
		r.mu.Unlock()
		return runner.ProxyNarrowState{}, fmt.Errorf("%w: %v", runner.ErrNarrowRefused, err)
	}
	changed := len(next) != len(r.list)
	r.list = next
	st := r.state()
	st.Changed = changed
	report, after := r.report, r.afterNarrow
	r.mu.Unlock()
	if report != nil {
		report(st.ConfigSHA256)
	}
	if r.wrongHash {
		st.ConfigSHA256 = "not-the-hash"
	}
	if after != nil {
		after()
	}
	return st, nil
}

func (r *narrowRunner) counts() (probes, narrows int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.probes, r.narrows
}

// narrowStore wraps the fixture's store with the faults an in-place edit must survive.
type narrowStore struct {
	*readbackStore
	advanceErr     error // AdvanceRunPolicyGeneration fails with it without writing
	commitThenFail bool  // it commits and then fails, as a lost acknowledgement would
	policyErr      error
	startErr       error
	afterBegin     func()
	begun          []string
	mu2            sync.Mutex
}

func (s *narrowStore) AdvanceRunPolicyGeneration(ctx context.Context, id uuid.UUID, expected store.PolicySlot, digest string, retired []string, fresh string, sealed []byte) (int, error) {
	if s.advanceErr != nil {
		return 0, s.advanceErr
	}
	gen, err := s.readbackStore.AdvanceRunPolicyGeneration(ctx, id, expected, digest, retired, fresh, sealed)
	if err == nil && s.commitThenFail {
		return 0, errors.New("commit acknowledgement lost")
	}
	return gen, err
}

func (s *narrowStore) RunPolicyState(ctx context.Context, id uuid.UUID) (types.RunPolicyState, bool, error) {
	if s.policyErr != nil {
		return types.RunPolicyState{}, false, s.policyErr
	}
	return s.readbackStore.RunPolicyState(ctx, id)
}

func (s *narrowStore) ActiveProxyStart(ctx context.Context, id uuid.UUID) (store.ProxyStart, bool, error) {
	if s.startErr != nil {
		return store.ProxyStart{}, false, s.startErr
	}
	return s.readbackStore.ActiveProxyStart(ctx, id)
}

func (s *narrowStore) BeginProxyStart(ctx context.Context, id uuid.UUID, gen int, jti, mode string, inPlace bool) (store.ProxyStart, error) {
	s.mu2.Lock()
	s.begun = append(s.begun, jti)
	s.mu2.Unlock()
	p, err := s.readbackStore.BeginProxyStart(ctx, id, gen, jti, mode, inPlace)
	if s.afterBegin != nil {
		s.afterBegin()
	}
	return p, err
}

type narrowFixture struct {
	*readbackFixture
	nr   *narrowRunner
	ns   *narrowStore
	orig []byte // the run's stored config before any edit
	jti  string
}

// newNarrowFixture is a live Kubernetes-style run: its proxy started from the stored config with an
// empty deny list, and its first start is enforced under generation 1.
func newNarrowFixture(t *testing.T) *narrowFixture {
	t.Helper()
	f := newEgressEditFixture(t)
	f.ls.mu.Lock()
	f.ls.jtis = []string{"jti-live", ""}
	f.ls.mu.Unlock()
	nr := &narrowRunner{hashedReviveRunner: f.hr, base: slices.Clone(f.rs.cfg)}
	f.srv.cfg.Runner = nr
	ns := &narrowStore{readbackStore: f.rb}
	f.srv.cfg.Store = ns
	nf := &narrowFixture{readbackFixture: f, nr: nr, ns: ns, orig: slices.Clone(f.rs.cfg), jti: "jti-live"}
	nr.report = func(hash string) {
		p, ok, _ := f.rb.ActiveProxyStart(context.Background(), f.run.ID)
		if !ok {
			t.Error("the proxy reported with no start recorded")
			return
		}
		postLoaded(t, f.srv, f.run.ID, p.JTI, hash)
	}
	ctx := context.Background()
	if _, err := f.rb.BeginProxyStart(ctx, f.run.ID, 1, nf.jti, store.ProxyStartExpected, false); err != nil {
		t.Fatal(err)
	}
	sha := proxy.ConfigSHA256(f.rs.cfg)
	if _, err := f.rb.AnswerProxyStart(ctx, f.run.ID, 1, sha, proxyReportGrace); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rb.ReportProxyStart(ctx, f.run.ID, nf.jti, sha); err != nil {
		t.Fatal(err)
	}
	if p := f.active(t); p.State != types.ProxyStartEnforced {
		t.Fatalf("fixture: first start = %s, want enforced", p.State)
	}
	return nf
}

// setStored replaces the run's stored config, and the config its proxy started from.
func (f *narrowFixture) setStored(t *testing.T, mutate func(*proxy.Config)) {
	t.Helper()
	cfg, err := proxy.LoadConfigBytes(f.rs.cfg)
	if err != nil {
		t.Fatalf("the stored config does not load: %v", err)
	}
	mutate(cfg)
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proxy.LoadConfigBytes(b); err != nil {
		t.Fatalf("the edited stored config does not load: %v", err)
	}
	f.rs.mu.Lock()
	f.rs.cfg = b
	f.rs.mu.Unlock()
	// The proxy started from the config without the list: that is what a live list is added to.
	cfg.LiveDenies = nil
	base, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.nr.mu.Lock()
	f.nr.base = base
	f.nr.mu.Unlock()
	f.orig = slices.Clone(b)
}

func (f *narrowFixture) stored(t *testing.T) []byte {
	t.Helper()
	b, err := f.srv.loadRunProxyConfig(context.Background(), f.run.ID)
	if err != nil {
		t.Fatalf("read the stored config: %v", err)
	}
	return b
}

func (f *narrowFixture) storedDenies(t *testing.T) []string {
	t.Helper()
	cfg, err := proxy.LoadConfigBytes(f.stored(t))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.LiveDenies
}

func (f *narrowFixture) gens() []types.PolicyGeneration {
	f.rb.generationStore.gmu.Lock()
	defer f.rb.generationStore.gmu.Unlock()
	return slices.Clone(f.rb.generationStore.state.Generations)
}

// nothingChanged: no generation, no start and no delivery, and the stored config as it was.
func (f *narrowFixture) nothingChanged(t *testing.T) {
	t.Helper()
	if g := f.gens(); len(g) != 0 {
		t.Errorf("generations = %+v, want none written", g)
	}
	if _, n := f.nr.counts(); n != 0 {
		t.Errorf("NarrowProxy ran %d times, want none", n)
	}
	if got := f.stored(t); string(got) != string(f.orig) {
		t.Errorf("the stored config changed:\n%s", got)
	}
	if p, ok, _ := f.rb.ActiveProxyStart(context.Background(), f.run.ID); ok && p.Generation != 1 {
		t.Errorf("active start is generation %d, want the first", p.Generation)
	}
}

func (f *narrowFixture) setStart(mutate func(*store.ProxyStart)) {
	f.rb.pmu.Lock()
	defer f.rb.pmu.Unlock()
	mutate(f.rb.starts[f.run.ID][1])
}

func (f *narrowFixture) editRow(t *testing.T) map[string]any {
	t.Helper()
	ev := f.audit.eventsFor(f.run.ID, "run.egress.edit")
	if len(ev) != 1 {
		t.Fatalf("run.egress.edit rows = %d, want one", len(ev))
	}
	return leaseAuditData(t, ev[0])
}

func (f *narrowFixture) edit(t *testing.T, body string, code int, reason string) *httptest.ResponseRecorder {
	t.Helper()
	w := f.editEgress(t, body)
	if w.Code != code || (reason != "" && errorReason(w) != reason) {
		t.Fatalf("edit %s: %d %s; want %d %s", body, w.Code, w.Body.String(), code, reason)
	}
	return w
}

func (f *narrowFixture) request() *http.Request {
	return httptest.NewRequest(http.MethodPatch, "/api/v1/runs/"+f.run.ID.String()+"/egress", nil)
}

// An in-place edit that the proxy takes and reports is confirmed: one generation, one start answered
// with the hash of the stored config, no new proxy, no revive row, and the row says it was in place.
func TestNarrowEdit_ConfirmsAndAnswers200(t *testing.T) {
	f := newNarrowFixture(t)
	w := f.edit(t, `{"remove_hosts":["api.anthropic.com"],"deny_hosts":["Evil.example"]}`, http.StatusOK, "")
	var out egressEditResult
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.PolicyGeneration != 2 {
		t.Fatalf("answer %s (%v), want policy_generation 2", w.Body.String(), err)
	}
	want := []string{"api.anthropic.com", "evil.example"}
	if _, n := f.nr.counts(); n != 1 || !slices.Equal(f.nr.sent[0], want) {
		t.Errorf("NarrowProxy sent %v, want one call with %v", f.nr.sent, want)
	}
	if got := f.storedDenies(t); !slices.Equal(got, want) {
		t.Errorf("stored live_denies = %v, want %v", got, want)
	}
	p := f.active(t)
	if p.State != types.ProxyStartEnforced || p.Generation != 2 || p.ExpectedSHA256 != proxy.ConfigSHA256(f.stored(t)) || !p.InPlace {
		t.Errorf("start = %+v, want generation 2 enforced on the hash of the stored config, recorded in place", p)
	}
	if len(f.rr.replaced) != 0 || len(f.audit.eventsFor(f.run.ID, "run.revive")) != 0 {
		t.Errorf("a proxy was replaced (%d) or a revive row written", len(f.rr.replaced))
	}
	if row := f.editRow(t); row["applied"] != "in_place" || row["policy_generation"] != float64(2) {
		t.Errorf("run.egress.edit row = %v", row)
	}
}

// A run on a registered runner is not edited in place: the router answers "not in place" before any
// exec, so the revive path decides, as before.
func TestNarrowEdit_RunnerRunKeepsRevivePath(t *testing.T) {
	f := newNarrowFixture(t)
	run := f.run
	rid := uuid.New()
	run.RunnerID = &rid
	_, rerr, inPlace := f.srv.narrowRunEgress(f.request(), run, liveEgressEdit{deny: []string{"evil.example"}})
	if inPlace != nil || rerr != nil {
		t.Fatalf("narrowRunEgress for a runner's run = %v, %v; want not in place", rerr, inPlace)
	}
	if p, n := f.nr.counts(); p != 0 || n != 0 {
		t.Errorf("probes %d, narrowings %d; want no call", p, n)
	}
}

// A substrate that does not narrow (Docker) stays on the revive path, which replaces the proxy.
func TestNarrowEdit_DockerKeepsRevivePath(t *testing.T) {
	f := newNarrowFixture(t)
	f.nr.notOffered = true
	f.confirmEachReplace(t)
	f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusOK, "")
	if len(f.rr.replaced) != 1 {
		t.Errorf("ReplaceProxy calls = %d, want 1", len(f.rr.replaced))
	}
	if _, n := f.nr.counts(); n != 0 {
		t.Errorf("NarrowProxy ran %d times on Docker", n)
	}
	if p := f.active(t); p.InPlace {
		t.Errorf("the revive's start = %+v, want it not recorded in place", p)
	}
	if row := f.editRow(t); row["applied"] != nil {
		t.Errorf("a Docker edit's row says applied %v", row["applied"])
	}
}

func TestNarrowEdit_OldPodIs409(t *testing.T) {
	f := newNarrowFixture(t)
	f.nr.probeErr = map[int]error{1: runner.ErrNarrowUnsupported}
	f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusConflict, reasonEgressEditUnsupported)
	f.nothingChanged(t)
}

// Each edit that would take something other than a host verdict away from the run is refused, naming
// it, with nothing written: the running proxy can only deny more.
func TestNarrowEdit_OracleRefusesLaneDrops(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		mutate     func(*proxy.Config)
	}{
		{"injection", "inj.example", func(c *proxy.Config) {
			c.Policy.AllowedDomains = append(c.Policy.AllowedDomains, "inj.example")
			c.Injection = []proxy.InjectionConfig{{InjectionRule: egress.InjectionRule{Host: "inj.example", Header: "Authorization", SecretName: "k", Format: "Bearer %s"}, GrantID: uuid.New()}}
		}},
		{"via_org", "org.example", func(c *proxy.Config) {
			c.ViaOrgRoutes = []proxy.ViaOrgRoute{{GrantID: uuid.New(), Host: "org.example"}}
		}},
		{"mcp server", "mcp.example", func(c *proxy.Config) {
			c.MCPServers = []proxy.MCPServerConfig{{Key: "docs", URL: "https://mcp.example/mcp"}}
		}},
		{"intercepted host", "mitm.example", func(c *proxy.Config) { c.MITMHosts = []string{"mitm.example"} }},
		{"aws sigv4", "s3.us-east-1.amazonaws.com", func(c *proxy.Config) {
			c.AWSSigV4 = &proxy.AWSSigV4Config{GrantID: uuid.New(), PortalHost: "portal.sso.us-east-1.amazonaws.com", AccountID: "123456789012",
				RoleName: "dev", Hosts: []string{"s3.us-east-1.amazonaws.com"}}
		}},
		{"github api door", "api.github.com", func(c *proxy.Config) {
			c.GitHubAPI = &proxy.GitHubAPIGrant{Grants: map[string]uuid.UUID{"octo": uuid.New()}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNarrowFixture(t)
			f.setStored(t, tc.mutate)
			w := f.edit(t, `{"deny_hosts":["`+tc.host+`"]}`, http.StatusConflict, reasonEgressEditUnsupported)
			if !strings.Contains(w.Body.String(), "can't take away") {
				t.Errorf("answer %s does not say what it cannot take away", w.Body.String())
			}
			f.nothingChanged(t)
		})
	}
	// A plain deny, an allowlist remove and a wildcard sub-host of an allowlisted host pass.
	f := newNarrowFixture(t)
	f.edit(t, `{"remove_hosts":["api.openai.com"],"deny_hosts":["evil.example","*.sub.example"]}`, http.StatusOK, "")
}

func TestNarrowEdit_RemoveNotListedIs422(t *testing.T) {
	f := newNarrowFixture(t)
	f.edit(t, `{"remove_hosts":["not-listed.example"]}`, http.StatusUnprocessableEntity, reasonEgressEditHostNotListed)
	f.nothingChanged(t)
}

// Each row of the state check: its answer, and no write for a refusal.
func TestNarrowEdit_StateTable(t *testing.T) {
	storedList := func(hosts ...string) func(*narrowFixture, *testing.T) {
		return func(f *narrowFixture, t *testing.T) { f.setStored(t, func(c *proxy.Config) { c.LiveDenies = hosts }) }
	}
	for _, tc := range []struct {
		name    string
		arrange func(*narrowFixture, *testing.T)
		code    int
		reason  string
		behind  bool
	}{
		{"U: no start", func(f *narrowFixture, t *testing.T) {
			f.rb.pmu.Lock()
			f.rb.starts = map[uuid.UUID]map[int]*store.ProxyStart{}
			f.rb.pmu.Unlock()
		}, http.StatusConflict, reasonEgressEditUnsupported, false},
		{"F: failed start", func(f *narrowFixture, t *testing.T) {
			f.setStart(func(p *store.ProxyStart) { p.State = types.ProxyStartFailed })
		}, http.StatusConflict, reasonEgressEditRunNotLive, false},
		{"L: legacy start", func(f *narrowFixture, t *testing.T) {
			f.setStart(func(p *store.ProxyStart) { p.State = types.ProxyStartUnconfirmed })
		}, http.StatusConflict, reasonEgressEditLegacyProxy, false},
		{"W: answered, waiting for its report", func(f *narrowFixture, t *testing.T) {
			f.setStart(func(p *store.ProxyStart) {
				d := p.StartedAt
				p.State, p.Deadline = types.ProxyStartPending, &d
			})
		}, http.StatusConflict, reasonEgressEditInProgress, false},
		{"X: proxy started from another config", func(f *narrowFixture, t *testing.T) {
			cfg, _ := proxy.LoadConfigBytes(f.orig)
			cfg.Policy.AllowedDomains = append(cfg.Policy.AllowedDomains, "other.example")
			f.nr.mu.Lock()
			f.nr.base, _ = json.Marshal(cfg)
			f.nr.mu.Unlock()
		}, http.StatusConflict, reasonEgressEditUnsupported, false},
		{"Y: proxy denies hosts nobody recorded", func(f *narrowFixture, t *testing.T) {
			f.nr.mu.Lock()
			f.nr.list = []string{"rogue.example"}
			f.nr.mu.Unlock()
		}, http.StatusConflict, reasonEgressEditUnsupported, false},
		{"B: enforced, proxy behind the stored list", func(f *narrowFixture, t *testing.T) {
			storedList("a.example")(f, t)
		}, http.StatusOK, "", true},
		{"OK: enforced, lists equal", func(f *narrowFixture, t *testing.T) {
			storedList("a.example")(f, t)
			f.nr.mu.Lock()
			f.nr.list = []string{"a.example"}
			f.nr.mu.Unlock()
		}, http.StatusOK, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNarrowFixture(t)
			tc.arrange(f, t)
			f.edit(t, `{"deny_hosts":["evil.example"]}`, tc.code, tc.reason)
			if tc.code != http.StatusOK {
				f.nothingChanged(t)
				return
			}
			if got := f.editRow(t)["proxy_was_behind"] == true; got != tc.behind {
				t.Errorf("proxy_was_behind = %v, want %v", got, tc.behind)
			}
		})
	}
}

// A start that died after its begin, with the proxy behind the stored list, is superseded by the next
// edit: that edit sends the whole list and confirms.
func TestNarrowEdit_DeadAttemptIsSuperseded(t *testing.T) {
	f := newNarrowFixture(t)
	f.setStored(t, func(c *proxy.Config) { c.LiveDenies = []string{"a.example"} })
	f.setStart(func(p *store.ProxyStart) { p.State, p.Deadline = types.ProxyStartPending, nil })
	f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusOK, "")
	if got := f.rb.get(f.run.ID, 1).State; got != types.ProxyStartSuperseded {
		t.Errorf("the dead start = %s, want superseded", got)
	}
	if f.nr.sent[0][0] != "a.example" || len(f.nr.sent[0]) != 2 {
		t.Errorf("sent %v, want the whole list", f.nr.sent)
	}
}

// A start that was delivered and never answered, whose report landed, with the proxy holding the whole
// stored list: the same-list edit still delivers (it never confirms from the probe) and supersedes it.
func TestNarrowEdit_DeliveredUnansweredIsSuperseded(t *testing.T) {
	f := newNarrowFixture(t)
	f.setStored(t, func(c *proxy.Config) { c.LiveDenies = []string{"a.example"} })
	f.nr.list = []string{"a.example"}
	f.setStart(func(p *store.ProxyStart) {
		p.State, p.Deadline, p.ReportedSHA256 = types.ProxyStartPending, nil, "reported"
	})
	f.edit(t, `{"deny_hosts":["a.example"]}`, http.StatusOK, "")
	if _, n := f.nr.counts(); n != 1 {
		t.Errorf("NarrowProxy ran %d times, want the delivery made all the same", n)
	}
	if f.rb.get(f.run.ID, 1).State != types.ProxyStartSuperseded {
		t.Error("the unanswered start was not superseded")
	}
	if f.editRow(t)["proxy_was_behind"] != nil {
		t.Error("proxy_was_behind set although the proxy held the whole list")
	}
}

// The proxy holds fewer denies than Wardyn stored: the whole list is sent and the row says so.
func TestNarrowEdit_ProxyBehindConverges(t *testing.T) {
	f := newNarrowFixture(t)
	f.setStored(t, func(c *proxy.Config) { c.LiveDenies = []string{"a.example"} })
	f.edit(t, `{"deny_hosts":["b.example"]}`, http.StatusOK, "")
	if want := []string{"a.example", "b.example"}; !slices.Equal(f.nr.sent[0], want) || !slices.Equal(f.nr.list, want) {
		t.Errorf("sent %v, proxy holds %v; want %v", f.nr.sent, f.nr.list, want)
	}
	if f.editRow(t)["proxy_was_behind"] != true {
		t.Error("the row does not say the proxy was behind")
	}
}

// A refusal nothing was delivered for: the start and the generation are abandoned, the stored config
// is back, and the run keeps its proxy.
func TestNarrowEdit_RefusedAbandonsAndKeepsCurrent(t *testing.T) {
	f := newNarrowFixture(t)
	f.nr.narrowErr = fmt.Errorf("%w: bad entry", runner.ErrNarrowRefused)
	f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusBadGateway, reasonReviveProxyKeptCurrent)
	g := f.gens()
	if len(g) != 2 || !g[1].Abandoned {
		t.Errorf("generations = %+v, want the edit's abandoned", g)
	}
	if got := f.rb.get(f.run.ID, 2).State; got != types.ProxyStartAbandoned {
		t.Errorf("the edit's start = %s, want abandoned", got)
	}
	if string(f.stored(t)) != string(f.orig) {
		t.Error("the stored config was not put back")
	}
	if len(f.audit.eventsFor(f.run.ID, "run.policy.unconfirmed")) != 0 {
		t.Error("an abandoned start wrote a readback row")
	}
}

// An answer that may have reached the proxy fails the start with no hash and is never abandoned; the
// wait contains it and the answer says the proxy did not confirm.
func TestNarrowEdit_UncertainFailsAndEndsTheRun(t *testing.T) {
	f := newNarrowFixture(t)
	f.nr.narrowErr = fmt.Errorf("%w: connection lost", runner.ErrNarrowUncertain)
	w := f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusServiceUnavailable, "")
	if r := errorReason(w); r != reasonEgressEditUnconfirmed && r != reasonEgressEditContainmentUnresolved {
		t.Errorf("reason = %s", r)
	}
	if got := f.rb.get(f.run.ID, 2); got.State != types.ProxyStartFailed {
		t.Errorf("the edit's start = %s, want failed", got.State)
	}
	if g := f.gens(); len(g) != 2 || g[1].Abandoned {
		t.Errorf("generations = %+v, want the edit's still current", g)
	}
}

// A hash the proxy was not asked to hold fails the start the same way.
func TestNarrowEdit_WrongHashFails(t *testing.T) {
	f := newNarrowFixture(t)
	f.nr.wrongHash = true
	w := f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusServiceUnavailable, "")
	if r := errorReason(w); r != reasonEgressEditUnconfirmed && r != reasonEgressEditContainmentUnresolved {
		t.Errorf("reason = %s", r)
	}
	if got := f.rb.get(f.run.ID, 2).State; got != types.ProxyStartFailed {
		t.Errorf("the edit's start = %s, want failed", got)
	}
	if g := f.gens(); len(g) != 2 || g[1].Abandoned {
		t.Errorf("generations = %+v, want the edit's still current", g)
	}
}

// Each read of the locked step fails in turn: nothing is written and nothing is sent.
func TestNarrowEdit_UnreadableStartChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*narrowFixture)
		code    int
	}{
		{"generation", func(f *narrowFixture) { f.ns.policyErr = errors.New("db down") }, http.StatusServiceUnavailable},
		{"stored config", func(f *narrowFixture) { f.rs.mu.Lock(); f.rs.dropped = true; f.rs.mu.Unlock() }, http.StatusServiceUnavailable},
		{"active start", func(f *narrowFixture) { f.ns.startErr = errors.New("db down") }, http.StatusServiceUnavailable},
		{"locked probe", func(f *narrowFixture) { f.nr.probeErr = map[int]error{2: errors.New("exec failed")} }, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNarrowFixture(t)
			tc.arrange(f)
			w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
			if w.Code != tc.code {
				t.Fatalf("edit: %d %s, want %d", w.Code, w.Body.String(), tc.code)
			}
			f.ns.policyErr, f.ns.startErr = nil, nil
			f.rs.mu.Lock()
			f.rs.dropped = false
			f.rs.mu.Unlock()
			f.nothingChanged(t)
		})
	}
}

// An advance that finds the generation moved is superseded; one whose outcome is unknown is
// reconciled: the write is found by its marker and abandoned with the stored config put back.
func TestNarrowEdit_AdvanceMovedIsSuperseded(t *testing.T) {
	f := newNarrowFixture(t)
	f.ns.advanceErr = store.ErrRunPolicyMoved
	f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusServiceUnavailable, reasonEgressEditSuperseded)
	f.nothingChanged(t)
}

func TestNarrowEdit_AdvanceUnknownReconciles(t *testing.T) {
	f := newNarrowFixture(t)
	f.ns.commitThenFail = true
	f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusBadGateway, reasonReviveProxyKeptCurrent)
	if g := f.gens(); len(g) != 2 || !g[1].Abandoned {
		t.Errorf("generations = %+v, want the edit's found and abandoned", g)
	}
	if string(f.stored(t)) != string(f.orig) {
		t.Error("the stored config was not put back")
	}
	if _, n := f.nr.counts(); n != 0 {
		t.Errorf("NarrowProxy ran %d times", n)
	}
}

// The same unknown outcome whose undo fails: the generation stays current, narrower than the proxy, so
// the answer is a lost result and the wait ends the run.
func TestNarrowEdit_ReconcileNotAbandonedContains(t *testing.T) {
	f := newNarrowFixture(t)
	f.ns.commitThenFail = true
	f.rb.generationStore.abandonErr = errors.New("db down")
	w := f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusServiceUnavailable, "")
	if r := errorReason(w); r != reasonEgressEditUnconfirmed && r != reasonEgressEditContainmentUnresolved {
		t.Errorf("reason = %s", r)
	}
	if _, n := f.nr.counts(); n != 0 {
		t.Errorf("NarrowProxy ran %d times", n)
	}
}

// A renewal that lands between the two reads of the token: the start is recorded again on the new one.
func TestNarrowEdit_BeginRetriesOnRenewal(t *testing.T) {
	f := newNarrowFixture(t)
	f.ns.afterBegin = func() {
		f.ls.mu.Lock()
		f.ls.jtis = []string{"jti-renewed", "jti-live"}
		f.ls.mu.Unlock()
	}
	f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusOK, "")
	if want := []string{"jti-live", "jti-renewed"}; !slices.Equal(f.ns.begun, want) {
		t.Errorf("starts begun on %v, want %v", f.ns.begun, want)
	}
}

// Holder H1 reads its source, H2 advances and stores more hosts, then H1's advance runs: it is
// superseded and the stored list keeps H2's hosts.
func TestNarrowEdit_LostLockStaleSourceCannotOverwrite(t *testing.T) {
	f := newNarrowFixture(t)
	f.nr.onProbe = func(call int) {
		if call != 2 {
			return
		}
		h2, err := proxy.WithLiveDenies(f.orig, []string{"h2.example"})
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := f.srv.advanceRunPolicyGeneration(context.Background(), f.run, types.RunPolicySpec{}, "", renewedTokens{}, "edit-h2", store.PolicySlot{Generation: 1}, h2); err != nil {
			t.Errorf("H2's advance: %v", err)
		}
	}
	f.edit(t, `{"deny_hosts":["h1.example"]}`, http.StatusServiceUnavailable, reasonEgressEditSuperseded)
	if got := f.storedDenies(t); !slices.Equal(got, []string{"h2.example"}) {
		t.Errorf("stored live_denies = %v, want only H2's", got)
	}
	if _, n := f.nr.counts(); n != 0 {
		t.Errorf("the superseded holder delivered %d times", n)
	}
}

// writeAs runs an in-place edit's write by hand, as a holder that then stalls: it returns the edit, ready to deliver.
func (f *narrowFixture) writeAs(t *testing.T, deny string) *narrowEdit {
	t.Helper()
	ctx := context.Background()
	rd, rerr := f.srv.readNarrowSnapshot(ctx, f.nr, f.run)
	if rerr != nil {
		t.Fatalf("read: %v", rerr)
	}
	edit := liveEgressEdit{deny: []string{deny}}
	denies, rerr := narrowDenies(rd.cfg, edit)
	if rerr != nil {
		t.Fatal(rerr)
	}
	policy, rerr := narrowOracle(ctx, f.run, rd.stored, rd.cfg, edit)
	if rerr != nil {
		t.Fatal(rerr)
	}
	next, err := proxy.WithLiveDenies(rd.stored, denies)
	if err != nil {
		t.Fatal(err)
	}
	ne := &narrowEdit{s: f.srv, n: f.nr, run: f.run, stored: rd.stored, denies: denies, want: proxy.ConfigSHA256(next)}
	if _, rerr := ne.write(ctx, ctx, policy, next, rd); rerr != nil {
		t.Fatalf("write: %v", rerr)
	}
	return ne
}

// Holder A advances and stalls, B edits and confirms, then A's late delivery and answer change neither
// B's enforced start nor the stored list.
func TestNarrowEdit_LostLockHolderCannotWiden(t *testing.T) {
	f := newNarrowFixture(t)
	a := f.writeAs(t, "a.example")
	f.edit(t, `{"deny_hosts":["b.example"]}`, http.StatusOK, "")
	stored := string(f.stored(t))
	_, rerr := a.deliver(context.Background())
	if rerr == nil {
		t.Error("A's late answer was accepted")
	}
	if p := f.active(t); p.State != types.ProxyStartEnforced || p.Generation != 3 {
		t.Errorf("active start = %+v, want B's enforced start (generation 3)", p)
	}
	if string(f.stored(t)) != stored {
		t.Error("A's late delivery changed the stored config")
	}
	if got := f.storedDenies(t); !slices.Equal(got, []string{"a.example", "b.example"}) {
		t.Errorf("stored live_denies = %v", got)
	}
}

// A newer edit stands over a stale holder's generation: its refusal abandons nothing of the newer one
// and the answer is superseded.
func TestNarrowEdit_StaleRefusalAbandonsOnlyItsOwn(t *testing.T) {
	f := newNarrowFixture(t)
	a := f.writeAs(t, "a.example")
	b, err := proxy.WithLiveDenies(f.stored(t), []string{"b.example"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.advanceRunPolicyGeneration(context.Background(), f.run, types.RunPolicySpec{}, "", renewedTokens{}, "edit-b", store.PolicySlot{Generation: 2}, b); err != nil {
		t.Fatal(err)
	}
	f.nr.narrowErr = fmt.Errorf("%w: no", runner.ErrNarrowRefused)
	_, rerr := a.deliver(context.Background())
	if rerr == nil || rerr.reason != reasonEgressEditSuperseded {
		t.Fatalf("A's refusal = %+v, want egress_edit_superseded", rerr)
	}
	if g := f.gens(); g[len(g)-1].Abandoned || string(f.stored(t)) != string(b) {
		t.Errorf("the newer edit was undone: %+v", g)
	}
}

// The undo's four outcomes after a definitive refusal.
func TestNarrowEdit_AbandonNoopContains(t *testing.T) {
	refused := fmt.Errorf("%w: no", runner.ErrNarrowRefused)
	newer := func(f *narrowFixture, abandonIt bool) func() {
		return func() {
			s := f.rb.generationStore
			s.gmu.Lock()
			s.state, _ = s.state.Advance("d3", nil, "edit-newer")
			if abandonIt {
				s.state = s.state.Abandon(3)
			}
			s.gmu.Unlock()
		}
	}
	for _, tc := range []struct {
		name    string
		arrange func(*narrowFixture)
		code    int
		reasons []string
	}{
		{"abandoned", func(f *narrowFixture) {}, http.StatusBadGateway, []string{reasonReviveProxyKeptCurrent}},
		{"no-op, a newer edit stands", func(f *narrowFixture) { f.rb.generationStore.beforeAbandon = newer(f, false) },
			http.StatusServiceUnavailable, []string{reasonEgressEditSuperseded}},
		{"no-op, the newer edit was undone", func(f *narrowFixture) { f.rb.generationStore.beforeAbandon = newer(f, true) },
			http.StatusServiceUnavailable, []string{reasonEgressEditUnconfirmed, reasonEgressEditContainmentUnresolved}},
		{"error", func(f *narrowFixture) { f.rb.generationStore.abandonErr = errors.New("db down") },
			http.StatusServiceUnavailable, []string{reasonEgressEditUnconfirmed, reasonEgressEditContainmentUnresolved}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNarrowFixture(t)
			f.nr.narrowErr = refused
			tc.arrange(f)
			w := f.editEgress(t, `{"deny_hosts":["evil.example"]}`)
			if w.Code != tc.code || !slices.Contains(tc.reasons, errorReason(w)) {
				t.Fatalf("edit: %d %s, want %d %v", w.Code, w.Body.String(), tc.code, tc.reasons)
			}
			for _, ev := range f.audit.eventsFor(f.run.ID, "run.policy.unconfirmed") {
				if leaseAuditData(t, ev)["reason"] == "start_superseded" {
					t.Error("a start_superseded row was written for an abandoned start")
				}
			}
		})
	}
}

// 261 characters is the longest entry; 262 is a 400 after the one routing probe, with no lock, write or delivery.
func TestNarrowEdit_EntryTooLongIs400(t *testing.T) {
	long := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = 'a'
			if i%61 == 60 {
				b[i] = '.'
			}
		}
		return string(b)
	}
	f := newNarrowFixture(t)
	w := f.edit(t, `{"deny_hosts":["`+long(proxy.MaxLiveDenyEntryBytes+1)+`"]}`, http.StatusBadRequest, reasonEgressEditInvalid)
	if !strings.Contains(w.Body.String(), "deny_hosts[0]: an entry is at most 261 characters on Kubernetes") {
		t.Errorf("answer %s", w.Body.String())
	}
	if p, n := f.nr.counts(); p != 1 || n != 0 {
		t.Errorf("probes %d, narrowings %d; want one probe and no delivery", p, n)
	}
	f.nothingChanged(t)
	f2 := newNarrowFixture(t)
	f2.edit(t, `{"deny_hosts":["`+long(proxy.MaxLiveDenyEntryBytes)+`"]}`, http.StatusOK, "")
}

func TestNarrowEdit_ListCapIs409(t *testing.T) {
	f := newNarrowFixture(t)
	full := make([]string, 0, proxy.MaxLiveDenies)
	for i := 0; i < proxy.MaxLiveDenies; i++ {
		full = append(full, fmt.Sprintf("h%04d.example", i))
	}
	slices.Sort(full)
	f.setStored(t, func(c *proxy.Config) { c.LiveDenies = full })
	f.nr.list = slices.Clone(full)
	w := f.edit(t, `{"deny_hosts":["one-more.example"]}`, http.StatusConflict, reasonEgressEditUnsupported)
	if !strings.Contains(w.Body.String(), "holds at most 1,024 live denies") {
		t.Errorf("answer %s", w.Body.String())
	}
	f.nothingChanged(t)
}

// Exit 3 at the routing probe, at the locked probe, and at the delivery is a 502 that changed nothing.
func TestNarrowEdit_NotListeningIs502KeptCurrent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*narrowFixture)
	}{
		{"routing probe", func(f *narrowFixture) { f.nr.probeErr = map[int]error{1: runner.ErrNarrowNotListening} }},
		{"locked probe", func(f *narrowFixture) { f.nr.probeErr = map[int]error{2: runner.ErrNarrowNotListening} }},
		{"delivery", func(f *narrowFixture) { f.nr.narrowErr = runner.ErrNarrowNotListening }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNarrowFixture(t)
			tc.arrange(f)
			w := f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusBadGateway, reasonReviveProxyKeptCurrent)
			if !strings.Contains(w.Body.String(), "Try again in a few seconds") {
				t.Errorf("answer %s lacks the try-again text", w.Body.String())
			}
			if tc.name == "delivery" {
				if g := f.gens(); len(g) != 2 || !g[1].Abandoned || string(f.stored(t)) != string(f.orig) {
					t.Errorf("generations %+v: want the edit abandoned and the config put back", g)
				}
				return
			}
			f.nothingChanged(t)
		})
	}
}

// An in-place edit's unconfirmed answer names the run's proxy, not a new one, carries no image cause,
// and an unresolved stop with no recorded fate says the run will be ended. A Docker edit's wording is
// unchanged.
func TestNarrowEdit_InPlaceWording(t *testing.T) {
	f := newNarrowFixture(t)
	ctx := context.Background()
	unconfirmed := egressEditUnconfirmed("start_failed")
	got := f.srv.withNarrowFate(ctx, f.run, unconfirmed)
	if !strings.Contains(got.msg, "the run's proxy did not confirm the narrowed policy") || strings.Contains(got.msg, "new proxy") || strings.Contains(got.msg, unconfirmedImageCause) {
		t.Errorf("unconfirmed: %q", got.msg)
	}
	unresolved := f.srv.withNarrowFate(ctx, f.run, egressEditUnresolved("start_failed", nil))
	if !strings.HasSuffix(unresolved.msg, "; once that is done, the run will be ended") || !strings.Contains(unresolved.msg, "narrowed policy") {
		t.Errorf("unresolved: %q", unresolved.msg)
	}
	other := reviveRefused(http.StatusBadGateway, reasonReviveProxyKeptCurrent, "kept")
	if f.srv.withNarrowFate(ctx, f.run, other) != other {
		t.Error("a refusal was reworded")
	}
	docker := f.srv.withRunFate(ctx, f.run, egressEditUnconfirmed("start_failed"))
	if !strings.Contains(docker.msg, "the run's new proxy did not confirm the edited policy") || !strings.HasSuffix(docker.msg, unconfirmedImageCause) {
		t.Errorf("a Docker edit's wording changed: %q", docker.msg)
	}
}

// A headless run off the person's own machine is ended, not kept, when its proxy does not confirm: the end
// records the run's failure hint. After an in-place narrowing that hint says the policy was narrowed and
// names no proxy image, which answered the narrowing and so cannot be the cause; after a replace it keeps
// the image cause, since a replaced proxy can be an old image.
func TestNarrowEdit_UnconfirmedRunHintSaysNarrowed(t *testing.T) {
	t.Run("in place", func(t *testing.T) {
		f := newNarrowFixture(t)
		f.st.run.Interactive = false
		f.nr.wrongHash = true
		f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusServiceUnavailable, "")
		got := f.st.FailureHint()
		if got != unconfirmedNarrowedHint || strings.Contains(got, "proxy image") || strings.Contains(got, "rc.18") {
			t.Errorf("failure hint = %q, want %q", got, unconfirmedNarrowedHint)
		}
	})
	t.Run("replace", func(t *testing.T) {
		f := newEgressEditFixture(t)
		f.st.run.Interactive = false
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- f.editEgress(t, `{"deny_hosts":["evil.example"]}`) }()
		f.mismatchAfterAnswer(t)
		if w := <-done; w.Code != http.StatusServiceUnavailable {
			t.Fatalf("edit: %d %s, want 503", w.Code, w.Body.String())
		}
		if got := f.st.FailureHint(); got != unconfirmedPolicyHint {
			t.Errorf("failure hint = %q, want %q", got, unconfirmedPolicyHint)
		}
	})
}

// runStopHint gives the narrowed words when the start's row says it was applied in place or the context
// is an in-place narrowing's own, and the replace path's words, with the image cause, otherwise. The
// subtests above go through a whole edit; this pins the choice itself. Catches: a hint chosen without
// reading the row or the mark, or either read the wrong way round.
func TestRunStopHint_TheRowOrTheMarkChoosesTheWords(t *testing.T) {
	marked := withNarrowedStart(context.Background())
	for _, c := range []struct {
		name    string
		ctx     context.Context
		inPlace bool
		stopped bool
		want    string
	}{
		{"row in place, stopped", context.Background(), true, true, unconfirmedNarrowedHint},
		{"row in place, stop pending", context.Background(), true, false, unconfirmedNarrowedStopPendingHint},
		{"marked, stopped", marked, false, true, unconfirmedNarrowedHint},
		{"marked, stop pending", marked, false, false, unconfirmedNarrowedStopPendingHint},
		{"neither, stopped", context.Background(), false, true, unconfirmedPolicyHint},
		{"neither, stop pending", context.Background(), false, false, unconfirmedStopPendingHint},
	} {
		if got := runStopHint(c.ctx, c.inPlace, c.stopped); got != c.want {
			t.Errorf("%s: hint = %q, want %q", c.name, got, c.want)
		}
	}
}

// A local-mode caller who is not the owner is refused with the revive path's 403 and audit row before
// any exec.
func TestNarrowEdit_OwnerCheckBeforeExec(t *testing.T) {
	f := newNarrowFixture(t)
	req := f.request().WithContext(withLocalPrincipal(context.Background(), "local:someone-else"))
	_, rerr, inPlace := f.srv.narrowRunEgress(req, f.run, liveEgressEdit{deny: []string{"evil.example"}})
	if rerr == nil || rerr.status != http.StatusForbidden || rerr.reason != reasonReviveLocalModeNotOwner || inPlace == nil {
		t.Fatalf("narrowRunEgress = %+v, %v; want the 403", rerr, inPlace)
	}
	if p, n := f.nr.counts(); p != 0 || n != 0 {
		t.Errorf("probes %d, narrowings %d; want no exec", p, n)
	}
	rows := f.audit.eventsFor(f.run.ID, "run.revive")
	if len(rows) != 1 || rows[0].Outcome != "denied" {
		t.Errorf("run.revive rows = %+v, want one denied", rows)
	}
}

// The request's context ends after the delivery: the start is still answered, on a context of its own,
// and the wait confirms.
func TestNarrowEdit_AnswerOnOwnContext(t *testing.T) {
	f := newNarrowFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.nr.afterNarrow = cancel
	res, rerr := f.srv.editRunEgress(f.request().WithContext(ctx), f.run, liveEgressEdit{deny: []string{"evil.example"}})
	if rerr != nil || res.PolicyGeneration != 2 {
		t.Fatalf("edit = %+v, %v; want generation 2 confirmed", res, rerr)
	}
	if p := f.active(t); p.State != types.ProxyStartEnforced || p.Generation != 2 {
		t.Errorf("start = %+v, want generation 2 enforced", p)
	}
}

// The stored config changes between the routing probe and the lock: the hash the start expects is of
// the locked read.
func TestNarrowEdit_ExpectedHashFromLockedRead(t *testing.T) {
	f := newNarrowFixture(t)
	f.nr.onProbe = func(call int) {
		if call != 1 {
			return
		}
		f.setStored(t, func(c *proxy.Config) { c.Policy.AllowedDomains = append(c.Policy.AllowedDomains, "late.example") })
	}
	f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusOK, "")
	want, err := proxy.WithLiveDenies(f.orig, []string{"evil.example"})
	if err != nil {
		t.Fatal(err)
	}
	if p := f.active(t); p.ExpectedSHA256 != proxy.ConfigSHA256(want) {
		t.Errorf("expected hash %s, want the hash of the locked read's config with the edit", p.ExpectedSHA256)
	}
}

// An active start that failed is refused whether or not its proxy was contained.
func TestNarrowEdit_FailedActiveStartIsRefused(t *testing.T) {
	for _, contained := range []bool{false, true} {
		t.Run(fmt.Sprintf("contained=%v", contained), func(t *testing.T) {
			f := newNarrowFixture(t)
			f.setStart(func(p *store.ProxyStart) {
				p.State = types.ProxyStartFailed
				if contained {
					now := p.StartedAt
					p.ContainedAt = &now
				}
			})
			f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusConflict, reasonEgressEditRunNotLive)
			f.nothingChanged(t)
		})
	}
}

// A proxy that did not start from the stored config is refused, and nothing changes.
func TestNarrowEdit_BaseMismatchRefusesUnchanged(t *testing.T) {
	f := newNarrowFixture(t)
	cfg, err := proxy.LoadConfigBytes(f.orig)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Policy.AllowedDomains = append(cfg.Policy.AllowedDomains, "elsewhere.example")
	f.nr.base, _ = json.Marshal(cfg)
	f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusConflict, reasonEgressEditUnsupported)
	f.nothingChanged(t)
}

// The edit's row says applied in place, and proxy_was_behind only when the proxy was; a Docker edit's row says neither.
func TestEgressEdit_AuditRowApplied(t *testing.T) {
	f := newNarrowFixture(t)
	f.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusOK, "")
	row := f.editRow(t)
	if row["applied"] != "in_place" || row["proxy_was_behind"] != nil {
		t.Errorf("in-place row = %v", row)
	}
	b := newNarrowFixture(t)
	b.setStored(t, func(c *proxy.Config) { c.LiveDenies = []string{"a.example"} })
	b.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusOK, "")
	if row := b.editRow(t); row["applied"] != "in_place" || row["proxy_was_behind"] != true {
		t.Errorf("behind row = %v", row)
	}
	d := newNarrowFixture(t)
	d.nr.notOffered = true
	d.confirmEachReplace(t)
	d.edit(t, `{"deny_hosts":["evil.example"]}`, http.StatusOK, "")
	if row := d.editRow(t); row["applied"] != nil || row["proxy_was_behind"] != nil {
		t.Errorf("Docker row = %v", row)
	}
}
