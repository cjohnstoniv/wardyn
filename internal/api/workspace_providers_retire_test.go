// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoEntraBlockJSON and adoOwnPATBlock are an entra block that signs in and one
// that pastes a token, with a ceiling the current catalogue accepts.
func adoEntraBlockJSON() string {
	ceiling, _ := json.Marshal(adoTestCeiling())
	return `{"tenant_id":"0f2c1f1e-9d3a-4b8c-8f2d-1a2b3c4d5e6f","client_id":"7a6b5c4d-3e2f-4a1b-9c8d-7e6f5a4b3c2d","capability_ceiling":` + string(ceiling) + `}`
}

func adoOwnPATBlock() string {
	ceiling, _ := json.Marshal(adoTestCeiling())
	return `{"token_mode":"own_pat","capability_ceiling":` + string(ceiling) + `}`
}

// TestSharedADOLanesRefusedAtBothWriteDoors (#1429): the retired shared pat and
// ssh lanes on an Azure DevOps row are a 400 from the console door AND the
// MDM/CLI site-config door, whatever else the row says — including a disabled
// row, and a row with no lanes at all, which used to read as the legacy pat and
// ssh lanes. The refused write never reaches the store.
func TestSharedADOLanesRefusedAtBothWriteDoors(t *testing.T) {
	adoRefusal := func(lane types.GitLane, reason string) string {
		return fmt.Sprintf(providers400Lane, string(lane), "azure_devops", reason)
	}
	emptyLanes := fmt.Sprintf(providers400ADOLanes, 0, string(types.GitLaneEntra), string(types.GitLanePAT))
	const services, server = `"https://dev.azure.com/acme"`, `"https://tfs.corp.example/acme"`
	for _, tc := range []struct{ name, row, want string }{
		{"Services, pat only", `{"id":"ado","kind":"azure_devops","base_urls":[` + services + `],"lanes":["pat"]}`,
			adoRefusal(types.GitLanePAT, adoPATRetired)},
		{"Services, ssh only", `{"id":"ado","kind":"azure_devops","base_urls":[` + services + `],"lanes":["ssh"]}`,
			adoRefusal(types.GitLaneSSH, adoSSHRetired)},
		{"Services, both", `{"id":"ado","kind":"azure_devops","base_urls":[` + services + `],"lanes":["pat","ssh"]}`,
			adoRefusal(types.GitLaneSSH, adoSSHRetired)},
		{"Services, both and a shared source named outright", `{"id":"ado","kind":"azure_devops","base_urls":[` + services + `],"lanes":["pat","ssh"],"credential_source":"shared"}`,
			adoRefusal(types.GitLaneSSH, adoSSHRetired)},
		{"Services, both plus entra", `{"id":"ado","kind":"azure_devops","base_urls":[` + services + `],"lanes":["pat","ssh","entra"],"credential_source":"per_user","entra":` + adoEntraBlockJSON() + `}`,
			adoRefusal(types.GitLaneSSH, adoSSHRetired)},
		{"Services, pat plus entra (a token lane Services does not have)", `{"id":"ado","kind":"azure_devops","base_urls":[` + services + `],"lanes":["entra","pat"],"credential_source":"per_user","entra":` + adoEntraBlockJSON() + `}`,
			adoRefusal(types.GitLanePAT, adoPATHosted)},
		{"Services, per_user pat without the entra lane", `{"id":"ado","kind":"azure_devops","base_urls":[` + services + `],"lanes":["pat"],"credential_source":"per_user"}`,
			adoRefusal(types.GitLanePAT, adoPATHosted)},
		{"Services, no lanes", `{"id":"ado","kind":"azure_devops","base_urls":[` + services + `]}`, emptyLanes},
		{"Services, an empty lane list", `{"id":"ado","kind":"azure_devops","base_urls":[` + services + `],"lanes":[]}`, emptyLanes},
		{"Services, no lanes and per_user", `{"id":"ado","kind":"azure_devops","base_urls":[` + services + `],"credential_source":"per_user"}`, emptyLanes},
		{"a disabled Services row is refused too", `{"id":"ado","kind":"azure_devops","disabled":true,"base_urls":[` + services + `],"lanes":["pat","ssh"]}`,
			adoRefusal(types.GitLaneSSH, adoSSHRetired)},
		{"Server, pat only", `{"id":"ado","kind":"azure_devops","base_urls":[` + server + `],"lanes":["pat"]}`,
			adoRefusal(types.GitLanePAT, adoPATRetired)},
		{"Server, pat with a shared source named outright", `{"id":"ado","kind":"azure_devops","base_urls":[` + server + `],"lanes":["pat"],"credential_source":"shared"}`,
			adoRefusal(types.GitLanePAT, adoPATRetired)},
		{"Server, ssh only", `{"id":"ado","kind":"azure_devops","base_urls":[` + server + `],"lanes":["ssh"]}`,
			adoRefusal(types.GitLaneSSH, adoSSHRetired)},
		{"Server, per_user ssh", `{"id":"ado","kind":"azure_devops","base_urls":[` + server + `],"lanes":["ssh"],"credential_source":"per_user"}`,
			adoRefusal(types.GitLaneSSH, adoSSHRetired)},
		{"Server, no lanes", `{"id":"ado","kind":"azure_devops","base_urls":[` + server + `]}`, emptyLanes},
		{"the second row of two is the one refused", `{"id":"ok","kind":"github","base_urls":["https://github.com/acme"]},{"id":"ado","kind":"azure_devops","base_urls":[` + server + `],"lanes":["pat","ssh"]}`,
			adoRefusal(types.GitLaneSSH, adoSSHRetired)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := `{"git":[` + tc.row + `]}`
			for _, door := range []struct{ path, body string }{
				{"/api/v1/workspace-providers", block},
				{"/api/v1/site-config", `{"workspace_providers":` + block + `}`},
			} {
				fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
				srv, _ := newProvidersHarness(t, fake)
				w := do(t, srv, http.MethodPut, door.path, adminToken, door.body)
				if w.Code != http.StatusBadRequest {
					t.Fatalf("PUT %s = %d, want 400; body=%s", door.path, w.Code, w.Body.String())
				}
				var body struct{ Error string }
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !strings.Contains(body.Error, tc.want) {
					t.Errorf("PUT %s refusal = %s, want it to carry %q", door.path, w.Body.String(), tc.want)
				}
				if fake.putSeen != nil {
					t.Errorf("PUT %s: a refused write reached the store", door.path)
				}
			}
		})
	}
}

// TestPerPersonADORowsAreAcceptedAtBothWriteDoors: what the retirement leaves
// an Azure DevOps row is per person — a hosted row's entra lane (own token: no
// tenant, no client) and a Server row's per_user pat lane with no entra block —
// and both save through both doors and read back exactly as written. A Server
// row with an entra block is still refused: Server has no Entra sign-in.
func TestPerPersonADORowsAreAcceptedAtBothWriteDoors(t *testing.T) {
	hosted := `{"id":"ado","kind":"azure_devops","disabled":true,"base_urls":["https://dev.azure.com/acme"],"lanes":["entra"],"credential_source":"per_user","entra":` + adoOwnPATBlock() + `}`
	server := `{"id":"ados","kind":"azure_devops","disabled":true,"base_urls":["https://tfs.corp.example/acme"],"lanes":["pat"],"credential_source":"per_user"}`
	mixed := `{"id":"adom","kind":"azure_devops","base_urls":["https://dev.azure.com/acme","https://tfs.corp.example/acme"],"lanes":["pat"],"credential_source":"per_user"}`
	block := `{"git":[` + hosted + `,` + server + `,` + mixed + `]}`
	for _, door := range []struct{ path, body string }{
		{"/api/v1/workspace-providers", block},
		{"/api/v1/site-config", `{"workspace_providers":` + block + `}`},
	} {
		fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
		srv, _ := newProvidersHarness(t, fake)
		w := do(t, srv, http.MethodPut, door.path, adminToken, door.body)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d, want 200; body=%s", door.path, w.Code, w.Body.String())
		}
		if fake.putSeen == nil || fake.putSeen.WorkspaceProviders == nil || len(fake.putSeen.WorkspaceProviders.Git) != 3 {
			t.Fatalf("PUT %s stored %+v, want the three rows", door.path, fake.putSeen)
		}
		got := fake.putSeen.WorkspaceProviders.Git
		if got[1].Entra != nil || got[1].CredentialSource != types.CredentialSourcePerUser || len(got[1].Lanes) != 1 || got[1].Lanes[0] != types.GitLanePAT {
			t.Errorf("PUT %s: the Server row was stored as %+v", door.path, got[1])
		}
	}

	serverWithEntra := `{"git":[{"id":"ados","kind":"azure_devops","base_urls":["https://tfs.corp.example/acme"],"lanes":["entra"],"credential_source":"per_user","entra":` + adoOwnPATBlock() + `}]}`
	fake := &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}}
	srv, _ := newProvidersHarness(t, fake)
	if w := do(t, srv, http.MethodPut, "/api/v1/workspace-providers", adminToken, serverWithEntra); w.Code != http.StatusBadRequest {
		t.Errorf("a Server row with an entra block = %d, want 400 (Server has no Entra sign-in); body=%s", w.Code, w.Body.String())
	}
}

// TestRetiredADORowsAreWhatMigration0102Writes: the rows migration 0102 writes
// must save through both doors, or the admin who turns one on is refused by the
// state the upgrade left them in. The JSON is the migration's output verbatim
// (internal/db's TestPG_RetireADOShared_* pin the same values), and it names
// the capabilities the per-area catalogue (#1409) calls project_read and
// code_read, so it runs once that catalogue is in.
func TestRetiredADORowsAreWhatMigration0102Writes(t *testing.T) {
	if !adoscope.Capability("project_read").Grantable() || !adoscope.Capability("code_read").Grantable() {
		t.Skip("migration 0102 writes the per-area catalogue's capability names (#1409); this catalogue predates them")
	}
	services := `{"id":"ado","kind":"azure_devops","disabled":true,"base_urls":["https://dev.azure.com/acme"],"lanes":["entra"],"credential_source":"per_user","entra":{"token_mode":"own_pat","capability_ceiling":["project_read","code_read"],"default_profile":[]}}`
	server := `{"id":"ados","kind":"azure_devops","disabled":true,"base_urls":["https://tfs.corp.example/acme"],"lanes":["pat"],"credential_source":"per_user"}`
	block := `{"git":[` + services + `,` + server + `]}`
	for _, door := range []struct{ path, body string }{
		{"/api/v1/workspace-providers", block},
		{"/api/v1/site-config", `{"workspace_providers":` + block + `}`},
	} {
		srv, _ := newProvidersHarness(t, &fakeProvidersStore{fakeSiteConfigStore: &fakeSiteConfigStore{}})
		if w := do(t, srv, http.MethodPut, door.path, adminToken, door.body); w.Code != http.StatusOK {
			t.Errorf("PUT %s of the migrated rows = %d, want 200; body=%s", door.path, w.Code, w.Body.String())
		}
	}
}
