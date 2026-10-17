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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const govCovChangesPath = "/api/v1/governance/changes"

var (
	govCovChangeID = uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	govCovGrantID  = uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")
)

// govCovGrantDelete is a held delete of one grant whose base state is "no such grant", so applying it
// against a Querier that finds no row passes the staleness check.
func govCovGrantDelete(s *Server, proposedBy, email string) types.GovernanceChange {
	payload, _ := json.Marshal(grantChangePayload{ID: govCovGrantID, SubjectType: types.CapabilitySubjectUser, Subject: "u@corp.example", Capability: "model", Value: "m"})
	return types.GovernanceChange{
		ID: govCovChangeID, TargetKind: govKindGrant, Op: "delete", TargetKey: "k", State: types.GovernanceChangePending,
		ProposedBy: proposedBy, ProposedByEmail: email, Payload: payload,
		BaseHash: computeETag(grantState(nil)), DeploymentHash: computeETag(s.cfg.DefaultPolicy),
	}
}

func TestGovCovListChangesFiltersByKindAndValidatesState(t *testing.T) {
	profile := types.GovernanceChange{ID: uuid.MustParse("aaaaaaaa-0000-0000-0000-0000000000a1"), TargetKind: govKindProfile}
	mapping := types.GovernanceChange{ID: uuid.MustParse("aaaaaaaa-0000-0000-0000-0000000000a2"), TargetKind: govKindRoleMapping}
	newer := types.GovernanceChange{ID: uuid.MustParse("aaaaaaaa-0000-0000-0000-0000000000a3"), TargetKind: "from_a_newer_binary"}
	st := &govCovStore{changes: []types.GovernanceChange{profile, mapping, newer}}
	s, _ := govCovServer(t, st)

	ids := func(w *httptest.ResponseRecorder) []string {
		t.Helper()
		var out []types.GovernanceChange
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("body %s: %v", w.Body.String(), err)
		}
		var got []string
		for _, c := range out {
			got = append(got, c.TargetKind)
		}
		return got
	}

	w := doSSO(t, s, http.MethodGet, govCovChangesPath, govCovSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin), "")
	if w.Code != http.StatusOK || st.listState != types.GovernanceChangePending {
		t.Fatalf("super admin list = %d, asked the store for state %q, want 200 and pending by default", w.Code, st.listState)
	}
	if got := ids(w); !reflect.DeepEqual(got, []string{govKindProfile, govKindRoleMapping}) {
		t.Errorf("super admin sees %v, want the profile and role-mapping changes but not the unknown kind", got)
	}

	w = doSSO(t, s, http.MethodGet, govCovChangesPath, govCovSession(t, "sub-alice", "alice@corp.example", oidc.RoleSecurityAdmin), "")
	if got := ids(w); !reflect.DeepEqual(got, []string{govKindProfile}) {
		t.Errorf("security admin sees %v, want only the profile change (a role mapping is a super admin's)", got)
	}

	st.listState = ""
	w = do(t, s, http.MethodGet, govCovChangesPath+"?state=applied", adminToken, "")
	if w.Code != http.StatusOK || st.listState != types.GovernanceChangeApplied {
		t.Errorf("?state=applied = %d, store asked for %q", w.Code, st.listState)
	}

	st.listState = ""
	w = do(t, s, http.MethodGet, govCovChangesPath+"?state=bogus", adminToken, "")
	if w.Code != http.StatusBadRequest || errorReason(w) != reasonGovernanceChangeStateInvalid {
		t.Errorf("?state=bogus = %d %q, want 400 %q", w.Code, errorReason(w), reasonGovernanceChangeStateInvalid)
	}
	if st.listState != "" {
		t.Errorf("an invalid state reached the store as %q", st.listState)
	}
	for _, state := range governanceChangeStates {
		if !strings.Contains(w.Body.String(), state) {
			t.Errorf("the refusal does not list the %q state: %s", state, w.Body.String())
		}
	}
}

func TestGovCovListChangesStoreFailure(t *testing.T) {
	s, _ := govCovServer(t, &govCovStore{listErr: errors.New("pg: down")})
	w := do(t, s, http.MethodGet, govCovChangesPath, adminToken, "")
	if w.Code != http.StatusInternalServerError || errorReason(w) != reasonInternalError || strings.Contains(w.Body.String(), "pg: down") {
		t.Errorf("list on a failing store = %d %s, want a 500 that does not leak the driver error", w.Code, w.Body.String())
	}
}

func TestGovCovGetChange(t *testing.T) {
	visible := types.GovernanceChange{ID: govCovChangeID, TargetKind: govKindProfile, Op: "create", TargetKey: "k", Diff: json.RawMessage(`{"changed":["name"]}`)}
	hidden := types.GovernanceChange{ID: govCovChangeID, TargetKind: govKindRoleMapping}
	alice := govCovSession(t, "sub-alice", "alice@corp.example", oidc.RoleSecurityAdmin)
	path := govCovChangesPath + "/" + govCovChangeID.String()

	for _, c := range []struct {
		name       string
		st         *govCovStore
		path       string
		wantCode   int
		wantReason string
	}{
		{"a visible change is returned with its diff", &govCovStore{got: visible}, path, http.StatusOK, ""},
		{"a change of a kind the caller may not approve is a 404", &govCovStore{got: hidden}, path, http.StatusNotFound, reasonGovernanceChangeNotFound},
		{"an unknown id is a 404", &govCovStore{getErr: store.ErrNotFound}, path, http.StatusNotFound, reasonGovernanceChangeNotFound},
		{"a store failure is a 500", &govCovStore{getErr: errors.New("pg: down")}, path, http.StatusInternalServerError, reasonInternalError},
		{"a malformed id is a 400", &govCovStore{}, govCovChangesPath + "/not-a-uuid", http.StatusBadRequest, reasonInvalidIDParam},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := govCovServer(t, c.st)
			w := doSSO(t, s, http.MethodGet, c.path, alice, "")
			if w.Code != c.wantCode || errorReason(w) != c.wantReason {
				t.Fatalf("GET = %d %q (%s), want %d %q", w.Code, errorReason(w), w.Body.String(), c.wantCode, c.wantReason)
			}
			if c.wantCode == http.StatusOK {
				m := govCovDecode(t, w)
				if m["target_kind"] != govKindProfile || m["op"] != "create" || m["diff"] == nil {
					t.Errorf("body = %v", m)
				}
			}
		})
	}
}

func TestGovCovSameGovernanceHuman(t *testing.T) {
	for _, c := range []struct {
		name                     string
		proposedBy, proposedMail string
		principal, email         string
		want                     bool
	}{
		{"the same principal", "sub-a", "", "sub-a", "", true},
		{"the same mailbox in another case", "sub-a", "Alice@Corp.Example", "sub-b", "alice@corp.example", true},
		{"two different people", "sub-a", "alice@corp.example", "sub-b", "bob@corp.example", false},
		{"no principal and no mailbox on either side is not the same human", "", "", "", "", false},
		{"an empty approver mailbox never matches an empty proposer mailbox", "sub-a", "", "sub-b", "", false},
		{"an empty approver principal does not match an empty proposer principal", "", "alice@corp.example", "", "bob@corp.example", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ch := types.GovernanceChange{ProposedBy: c.proposedBy, ProposedByEmail: c.proposedMail}
			if got := sameGovernanceHuman(ch, c.principal, c.email); got != c.want {
				t.Errorf("sameGovernanceHuman = %v, want %v", got, c.want)
			}
		})
	}
}

// govCovDecideThrough stands in for the decision transaction: it runs the handler's callback against
// q with ch, and answers out or the callback's error.
func govCovDecideThrough(q *govCovQuerier, ch, out types.GovernanceChange) func(store.GovernanceDecideFunc) (types.GovernanceChange, error) {
	return func(fn store.GovernanceDecideFunc) (types.GovernanceChange, error) {
		if err := fn(q, ch); err != nil {
			return types.GovernanceChange{}, err
		}
		return out, nil
	}
}

func TestGovCovApproveAppliesAndAuditsAsTheApprover(t *testing.T) {
	for _, c := range []struct {
		name       string
		bypass     bool
		wantActor  types.ActorType
		wantPerson string
	}{
		{"a second human approves", false, types.ActorHuman, "sub-bob"},
		{"the admin token approves by break-glass", true, types.ActorSystem, adminTokenPrincipal},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &govCovStore{}
			s, h := govCovServer(t, st)
			ch := govCovGrantDelete(s, "sub-alice", "alice@corp.example")
			q := &govCovQuerier{execFn: func(string) (pgconn.CommandTag, error) { return pgconn.NewCommandTag("DELETE 1"), nil }}
			out := ch
			out.State, out.DecidedBy = types.GovernanceChangeApplied, c.wantPerson
			st.got = ch
			st.decideFn = govCovDecideThrough(q, ch, out)

			var w *httptest.ResponseRecorder
			if c.bypass {
				w = do(t, s, http.MethodPost, govCovChangesPath+"/"+ch.ID.String()+"/approve", adminToken, "")
			} else {
				w = doSSO(t, s, http.MethodPost, govCovChangesPath+"/"+ch.ID.String()+"/approve",
					govCovSession(t, "sub-bob", "bob@corp.example", oidc.RoleSecurityAdmin), "")
			}
			if w.Code != http.StatusOK {
				t.Fatalf("approve = %d %s", w.Code, w.Body.String())
			}
			if m := govCovDecode(t, w); m["state"] != types.GovernanceChangeApplied || m["decided_by"] != c.wantPerson {
				t.Errorf("answer = %v", m)
			}
			if len(st.decisions) != 1 || st.decisions[0].To != types.GovernanceChangeApplied || st.decisions[0].By != c.wantPerson {
				t.Errorf("decision asked of the store = %+v", st.decisions)
			}
			if w := q.writes(); len(w) != 1 || !strings.Contains(w[0], "DELETE FROM capability_grants") {
				t.Errorf("the approval wrote %v, want exactly the held grant delete", w)
			}

			want := []string{"capability.grant.delete", "governance.change.approve"}
			if c.bypass {
				want = append(want, "governance.change.bypass")
			}
			if got := govCovAuditActions(h); !reflect.DeepEqual(got, want) {
				t.Fatalf("audit rows = %v, want %v", got, want)
			}
			target := govCovAudits(h, "capability.grant.delete")[0]
			if target.Target != govCovGrantID.String() || target.ActorType != c.wantActor || target.Actor != c.wantPerson {
				t.Errorf("target row = %q by %s/%s", target.Target, target.ActorType, target.Actor)
			}
			if d := govCovAuditData(t, target); d["change_id"] != ch.ID.String() || d["proposed_by"] != "sub-alice" {
				t.Errorf("target row does not tie back to the proposal: %v", d)
			}
			appr := govCovAudits(h, "governance.change.approve")[0]
			if appr.Target != ch.ID.String() || appr.Outcome != "success" {
				t.Errorf("approve row = %q %q", appr.Target, appr.Outcome)
			}
			if c.bypass {
				if b := govCovAuditData(t, govCovAudits(h, "governance.change.bypass")[0]); b["change_id"] != ch.ID.String() || b["target_kind"] != govKindGrant {
					t.Errorf("bypass row = %v", b)
				}
			}
		})
	}
}

func TestGovCovApproveRefusals(t *testing.T) {
	alice := govCovSession(t, "sub-alice", "alice@corp.example", oidc.RoleSecurityAdmin)
	for _, c := range []struct {
		name       string
		cookie     *http.Cookie
		proposedBy string
		email      string
		kind       string
		wantCode   int
		wantReason string
	}{
		{"the proposer cannot approve their own change", alice, "sub-alice", "", govKindGrant, http.StatusForbidden, string(authz.ReasonSecondHumanRequired)},
		{"nor can the same mailbox under another principal", alice, "sub-other", "ALICE@corp.example", govKindGrant, http.StatusForbidden, string(authz.ReasonSecondHumanRequired)},
		{"a security admin cannot approve a role mapping", alice, "sub-other", "x@corp.example", govKindRoleMapping, http.StatusForbidden, string(authz.ReasonAdminSurface)},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &govCovStore{}
			s, h := govCovServer(t, st)
			ch := govCovGrantDelete(s, c.proposedBy, c.email)
			ch.TargetKind = c.kind
			q := &govCovQuerier{}
			st.got = ch
			st.decideFn = govCovDecideThrough(q, ch, ch)
			w := doSSO(t, s, http.MethodPost, govCovChangesPath+"/"+ch.ID.String()+"/approve", c.cookie, "")
			if w.Code != c.wantCode || errorReason(w) != c.wantReason {
				t.Fatalf("approve = %d %q (%s), want %d %q", w.Code, errorReason(w), w.Body.String(), c.wantCode, c.wantReason)
			}
			if len(q.writes()) != 0 {
				t.Errorf("a refused approval wrote %v", q.writes())
			}
			for _, a := range govCovAuditActions(h) {
				if strings.HasPrefix(a, "governance.change.approve") || a == "capability.grant.delete" {
					t.Errorf("a refused approval audited %s", a)
				}
			}
		})
	}
}

func TestGovCovApproveSameHumanMayReject(t *testing.T) {
	st := &govCovStore{}
	s, h := govCovServer(t, st)
	ch := govCovGrantDelete(s, "sub-alice", "alice@corp.example")
	q := &govCovQuerier{}
	out := ch
	out.State = types.GovernanceChangeRejected
	st.got = ch
	st.decideFn = govCovDecideThrough(q, ch, out)
	alice := govCovSession(t, "sub-alice", "alice@corp.example", oidc.RoleSecurityAdmin)

	w := doSSO(t, s, http.MethodPost, govCovChangesPath+"/"+ch.ID.String()+"/reject", alice, `{"reason":"  changed my mind  "}`)
	if w.Code != http.StatusOK {
		t.Fatalf("reject own change = %d %s", w.Code, w.Body.String())
	}
	if len(st.decisions) != 1 || st.decisions[0].To != types.GovernanceChangeRejected || st.decisions[0].Reason != "changed my mind" ||
		st.decisions[0].By != "sub-alice" || st.decisions[0].ByEmail != "alice@corp.example" {
		t.Errorf("decision = %+v, want a rejection by alice with the trimmed reason", st.decisions)
	}
	if len(q.seen) != 0 {
		t.Errorf("a rejection touched the target: %v", q.seen)
	}
	if got := govCovAuditActions(h); !reflect.DeepEqual(got, []string{"governance.change.reject"}) {
		t.Fatalf("audit rows = %v, want only the rejection", got)
	}
	if d := govCovAuditData(t, govCovAudits(h, "governance.change.reject")[0]); d["reason"] != "changed my mind" || d["proposed_by"] != "sub-alice" {
		t.Errorf("reject row data = %v", d)
	}

	// Without a reason the row carries none.
	h2 := newHarness(t)
	s2 := New(func() Config { c := baseTestConfig(h2, st); c.OIDC = &oidc.Authenticator{}; return c }())
	w = doSSO(t, s2, http.MethodPost, govCovChangesPath+"/"+ch.ID.String()+"/reject", alice, "")
	if w.Code != http.StatusOK {
		t.Fatalf("reject with no body = %d %s", w.Code, w.Body.String())
	}
	if d := govCovAuditData(t, govCovAudits(h2, "governance.change.reject")[0]); d["reason"] != nil {
		t.Errorf("a reject with no reason recorded one: %v", d)
	}
}

func TestGovCovRejectReasonRefusals(t *testing.T) {
	for _, c := range []struct {
		name       string
		body       string
		wantCode   int
		wantReason string
	}{
		{"not json", `{"reason":`, http.StatusBadRequest, reasonInvalidRequestBody},
		{"an unknown field", `{"reason":"x","extra":1}`, http.StatusBadRequest, reasonInvalidRequestBody},
		{"over 512 characters", `{"reason":"` + strings.Repeat("é", 513) + `"}`, http.StatusBadRequest, reasonInvalidRequestBody},
		{"a control character", `{"reason":"a\u0007b"}`, http.StatusBadRequest, reasonInvalidRequestBody},
		{"a body over 16 KiB", `{"reason":"` + strings.Repeat("a", 17<<10) + `"}`, http.StatusRequestEntityTooLarge, reasonRequestBodyTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &govCovStore{got: types.GovernanceChange{ID: govCovChangeID, TargetKind: govKindGrant}}
			s, h := govCovServer(t, st)
			st.decideFn = func(store.GovernanceDecideFunc) (types.GovernanceChange, error) {
				t.Error("a refused reason reached the decision transaction")
				return types.GovernanceChange{}, nil
			}
			w := do(t, s, http.MethodPost, govCovChangesPath+"/"+govCovChangeID.String()+"/reject", adminToken, c.body)
			if w.Code != c.wantCode || errorReason(w) != c.wantReason {
				t.Fatalf("reject = %d %q (%s), want %d %q", w.Code, errorReason(w), w.Body.String(), c.wantCode, c.wantReason)
			}
			if len(st.decisions) != 0 || len(govCovAuditActions(h)) != 0 {
				t.Errorf("decisions %v / audit %v after a refused reason", st.decisions, govCovAuditActions(h))
			}
		})
	}

	// Exactly 512 characters is accepted, counted in characters rather than bytes.
	st := &govCovStore{got: types.GovernanceChange{ID: govCovChangeID, TargetKind: govKindGrant}}
	s, _ := govCovServer(t, st)
	st.decideFn = func(fn store.GovernanceDecideFunc) (types.GovernanceChange, error) {
		return st.got, fn(&govCovQuerier{}, st.got)
	}
	w := do(t, s, http.MethodPost, govCovChangesPath+"/"+govCovChangeID.String()+"/reject", adminToken, `{"reason":"`+strings.Repeat("é", 512)+`"}`)
	if w.Code != http.StatusOK {
		t.Errorf("a 512-character reason = %d %s, want it accepted", w.Code, w.Body.String())
	}
}

func TestGovCovDecideReadFailures(t *testing.T) {
	for _, c := range []struct {
		name       string
		st         *govCovStore
		path       string
		wantCode   int
		wantReason string
	}{
		{"a malformed id", &govCovStore{}, govCovChangesPath + "/nope/approve", http.StatusBadRequest, reasonInvalidIDParam},
		{"an unknown change", &govCovStore{getErr: store.ErrNotFound}, govCovChangesPath + "/" + govCovChangeID.String() + "/approve", http.StatusNotFound, reasonGovernanceChangeNotFound},
		{"a failing read", &govCovStore{getErr: errors.New("pg: down")}, govCovChangesPath + "/" + govCovChangeID.String() + "/approve", http.StatusInternalServerError, reasonInternalError},
		{"a target kind this binary does not know", &govCovStore{got: types.GovernanceChange{TargetKind: "from_a_newer_binary"}},
			govCovChangesPath + "/" + govCovChangeID.String() + "/approve", http.StatusInternalServerError, reasonInternalError},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := govCovServer(t, c.st)
			c.st.decideFn = func(store.GovernanceDecideFunc) (types.GovernanceChange, error) {
				t.Error("decision transaction opened after a failed read")
				return types.GovernanceChange{}, nil
			}
			w := do(t, s, http.MethodPost, c.path, adminToken, "")
			if w.Code != c.wantCode || errorReason(w) != c.wantReason {
				t.Errorf("approve = %d %q, want %d %q", w.Code, errorReason(w), c.wantCode, c.wantReason)
			}
			if len(c.st.decisions) != 0 {
				t.Errorf("a decision was recorded: %+v", c.st.decisions)
			}
		})
	}
}

func TestGovCovDecideLocalModeIsRefused(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	st := &govCovStore{got: types.GovernanceChange{ID: govCovChangeID, TargetKind: govKindGrant}}
	s, h := govCovServer(t, st, func(c *Config) { c.LocalMode = true })
	st.decideFn = func(store.GovernanceDecideFunc) (types.GovernanceChange, error) {
		t.Error("local mode reached the decision transaction")
		return types.GovernanceChange{}, nil
	}
	w := do(t, s, http.MethodPost, govCovChangesPath+"/"+govCovChangeID.String()+"/approve", adminToken, "")
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonGovernanceSecondHumanLocalMode {
		t.Errorf("approve in local mode = %d %q, want 503 %q", w.Code, errorReason(w), reasonGovernanceSecondHumanLocalMode)
	}
	if len(govCovAuditActions(h)) != 0 {
		t.Errorf("audited %v", govCovAuditActions(h))
	}
}

func TestGovCovAnswerFailedDecision(t *testing.T) {
	notPendingApplied := &store.ErrGovernanceChangeNotPending{State: types.GovernanceChangeApplied}
	lapsed := &store.ErrGovernanceChangeNotPending{State: types.GovernanceChangePending, Lapsed: true}
	for _, c := range []struct {
		name        string
		approve     bool
		bypass      bool
		err         error
		wantCode    int
		wantReason  string
		wantBody    string
		wantFailure string
		wantExpire  bool
	}{
		{"already decided", true, false, notPendingApplied, http.StatusConflict, reasonGovernanceChangeNotPending, "(applied)", "not_pending", false},
		{"lapsed moves the change to expired and says so", true, false, lapsed, http.StatusConflict, reasonGovernanceChangeNotPending, "no longer pending", "not_pending", true},
		{"stale target", true, false, store.ErrGovernanceChangeStale, http.StatusConflict, reasonGovernanceChangeStale, "propose it again", "stale", false},
		{"a refusal the apply decided", true, false, writeRefusal(http.StatusBadRequest, reasonCapabilityGrantInvalid, "invalid grant: %v", "x"), http.StatusBadRequest, reasonCapabilityGrantInvalid, "invalid grant: x", "error", false},
		{"the change vanished", true, false, store.ErrNotFound, http.StatusNotFound, reasonGovernanceChangeNotFound, "not found", "error", false},
		{"an unexpected error", true, false, errors.New("pg: boom"), http.StatusInternalServerError, reasonInternalError, "", "error", false},
		{"a failed bypass is recorded beside the failed approval", true, true, store.ErrGovernanceChangeStale, http.StatusConflict, reasonGovernanceChangeStale, "", "stale", false},
		{"a rejection that fails records no approval failure", false, false, store.ErrGovernanceChangeStale, http.StatusConflict, reasonGovernanceChangeStale, "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &govCovStore{got: types.GovernanceChange{ID: govCovChangeID, TargetKind: govKindGrant, TargetKey: "tk", ProposedBy: "sub-alice"}}
			s, h := govCovServer(t, st)
			st.decideFn = func(store.GovernanceDecideFunc) (types.GovernanceChange, error) {
				return types.GovernanceChange{}, c.err
			}
			verb := "approve"
			if !c.approve {
				verb = "reject"
			}
			var w *httptest.ResponseRecorder
			if c.bypass {
				w = do(t, s, http.MethodPost, govCovChangesPath+"/"+govCovChangeID.String()+"/"+verb, adminToken, "")
			} else {
				w = doSSO(t, s, http.MethodPost, govCovChangesPath+"/"+govCovChangeID.String()+"/"+verb,
					govCovSession(t, "sub-bob", "bob@corp.example", oidc.RoleSecurityAdmin), "")
			}
			if w.Code != c.wantCode || errorReason(w) != c.wantReason || !strings.Contains(w.Body.String(), c.wantBody) {
				t.Fatalf("%s = %d %q (%s), want %d %q containing %q", verb, w.Code, errorReason(w), w.Body.String(), c.wantCode, c.wantReason, c.wantBody)
			}
			if strings.Contains(w.Body.String(), "pg: boom") {
				t.Errorf("the 500 leaked the driver error: %s", w.Body.String())
			}
			fails := govCovAudits(h, "governance.change.approve")
			if c.wantFailure == "" {
				if len(fails) != 0 {
					t.Errorf("recorded approval rows %d for a rejection", len(fails))
				}
			} else {
				if len(fails) != 1 || fails[0].Outcome != "failure" || fails[0].Target != govCovChangeID.String() {
					t.Fatalf("approve rows = %+v, want one failure on the change", fails)
				}
				d := govCovAuditData(t, fails[0])
				if d["error"] != c.wantFailure || d["target_kind"] != govKindGrant || d["target_key"] != "tk" || d["proposed_by"] != "sub-alice" {
					t.Errorf("failure data = %v, want error class %q", d, c.wantFailure)
				}
			}
			if got := len(govCovAudits(h, "governance.change.expire")) == 1; got != c.wantExpire {
				t.Errorf("expire row present = %v, want %v", got, c.wantExpire)
			}
			bypass := govCovAudits(h, "governance.change.bypass")
			if c.bypass && c.approve {
				if len(bypass) != 1 || bypass[0].Outcome != "failure" || govCovAuditData(t, bypass[0])["error"] != c.wantFailure {
					t.Errorf("bypass rows = %+v, want one failure naming %q", bypass, c.wantFailure)
				}
			} else if len(bypass) != 0 {
				t.Errorf("unexpected bypass rows %+v", bypass)
			}
		})
	}
}

func TestGovCovRecheckApprover(t *testing.T) {
	tokenID := uuid.MustParse("cccccccc-0000-0000-0000-000000000003")
	s, h := govCovServer(t, &govCovStore{})
	kind := governanceChangeKinds[govKindGrant]
	r := govCovHumanReq(http.MethodPost, "/x", "sub-alice", "alice@corp.example", oidc.RoleSecurityAdmin)

	t.Run("a session or the admin token carries nothing to re-read", func(t *testing.T) {
		q := &govCovQuerier{}
		if err := s.recheckApprover(r.Context(), r, q, kind); err != nil || len(q.seen) != 0 {
			t.Errorf("recheck = %v after %v, want nil and no statement", err, q.seen)
		}
	})

	tokenReq := r.WithContext(withAPITokenID(r.Context(), tokenID))
	t.Run("a revoked or expired token is the auth lane's 401", func(t *testing.T) {
		q := &govCovQuerier{}
		err := s.recheckApprover(tokenReq.Context(), tokenReq, q, kind)
		var refusal *govRefusal
		if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "approver token revoked or expired") {
			t.Fatalf("recheck = %v, want the revoked-token refusal", err)
		}
		w := httptest.NewRecorder()
		refusal.write(w, tokenReq)
		if w.Code != http.StatusUnauthorized || errorReason(w) != reasonInvalidAdminToken {
			t.Errorf("refusal = %d %q, want 401 %q", w.Code, errorReason(w), reasonInvalidAdminToken)
		}
		if len(q.seen) != 1 || !strings.Contains(q.seen[0], "api_tokens") {
			t.Errorf("statements = %v, want the one token read", q.seen)
		}
		if len(govCovAudits(h, "auth.fail")) == 0 {
			t.Errorf("a revoked approver token was not audited as an auth failure: %v", govCovAuditActions(h))
		}
	})

	t.Run("a failing token read is an error, never a pass", func(t *testing.T) {
		boom := errors.New("pg: down")
		q := &govCovQuerier{rowErr: func(string) error { return boom }}
		err := s.recheckApprover(tokenReq.Context(), tokenReq, q, kind)
		var refusal *govRefusal
		if err == nil || errors.As(err, &refusal) || !errors.Is(err, boom) {
			t.Errorf("recheck = %v, want the store error itself", err)
		}
	})
}

func TestGovCovDenyApproverAndRefusalError(t *testing.T) {
	s, _ := govCovServer(t, &govCovStore{})
	ref := s.denyApprover(govKind{approver: operatorApprover})
	if ref.Error() != "governance change refused: approver tier" {
		t.Errorf("Error() = %q", ref.Error())
	}
	w := httptest.NewRecorder()
	ref.write(w, govCovHumanReq(http.MethodPost, "/x", "sub-a", "a@corp.example", oidc.RoleSecurityAdmin))
	if w.Code != http.StatusForbidden || errorReason(w) != string(authz.ReasonAdminSurface) {
		t.Errorf("operator-tier denial = %d %q, want 403 admin_surface", w.Code, errorReason(w))
	}
	w = httptest.NewRecorder()
	s.denyApprover(governanceChangeKinds[govKindGrant]).write(w, govCovHumanReq(http.MethodPost, "/x", "sub-a", "a@corp.example", oidc.RoleUser))
	if w.Code != http.StatusForbidden || errorReason(w) != string(authz.ReasonSecurityAdminSurface) {
		t.Errorf("security-tier denial = %d %q, want 403 security_admin_surface", w.Code, errorReason(w))
	}
}
