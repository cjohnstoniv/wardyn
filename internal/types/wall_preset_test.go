// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
)

// The built-ins are Open, Read-only and Sealed in that order, version 1, read-only by construction:
// Open names nothing, and every list Read-only and Sealed keep is a grantable read, so they remove
// writes only. (internal/api's TestWallPresetBuiltinsPassTheWallsValidator holds them to the walls
// validator too.)
func TestWallPresetBuiltins(t *testing.T) {
	got := BuiltinWallPresets()
	ids := []string{}
	for _, p := range got {
		ids = append(ids, p.ID)
		if p.Scope != WallPresetScopeBuiltin || p.Version != 1 || p.Name == "" || p.Description == "" {
			t.Errorf("built-in %q = %+v", p.ID, p)
		}
		if strings.ToLower(p.Name) != p.ID || !IsBuiltinWallPresetName(strings.ToUpper(p.Name)) {
			t.Errorf("built-in %q: the name %q is not its id in another case", p.ID, p.Name)
		}
		if p.Owner().Valid() {
			t.Errorf("built-in %q has a valid template owner; TemplateAuthorize must never allow one", p.ID)
		}
	}
	if !slices.Equal(ids, []string{WallPresetOpen, WallPresetReadOnly, WallPresetSealed}) {
		t.Fatalf("built-in ids = %v", ids)
	}
	if !got[0].Spec.Empty() || got[1].Spec.Empty() || got[2].Spec.Empty() {
		t.Fatal("Open must name nothing, and Read-only and Sealed something")
	}
	for _, p := range got[1:] {
		for _, c := range *p.Spec.GitHubCapabilities {
			if !c.Grantable() || (!strings.HasSuffix(string(c), "_read") && c != ghscope.CapIdentity) {
				t.Errorf("%s keeps GitHub %q, which is not a grantable read", p.ID, c)
			}
		}
		for _, c := range *p.Spec.AzureDevOpsCapabilities {
			if !c.Grantable() || !strings.HasSuffix(string(c), "_read") {
				t.Errorf("%s keeps Azure DevOps %q, which is not a grantable read", p.ID, c)
			}
		}
	}
	if n := len(*got[1].Spec.GitHubCapabilities); n != 6 {
		t.Errorf("Read-only keeps %d GitHub reads, want all 6", n)
	}
	if n := len(*got[1].Spec.AzureDevOpsCapabilities); n != 11 {
		t.Errorf("Read-only keeps %d Azure DevOps reads, want all 11", n)
	}
	if !slices.Contains(*got[1].Spec.AzureDevOpsCapabilities, adoscope.CapIdentityRead) {
		t.Error("Read-only drops Azure DevOps identity_read")
	}
}

// Sealed narrows every knob Read-only names at least as far, and more: no sites beyond the run's
// setup, an unlisted site refused, and every desktop transfer blocked.
func TestWallPresetSealedNestsInReadOnly(t *testing.T) {
	ro, _ := BuiltinWallPreset(WallPresetReadOnly)
	sealed, _ := BuiltinWallPreset(WallPresetSealed)
	if !slices.Equal(*ro.Spec.GitHubCapabilities, *sealed.Spec.GitHubCapabilities) ||
		!slices.Equal(*ro.Spec.AzureDevOpsCapabilities, *sealed.Spec.AzureDevOpsCapabilities) {
		t.Fatal("Sealed keeps capabilities Read-only does not")
	}
	if sealed.Spec.AllowedDomains == nil || len(*sealed.Spec.AllowedDomains) != 0 {
		t.Errorf("Sealed allowed_domains = %v, want []", sealed.Spec.AllowedDomains)
	}
	if sealed.Spec.FirstUseApproval == nil || *sealed.Spec.FirstUseApproval != FirstUseAlwaysDeny {
		t.Errorf("Sealed first_use_approval = %v", sealed.Spec.FirstUseApproval)
	}
	want := DesktopControls{PasteIn: TransferBlocked, CopyOut: TransferBlocked, Upload: TransferBlocked, Download: TransferBlocked}
	if sealed.Spec.Desktop == nil || *sealed.Spec.Desktop != want {
		t.Errorf("Sealed desktop = %+v", sealed.Spec.Desktop)
	}
	ov := sealed.Spec.Overlay()
	if ov.AllowAllEgress == nil || *ov.AllowAllEgress {
		t.Error("Sealed's overlay leaves open egress on")
	}
}

// Each call is a fresh copy: changing one never changes the next caller's built-in.
func TestWallPresetBuiltinsAreFreshCopies(t *testing.T) {
	a, _ := BuiltinWallPreset(WallPresetSealed)
	*a.Spec.AllowedDomains = append(*a.Spec.AllowedDomains, "example.com")
	(*a.Spec.GitHubCapabilities)[0] = ghscope.CapCodeWrite
	b, _ := BuiltinWallPreset(WallPresetSealed)
	if len(*b.Spec.AllowedDomains) != 0 || (*b.Spec.GitHubCapabilities)[0] != ghscope.CapCodeRead {
		t.Fatalf("a caller's change reached the next built-in: %+v", b.Spec)
	}
	if _, ok := BuiltinWallPreset("Sealed"); ok {
		t.Error("an id lookup folded case")
	}
	if IsBuiltinWallPresetName("Sealed team") || !IsBuiltinWallPresetName(" read-only ") {
		t.Error("IsBuiltinWallPresetName matched a different name, or missed a padded one")
	}
}

// The owner subject never reaches the wire; the owner maps onto the templates' shape for authority.
func TestWallPresetWireAndOwner(t *testing.T) {
	p := WallPreset{ID: "x", Scope: WallPresetScopePerson, Name: "mine", Version: 2, OwnerID: "alice@example.com"}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "alice") || strings.Contains(string(raw), "owner") {
		t.Fatalf("wire = %s", raw)
	}
	if o := p.Owner(); o != (TemplateOwner{Scope: TemplateScopePerson, Person: "alice@example.com"}) || !o.Valid() {
		t.Fatalf("person owner = %+v", o)
	}
	g := WallPreset{Scope: WallPresetScopeGroup, GroupID: "eng"}
	if o := g.Owner(); o.Scope != TemplateScopeGroup || o.Group != "eng" || o.Person != "" {
		t.Fatalf("group owner = %+v", o)
	}
	if o := (WallPreset{Scope: WallPresetScopeOrg}).Owner(); !o.Valid() || o.Scope != TemplateScopeOrg {
		t.Fatalf("org owner = %+v", o)
	}
}
