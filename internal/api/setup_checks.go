// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/setup"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// setup_checks.go holds the /setup/status checklist rows extracted from
// handleSetupStatus — one small pure function per row, in the order the wizard
// renders them. "info" is used for permanent / non-fixable or purely-optional
// conditions so the operator is never shown a red they cannot clear. (The
// remaining rows still live beside their state in setup.go.) firstBrokeredRepoFromRuns
// below is the one exception to "pure": it needs the store to confirm a run was
// actually brokered, not just guess from a Repo string.

// SetupCheck is one environment/readiness row. Status is ok|warn|fail|info;
// "info" is a permanent, non-fixable condition (e.g. no /dev/kvm on macOS) that
// must render as informational, not as a clearable warning. Platform lets the UI
// show environment-appropriate copy (linux|darwin|windows|wsl|any).
//
// Blocking decides ONE thing: whether the console must not open on
// this install at all — setupGateActive (the console's setup gate) redirects
// every route into the funnel while any row carries it, until onboarding
// completes. It is set on exactly three arms: runnerCheck's fail (no runner configured, so
// no run can happen; a configured runner whose Capabilities() errored is a
// substrate fault and never blocks), confinementFloorCheck's warn (a floor the runner cannot
// meet refuses every run before it launches), and ssoRBACCheck's warn (OIDC
// with no role mapping makes every signed-in human an admin, and the funnel's
// People step is where that is fixed).
//
// It is NOT "this needs fixing" — nearly every warn/fail row does — and NOT a
// severity ranking. Two families must never carry it. Rows graded through the
// CALLER's own credential rather than the install: llmProviderCheck's per_user
// and provider arms, bedrockProviderCheck's per_user arm, providerAccessCheck and
// awsSSOCredentialRow (the caller's own AWS SSO session). Confiscating the console
// over a fact about one person is exactly what this flag exists to prevent. And advisory
// install rows, whose grade belongs on every surface that renders them but
// whose fix is nobody's emergency: the SCM safest-path ladder, an ephemeral age
// key, TLS cookie posture, an acknowledged egress canary. (harnessCredential-
// Check is install-wide, not per-person — its managed-subscription arm reads an
// unscoped blob — and is advisory for the second reason, not the first.)
type SetupCheck struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Status   string `json:"status"`
	Platform string `json:"platform,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Fix      string `json:"fix,omitempty"`
	Blocking bool   `json:"blocking,omitempty"`
	// Cause narrows a row that can warn for more than one reason, the same
	// shape as SCMAccess.cause on the wire (ui/src/app/lib/types/setup.ts) — a
	// machine key, never prose, so a console reader can pick per-cause copy
	// without string-matching Detail. sso_rbac sets it ("default_role", #491:
	// #484's original no-role-map-and-no-admin-list warn leaves it empty), and so
	// does substrate_health ("runner_unreachable", "runner_auth", "sweep_stale").
	Cause string `json:"cause,omitempty"`
}

// runnerCheck grades the sandbox runner: no runner (or no live class) is the one
// FAIL on the checklist — runs cannot launch at all. CC2+ is ok; a CC1-only host
// is "info", not a warning: runs work, just at the weakest isolation.
func runnerCheck(rnr SetupRunner) SetupCheck {
	// A runner that is configured but could not report its capabilities is a
	// substrate fault, not a missing runner: sending every admin into the setup
	// funnel (Blocking) during an outage would hide the console that shows it.
	if rnr.Driver != "none" && rnr.capsUnreadable {
		return SetupCheck{
			ID: "runner", Label: "Sandbox runner", Status: "fail",
			Detail: "The sandbox runner is configured but did not report its capabilities, so runs cannot launch until it does.",
			Fix:    "The substrate_health row says what is wrong with the runner's substrate.",
		}
	}
	if rnr.Driver == "none" || len(rnr.ConfinementClasses) == 0 {
		return SetupCheck{
			ID: "runner", Label: "Sandbox runner", Status: "fail",
			Detail:   "No sandbox runner is configured, so runs cannot launch.",
			Fix:      "Start wardynd with -runner docker (built with -tags docker) so runs are confined and executed.",
			Blocking: true,
		}
	}
	labeled := make([]string, len(rnr.ConfinementClasses))
	hasCC2Plus := false
	for i, c := range rnr.ConfinementClasses {
		labeled[i] = tierLabel(types.ConfinementClass(c))
		if c != "CC1" {
			hasCC2Plus = true
		}
	}
	if !hasCC2Plus {
		fix := "Unlock the Wall or Vault tier: run `wardyn setup wall` (or `wardyn setup vault`) on the host — it detects your OS/Docker setup and prints the exact steps."
		// `wardyn setup wall`/`vault` probe and configure a
		// DOCKER host — meaningless advice on a k8s runner, where the lever is
		// pinning a cluster-registered RuntimeClass via Helm (README.md's
		// k8s.runtimeClasses.CC2/.CC3), not a command wardynd's own host runs.
		//
		// That Helm command has to be RUNNABLE: `helm upgrade --set …` exits
		// "requires 2 arguments" — no release, no chart — and the shape it
		// teaches (a bare --set, no -f) is the one
		// docs/OPERATIONS.md documents as resetting every OTHER value to chart
		// defaults, dropping exactly the values a Wardyn install cannot run
		// without. Name the release and chart, and re-pass the values file.
		// TestSetupFixHelmCommandsAreRunnable is the guard.
		if rnr.Driver == "k8s" {
			fix = "Unlock the Wall or Vault tier: register a gVisor (or Kata) RuntimeClass in the cluster, then pin it with `helm -n <namespace> upgrade <release> ./deploy/helm/wardyn -f your-values.yaml --set k8s.runtimeClasses.CC2=<name>` (or `.CC3=<name>`). Re-pass your values file — a bare `--set` resets every other value to the chart's defaults."
		}
		return SetupCheck{
			ID: "runner", Label: "Sandbox runner", Status: "info",
			Detail: "Only the Fence tier (weakest — a shared-kernel container) is available on this host; runs work but with the lowest isolation.",
			Fix:    fix,
		}
	}
	return SetupCheck{
		ID: "runner", Label: "Sandbox runner", Status: "ok",
		Detail: "Runner live with the Wall tier or stronger (" + strings.Join(labeled, ", ") + ").",
	}
}

// confinementFloorCheck warns when the operator's configured confinement
// floor (DefaultPolicy.MinConfinementClass) is a class this runner does NOT
// advertise: resolveEnforcedConfinement refuses every run on the default
// policy before it ever launches (invariant 5, fail closed), and without
// this row that shows up only as an opaque 422 on the first attempt, on a
// host that otherwise looks ready. MEMBERSHIP, not rank — the same contract
// resolveEnforcedConfinement itself enforces (M8): a Kata-only host
// advertises [CC1, CC3], no CC2, and still refuses a CC2 floor even though
// CC3 outranks it. No row at all — not even info — when there is nothing to
// warn about: no floor configured (policy runs unconfined-by-default, i.e.
// CC1), no advertised classes to compare against (runnerCheck already owns
// that failure), or the floor IS advertised.
func confinementFloorCheck(rnr SetupRunner, floor types.ConfinementClass) (SetupCheck, bool) {
	if floor == "" || len(rnr.ConfinementClasses) == 0 {
		return SetupCheck{}, false
	}
	if slices.Contains(rnr.ConfinementClasses, string(floor)) {
		return SetupCheck{}, false
	}
	advertised := strings.Join(rnr.ConfinementClasses, ", ")
	fix := fmt.Sprintf(
		"Lower the floor to a class this runner advertises (%s) — set WARDYN_DEFAULT_POLICY (or the Helm chart's defaultPolicy) to a policy JSON with that min_confinement_class; examples/policies/demo.json is a CC1 reference.",
		advertised)
	// Same shape as runnerCheck's: release, chart, and the
	// values file re-passed, or the pin lands as the only value the release has.
	if rnr.Driver == "k8s" {
		fix += fmt.Sprintf(" Or register the floor's RuntimeClass in the cluster and pin it: helm -n <namespace> upgrade <release> ./deploy/helm/wardyn -f your-values.yaml --set k8s.runtimeClasses.%s=<name> (pass your values file — a bare --set resets everything else to chart defaults).", floor)
	}
	return SetupCheck{
		ID: "confinement_floor", Label: "Confinement floor", Status: "warn",
		Detail: fmt.Sprintf("The configured confinement floor is %s, but this runner only advertises %s — every run on the default policy is refused before it launches.",
			floor, advertised),
		Fix:      fix,
		Blocking: true,
	}, true
}

// envBuilderCheck reports whether the per-run sandbox image builder is wired — the
// path a devcontainer build or a --image (bring-your-own-image) run needs. It is
// OFF by default on the bare binary (WARDYN_ENVBUILD unset, or wardynd not built
// with -tags docker) and ON in the compose stack, with no other setup signal, so a
// devcontainer/--image run that silently no-ops otherwise reads as a random failure.
// INFO (never a warning) when off: it is optional — only devcontainer/BYOI runs need
// it; a convention-image run does not.
func envBuilderCheck(wired bool) SetupCheck {
	if wired {
		return SetupCheck{
			ID: "env_builder", Label: "Sandbox image builder", Status: "ok",
			Detail: "The per-run image builder is wired: devcontainer builds and --image (bring-your-own-image) wraps will fire.",
		}
	}
	return SetupCheck{
		ID: "env_builder", Label: "Sandbox image builder", Status: "info",
		Detail: "The per-run image builder is off (the default on the bare binary AND every Helm/k8s install unless set): a --image " +
			"(bring-your-own-image) run is rejected, a devcontainer_repo run silently falls back to the convention image instead of building, " +
			"and a workspace with a registry/byo/custom base image chosen in the catalog REFUSES every run that uses it (the image can't be " +
			"wrapped with the agent runtime).",
		Fix: "Set WARDYN_ENVBUILD=true on a wardynd built with -tags docker, or use the compose stack (it enables the builder).",
	}
}

// k8sEgressContainmentCheck grades the k8s substrate's boot-time NetworkPolicy
// canary verdict (netpolProven, computed in setupRunnerInfo from
// ClassSupport.NetworkPolicy — a local value, not a wire field: nothing else
// reads it). Absent entirely on a non-k8s driver: docker proves L0
// (structural — no default route) and has no analogous row; only a
// Kubernetes deployment's egress claim rests on a packet filter that needs
// live proof.
//
// Graded FAIL, not warn, when unproven: this is the harder security claim
// (substrate.go's package doc: "never render a green containment row the
// canary didn't prove"), so an un-proven k8s deployment fails the setup
// checklist's "ready to launch" verdict even though CC1 sandboxes still
// create fine (runnerCheck's own separate, milder grade covers that).
//
// netpolProven is "enforced" | "unenforced" | "acknowledged" | "". A LIVE
// wardynd can only ever report one of the first three — a genuinely
// indeterminate canary (no override, no ack) refuses to boot entirely
// (internal/runner/k8s's newWithClient). "" covers three real cases, not
// two: a non-k8s driver; a k8s daemon build that predates this computation;
// AND a genuine k8s driver whose Capabilities() call itself just errored
// (setupRunnerInfo's own early-return path, which already leaves Driver
// "k8s" with nothing else resolved). All three grade identically here: an
// honest "can't confirm", never a silent Enforcing.
func k8sEgressContainmentCheck(driver, netpolProven string) (SetupCheck, bool) {
	if driver != "k8s" {
		return SetupCheck{}, false
	}
	const id, label = "k8s_egress_containment", "Egress containment"
	switch netpolProven {
	case "enforced":
		return SetupCheck{
			ID: id, Label: label, Status: "ok",
			Detail: "Enforcing · NetworkPolicy (the boot-time canary proved a deny-all policy actually blocks egress).",
		}, true
	case "acknowledged":
		// Never "ok" — an acknowledgment is not the canary proving
		// anything (phase B never even ran, see runEgressCanary's comment on
		// why running it would be theater behind an existing ambient deny).
		return SetupCheck{
			ID: id, Label: label, Status: "warn",
			Detail: "Acknowledged, not proven — the operator confirmed this cluster's ambient default-deny NetworkPolicy (applied " +
				"by the platform, not Wardyn) via WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY=1 (helm: env.WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY). " +
				"The boot-time canary could not independently confirm Wardyn's own deny-all policy takes effect: behind an existing " +
				"default-deny, that test can only ever also refuse, proving nothing either way, so it was skipped rather than run for show.",
			Fix: "Prove it instead of acknowledging it: exempt Wardyn's own pods from the platform's ambient policy (a " +
				"matchExpressions entry with key wardyn.managed, operator NotIn, values [\"true\"]) so the canary can run its real " +
				"deny-all test, then unset WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY (helm: env.WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY).",
		}, true
	case "unenforced":
		return SetupCheck{
			ID: id, Label: label, Status: "fail",
			Detail: "Not enforcing — the boot-time canary proved this cluster's CNI does not enforce NetworkPolicy. The " +
				"operator accepted that risk via WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1 (helm: env.WARDYN_K8S_ALLOW_UNENFORCED_NETPOL) " +
				"— every sandbox this substrate creates has UNCONFINED egress.",
			Fix: "Unset WARDYN_K8S_ALLOW_UNENFORCED_NETPOL (helm: env.WARDYN_K8S_ALLOW_UNENFORCED_NETPOL) and fix the cluster's " +
				"CNI/NetworkPolicy support to restore real confinement.",
		}, true
	default:
		return SetupCheck{
			ID: id, Label: label, Status: "fail",
			Detail: "Indeterminate — this control plane reports a Kubernetes runner but not its NetworkPolicy canary verdict, " +
				"so egress containment cannot be confirmed.",
			Fix: "Upgrade wardynd to a build that reports the canary verdict, and check its boot logs for the egress-canary result.",
		}, true
	}
}

// DRAFT (M2 canon pending) — the fix a mechanism principal's provider row
// carries.
const (
	// DRAFT (M2 canon pending)
	bedrockMechanismFix = "Sign in to the console (or use your own wdn_ API token) to see your own model access."
	// internalHostsCheckLabel names internalHostsCheck's dedicated row.
	// Detail is internalHostsDeclaredSentence
	// (site_config.go), reused verbatim from the write-time log, so it is
	// not repeated here.
	//
	// DRAFT (M2 canon pending)
	internalHostsCheckLabel = "Internal hosts (SSRF guard override)"
)

// llmProviderCheck is the "LLM access" row: the caller's own model-provider
// access (providerAccessLLMCheck) when a provider is granted, else INFO — a
// model provider is OPTIONAL, needed only for agent-harness runs, so "no model"
// is a deliberate non-blocking state, never a gap the operator must clear.
func llmProviderCheck(access []SetupProviderAccess) SetupCheck {
	if chk, ok := providerAccessLLMCheck(access); ok {
		return chk
	}
	return SetupCheck{
		ID: "llm_provider", Label: "LLM access", Status: "info",
		Detail: "No model provider serves you (optional): needed only for agent-harness runs. Bring-your-own-container and interactive runs work without one.",
		Fix:    "Optional — an admin adds a model provider under Settings → Model providers, then you connect your own credential for it.",
	}
}

// ageKeyCheck warns when the secret store's age key is EPHEMERAL: a fresh
// identity is minted at every boot with none configured, so what is stored now
// is lost at the next restart, and that boot refuses to start over the rows it
// cannot decrypt (convertSecretStore). No earlier ephemeral key's rows can be
// present while this row shows, since that same refusal kept them from booting;
// the refusal is where the operator learns those are unrecoverable (#755).
//
// The Fix must never offer `helm: env.WARDYN_AGE_KEY` as the cluster
// answer: it renders the secret store's MASTER key as a plaintext literal in
// the Deployment object — readable by anything with `get deploy`, and captured
// in every `helm get manifest`. The chart has two Secret-backed doors
// (secrets.ageKeyFromSecret over the postgres.dsn.secretRef Secret's `age-key`
// entry, or secrets.ageKeySecretRef.name for a separate one) and refuses a
// render naming more than one, so the advice names them as an either/or in the
// chart's own order. The host answer (-age-key, or the env var on a bare binary
// with no Deployment to leak into) stays: it is what compose and install.sh
// already write.
func ageKeyCheck(durable bool) SetupCheck {
	if durable {
		return SetupCheck{
			ID: "age_key", Label: "Secret store durability", Status: "ok",
			Detail: "The secret store age key is durable; stored secrets survive a restart.",
		}
	}
	return SetupCheck{
		ID: "age_key", Label: "Secret store durability", Status: "warn",
		Detail: "The secret store uses an EPHEMERAL age key generated at boot: everything stored under it (API keys, GitHub App credentials) is lost at the next restart — no key set afterward can decrypt it — and the next boot refuses to start until those rows are deleted.",
		Fix: "Generate a durable key with `wardynd -gen-age-key`, then wire it as WARDYN_AGE_KEY: " +
			"on a host, -age-key or the env var; " +
			"on Helm, keep it in a Secret — secrets.ageKeyFromSecret=true (an `age-key` entry in the Secret postgres.dsn.secretRef names) or secrets.ageKeySecretRef.name for a separate one. " +
			"Not env.WARDYN_AGE_KEY — that renders the master key as a plaintext literal in the Deployment.",
	}
}

// secretStoreChecks are the credential-storage rows (design §3, canon SETUP_CHECK.*): store_external in store
// mode; kek_service (KEK_SERVICE; keyService is "Vault Transit at {host}" or "Key Vault {vault}") when a key
// service wraps every data key; else the age-key row, kek_local on a multi-user install (whoever holds the database and the local key
// reads every credential), and platform_shared while no WARDYN_PLATFORM_KEY_FILE is set (§2.13 c: one leak of
// the age key then also forges run identities and sessions).
func secretStoreChecks(external, keyService string, durable, multiUser, platformSeparate, kekRequired bool) []SetupCheck {
	if external != "" {
		return []SetupCheck{{ID: "store_external", Label: "Credential storage", Status: "ok",
			Detail: "Credentials are stored in " + external + ". Wardyn holds no key; every use is logged there."}}
	}
	if keyService != "" {
		unlock := "a Transit decrypt in Vault's audit log"
		if strings.HasPrefix(keyService, "Key Vault") {
			unlock = "an unwrap in Key Vault's logs"
		}
		return []SetupCheck{{ID: "kek_service", Label: "Credential storage", Status: "ok",
			Detail: "Credentials stay sealed in Wardyn's database; the key that unlocks them is held in " + keyService +
				" and never leaves it. Wardyn holds no copy; each unlock is " + unlock + "."}}
	}
	checks := []SetupCheck{ageKeyCheck(durable)}
	if kekRequired {
		checks = append(checks, SetupCheck{
			ID: "kek_required_unmet", Label: "Credential key custody", Status: "fail",
			Detail: "This deployment requires a key service, but credentials are wrapped by the local key. This reports the posture; wardynd refuses to start in it.",
			Fix:    "Set WARDYN_KEK=transit or azurekv, then run `wardynd -rewrap`.",
		})
	}
	if durable && multiUser {
		checks = append(checks, SetupCheck{
			ID: "kek_local", Label: "Credential key", Status: "warn",
			Detail: "Credentials are encrypted with a key this deployment holds. Anyone with both the database and that key can read them. Connect a key service to keep the two apart.",
			Fix:    "Set WARDYN_KEK=transit with a Vault Transit key (docs/operations/secrets-and-keys.md), then run `wardynd -rewrap`.",
		})
	}
	if !platformSeparate {
		checks = append(checks, SetupCheck{
			ID: "platform_shared", Label: "Platform key separation", Status: "warn",
			Detail: "Wardyn's own signing and session keys are protected by the same key as people's credentials.",
			Fix:    "Mint a second key with `wardynd -gen-age-key`, point WARDYN_PLATFORM_KEY_FILE at it, run `wardynd -rewrap -rewrap-adopt-boot-keys` once (it says you have never moved the boot keys before), then restart wardynd with it set.",
		})
	}
	return checks
}

// credentialStorageMode names the kind of store this deployment keeps
// people's credentials in, for /setup/status's credential_storage field
// (design F-3, packet F): "local" | "key_service" | "key_service_key_vault" |
// "vault" | "key_vault" — never a host, path or vault name. external and keyService are the same two
// Server-config strings secretStoreChecks (above) grades; the store kind rides
// on which of the store's two Describe() spellings external carries ("Vault
// at …" for vaultkv, "Key Vault …" for azurekv, secretstore/vaultkv and
// /azurekv), so telling them apart needs no third Server field.
func credentialStorageMode(external, keyService string) string {
	switch {
	case strings.HasPrefix(external, "Key Vault"):
		return "key_vault"
	case external != "":
		return "vault"
	case strings.HasPrefix(keyService, "Key Vault"):
		return "key_service_key_vault"
	case keyService != "":
		return "key_service"
	default:
		return "local"
	}
}

// siteConfigCheck reports whether an operator-wide corporate baseline (upstream
// proxy, egress redirects, default SCM hosts) has been authored yet. "info" for
// the unconfigured/fully-configured cases — it is optional and skippable, never
// a blocking gate. "warn" when it IS configured but names a secret present does
// not currently hold (danglingSiteConfigSecretRefs) — e.g. after a
// `wardyn site-config set corp-baseline.json` recovery whose secrets were
// never restored: the document round-trips fine and reads as fully configured,
// but every credentialed path through it (the upstream proxy, a redirect's
// token) is dead until the named secret is set.
//
// Deliberately does not check the deprecated ArtifactOverrides: sc always
// comes from Store.GetSiteConfig, and every persisted document has already
// gone through either the PUT-time fold (foldLegacyArtifactOverrides,
// site_config.go) or the one-time 0030 migration, so that field is provably
// always empty by the time it is read back here.
func siteConfigCheck(sc types.SiteConfig, present map[string]bool) SetupCheck {
	// effectiveScmHosts, not ScmHosts: "is anything configured at all" has to
	// answer yes for an install whose git hosts come from provider rows, and no
	// for one whose only legacy entry is a host a disabled row has claimed
	// (workspace_providers.go).
	//
	// InternalHosts and UpstreamProxyNoProxy must be checked here too: a
	// document declaring ONLY InternalHosts (the one override that LIFTS the
	// proxy's private/reserved-IP SSRF guard, loud-warned at write time by
	// logWarnInternalHostsDeclared) would otherwise read as "No operator-wide
	// site config yet".
	// WorkspaceProviders is included too: an operator who explicitly configured
	// (even all-disabled) git provider rows has touched this surface, whatever
	// effectiveScmHosts currently derives from that state.
	if sc.UpstreamProxySecretRef == "" && sc.UpstreamProxyURL == "" && len(sc.EgressRedirects) == 0 &&
		len(effectiveScmHosts(sc)) == 0 && len(sc.InternalHosts) == 0 && len(sc.UpstreamProxyNoProxy) == 0 &&
		sc.WorkspaceProviders == nil {
		return SetupCheck{
			ID: "site_config", Label: "Site config (corporate baseline)", Status: "info",
			Detail: "No operator-wide site config yet (optional): a corporate upstream proxy, artifact-registry redirects, and default SCM hosts that every run would inherit.",
			Fix:    "Set one via PUT /api/v1/site-config (or the Network step — the Host proxy / Artifact redirect tabs).",
		}
	}
	if dangling := danglingSiteConfigSecretRefs(sc, present); len(dangling) > 0 {
		word := "secret"
		if len(dangling) > 1 {
			word = "secrets"
		}
		return SetupCheck{
			ID: "site_config", Label: "Site config (corporate baseline)", Status: "warn",
			Detail: fmt.Sprintf("An operator-wide site config is set, but %s %s is not set — every credentialed path through it (the upstream proxy / a redirect's token) is dead until it's restored.", word, strings.Join(dangling, ", ")),
			Fix:    "Restore the missing secret(s) with `wardyn secret set <name>`, or point the ref at a secret that exists.",
		}
	}
	return SetupCheck{
		ID: "site_config", Label: "Site config (corporate baseline)", Status: "info",
		Detail: "An operator-wide site config is set (upstream proxy / egress redirects / SCM hosts); every run inherits it.",
	}
}

// internalHostsCheck is the dedicated row for an InternalHosts declaration —
// the ONE operator override that LIFTS the proxy's private/
// reserved-IP SSRF guard, and until now visible only as a boot-time log line
// (logWarnInternalHostsDeclared, site_config.go) plus a bare count folded
// into siteConfigCheck's generic sentence. Reuses that same sentence
// (internalHostsDeclaredSentence) so an operator reads the identical claim on
// the console that they'd read in the deployment log.
//
// Always "info", never "warn"/"fail": this states a fact about the
// declaration an operator wrote down on purpose (Liftable-validated at
// write time), not a problem — and setupGateActive only ever fires on
// warn/fail, so an install with nothing else configured stays gate-inactive
// with only this row present. (ok, bool) mirrors ssoRBACCheck/
// tlsCookiePostureCheck's own "absent when not applicable" shape.
func internalHostsCheck(sc types.SiteConfig) (SetupCheck, bool) {
	if len(sc.InternalHosts) == 0 {
		return SetupCheck{}, false
	}
	return SetupCheck{
		ID: "internal_hosts", Label: internalHostsCheckLabel, Status: "info",
		Detail: internalHostsDeclaredSentence(sc.InternalHosts),
	}, true
}

// platformChecks are the platform rows — permanent and non-fixable, so always
// "info" (and absent on a platform they do not apply to).
func platformChecks(plat setup.Platform) []SetupCheck {
	var out []SetupCheck
	if plat.WSL {
		out = append(out, SetupCheck{
			ID: "platform_wsl", Label: "WSL networking", Status: "info", Platform: "wsl",
			Detail: "Running under WSL2: host<->sandbox networking is split. Reach the UI from Windows via localhost port-forwarding, and bind wardynd to a WSL-reachable address. With Docker Desktop's default NAT networking, sandbox->wardynd callbacks don't route in host mode — workspace Verify results never report and Record captures land empty.",
			Fix:    "Enable WSL2 mirrored networking ([wsl2] networkingMode=mirrored in %UserProfile%\\.wslconfig, then `wsl --shutdown`), or run the containerized stack (`make compose-up`) where callbacks route in-network.",
		})
	}
	if plat.OS == "darwin" {
		out = append(out, SetupCheck{
			ID: "platform_macos", Label: "macOS virtualization", Status: "info", Platform: "darwin",
			Detail: "macOS has no /dev/kvm; the Vault tier (CC3, hardware-virtualized) is unavailable — runs use container isolation.",
		})
	}
	return out
}

// refRulesetCheck grades one GitHub ref-confinement probe. Pure: the outbound
// call and its cache live in Server.githubRefRulesetCheck.
//
// NEVER "fail" — the row is advisory, and grading a security regression from a
// network error is exactly the failure mode this check has to avoid. err (any
// error: timeout, rate limit, a 403 on the read permission, a repo the
// installation cannot see, a ruleset whose bypass mode could not be read) is
// "info"/unknown; an unconfined repo is "warn".
func refRulesetCheck(repo string, confined bool, detail string, err error) SetupCheck {
	const (
		id    = "github_ref_ruleset"
		label = "GitHub ref confinement (token-side)"
		fix   = "Create the ruleset on the repo — the runnable `gh api` invocation is in docs/POLICIES.md under \"Bound the token itself: a GitHub ruleset\". It needs admin on the repo, which Wardyn does not have."
	)
	switch {
	case err != nil:
		return SetupCheck{
			ID: id, Label: label, Status: "info",
			Detail: "Could not read GitHub's rules for " + repo + ", so ref confinement is UNKNOWN — not known-bad: " + err.Error() +
				" Both reads it makes — the branch rules and the ruleset's bypass mode — are documented to need only Metadata: read, which every GitHub App holds; Wardyn has not confirmed that against a live installation." +
				" GHES is not supported here — the broker always talks to api.github.com.",
			Fix: fix,
		}
	case !confined:
		return SetupCheck{
			ID: id, Label: label, Status: "warn",
			Detail: detail +
				" Wardyn's own branch-namespace enforcement still holds on the brokered route by default (the receive-pack parser refuses any ref outside refs/heads/wardyn/<run-id>/, unless WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS=false); a ruleset is what would also hold for a token that escaped the proxy." +
				" This reads RULESETS only — classic branch protection also bounds a token but does not appear in that endpoint, so a repo protected that way is graded here as unconfined.",
			Fix: fix,
		}
	default:
		return SetupCheck{
			ID: id, Label: label, Status: "ok",
			Detail: detail +
				" Graded for " + repo + " only — the checklist probes one repo, while the WARDYN_GITHUB_REQUIRE_REF_RULESET gate grades every repo in the grant." +
				" The bypass part of that verdict is read from current_user_can_bypass, which GitHub returns to the caller making the request; whether it is computed meaningfully for a GitHub App installation token is not something Wardyn has confirmed.",
		}
	}
}

// firstBrokeredRepoRunScan bounds firstBrokeredRepoFromRuns to the N most
// recent runs — a fixed window, not a COUNT/EXISTS query (ponytail: widen it
// if a real install's brokered runs turn out to sit consistently deeper).
const firstBrokeredRepoRunScan = 20

// firstBrokeredRepoFromRuns is firstBrokeredRepo's fallback for the common
// case its own doc describes: shipped policies template scope.repos: [], so
// the repo actually brokered for a run usually comes from the run's declared
// --repo / workspace repos at CREATE time instead (augmentGitBrokerGrants,
// runs_create.go), unioned into the git-broker map only when the run also
// held a github_token grant — never recorded back onto the policy.
//
// This mirrors that exact pair of facts on the most recent runs: a declared
// repo Wardyn resolves as a github.com clone (gitBrokerKeyFromSlug), AND an
// actual github_token grant on that run (its persisted CredentialGrant rows,
// not guessed from Repo being non-empty). So it can't over-claim a repo that
// was only ever cloned read-only with no credential at all — a run with no
// repo, or a repo but no github grant, contributes nothing to the broker map
// either, and is skipped here the same way.
//
// Only run.Repo (the legacy single-repo field) is scanned; a run brokered
// purely through workspace_repos, with no legacy --repo, falls through to "".
// Bounded to firstBrokeredRepoRunScan runs — at the SQL level when the store
// is a store.Pager (indexed, newest-first), else in Go over the unbounded
// (but still newest-first) List — so an install with a GitHub App configured
// and a long run history that never touches GitHub can't turn this
// (5-minute-cached) check into a store round trip per run.
func (s *Server) firstBrokeredRepoFromRuns(ctx context.Context) string {
	if s.cfg.Store == nil {
		return ""
	}
	var runs []types.AgentRun
	var err error
	if pg, ok := s.cfg.Store.(store.Pager); ok {
		runs, err = pg.ListRunsPage(ctx, store.Page{Limit: firstBrokeredRepoRunScan})
	} else {
		runs, err = s.cfg.Store.ListRuns(ctx) // newest-first; only test doubles lack Pager
	}
	if err != nil {
		return ""
	}
	for i, run := range runs {
		if i >= firstBrokeredRepoRunScan {
			break
		}
		key := gitBrokerKeyFromSlug(run.Repo)
		if key == "" {
			continue // no repo, or not a github.com slug/URL
		}
		if grants, gerr := s.cfg.Store.ListGrantsByRun(ctx, run.ID); gerr == nil &&
			slices.ContainsFunc(grants, func(g types.CredentialGrant) bool { return g.Spec.Kind == types.GrantGitHubToken }) {
			return key
		}
	}
	return ""
}

// scmProviderCheck grades the SCM credential posture against the safest-path
// ladder (GitHub App > fine-grained PAT > deploy key > classic PAT/gh token >
// personal SSH key). NEVER a gate: warn means "a safer option exists", not
// "broken" — the configured lane still clones fine, and public repos need no
// SCM credential at all. Grading:
//   - GitHub App configured        -> ok   (brokered ≤1h scoped tokens; the only
//     rung Wardyn itself can expire — nothing safer to recommend)
//   - any ssh-key-<host> secret    -> warn (a STANDING resident-lane key Wardyn
//     can neither scope nor expire; auto-used by every future SSH clone)
//   - git-pat-<host> only          -> info (could already be a fine-grained
//     rung-2 token — server-side we cannot tell it from a classic one, so no
//     warn; the detail carries the upgrade hint instead)
//   - nothing configured           -> info (+ posture-aware Fix when the host
//     shows loose habits: gh CLI login, credential.helper store/cache,
//     ~/.git-credentials, ~/.netrc)
//
// a secret-NAME prefix scan, not a grant-usage check (grants are
// per-run, not standing config) — the <host-slug> convention (dots→hyphens,
// e.g. git-pat-github-com) is the contract the ScmProviderStep UI follows.
func scmProviderCheck(githubApp bool, secretNames []string, posture setup.SCMPosture) SetupCheck {
	var pats, sshKeys []string
	for _, n := range secretNames {
		switch {
		case strings.HasPrefix(n, "git-pat-"):
			pats = append(pats, n)
		case strings.HasPrefix(n, "ssh-key-"):
			sshKeys = append(sshKeys, n)
		}
	}
	loosePosture := posture.GhCLI || posture.GitCredentialsFile || posture.Netrc ||
		strings.HasPrefix(posture.CredentialHelper, "store") || strings.HasPrefix(posture.CredentialHelper, "cache")
	switch {
	case githubApp:
		via := append([]string{"GitHub App"}, pats...)
		detail := "Safest lane configured: the GitHub App mints a brokered, ≤1h, contents-scoped token per run — the only SCM credential Wardyn itself can expire. (" + strings.Join(via, ", ") + ")"
		if len(sshKeys) > 0 {
			// Honesty: the App does NOT retire a standing ssh-key-* secret —
			// SSH-protocol clones still auto-use it, resident, with no prompt.
			detail += " Note: standing SSH key secret(s) also present (" + strings.Join(sshKeys, ", ") + ") — the App doesn't retire them; delete if unused."
		}
		return SetupCheck{
			ID: "scm_provider", Label: "SCM provider credentials", Status: "ok",
			Detail: detail,
		}
	case len(sshKeys) > 0:
		detail := "SSH key secret(s) configured (" + strings.Join(sshKeys, ", ") + "): a STANDING credential, resident in the sandbox for each clone, that Wardyn can neither scope nor expire. It works — a safer rung exists."
		if len(pats) > 0 {
			detail += " PAT secret(s) also present (" + strings.Join(pats, ", ") + "): those are brokered per-clone and never resident."
		}
		return SetupCheck{
			ID: "scm_provider", Label: "SCM provider credentials", Status: "warn",
			Detail: detail,
			Fix:    "Prefer a GitHub App (brokered, expirable) or a fine-grained repo-scoped PAT (github.com/settings/personal-access-tokens/new → Contents: Read-only). If SSH, make it a single-repo read-only deploy key, not a personal identity.",
		}
	case len(pats) > 0:
		return SetupCheck{
			ID: "scm_provider", Label: "SCM provider credentials", Status: "info",
			Detail: "PAT secret(s) configured (" + strings.Join(pats, ", ") + "), brokered per-clone and never resident. If it is a classic whole-account PAT, re-issue it fine-grained + repo-scoped + short-expiry; a GitHub App is safer still.",
		}
	case loosePosture:
		return SetupCheck{
			ID: "scm_provider", Label: "SCM provider credentials", Status: "info",
			Detail: "No SCM credential configured yet (optional). Host posture note: this machine keeps broad or plaintext git credentials (gh CLI session, credential.helper store/cache, ~/.git-credentials or ~/.netrc) — Wardyn never reads them.",
			Fix:    "For private repos, prefer a GitHub App or a fine-grained repo-scoped PAT stored as git-pat-<host-slug> (e.g. git-pat-github-com) — or generate a read-only deploy key (make setup offers this).",
		}
	default:
		return SetupCheck{
			ID: "scm_provider", Label: "SCM provider credentials", Status: "info",
			Detail: "No SCM credential configured yet (optional): cloning a private GitHub/Azure DevOps repo needs a GitHub App, a git-pat-<host-slug> secret (HTTPS/PAT), or an ssh-key-<host-slug> secret (SSH) referenced from a matching grant.",
			Fix:    "Add a secret named git-pat-github-com / git-pat-dev-azure-com (or your GHES/ADO-Server host's slug) under Secrets and reference it from a git_pat grant — or configure a GitHub App.",
		}
	}
}

// hostProxyCheck summarizes host-proxy detection as a single non-blocking
// "info" row — the HostProxy field itself carries the full per-source detail
// the Host Proxy step renders. Always "info": detection never blocks setup,
// it only surfaces what's already configured on the host so the step can
// suggest matching settings.
//
// blind is true when this wardynd is containerized AND no host-side detection
// was seeded in: every tier (shell profiles, git, tool configs, OS/PAC) is then
// structurally unreachable, so an empty result must say "couldn't look there"
// rather than assert "nothing is there". Same honesty rule as vaultKVMDetail.
func hostProxyCheck(d setup.HostProxyDetection, blind bool) SetupCheck {
	var found []string
	if d.HTTPProxy != nil || d.HTTPSProxy != nil || d.AllProxy != nil {
		found = append(found, "an env/shell/OS proxy setting")
	}
	if d.GitProxy != nil {
		found = append(found, "a git config proxy")
	}
	if len(d.ToolConfigs) > 0 {
		names := make([]string, len(d.ToolConfigs))
		for i, tc := range d.ToolConfigs {
			names[i] = tc.Tool
		}
		found = append(found, "tool configs ("+strings.Join(names, ", ")+")")
	}
	if d.PAC != nil {
		found = append(found, "a PAC/WPAD auto-config URL (cannot be resolved automatically)")
	}
	if len(found) == 0 {
		if blind {
			return SetupCheck{
				ID: "host_proxy", Label: "Host proxy", Status: "info",
				Detail: "Detection ran inside the wardynd container, so it only sees this container's environment — not your host's shell profiles, git config, per-tool configs, or OS/PAC proxy settings. That is \"couldn't look there\", not \"nothing is there\".",
				Fix:    "If your host uses a corporate proxy, store its URL as a secret and reference it below — or re-run `make setup` (it detects on the host and seeds the result in).",
			}
		}
		return SetupCheck{
			ID: "host_proxy", Label: "Host proxy", Status: "info",
			Detail: "No host-side proxy configuration detected (env vars, shell profiles, git config, tool configs, or OS proxy settings).",
		}
	}
	detail := "Detected " + strings.Join(found, "; ") + "."
	if d.HasCredentials {
		detail += " A detected proxy carries an embedded credential — store it as a secret rather than a plain URL."
	}
	// A loopback-bound proxy is reachable from host processes but from nothing
	// else: a sandbox's 127.0.0.1 is its own, and on a VM-backed Docker host the
	// runtime VM cannot reach the host's loopback either. Chaining sandbox egress
	// through it therefore cannot work, and the failure lands late (at the first
	// approved request) rather than at setup — so say it here, where the operator
	// is still configuring. Warn, not fail: detection is never a gate, and a
	// host-mode wardynd on the same machine CAN use it.
	if a := d.LoopbackBound(); len(a) > 0 {
		return SetupCheck{
			ID: "host_proxy", Label: "Host proxy", Status: "warn",
			Detail: detail + " " + strings.Join(a, ", ") + " is bound to loopback, which a sandbox cannot reach (its 127.0.0.1 is its own; a VM-backed Docker host can't reach the host's loopback either). Chaining sandbox egress through it will fail to connect.",
			Fix:    "Point the upstream proxy at an address the sandbox can reach: run `wardyn setup proxy-relay <listen-port> <proxy-port>` on the host and store http://<host-gateway>:<listen-port> as the upstream-proxy secret — see docs/adoption/loopback-only-forward-proxy.md.",
		}
	}
	return SetupCheck{ID: "host_proxy", Label: "Host proxy", Status: "info", Detail: detail}
}

// artifactRepoCheck reports whether the operator has configured egress
// redirects (package-registry mirrors, or any other outbound redirect).
// Always "info": optional and non-blocking.
func artifactRepoCheck(sc types.SiteConfig) SetupCheck {
	if len(sc.EgressRedirects) == 0 {
		return SetupCheck{
			ID: "artifact_repo", Label: "Egress redirection", Status: "info",
			Detail: "No egress redirects configured (optional): point npm/pip/cargo/maven/go/nuget (or any other outbound host) at a corporate mirror/relay so runs never reach the public destination directly.",
			Fix:    "Set egress_redirects via PUT /api/v1/site-config (or the Corporate Network setup step).",
		}
	}
	ecos, network, tokened := map[string]bool{}, 0, 0
	for _, r := range sc.EgressRedirects {
		if r.Ecosystem == "" {
			network++
		} else {
			ecos[r.Ecosystem] = true
		}
		// Either token source counts — a redirect taking its token from an
		// integration injects one exactly like a bare-secret row does, so
		// counting only the latter would under-report what is wired.
		if r.TokenSecretRef != "" || r.TokenIntegrationRef != "" {
			tokened++
		}
	}
	// The "ecosystems: " clause is OMITTED, not rendered empty, when
	// every redirect is network-only — a bare "(ecosystems: ; 2 network-only)"
	// read as a truncated/broken row rather than "there are no ecosystem rows".
	parts := make([]string, 0, 2)
	if len(ecos) > 0 {
		parts = append(parts, "ecosystems: "+strings.Join(slices.Sorted(maps.Keys(ecos)), ", "))
	}
	parts = append(parts, fmt.Sprintf("%d network-only", network))
	detail := fmt.Sprintf("%d redirect(s) configured (%s); egress substitutes the corp destination in.",
		len(sc.EgressRedirects), strings.Join(parts, "; "))
	if tokened > 0 {
		detail += fmt.Sprintf(" %d with a token injected proxy-side.", tokened)
	}
	return SetupCheck{ID: "artifact_repo", Label: "Egress redirection", Status: "info", Detail: detail}
}

// permissionsPostureCheck grades the four capability-enforcement
// switches (capabilityKinds — egress_host, secret, workspace, image;
// capabilities.go) that gate member-narrowing/widening grants. An absent
// switch is capBatch.enforced's own documented default: FAIL-OPEN, i.e. that kind
// behaves exactly as an un-gated pre-0.6 deployment (capAllowed's doc
// comment). That is a deliberate, upgrade-safe DEFAULT, not a
// misconfiguration — a single-operator deployment may legitimately never
// enable any of them — so this row is always "info" and never escalates to
// warn/fail, and (like ready's own computation) never gates SetupStatus.Ready.
// It exists purely so an operator can see the posture without opening the
// Permissions screen.
func permissionsPostureCheck(enforcement map[string]bool) SetupCheck {
	var on, off []string
	for _, k := range capabilityKinds {
		if enforcement[k] {
			on = append(on, k)
		} else {
			off = append(off, k)
		}
	}
	joined := func(xs []string) string {
		if len(xs) == 0 {
			return "none"
		}
		return strings.Join(xs, ", ")
	}
	return SetupCheck{
		ID: "permissions_posture", Label: "Permission enforcement posture", Status: "info",
		Detail: fmt.Sprintf(
			"%d of %d permission kinds enforced. Enforced: %s. Fail-open (default allow, matching pre-0.6 behavior): %s.",
			len(on), len(capabilityKinds), joined(on), joined(off)),
	}
}
