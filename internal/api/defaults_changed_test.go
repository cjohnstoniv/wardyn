// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// Every registry entry is asked about real configuration: each has an estate where it holds and one,
// differing only in the configuration it reads, where it does not. An entry with no fixture here fails.
func TestDefaultChanges_PredicatesReadTheEstate(t *testing.T) {
	sso := types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{{ID: "aws", Kind: types.ModelProviderBedrockSSO}}}}
	ssoOff := types.SiteConfig{ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{{ID: "aws", Kind: types.ModelProviderBedrockSSO, Disabled: true}}}}
	hosts := types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.internal"}}}
	locked := types.SiteConfig{Components: &types.ComponentSettings{DenyResidentDelivery: true, AutonomyCap: "L1"}}
	fixtures := map[string]struct{ on, off upgradeEstate }{
		"run_components": {
			on:  upgradeEstate{CustomComponentOpen: true},
			off: upgradeEstate{CustomComponentOpen: true, Site: locked},
		},
		"run_max_age_cap": {
			on:  upgradeEstate{RunMaxAge: 72 * time.Hour},
			off: upgradeEstate{},
		},
		"self_host_runner_restricted": {
			on:  upgradeEstate{SelfHostRestricted: true},
			off: upgradeEstate{SelfHostRestricted: true, SelfHostNamed: true},
		},
		"aws_sso_config_env": {
			on:  upgradeEstate{AWSSSOProxyInject: true, Site: sso},
			off: upgradeEstate{AWSSSOProxyInject: true, Site: ssoOff},
		},
		"internal_hosts_on_runners": {
			on:  upgradeEstate{Site: hosts, ClaimedPersonal: 1},
			off: upgradeEstate{Site: hosts},
		},
		"recording_preferred": {
			on:  upgradeEstate{ClaimedPersonal: 2, RecordingStoreOn: true},
			off: upgradeEstate{ClaimedPersonal: 2, RecordingStoreOn: true, OrgRecording: types.RecordingRequired},
		},
		"runner_recording_follows_org": {
			on:  upgradeEstate{ClaimedPersonal: 1},
			off: upgradeEstate{ClaimedPersonal: 1, RecordingStoreOn: true},
		},
	}
	for _, d := range defaultChanges {
		f, ok := fixtures[d.Name]
		if !ok {
			t.Errorf("registry entry %s (%s) has no fixture", d.Name, d.Release)
			continue
		}
		if !d.Applies(f.on) {
			t.Errorf("%s: does not apply to the estate it describes", d.Name)
		}
		if d.Applies(f.off) {
			t.Errorf("%s: applies to an estate configured for the old behaviour", d.Name)
		}
		if d.Changed == "" || d.Old == "" || !releaseNewer(d.Release, "0.8.8") {
			t.Errorf("%s: entry incomplete or before the registry's window: %+v", d.Name, d)
		}
	}
}

func TestDefaultChanges_ReleaseWindow(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.9.3", "0.9.3-rc.2", true}, {"0.9.3-rc.2", "0.9.3-rc.1", true}, {"0.10.0", "0.9.9", true},
		{"0.9.2", "0.9.2", false}, {"0.9.1", "0.9.2", false}, {"dev", "0.9.2", false}, {"0.9.3", "", false},
	} {
		if got := releaseNewer(c.a, c.b); got != c.want {
			t.Errorf("releaseNewer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	for prior, want := range map[string]string{
		"0100_old.sql": "0.8.8", "0137_run_components.sql": "0.8.9", "0150_mid.sql": "0.8.9",
		"0179_runner_arch.sql": "0.9.1", "0180_run_recording.sql": "0.9.3-rc.2", "0188_run_renewed_token_jtis.sql": "0.9.3-rc.2",
	} {
		if got := releaseForMigration(prior); got != want {
			t.Errorf("releaseForMigration(%q) = %q, want %q", prior, got, want)
		}
	}
	for _, c := range []struct {
		name string
		o    store.UpgradeOrigin
		want string
		ok   bool
	}{
		{"no record yet", store.UpgradeOrigin{}, "", false},
		{"fresh database", store.UpgradeOrigin{Recorded: true}, "", false},
		{"the release booted before", store.UpgradeOrigin{Recorded: true, Previous: "0.9.3", PriorMigration: "0100_old.sql"}, "0.9.3", true},
		{"placed by migration", store.UpgradeOrigin{Recorded: true, PriorMigration: "0179_runner_arch.sql"}, "0.9.1", true},
		{"a newer run release wins", store.UpgradeOrigin{Recorded: true, PriorMigration: "0179_runner_arch.sql", LastRunRelease: "0.9.2"}, "0.9.2", true},
		{"an older run release loses", store.UpgradeOrigin{Recorded: true, PriorMigration: "0179_runner_arch.sql", LastRunRelease: "0.9.0"}, "0.9.1", true},
		{"a run release past this one is ignored", store.UpgradeOrigin{Recorded: true, PriorMigration: "0179_runner_arch.sql", LastRunRelease: "9.0.0"}, "0.9.1", true},
	} {
		got, ok := previousRelease(c.o, "0.9.3")
		if got != c.want || ok != c.ok {
			t.Errorf("%s: previousRelease = %q, %v; want %q, %v", c.name, got, ok, c.want, c.ok)
		}
	}
	all := upgradeEstate{
		CustomComponentOpen: true, RunMaxAge: time.Hour, SelfHostRestricted: true, AWSSSOProxyInject: true, ClaimedPersonal: 1, RecordingStoreOn: true,
		Site: types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.internal"}}, ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{{ID: "aws", Kind: types.ModelProviderBedrockSSO}}}},
	}
	names := func(ds []defaultChange) (out []string) {
		for _, d := range ds {
			out = append(out, d.Name)
		}
		return out
	}
	for prev, want := range map[string][]string{
		"0.9.3":      nil,
		"0.9.3-rc.2": nil,
		"0.9.2":      {"recording_preferred"},
		"0.9.1":      {"internal_hosts_on_runners", "recording_preferred"},
		"0.9.0":      {"self_host_runner_restricted", "aws_sso_config_env", "internal_hosts_on_runners", "recording_preferred"},
		"0.8.8":      {"run_components", "run_max_age_cap", "self_host_runner_restricted", "aws_sso_config_env", "internal_hosts_on_runners", "recording_preferred"},
	} {
		if got := names(changedDefaults(defaultChanges, prev, all)); !slices.Equal(got, want) {
			t.Errorf("since %s: %v, want %v", prev, got, want)
		}
	}
}

// upgradeFake is the store setup status reads for the two upgrade rows.
type upgradeFake struct {
	store.Store
	store.RunnerStore
	store.RunnerPoolStore
	runners      []types.Runner
	defaults     types.RunnerPoolDefaults
	restricted   map[string]map[string]bool
	grants       []types.CapabilityGrant
	origin       store.UpgradeOrigin
	enforcement  map[string]bool
	siteCfg      types.SiteConfig
	versionReads int
}

func (f *upgradeFake) ListRunners(context.Context) ([]types.Runner, error) { return f.runners, nil }
func (f *upgradeFake) GetRunnerPoolOrgDefaults(context.Context) (types.RunnerPoolDefaults, error) {
	return f.defaults, nil
}
func (f *upgradeFake) ListCapabilityRestrictions(context.Context) (map[string]map[string]bool, error) {
	return f.restricted, nil
}
func (f *upgradeFake) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return f.enforcement, nil
}
func (f *upgradeFake) ListCapabilityGrants(context.Context) ([]types.CapabilityGrant, error) {
	return f.grants, nil
}
func (f *upgradeFake) UpgradeOrigin(_ context.Context, current string) (store.UpgradeOrigin, error) {
	f.versionReads++
	if current != version.Version {
		return store.UpgradeOrigin{}, nil
	}
	return f.origin, nil
}

// upgradeRows runs the two rows through upgradeChecks, as handleSetupStatus does.
func upgradeRows(t *testing.T, f *upgradeFake) map[string]SetupCheck {
	t.Helper()
	s := &Server{cfg: Config{Store: f}}
	out := map[string]SetupCheck{}
	for _, chk := range s.upgradeChecks(context.Background(), f.siteCfg) {
		out[chk.ID] = chk
	}
	return out
}

// The defaults row lists what applies here since the release this deployment ran before, and goes away
// when the operator sets the old behaviour back or the record says there is nothing to compare with.
func TestDefaultsChangedRow_FollowsTheConfiguration(t *testing.T) {
	f := &upgradeFake{
		runners:    []types.Runner{{State: types.RunnerClaimed}, {State: types.RunnerRevoked}},
		restricted: map[string]map[string]bool{},
		origin:     store.UpgradeOrigin{Recorded: true, Previous: "0.9.2"},
		siteCfg:    types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.internal"}}},
	}
	// No recording store on this test server: the runner-follows-the-organisation entry applies.
	chk, ok := upgradeRows(t, f)["defaults_changed"]
	if !ok || chk.Status != "info" || !strings.Contains(chk.Detail, "Since 0.9.2, 1 default changed") ||
		!strings.Contains(chk.Detail, "runner_recording_follows_org") || strings.Contains(chk.Detail, "internal_hosts_on_runners") {
		t.Fatalf("row = %+v, %v; want the one 0.9.3 entry that applies, and not 0.9.2's", chk, ok)
	}
	if !strings.Contains(chk.Fix, "WARDYN_RECORDING_STORE") {
		t.Errorf("fix = %q, want how to get the old behaviour", chk.Fix)
	}
	f.origin.Previous = "0.9.1"
	if chk := upgradeRows(t, f)["defaults_changed"]; !strings.Contains(chk.Detail, "internal_hosts_on_runners") {
		t.Errorf("since 0.9.1 the row = %q, want 0.9.2's internal_hosts entry", chk.Detail)
	}
	f.siteCfg.InternalHosts = nil
	if chk := upgradeRows(t, f)["defaults_changed"]; strings.Contains(chk.Detail, "internal_hosts_on_runners") {
		t.Errorf("with no internal_hosts the row = %q, want no internal_hosts entry", chk.Detail)
	}
	f.origin.Previous = "0.9.2"
	f.runners = []types.Runner{{State: types.RunnerRevoked}}
	if _, ok := upgradeRows(t, f)["defaults_changed"]; ok {
		t.Error("the row stayed with no claimed runner, where no 0.9.3 entry applies")
	}
	f.runners = []types.Runner{{State: types.RunnerClaimed}}
	f.origin = store.UpgradeOrigin{Recorded: true}
	if _, ok := upgradeRows(t, f)["defaults_changed"]; ok {
		t.Error("a fresh database showed the row")
	}
	f.origin = store.UpgradeOrigin{}
	if _, ok := upgradeRows(t, f)["defaults_changed"]; ok || f.versionReads == 0 {
		t.Errorf("a release with no record showed the row (or the record was never read: %d reads)", f.versionReads)
	}
}

// The self_host_runner_grant row: restricted, claimed runners, and no grant row naming the value. An
// allow or a deny naming it, no claimed runner, or no restriction each clear it.
func TestSelfHostRunnerGrantRow(t *testing.T) {
	f := &upgradeFake{
		runners:    []types.Runner{{State: types.RunnerClaimed}, {State: types.RunnerClaimed}, {State: types.RunnerUnclaimed}},
		restricted: map[string]map[string]bool{capFeature: {featureSelfHostRunner: true}},
		grants:     []types.CapabilityGrant{{SubjectType: types.CapabilitySubjectAll, Capability: capFeature, Value: capWildcard, Effect: types.CapabilityAllow}},
	}
	chk, ok := upgradeRows(t, f)["self_host_runner_grant"]
	if !ok || chk.Status != "warn" || chk.Blocking || !strings.Contains(chk.Detail, "cannot mint runner tokens") || !strings.Contains(chk.Detail, "2 claimed person-owned runners keep working") {
		t.Fatalf("row = %+v, %v; want a warn: restricted, two claimed person-owned runners, only a wildcard allow", chk, ok)
	}
	if !strings.Contains(chk.Fix, selfHostGrantBody) || !strings.Contains(chk.Fix, "/api/v1/permissions/explain") {
		t.Errorf("fix = %q, want the exact grant body and the explain door", chk.Fix)
	}
	for _, effect := range []types.CapabilityEffect{types.CapabilityAllow, types.CapabilityDeny} {
		g := f.grants
		f.grants = append(slices.Clone(g), types.CapabilityGrant{SubjectType: types.CapabilitySubjectGroup, Subject: "eng", Capability: capFeature, Value: featureSelfHostRunner, Effect: effect})
		if _, ok := upgradeRows(t, f)["self_host_runner_grant"]; ok {
			t.Errorf("a %s naming self_host_runner left the row", effect)
		}
		f.grants = g
	}
	f.runners = []types.Runner{{State: types.RunnerUnclaimed}}
	if _, ok := upgradeRows(t, f)["self_host_runner_grant"]; ok {
		t.Error("the row showed with no claimed runner")
	}
	f.runners = []types.Runner{{State: types.RunnerClaimed, OwnerKind: types.RunnerOwnerOrg}, {State: types.RunnerClaimed, OwnerKind: types.RunnerOwnerOrg}}
	if chk, ok := upgradeRows(t, f)["self_host_runner_grant"]; ok {
		t.Errorf("an estate whose only claimed runners are the organisation's got the grant row: %+v", chk)
	}
	f.runners = []types.Runner{{State: types.RunnerClaimed}}
	f.restricted = map[string]map[string]bool{}
	if _, ok := upgradeRows(t, f)["self_host_runner_grant"]; ok {
		t.Error("the row showed with the value not restricted")
	}
}

// upgradeStatusFake adds the reads GET /setup/status makes of every store (setupCheckIdsStore's set).
type upgradeStatusFake struct{ *upgradeFake }

func (f upgradeStatusFake) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return f.siteCfg, nil
}
func (f upgradeStatusFake) ListRuns(context.Context) ([]types.AgentRun, error) { return nil, nil }

// Both rows reach an operator's GET /setup/status, which `wardyn setup status` and the console's setup
// review print, and leave it when the configuration no longer calls for them.
func TestSetupStatus_ShowsTheUpgradeRows(t *testing.T) {
	f := &upgradeFake{
		runners:    []types.Runner{{State: types.RunnerClaimed}},
		restricted: map[string]map[string]bool{capFeature: {featureSelfHostRunner: true}},
		origin:     store.UpgradeOrigin{Recorded: true, Previous: "0.9.2"},
	}
	ids := func() []string {
		return setupCheckIds(t, New(Config{AdminToken: adminToken, Store: upgradeStatusFake{f}}))
	}
	if got := ids(); !slices.Contains(got, "self_host_runner_grant") || !slices.Contains(got, "defaults_changed") {
		t.Fatalf("setup status ids = %v, want self_host_runner_grant and defaults_changed", got)
	}
	f.runners = nil
	if got := ids(); slices.Contains(got, "self_host_runner_grant") || slices.Contains(got, "defaults_changed") {
		t.Fatalf("with no claimed runner, setup status ids = %v, want neither upgrade row", got)
	}
}

// A registry entry carries the first release that shipped it, a candidate included: the recording
// defaults shipped in 0.9.3-rc.2, so an estate on rc.2 is not told again at rc.3 or at 0.9.3, and one
// coming from 0.9.2 is told both.
func TestDefaultChanges_CandidateLabels(t *testing.T) {
	storeOn := upgradeEstate{ClaimedPersonal: 1, RecordingStoreOn: true}
	storeOff := upgradeEstate{ClaimedPersonal: 1}
	shown := func(prev, current string) (out []string) {
		p, ok := previousRelease(store.UpgradeOrigin{Recorded: true, Previous: prev}, current)
		if !ok {
			t.Fatalf("previousRelease(%s, %s) found no previous release", prev, current)
		}
		for _, e := range []upgradeEstate{storeOn, storeOff} {
			for _, d := range changedDefaults(defaultChanges, p, e) {
				out = append(out, d.Name)
			}
		}
		return out
	}
	for _, c := range []struct {
		prev, current string
		want          []string
	}{
		{"0.9.3-rc.2", "0.9.3-rc.3", nil},
		{"0.9.3-rc.2", "0.9.3", nil},
		{"0.9.2", "0.9.3", []string{"recording_preferred", "runner_recording_follows_org"}},
	} {
		if got := shown(c.prev, c.current); !slices.Equal(got, c.want) {
			t.Errorf("%s -> %s: %v, want %v", c.prev, c.current, got, c.want)
		}
	}
}

// Organisation runners (0.9.3) count toward no entry: no earlier default reached them.
func TestDefaultChanges_OrganisationRunnersCountForNothing(t *testing.T) {
	f := &upgradeFake{
		runners:    []types.Runner{{State: types.RunnerClaimed, OwnerKind: types.RunnerOwnerOrg}},
		restricted: map[string]map[string]bool{capFeature: {featureSelfHostRunner: true}},
		origin:     store.UpgradeOrigin{Recorded: true, Previous: "0.9.1"},
		siteCfg:    types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.internal"}}},
	}
	if rows := upgradeRows(t, f); rows["self_host_runner_grant"].ID != "" || strings.Contains(rows["defaults_changed"].Detail, "internal_hosts_on_runners") ||
		strings.Contains(rows["defaults_changed"].Detail, "recording") {
		t.Fatalf("organisation runners reached an entry or the grant row: %+v", rows)
	}
	f.runners = append(f.runners, types.Runner{State: types.RunnerClaimed, OwnerKind: types.RunnerOwnerPerson})
	if rows := upgradeRows(t, f); rows["self_host_runner_grant"].ID == "" || !strings.Contains(rows["defaults_changed"].Detail, "internal_hosts_on_runners") {
		t.Fatalf("a person's runner beside them did not: %+v", rows)
	}
}

// 0.9.1 -> 0.9.3 by -migrate-only: the record keeps the 0.9.1 tip (0179), which places as 0.9.1, so
// 0.9.2's internal_hosts entry is kept; cmd/wardynd's TestMigrateOnly_RecordsTheReleaseBeforeTheBoot
// pins that the record holds 0179.
func TestDefaultChanges_FromTheOneNineOneTipKeepsTheOneNineTwoEntry(t *testing.T) {
	prev, ok := previousRelease(store.UpgradeOrigin{Recorded: true, PriorMigration: "0179_runner_arch.sql", LastRunRelease: "0.9.1"}, "0.9.3")
	e := upgradeEstate{ClaimedPersonal: 1, Site: types.SiteConfig{InternalHosts: []types.InternalHost{{HostSuffix: "corp.internal"}}}}
	var names []string
	for _, d := range changedDefaults(defaultChanges, prev, e) {
		names = append(names, d.Name)
	}
	if !ok || prev != "0.9.1" || !slices.Contains(names, "internal_hosts_on_runners") {
		t.Fatalf("previous = %q, %v; entries %v; want 0.9.1 and the internal_hosts entry", prev, ok, names)
	}
}
