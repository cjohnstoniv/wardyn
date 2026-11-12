// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fifteenADOCaps is a 15-capability list; the first five are what govWarnRow's ceiling allows.
func fifteenADOCaps() []adoscope.Capability {
	return []adoscope.Capability{
		adoscope.CapCodeRead, adoscope.CapCodeWrite, adoscope.CapPR, adoscope.CapWorkRead, adoscope.CapWorkWrite,
		adoscope.CapWikiRead, adoscope.CapWikiWrite, adoscope.CapBuildRead, adoscope.CapBuildExecute,
		adoscope.CapReleaseRead, adoscope.CapReleaseExecute, adoscope.CapPackagingRead, adoscope.CapTestRead,
		adoscope.CapProjectRead, adoscope.CapLibraryRead,
	}
}

// govWarnRow is an Entra-lane Azure DevOps provider row whose ceiling is the first n of fifteenADOCaps.
func govWarnRow(id string, n int) types.GitProvider {
	return types.GitProvider{
		ID: id, Kind: types.GitProviderAzureDevOps,
		Entra: &types.ADOEntraConfig{CapabilityCeiling: fifteenADOCaps()[:n]},
	}
}

// A profile whose explicit Azure DevOps list reaches past a provider row's capability_ceiling says so
// at the write, once per row, standalone or composed. It is a warning only: dispatch is what refuses.
func TestGovernanceUnitWriteWarnsPastRowCeiling(t *testing.T) {
	site := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{
		govWarnRow("narrow-row", 5), govWarnRow("wide-row", 15),
		{ID: "github-row", Kind: types.GitProviderGitHub},
	}}}
	h := newHarness(t)
	srv := New(baseTestConfig(h, &rubricSiteStore{govCrudStore: &govCrudStore{}, site: site}))
	warn := func(res *ResolvedUnit) []string { return srv.governanceADOCeilingWarnings(context.Background(), res) }

	t.Run("a standalone list of 15 over a row of 5", func(t *testing.T) {
		root := govCovStandalone(govCovBase, "root")
		root.Ceiling.AzureDevOpsCapabilities = fifteenADOCaps()
		got := warn(srv.resolveForDiff([]types.GovernanceUnit{root}, govCovBase))
		if len(got) != 1 || !strings.Contains(got[0], `"narrow-row"`) || !strings.Contains(got[0], "lists 10 Azure DevOps") {
			t.Fatalf("warnings = %q, want one for narrow-row naming 10 capabilities", got)
		}
		if !strings.Contains(got[0], adoscope.ShortLabel(adoscope.CapLibraryRead)) {
			t.Errorf("warning %q does not name the capabilities by console name", got[0])
		}
	})
	t.Run("an overlay that narrows to 10 still reaches past the row of 5", func(t *testing.T) {
		root := govCovStandalone(govCovBase, "root")
		root.Ceiling.AzureDevOpsCapabilities = fifteenADOCaps()
		ten := fifteenADOCaps()[:10]
		child := types.GovernanceUnit{ID: govCovPID, Name: "child", ParentID: &root.ID,
			Overlay: &types.CeilingOverlay{AzureDevOpsCapabilities: &ten}}
		got := warn(srv.resolveForDiff([]types.GovernanceUnit{root, child}, govCovPID))
		if len(got) != 1 || !strings.Contains(got[0], `"narrow-row"`) || !strings.Contains(got[0], "lists 5 Azure DevOps") {
			t.Fatalf("warnings = %q, want one for narrow-row naming 5 capabilities", got)
		}
	})
	t.Run("two rows past, two warnings", func(t *testing.T) {
		two := types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{Git: []types.GitProvider{
			govWarnRow("a", 5), govWarnRow("b", 10),
		}}}
		s := New(baseTestConfig(newHarness(t), &rubricSiteStore{govCrudStore: &govCrudStore{}, site: two}))
		got := s.governanceADOCeilingWarnings(context.Background(),
			&ResolvedUnit{Ceiling: types.RunPolicySpec{AzureDevOpsCapabilities: fifteenADOCaps()}})
		if len(got) != 2 || !strings.Contains(got[0], `"a"`) || !strings.Contains(got[1], `"b"`) {
			t.Errorf("warnings = %q, want one each for rows a and b", got)
		}
	})
	t.Run("a list inside every ceiling, or no list, says nothing", func(t *testing.T) {
		inside := &ResolvedUnit{Ceiling: types.RunPolicySpec{AzureDevOpsCapabilities: fifteenADOCaps()[:3]}}
		if got := warn(inside); len(got) != 0 {
			t.Errorf("warnings = %q, want none for a list inside the ceiling", got)
		}
		if got := warn(&ResolvedUnit{}); len(got) != 0 {
			t.Errorf("warnings = %q, want none without a list", got)
		}
		if got := warn(nil); len(got) != 0 {
			t.Errorf("warnings = %q, want none without a resolved profile", got)
		}
	})
	t.Run("a site config that cannot be read says nothing", func(t *testing.T) {
		s := New(baseTestConfig(newHarness(t), &govWarnBadSiteStore{govCrudStore: &govCrudStore{}}))
		got := s.governanceADOCeilingWarnings(context.Background(),
			&ResolvedUnit{Ceiling: types.RunPolicySpec{AzureDevOpsCapabilities: fifteenADOCaps()}})
		if len(got) != 0 {
			t.Errorf("warnings = %q, want none when the read fails", got)
		}
	})
}

// govWarnBadSiteStore is the fake governance store whose site config read fails.
type govWarnBadSiteStore struct{ *govCrudStore }

func (*govWarnBadSiteStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, context.DeadlineExceeded
}

// The control warnings of a standalone profile: the one deny-by-default limit, and each control the
// profile leaves out or sets weaker than the deployment default.
func TestGovernanceControlWarnings(t *testing.T) {
	deployment := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		Desktop:             &types.DesktopControls{CopyOut: types.TransferAllowed},
		Recording:           types.RecordingScreenActions,
		ToolRules:           []types.ToolRule{{Tool: "WebFetch", Effect: types.ToolDeny}},
		AutoStopAfterSec:    900,
		LLMInspection:       &types.LLMInspectionSpec{Mode: "block", DetectSecrets: true},
		Resources:           &types.ResourceLimits{CPUMillis: 2000, MemoryMiB: 4096}, // a size of its own: the sandbox size warning is not one of these
	}
	has := func(got []string, want string) bool {
		return slices.ContainsFunc(got, func(w string) bool { return strings.Contains(w, want) })
	}

	t.Run("limit false names the local run it refuses", func(t *testing.T) {
		got := governanceOmissionWarnings(deployment, deployment, types.GovernanceLimits{})
		if !has(got, "local_self_defined_components off") || !has(got, "cannot start a local run with a component they define themselves") ||
			!has(got, "Leaving it off is fine if that is intended") || !has(got, "(an overlay cannot turn it on)") {
			t.Errorf("warnings = %q, want the local_self_defined_components sentence", got)
		}
		if got := governanceOmissionWarnings(deployment, deployment, types.GovernanceLimits{LocalSelfDefinedComponents: true}); len(got) != 0 {
			t.Errorf("warnings = %q, want none with the limit on and a faithful copy", got)
		}
	})
	t.Run("each weaker control is named", func(t *testing.T) {
		profile := types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			Desktop:             &types.DesktopControls{CopyOut: types.TransferAllowed, Download: types.TransferAllowed},
			Recording:           types.RecordingOff,
			LLMInspection:       &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true},
		}
		got := governanceOmissionWarnings(profile, deployment, types.GovernanceLimits{LocalSelfDefinedComponents: true})
		for _, want := range []string{
			"desktop rules allow download, which the deployment default blocks",
			`recording level "off" is LOWER than the deployment default's "screen_actions"`,
			"omits 1 tool rule(s) the deployment default carries (WebFetch)",
			"no auto_stop_after_sec while the deployment default stops idle sandboxes after 900 s — this profile sets no idle cap, so a run stops only if its own policy or pool says so",
			`llm_inspection mode "alert" is WEAKER than the deployment default's "block"`,
		} {
			if !has(got, want) {
				t.Errorf("warnings %q do not contain %q", got, want)
			}
		}
		if has(got, "copy_out") {
			t.Errorf("warnings %q name copy_out, which the deployment default allows too", got)
		}
	})
	t.Run("a profile with no recording level says it sets no minimum", func(t *testing.T) {
		profile := deployment
		profile.Recording = ""
		got := governanceOmissionWarnings(profile, deployment, types.GovernanceLimits{LocalSelfDefinedComponents: true})
		if !has(got, `sets no recording level while the deployment default's is "screen_actions" — it sets no minimum`) || has(got, "LOWER") {
			t.Errorf("warnings = %q, want only the unset recording sentence", got)
		}
		off := deployment
		off.Recording = types.RecordingOff
		if got := governanceControlWarnings(profile, off, types.GovernanceLimits{LocalSelfDefinedComponents: true}); has(got, "recording") {
			t.Errorf("warnings = %q, want no recording warning when the default's level is off", got)
		}
	})
	t.Run("a profile at least as strict warns about none of them", func(t *testing.T) {
		profile := deployment
		profile.Desktop = &types.DesktopControls{CopyOut: types.TransferBlocked}
		profile.ToolRules = append(slices.Clone(deployment.ToolRules), types.ToolRule{Tool: "Bash", Effect: types.ToolHold})
		profile.AutoStopAfterSec = 300
		if got := governanceOmissionWarnings(profile, deployment, types.GovernanceLimits{LocalSelfDefinedComponents: true}); len(got) != 0 {
			t.Errorf("warnings = %q, want none for a stricter profile", got)
		}
	})
	t.Run("a deployment that sets none of them asks for none", func(t *testing.T) {
		plain := types.RunPolicySpec{MinConfinementClass: types.CC2}
		if got := governanceControlWarnings(plain, plain, types.GovernanceLimits{LocalSelfDefinedComponents: true}); len(got) != 0 {
			t.Errorf("warnings = %q, want none", got)
		}
	})
}

// govWarnSiteHoldStore is the held-write fake store with a site config of its own.
type govWarnSiteHoldStore struct {
	*govCovStore
	site types.SiteConfig
}

func (s *govWarnSiteHoldStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.site, nil
}

// A held profile write carries the same ceiling note a direct write returns, so the proposer and the
// approver read it too.
func TestGovernanceHeldWriteCarriesTheRowCeilingNote(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	super := govCovSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin)
	inner := &govCovStore{proposeSaved: types.GovernanceChange{ID: govCovChangeID, State: types.GovernanceChangePending}}
	st := &govWarnSiteHoldStore{govCovStore: inner, site: types.SiteConfig{WorkspaceProviders: &types.WorkspaceProviders{
		Git: []types.GitProvider{govWarnRow("narrow-row", 1)},
	}}}
	s, _ := govCovServer(t, st, func(c *Config) { c.GovernanceUnitPutCreates = true })
	body := `{"name":"held","ceiling":{"min_confinement_class":"CC2","allowed_domains":["pypi.org"],` +
		`"azure_devops_capabilities":["code_read","code_write","pr"]}}`
	w := doSSO(t, s, http.MethodPut, "/api/v1/governance/units/"+govCovPID.String(), super, body)
	d := govCovDiff(t, govCovHoldAnswer(t, w, inner))
	if !slices.ContainsFunc(d.Notes, func(n string) bool {
		return strings.Contains(n, `"narrow-row"`) && strings.Contains(n, "lists 2 Azure DevOps")
	}) {
		t.Errorf("notes = %q, want the narrow-row ceiling note", d.Notes)
	}
}
