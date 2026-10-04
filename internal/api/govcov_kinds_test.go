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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var (
	govCovPID  = uuid.MustParse("dddddddd-0000-0000-0000-000000000001")
	govCovBase = uuid.MustParse("dddddddd-0000-0000-0000-000000000002")
	govCovMid  = uuid.MustParse("dddddddd-0000-0000-0000-000000000003")
	govCovTop  = uuid.MustParse("dddddddd-0000-0000-0000-000000000004")
)

func govCovStandalone(id uuid.UUID, name string, domains ...string) types.GovernanceProfile {
	return types.GovernanceProfile{ID: id, Name: name, Ceiling: types.RunPolicySpec{AllowedDomains: domains, MinConfinementClass: types.CC2}}
}

func govCovComposedOn(id uuid.UUID, name string, base uuid.UUID, domains ...string) types.GovernanceProfile {
	return types.GovernanceProfile{ID: id, Name: name, BaseProfileID: &base, Overlay: &types.CeilingOverlay{AllowedDomains: &domains}}
}

func govCovProfileBody(name string, domains ...string) string {
	b, _ := json.Marshal(map[string]any{"name": name, "ceiling": map[string]any{"allowed_domains": domains, "min_confinement_class": "CC2"}})
	return string(b)
}

// govCovBuilt runs the real build over all for a request body, as the write door does.
func govCovBuilt(t *testing.T, s *Server, all []types.GovernanceProfile, id uuid.UUID, body string) (types.GovernanceProfile, *ResolvedProfile) {
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
		t.Fatalf("fixture body refused: %s", msg)
	}
	p, res, err := s.buildGovernanceProfile("alice", id, req, all)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return p, res
}

func TestGovCovSameJSON(t *testing.T) {
	for _, c := range []struct {
		name string
		a, b any
		want bool
	}{
		{"the same map in another key order", map[string]any{"a": 1, "b": 2}, map[string]any{"b": 2, "a": 1}, true},
		{"different values", map[string]any{"a": 1}, map[string]any{"a": 2}, false},
		{"a struct and the map of its JSON form", types.GovernanceLimits{MaxConcurrentRuns: 2}, map[string]any{"max_concurrent_runs": 2}, true},
		{"two nils", nil, nil, true},
		{"nil and empty are different documents", nil, []string{}, false},
		{"two values that cannot be encoded are never the same", make(chan int), make(chan int), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := sameJSON(c.a, c.b); got != c.want {
				t.Errorf("sameJSON = %v, want %v", got, c.want)
			}
		})
	}
}

func TestGovCovProfilePayloadKeepsAbsentAndNullApart(t *testing.T) {
	absent := governanceProfileRequest{Name: "p", Ceiling: types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}, MinConfinementClass: types.CC2}}
	raw, err := json.Marshal(profilePayloadOf(absent))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"contact"`, `"base_profile_id"`, `"overlay"`, `"overlay_limits"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("an absent member %s was written into the payload: %s", key, raw)
		}
	}
	back, msg := profileRequestFromPayload(raw)
	if msg != "" || back.contactSet || back.comp.baseSet || back.comp.overlaySet || back.comp.limitsSet {
		t.Fatalf("replaying an absent-member payload = %q, set flags contact=%v comp=%+v; absent must keep the stored value", msg, back.contactSet, back.comp)
	}

	nulled := absent
	nulled.Contact, nulled.BaseProfileID, nulled.Overlay, nulled.OverlayLimits = json.RawMessage(`null`), json.RawMessage(`null`), json.RawMessage(`null`), json.RawMessage(`null`)
	raw, err = json.Marshal(profilePayloadOf(nulled))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"contact":null`, `"base_profile_id":null`, `"overlay":null`, `"overlay_limits":null`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("an explicit null %s was lost from the payload: %s", key, raw)
		}
	}
	back, msg = profileRequestFromPayload(raw)
	if msg != "" || !back.contactSet || back.contact != nil || !back.comp.baseSet || back.comp.base != nil || !back.comp.overlaySet || back.comp.overlay != nil || !back.comp.limitsSet || back.comp.overlayLimits != nil {
		t.Errorf("replaying nulls = %q contact set=%v comp=%+v; null must clear", msg, back.contactSet, back.comp)
	}
}

func TestGovCovProfileRequestFromPayload(t *testing.T) {
	baseID := govCovBase.String()
	for _, c := range []struct {
		name    string
		payload string
		wantMsg string
	}{
		{"not json", `{"name":`, "invalid JSON body"},
		{"an unknown field", `{"name":"p","surprise":1}`, "invalid JSON body"},
		{"a blank name", `{"name":"  "}`, "name is required"},
		{"an invalid name", `{"name":"a\u0007b"}`, "name is invalid"},
		{"an invalid ceiling", `{"name":"p","ceiling":{"allowed_domains":["not a domain!"],"min_confinement_class":"CC2"}}`, "invalid ceiling"},
		{"negative limits", `{"name":"p","limits":{"max_concurrent_runs":-1}}`, "limits.max_concurrent_runs"},
		{"an invalid contact", `{"name":"p","contact":{"email":"not-an-email"}}`, "email"},
		{"a base that is not a string", `{"name":"p","base_profile_id":5,"overlay":{}}`, "base_profile_id: must be a profile id or null"},
		{"a base that is not a profile id", `{"name":"p","base_profile_id":"nope","overlay":{}}`, "base_profile_id: not a profile id"},
		{"the nil uuid as a base", `{"name":"p","base_profile_id":"00000000-0000-0000-0000-000000000000","overlay":{}}`, "base_profile_id: not a profile id"},
		{"an overlay with an unknown key", `{"name":"p","base_profile_id":"` + baseID + `","overlay":{"nope":1}}`, "invalid overlay"},
		{"overlay limits with an unknown key", `{"name":"p","base_profile_id":"` + baseID + `","overlay":{},"overlay_limits":{"nope":1}}`, "invalid overlay_limits"},
		{"a valid composed write", `{"name":" p ","base_profile_id":"` + baseID + `","overlay":{"allowed_domains":["pypi.org"]},"overlay_limits":{"max_concurrent_runs":2}}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, msg := profileRequestFromPayload(json.RawMessage(c.payload))
			if c.wantMsg != "" {
				if !strings.Contains(msg, c.wantMsg) {
					t.Fatalf("message = %q, want it to contain %q", msg, c.wantMsg)
				}
				if !reflect.DeepEqual(req.Ceiling, types.RunPolicySpec{}) || req.Name != "" {
					t.Errorf("a refused payload still returned a request: %+v", req)
				}
				return
			}
			if msg != "" {
				t.Fatalf("message = %q, want it to validate", msg)
			}
			if req.Name != "p" || req.comp.base == nil || *req.comp.base != govCovBase || req.comp.overlay == nil ||
				req.comp.overlay.AllowedDomains == nil || req.comp.overlayLimits == nil || *req.comp.overlayLimits.MaxConcurrentRuns != 2 {
				t.Errorf("replayed request = name %q comp %+v", req.Name, req.comp)
			}
		})
	}
}

func TestGovCovContactHelpers(t *testing.T) {
	owner := &policyref.Contact{Owner: "platform"}
	other := &policyref.Contact{Owner: "security"}
	if got := nonZeroContact(nil); got != nil {
		t.Errorf("nonZeroContact(nil) = %v", got)
	}
	if got := nonZeroContact(&policyref.Contact{}); got != nil {
		t.Errorf("an empty contact is stored as none, got %v", got)
	}
	if got := nonZeroContact(owner); got != owner {
		t.Errorf("nonZeroContact(owner) = %v", got)
	}
	cur := types.GovernanceProfile{Contact: owner}
	if got := profileContactAfter(cur, types.GovernanceProfile{Contact: other, ContactSet: true}); got != other {
		t.Errorf("a set contact replaces the stored one, got %v", got)
	}
	if got := profileContactAfter(cur, types.GovernanceProfile{ContactSet: true}); got != nil {
		t.Errorf("a set nil contact clears it, got %v", got)
	}
	if got := profileContactAfter(cur, types.GovernanceProfile{Contact: other}); got != owner {
		t.Errorf("an unset contact keeps the stored one, got %v", got)
	}
}

func TestGovCovRedactOverlayForRead(t *testing.T) {
	if redactOverlayForRead(nil) != nil {
		t.Error("nil overlay must stay nil")
	}
	plain := &types.CeilingOverlay{}
	if redactOverlayForRead(plain) != plain {
		t.Error("an overlay with no inspection block is returned as is")
	}
	empty := &types.CeilingOverlay{LLMInspection: &types.LLMInspectionSpec{}}
	if redactOverlayForRead(empty) != empty {
		t.Error("an inspection block with no secret values is returned as is")
	}
	secret := &types.CeilingOverlay{LLMInspection: &types.LLMInspectionSpec{WorkspaceSecretValues: []string{"hunter2", "hunter3"}}}
	got := redactOverlayForRead(secret)
	if !reflect.DeepEqual(got.LLMInspection.WorkspaceSecretValues, []string{"<2 value(s) redacted>"}) {
		t.Errorf("redacted values = %v", got.LLMInspection.WorkspaceSecretValues)
	}
	if !reflect.DeepEqual(secret.LLMInspection.WorkspaceSecretValues, []string{"hunter2", "hunter3"}) {
		t.Errorf("redaction mutated the stored overlay: %v", secret.LLMInspection.WorkspaceSecretValues)
	}
}

func TestGovCovNewProfileDiffViewNeverCarriesASecretValue(t *testing.T) {
	row := types.GovernanceProfile{
		Name: "p",
		Ceiling: types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			LLMInspection:       &types.LLMInspectionSpec{WorkspaceSecretValues: []string{"ceiling-secret-value"}},
		},
		Contact: &policyref.Contact{},
		Overlay: &types.CeilingOverlay{LLMInspection: &types.LLMInspectionSpec{WorkspaceSecretValues: []string{"overlay-secret-value"}}},
	}
	res := &ResolvedProfile{
		Ceiling:       types.RunPolicySpec{LLMInspection: &types.LLMInspectionSpec{WorkspaceSecretValues: []string{"effective-secret-value"}}},
		Limits:        types.GovernanceLimits{},
		AdminWarnings: []string{"warned"},
	}
	v := newProfileDiffView(row, res)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"ceiling-secret-value", "overlay-secret-value", "effective-secret-value"} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("the reviewer's view carries %q: %s", leaked, raw)
		}
	}
	if v.Contact != nil {
		t.Errorf("an empty contact must be omitted, got %+v", v.Contact)
	}
	if v.Effective == nil || !reflect.DeepEqual(v.Effective.Warnings, []string{"warned"}) {
		t.Errorf("effective = %+v, want the composed view with its warnings", v.Effective)
	}
	if got := newProfileDiffView(row, nil); got.Effective != nil {
		t.Errorf("a profile that does not compose has no effective view, got %+v", got.Effective)
	}
}

func TestGovCovResolveForDiff(t *testing.T) {
	s, _ := govCovServer(t, &govCovStore{})
	base := govCovStandalone(govCovBase, "base", "pypi.org", "github.com")
	child := govCovComposedOn(govCovPID, "child", govCovBase, "pypi.org")
	if res := s.resolveForDiff([]types.GovernanceProfile{base, child}, govCovPID); res == nil || res.ID != govCovPID || !res.Composed {
		t.Errorf("a composable profile resolved to %+v", res)
	}
	if res := s.resolveForDiff([]types.GovernanceProfile{base}, govCovPID); res != nil {
		t.Errorf("an absent profile resolved to %+v", res)
	}
	a := govCovComposedOn(govCovPID, "a", govCovBase, "pypi.org")
	b := govCovComposedOn(govCovBase, "b", govCovPID, "pypi.org")
	if res := s.resolveForDiff([]types.GovernanceProfile{a, b}, govCovPID); res != nil {
		t.Errorf("a cycle resolved to %+v", res)
	}
}

func TestGovCovProfileBaseState(t *testing.T) {
	top := govCovStandalone(govCovTop, "top", "pypi.org")
	mid := govCovComposedOn(govCovMid, "mid", govCovTop, "pypi.org")
	leaf := govCovComposedOn(govCovPID, "leaf", govCovMid, "pypi.org")
	all := []types.GovernanceProfile{top, mid, leaf}

	state := profileBaseState(all, govCovPID, leaf.BaseProfileID).(map[string]any)
	if state["self"].(types.GovernanceProfile).ID != govCovPID {
		t.Errorf("self = %+v, want the row itself", state["self"])
	}
	bases := state["bases"].([]types.GovernanceProfile)
	if len(bases) != 2 || bases[0].ID != govCovMid || bases[1].ID != govCovTop {
		t.Errorf("bases = %+v, want mid then top", bases)
	}

	state = profileBaseState(all, uuid.MustParse("dddddddd-0000-0000-0000-0000000000ff"), &govCovMid).(map[string]any)
	if state["self"] != nil || len(state["bases"].([]types.GovernanceProfile)) != 2 {
		t.Errorf("an absent row = self %v bases %d, want nil self and the chain it would compose on", state["self"], len(state["bases"].([]types.GovernanceProfile)))
	}

	missing := uuid.MustParse("dddddddd-0000-0000-0000-0000000000fe")
	state = profileBaseState(all, govCovPID, &missing).(map[string]any)
	if got := state["bases"].([]types.GovernanceProfile); got == nil || len(got) != 0 {
		t.Errorf("a base that does not exist ends the chain with an empty, non-nil list, got %#v", got)
	}

	cyc := govCovComposedOn(govCovTop, "top", govCovPID, "pypi.org")
	state = profileBaseState([]types.GovernanceProfile{cyc, mid, leaf}, govCovPID, leaf.BaseProfileID).(map[string]any)
	if got := state["bases"].([]types.GovernanceProfile); len(got) != 2 {
		t.Errorf("a cycle must end the walk at the row itself: %d bases", len(got))
	}

	// The hash a proposal stores moves when a base changes, which is what makes an approval stale.
	changed := append([]types.GovernanceProfile(nil), all...)
	changed[0].Name = "renamed"
	if computeETag(profileBaseState(all, govCovPID, leaf.BaseProfileID)) == computeETag(profileBaseState(changed, govCovPID, leaf.BaseProfileID)) {
		t.Error("renaming a base left the base state unchanged")
	}
}

func TestGovCovProfileWriteExempt(t *testing.T) {
	s, _ := govCovServer(t, &govCovStore{})
	cur := govCovStandalone(govCovPID, "p", "pypi.org", "github.com")
	cur.Contact = &policyref.Contact{Owner: "platform"}
	all := []types.GovernanceProfile{cur}
	for _, c := range []struct {
		name string
		body string
		all  []types.GovernanceProfile
		want bool
	}{
		{"a metadata-only rename applies", `{"name":"renamed","ceiling":{"allowed_domains":["pypi.org","github.com"],"min_confinement_class":"CC2"}}`, all, true},
		{"a narrowing write applies", govCovProfileBody("p", "pypi.org"), all, true},
		{"a widening write is held", govCovProfileBody("p", "pypi.org", "github.com", "api.anthropic.com"), all, false},
		{"a contact change is held even when the ceiling narrows", `{"name":"p","contact":{"owner":"elsewhere"},"ceiling":{"allowed_domains":["pypi.org"],"min_confinement_class":"CC2"}}`, all, false},
		{"clearing the contact is held", `{"name":"p","contact":null,"ceiling":{"allowed_domains":["pypi.org"],"min_confinement_class":"CC2"}}`, all, false},
		{"a create is never exempt", govCovProfileBody("p", "pypi.org"), nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, res := govCovBuilt(t, s, c.all, govCovPID, c.body)
			if got := s.profileWriteExempt(c.all, p, res); got != c.want {
				t.Errorf("profileWriteExempt = %v, want %v", got, c.want)
			}
		})
	}
	p, _ := govCovBuilt(t, s, all, govCovPID, govCovProfileBody("p", "pypi.org"))
	if s.profileWriteExempt(all, p, nil) {
		t.Error("a write that did not resolve can never be exempt")
	}
}

// govCovHoldAnswer is the 202 a held write answers and the one change it stored.
func govCovHoldAnswer(t *testing.T, w *httptest.ResponseRecorder, st *govCovStore) types.GovernanceChange {
	t.Helper()
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d %s, want 202", w.Code, w.Body.String())
	}
	if len(st.proposed) != 1 {
		t.Fatalf("%d proposals stored, want 1", len(st.proposed))
	}
	return st.proposed[0]
}

func govCovDiff(t *testing.T, ch types.GovernanceChange) governanceDiffBody {
	t.Helper()
	var d governanceDiffBody
	if err := json.Unmarshal(ch.Diff, &d); err != nil {
		t.Fatalf("diff %s: %v", ch.Diff, err)
	}
	return d
}

func TestGovCovHoldProfileWriteStoresTheProposal(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	super := govCovSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin)
	path := "/api/v1/governance/profiles/" + govCovPID.String()
	saved := types.GovernanceChange{ID: govCovChangeID, State: types.GovernanceChangePending}

	t.Run("a create is held with only an after view", func(t *testing.T) {
		st := &govCovStore{proposeSaved: saved}
		s, h := govCovServer(t, st)
		w := doSSO(t, s, http.MethodPut, path, super, govCovProfileBody("fresh", "pypi.org"))
		ch := govCovHoldAnswer(t, w, st)
		if ch.TargetKind != govKindProfile || ch.Op != "create" || ch.TargetKey != govCovPID.String() || ch.ProposedBy != "sub-super" {
			t.Errorf("stored change = %+v", ch)
		}
		if ch.BaseHash != computeETag(profileBaseState(nil, govCovPID, nil)) {
			t.Errorf("base hash = %s, want the empty graph's", ch.BaseHash)
		}
		d := govCovDiff(t, ch)
		if d.Before != nil || d.After == nil || !reflect.DeepEqual(d.Changed, changedPaths(nil, profileDiffStored{Name: "fresh", Ceiling: types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}, MinConfinementClass: types.CC2}})) {
			t.Errorf("diff = %s", ch.Diff)
		}
		if _, err := govCovReplayPayload(ch); err != nil {
			t.Errorf("the stored payload does not replay: %v", err)
		}
		if got := govCovAuditActions(h); !reflect.DeepEqual(got, []string{"governance.change.propose"}) {
			t.Errorf("audit rows = %v, want only the proposal (nothing was written)", got)
		}
	})

	t.Run("a widening update is held with before and after", func(t *testing.T) {
		cur := govCovStandalone(govCovPID, "p", "pypi.org")
		cur.Contact = &policyref.Contact{Owner: "platform"}
		st := &govCovStore{proposeSaved: saved, profiles: []types.GovernanceProfile{cur}}
		s, _ := govCovServer(t, st)
		w := doSSO(t, s, http.MethodPut, path, super, govCovProfileBody("p", "pypi.org", "github.com"))
		ch := govCovHoldAnswer(t, w, st)
		if ch.Op != "update" {
			t.Errorf("op = %q, want update", ch.Op)
		}
		d := govCovDiff(t, ch)
		if d.Before == nil || !reflect.DeepEqual(d.Changed, []string{"ceiling.allowed_domains"}) {
			t.Errorf("changed = %v, want only the stored ceiling.allowed_domains (the contact is untouched)", d.Changed)
		}
		if !strings.Contains(string(d.After), `"owner":"platform"`) {
			t.Errorf("the after view dropped the contact the update did not touch: %s", d.After)
		}
	})

	t.Run("a narrowing update applies directly and holds nothing", func(t *testing.T) {
		cur := govCovStandalone(govCovPID, "p", "pypi.org", "github.com")
		st := &govCovStore{profiles: []types.GovernanceProfile{cur}}
		s, h := govCovServer(t, st)
		w := doSSO(t, s, http.MethodPut, path, super, govCovProfileBody("p", "pypi.org"))
		if w.Code != http.StatusOK || len(st.proposed) != 0 {
			t.Fatalf("narrowing = %d, proposals %d, want a direct 200", w.Code, len(st.proposed))
		}
		if got := govCovAuditActions(h); !reflect.DeepEqual(got, []string{"governance.profile.write"}) {
			t.Errorf("audit rows = %v", got)
		}
	})

	t.Run("the admin token writes directly and records the break-glass row", func(t *testing.T) {
		st := &govCovStore{}
		s, h := govCovServer(t, st)
		w := do(t, s, http.MethodPut, path, adminToken, govCovProfileBody("p", "pypi.org"))
		if w.Code != http.StatusOK || len(st.proposed) != 0 {
			t.Fatalf("admin token write = %d, proposals %d", w.Code, len(st.proposed))
		}
		if got := govCovAuditActions(h); !reflect.DeepEqual(got, []string{"governance.profile.write", "governance.change.bypass"}) {
			t.Errorf("audit rows = %v", got)
		}
	})
}

// govCovReplayPayload decodes a stored profile payload the way an approval does.
func govCovReplayPayload(ch types.GovernanceChange) (governanceProfileRequest, error) {
	req, msg := profileRequestFromPayload(ch.Payload)
	if msg != "" {
		return req, errors.New(msg)
	}
	return req, nil
}

func TestGovCovHoldProfileDelete(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	super := govCovSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin)
	path := "/api/v1/governance/profiles/" + govCovPID.String()
	saved := types.GovernanceChange{ID: govCovChangeID}

	t.Run("a delete is held as a delete of the id, and never reaches the direct delete (the dry run is skipped, so the before view is of the empty graph)", func(t *testing.T) {
		st := &govCovStore{proposeSaved: saved}
		s, _ := govCovServer(t, st)
		w := doSSO(t, s, http.MethodDelete, path, super, "")
		ch := govCovHoldAnswer(t, w, st)
		if ch.Op != "delete" || ch.TargetKey != govCovPID.String() || string(ch.Payload) != `{"id":"`+govCovPID.String()+`"}` {
			t.Errorf("stored change = %+v payload %s", ch, ch.Payload)
		}
		if d := govCovDiff(t, ch); d.After != nil {
			t.Errorf("diff = %s, a delete carries no after view", ch.Diff)
		}
		if len(st.deleted) != 0 {
			t.Errorf("a held delete reached the direct delete: %v", st.deleted)
		}
	})

	for _, c := range []struct {
		name       string
		dryRun     error
		wantCode   int
		wantReason string
		wantBody   string
	}{
		{"an unknown profile is the direct delete's own 404", store.ErrNotFound, http.StatusNotFound, reasonGovernanceProfileNotFoundByID, ""},
		{"a profile with children names them", &store.ErrProfileHasChildren{Names: []string{"b-child", "a-child"}}, http.StatusConflict, reasonGovernanceProfileInUse, "b-child, a-child"},
		{"an assigned profile", store.ErrConflict, http.StatusConflict, reasonGovernanceProfileInUse, "still assigned"},
		{"a store failure", errors.New("pg: down"), http.StatusInternalServerError, reasonInternalError, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &govCovStore{dryRunFn: func(func(store.Querier) error) error { return c.dryRun }}
			s, _ := govCovServer(t, st)
			w := doSSO(t, s, http.MethodDelete, path, super, "")
			if w.Code != c.wantCode || errorReason(w) != c.wantReason || !strings.Contains(w.Body.String(), c.wantBody) {
				t.Errorf("delete = %d %q (%s), want %d %q containing %q", w.Code, errorReason(w), w.Body.String(), c.wantCode, c.wantReason, c.wantBody)
			}
			if len(st.proposed) != 0 {
				t.Errorf("a refused delete was proposed: %+v", st.proposed)
			}
		})
	}

	t.Run("the dry run stops at a failed graph lock and issues nothing after it", func(t *testing.T) {
		q := &govCovQuerier{execFn: func(string) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, errors.New("pg: lock timeout") }}
		st := &govCovStore{dryRunFn: func(fn func(store.Querier) error) error { return fn(q) }}
		s, _ := govCovServer(t, st)
		w := doSSO(t, s, http.MethodDelete, path, super, "")
		if w.Code != http.StatusInternalServerError || errorReason(w) != reasonInternalError || len(q.seen) != 1 || len(st.proposed) != 0 {
			t.Errorf("delete = %d after %d statements, proposals %d; want a 500 after the lock alone", w.Code, len(q.seen), len(st.proposed))
		}
	})

	t.Run("a failed profile list is a 500 and no delete is issued", func(t *testing.T) {
		q := &govCovQuerier{queryErr: errors.New("pg: down")}
		st := &govCovStore{dryRunFn: func(fn func(store.Querier) error) error { return fn(q) }}
		s, _ := govCovServer(t, st)
		w := doSSO(t, s, http.MethodDelete, path, super, "")
		if w.Code != http.StatusInternalServerError || errorReason(w) != reasonInternalError || len(q.writes()) != 0 || len(st.proposed) != 0 {
			t.Errorf("delete = %d, writes %v, proposals %d", w.Code, q.writes(), len(st.proposed))
		}
	})
}

func TestGovCovAssignmentKey(t *testing.T) {
	for _, subject := range []string{"alice@corp.example", "", "has:colon:inside", "a b"} {
		key := assignmentKey(types.CapabilitySubjectUser, subject)
		st, sub, ok := splitAssignmentKey(key)
		if !ok || st != "user" || sub != subject {
			t.Errorf("key %q split into (%q, %q, %v), want (user, %q, true)", key, st, sub, ok, subject)
		}
	}
	if _, _, ok := splitAssignmentKey("nocolon"); ok {
		t.Error("a key with no separator must not split")
	}
}

func TestGovCovAssignmentDiffViewAndState(t *testing.T) {
	s, _ := govCovServer(t, &govCovStore{})
	a := types.GovernanceAssignment{SubjectType: types.CapabilitySubjectUser, Subject: "u@corp.example", ProfileID: govCovPID, Priority: 4}

	if v := s.newAssignmentDiffView(a, nil); v.Profile != nil || v.Priority != 4 || v.Subject != "u@corp.example" || v.ProfileID != govCovPID {
		t.Errorf("an assignment with no chain = %+v, want the assignment fields and no profile", v)
	}

	standalone := govCovStandalone(govCovPID, "p", "pypi.org")
	standalone.Ceiling.LLMInspection = &types.LLMInspectionSpec{WorkspaceSecretValues: []string{"s3cret-value"}}
	v := s.newAssignmentDiffView(a, []types.GovernanceProfile{standalone})
	if v.Profile == nil || v.Profile.Name != "p" || v.Profile.Error != "" || v.Profile.ID != govCovPID {
		t.Fatalf("profile view = %+v", v.Profile)
	}
	if raw, _ := json.Marshal(v); strings.Contains(string(raw), "s3cret-value") {
		t.Errorf("the assignment view carries a secret value: %s", raw)
	}

	// A chain that cannot compose still names the profile, with its stored ceiling and the reason.
	broken := govCovComposedOn(govCovPID, "p", govCovBase, "pypi.org")
	v = s.newAssignmentDiffView(a, []types.GovernanceProfile{broken})
	if v.Profile == nil || v.Profile.Error == "" || v.Profile.Name != "p" {
		t.Errorf("a chain that does not compose = %+v, want the profile with an error", v.Profile)
	}

	none := assignmentState{}
	if got := none.state().(map[string]any); got["assignment"] != nil {
		t.Errorf("state with no assignment = %v", got)
	}
	cur := a
	withRow := assignmentState{current: &cur, chain: []types.GovernanceProfile{standalone}}
	if withRow.hash() == none.hash() || withRow.hash() != computeETag(withRow.state()) {
		t.Error("the state hash must differ with a row and equal the hash of its state")
	}
	renamed := standalone
	renamed.Name = "q"
	if (assignmentState{current: &cur, chain: []types.GovernanceProfile{renamed}}).hash() == withRow.hash() {
		t.Error("a rename of the referenced profile must change the base hash")
	}
}

func TestGovCovProfileChainQ(t *testing.T) {
	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()

	q := &govCovQuerier{}
	chain, err := profileChainQ(ctx, q, govCovPID, true)
	if err != nil || chain == nil || len(chain) != 0 {
		t.Fatalf("a missing profile = %#v, %v; want an empty non-nil chain", chain, err)
	}
	if len(q.seen) != 1 || !strings.HasSuffix(strings.TrimSpace(q.seen[0]), "FOR UPDATE") {
		t.Errorf("statements = %v, want one locking read", q.seen)
	}

	q = &govCovQuerier{}
	if chain, err := profileChainQ(ctx, q, uuid.Nil, false); err != nil || len(chain) != 0 || len(q.seen) != 0 {
		t.Errorf("the nil id read %v and returned %v, %v; want no read at all", q.seen, chain, err)
	}

	boom := errors.New("pg: down")
	q = &govCovQuerier{rowErr: func(string) error { return boom }}
	if _, err := profileChainQ(ctx, q, govCovPID, false); !errors.Is(err, boom) {
		t.Errorf("a failing read = %v, want the injected error", err)
	}
}

func TestGovCovReadAssignmentState(t *testing.T) {
	ctx := httptest.NewRequest(http.MethodGet, "/", nil).Context()
	boom := errors.New("pg: down")

	q := &govCovQuerier{}
	st, err := readAssignmentState(ctx, q, "user", "u", govCovPID, false)
	if err != nil || st.current != nil || st.chain == nil || len(st.chain) != 0 {
		t.Fatalf("nothing stored = %+v, %v; want no assignment and an empty chain", st, err)
	}
	if len(q.seen) != 2 || !strings.Contains(q.seen[0], "governance_assignments") || !strings.Contains(q.seen[1], "governance_profiles") {
		t.Errorf("statements = %v, want the assignment read then the profile read", q.seen)
	}

	q = &govCovQuerier{rowErr: func(string) error { return boom }}
	if _, err := readAssignmentState(ctx, q, "user", "u", govCovPID, true); !errors.Is(err, boom) || len(q.seen) != 1 {
		t.Errorf("a failed assignment read = %v after %d statements, want the error and no profile read", err, len(q.seen))
	}

	q = &govCovQuerier{rowErr: func(sql string) error {
		if strings.Contains(sql, "governance_profiles") {
			return boom
		}
		return pgx.ErrNoRows
	}}
	if _, err := readAssignmentState(ctx, q, "user", "u", govCovPID, false); !errors.Is(err, boom) {
		t.Errorf("a failed chain read = %v, want the injected error", err)
	}
}
