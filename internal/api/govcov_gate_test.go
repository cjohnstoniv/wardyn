// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestGovCovChangedPaths(t *testing.T) {
	type pair struct {
		A int      `json:"a"`
		B []string `json:"b"`
	}
	for _, c := range []struct {
		name          string
		before, after any
		want          []string
	}{
		{"nothing differs", map[string]any{"x": 1}, map[string]any{"x": 1}, nil},
		{"a create reports every leaf of the new side", nil, map[string]any{"name": "n", "limits": map[string]any{"max": 2}},
			[]string{"limits.max", "name"}},
		{"a delete reports every leaf of the old side", map[string]any{"name": "n", "inner": map[string]any{"k": true}}, nil,
			[]string{"inner.k", "name"}},
		{"a nested change reports its dotted path, sorted", map[string]any{"z": map[string]any{"k": 1}, "a": 1},
			map[string]any{"z": map[string]any{"k": 2}, "a": 2}, []string{"a", "z.k"}},
		{"a changed list is one leaf at its own path", pair{A: 1, B: []string{"x"}}, pair{A: 1, B: []string{"x", "y"}}, []string{"b"}},
		{"an added key is a path", map[string]any{"a": 1}, map[string]any{"a": 1, "b": 2}, []string{"b"}},
		{"scalars that differ are the root", "old", "new", []string{"(root)"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := changedPaths(c.before, c.after); !reflect.DeepEqual(got, c.want) {
				t.Errorf("changedPaths = %v, want %v", got, c.want)
			}
		})
	}
}

func TestGovCovRenderGovernanceDiff(t *testing.T) {
	var d governanceDiffBody
	raw := renderGovernanceDiff(nil, map[string]any{"name": "n"}, nil)
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if d.Before != nil {
		t.Errorf("a create carries before = %s, want it omitted", d.Before)
	}
	if string(d.After) != `{"name":"n"}` {
		t.Errorf("after = %s, want the new view", d.After)
	}
	if d.Changed == nil || len(d.Changed) != 0 || !strings.Contains(string(raw), `"changed":[]`) {
		t.Errorf("a diff with no changed paths must serialize changed as [], got %s", raw)
	}

	raw = renderGovernanceDiff(map[string]any{"name": "o"}, nil, []string{"name"})
	d = governanceDiffBody{}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if string(d.Before) != `{"name":"o"}` || d.After != nil || !reflect.DeepEqual(d.Changed, []string{"name"}) {
		t.Errorf("a delete diff = %s, want before only with changed [name]", raw)
	}
}

func TestGovCovGovernanceChangeTTL(t *testing.T) {
	for _, c := range []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{"unset waits the default", 0, defaultGovernanceChangeTTL},
		{"negative waits the default", -time.Hour, defaultGovernanceChangeTTL},
		{"a configured ttl wins", 90 * time.Minute, 90 * time.Minute},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{cfg: Config{GovernanceChangeTTL: c.set}}
			if got := s.governanceChangeTTL(); got != c.want {
				t.Errorf("governanceChangeTTL = %v, want %v", got, c.want)
			}
		})
	}
}

func TestGovCovWriteMode(t *testing.T) {
	human := func() *http.Request {
		return govCovHumanReq(http.MethodPost, "/x", "sub-alice", "alice@corp.example", oidc.RoleSecurityAdmin)
	}
	adminTok := func() *http.Request { return httptest.NewRequest(http.MethodPost, "/x", nil) }
	for _, c := range []struct {
		name       string
		switchOn   bool
		local      bool
		req        func() *http.Request
		wantMode   govWriteMode
		wantOK     bool
		wantCode   int
		wantReason string
	}{
		{"switch off is a direct write for a human", false, false, human, govDirect, true, 0, ""},
		{"switch off is a direct write even in local mode", false, true, human, govDirect, true, 0, ""},
		{"switch on and a human is held", true, false, human, govQueue, true, 0, ""},
		{"switch on and the admin token breaks glass", true, false, adminTok, govBypass, true, 0, ""},
		{"the admin token stays the way past local mode", true, true, adminTok, govBypass, true, 0, ""},
		{"switch on in local mode refuses a human", true, true, human, govDirect, false, http.StatusServiceUnavailable, reasonGovernanceSecondHumanLocalMode},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(envGovernanceSecondHuman, map[bool]string{true: "true", false: "false"}[c.switchOn])
			s := &Server{cfg: Config{LocalMode: c.local}}
			w := httptest.NewRecorder()
			mode, ok := s.governanceWriteMode(w, c.req())
			if mode != c.wantMode || ok != c.wantOK {
				t.Fatalf("governanceWriteMode = (%v, %v), want (%v, %v)", mode, ok, c.wantMode, c.wantOK)
			}
			if c.wantOK {
				if w.Body.Len() != 0 {
					t.Errorf("a decided mode wrote a body: %s", w.Body.String())
				}
				return
			}
			if w.Code != c.wantCode || errorReason(w) != c.wantReason {
				t.Errorf("refusal = %d %q, want %d %q", w.Code, errorReason(w), c.wantCode, c.wantReason)
			}
			if !strings.Contains(w.Body.String(), envGovernanceSecondHuman) {
				t.Errorf("the refusal does not name the switch: %s", w.Body.String())
			}
		})
	}
}

func TestGovCovIsAdminTokenCaller(t *testing.T) {
	if !isAdminTokenCaller(httptest.NewRequest(http.MethodGet, "/x", nil)) {
		t.Error("a request with no human is the shared admin token")
	}
	if isAdminTokenCaller(govCovHumanReq(http.MethodGet, "/x", "sub-a", "a@corp.example", oidc.RoleAdmin)) {
		t.Error("an SSO super admin is a human, not the admin token")
	}
}

func TestGovCovRecordGovernanceBypass(t *testing.T) {
	s, h := govCovServer(t, &govCovStore{})
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	s.recordGovernanceBypass(r, govKindGrant, "target-1", "failure", map[string]any{"change_id": "c-1", "reason": "overridden"})
	rows := govCovAudits(h, "governance.change.bypass")
	if len(rows) != 1 {
		t.Fatalf("%d bypass rows, want 1", len(rows))
	}
	ev := rows[0]
	if ev.Target != "target-1" || ev.Outcome != "failure" || ev.ActorType != types.ActorSystem || ev.Actor != adminTokenPrincipal {
		t.Errorf("row = target %q outcome %q actor %q/%q", ev.Target, ev.Outcome, ev.ActorType, ev.Actor)
	}
	data := govCovAuditData(t, ev)
	if data["switch"] != envGovernanceSecondHuman || data["target_kind"] != govKindGrant || data["change_id"] != "c-1" {
		t.Errorf("data = %v", data)
	}
	if data["reason"] != "overridden" {
		t.Errorf("the caller's extra reason must override the default, got %v", data["reason"])
	}

	h2 := newHarness(t)
	s2 := New(baseTestConfig(h2, &govCovStore{}))
	s2.recordGovernanceBypass(r, govKindProfile, "t", "success", nil)
	d := govCovAuditData(t, govCovAudits(h2, "governance.change.bypass")[0])
	if d["reason"] != "admin_token_break_glass" {
		t.Errorf("default reason = %v, want admin_token_break_glass", d["reason"])
	}
}

func TestGovCovCanSeeGovernanceKind(t *testing.T) {
	s := &Server{}
	for _, c := range []struct {
		name string
		req  *http.Request
		kind string
		want bool
	}{
		{"security admin sees a profile change", govCovHumanReq("GET", "/x", "s", "s@x", oidc.RoleSecurityAdmin), govKindProfile, true},
		{"security admin does not see a role mapping", govCovHumanReq("GET", "/x", "s", "s@x", oidc.RoleSecurityAdmin), govKindRoleMapping, false},
		{"super admin sees a role mapping", govCovHumanReq("GET", "/x", "s", "s@x", oidc.RoleAdmin), govKindRoleMapping, true},
		{"a member sees nothing", govCovHumanReq("GET", "/x", "s", "s@x", oidc.RoleUser), govKindGrant, false},
		{"the admin token sees every kind", httptest.NewRequest("GET", "/x", nil), govKindRoleMapping, true},
		{"a kind this binary does not know is shown to nobody", httptest.NewRequest("GET", "/x", nil), "from_a_newer_binary", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := s.canSeeGovernanceKind(c.req, c.kind); got != c.want {
				t.Errorf("canSeeGovernanceKind(%q) = %v, want %v", c.kind, got, c.want)
			}
		})
	}
}

func TestGovCovProposeGovernanceChange(t *testing.T) {
	expires := time.Now().UTC().Truncate(time.Second)
	saved := types.GovernanceChange{
		ID: uuid.MustParse("11111111-1111-1111-1111-111111111111"), State: types.GovernanceChangePending,
		ExpiresAt: expires,
	}
	expiredID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	st := &govCovStore{proposeSaved: saved, proposeExpired: []uuid.UUID{expiredID}}
	s, h := govCovServer(t, st, func(c *Config) { c.GovernanceChangeTTL = 5 * time.Hour })
	r := govCovHumanReq(http.MethodPost, "/x", "sub-alice", "Alice@Corp.Example", oidc.RoleSecurityAdmin)
	w := httptest.NewRecorder()

	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindGrant, op: "upsert", key: "k1", payload: map[string]any{"p": 1},
		before: map[string]any{"v": 1}, after: map[string]any{"v": 2}, changed: []string{"v"}, baseState: map[string]any{"base": 1},
	})

	if w.Code != http.StatusAccepted || w.Header().Get("Location") != "/api/v1/governance/changes/"+saved.ID.String() {
		t.Fatalf("answer = %d Location %q", w.Code, w.Header().Get("Location"))
	}
	var body struct {
		Pending types.GovernanceChange `json:"pending_change"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Pending.ID != saved.ID {
		t.Fatalf("body %s: %v", w.Body.String(), err)
	}
	if len(st.proposed) != 1 {
		t.Fatalf("%d proposals stored, want 1", len(st.proposed))
	}
	ch := st.proposed[0]
	if ch.TargetKind != govKindGrant || ch.Op != "upsert" || ch.TargetKey != "k1" || string(ch.Payload) != `{"p":1}` {
		t.Errorf("stored change = %+v", ch)
	}
	if ch.ProposedBy != "sub-alice" || ch.ProposedByEmail != "Alice@Corp.Example" {
		t.Errorf("proposer = %q / %q, want the caller's principal and mailbox", ch.ProposedBy, ch.ProposedByEmail)
	}
	if ch.BaseHash != computeETag(map[string]any{"base": 1}) || ch.DeploymentHash != computeETag(s.cfg.DefaultPolicy) {
		t.Errorf("hashes = %q / %q, want the base state's and the deployment default's", ch.BaseHash, ch.DeploymentHash)
	}
	if !reflect.DeepEqual(ch.Diff, renderGovernanceDiff(map[string]any{"v": 1}, map[string]any{"v": 2}, []string{"v"})) {
		t.Errorf("diff = %s", ch.Diff)
	}
	if st.proposedTTL != 5*time.Hour {
		t.Errorf("ttl = %v, want the configured 5h", st.proposedTTL)
	}
	if got, want := govCovAuditActions(h), []string{"governance.change.expire", "governance.change.propose"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("audit rows = %v, want %v (the lapsed change's expiry first)", got, want)
	}
	exp := govCovAudits(h, "governance.change.expire")[0]
	if exp.Target != expiredID.String() || govCovAuditData(t, exp)["target_key"] != "k1" {
		t.Errorf("expire row = target %q data %s", exp.Target, exp.Data)
	}
	prop := govCovAuditData(t, govCovAudits(h, "governance.change.propose")[0])
	if prop["op"] != "upsert" || prop["target_kind"] != govKindGrant || prop["expires_at"] != expires.Format(time.RFC3339) {
		t.Errorf("propose row data = %v", prop)
	}
}

// TestGovCovProposeRefusalCarriesTheHeldChange: the 409 a second proposal gets names the change that
// holds the target in pending_change, so a repeat apply can report it as pending. A held change that
// cannot be read, or is no longer pending, leaves the plain refusal.
func TestGovCovProposeRefusalCarriesTheHeldChange(t *testing.T) {
	pendingID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	for _, c := range []struct {
		name     string
		got      types.GovernanceChange
		getErr   error
		wantHeld bool
	}{
		{"the held change is read", types.GovernanceChange{ID: pendingID, State: types.GovernanceChangePending}, nil, true},
		{"the read fails", types.GovernanceChange{}, errors.New("pg: gone"), false},
		{"it was decided meanwhile", types.GovernanceChange{ID: pendingID, State: types.GovernanceChangeApplied}, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &govCovStore{proposeErr: &store.ErrGovernanceChangePending{ID: pendingID}, got: c.got, getErr: c.getErr}
			s, _ := govCovServer(t, st)
			w := httptest.NewRecorder()
			s.proposeGovernanceChange(w, govCovHumanReq(http.MethodPost, "/x", "sub-a", "a@corp.example", oidc.RoleSecurityAdmin),
				govProposal{kind: govKindProfile, op: "update", key: "k", payload: map[string]any{}})
			if w.Code != http.StatusConflict || errorReason(w) != reasonGovernanceChangePending {
				t.Fatalf("answer = %d %q, want 409 %q", w.Code, errorReason(w), reasonGovernanceChangePending)
			}
			var body struct {
				Pending *types.GovernanceChange `json:"pending_change"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %s: %v", w.Body.String(), err)
			}
			if c.wantHeld != (body.Pending != nil && body.Pending.ID == pendingID) {
				t.Errorf("pending_change = %+v, want held=%v", body.Pending, c.wantHeld)
			}
		})
	}
}

func TestGovCovProposeGovernanceChangeFailures(t *testing.T) {
	pendingID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	for _, c := range []struct {
		name       string
		proposeErr error
		payload    any
		wantCode   int
		wantReason string
		wantInBody string
	}{
		{"a live change holds the target", &store.ErrGovernanceChangePending{ID: pendingID}, map[string]any{},
			http.StatusConflict, reasonGovernanceChangePending, pendingID.String()},
		{"a store failure is a 500", errors.New("pg: connection reset"), map[string]any{},
			http.StatusInternalServerError, reasonInternalError, ""},
		{"a payload that cannot be encoded is a 500 and nothing is stored", nil, make(chan int),
			http.StatusInternalServerError, reasonInternalError, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &govCovStore{proposeErr: c.proposeErr}
			s, h := govCovServer(t, st)
			w := httptest.NewRecorder()
			s.proposeGovernanceChange(w, govCovHumanReq(http.MethodPost, "/x", "sub-a", "a@corp.example", oidc.RoleSecurityAdmin),
				govProposal{kind: govKindProfile, op: "create", key: "k", payload: c.payload})
			if w.Code != c.wantCode || errorReason(w) != c.wantReason {
				t.Fatalf("answer = %d %q (%s), want %d %q", w.Code, errorReason(w), w.Body.String(), c.wantCode, c.wantReason)
			}
			if !strings.Contains(w.Body.String(), c.wantInBody) {
				t.Errorf("body %s lacks %q", w.Body.String(), c.wantInBody)
			}
			if strings.Contains(w.Body.String(), "connection reset") {
				t.Errorf("the 500 leaked the driver error: %s", w.Body.String())
			}
			if w.Header().Get("Location") != "" || len(govCovAuditActions(h)) != 0 {
				t.Errorf("a refused proposal set Location %q and audited %v", w.Header().Get("Location"), govCovAuditActions(h))
			}
			if _, isChan := c.payload.(chan int); isChan && len(st.proposed) != 0 {
				t.Errorf("an unencodable payload still reached the store: %+v", st.proposed)
			}
		})
	}
}
