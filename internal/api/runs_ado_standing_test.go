// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// #1384: a member's bounded Azure DevOps list is said at the doors. Row default
// [read code_write], no governance list: [read pr] keeps [read] and names the
// dropped "Open pull requests" on the 201 (and the run.create row the run
// detail reads), [pr] alone leaves nothing standing and Review refuses it with
// the reason dispatch would.
func TestADOStandingAtTheDoors(t *testing.T) {
	post := func(t *testing.T, path, caps string) (*httptest.ResponseRecorder, *govEscapeStore, *recRecorder) {
		t.Helper()
		srv, st, audit := govEscapeFixture(t, &capStore{})
		st.siteConfig = adoSite(adoEntraTestRow())
		st.workspaces = []types.Workspace{{
			ID: uuid.New(), Name: "app", OwnedBy: govMemberSub,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: adoTestRepo}},
		}}
		body := `{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2",` +
			`"workspace_repos":[{"repo":` + quote(adoTestRepo) + `,"target":"/work/repo"}],"azure_devops_capabilities":` + caps + `}}`
		return doSSO(t, srv, http.MethodPost, path, govSession(t, govMemberSub, []string{"eng"}, false), body), st, audit
	}
	const dropped = "Open pull requests"
	t.Run("narrowed: the 201 and the run.create row say so", func(t *testing.T) {
		w, st, audit := post(t, "/api/v1/runs", `["read","pr"]`)
		if w.Code != http.StatusCreated {
			t.Fatalf("create = %d: %s", w.Code, w.Body.String())
		}
		var resp createRunResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if n := countContaining(resp.Warnings, "“"+dropped+"”"); n != 1 {
			t.Errorf("201 warnings = %q, want exactly one sentence naming %q", resp.Warnings, dropped)
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
		if err := json.Unmarshal(ev.Data, &data); err != nil || countContaining(data.Clamp, "“"+dropped+"”") != 1 {
			t.Errorf("run.create clamp_warnings = %q (err %v), want the same sentence", data.Clamp, err)
		}
	})
	t.Run("a list the bound keeps whole says nothing", func(t *testing.T) {
		w, _, _ := post(t, "/api/v1/runs", `["read"]`)
		if w.Code != http.StatusCreated || strings.Contains(w.Body.String(), "Azure DevOps capabilit") {
			t.Fatalf("create = %d: %s", w.Code, w.Body.String())
		}
	})
	t.Run("preflight carries the narrowing", func(t *testing.T) {
		w, _, _ := post(t, "/api/v1/runs/preflight", `["read","pr"]`)
		var resp preflightResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != http.StatusOK ||
			countContaining(resp.Warnings, "“"+dropped+"”") != 1 {
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

func countContaining(ss []string, sub string) int {
	n := 0
	for _, s := range ss {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}
