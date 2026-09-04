// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestCreateRunRequest_IsClientDTOAlias pins that the server's create-run body
// (createRunRequest) is the SAME type as the public SDK's client.CreateRunRequest,
// not a hand-maintained copy that can drift. The declaration in runs.go is a type
// alias (`type createRunRequest = client.CreateRunRequest`), so the two
// assignments below hold at COMPILE time — reverting the alias to an independent
// struct fails to build HERE rather than silently re-opening the three-way drift
// the alias closed. Its counterpart on the SDK side,
// pkg/client/dto_parity_test.go, pins the wire tag OPTIONS, which type identity
// does not cover.
func TestCreateRunRequest_IsClientDTOAlias(t *testing.T) {
	// Compile-time identity: only legal because createRunRequest IS
	// client.CreateRunRequest. A struct copy would not assign either direction.
	var _ createRunRequest = client.CreateRunRequest{}
	var _ client.CreateRunRequest = createRunRequest{}
}

// TestRequestDTOs_AreClientDTOAliases pins the same property for the other
// server bodies whose SDK twin is exported. workspaceRequest is the one that
// bit: as a copy it lacked llm_cred, and decodeWorkspaceRequest rejects unknown
// fields, so an SDK caller could neither set the model binding nor smuggle it
// through while the console could. sourceRequest (sources.go) reopened the
// same drift as an independent struct copy until it was aliased too.
func TestRequestDTOs_AreClientDTOAliases(t *testing.T) {
	var _ policyRequest = client.PolicyRequest{}
	var _ client.PolicyRequest = policyRequest{}
	var _ workspaceRequest = client.WorkspaceRequest{}
	var _ client.WorkspaceRequest = workspaceRequest{}
	var _ sourceRequest = client.SourceRequest{}
	var _ client.SourceRequest = sourceRequest{}
}

// jsonTagNames returns the wire names of a struct's json-tagged, exported
// fields (tag "-" and unexported fields excluded).
func jsonTagNames(t *testing.T, typ reflect.Type) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = true
	}
	return out
}

// R5 F027: putIntegrationRequest (setup_integrations.go) hand-mirrors
// types.Integration with no SDK twin to alias, so nothing made adding a field
// to the stored type also make it settable — a new operator-settable field
// built clean, passed every alias/parity test, and was simply unreachable over
// the wire (decodeStrict 400s the field the console would send). This is that
// missing compile-independent parity: every types.Integration wire field is
// either settable on the PUT body or explicitly server-owned, and the
// server-owned list is written out so a new field forces a deliberate choice
// rather than defaulting to "silently unsettable".
func TestPutIntegrationRequest_MirrorsSettableIntegrationFields(t *testing.T) {
	// The fields the SERVER owns on a stored Integration: the id comes from the
	// URL (path-is-authoritative, like handleDeleteSecret) and the timestamps
	// are stamped by handlePutIntegration from s.cfg.Now(). Everything else on
	// types.Integration is operator-settable and must be on the PUT body.
	serverOwned := map[string]bool{"id": true, "created_at": true, "updated_at": true}

	stored := jsonTagNames(t, reflect.TypeOf(types.Integration{}))
	settable := jsonTagNames(t, reflect.TypeOf(putIntegrationRequest{}))

	for name := range stored {
		if serverOwned[name] {
			if settable[name] {
				t.Errorf("putIntegrationRequest accepts %q, which handlePutIntegration owns — remove it from the DTO or from serverOwned here", name)
			}
			continue
		}
		if !settable[name] {
			t.Errorf("types.Integration has wire field %q but putIntegrationRequest does not — an operator cannot set it (decodeStrict 400s it). Add it to the DTO and to handlePutIntegration's construction, or declare it server-owned in serverOwned here", name)
		}
	}
	for name := range settable {
		if !stored[name] {
			t.Errorf("putIntegrationRequest accepts %q, which types.Integration has no wire field for — the PUT would silently drop it", name)
		}
	}
}
