// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// entraRow is an Azure DevOps row on the entra lane with a block that
// validates, so a case shows only what it varies.
func entraRow(mutate func(*types.GitProvider)) types.GitProvider {
	row := types.GitProvider{
		ID:       "ado",
		Kind:     types.GitProviderAzureDevOps,
		BaseURLs: []string{"https://dev.azure.com/acme"},
		Lanes:    []types.GitLane{types.GitLaneEntra},
		Entra: &types.ADOEntraConfig{
			TenantID:          "0f2c1f1e-9d3a-4b8c-8f2d-1a2b3c4d5e6f",
			ClientID:          "7a6b5c4d-3e2f-4a1b-9c8d-7e6f5a4b3c2d",
			CapabilityCeiling: []adoscope.Capability{adoscope.CapRead, adoscope.CapCodeWrite, adoscope.CapPR},
		},
	}
	if mutate != nil {
		mutate(&row)
	}
	return row
}

// withSource sets the credential source on a one-row block, so the table below
// can vary it without a second builder.
func withSource(p *types.WorkspaceProviders, src types.CredentialSource) *types.WorkspaceProviders {
	p.Git[0].CredentialSource = src
	return p
}

// TestValidateProviderEntra is the entra lane's write boundary.
func TestValidateProviderEntra(t *testing.T) {
	block := func(rows ...types.GitProvider) *types.WorkspaceProviders {
		return &types.WorkspaceProviders{Git: rows}
	}
	for _, tc := range []struct {
		name    string
		block   *types.WorkspaceProviders
		wantErr bool
	}{
		{"dev.azure.com with a block", block(entraRow(nil)), false},
		{"a legacy visualstudio.com host", block(entraRow(func(r *types.GitProvider) {
			r.BaseURLs = []string{"https://acme.visualstudio.com"}
		})), false},
		{"the lane beside the legacy lanes", block(entraRow(func(r *types.GitProvider) {
			r.Lanes = []types.GitLane{types.GitLanePAT, types.GitLaneEntra}
		})), false},

		{"an Azure DevOps SERVER host cannot carry the lane", block(entraRow(func(r *types.GitProvider) {
			r.BaseURLs = []string{"https://tfs.corp.example/acme"}
		})), true},
		{"one hosted address does not licence a self-hosted one", block(entraRow(func(r *types.GitProvider) {
			r.BaseURLs = []string{"https://dev.azure.com/acme", "https://tfs.corp.example/acme"}
		})), true},
		{"the lane with no block", block(entraRow(func(r *types.GitProvider) {
			r.Entra = nil
		})), true},
		{"a block with no lane", block(types.GitProvider{
			ID: "ado", Kind: types.GitProviderAzureDevOps,
			BaseURLs: []string{"https://dev.azure.com/acme"},
			Entra: &types.ADOEntraConfig{
				TenantID:          "0f2c1f1e-9d3a-4b8c-8f2d-1a2b3c4d5e6f",
				ClientID:          "7a6b5c4d-3e2f-4a1b-9c8d-7e6f5a4b3c2d",
				CapabilityCeiling: []adoscope.Capability{adoscope.CapRead},
			},
		}), true},

		{"a tenant that is not a GUID", block(entraRow(func(r *types.GitProvider) {
			r.Entra.TenantID = "contoso.onmicrosoft.com"
		})), true},
		{"the multi-tenant alias is the opposite of pinning a tenant", block(entraRow(func(r *types.GitProvider) {
			r.Entra.TenantID = "common"
		})), true},
		{"the organizations alias too", block(entraRow(func(r *types.GitProvider) {
			r.Entra.TenantID = "organizations"
		})), true},
		{"an empty tenant", block(entraRow(func(r *types.GitProvider) {
			r.Entra.TenantID = ""
		})), true},
		{"a client that is not a GUID", block(entraRow(func(r *types.GitProvider) {
			r.Entra.ClientID = "my-app"
		})), true},
		{"an uppercase GUID is still a GUID", block(entraRow(func(r *types.GitProvider) {
			r.Entra.TenantID = "0F2C1F1E-9D3A-4B8C-8F2D-1A2B3C4D5E6F"
		})), false},
		{"a GUID in braces is not one", block(entraRow(func(r *types.GitProvider) {
			r.Entra.TenantID = "{0f2c1f1e-9d3a-4b8c-8f2d-1a2b3c4d5e6f}"
		})), true},

		{"an empty ceiling has no honest reading", block(entraRow(func(r *types.GitProvider) {
			r.Entra.CapabilityCeiling = nil
		})), true},
		{"an invented capability in the ceiling", block(entraRow(func(r *types.GitProvider) {
			r.Entra.CapabilityCeiling = []adoscope.Capability{adoscope.CapRead, "superuser"}
		})), true},
		{"a DENIED area is not a capability anyone can be granted", block(entraRow(func(r *types.GitProvider) {
			r.Entra.CapabilityCeiling = []adoscope.Capability{adoscope.CapRead, adoscope.CapDeniedTokens}
		})), true},
		{"the unclassified-write placeholder is not grantable either", block(entraRow(func(r *types.GitProvider) {
			r.Entra.CapabilityCeiling = []adoscope.Capability{adoscope.CapUnclassifiedWrite}
		})), true},

		{"a profile inside the ceiling", block(entraRow(func(r *types.GitProvider) {
			r.Entra.DefaultProfile = []adoscope.Capability{adoscope.CapRead, adoscope.CapCodeWrite}
		})), false},
		{"a profile outside the ceiling", block(entraRow(func(r *types.GitProvider) {
			r.Entra.DefaultProfile = []adoscope.Capability{adoscope.CapRead, adoscope.CapPolicyBypass}
		})), true},
		{"an invented capability in the profile", block(entraRow(func(r *types.GitProvider) {
			r.Entra.DefaultProfile = []adoscope.Capability{"superuser"}
		})), true},
		{"an EMPTY profile reads as the read profile, which the ceiling admits", block(entraRow(func(r *types.GitProvider) {
			r.Entra.CapabilityCeiling = []adoscope.Capability{adoscope.CapRead}
			r.Entra.DefaultProfile = nil
		})), false},
		{"an empty profile against a ceiling that cannot read is a contradiction", block(entraRow(func(r *types.GitProvider) {
			r.Entra.CapabilityCeiling = []adoscope.Capability{adoscope.CapCodeWrite}
			r.Entra.DefaultProfile = nil
		})), true},

		{"the bearer token mode", block(entraRow(func(r *types.GitProvider) {
			r.Entra.TokenMode = types.ADOTokenModeBearer
		})), false},
		{"the minted-PAT token mode is accepted", block(entraRow(func(r *types.GitProvider) {
			r.Entra.TokenMode = types.ADOTokenModeMintedPAT
		})), false},
		{"an invented token mode", block(entraRow(func(r *types.GitProvider) {
			r.Entra.TokenMode = "oauth"
		})), true},

		{"per_user on the entra lane", block(entraRow(func(r *types.GitProvider) {
			r.CredentialSource = types.CredentialSourcePerUser
		})), false},
		{"per_user without the lane has no per-person path", withSource(block(adoRow("ado", false, "https://dev.azure.com/acme")), types.CredentialSourcePerUser), true},
		{"shared without the lane is today's behaviour", withSource(block(adoRow("ado", false, "https://dev.azure.com/acme")), types.CredentialSourceShared), false},
		{"an invented credential source", withSource(block(githubRow("gh", false, "https://github.com/acme")), "borrowed"), true},
		{"an absent credential source on a legacy row", block(githubRow("gh", false, "https://github.com/acme")), false},

		{"the lane is refused on a github row's host", block(types.GitProvider{
			ID: "gh", Kind: types.GitProviderGitHub, BaseURLs: []string{"https://github.com/acme"},
			Lanes: []types.GitLane{types.GitLaneEntra},
		}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWorkspaceProviders(normalizeWorkspaceProviders(tc.block))
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateWorkspaceProviders() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestEntraRefusalsGoThroughTheConstants pins that the refusals are the DRAFT
// constants rather than hand-typed prose, so the canon sitting swaps one file
// and no test.
func TestEntraRefusalsGoThroughTheConstants(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  types.GitProvider
		want string
	}{
		{"host", entraRow(func(r *types.GitProvider) {
			r.BaseURLs = []string{"https://tfs.corp.example/acme"}
		}), "does not accept Entra tokens"},
		{"no block", entraRow(func(r *types.GitProvider) { r.Entra = nil }), "needs an entra block"},
		{"profile outside the ceiling", entraRow(func(r *types.GitProvider) {
			r.Entra.DefaultProfile = []adoscope.Capability{adoscope.CapPolicyBypass}
		}), "outside capability_ceiling"},
		{"per_user", func() types.GitProvider {
			r := adoRow("ado", false, "https://dev.azure.com/acme")
			r.CredentialSource = types.CredentialSourcePerUser
			return r
		}(), "needs the \"entra\" lane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWorkspaceProviders(&types.WorkspaceProviders{Git: []types.GitProvider{tc.row}})
			if err == nil {
				t.Fatal("validateWorkspaceProviders() = nil, want a refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestEmptyLanesDoesNotAdmitEntra is THE upgrade pin for this change.
//
// laneAllowed expands an empty Lanes list into permission, so adding a lane to
// the closed set without guarding that site would have opted every stored row
// on every install into the new lane — an opening no admin authored. A stored
// row keeps exactly the three lanes it had, and reaches the entra lane only by
// NAMING it.
func TestEmptyLanesDoesNotAdmitEntra(t *testing.T) {
	stored := adoRow("ado", false, "https://dev.azure.com/acme") // no Lanes: the 0.7.9 shape
	for _, lane := range types.LegacyGitLanes {
		if !laneAllowed(stored, lane) {
			t.Errorf("laneAllowed(empty, %q) = false — an empty list must keep admitting the legacy lanes", lane)
		}
	}
	if laneAllowed(stored, types.GitLaneEntra) {
		t.Fatal("an empty Lanes list admits the entra lane — every stored row was just opted into a lane nobody wrote")
	}
	named := entraRow(nil)
	if !laneAllowed(named, types.GitLaneEntra) {
		t.Fatal("a row that NAMES the entra lane is refused it")
	}
	if laneAllowed(named, types.GitLanePAT) {
		t.Error("a row naming only the entra lane permits the pat lane — the field narrows")
	}
	// The closed set and the legacy set must stay different: the day they are
	// equal again, this whole guard is a no-op.
	if len(types.LegacyGitLanes) == len(types.ClosedGitLaneList()) {
		t.Fatal("LegacyGitLanes covers the whole closed set — the empty-lanes guard no longer guards anything")
	}
	if slices.Contains(types.LegacyGitLanes, types.GitLaneEntra) {
		t.Fatal("the entra lane is in LegacyGitLanes — that list is frozen at the lanes an absent field meant")
	}
}

// storedProviderBlock is a workspace-providers document as 0.7.9 wrote it:
// rows with no credential source, no entra block, and one row with no lanes at
// all. It is a STRING and not a struct on purpose — a struct literal would be
// rebuilt by the same code the test is checking.
const storedProviderBlock = `{"git":[` +
	`{"id":"gh","kind":"github","base_urls":["https://github.com/acme"],"lanes":["app","pat"]},` +
	`{"id":"ado","kind":"azure_devops","base_urls":["https://dev.azure.com/acme"]},` +
	`{"id":"ghes","kind":"github","disabled":true,"base_urls":["https://git.corp.example/acme"],"lanes":["pat"]}` +
	`],"storage":{"ephemeral":{"default_disk_mib":4096,"max_disk_mib":20480}}}`

// TestStoredProviderBlockRoundTripsByteIdentical proves the row shape is
// BACKWARD COMPATIBLE in the only way that matters here: a document written
// before this change reads back, re-serializes to the same bytes, gains no
// lane and gains no Entra block.
//
// The re-serialization is the half that catches a field added without
// omitempty, which would rewrite every stored document the first time site
// config was saved — and, because site config is delivered by MDM, would
// rewrite it on every managed desktop.
func TestStoredProviderBlockRoundTripsByteIdentical(t *testing.T) {
	var block types.WorkspaceProviders
	if err := json.Unmarshal([]byte(storedProviderBlock), &block); err != nil {
		t.Fatalf("unmarshal the stored block: %v", err)
	}
	again, err := json.Marshal(block)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(again) != storedProviderBlock {
		t.Fatalf("a stored block did not round-trip:\n got %s\nwant %s", again, storedProviderBlock)
	}
	if err := validateWorkspaceProviders(&block); err != nil {
		t.Fatalf("a stored block no longer validates: %v", err)
	}
	for _, row := range block.Git {
		if row.Entra != nil {
			t.Errorf("row %q gained an entra block", row.ID)
		}
		if row.CredentialSource != "" {
			t.Errorf("row %q gained credential source %q", row.ID, row.CredentialSource)
		}
		if slices.Contains(row.Lanes, types.GitLaneEntra) {
			t.Errorf("row %q gained the entra lane", row.ID)
		}
		if laneAllowed(row, types.GitLaneEntra) {
			t.Errorf("row %q admits the entra lane", row.ID)
		}
		if !row.Entra.RESTAPIEnabled() {
			t.Errorf("row %q reads as REST-disabled — rest_api defaults to true", row.ID)
		}
	}
	// The ETag both doors compute is over these bytes, so an unchanged
	// document must keep an unchanged ETag — otherwise every open console
	// would 412 once on upgrade.
	if before, after := computeETag(mustUnmarshalProviders(t, storedProviderBlock)), computeETag(block); before != after {
		t.Fatalf("ETag changed for an unchanged document: %q -> %q", before, after)
	}
}

// mustUnmarshalProviders is the second decode the ETag comparison needs.
func mustUnmarshalProviders(t *testing.T, raw string) types.WorkspaceProviders {
	t.Helper()
	var block types.WorkspaceProviders
	if err := json.Unmarshal([]byte(raw), &block); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return block
}

// TestEntraBlockRoundTripsThroughBothDoors proves the new block survives the
// wire in the shape it was written, including the pointer-valued rest_api
// whose whole reason for being a pointer is that its default is true.
func TestEntraBlockRoundTripsThroughBothDoors(t *testing.T) {
	off := false
	row := entraRow(func(r *types.GitProvider) {
		r.CredentialSource = types.CredentialSourcePerUser
		r.Entra.DefaultProfile = adoscope.ProfileRead()
		r.Entra.TokenMode = types.ADOTokenModeMintedPAT
		r.Entra.RESTAPI = &off
	})
	raw, err := json.Marshal(types.WorkspaceProviders{Git: []types.GitProvider{row}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back types.WorkspaceProviders
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	got := back.Git[0]
	if got.CredentialSource != types.CredentialSourcePerUser {
		t.Errorf("credential_source = %q", got.CredentialSource)
	}
	if got.Entra == nil {
		t.Fatal("the entra block did not survive the wire")
	}
	if got.Entra.RESTAPIEnabled() {
		t.Error("rest_api:false read back as enabled")
	}
	if !slices.Equal(got.Entra.CapabilityCeiling, row.Entra.CapabilityCeiling) {
		t.Errorf("capability_ceiling = %v, want %v", got.Entra.CapabilityCeiling, row.Entra.CapabilityCeiling)
	}
	if !strings.Contains(string(raw), `"rest_api":false`) {
		t.Errorf("rest_api did not serialize: %s", raw)
	}
	if err := validateWorkspaceProviders(&back); err != nil {
		t.Fatalf("the round-tripped row no longer validates: %v", err)
	}
}

// TestEntraProfileDefaultsToRead pins the one spelling of the default: a row
// that named no profile gets the catalogue's read-only one, so no call site
// has to decide what "unset" means.
func TestEntraProfileDefaultsToRead(t *testing.T) {
	cfg := entraRow(nil).Entra
	if !slices.Equal(cfg.Profile(), adoscope.ProfileRead()) {
		t.Fatalf("Profile() = %v, want %v", cfg.Profile(), adoscope.ProfileRead())
	}
	cfg.DefaultProfile = []adoscope.Capability{adoscope.CapRead, adoscope.CapPR}
	if !slices.Equal(cfg.Profile(), cfg.DefaultProfile) {
		t.Fatalf("Profile() = %v, want the written profile %v", cfg.Profile(), cfg.DefaultProfile)
	}
	var absent *types.ADOEntraConfig
	if !slices.Equal(absent.Profile(), adoscope.ProfileRead()) {
		t.Fatalf("a row with no block answers %v", absent.Profile())
	}
	if !absent.RESTAPIEnabled() {
		t.Error("a row with no block reads as REST-disabled")
	}
}
