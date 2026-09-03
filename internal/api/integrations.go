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

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// integrations.go computes the Integrations screen's capability matrix: for
// one named connection to a system outside Wardyn (an "integration"), what it
// powers, whether that's ON / OFF / NEEDS_SETUP / an impossible protocol fact
// right now, why, and where the credential lives when it IS wired. It is a
// PURE function over the stored row (capabilitiesFor) — no storage, no HTTP,
// no wiring: capEnv carries every external fact it needs.
//
// It reads the base-component shape (types.Integration) DIRECTLY: kind routes,
// the row's own secret rows carry their delivery, and Egress says where the
// system lives. The pre-base-component shim view is gone.

// capEnv carries the external readiness signals capabilitiesFor cannot derive
// from the stored row alone: secret-store presence, host detection, and
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
	// BedrockCredentialPresent mirrors SetupBedrock's four-lane credential OR
	// (bearer, captured AWS SSO, ~/.aws mount, resident SigV4 keys) — the
	// precondition resolveBedrockAuth ALSO requires beyond region+model. Without
	// this, bedrockCaps read "available" off region+model alone, so a
	// wizard-completed Bedrock integration with zero credentials of any kind
	// still showed model_api/tool:claude-code/wardyn_features as available —
	// both surfaces (GET /integrations, SetupStatus.Integrations) believing a
	// run that would silently get no Bedrock transport at all.
	BedrockCredentialPresent bool
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
	ID        string   `json:"id"`
	State     CapState `json:"state"`
	Reason    string   `json:"reason,omitempty"`
	Residency string   `json:"residency,omitempty"`
}

// model_api / wardyn_features copy canon not already owned by the harness
// catalog's tool-compatibility facts (harness.go) — verbatim from the
// Integrations screen mock (scratchpad mockup/wardyn-integrations.js, the T
// object). Do not paraphrase; keep in sync with that file.
const (
	reasonXSubDirect      = "A subscription token is accepted only for Claude-Code-shaped requests; anything else comes back 429. That's Anthropic's gate, not a Wardyn setting."
	reasonBedrockFeatures = "Wardyn's own features reach Bedrock through the AWS credential chain — the same lane this integration uses."
	// reasonBedrockUnset states the real dispatch behavior: without both a
	// region and a model id, resolveBedrockAuth never reports ready and the run
	// falls through to whatever other lane it has (usually none).
	reasonBedrockUnset = "Region and model id are unset — a run can't reach Bedrock until both are set."
	// reasonBedrockNoCreds: region+model are set but resolveBedrockAuth's
	// credential ladder (bearer, captured AWS SSO, ~/.aws mount, resident SigV4
	// keys) has nothing to offer — a run still gets no Bedrock transport.
	reasonBedrockNoCreds = "Region and model are set, but no AWS credential is configured — add a bearer key, resident access keys, an AWS SSO login, or a ~/.aws mount."
	// reasonHostCLIOptIn is the canon note for the host-CLI lane's Wardyn-features
	// cell — kept in sync with ui/src/app/lib/integrations.ts's CAPS.sub hostCli
	// note, not the (stale) mock: nothing in the console switches this lane on,
	// and nothing at the server flips it either — the resident_host cell is
	// unconditionally CapOff (the WARDYN_COMPOSER_CONFIG switch went with the
	// composer).
	reasonHostCLIOptIn = "Off for this lane — no switch in this console turns it on."
)

// capabilitiesFor computes the full capability matrix for one integration.
// Pure: no storage, no HTTP, no wiring — env carries every external fact it
// needs. Routing is on KIND alone: any slug outside the closed set is a
// GENERIC connection whose behavior is fully described by its own row.
func capabilitiesFor(in types.Integration, env capEnv) []Capability {
	// A GENERIC-kind row's behavior is fully described by its own egress +
	// secret delivery (genericCaps), never by code — checked FIRST; the closed
	// kinds fall through to the bespoke matrices below.
	//
	// 0.5 refuses to WRITE a generic kind (validateIntegrationWrite), but a row
	// stored under an earlier release still deserializes and is still injected
	// by integrations_run.go, so this branch stays: a legacy row has to keep
	// reporting its real capabilities on GET rather than falling through to a
	// matrix that knows nothing about it.
	if genericIntegrationKind(in.Kind) {
		caps := genericCaps(in, env)
		applyDisabled(caps, in)
		return caps
	}
	var caps []Capability
	switch in.Kind {
	case types.IntegrationKindAnthropicAPIKey:
		caps = directKeyCaps(in, env, "claude-code", "codex-cli")
	case types.IntegrationKindAnthropicSubscription:
		caps = subscriptionCaps(in, env)
	case types.IntegrationKindBedrock:
		caps = bedrockCaps(in, env)
	case types.IntegrationKindOpenAIAPIKey:
		caps = directKeyCaps(in, env, "codex-cli", "claude-code")
	case types.IntegrationKindGitHubApp:
		caps = githubAppCaps(in, env)
	case types.IntegrationKindGitHost:
		caps = []Capability{
			gatedCap("clone:pat", in.RoleSecret("pat"), env, "resident_env"),
			gatedCap("clone:ssh", in.RoleSecret("ssh_key"), env, "resident_env"),
			{ID: "egress_host", State: CapAvailable},
		}
	}
	applyDisabled(caps, in)
	return caps
}

// reasonNoDeliveryLane is the honest line for a generic integration whose
// secrets declare no proxy_header delivery: Wardyn opens the path, and that is
// genuinely the difference between a run reaching the system and not reaching
// it at all — but the resident and brokered lanes are hand-written per provider
// (see residentDeliveryRefusal, integrations_write.go), so nothing here can
// deliver a credential.
const reasonNoDeliveryLane = "Wardyn can open the path to these hosts. Delivering this system's credential into the sandbox isn't built — the resident and brokered lanes are hand-written per provider."

// genericCaps is the matrix for a GENERIC-kind integration: one whose behavior
// is fully described by its own egress and secret delivery rather than by a
// kind this file switches on. Two honest cells, both derived from the stored
// row:
//
//   - egress_host — the hosts become reachable for a run granted this row.
//     needs_setup while the row names none: an integration with no hosts opens
//     nothing, and saying "available" there would be the lie this whole file
//     exists to avoid.
//   - credential — proxy-injected when a header names a stored secret;
//     otherwise the stated "egress only" fact, which is what the two groups
//     that authenticate outside HTTP (cloud providers, data stores) get. A
//     header names a delivery MECHANISM, not a destination: with no hosts
//     there is nothing for the runtime fold to present it AT, so this cell
//     needs_setup with the SAME no-hosts reason as egress_host rather than
//     the header-alone "available" — the identical lie egress_host above
//     already refuses to tell.
func genericCaps(in types.Integration, env capEnv) []Capability {
	noHosts := len(in.Egress) == 0
	reach := Capability{ID: "egress_host", State: CapAvailable}
	if noHosts {
		reach = Capability{ID: "egress_host", State: CapNeedsSetup, Reason: "No hosts named yet — nothing becomes reachable."}
	}
	cred := Capability{ID: "credential", State: CapImpossible, Reason: reasonNoDeliveryLane}
	// ROLE-AGNOSTIC (base-component model): the row's proxy_header-delivered
	// secret IS its credential, whatever role name it carries — delivery is the
	// contract, the role is a label. Same rule the two runtime seams follow
	// (applyIntegrationInjection, resolveRedirectToken).
	if secret, _, _, ok := in.HeaderSecret(); ok {
		cred = gatedCap("credential", secret, env, "proxy_injected")
		if noHosts {
			cred = Capability{ID: "credential", State: CapNeedsSetup, Reason: reach.Reason}
		}
	}
	return []Capability{reach, cred}
}

// directKeyCaps builds the shared 4-cell matrix for the two direct-api-key ai
// types (anthropic_api_key, openai_api_key): model_api, the driven tool, and
// wardyn_features all gate on the SAME api_key secret and share one reason
// when it's missing; the OTHER tool is an unconditional protocol-fact
// impossibility sourced from the harness catalog (harness.go) — it speaks a
// different API, no setup state changes that.
func directKeyCaps(in types.Integration, env capEnv, drivenHarness, otherHarness string) []Capability {
	ref := in.RoleSecret("api_key")
	const residency = "proxy_injected"
	return []Capability{
		gatedCap("model_api", ref, env, residency),
		gatedCap("tool:"+drivenHarness, ref, env, residency),
		{ID: "tool:" + otherHarness, State: CapImpossible, Reason: harnessProviderReason(otherHarness, in.Kind)},
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
func subscriptionCaps(in types.Integration, env capEnv) []Capability {
	lane, _ := in.Config["lane"].(string)
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
	// through the sandbox wire — but ONLY once a managed token is actually
	// connected (PLATFORM-API-4): without this gate the cell read "available"
	// for a lane that is not — the exact drift bedrockCaps below refuses to
	// tell — because tool:claude-code (right below) already needs the SAME
	// managed-blob signal to leave needs_setup.
	features := Capability{ID: "wardyn_features", State: CapAvailable, Residency: residency}
	switch {
	case lane == "resident_host":
		features = Capability{ID: "wardyn_features", State: CapOff, Reason: reasonHostCLIOptIn, Residency: residency}
	case !envManagedBlobPresent(env, "anthropic"):
		features = Capability{ID: "wardyn_features", State: CapNeedsSetup, Reason: "no managed Claude subscription connected", Residency: residency}
	}
	return []Capability{
		{ID: "model_api", State: CapImpossible, Reason: reasonXSubDirect},
		{ID: "tool:claude-code", State: claudeState, Reason: claudeReason, Residency: residency},
		{ID: "tool:codex-cli", State: CapImpossible, Reason: harnessProviderReason("codex-cli", in.Kind)},
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
//
// Region/model are read from THE INTEGRATION FIRST, with the boot flags
// (env.Bedrock*Set) as the fallback — the same precedence resolveBedrockAuth
// applies at dispatch (`cmp.Or(ws.Region, s.cfg.BedrockRegion)`, runs_bedrock.go:
// "a selection wins only the fields it sets"). Reading the boot flags alone is
// what made a wizard-completed Bedrock integration report needs_setup forever on
// a deployment that never set WARDYN_BEDROCK_* — while its runs authenticated
// fine.
func bedrockCaps(in types.Integration, env capEnv) []Capability {
	lane, _ := in.Config["auth_lane"].(string)
	residency := "resident_env"
	if lane == "bearer" {
		residency = "proxy_injected"
	}
	region, _ := in.Config["region"].(string)
	model, _ := in.Config["model"].(string)
	regionSet := strings.TrimSpace(region) != "" || env.BedrockRegionSet
	modelSet := strings.TrimSpace(model) != "" || env.BedrockModelSet
	// Region+model gate EVERY Bedrock cell, not just Wardyn's own features:
	// resolveBedrockAuth (runs_bedrock.go) returns an unready bedrockAuth when
	// either is empty, and dispatch then falls silently to the api-key lane. A
	// cell that reads "available" while the run it describes cannot reach
	// Bedrock at all is exactly the drift this matrix exists to prevent.
	if !regionSet || !modelSet {
		return []Capability{
			{ID: "model_api", State: CapNeedsSetup, Reason: reasonBedrockUnset},
			{ID: "tool:claude-code", State: CapNeedsSetup, Reason: reasonBedrockUnset},
			{ID: "tool:codex-cli", State: CapImpossible, Reason: harnessProviderReason("codex-cli", in.Kind)},
			{ID: "wardyn_features", State: CapNeedsSetup, Reason: reasonBedrockUnset},
		}
	}
	// A configured region+model with NO credential anywhere in
	// resolveBedrockAuth's ladder is exactly as unreachable as an unset
	// region/model — same drift this matrix exists to prevent.
	if !env.BedrockCredentialPresent {
		return []Capability{
			{ID: "model_api", State: CapNeedsSetup, Reason: reasonBedrockNoCreds},
			{ID: "tool:claude-code", State: CapNeedsSetup, Reason: reasonBedrockNoCreds},
			{ID: "tool:codex-cli", State: CapImpossible, Reason: harnessProviderReason("codex-cli", in.Kind)},
			{ID: "wardyn_features", State: CapNeedsSetup, Reason: reasonBedrockNoCreds},
		}
	}
	return []Capability{
		{ID: "model_api", State: CapAvailable, Residency: residency},
		{ID: "tool:claude-code", State: CapAvailable, Residency: residency},
		{ID: "tool:codex-cli", State: CapImpossible, Reason: harnessProviderReason("codex-cli", in.Kind)},
		{ID: "wardyn_features", State: CapAvailable, Residency: residency, Reason: reasonBedrockFeatures},
	}
}

// githubAppCaps builds the github_app matrix: clone:app is a single brokered
// capability that needs BOTH credentials (an installation token minted from
// only one half is not a thing GitHub offers); egress_host is unconditional
// (naming the host in the allowlist needs no credential).
func githubAppCaps(in types.Integration, env capEnv) []Capability {
	okID, _ := secretGate(in.RoleSecret("app_id"), env)
	okKey, _ := secretGate(in.RoleSecret("app_key"), env)
	clone := Capability{ID: "clone:app", State: CapAvailable, Residency: "brokered"}
	if !okID || !okKey {
		clone = Capability{ID: "clone:app", State: CapNeedsSetup, Reason: "needs both app_id and app_key credentials"}
	}
	return []Capability{clone, {ID: "egress_host", State: CapAvailable}}
}

// applyDisabled mutates caps in place (slices share their backing array, so
// no return value is needed): a whole-integration Disabled forces EVERY cell
// off with one uniform reason, regardless of what it would otherwise be —
// including a protocol-fact impossibility, per the approved spec ("every
// cell off"). Absent that, a capability named in DisabledCaps is forced off
// individually. Both overrides clear Residency: nothing is actually wired
// while off.
func applyDisabled(caps []Capability, in types.Integration) {
	if in.Disabled {
		for i := range caps {
			caps[i] = Capability{ID: caps[i].ID, State: CapOff, Reason: "integration disabled"}
		}
		return
	}
	if len(in.DisabledCapabilities) == 0 {
		return
	}
	disabled := make(map[string]bool, len(in.DisabledCapabilities))
	for _, id := range in.DisabledCapabilities {
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

// UnmarshalJSON decodes the embedded Integration (whose own custom
// UnmarshalJSON would otherwise be PROMOTED onto this wrapper and silently
// swallow the wrapper's fields) and then the wrapper's source field.
func (r *integrationRow) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, &r.Integration); err != nil {
		return err
	}
	var src struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(b, &src); err != nil {
		return err
	}
	r.Source = src.Source
	return nil
}

// effectiveIntegrations returns the operator's stored integrations union rows
// derived from state that already exists, deterministically ordered
// (category, then id) so the API response and any test are stable regardless
// of map/store iteration order upstream. A nil/erroring Store degrades to
// "no stored rows, no SiteConfig-derived legacy rows" rather than failing —
// this is a read surface, never a gate.
//
// present/bedrock are legacyIntegrations' two live signals, taken as
// parameters (PLATFORM-API-7) rather than recomputed here: a caller resolving
// several refs in one request (resolveIntegrationRef, launchRecordRun)
// computes each ONCE and reuses it, instead of paying a full secret listing +
// Bedrock age-decrypt probe per call — /setup/status, the endpoint the wizard
// polls, used to redo both 2-3x per request this way.
func (s *Server) effectiveIntegrations(ctx context.Context, present map[string]bool, bedrock SetupBedrock) []integrationRow {
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
	rows = append(rows, s.legacyIntegrations(ctx, sc, stored, present, bedrock)...)
	slices.SortFunc(rows, func(a, b integrationRow) int {
		if c := cmp.Compare(integrationGroup(a.Kind), integrationGroup(b.Kind)); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return rows
}

// integrationGroup is the surface's own grouping, DERIVED from kind (the
// stored category is gone by design): the AI provider flavors first, then
// source control, then everything else as one flat "Connections" set. The
// returned rank doubles as the sort key above, so the API response and the
// three groups the screen renders can never disagree about what goes where.
func integrationGroup(kind string) int {
	switch {
	case types.AIProviderKind(kind):
		return 0
	case kind == types.IntegrationKindGitHubApp || kind == types.IntegrationKindGitHost:
		return 1
	default:
		return 2
	}
}

// legacyIntegrations derives one row per pre-existing source of truth a
// pre-Integrations-entity Wardyn already reads at dispatch time, so an
// operator's existing setup is never invisible on the Integrations surface.
// stored gates out any id a REAL stored integration already claims (the
// "stored row wins" rule): an operator who explicitly configures an
// integration under one of these ids takes over that slot and stops seeing
// the synthesized duplicate; a stored row under any OTHER id coexists
// alongside these untouched. present is the ONE present-secret map every
// other verdict is computed from (secrets.go); bedrock is setupBedrock's own
// "is Bedrock touched at all" verdict — both taken as parameters, not
// recomputed (PLATFORM-API-7; see effectiveIntegrations' doc).
func (s *Server) legacyIntegrations(ctx context.Context, sc types.SiteConfig, stored map[string]bool, present map[string]bool, bedrock SetupBedrock) []integrationRow {
	var rows []integrationRow
	add := func(id string, in types.Integration) {
		if stored[id] {
			return
		}
		in.ID = id
		rows = append(rows, integrationRow{Integration: in, Source: "legacy"})
	}

	// AI providers: direct API keys. The api_key secret rides the provider's
	// proxy-header injection convention (types.AIKeyDelivery — the same facts
	// the harness catalog's Gateway rows encode), stated on the row so the
	// derived shape says what actually happens.
	if present["anthropic-api-key"] {
		add("anthropic_api_key", types.Integration{
			Name: "Anthropic API key", Kind: types.IntegrationKindAnthropicAPIKey,
			Secrets: []types.IntegrationSecret{{Role: "api_key", SecretName: "anthropic-api-key",
				Delivery: types.AIKeyDelivery(types.IntegrationKindAnthropicAPIKey)}},
		})
	}
	if present["openai-api-key"] {
		add("openai_api_key", types.Integration{
			Name: "OpenAI API key", Kind: types.IntegrationKindOpenAIAPIKey,
			Secrets: []types.IntegrationSecret{{Role: "api_key", SecretName: "openai-api-key",
				Delivery: types.AIKeyDelivery(types.IntegrationKindOpenAIAPIKey)}},
		})
	}

	// The two Claude-subscription lanes. These can coexist (a host-mode
	// wardynd may also have a managed blob captured), so they get distinct ids
	// rather than sharing "anthropic_subscription".
	if s.cfg.SubscriptionToken != nil {
		if tok, err := s.cfg.SubscriptionToken.Peek(); err == nil && tok.Value != "" {
			add("anthropic_subscription:resident_host", types.Integration{
				Name: "Claude subscription (resident host)", Kind: types.IntegrationKindAnthropicSubscription,
				Config: map[string]any{"lane": "resident_host"},
			})
		}
	}
	if s.managedInjectReady("claude-code") {
		add("anthropic_subscription:managed", types.Integration{
			Name: "Claude subscription (managed)", Kind: types.IntegrationKindAnthropicSubscription,
			Config: map[string]any{"lane": "managed"},
		})
	}

	// Bedrock — ONE row. Reuses setupBedrock's own "is Bedrock touched at all"
	// predicate (region/model/AWS profile/any bedrock secret) rather than
	// re-deriving it a second way.
	if bedrock.configured() {
		add("bedrock", types.Integration{
			Name: "AWS Bedrock", Kind: types.IntegrationKindBedrock,
			Config: map[string]any{"auth_lane": "auto", "region": bedrock.Region, "model": bedrock.Model},
		})
	}

	// Source control: the GitHub App. Both halves ride the broker's own
	// bespoke lane (installation tokens minted control-plane-side), so neither
	// declares a delivery.
	if present[secretGitHubAppID] && present[secretGitHubAppKey] {
		add("github_app", types.Integration{
			Name: "GitHub App", Kind: types.IntegrationKindGitHubApp,
			Secrets: []types.IntegrationSecret{
				{Role: "app_id", SecretName: secretGitHubAppID},
				{Role: "app_key", SecretName: secretGitHubAppKey},
			},
			Config: map[string]any{"host": "github.com"},
		})
	}

	// Source control: git-pat-<slug>/ssh-key-<slug> secrets, merged with
	// SiteConfig.ScmHosts by host. Built directly (not through add) because
	// each row's id depends on the derived host; gitHostRows applies the same
	// stored-wins gate per row.
	rows = append(rows, gitHostRows(present, sc.ScmHosts, stored)...)

	// NOT DERIVED (deliberate): artifact_mirror + host_proxy. Both are network
	// TOPOLOGY, not connections — their config already lives, and stays, under
	// Corporate network (SiteConfig.EgressRedirects / UpstreamProxy*), and the
	// read-time fold drops legacy-stored rows of those two categories from this
	// surface too (types.IntegrationList). Deriving them here duplicated a
	// surface that owns them.
	return rows
}

// gitHostCreds accumulates the pat/ssh-key secret names discovered for one
// host while scanning the git-pat-<slug>/ssh-key-<slug> secret-name
// convention (the same convention setup.go's scmProviderCheck documents).
type gitHostCreds struct{ pat, sshKey string }

// secretRows builds the git_host secret rows capabilitiesFor's git_host case
// reads ("pat"/"ssh_key"), or nil when neither lane is configured. No
// delivery: both lanes are the SCM broker's own bespoke transport.
func (c gitHostCreds) secretRows() []types.IntegrationSecret {
	var rows []types.IntegrationSecret
	if c.pat != "" {
		rows = append(rows, types.IntegrationSecret{Role: "pat", SecretName: c.pat})
	}
	if c.sshKey != "" {
		rows = append(rows, types.IntegrationSecret{Role: "ssh_key", SecretName: c.sshKey})
	}
	return rows
}

// slugHost is the canonical secret-name slug for a host: lowercase, collapse
// any run of non [a-z0-9] characters to "-", strip leading/trailing "-".
// Verbatim port of ui/src/app/lib/scm-provider.ts's slugHost (itself ported
// from wardyn-frames.js:617) — prefixed with "git-pat-"/"ssh-key-" this is the
// CONVENTIONAL secret name for a host (setup.go's scmProviderCheck).
func slugHost(host string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(host)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// hostFromSecretSlug reverses the "<host-slug>" naming convention
// (dots -> hyphens, e.g. git-pat-github-com) documented in setup.go's
// scmProviderCheck: strip prefix, turn hyphens back into dots.
//
// ponytail: lossy for a host whose own name contains a hyphen (e.g.
// "ghe-prod.corp.com" slugs to "ghe-prod-corp-com" and would round-trip
// wrong) — the same ambiguity the existing convention already accepts
// (scmProviderCheck treats these as opaque display names, never a validated
// host). gitHostRows below uses this ONLY as the orphan fallback (a secret
// whose slug matches no registered ScmHosts entry); a registered host always
// merges via slugHost's FORWARD match instead. Upgrade path: store the real
// host alongside the secret if an exact reverse ever matters for an orphan.
func hostFromSecretSlug(name, prefix string) string {
	return strings.ReplaceAll(strings.TrimPrefix(name, prefix), "-", ".")
}

// gitHostRows derives one git_host row per host named by a
// SiteConfig.ScmHosts entry OR a git-pat-<slug>/ssh-key-<slug> secret, merged
// by host: an operator's declared ScmHosts host with no credential yet still
// gets a row (egress_host only; clone:pat/clone:ssh read needs_setup), and a
// host that also has a credential carries it.
//
// Host recovery is FORWARD, not reversed (ports ui/src/app/lib/scm-provider.ts's
// deriveProviders — see its doc comment for the full rationale): for each
// ScmHosts entry we compute slugHost(host) ONCE and match secrets against
// THAT, so a hyphenated hostname ("ghe-prod.corp.com") merges onto its OWN
// row instead of ALSO spawning a second, wrongly-named one from
// hostFromSecretSlug's naive hyphens-to-dots reverse. That reverse survives
// only as the fallback for an ORPHAN secret — one whose slug matches no
// registered ScmHosts entry — where a best-guess dotted host beats dropping
// the credential entirely. slugHost is also many-to-one (two registered hosts
// can share a slug), so a matching secret's lane is added to EVERY host that
// slug matches.
func gitHostRows(secretNames map[string]bool, scmHosts []string, stored map[string]bool) []integrationRow {
	byHost := map[string]gitHostCreds{}
	slugToHosts := map[string][]string{}
	for _, h := range scmHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if _, ok := byHost[h]; !ok {
			byHost[h] = gitHostCreds{}
		}
		slug := slugHost(h)
		slugToHosts[slug] = append(slugToHosts[slug], h)
	}
	for n := range secretNames {
		prefix := ""
		switch {
		case strings.HasPrefix(n, "git-pat-"):
			prefix = "git-pat-"
		case strings.HasPrefix(n, "ssh-key-"):
			prefix = "ssh-key-"
		default:
			continue
		}
		hosts, ok := slugToHosts[strings.TrimPrefix(n, prefix)]
		if !ok {
			hosts = []string{hostFromSecretSlug(n, prefix)}
		}
		for _, h := range hosts {
			c := byHost[h]
			if prefix == "git-pat-" {
				c.pat = n
			} else {
				c.sshKey = n
			}
			byHost[h] = c
		}
	}
	var rows []integrationRow
	for host, c := range byHost {
		id := "git_host:" + host
		if stored[id] {
			continue
		}
		rows = append(rows, integrationRow{Integration: types.Integration{
			ID: id, Name: host, Kind: types.IntegrationKindGitHost,
			Secrets: c.secretRows(),
		}, Source: "legacy"})
	}
	return rows
}

// Integration WRITES (validation) + the run resolution ladder built ON TOP of
// effectiveIntegrations/capabilitiesFor above live in integrations_write.go
// (split out once this file crossed the 1000-line gate):
// knownIntegrationTypes, genericIntegrationCategories, validateIntegrationWrite
// (setup_integrations.go's write endpoints),
// resolveIntegrationRef/defaultAgentRunsIntegration (llmcred.go's run-time
// resolution ladder).
