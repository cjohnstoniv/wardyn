// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// memPresetStore is the launch-preset slice of the store, in memory, with the
// PG upsert's contract: a new name lands at version 1, a body identical to the
// stored one writes nothing, and a changed body moves the version by one. It
// lets the preset handlers run in the unit suites, which carry no Postgres.
type memPresetStore struct {
	store.Store
	mu    sync.Mutex
	rows  map[string]types.LaunchPreset
	utype []types.UserType
}

func (m *memPresetStore) ListLaunchPresets(context.Context) ([]types.LaunchPreset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []types.LaunchPreset
	for _, p := range m.rows {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b types.LaunchPreset) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (m *memPresetStore) GetLaunchPreset(_ context.Context, name string) (types.LaunchPreset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[name]
	if !ok {
		return types.LaunchPreset{}, store.ErrNotFound
	}
	return p, nil
}

func (m *memPresetStore) PutLaunchPreset(_ context.Context, p types.LaunchPreset) (types.LaunchPreset, store.PresetWrite, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]types.LaunchPreset{}
	}
	old, ok := m.rows[p.Name]
	switch {
	case !ok:
		p.Version = 1
		m.rows[p.Name] = p
		return p, store.PresetCreated, nil
	case old.Description == p.Description && slices.Equal(old.UserTypes, p.UserTypes) && string(old.Request) == string(p.Request):
		return old, store.PresetUnchanged, nil
	}
	p.Version = old.Version + 1
	m.rows[p.Name] = p
	return p, store.PresetUpdated, nil
}

func (m *memPresetStore) DeleteLaunchPreset(_ context.Context, name string) (types.LaunchPreset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.rows[name]
	if !ok {
		return types.LaunchPreset{}, store.ErrNotFound
	}
	delete(m.rows, name)
	return p, nil
}

func (m *memPresetStore) ListUserTypes(context.Context) ([]types.UserType, error) {
	return m.utype, nil
}

type memPresetFixture struct {
	srv   *Server
	store *memPresetStore
	audit *recRecorder
}

func newMemPresetFixture(t *testing.T) memPresetFixture {
	t.Helper()
	h := newHarness(t)
	st := &memPresetStore{utype: []types.UserType{{ID: "contractor", Name: "Contractor"}}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	return memPresetFixture{srv: New(cfg), store: st, audit: h.audit}
}

func (f memPresetFixture) put(t *testing.T, name string, req client.PresetRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(req)
	return do(t, f.srv, http.MethodPut, "/api/v1/presets/"+name, adminToken, string(body))
}

func reasonOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var got errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	return got.Reason
}

func TestPresetWriteRefusals(t *testing.T) {
	f := newMemPresetFixture(t)
	ok := client.CreateRunRequest{Agent: "claude-code", Repo: "acme/widgets"}
	withTask := ok
	withTask.Task = "baked in"
	noAgent := client.CreateRunRequest{Repo: "acme/widgets"}
	badClass := ok
	badClass.ConfinementClass = "CC9"
	badPolicy := ok
	badPolicy.InlinePolicy = &types.RunPolicySpec{AllowedDomains: []string{"not a host"}}
	longRepo := ok
	longRepo.Repo = strings.Repeat("r", maxRunRepoLen+1)

	for _, tc := range []struct {
		name   string
		preset string
		req    client.PresetRequest
		want   string // substring of the refusal message
	}{
		{"uppercase name", "Not_A_Slug", client.PresetRequest{Request: ok}, "lowercase letters and digits"},
		{"name too long", strings.Repeat("a", maxPresetNameLen+1), client.PresetRequest{Request: ok}, "at most 63 characters"},
		{"double hyphen", "a--b", client.PresetRequest{Request: ok}, "single hyphens"},
		{"description too long", "p", client.PresetRequest{Description: strings.Repeat("d", maxPresetDescriptionLen+1), Request: ok}, "description must be at most"},
		{"description control char", "p", client.PresetRequest{Description: "a\x07b", Request: ok}, "no control characters"},
		{"per-launch task baked in", "p", client.PresetRequest{Request: withTask}, "request.task is set per launch"},
		{"no agent", "p", client.PresetRequest{Request: noAgent}, "agent is required"},
		{"unknown confinement class", "p", client.PresetRequest{Request: badClass}, "unknown confinement_class"},
		{"invalid inline policy", "p", client.PresetRequest{Request: badPolicy}, "invalid inline_policy"},
		{"unknown user type", "p", client.PresetRequest{UserTypes: []string{"ghost"}, Request: ok}, "does not exist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := f.put(t, tc.preset, tc.req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("= %d %s, want 400", w.Code, w.Body.String())
			}
			if got := reasonOf(t, w); got != reasonPresetRequestInvalid {
				t.Errorf("reason = %q, want %q", got, reasonPresetRequestInvalid)
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body %s lacks %q", w.Body.String(), tc.want)
			}
		})
	}

	// A text-field refusal keeps its own reason rather than the preset's.
	if w := f.put(t, "p", client.PresetRequest{Request: longRepo}); w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonRunFieldTooLong {
		t.Errorf("over-long repo = %d %s, want 400 %s", w.Code, w.Body.String(), reasonRunFieldTooLong)
	}
	if list, _ := f.store.ListLaunchPresets(context.Background()); len(list) != 0 {
		t.Errorf("refused writes stored %d preset(s)", len(list))
	}
	if w := do(t, f.srv, http.MethodPut, "/api/v1/presets/p", adminToken, `{"request":{"agent":"x"},"surprise":1}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown body field = %d, want 400", w.Code)
	}
}

func TestPresetCRUDVersioningAndAudit(t *testing.T) {
	f := newMemPresetFixture(t)
	req := client.PresetRequest{
		Description: "widgets CI",
		UserTypes:   []string{"contractor", "contractor"},
		Request:     client.CreateRunRequest{Agent: "claude-code", Repo: "acme/widgets"},
	}
	var p client.Preset
	w := f.put(t, "widgets", req)
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || w.Code != http.StatusCreated || p.Version != 1 {
		t.Fatalf("create = %d %s, want 201 at version 1", w.Code, w.Body.String())
	}
	if !slices.Equal(p.UserTypes, []string{"contractor"}) || p.Request.Repo != "acme/widgets" || p.CreatedBy == "" {
		t.Errorf("created preset = %+v, want deduplicated user types, the stored request and a creator", p)
	}

	w = f.put(t, "widgets", req)
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || w.Code != http.StatusOK || p.Version != 1 {
		t.Fatalf("identical put = %d %s, want 200 at version 1", w.Code, w.Body.String())
	}
	req.Request.Repo = "acme/gadgets"
	w = f.put(t, "widgets", req)
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || w.Code != http.StatusOK || p.Version != 2 {
		t.Fatalf("changed put = %d %s, want 200 at version 2", w.Code, w.Body.String())
	}

	count := func(action string) (n int) {
		for _, ev := range f.audit.snapshot() {
			if ev.Action == action {
				n++
			}
		}
		return n
	}
	if count("preset.create") != 1 || count("preset.update") != 1 {
		t.Errorf("audit rows: %d create, %d update; want 1 and 1 (the identical put writes none)", count("preset.create"), count("preset.update"))
	}

	w = do(t, f.srv, http.MethodGet, "/api/v1/presets/widgets", adminToken, "")
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || w.Code != http.StatusOK || p.Request.Repo != "acme/gadgets" {
		t.Errorf("get = %d %s, want the version-2 request", w.Code, w.Body.String())
	}
	var doc client.PresetsDocument
	w = do(t, f.srv, http.MethodGet, "/api/v1/presets", adminToken, "")
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || len(doc.Presets) != 1 || doc.Presets[0].Name != "widgets" {
		t.Errorf("list = %d %s, want the one preset", w.Code, w.Body.String())
	}
	if w := do(t, f.srv, http.MethodGet, "/api/v1/presets/nope", adminToken, ""); w.Code != http.StatusNotFound || reasonOf(t, w) != reasonPresetNotFound {
		t.Errorf("get unknown = %d %s, want 404 %s", w.Code, w.Body.String(), reasonPresetNotFound)
	}

	if w := do(t, f.srv, http.MethodDelete, "/api/v1/presets/widgets", adminToken, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	if w := do(t, f.srv, http.MethodDelete, "/api/v1/presets/widgets", adminToken, ""); w.Code != http.StatusNotFound || reasonOf(t, w) != reasonPresetNotFound {
		t.Errorf("second delete = %d %s, want 404 %s", w.Code, w.Body.String(), reasonPresetNotFound)
	}
	if count("preset.delete") != 1 {
		t.Errorf("%d preset.delete rows, want 1 (the 404 delete writes none)", count("preset.delete"))
	}
}

func TestPresetClosedToOtherUserTypesIsHidden(t *testing.T) {
	f := newMemPresetFixture(t)
	for name, types := range map[string][]string{"open": nil, "contractors-only": {"contractor"}} {
		w := f.put(t, name, client.PresetRequest{UserTypes: types, Request: client.CreateRunRequest{Agent: "claude-code"}})
		if w.Code != http.StatusCreated {
			t.Fatalf("put %s = %d: %s", name, w.Code, w.Body.String())
		}
	}
	member := ssoSession(t, "sub-preset-member", "member@corp.example", oidc.RoleUser)

	var doc client.PresetsDocument
	w := doSSO(t, f.srv, http.MethodGet, "/api/v1/presets", member, "")
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || w.Code != http.StatusOK || len(doc.Presets) != 1 || doc.Presets[0].Name != "open" {
		t.Errorf("standard member list = %d %s, want only the open preset", w.Code, w.Body.String())
	}
	if w := doSSO(t, f.srv, http.MethodGet, "/api/v1/presets/contractors-only", member, ""); w.Code != http.StatusNotFound || reasonOf(t, w) != reasonPresetNotFound {
		t.Errorf("standard member read of a closed preset = %d %s, want 404 %s", w.Code, w.Body.String(), reasonPresetNotFound)
	}
	if w := doSSO(t, f.srv, http.MethodGet, "/api/v1/presets/open", member, ""); w.Code != http.StatusOK {
		t.Errorf("standard member read of an open preset = %d, want 200", w.Code)
	}
	contractor := ssoSessionOfType(t, "sub-contractor", "c@corp.example", oidc.RoleUser, "contractor")
	if w := doSSO(t, f.srv, http.MethodGet, "/api/v1/presets/contractors-only", contractor, ""); w.Code != http.StatusOK {
		t.Errorf("contractor read of its own preset = %d, want 200", w.Code)
	}
	body, _ := json.Marshal(client.PresetRequest{Request: client.CreateRunRequest{Agent: "claude-code"}})
	if w := doSSO(t, f.srv, http.MethodPut, "/api/v1/presets/mine", member, string(body)); w.Code != http.StatusForbidden {
		t.Errorf("member put = %d, want 403", w.Code)
	}
}

// TestExpandRunPreset drives the launch-side expansion directly: what a
// preset launch may carry, what it is refused for, and what the expanded
// request keeps of the caller's body.
func TestExpandRunPreset(t *testing.T) {
	f := newMemPresetFixture(t)
	if w := f.put(t, "widgets", client.PresetRequest{Request: client.CreateRunRequest{Agent: "claude-code", Repo: "acme/widgets"}}); w.Code != http.StatusCreated {
		t.Fatalf("seed = %d: %s", w.Code, w.Body.String())
	}
	if w := f.put(t, "closed", client.PresetRequest{UserTypes: []string{"contractor"}, Request: client.CreateRunRequest{Agent: "claude-code"}}); w.Code != http.StatusCreated {
		t.Fatalf("seed closed = %d: %s", w.Code, w.Body.String())
	}
	expand := func(req createRunRequest) (createRunRequest, *httptest.ResponseRecorder, bool) {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		w := httptest.NewRecorder()
		ok := f.srv.expandRunPreset(w, r, &req)
		return req, w, ok
	}

	t.Run("no preset passes through", func(t *testing.T) {
		in := createRunRequest{Agent: "codex-cli", Repo: "x/y"}
		out, w, ok := expand(in)
		if !ok || out.Agent != "codex-cli" || out.Preset != "" || w.Body.Len() != 0 {
			t.Errorf("ok=%v out=%+v body=%q, want the request untouched", ok, out, w.Body.String())
		}
	})
	t.Run("version without name", func(t *testing.T) {
		_, w, ok := expand(createRunRequest{PresetVersion: 2})
		if ok || w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonPresetVersionNoName {
			t.Errorf("ok=%v %d %s, want 400 %s", ok, w.Code, w.Body.String(), reasonPresetVersionNoName)
		}
	})
	t.Run("extra field beside preset", func(t *testing.T) {
		_, w, ok := expand(createRunRequest{Preset: "widgets", Image: "ghcr.io/x/y:1"})
		if ok || w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonPresetField || !strings.Contains(w.Body.String(), "image") {
			t.Errorf("ok=%v %d %s, want 400 %s naming image", ok, w.Code, w.Body.String(), reasonPresetField)
		}
	})
	t.Run("unknown preset", func(t *testing.T) {
		_, w, ok := expand(createRunRequest{Preset: "ghost"})
		if ok || w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonPresetUnknown {
			t.Errorf("ok=%v %d %s, want 422 %s", ok, w.Code, w.Body.String(), reasonPresetUnknown)
		}
	})
	t.Run("closed to the caller's type answers like unknown", func(t *testing.T) {
		member := ssoSession(t, "sub-member", "m@corp.example", oidc.RoleUser)
		for _, name := range []string{"closed", "ghost"} {
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/runs", member, `{"preset":"`+name+`"}`)
			want := `preset \"` + name + `\" does not exist or is not open to you`
			if w.Code != http.StatusUnprocessableEntity || reasonOf(t, w) != reasonPresetUnknown || !strings.Contains(w.Body.String(), want) {
				t.Errorf("launch %q = %d %s, want 422 %s with the same message either way", name, w.Code, w.Body.String(), reasonPresetUnknown)
			}
		}
	})
	t.Run("pinned version moved", func(t *testing.T) {
		_, w, ok := expand(createRunRequest{Preset: "widgets", PresetVersion: 7})
		if ok || w.Code != http.StatusConflict || reasonOf(t, w) != reasonPresetVersionMoved {
			t.Errorf("ok=%v %d %s, want 409 %s", ok, w.Code, w.Body.String(), reasonPresetVersionMoved)
		}
	})
	t.Run("expands, keeping title and task", func(t *testing.T) {
		out, _, ok := expand(createRunRequest{Preset: "widgets", PresetVersion: 1, Title: "nightly", Task: "run tests"})
		if !ok {
			t.Fatal("expansion refused")
		}
		if out.Agent != "claude-code" || out.Repo != "acme/widgets" || out.Title != "nightly" || out.Task != "run tests" ||
			out.Preset != "widgets" || out.PresetVersion != 1 {
			t.Errorf("expanded = %+v, want the preset's request with the caller's title/task and the (widgets, 1) stamp", out)
		}
	})
	t.Run("stored request that no longer decodes is a server error", func(t *testing.T) {
		f.store.mu.Lock()
		f.store.rows["broken"] = types.LaunchPreset{Name: "broken", Version: 1, Request: json.RawMessage(`{"not_a_field":1}`)}
		f.store.mu.Unlock()
		_, w, ok := expand(createRunRequest{Preset: "broken"})
		if ok || w.Code != http.StatusInternalServerError {
			t.Errorf("ok=%v %d %s, want 500", ok, w.Code, w.Body.String())
		}
	})
}

func TestSetRunFieldsNamesOnlyNonZeroFieldsByJSONKey(t *testing.T) {
	got := setRunFields(createRunRequest{Title: "t", Task: "x", PresetVersion: 3})
	slices.Sort(got)
	if want := []string{"preset_version", "task", "title"}; !slices.Equal(got, want) {
		t.Errorf("setRunFields = %v, want %v", got, want)
	}
	if got := setRunFields(createRunRequest{}); len(got) != 0 {
		t.Errorf("zero request names %v, want none", got)
	}
}
