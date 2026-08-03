// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "fmt"

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
	}
	applyDisabled(caps, v)
	return caps
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
	return []Capability{
		{ID: "model_api", State: CapImpossible, Reason: reasonXSubDirect},
		{ID: "tool:claude-code", State: claudeState, Reason: claudeReason, Residency: residency},
		{ID: "tool:codex-cli", State: CapImpossible, Reason: harnessProviderReason("codex-cli", v.Type)},
		{ID: "wardyn_features", State: CapAvailable, Residency: residency},
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

// bedrockCaps builds the bedrock matrix. model_api and tool:claude-code are
// unconditionally available: the claude-code CLI resolves its own AWS
// credentials (bearer/SSO/~/.aws/static keys) the same way an ambient AWS SDK
// would, so Wardyn does not presume to gate the AGENT's own configuration on
// its region/model knobs. wardyn_features is the one cell that DOES need
// Region+model set, because Wardyn's own control-plane code calls Bedrock
// directly with no such fallback.
func bedrockCaps(v integrationView, env capEnv) []Capability {
	lane, _ := v.Config["lane"].(string)
	residency := "resident_env"
	if lane == "bearer" {
		residency = "proxy_injected"
	}
	caps := []Capability{
		{ID: "model_api", State: CapAvailable, Residency: residency},
		{ID: "tool:claude-code", State: CapAvailable, Residency: residency},
		{ID: "tool:codex-cli", State: CapImpossible, Reason: harnessProviderReason("codex-cli", v.Type)},
	}
	if env.BedrockRegionSet && env.BedrockModelSet {
		caps = append(caps, Capability{ID: "wardyn_features", State: CapAvailable, Residency: residency, Reason: reasonBedrockFeatures})
	} else {
		caps = append(caps, Capability{ID: "wardyn_features", State: CapNeedsSetup, Reason: "Region/model unset"})
	}
	return caps
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
