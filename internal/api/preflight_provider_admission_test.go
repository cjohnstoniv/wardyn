// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// firePreflightAndCreate drives the SAME body through /runs/preflight and
// /runs with the same provider policy and the same caller, and hands back both
// responses. The workspace is onboarded for the reason admitRunDoor onboards
// it: an un-onboarded repo is refused one line earlier, for the wrong reason.
func firePreflightAndCreate(t *testing.T, sc types.SiteConfig, repo, body string, operator bool) (pre, create *httptest.ResponseRecorder) {
	t.Helper()
	fire := func(path string) *httptest.ResponseRecorder {
		srv, st, _ := govEscapeFixture(t, &capStore{})
		st.siteConfig = sc
		st.workspaces = []types.Workspace{{
			ID: uuid.New(), Name: "app", OwnedBy: govMemberSub,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: repo}},
		}}
		session := govSession(t, govMemberSub, []string{"eng"}, false)
		if operator {
			session = admitAdminSession(t)
		}
		return doSSO(t, srv, http.MethodPost, path, session, body)
	}
	return fire("/api/v1/runs/preflight"), fire("/api/v1/runs")
}

// handlePreflightRun reproduced resolveRunPolicy's 4xx set, the workspace seed
// and the confinement floor — but NOT requestRepoProviderRefusals, the gate
// over the two FREE-TEXT repository fields. So Review showed a clean checklist
// for a `repo`/`devcontainer_repo` POST /runs then refused: 422 for an
// operator, 403 for a member. TestPreflightMirrorsLaunchGates could not catch
// it because the whole decodeAndValidateCreateRun wrapper was excepted.
func TestPreflightAnswersTheSameProviderRefusalAsCreate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		operator bool
	}{
		{
			name: "the legacy repo field, refused for an operator",
			body: `{"agent":"claude-code","task":"t","repo":` + quote(admitOffRow) + `}`, operator: true,
		},
		{
			name: "the legacy repo field, refused for a member",
			body: `{"agent":"claude-code","task":"t","repo":` + quote(admitOffRow) + `}`,
		},
		{
			name: "devcontainer_repo, operator-only and cloned server-side",
			body: `{"agent":"claude-code","task":"t","devcontainer_repo":` + quote(admitOffRow) + `}`, operator: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pre, create := firePreflightAndCreate(t, admitSite(), admitOffRow, tc.body, tc.operator)
			if create.Code == http.StatusOK || create.Code == http.StatusCreated {
				t.Fatalf("fixture no longer refuses at create (code %d: %s)", create.Code, create.Body.String())
			}
			if pre.Code != create.Code {
				t.Errorf("preflight code = %d, create code = %d — Review previewed a run launch refuses\npreflight body: %s",
					pre.Code, create.Code, pre.Body.String())
			}
			if pre.Body.String() != create.Body.String() {
				t.Errorf("preflight body = %s\ncreate body   = %s\nthe two doors must answer the same refusal",
					pre.Body.String(), create.Body.String())
			}
		})
	}
}

// TestPreflightAndCreateRefuseInTheSameORDER is R9. The structural parity guard
// can see which gates each door runs but not the sequence, so a body that
// violates TWO of them is the only input that can tell the orders apart — and
// Review's job is to answer the refusal the caller is about to meet, not merely
// one of the refusals they would meet.
func TestPreflightAndCreateRefuseInTheSameORDER(t *testing.T) {
	// An over-long repo (a field-cap 400) that is ALSO outside every enabled
	// provider (an admission 422). Create asks admission first.
	longUnadmitted := "other/" + strings.Repeat("a", maxRunRepoLen)
	body := `{"agent":"claude-code","task":"t","repo":` + quote(longUnadmitted) + `}`
	pre, create := firePreflightAndCreate(t, admitSite(), longUnadmitted, body, true)
	if create.Code != http.StatusUnprocessableEntity {
		t.Fatalf("fixture no longer exercises the ordering (create code = %d): %s", create.Code, create.Body.String())
	}
	if pre.Code != create.Code {
		t.Errorf("preflight code = %d, create code = %d — the two doors run the same gates in a different order\npreflight body: %s",
			pre.Code, create.Code, pre.Body.String())
	}
}

// TestPreflightUnchangedInLegacyOpenMode is the negative control: with NO
// provider rows admission is a no-op (legacy open mode, byte-identical to
// 0.7.1), so preflight must still answer 200 for the same repo.
func TestPreflightUnchangedInLegacyOpenMode(t *testing.T) {
	body := `{"agent":"claude-code","task":"t","repo":` + quote(admitOffRow) + `}`
	pre, _ := firePreflightAndCreate(t, types.SiteConfig{}, admitOffRow, body, true)
	if pre.Code != http.StatusOK {
		t.Fatalf("preflight code = %d, want 200 in legacy open mode: %s", pre.Code, pre.Body.String())
	}
}
