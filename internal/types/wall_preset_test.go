// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
)

// The built-ins are Open, Read-only, Sealed and No network in that order, version 1, read-only by construction:
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
		if strings.ReplaceAll(strings.ToLower(p.Name), " ", "-") != p.ID || !IsBuiltinWallPresetName(strings.ToUpper(p.Name)) ||
			!IsBuiltinWallPresetName(p.ID) {
			t.Errorf("built-in %q: the name %q is not its id in another case", p.ID, p.Name)
		}
		if p.Owner().Valid() {
			t.Errorf("built-in %q has a valid template owner; TemplateAuthorize must never allow one", p.ID)
		}
	}
	if !slices.Equal(ids, []string{WallPresetOpen, WallPresetReadOnly, WallPresetSealed, WallPresetNoNetwork}) {
		t.Fatalf("built-in ids = %v", ids)
	}
	if !got[0].Spec.Empty() || got[1].Spec.Empty() || got[2].Spec.Empty() || got[3].Spec.Empty() {
		t.Fatal("Open must name nothing, and Read-only, Sealed and No network something")
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
	// Read-only, Sealed and No network refuse every push, and their overlays carry it to the run's push rules.
	for _, p := range got[1:] {
		if !p.Spec.DeniesAllPushes() {
			t.Errorf("%s does not refuse every push", p.ID)
		}
		if ov := p.Spec.Overlay(); ov.PushRules == nil || ov.PushRules.DenyAll == nil || !*ov.PushRules.DenyAll {
			t.Errorf("%s's overlay drops push_rules.deny_all: %+v", p.ID, ov.PushRules)
		}
		// No network's description names Sealed, which refuses every push, and a run with no network
		// reaches no code host, so the note on secrets does not apply to it.
		if p.ID == WallPresetNoNetwork {
			if !strings.HasPrefix(p.Description, "Sealed, ") {
				t.Errorf("%s's description %q does not start from Sealed", p.ID, p.Description)
			}
			continue
		}
		if !strings.Contains(p.Description, "push") {
			t.Errorf("%s's description %q does not say it refuses pushes", p.ID, p.Description)
		}
		if !strings.HasSuffix(p.Description, " "+PushDenyAllSecretsNote) {
			t.Errorf("%s's description %q does not end with the note on secrets a person adds", p.ID, p.Description)
		}
	}
	if got[0].Spec.DeniesAllPushes() {
		t.Error("Open refuses every push")
	}
	if strings.Contains(got[0].Description, PushDenyAllSecretsNote) {
		t.Error("Open, which refuses no push, carries the note on secrets")
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
	if ro.Spec.DeniesAllPushes() && !sealed.Spec.DeniesAllPushes() {
		t.Error("Sealed pushes where Read-only refuses")
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
	*a.Spec.PushRules.DenyAll = false
	b, _ := BuiltinWallPreset(WallPresetSealed)
	if len(*b.Spec.AllowedDomains) != 0 || (*b.Spec.GitHubCapabilities)[0] != ghscope.CapCodeRead || !b.Spec.DeniesAllPushes() {
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

// No network's spec is Sealed's: what it adds is not a walls knob (the seal, the proxy switch, the
// create refusal and the approval refusal key on its id).
func TestWallPresetNoNetworkIsSealedsSpec(t *testing.T) {
	sealed, _ := BuiltinWallPreset(WallPresetSealed)
	nn, ok := BuiltinWallPreset(WallPresetNoNetwork)
	if !ok || nn.Name != "No network" || !reflect.DeepEqual(nn.Spec, sealed.Spec) {
		t.Fatalf("no-network = %+v, want Sealed's spec", nn)
	}
	*nn.Spec.AllowedDomains = append(*nn.Spec.AllowedDomains, "example.com")
	if again, _ := BuiltinWallPreset(WallPresetSealed); len(*again.Spec.AllowedDomains) != 0 {
		t.Fatal("No network shares Sealed's lists")
	}
	if !IsBuiltinWallPresetName("no network") || !IsBuiltinWallPresetName("NO-NETWORK") {
		t.Error("a stored preset could take No network's name")
	}
}
