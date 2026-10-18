// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const ovMount = "/home/agent/ops"

// ovOverlayFixture is ovMemberFixture with an operator mount on the member's
// default and the secret capability enforced, so the member pipeline would drop
// the default's operator grant pairing if it read the default as theirs.
func ovOverlayFixture(t *testing.T) (*Server, *pvStore) {
	t.Helper()
	srv, st := ovMemberFixture(t)
	st.workspaces = []types.Workspace{{ID: uuid.New(), Name: "ops", Status: types.WorkspaceScanned, Sources: []types.WorkspaceSource{
		{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/ops", Target: ovMount}}}}
	mounts := []types.WorkspaceMount{{Source: "/srv/ops", Target: ovMount}}
	st.capStore.govProfile.Ceiling.WorkspaceMounts = mounts
	srv.cfg.DefaultPolicy.WorkspaceMounts = mounts
	st.capStore.enf = map[string]bool{capSecret: true}
	return srv, st
}

func ovOverlayBody(overlay string) string {
	return `{"agent":"claude-code","task":"t","policy_overlay":` + overlay + `}`
}

// ovDoorSpecs answers one body at all three doors and returns the preview's spec,
// create's enforced envelope and the provenance the two dry doors agree on.
func ovDoorSpecs(t *testing.T, srv *Server, body string) (preview, created types.RunPolicySpec, rows []provenanceRow) {
	t.Helper()
	cookie := ovCookie(t)
	pw := doSSO(t, srv, http.MethodPost, previewDoor, cookie, body)
	rows = sameProvenance(t, pw, doSSO(t, srv, http.MethodPost, preflightDoor, cookie, body))
	var pv policyPreviewResponse
	if err := json.Unmarshal(pw.Body.Bytes(), &pv); err != nil {
		t.Fatal(err)
	}
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body)
	var run struct {
		ID uuid.UUID `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("create = %d %v: %s", w.Code, err, w.Body.String())
	}
	ev := waitForRecAudit(t, srv.cfg.Audit.(*recRecorder), run.ID, "run.policy.resolve", "success")
	if err := json.Unmarshal(ev.Data, &created); err != nil {
		t.Fatal(err)
	}
	return pv.Spec, created, rows
}

func grantKeys(gs []types.GrantSpec) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, grantKey(g))
	}
	return out
}

// An overlay that names only the hosts keeps what the default's operator wrote
// in the keys it leaves out: the mount and the grant pairing the member could not
// author themselves. Preview, Review and create agree.
func TestPolicyOverlayKeepsOperatorContentOfTheDefault(t *testing.T) {
	srv, _ := ovOverlayFixture(t)
	body := ovOverlayBody(`{"allowed_domains":["api.anthropic.com"],"denied_domains":["bad.example"]}`)
	preview, created, _ := ovDoorSpecs(t, srv, body)
	for door, spec := range map[string]types.RunPolicySpec{"preview": preview, "create": created} {
		if len(spec.WorkspaceMounts) != 1 || spec.WorkspaceMounts[0].Target != ovMount {
			t.Errorf("%s dropped the operator's mount: %+v", door, spec.WorkspaceMounts)
		}
		if !slices.Contains(grantKeys(spec.EligibleGrants), "api_key:api.anthropic.com") {
			t.Errorf("%s dropped the operator's grant pairing: %v", door, grantKeys(spec.EligibleGrants))
		}
		if !slices.Equal(spec.AllowedDomains, []string{"api.anthropic.com"}) || !slices.Contains(spec.DeniedDomains, "bad.example") {
			t.Errorf("%s allowed %v denied %v, want the overlay's hosts", door, spec.AllowedDomains, spec.DeniedDomains)
		}
	}
}

// An overlay's own keys meet the member's ceiling like an inline policy: a host
// outside it is removed and reads person/clamped, a host the overlay replaced
// away reads person/removed, and the default's untouched entries stay the policy's.
func TestPolicyOverlayKeysAreBoundedLikeInline(t *testing.T) {
	srv, _ := ovOverlayFixture(t)
	body := ovOverlayBody(`{"allowed_domains":["api.anthropic.com","` + ovHostOutside + `"]}`)
	preview, created, rows := ovDoorSpecs(t, srv, body)
	for door, spec := range map[string]types.RunPolicySpec{"preview": preview, "create": created} {
		if !slices.Equal(spec.AllowedDomains, []string{"api.anthropic.com"}) {
			t.Errorf("%s allowed = %v, want the outside host bounded and the replaced one gone", door, spec.AllowedDomains)
		}
	}
	person, def := provSource{Kind: provKindPerson}, policyProvSource(policySourceRecord{Kind: policyKindProfile, Name: "walled"})
	for _, want := range []provenanceRow{
		{fieldAllowed, "api.anthropic.com", person, provEffectAdded},
		{fieldAllowed, ovHostOutside, person, provEffectClamped},
		{fieldAllowed, ovHostInside, person, provEffectRemoved},
		{fieldGrants, "api_key:api.anthropic.com", def, provEffectAdded},
		{fieldMounts, ovMount, def, provEffectAdded},
	} {
		if !slices.Contains(rows, want) {
			t.Errorf("missing %+v in %+v", want, rows)
		}
	}
}

// The cross-key risk: an overlay that removes the host a default grant targets
// leaves the grant as the default had it, without egress, and the person/removed
// row is what explains the tool that then cannot reach its host.
func TestPolicyOverlayRemovedHostLeavesTheDefaultGrant(t *testing.T) {
	srv, _ := ovOverlayFixture(t)
	preview, created, rows := ovDoorSpecs(t, srv, ovOverlayBody(`{"allowed_domains":["`+ovHostInside+`"]}`))
	for door, spec := range map[string]types.RunPolicySpec{"preview": preview, "create": created} {
		if slices.Contains(spec.AllowedDomains, "api.anthropic.com") || !slices.Contains(grantKeys(spec.EligibleGrants), "api_key:api.anthropic.com") {
			t.Errorf("%s allowed %v grants %v, want the host gone and the default's grant kept", door, spec.AllowedDomains, grantKeys(spec.EligibleGrants))
		}
	}
	if !provHas(rows, fieldAllowed, "api.anthropic.com", provSource{Kind: provKindPerson}, provEffectRemoved) {
		t.Errorf("no person/removed row explains the grant's missing host: %+v", rows)
	}
}

// D3: an overlay lays onto the caller's default, so it never rides beside
// policy_id or inline_policy; beside a preset the preset's own refusal answers;
// and every malformed overlay is refused by name, never trimmed.
func TestPolicyOverlayConflictsAndShape(t *testing.T) {
	const redacted = `policy_overlay.allowed_domains[0] is a redacted placeholder, not a value: set the real value or leave the field out`
	for _, tc := range []struct {
		name, extra, reason, msg string
	}{
		{"beside policy_id", `,"policy_id":"` + uuid.NewString() + `","policy_overlay":{"allowed_domains":["a.example.com"]}`, reasonPolicyOverlayConflict, policyOverlayConflictMsg()},
		{"beside inline_policy", `,"inline_policy":{"allowed_domains":["a.example.com"]},"policy_overlay":{"allowed_domains":["a.example.com"]}`, reasonPolicyOverlayConflict, policyOverlayConflictMsg()},
		{"null", `,"policy_overlay":null`, reasonPolicyOverlayInvalid, ""},
		{"an empty object", `,"policy_overlay":{}`, reasonPolicyOverlayInvalid, ""},
		{"not an object", `,"policy_overlay":["a.example.com"]`, reasonPolicyOverlayInvalid, ""},
		{"an unknown key", `,"policy_overlay":{"allowed_hosts":["a.example.com"]}`, reasonPolicyOverlayInvalid, ""},
		{"a null value", `,"policy_overlay":{"allowed_domains":null}`, reasonPolicyOverlayInvalid, ""},
		{"a value of the wrong type", `,"policy_overlay":{"max_holds":"many"}`, reasonPolicyOverlayInvalid, ""},
		{"an excluded nested field", `,"policy_overlay":{"llm_inspection":{"mode":"audit","workspace_secret_values":["x"]}}`, reasonPolicyOverlayInvalid, ""},
		{"a redacted placeholder", `,"policy_overlay":{"allowed_domains":["<redacted>"]}`, reasonPolicyOverlayInvalid, "policy_overlay is not usable: " + redacted + "."},
	} {
		for _, door := range contractDoors {
			t.Run(tc.name+"/"+door.name, func(t *testing.T) {
				h := newHarness(t)
				w := do(t, h.srv, http.MethodPost, door.path, adminToken, contractBody(tc.extra))
				if w.Code != http.StatusBadRequest || errorReason(w) != tc.reason {
					t.Fatalf("got %d %s, want 400 %s", w.Code, w.Body.String(), tc.reason)
				}
				if !strings.Contains(w.Body.String(), "policy_overlay") {
					t.Errorf("the refusal does not name policy_overlay: %s", w.Body.String())
				}
				if tc.msg != "" {
					var body struct{ Error string }
					if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error != tc.msg {
						t.Errorf("sentence = %q, want %q", body.Error, tc.msg)
					}
				}
			})
		}
	}
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, `{"preset":"ops","policy_overlay":{"allowed_domains":["a.example.com"]}}`)
	if w.Code != http.StatusBadRequest || errorReason(w) != reasonPresetField {
		t.Errorf("beside a preset = %d %s, want 400 %s", w.Code, w.Body.String(), reasonPresetField)
	}
}

// The run-mode checks read an overlay's run-wide rules exactly as an inline
// policy's, spoken of at policy_overlay.
func TestPolicyOverlayRunModeChecks(t *testing.T) {
	for _, tc := range []struct{ name, body, msg string }{
		{"run-wide tool rules", interactiveOK + `,"policy_overlay":{"tool_rules":[{"tool":"Bash","effect":"hold"}]}`,
			runModeConflictMsg("policy_overlay.tool_rules", "Tool rules belong to each tool: use tools[].tool_rules and default_effect.")},
		{"run-wide push rules", interactiveOK + `,"policy_overlay":{"push_rules":{"deny_paths":["a/"]}}`, ""},
		{"a background web gateway", `"experience":"background",` + agentTask + `,"policy_overlay":{"ui_apps":[{"name":"ide","port":8080}]},` + noRepos,
			runModeConflictMsg("policy_overlay.ui_apps", "A background task has no web application gateway.")},
		{"a background idle stop", `"experience":"background",` + agentTask + `,"policy_overlay":{"auto_stop_after_sec":600},` + noRepos, idleStopBackgroundMsg()},
	} {
		for _, door := range contractDoors {
			t.Run(tc.name+"/"+door.name, func(t *testing.T) {
				h := newHarness(t)
				h.srv.cfg.AgentImages = composeAgentImages
				w := do(t, h.srv, http.MethodPost, door.path, adminToken, modeBody(tc.body))
				if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonRunModeConflict || !strings.Contains(w.Body.String(), "policy_overlay") && tc.msg != idleStopBackgroundMsg() {
					t.Fatalf("got %d %s, want 422 %s naming policy_overlay", w.Code, w.Body.String(), reasonRunModeConflict)
				}
				if tc.msg != "" {
					var body struct{ Error string }
					if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Error != tc.msg {
						t.Errorf("sentence = %q, want %q", body.Error, tc.msg)
					}
				}
			})
		}
	}
}

// The overlay's audit names its keys and never a value: the canary reaches the
// run's enforced policy (run.policy.resolve) and no other row. run.create binds
// the keys to the run, and its policy_source records the default it was laid on.
func TestPolicyOverlayAuditCarriesKeysNotValues(t *testing.T) {
	const canary = "canary-overlay-7f3.example"
	srv, st := ovOverlayFixture(t)
	_, created, _ := ovDoorSpecs(t, srv, ovOverlayBody(`{"denied_domains":["`+canary+`"]}`))
	if !slices.Contains(created.DeniedDomains, canary) {
		t.Fatalf("the overlay did not reach the run: %v", created.DeniedDomains)
	}
	var applied, runCreate int
	for _, ev := range st.audit.snapshot() {
		if ev.Action != "run.policy.resolve" && strings.Contains(string(ev.Data)+ev.Target, canary) {
			t.Errorf("%s carries the overlay's value: %s", ev.Action, ev.Data)
		}
		switch ev.Action {
		case "policy.overlay.apply":
			applied++
			if string(ev.Data) != `{"keys":["denied_domains"]}` || ev.RunID != nil {
				t.Errorf("policy.overlay.apply = run %v %s, want no run id and the keys", ev.RunID, ev.Data)
			}
		case "run.create":
			runCreate++
			var data struct {
				Overlay []string           `json:"policy_overlay"`
				Source  policySourceRecord `json:"policy_source"`
			}
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(data.Overlay, []string{"denied_domains"}) || !slices.Equal(data.Source.Overlay, []string{"denied_domains"}) {
				t.Errorf("run.create policy_overlay %v policy_source.overlay %v, want the keys", data.Overlay, data.Source.Overlay)
			}
		}
	}
	if applied != 1 || runCreate != 1 {
		t.Errorf("policy.overlay.apply rows %d run.create rows %d, want one each: the dry doors audit nothing", applied, runCreate)
	}
}

// DR-0088: the New Run edits beside a saved policy travel as overrides, never as
// an overlay. That body answers the same at the three doors, and again when the
// same bytes are sent a second time; an overlay beside the policy_id stays refused.
func TestPolicyIDWithOverridesParity(t *testing.T) {
	srv, st := ovOverlayFixture(t)
	st.capStore.enf = nil
	id := uuid.New()
	st.policies[id] = types.RunPolicy{ID: id, Name: "ops", Spec: types.RunPolicySpec{
		MinConfinementClass: types.CC1, AllowedDomains: []string{"api.anthropic.com"}}}
	body := `{"agent":"claude-code","task":"t","policy_id":"` + id.String() + `","overrides":{"hosts":{"add_allowed":["` + ovHostInside + `"]},` +
		`"agent":{"add_secrets":[{"secret_name":"my-key","host":"api.anthropic.com"}]}}}`
	var first types.RunPolicySpec
	for i := range 2 {
		preview, created, rows := ovDoorSpecs(t, srv, body)
		for door, spec := range map[string]types.RunPolicySpec{"preview": preview, "create": created} {
			if !slices.Contains(spec.AllowedDomains, ovHostInside) || !slices.Contains(grantKeys(spec.EligibleGrants), "api_key:api.anthropic.com") {
				t.Errorf("send %d %s: allowed %v grants %v, want the added host and secret", i, door, spec.AllowedDomains, grantKeys(spec.EligibleGrants))
			}
		}
		if !provHas(rows, fieldAllowed, ovHostInside, provSource{Kind: provKindPerson}, provEffectAdded) {
			t.Errorf("send %d: the added host is not the person's: %+v", i, rows)
		}
		if i == 0 {
			first = created
		} else if !slices.Equal(first.AllowedDomains, created.AllowedDomains) || !slices.Equal(grantKeys(first.EligibleGrants), grantKeys(created.EligibleGrants)) {
			t.Errorf("the reused body answered differently: %+v then %+v", first, created)
		}
	}
	overlay := `{"agent":"claude-code","task":"t","policy_id":"` + id.String() + `","policy_overlay":{"allowed_domains":["` + ovHostInside + `"]}}`
	for _, door := range contractDoors {
		if w := doSSO(t, srv, http.MethodPost, door.path, ovCookie(t), overlay); w.Code != http.StatusBadRequest || errorReason(w) != reasonPolicyOverlayConflict {
			t.Errorf("%s: an overlay beside policy_id = %d %s, want 400 %s", door.name, w.Code, w.Body.String(), reasonPolicyOverlayConflict)
		}
	}
}

// withPolicyKeys copies exactly the named key, for every top-level policy key:
// one it skipped would silently drop that key of a document's overlay.
func TestWithPolicyKeysCopiesEveryPolicyKey(t *testing.T) {
	var from types.RunPolicySpec
	v := reflect.ValueOf(&from).Elem()
	for i := range v.NumField() {
		switch f := v.Field(i); f.Kind() {
		case reflect.Slice:
			f.Set(reflect.MakeSlice(f.Type(), 1, 1))
		case reflect.Pointer:
			f.Set(reflect.New(f.Type().Elem()))
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int:
			f.SetInt(1)
		case reflect.String:
			f.SetString("x")
		default:
			t.Fatalf("%s is a %s this test cannot fill", v.Type().Field(i).Name, f.Kind())
		}
	}
	for i := range policySpecType.NumField() {
		key, _, _ := strings.Cut(policySpecType.Field(i).Tag.Get("json"), ",")
		out := reflect.ValueOf(withPolicyKeys(types.RunPolicySpec{}, from, []string{key}))
		for j := range out.NumField() {
			if copied := !out.Field(j).IsZero(); copied != (i == j) {
				t.Errorf("key %s: field %s copied = %v", key, policySpecType.Field(j).Name, copied)
			}
		}
	}
}

// policy.overlay.apply is written only for a create the policy step let through:
// a card edit refused after the overlay was laid leaves no row behind.
func TestPolicyOverlayRefusedCreateLeavesNoApplyRow(t *testing.T) {
	srv, st := ovOverlayFixture(t)
	body := `{"agent":"claude-code","task":"t","policy_overlay":{"denied_domains":["x.example"]},` +
		`"overrides":{"agent":{"add_secrets":[{"secret_name":"theirs","host":"api.anthropic.com"}]}}}`
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", ovCookie(t), body); w.Code == http.StatusCreated {
		t.Fatalf("create = %d, want a refusal: %s", w.Code, w.Body.String())
	}
	for _, ev := range st.audit.snapshot() {
		if ev.Action == "policy.overlay.apply" {
			t.Errorf("a refused create wrote %s %s", ev.Action, ev.Data)
		}
	}
}

// The run page explains a key the overlay replaced as the person's overlay, its
// removals and additions alike; a key it left alone keeps its usual cause.
func TestRunPolicyViewOverlayCause(t *testing.T) {
	base := types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com", ovHostInside}}
	resolved := types.RunPolicySpec{MinConfinementClass: types.CC3, AllowedDomains: []string{"api.anthropic.com", "mine.example"},
		DeniedDomains: []string{"x.example"}}
	srv, st := pvFixture(t, &capStore{})
	run := st.seedRun(pvOwner, types.RunRunning)
	src := pvSource(policyKindProfile, nil, "walled", true, base)
	src.Overlay = []string{"allowed_domains", "min_confinement_class"}
	st.rows(run.ID, pvCreate(nil, src), pvResolve(resolved, false))
	_, resp := pvMember(t, pvOwner).get(t, srv, run.ID)
	if c := pvChange(resp.Changes, causeOverlay, fieldAllowed); c == nil || !slices.Equal(c.Added, []string{"mine.example"}) || !slices.Equal(c.Removed, []string{ovHostInside}) {
		t.Errorf("the overlay's hosts: %+v in %+v", c, resp.Changes)
	}
	if c := pvChange(resp.Changes, causeOverlay, fieldMinCC); c == nil {
		t.Errorf("the overlay's confinement: %+v", resp.Changes)
	}
	if c := pvChange(resp.Changes, causeLimits, fieldDenied); c == nil || !slices.Equal(c.Added, []string{"x.example"}) {
		t.Errorf("a key the overlay left alone lost its cause: %+v", resp.Changes)
	}
}

// A create the policy step let through and a later step refused leaves no policy
// apply row either, for an overlay and for an inline policy alike. The workspace is
// refused after the policy step, so each body reaches it.
func TestPolicyApplyRowsWaitForTheWholeFold(t *testing.T) {
	srv, st := ovOverlayFixture(t)
	for _, body := range []string{
		`{"agent":"claude-code","task":"t","policy_overlay":{"denied_domains":["x.example"]},"workspace_id":"` + uuid.NewString() + `"}`,
		`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2"},"workspace_id":"` + uuid.NewString() + `"}`,
	} {
		if w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", ovCookie(t), body); w.Code != http.StatusNotFound || errorReason(w) != reasonWorkspaceNotFound {
			t.Fatalf("create = %d %s, want the workspace's 404 %s after the policy step", w.Code, w.Body.String(), reasonWorkspaceNotFound)
		}
	}
	for _, ev := range st.audit.snapshot() {
		if ev.Action == "policy.overlay.apply" || ev.Action == "policy.inline.apply" {
			t.Errorf("a create refused after the policy step wrote %s %s", ev.Action, ev.Data)
		}
	}
}

// stalePoolStore refuses the run's insert as a pool changed under it: the last refusal a create
// can answer, after its policy has been resolved and its identity minted.
type stalePoolStore struct{ store.Store }

func (stalePoolStore) CreateRun(context.Context, types.AgentRun) (types.AgentRun, error) {
	return types.AgentRun{}, store.ErrRunnerPoolStale
}

// The policy apply rows are written with the run's row: a create refused at the insert leaves
// neither them nor a run.create.
func TestPolicyApplyRowsFollowTheRunRow(t *testing.T) {
	srv, st := ovOverlayFixture(t)
	srv.cfg.Store = stalePoolStore{srv.cfg.Store}
	for _, body := range []string{
		ovOverlayBody(`{"denied_domains":["x.example"]}`),
		`{"agent":"claude-code","task":"t","inline_policy":{"min_confinement_class":"CC2"}}`,
	} {
		if w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", ovCookie(t), body); w.Code != http.StatusConflict || errorReason(w) != string(runnerpool.ReasonStale) {
			t.Fatalf("create = %d %s, want 409 %s at the insert", w.Code, w.Body.String(), runnerpool.ReasonStale)
		}
	}
	for _, ev := range st.audit.snapshot() {
		if ev.Action == "policy.overlay.apply" || ev.Action == "policy.inline.apply" || ev.Action == "run.create" {
			t.Errorf("a create refused at the insert wrote %s %s", ev.Action, ev.Data)
		}
	}
}
