// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// #1384: a member's bounded Azure DevOps list is said at the doors. Row default
// [read code_write], no governance list: [read pr] keeps [read] and names the
// dropped "Open pull requests" on the 201 (and the run.create row the run
// detail reads), [pr] alone leaves nothing standing and Review refuses it with
// the reason dispatch would.
func TestADOStandingAtTheDoors(t *testing.T) {
	// who: "member" (no governance list), "governed" (a governance list granting
	// PR), "operator" (the admin token) or "admin in the User view".
	postAs := func(t *testing.T, who, path, caps string) (*httptest.ResponseRecorder, *govEscapeStore, *recRecorder) {
		t.Helper()
		cs := &capStore{userTypes: utKnown}
		sub := govMemberSub
		cookie := govSession(t, sub, []string{"eng"}, false)
		switch who {
		case "governed":
			cs.govProfile = &types.GovernanceProfile{ID: uuid.New(), Name: "ado", Ceiling: types.RunPolicySpec{
				MinConfinementClass: types.CC2, AzureDevOpsCapabilities: []adoscope.Capability{adoscope.CapPR}}}
			cs.govTier, cs.govHasGroupTier = types.CapabilitySubjectGroup, true
		case "admin in the User view":
			sub = uvAdminSub
			cookie = uvSession(t, sub, oidc.RoleAdmin, types.UserTypeStandard, utPM)
		}
		srv, st, audit := govEscapeFixture(t, cs)
		st.siteConfig = adoSite(adoEntraTestRow())
		st.workspaces = []types.Workspace{{
			ID: uuid.New(), Name: "app", OwnedBy: sub,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: adoTestRepo}},
		}}
		body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2",` +
			`"workspace_repos":[{"repo":` + quote(adoTestRepo) + `,"target":"/work/repo"}],"azure_devops_capabilities":` + caps + `}}`
		if who == "operator" {
			return do(t, srv, http.MethodPost, path, adminToken, body), st, audit
		}
		return doSSO(t, srv, http.MethodPost, path, cookie, body), st, audit
	}
	post := func(t *testing.T, path, caps string) (*httptest.ResponseRecorder, *govEscapeStore, *recRecorder) {
		t.Helper()
		return postAs(t, "member", path, caps)
	}
	const sentence = "Not included: “Open pull requests”. Your administrator hasn't granted it to you, and it isn't in this provider's default access."
	t.Run("narrowed: the 201 and the run.create row say so", func(t *testing.T) {
		w, st, audit := post(t, "/api/v1/runs", `["read","pr"]`)
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d: %s", w.Code, w.Body.String())
		}
		var resp createRunResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if n := countEqual(resp.Warnings, sentence); n != 1 {
			t.Errorf("201 warnings = %q, want exactly one %q", resp.Warnings, sentence)
		}
		st.mu.Lock()
		var runID uuid.UUID
		for id := range st.runs {
			runID = id
		}
		st.mu.Unlock()
		ev := waitForRecAudit(t, audit, runID, "run.create", "success")
		var data struct {
			Clamp []string `json:"clamp_warnings"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil || countEqual(data.Clamp, sentence) != 1 {
			t.Errorf("run.create clamp_warnings = %q (err %v), want the same sentence", data.Clamp, err)
		}
	})
	t.Run("several dropped: the plural sentence", func(t *testing.T) {
		w, _, _ := post(t, "/api/v1/runs", `["read","pr","policy_admin"]`)
		want := "Not included: “Open pull requests”, “" + adoscope.ShortLabel(adoscope.CapPolicyAdmin) +
			"”. Your administrator hasn't granted them to you, and they aren't in this provider's default access."
		var resp createRunResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != http.StatusCreated || countEqual(resp.Warnings, want) != 1 {
			t.Fatalf("create = %d warnings %q, want one %q", w.Code, resp.Warnings, want)
		}
	})
	// The bound is the member's: an operator's own list stands whole, a governance
	// list that grants the capability keeps it, and an admin looking through the
	// User view is bound exactly like a member.
	for who, want := range map[string]bool{"operator": false, "governed": false, "admin in the User view": true} {
		t.Run(who+": narrowing "+fmt.Sprint(want), func(t *testing.T) {
			w, _, _ := postAs(t, who, "/api/v1/runs", `["read","pr"]`)
			if w.Code != http.StatusCreated {
				t.Fatalf("create = %d: %s", w.Code, w.Body.String())
			}
			var resp createRunResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if got := countEqual(resp.Warnings, sentence) == 1; got != want {
				t.Errorf("sentence present = %v, want %v (warnings %q)", got, want, resp.Warnings)
			}
		})
	}
	t.Run("a list the bound keeps whole says nothing", func(t *testing.T) {
		w, _, _ := post(t, "/api/v1/runs", `["read"]`)
		if w.Code != http.StatusCreated || strings.Contains(w.Body.String(), "Not included") {
			t.Fatalf("create = %d: %s", w.Code, w.Body.String())
		}
	})
	t.Run("preflight carries the narrowing", func(t *testing.T) {
		w, _, _ := post(t, "/api/v1/runs/preflight", `["read","pr"]`)
		var resp preflightResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != http.StatusOK ||
			countEqual(resp.Warnings, sentence) != 1 {
			t.Fatalf("preflight = %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("preflight refuses a list nothing of which may stand", func(t *testing.T) {
		w, _, _ := post(t, "/api/v1/runs/preflight", `["pr"]`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("preflight = %d, want 422: %s", w.Code, w.Body.String())
		}
		for _, want := range []string{`"reason":"` + reasonADOCapabilitiesNonePermitted + `"`, adoNonePermitted([]adoscope.Capability{adoscope.CapPR})[:40]} {
			if !strings.Contains(w.Body.String(), want) {
				t.Errorf("body %s does not carry %q", w.Body.String(), want)
			}
		}
	})
}

func countEqual(ss []string, want string) int {
	n := 0
	for _, s := range ss {
		if s == want {
			n++
		}
	}
	return n
}
