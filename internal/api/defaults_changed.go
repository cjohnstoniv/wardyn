// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// Defaults changed by an upgrade. Every release that changes a default with an effect an operator
// can see adds an entry here, and the same text to its CHANGELOG section "Defaults changed by this
// upgrade" (RELEASING.md). Setup status lists the entries newer than the release this deployment ran
// before, and only those whose Applies holds on this deployment's own configuration.

// upgradeEstate is the configuration the entries are asked about, read once per setup status call.
type upgradeEstate struct {
	Site types.SiteConfig
	// ClaimedSelfHosted counts claimed self-hosted (laptop) runners.
	ClaimedSelfHosted int
	// OrgRecording is the organisation's runner-pool default recording setting; "" is preferred.
	OrgRecording types.RecordingRequirement
	// RecordingStoreOn: the server keeps recordings (WARDYN_RECORDING_STORE is not off).
	RecordingStoreOn  bool
	RunMaxAge         time.Duration
	AWSSSOProxyInject bool
	// SelfHostRestricted: the feature value self_host_runner is restricted; SelfHostNamed: a grant row
	// (allow or deny, any subject) names it.
	SelfHostRestricted, SelfHostNamed bool
	// CustomComponentOpen: a person with no grant of their own may define a custom component: no
	// deny for everyone names custom_component or `*`, and `feature` is not enforced unless an allow
	// for everyone covers it.
	CustomComponentOpen bool
}

// defaultChange is one registry entry.
type defaultChange struct {
	Release string // the release the default changed in
	Name    string
	Changed string // what changed, one sentence
	Old     string // what to set to get the old behaviour
	Applies func(upgradeEstate) bool
}

const customComponentDenyBody = `{"subject_type": "all", "capability": "feature", "value": "custom_component", "effect": "deny"}`

// defaultChanges is the registry, oldest first.
var defaultChanges = []defaultChange{
	{
		Release: "0.8.9", Name: "run_components",
		Changed: "Runs can carry custom components that add destinations and credentials: anyone may define their own (custom_component), " +
			"a component's secret may be delivered as an environment variable or a file, and no autonomy cap applies to them.",
		Old: "Deny custom components to everyone (POST /api/v1/permissions/grants with " + customComponentDenyBody + "), " +
			"or set components.deny_resident_delivery: true and components.autonomy_cap in the site config.",
		Applies: func(e upgradeEstate) bool {
			c := e.Site.Components
			return e.CustomComponentOpen && (c == nil || !c.DenyResidentDelivery || c.AutonomyCap == "")
		},
	},
	{
		Release: "0.9.0", Name: "run_max_age_cap",
		Changed: "WARDYN_RUN_MAX_AGE became every new run's lifetime cap, not only a reaper stop: a run no profile or pool gives an end " +
			"ends at creation plus the max age, no_end is refused, and no one can extend a run past it.",
		Old: "No setting keeps the reaper stop without the cap; unsetting WARDYN_RUN_MAX_AGE removes both.",
		Applies: func(e upgradeEstate) bool {
			return e.RunMaxAge > 0
		},
	},
	{
		Release: "0.9.1", Name: "self_host_runner_restricted",
		Changed: "Minting a token for a runner of one's own needs the self_host_runner permission, which is restricted: " +
			"people who are not admins are refused (403 capability_feature) until an allow names it.",
		Old: "POST /api/v1/permissions/grants with " + selfHostGrantBody + ".",
		Applies: func(e upgradeEstate) bool {
			return e.SelfHostRestricted && !e.SelfHostNamed
		},
	},
	{
		Release: "0.9.1", Name: "aws_sso_config_env",
		Changed: "With AWS SSO proxy-inject on (the default), WARDYN_AWS_SSO_CONFIG_B64 reaches the sandbox as a plain environment variable: " +
			"it holds the start URL, region, account id and role name, no token, and docker inspect or a pod spec shows them.",
		Old: "Only WARDYN_AWS_SSO_PROXY_INJECT=off makes it a secret again, and that also writes the AWS sign-in token into the sandbox; keeping the default is safer.",
		Applies: func(e upgradeEstate) bool {
			return e.AWSSSOProxyInject && hasEnabledProvider(e.Site, types.ModelProviderBedrockSSO)
		},
	},
	{
		Release: "0.9.2", Name: "internal_hosts_on_runners",
		Changed: "Self-hosted (laptop) runners receive the organisation's internal_hosts, so a run there reaches hosts that resolve to private addresses; " +
			"before, they were dropped for every local run.",
		Old: "No setting sends them to the cluster only; a suffix laptops must not reach has to come out of internal_hosts, which removes it everywhere.",
		Applies: func(e upgradeEstate) bool {
			return len(e.Site.InternalHosts) > 0 && e.ClaimedSelfHosted > 0
		},
	},
	{
		Release: "0.9.3", Name: "recording_preferred",
		Changed: "Recording defaults to preferred: a run a non-recording laptop runner takes goes ahead unrecorded and says why (runner_declined), " +
			"where a runner older than 0.9.3 recorded it regardless.",
		Old: `Set "recording": "required" in PUT /api/v1/runner-pool-defaults, beside the stored pool_id and barrier (read them first with GET; ` +
			"a write that leaves barrier out clears it). A run only a non-recording runner could take is then refused instead of running unrecorded.",
		Applies: func(e upgradeEstate) bool {
			return e.ClaimedSelfHosted > 0 && e.RecordingStoreOn && e.OrgRecording != types.RecordingRequired
		},
	},
	{
		Release: "0.9.3", Name: "runner_recording_follows_org",
		Changed: "A laptop runner records a run only when its organisation keeps recordings: with the recording store off, runs are marked org_off " +
			"and nothing is uploaded, where before the runner recorded and its upload failed.",
		Old: "Turn the organisation's recording store on (WARDYN_RECORDING_STORE pg or fs); the runner's own --recording-store stays its ceiling.",
		Applies: func(e upgradeEstate) bool {
			return e.ClaimedSelfHosted > 0 && !e.RecordingStoreOn
		},
	},
}

func hasEnabledProvider(sc types.SiteConfig, kind types.ModelProviderKind) bool {
	if sc.ModelProviders == nil {
		return false
	}
	for _, p := range sc.ModelProviders.Providers {
		if p.Kind == kind && !p.Disabled {
			return true
		}
	}
	return false
}

// changedDefaults returns the entries newer than previous whose Applies holds on e.
func changedDefaults(registry []defaultChange, previous string, e upgradeEstate) []defaultChange {
	var out []defaultChange
	for _, d := range registry {
		if releaseNewer(d.Release, previous) && d.Applies(e) {
			out = append(out, d)
		}
	}
	return out
}

// defaultsChangedCheck grades the defaults_changed row. It is information, never a warning: each
// entry is a choice the release made that the operator may keep, and the row stays until the next
// upgrade.
func defaultsChangedCheck(previous string, changed []defaultChange) (SetupCheck, bool) {
	if len(changed) == 0 {
		return SetupCheck{}, false
	}
	noun := "defaults"
	if len(changed) == 1 {
		noun = "default"
	}
	detail := []string{fmt.Sprintf("Since %s, %d %s changed in a way that reaches this deployment's configuration.", previous, len(changed), noun)}
	fix := []string{"For the old behaviour:"}
	for _, d := range changed {
		detail = append(detail, fmt.Sprintf("%s %s: %s", d.Release, d.Name, d.Changed))
		fix = append(fix, fmt.Sprintf("%s: %s", d.Name, d.Old))
	}
	return SetupCheck{ID: "defaults_changed", Label: "Defaults changed by this upgrade", Status: "info",
		Detail: strings.Join(detail, " "), Fix: strings.Join(fix, " ")}, true
}

// releaseTips is the newest migration each release before deployment_versions shipped (migration
// 0192), oldest first, read from the release tags. It places a deployment's first record: the
// newest migration the database held before is the newest one its previous release shipped. It
// never grows: every later upgrade finds the release it ran before in deployment_versions.
var releaseTips = []struct{ release, tip string }{
	{"0.8.8", "0132_run_pause_settles.sql"},
	{"0.8.9", "0137_run_components.sql"},
	{"0.9.0", "0177_member_drives.sql"},
	{"0.9.1", "0179_runner_arch.sql"},
	{"0.9.2", "0179_runner_arch.sql"},
	{"0.9.3-rc.1", "0179_runner_arch.sql"},
	{"0.9.3-rc.2", "0180_run_recording.sql"},
}

// releaseForMigration is the oldest release whose newest migration is the latest one at or before
// prior. Releases that shipped the same newest migration cannot be told apart, so the oldest is
// taken and the row may name a change the deployment already had, never miss one. A database older
// than 0.8.8 places as 0.8.8, which lists the same entries.
func releaseForMigration(prior string) string {
	best := releaseTips[0]
	for _, r := range releaseTips {
		if r.tip <= prior && r.tip > best.tip {
			best = r
		}
	}
	return best.release
}

// previousRelease is the release this deployment ran before current, and false when there is none
// to compare with: no record yet, or a fresh database. A first record takes the newer of the release
// placed by the prior migration and the release that last started a run here.
func previousRelease(o store.UpgradeOrigin, current string) (string, bool) {
	switch {
	case !o.Recorded:
		return "", false
	case o.Previous != "":
		return o.Previous, true
	case o.PriorMigration == "":
		return "", false
	}
	prev := releaseForMigration(o.PriorMigration)
	if releaseNewer(o.LastRunRelease, prev) && releaseNewer(current, o.LastRunRelease) {
		prev = o.LastRunRelease
	}
	return prev, true
}

// releaseNewer reports whether release a is newer than b. A release is MAJOR.MINOR.PATCH with an
// optional -rc.N, which sorts before the release itself; anything else is never newer.
func releaseNewer(a, b string) bool {
	pa, okA := parseRelease(a)
	pb, okB := parseRelease(b)
	if !okA || !okB {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

// parseRelease reads MAJOR.MINOR.PATCH[-rc.N] as four numbers; the fourth is N, or a value above any
// N for a release with no -rc.
func parseRelease(v string) ([4]int, bool) {
	var out [4]int
	core, rc, hasRC := strings.Cut(v, "-rc.")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	out[3] = int(^uint(0) >> 1)
	if hasRC {
		n, err := strconv.Atoi(rc)
		if err != nil || n < 0 {
			return out, false
		}
		out[3] = n
	}
	return out, true
}

// upgradeEstate reads the configuration the registry and the self_host_runner_grant row ask about.
// Any read that fails leaves both rows out (false): nothing is guessed.
func (s *Server) upgradeEstate(ctx context.Context, site types.SiteConfig) (upgradeEstate, bool) {
	e := upgradeEstate{Site: site, RecordingStoreOn: s.cfg.RecordingStore != nil, RunMaxAge: s.cfg.RunMaxAge, AWSSSOProxyInject: s.cfg.AWSSSOProxyInject}
	fail := func(what string, err error) (upgradeEstate, bool) {
		slog.WarnContext(ctx, "wardynd: setup status: "+what, slog.Any("err", err))
		return upgradeEstate{}, false
	}
	if rs, ok := s.cfg.Store.(store.RunnerStore); ok {
		runners, err := rs.ListRunners(ctx)
		if err != nil {
			return fail("list runners", err)
		}
		for _, r := range runners {
			if r.State == types.RunnerClaimed {
				e.ClaimedSelfHosted++
			}
		}
	}
	// Neither row can show without a boot record or a claimed runner; stop before the grant reads.
	if _, versioned := s.cfg.Store.(store.DeploymentVersionStore); !versioned && e.ClaimedSelfHosted == 0 {
		return upgradeEstate{}, false
	}
	if ps, ok := s.cfg.Store.(store.RunnerPoolStore); ok {
		d, err := ps.GetRunnerPoolOrgDefaults(ctx)
		if err != nil {
			return fail("read runner pool defaults", err)
		}
		e.OrgRecording = d.Recording
	}
	restricted, err := s.cfg.Store.ListCapabilityRestrictions(ctx)
	if err != nil {
		return fail("list capability restrictions", err)
	}
	e.SelfHostRestricted = restricted[capFeature][featureSelfHostRunner]
	enf, err := s.cfg.Store.GetCapabilityEnforcement(ctx)
	if err != nil {
		return fail("read capability enforcement", err)
	}
	grants, err := s.cfg.Store.ListCapabilityGrants(ctx)
	if err != nil {
		return fail("list capability grants", err)
	}
	componentDenied, componentAllowed := false, false
	for _, g := range grants {
		if g.Capability != capFeature {
			continue
		}
		if g.Value == featureSelfHostRunner {
			e.SelfHostNamed = true
		}
		if g.SubjectType == types.CapabilitySubjectAll && (g.Value == featureCustomComponent || g.Value == capWildcard) {
			componentDenied = componentDenied || g.Effect == types.CapabilityDeny
			componentAllowed = componentAllowed || g.Effect == types.CapabilityAllow
		}
	}
	e.CustomComponentOpen = !componentDenied && (!enf[capFeature] || componentAllowed)
	return e, true
}

// upgradeChecks is the self_host_runner_grant and defaults_changed rows, in that order, for an operator.
func (s *Server) upgradeChecks(ctx context.Context, site types.SiteConfig) []SetupCheck {
	e, ok := s.upgradeEstate(ctx, site)
	if !ok {
		return nil
	}
	var out []SetupCheck
	if chk, ok := selfHostGrantCheck(e); ok {
		out = append(out, chk)
	}
	if chk, ok := s.defaultsChangedRow(ctx, e); ok {
		out = append(out, chk)
	}
	return out
}

// defaultsChangedRow is the defaults_changed row: left out with no boot record, on a fresh database,
// or when no entry newer than the previous release applies here.
func (s *Server) defaultsChangedRow(ctx context.Context, e upgradeEstate) (SetupCheck, bool) {
	vs, ok := s.cfg.Store.(store.DeploymentVersionStore)
	if !ok {
		return SetupCheck{}, false
	}
	o, err := vs.UpgradeOrigin(ctx, version.Version)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: setup status: read deployment versions", slog.Any("err", err))
		return SetupCheck{}, false
	}
	prev, ok := previousRelease(o, version.Version)
	if !ok {
		return SetupCheck{}, false
	}
	return defaultsChangedCheck(prev, changedDefaults(defaultChanges, prev, e))
}
