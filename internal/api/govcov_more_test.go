// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A graph the proof of narrowing cannot read is held for approval, never exempted.
func TestGovCovProfileWriteExemptIsRefusedOnAGraphItCannotRead(t *testing.T) {
	s, _ := govCovServer(t, &govCovStore{})
	res := &ResolvedProfile{}
	domains := []string{"pypi.org"}
	other := []string{"github.com"}

	t.Run("a stored profile whose base is missing", func(t *testing.T) {
		cur := govCovComposedOn(govCovPID, "p", govCovBase, "pypi.org")
		next := cur
		next.Overlay = &types.CeilingOverlay{AllowedDomains: &other}
		if s.profileWriteExempt([]types.GovernanceProfile{cur}, next, res) {
			t.Error("a write over a chain with a missing base was exempted")
		}
	})

	t.Run("a stored profile that has a base but no overlay", func(t *testing.T) {
		base := govCovStandalone(govCovBase, "base", "pypi.org")
		cur := govCovStandalone(govCovPID, "p", "pypi.org")
		cur.BaseProfileID = &base.ID
		next := cur
		next.Ceiling.AllowedDomains = domains[:0]
		if s.profileWriteExempt([]types.GovernanceProfile{base, cur}, next, res) {
			t.Error("a write over a chain that does not compose was exempted")
		}
		if got := s.resolveForDiff([]types.GovernanceProfile{base, cur}, govCovPID); got != nil {
			t.Errorf("a chain that does not compose resolved to %+v, want no effective view", got)
		}
	})
}

func TestGovCovCheckGraphWriteOnDamagedGraphs(t *testing.T) {
	s, _ := govCovServer(t, &govCovStore{})
	composeOn := func(id string) string {
		return `{"name":"p","base_profile_id":"` + id + `","overlay":{}}`
	}

	t.Run("a base whose own base is missing is an error, not a refusal", func(t *testing.T) {
		x := govCovComposedOn(govCovMid, "x", govCovTop, "pypi.org") // govCovTop is not in the list
		err := govCovBuildErr(t, s, []types.GovernanceProfile{x}, govCovPID, composeOn(govCovMid.String()))
		var ref *profileWriteError
		if err == nil || errors.As(err, &ref) || !strings.Contains(err.Error(), "names a base that does not exist") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("a base that cannot itself be applied is a 409 naming that", func(t *testing.T) {
		m := govCovStandalone(govCovBase, "m", "pypi.org")
		m.Ceiling.AllowedMethods = []string{"GET"}
		post := []string{"POST"}
		x := types.GovernanceProfile{ID: govCovMid, Name: "x", BaseProfileID: &m.ID, Overlay: &types.CeilingOverlay{AllowedMethods: &post}}
		err := govCovBuildErr(t, s, []types.GovernanceProfile{m, x}, govCovPID, composeOn(govCovMid.String()))
		if ref := govCovAsRefusal(t, err); ref.status != http.StatusConflict || ref.reason != reasonGovernanceOverlayUnsatisfiable ||
			!strings.Contains(ref.msg, "the profile this one builds on cannot be applied") {
			t.Errorf("refusal = %+v", ref)
		}
	})

	t.Run("a descendant that does not compose for another reason is an error", func(t *testing.T) {
		top := govCovStandalone(govCovTop, "top", "pypi.org")
		broken := govCovStandalone(govCovMid, "broken", "pypi.org")
		broken.BaseProfileID = &top.ID // a base with no overlay
		body := govCovProfileBody("top", "pypi.org")
		err := govCovBuildErr(t, s, []types.GovernanceProfile{top, broken}, govCovTop, body)
		var ref *profileWriteError
		if err == nil || errors.As(err, &ref) || !strings.Contains(err.Error(), "has a base but no overlay") {
			t.Errorf("error = %v", err)
		}
	})
}

func TestGovCovApplyAssignmentProfileReadFailure(t *testing.T) {
	boom := errors.New("pg: down")
	s, _ := govCovServer(t, &govCovStore{})
	q := &govCovQuerier{rowErr: func(sql string) error {
		if strings.Contains(sql, "governance_profiles") {
			return boom
		}
		return pgx.ErrNoRows
	}}
	req := govCovAssignChange(t, s, "upsert", governanceAssignmentRequest{
		SubjectType: types.CapabilitySubjectUser, Subject: "u@corp.example", ProfileID: govCovPID,
	})
	r := govCovHumanReq(http.MethodPost, "/x", "sub-bob", "bob@corp.example", oidc.RoleSecurityAdmin)
	if _, err := applyAssignmentChange(s, r, q, req); !errors.Is(err, boom) || len(q.writes()) != 0 {
		t.Errorf("error = %v, writes %v; want the read failure and no write", err, q.writes())
	}
}

func TestGovCovHoldEnforcementReadFailureInsideTheDryRun(t *testing.T) {
	q := &govCovQuerier{queryErr: errors.New("pg: down")}
	st := &govCovStore{dryRunFn: func(fn func(store.Querier) error) error { return fn(q) }}
	s, _ := govCovServer(t, st)
	w := httptest.NewRecorder()
	s.holdEnforcement(w, govCovHumanReq(http.MethodPut, "/x", "sub-a", "a@corp.example", oidc.RoleSecurityAdmin), map[string]bool{capSecret: true})
	if w.Code != http.StatusInternalServerError || errorReason(w) != reasonInternalError || len(st.proposed) != 0 || len(q.seen) != 1 || !strings.Contains(q.seen[0], "capability_enforcement") {
		t.Errorf("= %d, proposals %d, statements %v", w.Code, len(st.proposed), q.seen)
	}
}

func TestGovCovDecideStaleDeploymentDefault(t *testing.T) {
	st := &govCovStore{}
	s, h := govCovServer(t, st)
	ch := govCovGrantDelete(s, "sub-alice", "alice@corp.example")
	ch.DeploymentHash = `"the-deployment-default-as-it-was"`
	q := &govCovQuerier{}
	st.got = ch
	st.decideFn = govCovDecideThrough(q, ch, ch)
	w := doSSO(t, s, http.MethodPost, govCovChangesPath+"/"+ch.ID.String()+"/approve",
		govCovSession(t, "sub-bob", "bob@corp.example", oidc.RoleSecurityAdmin), "")
	if w.Code != http.StatusConflict || errorReason(w) != reasonGovernanceChangeStale {
		t.Fatalf("approve = %d %q (%s)", w.Code, errorReason(w), w.Body.String())
	}
	if len(q.seen) != 0 {
		t.Errorf("the target was touched after the deployment default moved: %v", q.seen)
	}
	rows := govCovAudits(h, "governance.change.approve")
	if len(rows) != 1 || rows[0].Outcome != "failure" || govCovAuditData(t, rows[0])["error"] != "stale" {
		t.Errorf("approve rows = %+v", rows)
	}
	if len(govCovAudits(h, "capability.grant.delete")) != 0 {
		t.Error("a stale approval audited the target write")
	}
}

// An API token that authenticated the request is read again inside the decision transaction.
func TestGovCovDecideRechecksAnAPITokenApprover(t *testing.T) {
	boom := errors.New("pg: down")
	tok := types.APIToken{ID: govCovGrantID, Principal: "sub-bob", Email: "bob@corp.example", Role: oidc.RoleSecurityAdmin, UserType: types.UserTypeStandard}
	for _, c := range []struct {
		name       string
		rowErr     func(string) error
		wantCode   int
		wantReason string
		wantFail   bool
	}{
		{"a token revoked since it authenticated is the auth lane's 401", nil, http.StatusUnauthorized, reasonInvalidAdminToken, false},
		{"a token whose re-read fails is a 500 and a recorded failure", func(string) error { return boom }, http.StatusInternalServerError, reasonInternalError, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &govCovStore{apiToken: tok}
			s, h := govCovServer(t, st)
			ch := govCovGrantDelete(s, "sub-alice", "alice@corp.example")
			q := &govCovQuerier{rowErr: c.rowErr}
			st.got = ch
			st.decideFn = govCovDecideThrough(q, ch, ch)
			w := do(t, s, http.MethodPost, govCovChangesPath+"/"+ch.ID.String()+"/approve", "wdn_test-token", "")
			if w.Code != c.wantCode || errorReason(w) != c.wantReason {
				t.Fatalf("approve = %d %q (%s)", w.Code, errorReason(w), w.Body.String())
			}
			if len(q.seen) != 1 || !strings.Contains(q.seen[0], "api_tokens") || len(q.writes()) != 0 {
				t.Errorf("statements = %v, want the one token re-read and no write", q.seen)
			}
			if got := len(govCovAudits(h, "governance.change.approve")) == 1; got != c.wantFail {
				t.Errorf("failure row present = %v, want %v", got, c.wantFail)
			}
		})
	}
}
