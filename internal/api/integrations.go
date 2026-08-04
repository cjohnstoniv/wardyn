// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// integrations.go computes the Integrations screen's capability matrix: for
// one named connection to a system outside Wardyn (an "integration"), what it
// powers, whether that's ON / OFF / NEEDS_SETUP / an impossible protocol fact
// right now, why, and where the credential lives when it IS wired. It is a
// PURE function over a snapshot (capabilitiesFor) — no storage, no HTTP, no
// wiring — so the real store-backed type (types.Integration, a later wave)
// can be adapted into integrationView without this file changing.
//
// integrationView/capEnv are PROVISIONAL, package-local stand-ins for that
// later wave's real types; capabilitiesFor is the part meant to survive.

// integrationView is the read-only shape capabilitiesFor needs from a stored
// integration. Credentials/Config use generic string keys (documented per
// case in capabilitiesFor) rather than a typed field per credential, since the
// real shape arrives with types.Integration.
type integrationView struct {
	ID, Category, Type string
	Disabled           bool
	Header             string            // HTTP field the proxy presents this integration's credential in ("" = no proxy-injected lane)
	Hosts              []string          // where the system lives; empty means this row opens nothing
	Credentials        map[string]string // credential-slot name -> stored secret ref (e.g. "api_key" -> "anthropic-api-key")
	Config             map[string]any    // type-specific knobs (e.g. "lane", "ecosystems")
	DisabledCaps       []string          // capability IDs the operator turned off individually
}

// capEnv carries the external readiness signals capabilitiesFor cannot derive
// from an integrationView alone: secret-store presence, host detection, and
// managed/resident subscription liveness.
//
// ponytail: the original sketch for this struct also carried a
// ComposerConfigEnvSet field. Dropped — no cell in the approved matrix below
// reads it, and "(adjust as needed)" explicitly licensed trimming it. Add it
// back the day a matrix cell actually needs it.
type capEnv struct {
	SecretPresent            func(string) bool
	HostLike                 bool
	BedrockRegionSet         bool
	BedrockModelSet          bool
	ManagedBlobPresent       func(provider string) bool
	ResidentSubscriptionLive bool
}

// CapState is one capability's current state.
type CapState string

const (
	CapAvailable  CapState = "available"   // wired and usable right now
	CapOff        CapState = "off"         // disabled (integration- or capability-level)
	CapNeedsSetup CapState = "needs_setup" // possible, but a credential/config gap blocks it today
	CapImpossible CapState = "impossible"  // a protocol fact, never fixable by configuration
)

// Capability is one cell of the matrix: what (ID), current state, why
// (Reason — populated for needs_setup/impossible, and optionally to annotate
// an available cell with a qualifying fact), and where the credential lives
// when wired (Residency; "" when not applicable — e.g. an impossible cell).
type Capability struct {
	ID        string
	State     CapState
	Reason    string
	Residency string
}

// model_api / wardyn_features copy canon not already owned by the harness
// catalog's tool-compatibility facts (harness.go) — verbatim from the
// Integrations screen mock (scratchpad mockup/wardyn-integrations.js, the T
// object). Do not paraphrase; keep in sync with that file.
const (
	reasonXSubDirect      = "A subscription token is accepted only for Claude-Code-shaped requests; anything else comes back 429. That's Anthropic's gate, not a Wardyn setting."
	reasonXAzureDirect    = "No sandbox lane exists — Azure is called from the control plane only."
	reasonBedrockFeatures = "Wardyn's own features reach Bedrock through the AWS credential chain — the same lane this integration uses."
	// reasonBedrockUnset states the real dispatch behavior: without both a
	// region and a model id, resolveBedrockAuth never reports ready and the run
	// falls through to whatever other lane it has (usually none).
	reasonBedrockUnset = "Region and model id are unset — a run can't reach Bedrock until both are set."
	// reasonHostCLIOptIn is the canon note for the host-CLI lane's Wardyn-features
	// cell (mock: CAPS.sub, hostCli branch) — verbatim.
	reasonHostCLIOptIn = "Opt-in — off until you switch it on."
)

// capabilitiesFor computes the full capability matrix for one integration.
// Pure: no storage, no HTTP, no wiring — env carries every external fact it
// needs. Unknown v.Type reads as "no capabilities" (nil), the same honest
// default an unrecognized harness id gets in harness.go.
func capabilitiesFor(v integrationView, env capEnv) []Capability {
	var caps []Capability
	switch v.Type {
	case "anthropic_api_key":
		caps = directKeyCaps(v, env, "claude-code", "codex-cli")
	case "anthropic_subscription":
		caps = subscriptionCaps(v, env)
	case "bedrock":
		caps = bedrockCaps(v, env)
	case "openai_api_key":
		caps = directKeyCaps(v, env, "codex-cli", "claude-code")
	case "azure_openai":
		caps = []Capability{
			{ID: "model_api", State: CapImpossible, Reason: reasonXAzureDirect},
			{ID: "tool:claude-code", State: CapImpossible, Reason: harnessProviderReason("claude-code", v.Type)},
			{ID: "tool:codex-cli", State: CapImpossible, Reason: harnessProviderReason("codex-cli", v.Type)},
			{ID: "wardyn_features", State: CapAvailable, Residency: "control_plane"},
		}
	case "github_app":
		caps = githubAppCaps(v, env)
	case "git_host":
		caps = []Capability{
			gatedCap("clone:pat", v.Credentials["pat"], env, "resident_env"),
			gatedCap("clone:ssh", v.Credentials["ssh_key"], env, "resident_env"),
			{ID: "egress_host", State: CapAvailable},
		}
	case "artifact_mirror":
		caps = artifactMirrorCaps(v, env)
	case "host_proxy":
		caps = []Capability{gatedCap("egress_upstream", v.Credentials["secret"], env, "proxy_injected")}
	default:
		// Only a GENERIC category derives a matrix from the row itself. An
		// unrecognized Type in a TYPED category still reads as "no capabilities"
		// (nil) — the same honest default an unrecognized harness id gets — since
		// that row's behavior was supposed to come from code that doesn't exist.
		if genericIntegrationCategories[types.IntegrationCategory(v.Category)] {
			caps = genericCaps(v, env)
		}
	}
	applyDisabled(caps, v)
	return caps
}

// reasonNoDeliveryLane is the honest line for a generic integration that names
// no header: Wardyn opens the path, and that is genuinely the difference
// between a run reaching the system and not reaching it at all — but the
// resident and brokered lanes are hand-written per provider (see
// types.Integration.Header), so nothing here can deliver a credential.
const reasonNoDeliveryLane = "Wardyn can open the path to these hosts. Delivering this system's credential into the sandbox isn't built — the resident and brokered lanes are hand-written per provider."

// genericCaps is the matrix for a GENERIC-category integration (see
// types.IntegrationCategory): one whose behavior is fully described by its own
// hosts and header rather than by a type this file switches on. Two honest
// cells, both derived from the stored row:
//
//   - egress_host — the hosts become reachable for a run granted this row.
//     needs_setup while the row names none: an integration with no hosts opens
//     nothing, and saying "available" there would be the lie this whole file
//     exists to avoid.
//   - credential — proxy-injected when a header names a stored secret;
//     otherwise the stated "egress only" fact, which is what the two groups
//     that authenticate outside HTTP (cloud providers, data stores) get.
func genericCaps(v integrationView, env capEnv) []Capability {
	reach := Capability{ID: "egress_host", State: CapAvailable}
	if len(v.Hosts) == 0 {
		reach = Capability{ID: "egress_host", State: CapNeedsSetup, Reason: "No hosts named yet — nothing becomes reachable."}
	}
	cred := Capability{ID: "credential", State: CapImpossible, Reason: reasonNoDeliveryLane}
	if v.Header != "" {
		cred = gatedCap("credential", v.Credentials[types.IntegrationCredentialToken], env, "proxy_injected")
	}
	return []Capability{reach, cred}
}

// directKeyCaps builds the shared 4-cell matrix for the two direct-api-key ai
// types (anthropic_api_key, openai_api_key): model_api, the driven tool, and
// wardyn_features all gate on the SAME api_key secret and share one reason
// when it's missing; the OTHER tool is an unconditional protocol-fact
// impossibility sourced from the harness catalog (harness.go) — it speaks a
// different API, no setup state changes that.
func directKeyCaps(v integrationView, env capEnv, drivenHarness, otherHarness string) []Capability {
	ref := v.Credentials["api_key"]
	const residency = "proxy_injected"
	return []Capability{
		gatedCap("model_api", ref, env, residency),
		gatedCap("tool:"+drivenHarness, ref, env, residency),
		{ID: "tool:" + otherHarness, State: CapImpossible, Reason: harnessProviderReason(otherHarness, v.Type)},
		gatedCap("wardyn_features", ref, env, residency),
	}
}

// subscriptionCaps builds the anthropic_subscription matrix. Config["lane"]
// selects "managed" (container login; the recommended default — also what an
// unset/unrecognized lane value falls back to) or "resident_host" (host CLI
// login). model_api is an unconditional protocol fact (a subscription token
// is only accepted for Claude-Code-shaped requests); wardyn_features is
// unconditionally available regardless of lane — an operator opts it off
// per-integration via DisabledCaps; capabilitiesFor doesn't need to know why.
func subscriptionCaps(v integrationView, env capEnv) []Capability {
	lane, _ := v.Config["lane"].(string)
	claudeState, claudeReason, residency := CapAvailable, "", "proxy_injected"
	if lane == "resident_host" {
		residency = "resident_mount"
		if !env.ResidentSubscriptionLive {
			claudeState, claudeReason = CapNeedsSetup, residentHostReason(env)
		}
	} else if !envManagedBlobPresent(env, "anthropic") {
		claudeState, claudeReason = CapNeedsSetup, "no managed Claude subscription connected"
	}
	// Wardyn's own features ride the HOST-CLI lane only when the operator opts
	// in: that wire shells out to the resident `claude` login on the control
	// plane, which is subscription-ToS-sensitive, so the composer backend that
	// implements it ships disabled by default (backends.factory). Reporting it
	// "available" here would promise Composer a session it will not use — and
	// would default Wardyn's own calls onto the operator's personal login. The
	// managed lane has no such caveat: it sends Claude-Code-shaped requests
	// through the sandbox wire.
	features := Capability{ID: "wardyn_features", State: CapAvailable, Residency: residency}
	if lane == "resident_host" {
		features = Capability{ID: "wardyn_features", State: CapOff, Reason: reasonHostCLIOptIn, Residency: residency}
	}
	return []Capability{
		{ID: "model_api", State: CapImpossible, Reason: reasonXSubDirect},
		{ID: "tool:claude-code", State: claudeState, Reason: claudeReason, Residency: residency},
		{ID: "tool:codex-cli", State: CapImpossible, Reason: harnessProviderReason("codex-cli", v.Type)},
		features,
	}
}

// residentHostReason explains why the resident_host subscription lane isn't
// live yet, distinguishing a genuinely sealed control plane (no host to see)
// from a host-mode wardynd that simply has no active Claude CLI session.
func residentHostReason(env capEnv) string {
	if !env.HostLike {
		return "host-only: wardynd runs in a container and can only see a ~/.claude mounted into it — the managed lane avoids this"
	}
	return "host-only: no live Claude CLI session found on this host"
}

// bedrockCaps builds the bedrock matrix — ONE integration carrying the lane
// choice (auto|bearer|sso|aws_dir|static), because that is what
// resolveBedrockAuth resolves: one account, one region, one model, an ordered
// credential fallback. Residency follows the lane (bearer is injected on the
// wire; every other lane puts AWS credentials inside the sandbox).
func bedrockCaps(v integrationView, env capEnv) []Capability {
	lane, _ := v.Config["lane"].(string)
	residency := "resident_env"
	if lane == "bearer" {
		residency = "proxy_injected"
	}
	// Region+model gate EVERY Bedrock cell, not just Wardyn's own features:
	// resolveBedrockAuth (runs_bedrock.go) returns an unready bedrockAuth when
	// either is empty, and dispatch then falls silently to the api-key lane. A
	// cell that reads "available" while the run it describes cannot reach
	// Bedrock at all is exactly the drift this matrix exists to prevent.
	if !env.BedrockRegionSet || !env.BedrockModelSet {
		return []Capability{
			{ID: "model_api", State: CapNeedsSetup, Reason: reasonBedrockUnset},
			{ID: "tool:claude-code", State: CapNeedsSetup, Reason: reasonBedrockUnset},
			{ID: "tool:codex-cli", State: CapImpossible, Reason: harnessProviderReason("codex-cli", v.Type)},
			{ID: "wardyn_features", State: CapNeedsSetup, Reason: reasonBedrockUnset},
		}
	}
	return []Capability{
		{ID: "model_api", State: CapAvailable, Residency: residency},
		{ID: "tool:claude-code", State: CapAvailable, Residency: residency},
		{ID: "tool:codex-cli", State: CapImpossible, Reason: harnessProviderReason("codex-cli", v.Type)},
		{ID: "wardyn_features", State: CapAvailable, Residency: residency, Reason: reasonBedrockFeatures},
	}
}

// githubAppCaps builds the github_app matrix: clone:app is a single brokered
// capability that needs BOTH credentials (an installation token minted from
// only one half is not a thing GitHub offers); egress_host is unconditional
// (naming the host in the allowlist needs no credential).
func githubAppCaps(v integrationView, env capEnv) []Capability {
	okID, _ := secretGate(v.Credentials["app_id"], env)
	okKey, _ := secretGate(v.Credentials["app_key"], env)
	clone := Capability{ID: "clone:app", State: CapAvailable, Residency: "brokered"}
	if !okID || !okKey {
		clone = Capability{ID: "clone:app", State: CapNeedsSetup, Reason: "needs both app_id and app_key credentials"}
	}
	return []Capability{clone, {ID: "egress_host", State: CapAvailable}}
}

// artifactMirrorCaps builds one redirect:<ecosystem> capability per
// Config["ecosystems"] entry. The token is optional: an anonymous-read corp
// registry works with the URL redirect alone, so a missing/dangling token
// ref never needs_setup — it degrades to a config-only redirect (mirroring
// planArtifactRedirect's real dispatch-time behavior in artifact_redirect.go)
// and that degrade is reflected in Residency and Reason, not in State.
func artifactMirrorCaps(v integrationView, env capEnv) []Capability {
	residency, reason := "config_only", "no token configured — URL-only redirect (anonymous read)"
	if envSecretPresent(env, v.Credentials["token"]) {
		residency, reason = "proxy_injected", ""
	}
	var caps []Capability
	for _, eco := range stringSlice(v.Config["ecosystems"]) {
		caps = append(caps, Capability{ID: "redirect:" + eco, State: CapAvailable, Residency: residency, Reason: reason})
	}
	return caps
}

// applyDisabled mutates caps in place (slices share their backing array, so
// no return value is needed): a whole-integration Disabled forces EVERY cell
// off with one uniform reason, regardless of what it would otherwise be —
// including a protocol-fact impossibility, per the approved spec ("every
// cell off"). Absent that, a capability named in DisabledCaps is forced off
// individually. Both overrides clear Residency: nothing is actually wired
// while off.
func applyDisabled(caps []Capability, v integrationView) {
	if v.Disabled {
		for i := range caps {
			caps[i] = Capability{ID: caps[i].ID, State: CapOff, Reason: "integration disabled"}
		}
		return
	}
	if len(v.DisabledCaps) == 0 {
		return
	}
	disabled := make(map[string]bool, len(v.DisabledCaps))
	for _, id := range v.DisabledCaps {
		disabled[id] = true
	}
	for i := range caps {
		if disabled[caps[i].ID] {
			caps[i] = Capability{ID: caps[i].ID, State: CapOff, Reason: "disabled"}
		}
	}
}

// envSecretPresent reports whether ref is both configured (non-empty) and
// actually stored, per env.SecretPresent (nil-safe: an unset callback reads
// as "nothing is stored", not a panic).
func envSecretPresent(env capEnv, ref string) bool {
	return ref != "" && env.SecretPresent != nil && env.SecretPresent(ref)
}

// envManagedBlobPresent is the nil-safe form of env.ManagedBlobPresent.
func envManagedBlobPresent(env capEnv, provider string) bool {
	return env.ManagedBlobPresent != nil && env.ManagedBlobPresent(provider)
}

// secretGate reports whether ref names a credential that is actually stored.
// ok is false both for an unconfigured ref ("") and a configured-but-absent
// one (dangling ref); reason is "" iff ok.
func secretGate(ref string, env capEnv) (ok bool, reason string) {
	if ref == "" {
		return false, "no credential configured"
	}
	if !envSecretPresent(env, ref) {
		return false, fmt.Sprintf("secret %q not stored", ref)
	}
	return true, ""
}

// gatedCap builds a Capability that is available (at residency) when ref
// names a stored secret, else needs_setup with why.
func gatedCap(id, ref string, env capEnv, residency string) Capability {
	if ok, reason := secretGate(ref, env); !ok {
		return Capability{ID: id, State: CapNeedsSetup, Reason: reason}
	}
	return Capability{ID: id, State: CapAvailable, Residency: residency}
}

// stringSlice reads a []string out of a Config value that may arrive either
// as a plain Go literal (test/direct construction) or as []any (the shape
// map[string]any takes after a JSON round-trip).
func stringSlice(v any) []string {
	switch vv := v.(type) {
	case []string:
		return vv
	case []any:
		out := make([]string, 0, len(vv))
		for _, e := range vv {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// ─── effectiveIntegrations: stored ∪ derived legacy rows ────────────────────
//
// The killer feature of the Integrations entity is that an operator who never
// opens the surface keeps byte-identical behavior forever: nothing is seeded
// at boot. Instead, effectiveIntegrations computes the STORED rows union rows
// DERIVED on the fly from state that already exists (a secret, a boot-config
// knob, a legacy SiteConfig field) — read-only, nothing here ever persists
// anything. This section is deliberately NOT pure (it reads Server config,
// the secret store, the site-config store): capabilitiesFor above stays
// exactly as it was; this is new code built around it, per the file's own
// design note.

// integrationRow is one entry of the effective integration set: either a
// STORED types.Integration or one synthesized from pre-existing config/secret
// state (Source distinguishes them). Unexported — the derivation's internal
// currency; the wire shape is SetupIntegration (setup_integrations.go), which
// adds the live capability matrix.
type integrationRow struct {
	types.Integration
	Source string `json:"source"` // "stored" | "legacy"
}

// effectiveIntegrations returns the operator's stored integrations union rows
// derived from state that already exists, deterministically ordered
// (category, then id) so the API response and any test are stable regardless
// of map/store iteration order upstream. A nil/erroring Store degrades to
// "no stored rows, no SiteConfig-derived legacy rows" rather than failing —
// this is a read surface, never a gate.
func (s *Server) effectiveIntegrations(ctx context.Context) []integrationRow {
	var sc types.SiteConfig
	if s.cfg.Store != nil {
		if got, err := s.cfg.Store.GetSiteConfig(ctx); err == nil {
			sc = got
		}
	}
	stored := make(map[string]bool, len(sc.Integrations))
	rows := make([]integrationRow, 0, len(sc.Integrations))
	for _, in := range sc.Integrations {
		stored[in.ID] = true
		rows = append(rows, integrationRow{Integration: in, Source: "stored"})
	}
	rows = append(rows, s.legacyIntegrations(ctx, sc, stored)...)
	slices.SortFunc(rows, func(a, b integrationRow) int {
		if c := cmp.Compare(a.Category, b.Category); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return rows
}

// legacyIntegrations derives one row per pre-existing source of truth a
// pre-Integrations-entity Wardyn already reads at dispatch time, so an
// operator's existing setup is never invisible on the Integrations surface.
// stored gates out any id a REAL stored integration already claims (the
// "stored row wins" rule): an operator who explicitly configures an
// integration under one of these ids takes over that slot and stops seeing
// the synthesized duplicate; a stored row under any OTHER id coexists
// alongside these untouched.
func (s *Server) legacyIntegrations(ctx context.Context, sc types.SiteConfig, stored map[string]bool) []integrationRow {
	present := s.presentSecretNames(ctx) // the ONE present-secret map every other verdict is computed from (secrets.go)
	var rows []integrationRow
	add := func(id string, in types.Integration) {
		if stored[id] {
			return
		}
		in.ID = id
		rows = append(rows, integrationRow{Integration: in, Source: "legacy"})
	}

	// ai_provider: direct API keys.
	if present["anthropic-api-key"] {
		add("anthropic_api_key", types.Integration{
			Name: "Anthropic API key", Category: types.IntegrationAIProvider, Type: "anthropic_api_key",
			Credentials: map[string]string{"api_key": "anthropic-api-key"},
		})
	}
	if present["openai-api-key"] {
		add("openai_api_key", types.Integration{
			Name: "OpenAI API key", Category: types.IntegrationAIProvider, Type: "openai_api_key",
			Credentials: map[string]string{"api_key": "openai-api-key"},
		})
	}

	// ai_provider: the two Claude-subscription lanes. These can coexist (a
	// host-mode wardynd may also have a managed blob captured), so they get
	// distinct ids rather than sharing "anthropic_subscription".
	if s.cfg.SubscriptionToken != nil {
		if tok, err := s.cfg.SubscriptionToken.Peek(); err == nil && tok.Value != "" {
			add("anthropic_subscription:resident_host", types.Integration{
				Name: "Claude subscription (resident host)", Category: types.IntegrationAIProvider, Type: "anthropic_subscription",
				Config: mustJSON(map[string]any{"lane": "resident_host"}),
			})
		}
	}
	if s.managedInjectReady("claude-code") {
		add("anthropic_subscription:managed", types.Integration{
			Name: "Claude subscription (managed)", Category: types.IntegrationAIProvider, Type: "anthropic_subscription",
			Config: mustJSON(map[string]any{"lane": "managed"}),
		})
	}

	// ai_provider: Bedrock — ONE row. Reuses setupBedrock's own "is Bedrock
	// touched at all" predicate (region/model/AWS profile/any bedrock secret)
	// rather than re-deriving it a second way.
	if b := s.setupBedrock(present); b.configured() {
		add("bedrock", types.Integration{
			Name: "AWS Bedrock", Category: types.IntegrationAIProvider, Type: "bedrock",
			Config: mustJSON(map[string]any{"lane": "auto", "region": b.Region, "model": b.Model}),
		})
	}

	// scm_host: the GitHub App.
	if present[secretGitHubAppID] && present[secretGitHubAppKey] {
		add("github_app", types.Integration{
			Name: "GitHub App", Category: types.IntegrationSCMHost, Type: "github_app",
			Credentials: map[string]string{"app_id": secretGitHubAppID, "app_key": secretGitHubAppKey},
			Config:      mustJSON(map[string]any{"host": "github.com"}),
		})
	}

	// scm_host: git-pat-<slug>/ssh-key-<slug> secrets, merged with
	// SiteConfig.ScmHosts by host. Built directly (not through add) because
	// each row's id depends on the derived host; gitHostRows applies the same
	// stored-wins gate per row.
	rows = append(rows, gitHostRows(present, sc.ScmHosts, stored)...)

	// artifact_mirror: one row per corp mirror host.
	rows = append(rows, artifactMirrorRows(sc, stored)...)

	// host_proxy: the corporate upstream proxy.
	if sc.UpstreamProxySecretRef != "" {
		add("host_proxy", types.Integration{
			Name: "Corporate upstream proxy", Category: types.IntegrationHostProxy, Type: "host_proxy",
			Credentials: map[string]string{"secret": sc.UpstreamProxySecretRef},
		})
	}

	return rows
}

// gitHostCreds accumulates the pat/ssh-key secret names discovered for one
// host while scanning the git-pat-<slug>/ssh-key-<slug> secret-name
// convention (the same convention setup.go's scmProviderCheck documents).
type gitHostCreds struct{ pat, sshKey string }

// credentials builds the git_host Credentials map capabilitiesFor's git_host
// case reads ("pat"/"ssh_key"), or nil when neither lane is configured.
func (c gitHostCreds) credentials() map[string]string {
	m := map[string]string{}
	if c.pat != "" {
		m["pat"] = c.pat
	}
	if c.sshKey != "" {
		m["ssh_key"] = c.sshKey
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// hostFromSecretSlug reverses the "<host-slug>" naming convention
// (dots -> hyphens, e.g. git-pat-github-com) documented in setup.go's
// scmProviderCheck: strip prefix, turn hyphens back into dots.
//
// ponytail: lossy for a host whose own name contains a hyphen (e.g.
// "ghe-prod.corp.com" slugs to "ghe-prod-corp-com" and would round-trip
// wrong) — the same ambiguity the existing convention already accepts
// (scmProviderCheck treats these as opaque display names, never a validated
// host). Upgrade path: store the real host alongside the secret if an exact
// reverse ever matters.
func hostFromSecretSlug(name, prefix string) string {
	return strings.ReplaceAll(strings.TrimPrefix(name, prefix), "-", ".")
}

// gitHostRows derives one scm_host/git_host row per host named by a
// git-pat-<slug>/ssh-key-<slug> secret OR a SiteConfig.ScmHosts entry, merged
// by host: an operator's declared ScmHosts host with no credential yet still
// gets a row (egress_host only; clone:pat/clone:ssh read needs_setup), and a
// host that also has a credential carries it.
func gitHostRows(secretNames map[string]bool, scmHosts []string, stored map[string]bool) []integrationRow {
	byHost := map[string]gitHostCreds{}
	for n := range secretNames {
		switch {
		case strings.HasPrefix(n, "git-pat-"):
			h := hostFromSecretSlug(n, "git-pat-")
			c := byHost[h]
			c.pat = n
			byHost[h] = c
		case strings.HasPrefix(n, "ssh-key-"):
			h := hostFromSecretSlug(n, "ssh-key-")
			c := byHost[h]
			c.sshKey = n
			byHost[h] = c
		}
	}
	for _, h := range scmHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if _, ok := byHost[h]; !ok {
			byHost[h] = gitHostCreds{}
		}
	}
	var rows []integrationRow
	for host, c := range byHost {
		id := "git_host:" + host
		if stored[id] {
			continue
		}
		rows = append(rows, integrationRow{Integration: types.Integration{
			ID: id, Name: host, Category: types.IntegrationSCMHost, Type: "git_host",
			Credentials: c.credentials(),
		}, Source: "legacy"})
	}
	return rows
}

// artifactMirrorRows derives one artifact_mirror row per corp mirror/relay HOST
// (several redirects — ecosystem-scoped or network-only alike — commonly share
// one destination), mirroring planArtifactRedirect's dedupe-by-host shape
// (artifact_redirect.go). A row's token credential is the FIRST TokenSecretRef
// seen for that host in EgressRedirects' stored order — the common case is one
// token per host; a host with genuinely divergent per-redirect tokens still
// redirects every one of them (Config carries every ecosystem it touches), it
// just reports one representative credential. Ecosystems is empty for a purely
// network-only host (no package-manager config file, egress substitution only).
func artifactMirrorRows(sc types.SiteConfig, stored map[string]bool) []integrationRow {
	type hostEcos struct {
		ecosystems []string
		token      string
	}
	byHost := map[string]*hostEcos{}
	for _, r := range sc.EgressRedirects {
		host := strings.ToLower(workspacescan.HostOf(r.To))
		if host == "" {
			continue
		}
		he, ok := byHost[host]
		if !ok {
			he = &hostEcos{token: r.TokenSecretRef}
			byHost[host] = he
		}
		if r.Ecosystem != "" {
			he.ecosystems = append(he.ecosystems, r.Ecosystem)
		}
	}
	var rows []integrationRow
	for host, he := range byHost {
		id := "artifact_mirror:" + host
		if stored[id] {
			continue
		}
		var creds map[string]string
		if he.token != "" {
			creds = map[string]string{"token": he.token}
		}
		rows = append(rows, integrationRow{Integration: types.Integration{
			ID: id, Name: host, Category: types.IntegrationArtifactMirror, Type: "artifact_mirror",
			Credentials: creds,
			Config:      mustJSON(map[string]any{"ecosystems": he.ecosystems}),
		}, Source: "legacy"})
	}
	return rows
}

// ─── Integration WRITES: validation + the run/composer resolution ladder ────
//
// Everything below builds ON TOP of effectiveIntegrations/capabilitiesFor
// above without changing either: validateIntegrationWrite backs the write
// endpoints (setup_integrations.go); resolveIntegrationRef/
// defaultAgentRunsIntegration back the run-time resolution ladder
// (llmcred.go); WardynFeaturesBackend backs the composer-registry boot
// derivation (cmd/wardynd/composer.go).

// knownIntegrationTypes is the closed type set PER CATEGORY a write may name —
// a hand-kept mirror of capabilitiesFor's switch above (frozen; see this
// file's own doc comment) rather than a reflection-based derivation, so a type
// this list is missing fails LOUD here ("invalid type") instead of silently
// passing validation and landing on "no capabilities" in the live matrix.
var knownIntegrationTypes = map[types.IntegrationCategory]map[string]bool{
	types.IntegrationAIProvider: {
		"anthropic_api_key": true, "anthropic_subscription": true, "bedrock": true,
		"openai_api_key": true, "azure_openai": true,
	},
	types.IntegrationSCMHost:        {"github_app": true, "git_host": true},
	types.IntegrationArtifactMirror: {"artifact_mirror": true},
	types.IntegrationHostProxy:      {"host_proxy": true},
}

// genericIntegrationCategories are the categories whose behavior does NOT
// depend on Type (see types.IntegrationCategory): the row's own hosts, header
// and secret ref are the whole contract, so Type is an open slug validated for
// shape only. Enumerating ~35 provider types here would buy nothing but a
// second hand-kept mirror of the UI catalog to drift against.
var genericIntegrationCategories = map[types.IntegrationCategory]bool{
	types.IntegrationPackageFeed:       true,
	types.IntegrationContainerRegistry: true,
	types.IntegrationCloudProvider:     true,
	types.IntegrationDataStore:         true,
	types.IntegrationMCPServer:         true,
	types.IntegrationWorkTracking:      true,
	types.IntegrationObservability:     true,
	types.IntegrationOtherService:      true,
}

// maxIntegrationHosts bounds one integration's host list. Well under the
// per-run 64-host egress cap the requirements surface already warns at, so a
// single integration can never be the thing that blows it.
const maxIntegrationHosts = 32

// validateIntegrationHosts checks the host list against the SAME shape rule
// every operator-supplied policy allowlist entry runs (proxy.ValidDomainEntry
// — exact host, leading-"*." wildcard, optional ":port"), because these
// entries become exactly that: allowlist entries on a granted run.
//
// hasHeader tightens it: proxy-side injection matches EXACT allowlist entries
// only (Policy.AllowedExactHost, deliberately, so a credential can never leak
// to a wildcard-matched host), so a wildcard on a header-delivering integration
// would open the path and silently never present the credential. Reject it at
// write time and say why, rather than ship a row that lies about being
// credentialed.
func validateIntegrationHosts(hosts []string, hasHeader bool) error {
	if len(hosts) > maxIntegrationHosts {
		return fmt.Errorf("hosts: %d entries exceeds the %d-host limit", len(hosts), maxIntegrationHosts)
	}
	for i, h := range hosts {
		if err := proxy.ValidDomainEntry(h); err != nil {
			return fmt.Errorf("hosts[%d]: %w", i, err)
		}
		if hasHeader && strings.HasPrefix(strings.TrimSpace(h), "*.") {
			return fmt.Errorf("hosts[%d]: %q is a wildcard, and a credential header is only added to an EXACT host — "+
				"the proxy would open the path but never present the credential. Name the hosts individually, or clear the header", i, h)
		}
	}
	return nil
}

// validateIntegrationCredentialDelivery checks the proxy-injected delivery
// triple (Header, Format, and the secret the header carries).
//
// Header is a trust boundary: it is written verbatim onto a forwarded request,
// so it must be a real HTTP field-name token (egress.ValidHeaderName — which
// excludes CR/LF, ':' and space by construction). Format is the other half of
// the same wire value: fmt.Sprintf substitutes the secret into it, so it needs
// exactly one %s (a format with none silently DROPS the credential and sends a
// bare prefix; one with two renders "%!s(MISSING)") and no CR/LF of its own.
func validateIntegrationCredentialDelivery(in types.Integration) error {
	if in.Header == "" {
		if in.Format != "" {
			return fmt.Errorf("format: set without a header — nothing presents this value")
		}
		return nil
	}
	if !egress.ValidHeaderName(in.Header) {
		return fmt.Errorf("header: %q is not a valid HTTP header name "+
			"(letters, digits and !#$%%&'*+-.^_`|~ only — no spaces, no ':', no line breaks)", in.Header)
	}
	if in.Format != "" {
		if strings.Count(in.Format, "%s") != 1 || strings.Count(in.Format, "%") != 1 {
			return fmt.Errorf("format: %q must contain exactly one %%s (where the secret goes) and no other verb", in.Format)
		}
		if strings.ContainsAny(in.Format, "\r\n") {
			return fmt.Errorf("format: must not contain a line break")
		}
	}
	if in.Credentials[types.IntegrationCredentialToken] == "" {
		return fmt.Errorf("credentials[%s]: a header is set but names no secret to present in it", types.IntegrationCredentialToken)
	}
	return nil
}

// validIntegrationDefaultFor is DefaultFor's closed value set (see
// types.Integration's doc comment: "agent_runs" and/or "wardyn_features").
var validIntegrationDefaultFor = map[string]bool{"agent_runs": true, "wardyn_features": true}

// validateIntegrationWrite enforces an operator-authored Integration's
// structural + security invariants before it is persisted (PUT
// /integrations/{id}, and defensively on adopt): id shape (secretNameRE — the
// same identifier rule secret names use), category/type against the known
// sets above, every credential value a real non-reserved secret name
// (validSecretRef — the same rule site-config's *SecretRef fields use),
// DefaultFor closed to {agent_runs, wardyn_features}, and the two per-type
// Config checks this codebase already has an established rule for: an
// artifact_mirror's ecosystems must be the same closed set ArtifactOverrides
// uses (site_config.go), and a bedrock integration may not half-override
// region/model — the identical hazard
// `git show ecc1903~1:internal/api/llmcred.go`'s
// TestValidateWorkspaceLLMCred_Rejections pinned for the pre-Integration
// shape (a region-scoped inference profile 403s at invoke with only one set).
// Other per-type Config knobs (e.g. a subscription's lane) are deliberately
// left permissive: capabilitiesFor documents an unrecognized value as a
// graceful fallback, not an error, and this validator should not be stricter
// than the reader.
func validateIntegrationWrite(in types.Integration) error {
	if !secretNameRE.MatchString(in.ID) {
		return fmt.Errorf("id: invalid identifier %q (lowercase alphanumeric, '.', '_', '-', 1-128 chars)", in.ID)
	}
	switch knownTypes, typed := knownIntegrationTypes[in.Category]; {
	case typed:
		if !knownTypes[in.Type] {
			return fmt.Errorf("type: %q is not a known %s type", in.Type, in.Category)
		}
	case genericIntegrationCategories[in.Category]:
		// Open type set — shape only (see genericIntegrationCategories).
		if !secretNameRE.MatchString(in.Type) {
			return fmt.Errorf("type: invalid identifier %q (lowercase alphanumeric, '.', '_', '-', 1-128 chars)", in.Type)
		}
	default:
		return fmt.Errorf("category: unknown %q", in.Category)
	}
	for role, ref := range in.Credentials {
		if ref != "" && !validSecretRef(ref) {
			return fmt.Errorf("credentials[%s]: invalid or reserved secret name %q", role, ref)
		}
	}
	if err := validateIntegrationHosts(in.Hosts, in.Header != ""); err != nil {
		return err
	}
	if err := validateIntegrationCredentialDelivery(in); err != nil {
		return err
	}
	if len(in.Docs) > 2048 {
		return fmt.Errorf("docs: too long (%d bytes, max 2048)", len(in.Docs))
	}
	for _, d := range in.DefaultFor {
		if !validIntegrationDefaultFor[d] {
			return fmt.Errorf("default_for: unknown %q (want agent_runs and/or wardyn_features)", d)
		}
	}
	if len(in.Config) == 0 {
		return nil
	}
	var cfg map[string]any
	if err := json.Unmarshal(in.Config, &cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if in.Type == "bedrock" {
		region, _ := cfg["region"].(string)
		model, _ := cfg["model"].(string)
		if (region == "") != (model == "") {
			return fmt.Errorf("config: bedrock region and model must be set together (a region-scoped inference profile 403s at invoke with only one)")
		}
	}
	if in.Category == types.IntegrationArtifactMirror {
		for _, eco := range stringSlice(cfg["ecosystems"]) {
			if !validArtifactEcosystems[eco] {
				return fmt.Errorf("config.ecosystems: unknown ecosystem %q", eco)
			}
		}
	}
	return nil
}

// applyDefaultForRadio enforces DefaultFor's RADIO semantics across rows: each
// value in newDefaultFor is a single-select mark, so setting it on the row
// named id CLEARS that same value from every OTHER row's DefaultFor in the
// same write — never a 409, per the approved spec. Mutates rows in place
// (mirroring applyDisabled's own in-place style above); a row with nothing to
// clear is left untouched (including its slice identity, so an unrelated
// write never appears to "touch" every other row).
func applyDefaultForRadio(rows []types.Integration, id string, newDefaultFor []string) {
	if len(newDefaultFor) == 0 {
		return
	}
	marks := make(map[string]bool, len(newDefaultFor))
	for _, m := range newDefaultFor {
		marks[m] = true
	}
	for i := range rows {
		if rows[i].ID == id || len(rows[i].DefaultFor) == 0 {
			continue
		}
		cleared := slices.DeleteFunc(slices.Clone(rows[i].DefaultFor), func(m string) bool { return marks[m] })
		if len(cleared) != len(rows[i].DefaultFor) {
			rows[i].DefaultFor = cleared
		}
	}
}

// resolveIntegrationRef resolves ref against the EFFECTIVE integration set
// (stored ∪ legacy-derived — effectiveIntegrations above) into the concrete
// types.Integration it names. Effective, not stored-only, so a run/workspace
// binding "just works" against a well-known legacy id (e.g.
// "anthropic_api_key") with no adoption step required first — the entire
// point of deriving legacy rows in the first place. ok=false when ref is
// empty or names nothing at all.
func (s *Server) resolveIntegrationRef(ctx context.Context, ref string) (types.Integration, bool) {
	if ref == "" {
		return types.Integration{}, false
	}
	for _, row := range s.effectiveIntegrations(ctx) {
		if row.ID == ref {
			return row.Integration, true
		}
	}
	return types.Integration{}, false
}

// defaultAgentRunsIntegration returns the STORED ai_provider integration
// marked DefaultFor: agent_runs, optionally narrowed to onlyType (""=any
// type). Only a STORED row can carry DefaultFor at all (a legacy-derived row
// is never persisted, so it never has one — see types.Integration's doc
// comment), which is exactly what keeps this tier a no-op with zero stored
// integrations regardless of onlyType. ok=false when none is marked.
func (s *Server) defaultAgentRunsIntegration(ctx context.Context, onlyType string) (types.Integration, bool) {
	var sc types.SiteConfig
	if s.cfg.Store != nil {
		if got, err := s.cfg.Store.GetSiteConfig(ctx); err == nil {
			sc = got
		}
	}
	for _, in := range sc.Integrations {
		if in.Category != types.IntegrationAIProvider || !slices.Contains(in.DefaultFor, "agent_runs") {
			continue
		}
		if onlyType != "" && in.Type != onlyType {
			continue
		}
		return in, true
	}
	return types.Integration{}, false
}

// WardynFeaturesBackend returns the STORED ai_provider integration marked
// DefaultFor: wardyn_features whose wardyn_features capability reads
// "available" right now (capabilitiesFor above — never a needs_setup/off/
// impossible one), for cmd/wardynd's composer-registry boot derivation
// (WARDYN_COMPOSER_CONFIG unset — see cmd/wardynd/composer.go). Exported as a
// plain function of an already-fetched SiteConfig plus the same live signals
// liveCapEnv folds from Server config, because cmd/wardynd builds the
// composer registry BEFORE the api.Server exists (Config.Composer is
// late-bound INTO it once built) — there is no live Server here to read them
// from. ok=false (no eligible integration) is the signal to keep today's
// behavior: no registry, compose 404s honestly.
func WardynFeaturesBackend(sc types.SiteConfig, secretPresent func(string) bool, bedrockRegionSet, bedrockModelSet bool, managedBlobPresent func(string) bool) (types.Integration, bool) {
	env := capEnv{
		SecretPresent: secretPresent, BedrockRegionSet: bedrockRegionSet,
		BedrockModelSet: bedrockModelSet, ManagedBlobPresent: managedBlobPresent,
	}
	for _, in := range sc.Integrations {
		if in.Category != types.IntegrationAIProvider || !slices.Contains(in.DefaultFor, "wardyn_features") {
			continue
		}
		for _, c := range capabilitiesFor(toIntegrationView(integrationRow{Integration: in}), env) {
			if c.ID == "wardyn_features" && c.State == CapAvailable {
				return in, true
			}
		}
	}
	return types.Integration{}, false
}
