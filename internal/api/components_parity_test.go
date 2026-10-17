// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// parityCase is one refusal class asked of the three run doors.
type parityCase struct {
	// setup arranges the fixture and returns the request body.
	setup func(t *testing.T, f *componentFixture) map[string]any
	// asAdmin asks as the admin bearer rather than the signed-in member.
	asAdmin bool
	status  int
	reason  string
	// previewAdmits: the policy preview keeps its body for this class by
	// design and answers 200 (A27 for a missing secret; the preview does not
	// resolve autonomy). Create and Review still refuse identically.
	previewAdmits bool
}

// parityAnswer is what one door answered and audited.
type parityAnswer struct {
	door string
	doorDecisions
	raw string
}

func (f *componentFixture) askAll(t *testing.T, c parityCase, body map[string]any) []parityAnswer {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var out []parityAnswer
	for _, door := range componentDoors {
		before := len(f.rec.snapshot())
		var w *httptest.ResponseRecorder
		if c.asAdmin {
			w = do(t, f.srv, http.MethodPost, door, adminToken, string(raw))
		} else {
			w = kernelLaunch(t, f.srv, f.st.govEscapeStore, kernelUserTier, []string{"eng"}, door, string(raw))
		}
		out = append(out, parityAnswer{door: door, doorDecisions: decisionsOf(t, f.rec.snapshot()[before:], w), raw: w.Body.String()})
	}
	return out
}

// parityWorkspace is a scanned local workspace whose owner denied one host for good.
func parityWorkspace(t *testing.T, f *componentFixture) types.Workspace {
	ws := types.Workspace{
		ID: uuid.New(), Name: "ws", Status: types.WorkspaceScanned, DeniedEgress: []string{"ws-denied.example"},
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: t.TempDir()}},
	}
	f.st.ws, f.st.workspaces = &ws, []types.Workspace{ws}
	return ws
}

func parityBody(refs ...any) func(*testing.T, *componentFixture) map[string]any {
	return func(*testing.T, *componentFixture) map[string]any { return componentBody(refs...) }
}

// parityOrg is a body naming one organisation component with def.
func parityOrg(def types.ComponentDefinition, arrange func(f *componentFixture)) func(*testing.T, *componentFixture) map[string]any {
	return func(_ *testing.T, f *componentFixture) map[string]any {
		if arrange != nil {
			arrange(f)
		}
		return componentBody(f.org(compOrgID, def))
	}
}

func residentOff(f *componentFixture) {
	f.st.siteConfig.Components = &types.ComponentSettings{DenyResidentDelivery: true}
}

func denyEveryone(f *componentFixture, kind, value string) {
	f.cs.grants = append(f.cs.grants, grant(types.CapabilitySubjectAll, "", kind, value, types.CapabilityDeny))
}

// parityCases is every refusal class a body carrying components can meet.
func parityCases() map[string]parityCase {
	own := inlineComponent([]string{"svc.example"}, headerSecret(compOwnSecret, "svc.example"))
	orgEnv := types.ComponentDefinition{Secrets: []types.ComponentSecret{{SecretName: compOwnSecret,
		Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryEnv, Var: "ORG_TOKEN"}}}}
	cases := map[string]parityCase{
		"shape: one component twice": {status: 400, reason: "component_ref_invalid",
			setup: parityBody(map[string]any{"id": compOrgID}, map[string]any{"id": strings.ToUpper(compOrgID)})},
		"shape: a non-uuid id (A26)": {status: 400, reason: reasonInvalidRequestBody,
			setup: parityBody(map[string]any{"id": "jira-api"})},
		"who: custom components turned off": {status: 403, reason: "capability_feature",
			setup: func(_ *testing.T, f *componentFixture) map[string]any {
				denyEveryone(f, capFeature, featureCustomComponent)
				return componentBody(own)
			}},
		"who: an organisation's component not granted": {status: 403, reason: "capability_component",
			setup: parityOrg(types.ComponentDefinition{Hosts: []string{"org-api.example"}}, func(f *componentFixture) {
				f.cs.restricted = map[string]map[string]bool{capComponent: {compOrgID: true}}
			})},
		"who: an id that names nothing (A18)": {status: 403, reason: "capability_component",
			setup: parityBody(map[string]any{"id": compAbsentID})},
		"who: another person's saved row (A18)": {status: 403, reason: "capability_component",
			setup: func(_ *testing.T, f *componentFixture) map[string]any {
				f.st.components = append(f.st.components, types.Component{ID: uuid.MustParse(compOtherID), Owner: "sub-carol", Name: "carol's",
					Definition: types.ComponentDefinition{Hosts: []string{"carol.example"}}})
				return componentBody(map[string]any{"id": compOtherID})
			}},
		"model host": {status: 422, reason: "component_host_serves_model",
			setup: parityBody(inlineComponent([]string{"API.OpenAI.com.:8443"}))},
		"definition: a reserved environment name (A36)": {status: 422, reason: "component_definition_invalid",
			setup: parityBody(inlineComponent(nil, envSecret(compOwnSecret, "NODE_OPTIONS")))},
		"secret: not the person's own": {status: 422, reason: "component_secret_not_owned", previewAdmits: true,
			setup: parityBody(inlineComponent([]string{"svc.example"}, headerSecret(compOperatorSecret, "svc.example")))},
		"secret: a shared one the operator has not stored": {status: 422, reason: "component_secret_missing", previewAdmits: true,
			setup: parityOrg(types.ComponentDefinition{Hosts: []string{"org-api.example"}, Secrets: []types.ComponentSecret{{
				SecretName: "not-stored", Shared: true, Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}}}}, nil)},
		"host: the deployment's deny list": {status: 422, reason: "component_host_denied",
			setup: func(_ *testing.T, f *componentFixture) map[string]any {
				f.srv.cfg.DefaultPolicy.DeniedDomains = []string{"*.blocked.example"}
				return componentBody(inlineComponent([]string{"api.blocked.example:8443"}))
			}},
		"host: an egress_host deny row": {status: 422, reason: "component_host_denied",
			setup: func(_ *testing.T, f *componentFixture) map[string]any {
				denyEveryone(f, capEgressHost, "capdenied.example")
				return componentBody(inlineComponent([]string{"capdenied.example"}))
			}},
		// F3: launch folds a workspace's denies into the spec only after the
		// run exists; the gate must read them itself at every door.
		"host: the workspace's deny (F3)": {status: 422, reason: "component_host_denied", asAdmin: true,
			setup: func(t *testing.T, f *componentFixture) map[string]any {
				body := componentBody(inlineComponent([]string{"ws-denied.example"}))
				body["workspace_id"] = parityWorkspace(t, f).ID.String()
				return body
			}},
		"host: another component's header host": {status: 422, reason: "component_host_collision",
			setup: parityBody(own, inlineComponent([]string{"svc.example:443"}, headerSecret(compOwnSecret, "svc.example")))},
		"host: two credentials for one host": {status: 422, reason: "credential_host_collision",
			setup: func(_ *testing.T, f *componentFixture) map[string]any {
				twoCredentialsOnOneHost(f)
				return componentBody(own)
			}},
		"resident: a person's file with the switch on": {status: 422, reason: "component_resident_delivery_denied",
			setup: func(_ *testing.T, f *componentFixture) map[string]any {
				residentOff(f)
				return componentBody(inlineComponent(nil, map[string]any{"secret_name": compOwnSecret, "delivery": map[string]any{"mode": "file", "file": "token"}}))
			}},
		// The organisation's row is bound by the switch too: no org exemption.
		"resident: an organisation's row with the switch on": {status: 422, reason: "component_resident_delivery_denied",
			setup: parityOrg(orgEnv, residentOff)},
		"autonomy: the organisation's cap": {status: 403, reason: "component_autonomy", previewAdmits: true,
			setup: func(_ *testing.T, f *componentFixture) map[string]any {
				f.st.siteConfig.Components = &types.ComponentSettings{AutonomyCap: types.AutonomyL0}
				return componentBody(own)
			}},
	}
	// A2: a person may not name an address in any spelling.
	for _, literal := range []string{"10.0.0.1", "[fd00::1]:443", "::ffff:10.0.0.1", "0x7f000001", "10.0.0.1."} {
		cases["definition: a person's IP literal "+literal+" (A2)"] = parityCase{status: 422, reason: "component_definition_invalid",
			setup: parityBody(inlineComponent([]string{literal}))}
	}
	return cases
}

// TestComponentsParity_EveryRefusalClassAnswersTheSameAtThreeDoors extends the
// decisions-equal harness to the policy preview: for a body that carries
// components, create, Review and the preview answer each refusal class with
// the same status, reason and bytes, and write the same authz.denied rows.
// None of them leaves a run or a snapshot behind.
func TestComponentsParity_EveryRefusalClassAnswersTheSameAtThreeDoors(t *testing.T) {
	cases := parityCases()
	for _, name := range sortedKeys(cases) {
		c := cases[name]
		t.Run(name, func(t *testing.T) {
			f := newComponentFixture(t)
			answers := f.askAll(t, c, c.setup(t, f))
			launch := answers[0]
			for _, a := range answers {
				if a.door == componentDoors[2] && c.previewAdmits {
					if a.code != http.StatusOK {
						t.Errorf("%s = %d %s, want 200 (the preview keeps its body for this class)", a.door, a.code, a.raw)
					}
					continue
				}
				if a.code != c.status {
					t.Errorf("%s = %d %s, want %d %s", a.door, a.code, a.raw, c.status, c.reason)
					continue
				}
				if got := errorReasonOf(a.raw); got != c.reason {
					t.Errorf("%s reason = %q, want %q", a.door, got, c.reason)
				}
				if a.body != launch.body {
					t.Errorf("%s answered %s, launch answered %s", a.door, a.body, launch.body)
				}
				if !slices.Equal(a.refused, launch.refused) {
					t.Errorf("%s authz.denied rows %v, launch %v", a.door, a.refused, launch.refused)
				}
			}
			if c.status == http.StatusForbidden && len(launch.refused) != 1 {
				t.Errorf("launch authz.denied rows = %v, want one", launch.refused)
			}
			if len(f.st.runs) != 0 || len(f.st.snapshots) != 0 {
				t.Errorf("a refused request left %d runs and %d snapshots", len(f.st.runs), len(f.st.snapshots))
			}
		})
	}
}

// TestComponentsParity_AbsentIDsAnswerTheUngrantedBytes pins A18 across the
// doors at once: an ungranted real id, an absent id and another person's row
// answer the same bytes and the same authz.denied row at all three doors.
func TestComponentsParity_AbsentIDsAnswerTheUngrantedBytes(t *testing.T) {
	cases := parityCases()
	var bodies, rows []string
	for _, name := range []string{
		"who: an organisation's component not granted",
		"who: an id that names nothing (A18)",
		"who: another person's saved row (A18)",
	} {
		f := newComponentFixture(t)
		for _, a := range f.askAll(t, cases[name], cases[name].setup(t, f)) {
			bodies = append(bodies, strings.TrimSpace(a.raw))
			rows = append(rows, strings.Join(a.refused, "|"))
		}
	}
	for i := range bodies {
		if bodies[i] != componentRefusalBody || rows[i] != rows[0] {
			t.Errorf("answer %d = %s / %s, want %s / %s", i, bodies[i], rows[i], componentRefusalBody, rows[0])
		}
	}
}

// TestComponentsParity_AdmittedAtThreeDoors is the control for the refusal
// table: the closest admitted variant of each case passes at all three doors,
// so each refusal above is the class it names. It also pins A28: a shared
// grant's secret name reaches no member-facing body.
func TestComponentsParity_AdmittedAtThreeDoors(t *testing.T) {
	cases := map[string]parityCase{
		"a person's own header and env":                      {setup: parityBody(inlineComponent([]string{"svc.example"}, headerSecret(compOwnSecret, "svc.example"), envSecret(compOwnSecret, "SVC_TOKEN")))},
		"an organisation's IP literal keeps admin semantics": {setup: parityOrg(types.ComponentDefinition{Hosts: []string{"10.20.30.40"}}, nil)},
		"a host the workspace does not deny (F3 control)": {asAdmin: true, setup: func(t *testing.T, f *componentFixture) map[string]any {
			body := componentBody(inlineComponent([]string{"ws-allowed.example"}))
			body["workspace_id"] = parityWorkspace(t, f).ID.String()
			return body
		}},
		"an organisation's header with the resident switch on": {setup: parityOrg(types.ComponentDefinition{Hosts: []string{"org-api.example"},
			Secrets: []types.ComponentSecret{{SecretName: compOwnSecret, Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}}}}, residentOff)},
		"a shared secret the operator holds (A28)": {setup: parityOrg(types.ComponentDefinition{Hosts: []string{"org-api.example"},
			Secrets: []types.ComponentSecret{{SecretName: compOperatorSecret, Shared: true, Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}}}}, nil)},
	}
	for _, name := range sortedKeys(cases) {
		c := cases[name]
		t.Run(name, func(t *testing.T) {
			f := newComponentFixture(t)
			for _, a := range f.askAll(t, c, c.setup(t, f)) {
				if a.code >= 300 {
					t.Errorf("%s = %d %s, want admitted", a.door, a.code, a.raw)
				}
				if strings.Contains(a.raw, compOperatorSecret) {
					t.Errorf("%s names the operator's shared secret: %s", a.door, a.raw)
				}
			}
		})
	}
}
