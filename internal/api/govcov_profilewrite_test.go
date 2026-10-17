// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// govCovBuildErr runs the real build for a request body and returns what it refused with.
func govCovBuildErr(t *testing.T, s *Server, all []types.GovernanceProfile, id uuid.UUID, body string) error {
	t.Helper()
	var req governanceProfileRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	req, msg := finishGovernanceProfileRequest(req)
	if msg == "" {
		msg = req.parseComposition()
	}
	if msg != "" {
		t.Fatalf("fixture body refused before the build: %s", msg)
	}
	_, _, err := s.buildGovernanceProfile("alice", id, req, all)
	return err
}

func TestGovCovParseComposition(t *testing.T) {
	baseID := govCovBase.String()
	for _, c := range []struct {
		name    string
		req     governanceProfileRequest
		wantMsg string
		check   func(t *testing.T, c compositionRequest)
	}{
		{"nothing set", governanceProfileRequest{}, "", func(t *testing.T, c compositionRequest) {
			if c != (compositionRequest{}) {
				t.Errorf("comp = %+v, want none", c)
			}
		}},
		{"a base, an overlay and limits", governanceProfileRequest{
			BaseProfileID: json.RawMessage(`"` + baseID + `"`),
			Overlay:       json.RawMessage(`{"allowed_domains":["pypi.org"]}`),
			OverlayLimits: json.RawMessage(`{"max_concurrent_runs":1}`),
		}, "", func(t *testing.T, c compositionRequest) {
			if !c.baseSet || *c.base != govCovBase || !c.overlaySet || c.overlay == nil || !c.limitsSet || *c.overlayLimits.MaxConcurrentRuns != 1 {
				t.Errorf("comp = %+v", c)
			}
		}},
		{"explicit nulls are set and nil", governanceProfileRequest{
			BaseProfileID: json.RawMessage(`null`), Overlay: json.RawMessage(`null`), OverlayLimits: json.RawMessage(`null`),
		}, "", func(t *testing.T, c compositionRequest) {
			if !c.baseSet || c.base != nil || !c.overlaySet || c.overlay != nil || !c.limitsSet || c.overlayLimits != nil {
				t.Errorf("comp = %+v", c)
			}
		}},
		{"a base that is not a string", governanceProfileRequest{BaseProfileID: json.RawMessage(`7`)}, "base_profile_id: must be a profile id or null", nil},
		{"a base that is not a uuid", governanceProfileRequest{BaseProfileID: json.RawMessage(`"x"`)}, "base_profile_id: not a profile id", nil},
		{"an overlay with an unknown key", governanceProfileRequest{Overlay: json.RawMessage(`{"nope":true}`)}, "invalid overlay:", nil},
		{"limits with an unknown key", governanceProfileRequest{OverlayLimits: json.RawMessage(`{"nope":true}`)}, "invalid overlay_limits:", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := c.req
			msg := req.parseComposition()
			if c.wantMsg != "" {
				if !strings.HasPrefix(msg, c.wantMsg) {
					t.Errorf("message = %q, want prefix %q", msg, c.wantMsg)
				}
				return
			}
			if msg != "" {
				t.Fatalf("message = %q", msg)
			}
			c.check(t, req.comp)
		})
	}
}

func TestGovCovComposedFrom(t *testing.T) {
	stored := govCovComposedOn(govCovPID, "p", govCovBase, "pypi.org")
	stored.OverlayLimits = &types.LimitsOverlay{}
	all := []types.GovernanceProfile{stored}
	newBase := govCovMid
	newOverlay := &types.CeilingOverlay{}
	newLimits := &types.LimitsOverlay{DenyInteractive: new(bool)}

	for _, c := range []struct {
		name         string
		id           uuid.UUID
		comp         compositionRequest
		wantBase     *uuid.UUID
		wantOverlay  *types.CeilingOverlay
		wantLimits   *types.LimitsOverlay
		wantBaseKept bool
	}{
		{"an unnamed profile has no composition", govCovTop, compositionRequest{}, nil, nil, nil, false},
		{"absent members keep the stored composition", govCovPID, compositionRequest{}, stored.BaseProfileID, stored.Overlay, stored.OverlayLimits, true},
		{"a null overlay clears the base and the limits too", govCovPID, compositionRequest{overlaySet: true}, nil, nil, nil, false},
		{"a new base replaces the stored one", govCovPID, compositionRequest{baseSet: true, base: &newBase}, &newBase, stored.Overlay, stored.OverlayLimits, false},
		{"a new overlay and new limits replace the stored ones", govCovPID, compositionRequest{overlaySet: true, overlay: newOverlay, limitsSet: true, overlayLimits: newLimits},
			stored.BaseProfileID, newOverlay, newLimits, true},
		{"null limits clear only the limits", govCovPID, compositionRequest{limitsSet: true}, stored.BaseProfileID, stored.Overlay, nil, true},
		{"a null base on a new profile stays none", govCovTop, compositionRequest{baseSet: true}, nil, nil, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			base, overlay, limits := composedFrom(all, c.id, c.comp)
			if base != c.wantBase || overlay != c.wantOverlay || limits != c.wantLimits {
				t.Errorf("composedFrom = (%v, %v, %v), want (%v, %v, %v)", base, overlay, limits, c.wantBase, c.wantOverlay, c.wantLimits)
			}
		})
	}
}

func TestGovCovBuildGovernanceProfile(t *testing.T) {
	s, _ := govCovServer(t, &govCovStore{})
	base := govCovStandalone(govCovBase, "base", "pypi.org", "github.com")
	baseID := govCovBase.String()
	composedBody := func(overlay string) string {
		return `{"name":"child","base_profile_id":"` + baseID + `","overlay":` + overlay + `}`
	}
	for _, c := range []struct {
		name       string
		all        []types.GovernanceProfile
		body       string
		wantStatus int
		wantReason string
		wantMsg    string
	}{
		{"a base with no overlay", nil, `{"name":"c","base_profile_id":"` + baseID + `","overlay":null}`, 400, reasonGovernanceOverlayInvalid, "a base needs an overlay"},
		{"overlay limits with no overlay", nil, `{"name":"c","overlay":null,"overlay_limits":{"max_concurrent_runs":1}}`, 400, reasonGovernanceOverlayInvalid, "overlay_limits: needs an overlay"},
		{"a composed profile that also states a ceiling", []types.GovernanceProfile{base},
			`{"name":"c","ceiling":{"allowed_domains":["pypi.org"],"min_confinement_class":"CC2"},"base_profile_id":"` + baseID + `","overlay":{}}`, 400, reasonGovernanceOverlayInvalid, "ceiling and limits must be empty"},
		{"a composed profile that also states limits", []types.GovernanceProfile{base},
			`{"name":"c","limits":{"max_concurrent_runs":2},"base_profile_id":"` + baseID + `","overlay":{}}`, 400, reasonGovernanceOverlayInvalid, "ceiling and limits must be empty"},
		{"a standalone profile with an empty ceiling", nil, `{"name":"c"}`, 400, reasonGovernanceProfileRequestInvalid, "invalid ceiling"},
		{"a standalone profile with a grant the deployment never provisioned", nil,
			`{"name":"c","ceiling":{"min_confinement_class":"CC2","eligible_grants":[{"kind":"github_token"}]}}`, 400, reasonGovernanceCeilingInvalid, "invalid ceiling"},
		{"a base that does not exist", nil, composedBody(`{}`), 400, reasonGovernanceOverlayInvalid, "base_profile_id: no such profile"},
		{"an overlay naming a domain the base does not permit", []types.GovernanceProfile{base}, composedBody(`{"allowed_domains":["evil.example"]}`), 400, reasonGovernanceOverlayInvalid, ""},
		{"an overlay grant the base does not hold", []types.GovernanceProfile{base}, composedBody(`{"eligible_grants":[{"kind":"github_token"}]}`), 400, reasonGovernanceOverlayInvalid, "overlay.eligible_grants"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ref := govCovAsRefusal(t, govCovBuildErr(t, s, c.all, govCovPID, c.body))
			if ref.status != c.wantStatus || ref.reason != c.wantReason || !strings.Contains(ref.msg, c.wantMsg) {
				t.Errorf("refusal = %+v, want %d %q containing %q", ref, c.wantStatus, c.wantReason, c.wantMsg)
			}
		})
	}

	t.Run("a standalone write stores its own ceiling and limits", func(t *testing.T) {
		p, res := govCovBuilt(t, s, nil, govCovPID, `{"name":"c","ceiling":{"allowed_domains":["pypi.org"],"min_confinement_class":"CC2"},"limits":{"max_concurrent_runs":2}}`)
		if p.Composed() || p.CreatedBy != "alice" || p.Limits.MaxConcurrentRuns != 2 || !reflect.DeepEqual(p.Ceiling.AllowedDomains, []string{"pypi.org"}) {
			t.Errorf("row = %+v", p)
		}
		if res == nil || res.Composed || res.Limits.MaxConcurrentRuns != 2 {
			t.Errorf("resolved = %+v", res)
		}
	})

	t.Run("a composed write stores the composition and no ceiling", func(t *testing.T) {
		p, res := govCovBuilt(t, s, []types.GovernanceProfile{base}, govCovPID, composedBody(`{"allowed_domains":["pypi.org"]}`)+"")
		if !p.Composed() || p.BaseProfileID == nil || *p.BaseProfileID != govCovBase || !reflect.DeepEqual(p.Ceiling, types.RunPolicySpec{}) {
			t.Errorf("row = %+v", p)
		}
		if res == nil || !res.Composed || !reflect.DeepEqual(res.Ceiling.AllowedDomains, []string{"pypi.org"}) {
			t.Errorf("resolved = %+v", res)
		}
	})
}

func TestGovCovCheckGraphWrite(t *testing.T) {
	s, _ := govCovServer(t, &govCovStore{})
	top := govCovStandalone(govCovTop, "top", "pypi.org", "github.com")
	mid := govCovComposedOn(govCovMid, "mid", govCovTop, "pypi.org", "github.com")
	leaf := govCovComposedOn(govCovBase, "leaf", govCovMid, "pypi.org")

	t.Run("a base chain that would loop is a 409 cycle", func(t *testing.T) {
		// top becomes composed on leaf, which is composed on mid, which is composed on top.
		body := `{"name":"top","base_profile_id":"` + govCovBase.String() + `","overlay":{}}`
		ref := govCovAsRefusal(t, govCovBuildErr(t, s, []types.GovernanceProfile{top, mid, leaf}, govCovTop, body))
		if ref.status != http.StatusConflict || ref.reason != reasonGovernanceProfileCycle || !strings.Contains(ref.msg, "its own base") {
			t.Errorf("refusal = %+v", ref)
		}
	})

	t.Run("a chain past three profiles is a 409 depth", func(t *testing.T) {
		body := `{"name":"x","base_profile_id":"` + govCovBase.String() + `","overlay":{}}`
		ref := govCovAsRefusal(t, govCovBuildErr(t, s, []types.GovernanceProfile{top, mid, leaf}, govCovPID, body))
		if ref.status != http.StatusConflict || ref.reason != reasonGovernanceProfileDepth || !strings.Contains(ref.msg, "more than 3 profiles deep") {
			t.Errorf("refusal = %+v", ref)
		}
	})

	t.Run("narrowing a base so its child has nothing left is a 409 naming the child", func(t *testing.T) {
		both := govCovStandalone(govCovTop, "top", "pypi.org")
		both.Ceiling.AllowedMethods = []string{"GET", "POST"}
		post := []string{"POST"}
		child := types.GovernanceProfile{ID: govCovMid, Name: "child", BaseProfileID: &both.ID, Overlay: &types.CeilingOverlay{AllowedMethods: &post}}
		// top now permits GET only: the child's overlay (POST) shares no method with it.
		body := `{"name":"top","ceiling":{"allowed_domains":["pypi.org"],"min_confinement_class":"CC2","allowed_methods":["GET"]}}`
		ref := govCovAsRefusal(t, govCovBuildErr(t, s, []types.GovernanceProfile{both, child}, govCovTop, body))
		if ref.status != http.StatusConflict || ref.reason != reasonGovernanceOverlayUnsatisfiable || !strings.Contains(ref.msg, `profile "child"`) {
			t.Errorf("refusal = %+v", ref)
		}
	})
}

func TestGovCovUnsatisfiableWriteAndAdminResolveError(t *testing.T) {
	plain := errors.New("not an overlay error")
	if got := unsatisfiableWrite(plain, "p"); got != plain {
		t.Errorf("a non-overlay error must pass through unchanged, got %v", got)
	}
	u := &overlayUnsatisfiableError{Leaf: "p", Detail: "allowed_domains: empty after the meet"}
	ref := govCovAsRefusal(t, unsatisfiableWrite(u, "p"))
	if ref.status != http.StatusConflict || ref.reason != reasonGovernanceOverlayUnsatisfiable || ref.msg != `profile "p" cannot be applied: allowed_domains: empty after the meet` {
		t.Errorf("refusal = %+v", ref)
	}
	if got := adminResolveError(u); got != u.Detail {
		t.Errorf("an admin sees the detail, got %q", got)
	}
	if got := adminResolveError(plain); got != plain.Error() {
		t.Errorf("a plain error is shown as is, got %q", got)
	}
	if got := (&profileWriteError{reason: "r", msg: "m"}).Error(); got != "r: m" {
		t.Errorf("profileWriteError.Error() = %q", got)
	}
}

func TestGovCovNewProfileView(t *testing.T) {
	row := govCovStandalone(govCovPID, "p", "pypi.org")
	row.Ceiling.LLMInspection = &types.LLMInspectionSpec{WorkspaceSecretValues: []string{"stored-secret-value"}}
	res := &ResolvedProfile{Ceiling: types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}}, Limits: types.GovernanceLimits{MaxConcurrentRuns: 3}, AdminWarnings: []string{"w"}}

	v := newProfileView(row, res, nil)
	if raw, _ := json.Marshal(v); strings.Contains(string(raw), "stored-secret-value") {
		t.Errorf("the view carries the stored secret value: %s", raw)
	}
	if v.Effective.Limits.MaxConcurrentRuns != 3 || !reflect.DeepEqual(v.Effective.Warnings, []string{"w"}) || v.Effective.Error != "" {
		t.Errorf("effective = %+v", v.Effective)
	}

	u := &overlayUnsatisfiableError{Leaf: "p", Detail: "base detail"}
	v = newProfileView(row, nil, u)
	if v.Effective.Error != "base detail" || len(v.Effective.Ceiling.AllowedDomains) != 0 {
		t.Errorf("effective on an unsatisfiable chain = %+v, want the admin detail and no content", v.Effective)
	}
	if v = newProfileView(row, nil, nil); !reflect.DeepEqual(v.Effective, governanceEffective{}) {
		t.Errorf("effective with neither a resolution nor an error = %+v", v.Effective)
	}
}

func TestGovCovDescendantsOf(t *testing.T) {
	top := govCovStandalone(govCovTop, "top", "pypi.org")
	zed := govCovComposedOn(govCovMid, "zed", govCovTop, "pypi.org")
	amy := govCovComposedOn(govCovBase, "amy", govCovTop, "pypi.org")
	grand := govCovComposedOn(govCovPID, "grand", govCovMid, "pypi.org")
	unrelated := govCovStandalone(uuid.MustParse("dddddddd-0000-0000-0000-0000000000aa"), "unrelated", "pypi.org")

	names := func(rows []types.GovernanceProfile) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Name)
		}
		return out
	}
	got := descendantsOf([]types.GovernanceProfile{top, zed, amy, grand, unrelated}, govCovTop)
	if !reflect.DeepEqual(names(got), []string{"amy", "grand", "zed"}) {
		t.Errorf("descendants = %v, want every profile composed on top, by name, and not the unrelated one", names(got))
	}
	if got := descendantsOf([]types.GovernanceProfile{top, unrelated}, govCovTop); len(got) != 0 {
		t.Errorf("descendants of a leaf = %v", names(got))
	}

	// A seeded cycle ends the walk instead of looping, and a base the list lacks ends it too.
	a := govCovComposedOn(govCovMid, "a", govCovBase, "pypi.org")
	b := govCovComposedOn(govCovBase, "b", govCovMid, "pypi.org")
	if got := descendantsOf([]types.GovernanceProfile{a, b}, govCovTop); len(got) != 0 {
		t.Errorf("a cycle that never reaches the profile produced %v", names(got))
	}
	orphan := govCovComposedOn(govCovPID, "orphan", uuid.MustParse("dddddddd-0000-0000-0000-0000000000bb"), "pypi.org")
	if got := descendantsOf([]types.GovernanceProfile{top, orphan}, govCovTop); len(got) != 0 {
		t.Errorf("a profile whose base is missing produced %v", names(got))
	}
}

func TestGovCovOverlayFieldNames(t *testing.T) {
	domains := []string{"pypi.org"}
	n := 2
	p := types.GovernanceProfile{
		Overlay:       &types.CeilingOverlay{AllowedDomains: &domains},
		OverlayLimits: &types.LimitsOverlay{MaxConcurrentRuns: &n},
	}
	if got := overlayFieldNames(p); !reflect.DeepEqual(got, []string{"allowed_domains", "max_concurrent_runs"}) {
		t.Errorf("names = %v, want the member names sorted and no values", got)
	}
	if got := overlayFieldNames(types.GovernanceProfile{}); got == nil || len(got) != 0 {
		t.Errorf("a standalone profile has no overlay fields, got %#v", got)
	}
	if got := overlayFieldNames(types.GovernanceProfile{Overlay: &types.CeilingOverlay{}}); len(got) != 0 {
		t.Errorf("an empty overlay names nothing, got %v", got)
	}
	if profileJSONKeys(make(chan int)) != nil {
		t.Error("a value that cannot be encoded has no keys")
	}
	if profileJSONKeys([]string{"a"}) != nil {
		t.Error("a value that is not an object has no keys")
	}
}

// govCovProfilePut runs PUT /governance/profiles/{id} as the admin token against st.
func govCovProfilePut(t *testing.T, s *Server, body string) (code int, reason string, resp string) {
	t.Helper()
	w := do(t, s, http.MethodPut, "/api/v1/governance/profiles/"+govCovPID.String(), adminToken, body)
	return w.Code, errorReason(w), w.Body.String()
}

func TestGovCovProfileWriteDoorRefusals(t *testing.T) {
	good := govCovProfileBody("p", "pypi.org")
	for _, c := range []struct {
		name       string
		body       string
		st         *govCovStore
		wantCode   int
		wantReason string
	}{
		{"a composition member that does not parse", `{"name":"p","base_profile_id":3,"overlay":{}}`, &govCovStore{}, 400, reasonGovernanceOverlayInvalid},
		{"a composition the build refuses", `{"name":"p","base_profile_id":"` + govCovBase.String() + `","overlay":{}}`, &govCovStore{}, 400, reasonGovernanceOverlayInvalid},
		{"a name another profile holds", good, &govCovStore{writeErr: store.ErrConflict}, 409, reasonGovernanceProfileNameConflict},
		{"a store failure", good, &govCovStore{writeErr: errors.New("pg: down")}, 500, reasonInternalError},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, h := govCovServer(t, c.st)
			code, reason, resp := govCovProfilePut(t, s, c.body)
			if code != c.wantCode || reason != c.wantReason {
				t.Fatalf("PUT = %d %q (%s), want %d %q", code, reason, resp, c.wantCode, c.wantReason)
			}
			if strings.Contains(resp, "pg: down") {
				t.Errorf("the 500 leaked the driver error: %s", resp)
			}
			if len(govCovAuditActions(h)) != 0 {
				t.Errorf("a refused write audited %v", govCovAuditActions(h))
			}
		})
	}

	t.Run("a name conflict names the profile", func(t *testing.T) {
		s, _ := govCovServer(t, &govCovStore{writeErr: store.ErrConflict})
		_, _, resp := govCovProfilePut(t, s, good)
		if !strings.Contains(resp, `a governance profile named \"p\" already exists`) {
			t.Errorf("body = %s", resp)
		}
	})

	t.Run("local mode with the switch on refuses before decoding the body", func(t *testing.T) {
		t.Setenv(envGovernanceSecondHuman, "true")
		s, _ := govCovServer(t, &govCovStore{}, func(c *Config) { c.LocalMode = true })
		w := do(t, s, http.MethodPut, "/api/v1/governance/profiles/"+govCovPID.String(), adminToken, `not json`)
		if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonGovernanceSecondHumanLocalMode {
			t.Errorf("= %d %q", w.Code, errorReason(w))
		}
	})
}

func TestGovCovProfileWriteDoorAnswersWithWarningsAndAudit(t *testing.T) {
	s, h := govCovServer(t, &govCovStore{})
	code, _, resp := govCovProfilePut(t, s, govCovProfileBody("p", "pypi.org"))
	if code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, resp)
	}
	var out governanceProfileResponse
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		t.Fatal(err)
	}
	if out.Profile.ID != govCovPID || out.Profile.Name != "p" || !reflect.DeepEqual(out.Profile.Effective.Ceiling.AllowedDomains, []string{"pypi.org"}) {
		t.Errorf("profile = %+v", out.Profile)
	}
	// The deployment default also allows api.anthropic.com and github.com; a standalone profile that
	// omits them says so.
	if len(out.Warnings) == 0 || !strings.Contains(strings.Join(out.Warnings, " "), "omits") {
		t.Errorf("warnings = %v, want the omission warning", out.Warnings)
	}
	if got := govCovAuditActions(h); !reflect.DeepEqual(got, []string{"governance.profile.write"}) {
		t.Fatalf("audit rows = %v, want the write alone with the switch off", got)
	}
	d := govCovAuditData(t, govCovAudits(h, "governance.profile.write")[0])
	if d["name"] != "p" || d["min_confinement_class"] != "CC2" {
		t.Errorf("audit data = %v", d)
	}
}

func TestGovCovProfileDeleteDoor(t *testing.T) {
	path := "/api/v1/governance/profiles/" + govCovPID.String()
	for _, c := range []struct {
		name       string
		err        error
		wantCode   int
		wantReason string
		wantBody   string
	}{
		{"a missing profile", store.ErrNotFound, 404, reasonGovernanceProfileNotFoundByID, ""},
		{"a profile that is a base", &store.ErrProfileHasChildren{Names: []string{"a", "b"}}, 409, reasonGovernanceProfileInUse, "a, b"},
		{"a profile still assigned", store.ErrConflict, 409, reasonGovernanceProfileInUse, "still assigned"},
		{"a store failure", errors.New("pg: down"), 500, reasonInternalError, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &govCovStore{deleteErr: c.err}
			s, h := govCovServer(t, st)
			w := do(t, s, http.MethodDelete, path, adminToken, "")
			if w.Code != c.wantCode || errorReason(w) != c.wantReason || !strings.Contains(w.Body.String(), c.wantBody) {
				t.Errorf("DELETE = %d %q (%s)", w.Code, errorReason(w), w.Body.String())
			}
			if len(govCovAuditActions(h)) != 0 {
				t.Errorf("a refused delete audited %v", govCovAuditActions(h))
			}
		})
	}

	t.Run("the admin token deletes directly and records the break-glass row", func(t *testing.T) {
		t.Setenv(envGovernanceSecondHuman, "true")
		st := &govCovStore{}
		s, h := govCovServer(t, st)
		w := do(t, s, http.MethodDelete, path, adminToken, "")
		if w.Code != http.StatusNoContent || !reflect.DeepEqual(st.deleted, []uuid.UUID{govCovPID}) {
			t.Fatalf("DELETE = %d, deleted %v", w.Code, st.deleted)
		}
		if got := govCovAuditActions(h); !reflect.DeepEqual(got, []string{"governance.profile.delete", "governance.change.bypass"}) {
			t.Errorf("audit rows = %v", got)
		}
	})

	t.Run("local mode with the switch on refuses and deletes nothing", func(t *testing.T) {
		t.Setenv(envGovernanceSecondHuman, "true")
		st := &govCovStore{}
		s, _ := govCovServer(t, st, func(c *Config) { c.LocalMode = true })
		w := do(t, s, http.MethodDelete, path, adminToken, "")
		if w.Code != http.StatusServiceUnavailable || errorReason(w) != reasonGovernanceSecondHumanLocalMode || len(st.deleted) != 0 {
			t.Errorf("= %d %q, deleted %v", w.Code, errorReason(w), st.deleted)
		}
	})
}
