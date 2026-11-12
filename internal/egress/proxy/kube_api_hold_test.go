// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// kubeHoldScope is the requested_scope a held kubectl write raises, as the
// control plane decodes it: these keys and no other.
type kubeHoldScope struct {
	Cluster     string          `json:"cluster"`
	Namespace   string          `json:"namespace"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	Query       string          `json:"query,omitempty"`
	MediaType   string          `json:"media_type,omitempty"`
	Body        json.RawMessage `json:"body,omitempty"`
	CallSHA256  string          `json:"call_sha256"`
	HoldSeconds int             `json:"hold_seconds,omitempty"`
}

type kubeHoldRow struct {
	id    uuid.UUID
	scope kubeHoldScope
	state types.ApprovalState
}

// kubeHoldCP is a control plane for held kubectl writes: it decodes every
// raised scope strictly, folds a raise into the PENDING row with the same
// call_sha256, and answers polls from its rows.
type kubeHoldCP struct {
	srv *httptest.Server

	mu         sync.Mutex
	rows       []*kubeHoldRow
	rejoins    int
	newState   types.ApprovalState // the state a new row starts in; PENDING when empty
	refuse     int                 // when set, every raise is answered with this status
	pollStatus int                 // when set, every poll is answered with this status
	badScopes  int
}

func newKubeHoldCP(t *testing.T, h *kubeHarness) *kubeHoldCP {
	t.Helper()
	c := &kubeHoldCP{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/internal/approvals" {
			var body struct {
				Kind           types.ApprovalKind `json:"kind"`
				RequestedScope json.RawMessage    `json:"requested_scope"`
				RejoinID       *uuid.UUID         `json:"rejoin_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var scope kubeHoldScope
			dec := json.NewDecoder(bytes.NewReader(body.RequestedScope))
			dec.DisallowUnknownFields()
			if body.Kind != types.ApprovalToolCall || dec.Decode(&scope) != nil || scope.CallSHA256 == "" {
				c.badScopes++
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			if c.refuse != 0 {
				http.Error(w, "refused", c.refuse)
				return
			}
			if body.RejoinID != nil {
				// A rejoin stamps its PENDING row and creates nothing, as the real route does.
				c.rejoins++
				for _, row := range c.rows {
					if row.id == *body.RejoinID && row.state == types.ApprovalPending {
						_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: row.id, State: row.state})
						return
					}
				}
				http.Error(w, "no pending approval to rejoin", http.StatusNotFound)
				return
			}
			for _, row := range c.rows {
				if row.state == types.ApprovalPending && row.scope.CallSHA256 == scope.CallSHA256 {
					_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: row.id, State: row.state})
					return
				}
			}
			row := &kubeHoldRow{id: uuid.New(), scope: scope, state: cmpState(c.newState)}
			c.rows = append(c.rows, row)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: row.id, State: types.ApprovalPending})
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/internal/approvals/") {
			if c.pollStatus != 0 {
				http.Error(w, "unreadable", c.pollStatus)
				return
			}
			for _, row := range c.rows {
				if r.URL.Path == "/api/v1/internal/approvals/"+row.id.String() {
					_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: row.id, State: row.state})
					return
				}
			}
		}
		http.Error(w, "unexpected", http.StatusTeapot)
	}))
	t.Cleanup(c.srv.Close)
	h.p.approval = newApprovalClient(c.srv.URL, newTokenSource("tok"), h.p.runID, c.srv.Client())
	prevPoll, prevSecond := holdPollInterval, kubeCallSecond
	holdPollInterval, kubeCallSecond = 2*time.Millisecond, time.Millisecond
	t.Cleanup(func() { holdPollInterval, kubeCallSecond = prevPoll, prevSecond })
	return c
}

func cmpState(s types.ApprovalState) types.ApprovalState {
	if s == "" {
		return types.ApprovalPending
	}
	return s
}

func (c *kubeHoldCP) decideAll(s types.ApprovalState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, row := range c.rows {
		if row.state == types.ApprovalPending {
			row.state = s
		}
	}
}

func (c *kubeHoldCP) snapshot() []kubeHoldRow {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]kubeHoldRow, len(c.rows))
	for i, row := range c.rows {
		out[i] = *row
	}
	return out
}

func holdHarness(t *testing.T, o kubeOpts) (*kubeHarness, *kubeHoldCP) {
	t.Helper()
	o.approveEach, o.trustCA = true, true
	h := newKubeHarness(t, o)
	return h, newKubeHoldCP(t, h)
}

// A held write: a merge patch of one deployment's replicas.
const (
	holdTarget = "/apis/apps/v1/namespaces/team-a/deployments/web"
	holdBody   = `{"spec": {"replicas": 3}}`
	holdCanon  = `{"spec":{"replicas":3}}`
	holdCT     = "Content-Type: application/merge-patch+json"
)

// wantWaiting asserts the 403 Status a held write answers while nobody has said yes.
func wantWaiting(t *testing.T, label string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"wardyn":"kube_call_waiting"`) {
		t.Fatalf("%s: status %d body %s, want 403 kube_call_waiting", label, rec.Code, rec.Body)
	}
}

// The write waits for a person, then reaches the API server once, with the
// canonical body the person was shown and the minted token.
func TestKubeApproveEachHoldsThenForwardsOnce(t *testing.T) {
	h, cp := holdHarness(t, kubeOpts{write: true})
	// A wider second than the helper's, so the approval is raised well inside the window on a slow runner.
	kubeCallSecond = 50 * time.Millisecond
	wantWaiting(t, "before a yes", h.tunnelBody(t, http.MethodPatch, holdTarget, holdBody, holdCT))
	if seen, n := h.upstreamSaw(), h.minted.Load(); len(seen) != 0 || n != 0 {
		t.Fatalf("a held write reached the API server %q or minted %d tokens", seen, n)
	}
	rows := cp.snapshot()
	if len(rows) != 1 {
		t.Fatalf("raised %d approvals, want 1", len(rows))
	}
	s := rows[0].scope
	if s.Cluster != kubeHost || s.Namespace != "team-a" || s.Method != http.MethodPatch || s.Path != holdTarget ||
		s.MediaType != "application/merge-patch+json" || string(s.Body) != holdCanon || s.HoldSeconds != kubeCallWindow {
		t.Fatalf("scope = %+v", s)
	}
	cp.decideAll(types.ApprovalApproved)
	rec := h.tunnelBody(t, http.MethodPatch, holdTarget, holdBody, holdCT)
	if rec.Code != http.StatusOK {
		t.Fatalf("after a yes: status %d body %s", rec.Code, rec.Body)
	}
	if seen := h.upstreamSaw(); len(seen) != 1 || seen[0] != "PATCH "+holdTarget+" "+kubeToken {
		t.Fatalf("the API server saw %q", seen)
	}
	h.mu.Lock()
	body := h.bodies[0]
	h.mu.Unlock()
	if body != holdCanon {
		t.Fatalf("the API server received %q, want the approved %q", body, holdCanon)
	}
	if len(cp.snapshot()) != 1 {
		t.Fatalf("the retry raised a second approval")
	}
	if log := h.log(); !strings.Contains(log, rows[0].id.String()) || !strings.Contains(log, ruleSourceKubeAPI) {
		t.Errorf("the forwarded row does not name its approval: %s", log)
	}
}

// One yes is spent once: the same write again asks again and is not sent.
func TestKubeApproveEachSecondSpendRefused(t *testing.T) {
	h, cp := holdHarness(t, kubeOpts{write: true})
	cp.newState = types.ApprovalApproved
	if rec := h.tunnelBody(t, http.MethodPatch, holdTarget, holdBody, holdCT); rec.Code != http.StatusOK {
		t.Fatalf("first: status %d body %s", rec.Code, rec.Body)
	}
	cp.mu.Lock()
	cp.newState = ""
	cp.mu.Unlock()
	wantWaiting(t, "the same write again", h.tunnelBody(t, http.MethodPatch, holdTarget, holdBody, holdCT))
	if seen := h.upstreamSaw(); len(seen) != 1 {
		t.Fatalf("one approval sent %d writes: %q", len(seen), seen)
	}
	if rows := cp.snapshot(); len(rows) != 2 || rows[1].state != types.ApprovalPending {
		t.Fatalf("rows = %+v, want a second, pending approval", rows)
	}
}

// replayRefused: request a waits and is approved; b, which differs from a in
// one part of its key, is not sent on that approval; then a is.
func replayRefused(t *testing.T, o kubeOpts, a, b holdReq) {
	t.Helper()
	h, cp := holdHarness(t, o)
	wantWaiting(t, "a", a(t, h))
	cp.decideAll(types.ApprovalApproved)
	wantWaiting(t, "b on a's approval", b(t, h))
	if seen := h.upstreamSaw(); len(seen) != 0 {
		t.Fatalf("b was sent on a's approval: %q", seen)
	}
	if rows := cp.snapshot(); len(rows) != 2 || rows[0].scope.CallSHA256 == rows[1].scope.CallSHA256 {
		t.Fatalf("b did not ask on its own key: %+v", rows)
	}
	if rec := a(t, h); rec.Code != http.StatusOK {
		t.Fatalf("a after its yes: status %d body %s", rec.Code, rec.Body)
	}
	if seen := h.upstreamSaw(); len(seen) != 1 {
		t.Fatalf("the API server saw %q, want a alone", seen)
	}
}

// holdReq sends one request through h.
type holdReq func(t *testing.T, h *kubeHarness) *httptest.ResponseRecorder

func patchWith(target, body string, hdr ...string) holdReq {
	return func(t *testing.T, h *kubeHarness) *httptest.ResponseRecorder {
		return h.tunnelBody(t, http.MethodPatch, target, body, hdr...)
	}
}

func TestKubeApproveEachOtherQueryNotReused(t *testing.T) {
	replayRefused(t, kubeOpts{write: true},
		patchWith(holdTarget+"?fieldManager=kubectl-edit", holdBody, holdCT),
		patchWith(holdTarget+"?fieldManager=other", holdBody, holdCT))
}

func TestKubeApproveEachOtherMediaTypeNotReused(t *testing.T) {
	replayRefused(t, kubeOpts{write: true},
		patchWith(holdTarget, holdBody, holdCT),
		patchWith(holdTarget, holdBody, "Content-Type: application/strategic-merge-patch+json"))
}

func TestKubeApproveEachOtherBodyNotReused(t *testing.T) {
	replayRefused(t, kubeOpts{write: true},
		patchWith(holdTarget, holdBody, holdCT),
		patchWith(holdTarget, `{"spec":{"replicas":30}}`, holdCT))
}

func TestKubeApproveEachNotReusedForOtherObject(t *testing.T) {
	replayRefused(t, kubeOpts{write: true},
		patchWith(holdTarget, holdBody, holdCT),
		patchWith("/apis/apps/v1/namespaces/team-b/deployments/web", holdBody, holdCT))
}

// A dry run a person approved is not spent by the real write.
func TestKubeApproveEachDryRunNotSpentByRealWrite(t *testing.T) {
	replayRefused(t, kubeOpts{write: true},
		patchWith(holdTarget+"?dryRun=All", holdBody, holdCT),
		patchWith(holdTarget, holdBody, holdCT))
}

// A collection delete approved for one selector is not spent by another.
func TestKubeApproveEachSelectorChangeNotReused(t *testing.T) {
	del := func(sel string) holdReq {
		return func(t *testing.T, h *kubeHarness) *httptest.ResponseRecorder {
			return h.tunnelBody(t, http.MethodDelete, "/api/v1/namespaces/team-a/pods?labelSelector="+sel, "")
		}
	}
	replayRefused(t, kubeOpts{write: true}, del("app%3Dweb"), del("app%3Dapi"))
}

// A declined write is refused, and so is the same write again, without asking.
func TestKubeApproveEachDeniedRefused(t *testing.T) {
	h, cp := holdHarness(t, kubeOpts{write: true})
	cp.newState = types.ApprovalDenied
	for i := range 2 {
		rec := h.tunnelBody(t, http.MethodPatch, holdTarget, holdBody, holdCT)
		h.mustRefuse(t, "declined", rec, kubeCallDeclined)
		if n := len(cp.snapshot()); n != 1 {
			t.Fatalf("try %d: %d approvals raised, want 1", i, n)
		}
	}
}

// A write whose window ends is refused with the approval to wait for; once a
// person says yes, the same write retried spends it, rejoining the open request.
func TestKubeApproveEachTimeoutRefusesRetrySpends(t *testing.T) {
	h, cp := holdHarness(t, kubeOpts{write: true})
	rec := h.tunnelBody(t, http.MethodPatch, holdTarget, holdBody, holdCT)
	wantWaiting(t, "the window", rec)
	rows := cp.snapshot()
	if len(rows) != 1 || !strings.Contains(rec.Body.String(), rows[0].id.String()) {
		t.Fatalf("the refusal does not name the approval: %s", rec.Body)
	}
	wantWaiting(t, "a retry still waiting", h.tunnelBody(t, http.MethodPatch, holdTarget, holdBody, holdCT))
	cp.decideAll(types.ApprovalApproved)
	if rec := h.tunnelBody(t, http.MethodPatch, holdTarget, holdBody, holdCT); rec.Code != http.StatusOK {
		t.Fatalf("after a yes: status %d body %s", rec.Code, rec.Body)
	}
	cp.mu.Lock()
	rejoins := cp.rejoins
	cp.mu.Unlock()
	if n := len(cp.snapshot()); n != 1 || rejoins == 0 {
		t.Fatalf("rows %d rejoins %d, want one row rejoined", n, rejoins)
	}
}

// A restart is keyed without its timestamp: kubectl run again spends the yes
// given to the first run, and the API server gets the second run's patch.
func TestKubeApproveEachRestartRetrySpendsSameApproval(t *testing.T) {
	h, cp := holdHarness(t, kubeOpts{restart: true})
	wantWaiting(t, "first run", h.tunnelBody(t, http.MethodPatch, restartTarget, restartBody, restartCTHeader))
	cp.decideAll(types.ApprovalApproved)
	second := strings.Replace(restartBody, "18:30:00Z", "18:31:00Z", 1)
	if rec := h.tunnelBody(t, http.MethodPatch, restartTarget, second, restartCTHeader); rec.Code != http.StatusOK {
		t.Fatalf("second run: status %d body %s", rec.Code, rec.Body)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.bodies) != 1 || h.bodies[0] != second {
		t.Fatalf("the API server received %q", h.bodies)
	}
	if n := len(cp.rows); n != 1 {
		t.Fatalf("%d approvals, want 1", n)
	}
}

// Reads are never held.
func TestKubeApproveEachGETNotHeld(t *testing.T) {
	h, cp := holdHarness(t, kubeOpts{write: true})
	if rec := h.tunnel(t, http.MethodGet, "/api/v1/namespaces/team-a/pods"); rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if n := len(cp.snapshot()); n != 0 {
		t.Fatalf("a read raised %d approvals", n)
	}
}

// A request the door refuses asks nobody.
func TestKubeApproveEachForbiddenNeverRaises(t *testing.T) {
	h, cp := holdHarness(t, kubeOpts{write: true})
	h.mustRefuse(t, "a Secret", h.tunnelBody(t, http.MethodPost, "/api/v1/namespaces/team-a/secrets", `{}`, "Content-Type: application/json"), kubeAPIRefused)
	h.mustRefuse(t, "another namespace", h.tunnelBody(t, http.MethodPatch, "/apis/apps/v1/namespaces/prod/deployments/web", holdBody, holdCT), kubeAPIRefused)
	h2, cp2 := holdHarness(t, kubeOpts{restart: true})
	h2.mustRefuse(t, "a restart row's other write", h2.tunnelBody(t, http.MethodPatch, holdTarget, holdBody, holdCT), kubeAPIRefused)
	if n := len(cp.snapshot()) + len(cp2.snapshot()); n != 0 {
		t.Fatalf("refused requests raised %d approvals", n)
	}
}

// A body a person cannot be shown exactly is refused before anyone is asked.
func TestKubeApproveEachNonJSONBodyRefused(t *testing.T) {
	for name, c := range map[string]struct {
		body string
		hdr  []string
	}{
		"protobuf":            {"\x0a\x02k8s", []string{"Content-Type: application/vnd.kubernetes.protobuf"}},
		"apply patch as YAML": {"spec:\n  replicas: 3\n", []string{"Content-Type: application/apply-patch+yaml"}},
		"not JSON":            {"spec=3", []string{holdCT}},
		"no media type":       {holdBody, nil},
		"a repeated key":      {`{"spec":{"replicas":1},"spec":{"replicas":9}}`, []string{holdCT}},
		"gzip":                {holdBody, []string{holdCT, "Content-Encoding: gzip"}},
		"two media types":     {holdBody, []string{holdCT, holdCT}},
	} {
		t.Run(name, func(t *testing.T) {
			h, cp := holdHarness(t, kubeOpts{write: true})
			h.mustRefuse(t, name, h.tunnelBody(t, http.MethodPatch, holdTarget, c.body, c.hdr...), kubeAPIRefused)
			if n := len(cp.snapshot()); n != 0 {
				t.Fatalf("raised %d approvals", n)
			}
		})
	}
}

// A body over 32 KiB is refused, never truncated, chunked or not.
func TestKubeApproveEachOversizeBodyRefused(t *testing.T) {
	big := `{"data":{"k":"` + strings.Repeat("x", types.MaxMCPCallBytes) + `"}}`
	for name, hdr := range map[string][]string{
		"with a length": {"Content-Type: application/json"},
		"chunked":       {"Content-Type: application/json", "Transfer-Encoding: chunked"},
	} {
		t.Run(name, func(t *testing.T) {
			h, cp := holdHarness(t, kubeOpts{write: true})
			rec := h.tunnelBody(t, http.MethodPut, "/api/v1/namespaces/team-a/configmaps/c", big, hdr...)
			h.mustRefuse(t, name, rec, kubeAPIRefused)
			if !strings.Contains(rec.Body.String(), "32 KiB") {
				t.Errorf("the refusal does not say why: %s", rec.Body)
			}
			if n := len(cp.snapshot()); n != 0 {
				t.Fatalf("raised %d approvals", n)
			}
		})
	}
}

// Fail closed: an answer the proxy cannot read, a raise the control plane
// will not take, and a proxy with no way to ask each leave the write unsent.
func TestKubeApproveEachHoldStoreUnreadableFailsClosed(t *testing.T) {
	t.Run("unreadable answer", func(t *testing.T) {
		h, cp := holdHarness(t, kubeOpts{write: true})
		cp.newState, cp.pollStatus = types.ApprovalApproved, http.StatusInternalServerError
		rec := h.tunnelBody(t, http.MethodPatch, holdTarget, holdBody, holdCT)
		wantWaiting(t, "approved but unreadable", rec)
		h.mustRefuse(t, "approved but unreadable", rec, kubeCallWaiting)
	})
	t.Run("raise failed", func(t *testing.T) {
		h, cp := holdHarness(t, kubeOpts{write: true})
		cp.refuse = http.StatusServiceUnavailable
		h.mustRefuse(t, "raise failed", h.tunnelBody(t, http.MethodPatch, holdTarget, holdBody, holdCT), kubeAPIRefused)
	})
	t.Run("raise refused", func(t *testing.T) {
		h, cp := holdHarness(t, kubeOpts{write: true})
		cp.refuse = http.StatusTooManyRequests
		h.mustRefuse(t, "raise refused", h.tunnelBody(t, http.MethodPatch, holdTarget, holdBody, holdCT), kubeAPIRefused)
	})
	t.Run("no way to ask", func(t *testing.T) {
		h, _ := holdHarness(t, kubeOpts{write: true})
		h.p.approval = nil
		h.mustRefuse(t, "no approval client", h.tunnelBody(t, http.MethodPatch, holdTarget, holdBody, holdCT), kubeAPIRefused)
	})
}

// A row without approve_each sends its writes as before.
func TestKubeApproveEachOffUnchanged(t *testing.T) {
	h := newKubeHarness(t, kubeOpts{write: true, trustCA: true})
	if rec := h.tunnelBody(t, http.MethodPatch, holdTarget, holdBody, holdCT); rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
}

// The proxy config's decode is strict: approve_each is a key this proxy knows,
// a misspelling is refused, and it holds writes only on a row that has some.
func TestKubeAPIGrantNewKeysStrictDecode(t *testing.T) {
	decode := func(grant string) (*Config, error) {
		var c Config
		dec := json.NewDecoder(strings.NewReader(`{"kube_api":` + grant + `}`))
		dec.DisallowUnknownFields()
		return &c, dec.Decode(&c)
	}
	c, err := decode(`{"host":"k8s.example.com","port":6443,"namespaces":["team-a"],"write":true,"approve_each":true}`)
	if err != nil {
		t.Fatalf("approve_each refused: %v", err)
	}
	if out, _ := json.Marshal(c.KubeAPI); !strings.Contains(string(out), `"approve_each":true`) {
		t.Fatalf("approve_each did not round-trip: %s", out)
	}
	if _, err := decode(`{"host":"k8s.example.com","port":6443,"namespaces":["team-a"],"approve_eachh":true}`); err == nil {
		t.Fatal("a misspelt key was accepted")
	}
	const tokenName = "wardyn-cli-1b4e28ba-2fa1-11d2-883f-0016d3cca427-kubectl"
	c, err = decode(`{"host":"k8s.example.com","port":6443,"namespaces":["team-a"],"approve_each":true}`)
	if err != nil {
		t.Fatal(err)
	}
	c.MITMCACertPEM, c.MITMCAKeyPEM = "cert", "key"
	c.Injection = []InjectionConfig{{InjectionRule: egress.InjectionRule{Host: "k8s.example.com", Header: "Authorization", SecretName: tokenName, RequireTLS: true}, GrantID: uuid.New()}}
	if err := c.validateKubeAPI(); err == nil {
		t.Fatal("approve_each on a read-only grant was accepted")
	}
}
