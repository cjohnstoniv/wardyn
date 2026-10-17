// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestComponentFacts_PGGrantedUngrantedAbsentAtEveryDoor is the member
// isolation matrix against the real store: the component rows, the
// restriction and the grant are Postgres rows, and the answers are the ones
// the in-memory matrix pins. Guarded by WARDYN_TEST_PG.
func TestComponentFacts_PGGrantedUngrantedAbsentAtEveryDoor(t *testing.T) {
	h, secrets := newRunOwnerPGHarness(t)
	ctx := context.Background()
	// The harness's bare api_key ceiling entry is no grant a default-policy run can hold.
	h.srv.cfg.DefaultPolicy.EligibleGrants = nil
	st := h.srv.cfg.Store
	cs, ok := st.(store.ComponentStore)
	if !ok {
		t.Fatal("the Postgres store has no component seam")
	}
	const (
		granted       = "facts-granted"
		other         = "facts-other"
		sharedSecret  = "facts-org-shared"
		orgConfigured = "facts-org-config-value"
	)
	if err := secrets.Put(ctx, sharedSecret, []byte("operator-value")); err != nil {
		t.Fatal(err)
	}
	orgID, mineID, othersID := uuid.New(), uuid.New(), uuid.New()
	// The restriction before the row, the order the organisation's write takes.
	if err := st.SetCapabilityRestriction(ctx, capComponent, orgID.String(), true, "admin"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []types.Component{
		{ID: orgID, Name: "Org Tool", Definition: types.ComponentDefinition{
			Hosts: []string{"org-api.example"},
			Secrets: []types.ComponentSecret{{SecretName: sharedSecret, Shared: true,
				Delivery: types.ComponentDelivery{Mode: types.ComponentDeliveryHeader, Host: "org-api.example"}}},
			Config: map[string]string{"ORG_REGION": orgConfigured},
		}},
		{ID: mineID, Owner: granted, Name: "My Tool", Definition: types.ComponentDefinition{Hosts: []string{"mine.example"}}},
		{ID: othersID, Owner: other, Name: "Their Tool", Definition: types.ComponentDefinition{Hosts: []string{"theirs.example"}}},
	} {
		if _, err := cs.CreateComponent(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.UpsertCapabilityGrant(ctx, types.CapabilityGrant{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: granted,
		Capability: capComponent, Value: orgID.String(), Effect: types.CapabilityAllow, CreatedBy: "admin",
	}); err != nil {
		t.Fatal(err)
	}
	orgFact := `{"kind":"custom","id":"` + orgID.String() + `","name":"Org Tool","version":1,"reason":"org","status":"ready","requirements":[],` +
		`"hosts":["org-api.example"],"secrets":[{"delivery":"header","shared":true}],"config_keys":["ORG_REGION"],"tls_intercept":true}`
	mineFact := `{"kind":"custom","id":"` + mineID.String() + `","name":"My Tool","version":1,"reason":"self","status":"ready","requirements":[],` +
		`"hosts":["mine.example"],"self_defined":true}`
	sessions := map[string]*http.Cookie{
		granted: ssoSession(t, granted, "granted@example.com", oidc.RoleUser),
		other:   ssoSession(t, other, "other@example.com", oidc.RoleUser),
	}
	for _, tc := range []struct {
		name, who, id string
		fact          string // "" for the one refusal
	}{
		{"granted", granted, orgID.String(), orgFact},
		{"granted, upper case", granted, strings.ToUpper(orgID.String()), orgFact},
		{"own saved component", granted, mineID.String(), mineFact},
		{"ungranted", other, orgID.String(), ""},
		{"absent", granted, uuid.NewString(), ""},
		{"another person's", granted, othersID.String(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"agent":"claude-code","task":"components","components":[{"id":"` + tc.id + `"}]}`
			// The dry doors first: launch creates a run.
			for _, door := range []string{componentDoors[2], componentDoors[1], componentDoors[0]} {
				w := doSSO(t, h.srv, http.MethodPost, door, sessions[tc.who], body)
				for _, private := range []string{sharedSecret, orgConfigured, "Their Tool", "theirs.example"} {
					if strings.Contains(w.Body.String(), private) {
						t.Errorf("%s carries %q: %s", door, private, w.Body.String())
					}
				}
				switch {
				case tc.fact == "":
					if w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != componentRefusalBody {
						t.Errorf("%s = %d %s, want 403 %s", door, w.Code, w.Body.String(), componentRefusalBody)
					}
				case door == componentDoors[0]:
					if w.Code != http.StatusCreated {
						t.Errorf("%s = %d %s, want 201", door, w.Code, w.Body.String())
					}
				default:
					if got := nonAgentDoorFacts(t, door, w); !slices.Equal(got, []string{tc.fact}) {
						t.Errorf("%s components =\n %v\nwant\n %v", door, got, []string{tc.fact})
					}
				}
			}
		})
	}
	for _, door := range componentDoors {
		w := doSSO(t, h.srv, http.MethodPost, door, sessions[granted], `{"agent":"claude-code","task":"components","components":[{"id":"jira-api"}]}`)
		if got := decodeErrorBody(t, w); w.Code != http.StatusBadRequest || got.Reason != reasonInvalidRequestBody {
			t.Errorf("%s = %d %s, want 400 %s", door, w.Code, w.Body.String(), reasonInvalidRequestBody)
		}
	}
}
