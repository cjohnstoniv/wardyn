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
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The three reads a service identity's token reaches (GET /audit per run, GET /presets and /presets/{name},
// GET /workspaces): each answers only what the service may already launch with, and nothing of another
// identity's or its sponsor's.

const (
	svcName2 = "ci-bot2"
	svcPrin2 = "service:ci-bot2"
	svcType  = "ci"
	svcType2 = "ci2"
)

// pgTeeRecorder writes every row to the audit table (what GET /audit reads) and to the in-memory recorder
// (what waitForRecAudit polls).
type pgTeeRecorder struct {
	rec *recRecorder
	pg  store.Recorder
}

func (r pgTeeRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	err := r.pg.Record(ctx, ev)
	_ = r.rec.Record(ctx, ev)
	return err
}

// siteConfigUnreadable is the PG store with a site config that cannot be read.
type siteConfigUnreadable struct{ store.PG }

func (siteConfigUnreadable) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, errors.New("site config read failed")
}

func (e svcEnv) mustCreateTyped(t *testing.T, name, sponsor, userType string) {
	t.Helper()
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/service-identities", e.sec,
		`{"name":"`+name+`","sponsor":"`+sponsor+`","description":"ci runner","user_type":"`+userType+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create service identity %s: %d %s, want 201", name, w.Code, w.Body.String())
	}
}

// launchRun creates a run with the stored policy pol and waits for its run.create row.
func (e svcEnv) launchRun(t *testing.T, send func(method, path, body string) *httptest.ResponseRecorder, pol string) uuid.UUID {
	t.Helper()
	w := send(http.MethodPost, "/api/v1/runs", `{"agent":"claude-code","task":"t","policy_id":"`+pol+`"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("run create: %d %s, want 201", w.Code, w.Body.String())
	}
	var created createRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	waitForRecAudit(t, e.h.audit, created.ID, "run.create", "success")
	return created.ID
}

func listedWorkspaces(t *testing.T, w *httptest.ResponseRecorder) []uuid.UUID {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("GET /workspaces = %d %s, want 200", w.Code, w.Body.String())
	}
	var rows []struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	out := []uuid.UUID{}
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func listedPresets(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var doc client.PresetsDocument
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &doc) != nil {
		t.Fatalf("GET /presets = %d %s, want 200", w.Code, w.Body.String())
	}
	out := []string{}
	for _, p := range doc.Presets {
		out = append(out, p.Name)
	}
	slices.Sort(out)
	return out
}

// TestServiceReads: a service token reads its own runs' audit, the presets open to its type and the
// workspaces it may launch; another identity's, its sponsor's and any unlaunchable row stay out, and the
// workspace detail and audit export stay off the allow-list.
func TestServiceReads(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	e.h.srv.cfg.Audit = pgTeeRecorder{rec: e.h.audit, pg: store.Recorder{Pool: e.pool}}
	for _, ut := range []string{svcType, svcType2} {
		if _, err := e.st.CreateUserType(ctx, types.UserType{ID: ut, Name: strings.ToUpper(ut)}); err != nil {
			t.Fatalf("seed user type %s: %v", ut, err)
		}
	}
	e.mustCreateTyped(t, svcName, svcSponsor, svcType)
	e.mustCreateTyped(t, svcName2, svcSponsor2, svcType2)
	tok := e.mustMint(t, e.sponsor, svcName).Token
	tok2 := e.mustMint(t, e.sponsor2, svcName2).Token
	svc := func(method, path, body string) *httptest.ResponseRecorder { return e.as(t, tok, method, path, body) }
	get := func(path string) *httptest.ResponseRecorder { return svc(http.MethodGet, path, "") }
	pol := e.plainPolicy(t, "svc-reads-pol")

	// Runs: the service's own, a second identity's, and the sponsor's own.
	own := e.launchRun(t, svc, pol)
	other := e.launchRun(t, func(m, p, b string) *httptest.ResponseRecorder { return e.as(t, tok2, m, p, b) }, pol)
	sponsors := e.launchRun(t, func(m, p, b string) *httptest.ResponseRecorder { return doSSO(t, e.h.srv, m, p, e.sponsor, b) }, pol)

	// Presets, one per openness shape.
	for name, uts := range map[string][]string{
		"open-all": nil, "open-ci": {svcType}, "open-ci2": {svcType2}, "open-sponsor": {"standard"},
	} {
		body, _ := json.Marshal(client.PresetRequest{UserTypes: uts, Request: client.CreateRunRequest{Agent: "claude-code"}})
		if w := do(t, e.h.srv, http.MethodPut, "/api/v1/presets/"+name, adminToken, string(body)); w.Code != http.StatusCreated {
			t.Fatalf("put preset %s = %d %s", name, w.Code, w.Body.String())
		}
	}

	// Workspaces: the launchable one first, so every newer row is one the service may not launch.
	ephemeral := []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}}
	mkWS := func(name, owner string) uuid.UUID {
		t.Helper()
		ws, err := e.st.CreateWorkspace(ctx, types.Workspace{ID: uuid.New(), Name: name, OwnedBy: owner,
			Status: types.WorkspaceScanned, Sources: ephemeral})
		if err != nil {
			t.Fatalf("create workspace %s: %v", name, err)
		}
		return ws.ID
	}
	wsOpen := mkWS("operator-open", "")
	wsOther := mkWS("second-identity", svcPrin2)
	wsSponsor := mkWS("sponsor-owned", svcSponsor)
	wsRestricted := mkWS("operator-restricted", "")
	if err := e.st.SetCapabilityRestriction(ctx, capWorkspace, wsRestricted.String(), true, "test"); err != nil {
		t.Fatal(err)
	}

	emptyAudit := func(t *testing.T, path string) string {
		t.Helper()
		w := get(path)
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
			t.Errorf("GET %s = %d %s, want 200 []", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	adminHasRows := func(t *testing.T, run uuid.UUID) {
		t.Helper()
		w := do(t, e.h.srv, http.MethodGet, "/api/v1/audit?run_id="+run.String(), adminToken, "")
		if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) == "[]" {
			t.Fatalf("admin GET /audit?run_id=%s = %d %s, want rows (the empty answer must not be trivial)", run, w.Code, w.Body.String())
		}
	}

	t.Run("audit_own_run", func(t *testing.T) {
		w := get("/api/v1/audit?run_id=" + own.String())
		var rows []types.AuditEvent
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil {
			t.Fatalf("GET /audit own run = %d %s, want 200", w.Code, w.Body.String())
		}
		found := false
		for _, ev := range rows {
			if ev.RunID == nil || *ev.RunID != own {
				t.Errorf("row %s names run %v, want only %s", ev.Action, ev.RunID, own)
			}
			found = found || ev.Action == "run.create" && ev.Actor == svcPrin
		}
		if !found {
			t.Errorf("own run's audit = %s, want a run.create row with actor %s", w.Body.String(), svcPrin)
		}
	})
	t.Run("audit_other_identity_run", func(t *testing.T) {
		adminHasRows(t, other)
		got := emptyAudit(t, "/api/v1/audit?run_id="+other.String())
		if unknown := emptyAudit(t, "/api/v1/audit?run_id="+uuid.NewString()); got != unknown {
			t.Errorf("another identity's run %q differs from an unknown run %q", got, unknown)
		}
		if none := emptyAudit(t, "/api/v1/audit"); got != none {
			t.Errorf("another identity's run %q differs from no run_id %q", got, none)
		}
	})
	t.Run("audit_sponsor_run", func(t *testing.T) {
		adminHasRows(t, sponsors)
		emptyAudit(t, "/api/v1/audit?run_id="+sponsors.String())
	})
	t.Run("audit_no_run_id", func(t *testing.T) {
		emptyAudit(t, "/api/v1/audit")
	})
	t.Run("audit_export_scoped", func(t *testing.T) {
		if w := get("/api/v1/audit/export?run_id=" + own.String()); w.Code != http.StatusForbidden || respReason(w) != "service_scope" {
			t.Errorf("GET /audit/export = %d %s, want 403 service_scope", w.Code, w.Body.String())
		}
	})

	t.Run("presets_listed", func(t *testing.T) {
		if got, want := listedPresets(t, get("/api/v1/presets")), []string{"open-all", "open-ci"}; !slices.Equal(got, want) {
			t.Errorf("service preset list = %v, want %v", got, want)
		}
		for _, name := range []string{"open-all", "open-ci"} {
			if w := get("/api/v1/presets/" + name); w.Code != http.StatusOK {
				t.Errorf("GET /presets/%s = %d %s, want 200", name, w.Code, w.Body.String())
			}
		}
	})
	t.Run("presets_unlaunchable", func(t *testing.T) {
		missing := get("/api/v1/presets/no-such-preset")
		if missing.Code != http.StatusNotFound {
			t.Fatalf("missing preset = %d %s, want 404", missing.Code, missing.Body.String())
		}
		// A type the service does not hold (the second identity's), and the sponsor's type.
		for _, name := range []string{"open-ci2", "open-sponsor"} {
			if got := listedPresets(t, get("/api/v1/presets")); slices.Contains(got, name) {
				t.Errorf("preset %s is listed for the service: %v", name, got)
			}
			if w := get("/api/v1/presets/" + name); w.Code != http.StatusNotFound || w.Body.String() != missing.Body.String() {
				t.Errorf("GET /presets/%s = %d %s, want the missing name's 404 %s", name, w.Code, w.Body.String(), missing.Body.String())
			}
			w := svc(http.MethodPost, "/api/v1/runs", `{"preset":"`+name+`","task":"x"}`)
			if w.Code != http.StatusUnprocessableEntity || respReason(w) != reasonPresetUnknown {
				t.Errorf("launch preset %s = %d %s, want 422 %s", name, w.Code, w.Body.String(), reasonPresetUnknown)
			}
		}
	})
	t.Run("presets_read_equals_launch", func(t *testing.T) {
		srv := e.h.srv
		prevGovern := srv.cfg.GovernAdminRuns
		t.Cleanup(func() { srv.cfg.GovernAdminRuns = prevGovern })
		for _, role := range []string{oidc.RoleUser, oidc.RoleAdmin} {
			for _, govern := range []bool{false, true} {
				srv.cfg.GovernAdminRuns = govern
				c := withServiceIdentity(withHumanIdentity(context.Background(), svcPrin, "", role, svcType, []string{}, false), svcName, svcSponsor)
				r := httptest.NewRequest(http.MethodGet, "/api/v1/presets", nil).WithContext(c)
				for _, uts := range [][]string{nil, {svcType}, {svcType2}, {"standard"}, {svcType2, svcType}} {
					p := types.LaunchPreset{Name: "p", UserTypes: uts}
					want := len(uts) == 0 || slices.Contains(uts, svcType)
					if read, launch := srv.presetOpenTo(r, p), srv.presetLaunchOpenTo(r, p); read != launch || read != want {
						t.Errorf("role %s govern %v types %v: presetOpenTo=%v presetLaunchOpenTo=%v, want both %v", role, govern, uts, read, launch, want)
					}
				}
			}
		}
	})

	t.Run("workspaces_listed", func(t *testing.T) {
		if got := listedWorkspaces(t, get("/api/v1/workspaces")); !slices.Equal(got, []uuid.UUID{wsOpen}) {
			t.Errorf("service workspace list = %v, want only the launchable %s", got, wsOpen)
		}
	})
	t.Run("workspaces_not_available", func(t *testing.T) {
		if got := listedWorkspaces(t, get("/api/v1/workspaces")); slices.Contains(got, wsRestricted) {
			t.Errorf("the capWorkspace-restricted workspace %s is listed: %v", wsRestricted, got)
		}
		preflight := func(id uuid.UUID) int {
			return svc(http.MethodPost, "/api/v1/runs/preflight",
				fmt.Sprintf(`{"agent":"claude-code","task":"t","task_mode":"exec","policy_id":%q,"workspace_id":%q}`, pol, id)).Code
		}
		if open, restricted := preflight(wsOpen), preflight(wsRestricted); open != http.StatusOK || restricted == http.StatusOK {
			t.Errorf("preflight open=%d restricted=%d, want 200 and a refusal", open, restricted)
		}
	})
	t.Run("workspaces_other_identity", func(t *testing.T) {
		if got := listedWorkspaces(t, get("/api/v1/workspaces")); slices.Contains(got, wsOther) {
			t.Errorf("the second identity's workspace %s is listed: %v", wsOther, got)
		}
	})
	t.Run("workspaces_sponsor", func(t *testing.T) {
		if got := listedWorkspaces(t, get("/api/v1/workspaces")); slices.Contains(got, wsSponsor) {
			t.Errorf("the sponsor's workspace %s is listed: %v", wsSponsor, got)
		}
	})
	t.Run("workspaces_limit_1", func(t *testing.T) {
		if got := listedWorkspaces(t, get("/api/v1/workspaces?limit=1")); !slices.Equal(got, []uuid.UUID{wsOpen}) {
			t.Errorf("?limit=1 = %v, want the first launchable row %s", got, wsOpen)
		}
	})
	t.Run("workspaces_detail_scoped", func(t *testing.T) {
		first := get("/api/v1/workspaces/" + wsOpen.String())
		if first.Code != http.StatusForbidden || respReason(first) != "service_scope" {
			t.Errorf("GET /workspaces/{id} = %d %s, want 403 service_scope", first.Code, first.Body.String())
		}
		if unknown := get("/api/v1/workspaces/" + uuid.NewString()); unknown.Code != first.Code || unknown.Body.String() != first.Body.String() {
			t.Errorf("detail of an unknown id = %d %s, want the same answer as a real one", unknown.Code, unknown.Body.String())
		}
	})
	// Last: it swaps the store.
	t.Run("workspaces_site_config_unreadable", func(t *testing.T) {
		prev := e.h.srv.cfg.Store
		e.h.srv.cfg.Store = siteConfigUnreadable{e.st}
		t.Cleanup(func() { e.h.srv.cfg.Store = prev })
		w := get("/api/v1/workspaces")
		if w.Code != http.StatusServiceUnavailable || respReason(w) != reasonRosterUnreadable {
			t.Errorf("service list with an unreadable site config = %d %s, want 503 %s", w.Code, w.Body.String(), reasonRosterUnreadable)
		}
		// A person's list keeps its advisory, degraded stamp.
		if hw := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/workspaces", e.sponsor, ""); hw.Code != http.StatusOK {
			t.Errorf("a person's list with an unreadable site config = %d %s, want 200", hw.Code, hw.Body.String())
		}
	})
}
