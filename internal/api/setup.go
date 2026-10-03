// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/setup"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GitHub App credential secret names (mirrors cmd/wardynd's secretGitHubAppID /
// secretGitHubAppKey). Presence of BOTH sets Secrets.GitHubApp.
const (
	secretGitHubAppID  = "github-app-id"
	secretGitHubAppKey = "github-app-key"
)

// setupRecheckParam is the console's Re-check, spelled on the wire: any non-empty
// value on GET /setup/status drops the host-proxy memo first, so the answer comes
// from the host rather than from up to 30s ago. Additive and ignorable — an older
// console omits it and gets exactly today's behaviour.
const setupRecheckParam = "recheck"

// First-run setup readiness surface.
//
// GET /api/v1/setup/status returns the aggregate a first-run "Getting started"
// wizard needs to detect the environment, providers, credentials, and runner
// capability of THIS control plane. The struct types below are the single
// frozen contract shared with the UI (ui/src/app/lib/types.ts SetupStatus) —
// keep the two in exact sync (snake_case wire fields).

// SetupStatus is the aggregate readiness snapshot for GET /api/v1/setup/status.
type SetupStatus struct {
	// Ready is server-computed and CONSERVATIVE (false when the runner is nil),
	// so the wizard opens rather than hiding a half-configured bootstrap.
	Ready bool `json:"ready"`
	// Checks is the single list of environment/readiness rows the UI renders.
	Checks []SetupCheck `json:"checks"`
	// Auth is the active public-API auth posture.
	Auth SetupAuth `json:"auth"`
	// Runner is the sandbox runner + the confinement classes actually live on
	// this host (from Runner.Capabilities, same source as /healthz).
	Runner SetupRunner `json:"runner"`
	// Providers reports resident coding-agent CLIs detected on the wardynd host.
	Providers []SetupProvider `json:"providers"`
	// Secrets reports which known secrets are present (NAMES only, reserved
	// names excluded) — never any value.
	Secrets SetupSecrets `json:"secrets"`
	// AgeKey reports whether the at-rest secret store survives a restart.
	AgeKey SetupAgeKey `json:"age_key"`
	// CredentialStorage names the KIND of store credentials live in — never a
	// host, path or vault name (design F-3): local/key_service/vault/key_vault.
	// Unlike Checks (the same fact as admin-only detail), this is KEPT through
	// redactSetupStatusForUser — every person reads it, not just an admin.
	CredentialStorage string `json:"credential_storage,omitempty"`
	// HasRuns drives the wizard's "launch your first run" done state.
	HasRuns bool `json:"has_runs"`
	// OnboardingComplete reports whether an operator has finished (or
	// deliberately left) the Getting Started funnel on this install —
	// SiteConfig.OnboardingCompletedAt, flattened to the only bit the console
	// needs. It is a fact about the install, not about the browser: browser
	// localStorage would outlive wiped databases and disagree with itself
	// between 127.0.0.1 and localhost (different origins, different storage).
	// The first-run landing, the welcome hero and the setup gate all read THIS.
	OnboardingComplete bool `json:"onboarding_complete"`
	// Platform is the OS + WSL posture the environment-step copy keys off.
	Platform SetupPlatform `json:"platform"`
	// HostProxy is the host-side proxy detection (env/shell/git/tool-config/OS)
	// the Host Proxy Getting-Started step renders. Read-only detection; it
	// never configures anything (the upstream-proxy plumbing is separate).
	HostProxy setup.HostProxyDetection `json:"host_proxy"`
	// SCM is the presence-only git-credential posture (gh CLI login, helper,
	// plaintext stores) the ScmProviderStep's ladder recommendations key off.
	SCM setup.SCMPosture `json:"scm"`
	// Integrations is the effective integration set (stored ∪ legacy-derived,
	// see effectiveIntegrations in integrations.go) with each row's live
	// capability matrix — the same shape GET /api/v1/integrations returns,
	// folded in here so the wizard needs one fewer round trip. ADDITIVE field;
	// omitted when empty.
	Integrations []SetupIntegration `json:"integrations,omitempty"`
	// Harnesses is the STATIC coding-agent harness catalog (harnessCatalog,
	// harness.go) — which tools Wardyn knows how to run and whether it can
	// wire each one a managed model credential or a container-login
	// subscription. ADDITIVE field; omitted when empty.
	Harnesses []SetupHarnessTool `json:"harnesses,omitempty"`
	// LLMReady is the server-computed "does SOME run's LLM access path exist"
	// verdict (HIGH-4 review fix): at least one enabled model provider serves an
	// agent (llmPathExists). A DEPLOYMENT fact — whether THIS person can use one
	// is ProviderAccess. It exists because a MEMBER'S redacted response
	// (redactSetupStatusForUser) drops the checks detail that would otherwise
	// let the console derive this itself — LLMReady is computed BEFORE redaction
	// and deliberately left untouched BY it, so the console's readiness chip /
	// new-run banner / demo gating keep working for a member. Kept in exact sync
	// with ui/src/app/lib/types.ts's SetupStatus.
	LLMReady bool `json:"llm_ready"`
	// ChecksRedacted marks a body whose Checks/Providers/Secrets detail was
	// stripped for this caller's tier — an empty list here is withheld, not a fact.
	ChecksRedacted bool `json:"checks_redacted,omitempty"`
	// SCMAccess is THIS PRINCIPAL's Azure DevOps access state — scmaccess.go's
	// computeSCMAccess, ProviderAccess's sibling for #386. Zero value (state "")
	// when no Azure DevOps row is configured; safe for a member by
	// construction (their own state, computed from their own OIDC subject).
	SCMAccess SCMAccess `json:"scm_access,omitzero"`
	// TrustedCACerts is the number of additional roots WARDYN_TRUSTED_CA_FILE
	// loaded at boot (0 = unset). Derived from Config.TrustedCAPEM, never a
	// second boot-time field — see handleSetupStatus. Go + test only: no
	// console reader exists yet (the ui/src/app/lib/types.ts mirror is
	// hand-maintained, added when the Network step renders it) and
	// redactSetupStatusForUser does not zero it — a bare count carries no
	// PEM content, host name, or other detail members are barred from.
	TrustedCACerts int `json:"trusted_ca_certs,omitempty"`
	// ModelProviders is the model providers THIS PRINCIPAL may use, in the
	// member-safe shape (SetupModelProvider) — the same for every tier, so the
	// member redaction has nothing to strip. `omitzero`, not `omitempty`:
	// setupModelProviders returns nil when there is no provider block at all
	// (the field is then absent, exactly as before), and a non-nil, possibly
	// empty slice whenever a block exists — including one this caller is
	// granted nothing from, or one capVisible's own filter-error path
	// answered with rows[:0:0] — so the field is then present as `[]`. A
	// console reading this field can tell "no block" from "granted none"
	// only because of that distinction; `omitempty` could not make it (it
	// drops both nil and an empty-but-present slice alike).
	ModelProviders []SetupModelProvider `json:"model_providers,omitzero"`
	// ProviderAccess is THIS PRINCIPAL's connection state for every provider in
	// ModelProviders (MP-12) — one row per provider. Getting started and the
	// setup checklist read this, so a person granted several providers sees all
	// of them. Kept for members: a state
	// name, an already-composed action sentence, and a deadline instant — no
	// secret names, no start URL. The pin-mismatch action is the one place the
	// pinned account and role appear (SetupProviderAccess's doc). Absent with
	// no provider block.
	ProviderAccess []SetupProviderAccess `json:"provider_access,omitempty"`
}

// SetupCheck is defined in setup_checks.go — setup.go is at its allowlisted
// line-count cap (scripts/check-file-size.sh).

// SetupAuth is the active public-API auth mode: local (loopback bypass) | sso
// (OIDC) | token (admin bearer) | disabled (no auth configured, API closed).
type SetupAuth struct {
	Mode          string `json:"mode"`
	LocalLoopback bool   `json:"local_loopback"`
}

// SetupRunner echoes the runner name and the live confinement classes/substrates.
type SetupRunner struct {
	Driver                string            `json:"driver"`
	ConfinementClasses    []string          `json:"confinement_classes"`
	ConfinementSubstrates map[string]string `json:"confinement_substrates,omitempty"`
	// EphemeralDiskEnforcement is what actually binds a run's disk_mib on this
	// deployment — `filesystem`, `eviction` or `none`; absent reads as `none`
	// (runner.Capabilities.EphemeralDiskEnforcement, weakest across substrates).
	// The Workspace Providers screen renders it beside default_disk_mib so an
	// admin setting a number can see whether anything will hold it.
	//
	// Operator-only: redactSetupStatusForUser rebuilds this struct with
	// ConfinementClasses and Kubernetes alone, so the word never reaches a
	// member. It is deliberately absent from the ANONYMOUS /healthz, which
	// composes its own body field by field.
	EphemeralDiskEnforcement types.StorageEnforcement `json:"ephemeral_disk_enforcement,omitempty"`
	// Kubernetes is the ONE substrate bit a member may read: the runner is the
	// Kubernetes driver. Driver itself stays operator-only, so without this a
	// member's console cannot tell a Kubernetes install apart and would hand
	// them the Docker host's /dev/kvm remedy for Vault. A boolean, deliberately
	// not the driver name or anything a substrate reports about itself.
	Kubernetes bool `json:"kubernetes,omitempty"`
}

// SetupProvider is a coding-agent CLI (claude|codex) detected on the wardynd
// host's PATH.
type SetupProvider struct {
	Tool      string `json:"tool"`
	Installed bool   `json:"installed"`
}

// SetupSecrets reports present secret NAMES (reserved names excluded) and a
// convenience bool for whether both GitHub App secrets are set.
type SetupSecrets struct {
	Present   []string `json:"present"`
	GitHubApp bool     `json:"github_app"`
}

// SetupAgeKey reports whether the secret store survives a restart (a stable
// WARDYN_AGE_KEY was supplied vs an ephemeral generated one).
type SetupAgeKey struct {
	Durable bool `json:"durable"`
}

// SetupPlatform is the wardynd host's OS + WSL posture.
type SetupPlatform struct {
	OS  string `json:"os"`
	WSL bool   `json:"wsl"`
	// KVM: the host exposes /dev/kvm — lets the UI split Vault's "incompatible
	// with this hardware" from a fixable "needs setup" (additive; old UIs ignore).
	KVM bool `json:"kvm"`
}

// llmPathExists is LLMReady: at least one enabled model provider serves an
// agent. A deployment fact, never a person's.
func llmPathExists(sc types.SiteConfig) bool {
	return slices.ContainsFunc(modelProviderRows(sc), func(p types.ModelProvider) bool {
		return !p.Disabled && len(p.Harnesses) > 0
	})
}

// handleSetupStatus assembles the first-run readiness snapshot. It sits behind
// humanOrAdminAuth (reaching it already proves auth: local-mode bypass, an OIDC
// session, or the admin bearer), so it may enumerate resident CLIs, present
// secret names, and per-backend composer readiness — capability disclosure that
// must never appear on the public /healthz.
//
// The handler gathers state; every checklist row is a small pure function below
// (one per item, in the order the wizard renders them).
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	// One capability snapshot for every per-person list below (capVisible).
	ctx := withCapBatch(r.Context())

	// auth: same derivation handleMe uses, plus the "disabled" edge (no auth
	// configured at all — practically unreachable here since adminAuth would have
	// 401'd, but kept honest against the frozen contract).
	authMode := "token"
	switch {
	case s.cfg.LocalMode:
		authMode = "local"
	case oidc.PrincipalFromContext(ctx) != "":
		authMode = "sso"
	case s.cfg.AdminToken == "":
		authMode = "disabled"
	}

	rnr, k8sNetpolProven := setupRunnerInfo(ctx, s.cfg.Runner)

	providers := s.setupProviders()

	// secrets: names only (reserved excluded); github_app iff both App secrets present.
	secretNames, present, sec, err := s.setupSecretsSnapshot(ctx)
	if err != nil {
		writeServerError(w, r, "list secrets", err)
		return
	}

	plat := setup.DetectPlatform()
	// Re-check (setup-screen.tsx) means "look at the host again", so it has to be
	// able to say so: without this it was a client-side refetch of a memo up to
	// 30s old, and a host proxy the operator had just configured could not be
	// made to appear no matter how many times they pressed the button. OPERATOR
	// ONLY — the deployer funnel is the only surface that offers Re-check, and a
	// member must not be able to make the daemon sweep the host on demand.
	hostProxy := cachedHostProxy()
	if r.URL.Query().Get(setupRecheckParam) != "" && s.isOperator(ctx) {
		hostProxy = hostProxyRecheck()
	}
	scmPosture := setup.DetectSCMPosture()

	// ONE site-config read for the whole handler.
	siteCfg, siteCfgOK := s.siteConfigSnapshot(ctx)

	// llm_ready (HIGH-4 review fix): computed BEFORE redaction and left
	// untouched by it (see redactSetupStatusForUser) — a member's console needs
	// the ANSWER even though it can no longer see the detail that produced it.
	llmReady := llmPathExists(siteCfg)
	modelProviders, providerAccess, providerChecks := s.setupModelProviderState(ctx, siteCfg, runIdentitySubject(ctx, principalFromRequest(r)))

	// checks: the rows the wizard renders. "info" is used for permanent /
	// non-fixable or purely-optional conditions so the user is never shown a red
	// they cannot clear. Each granted provider's own row follows LLM access.
	checks := append([]SetupCheck{
		runnerCheck(rnr), agentImageCheck(s.cfg.AgentImages),
		claudeSignInImageCheck(ctx, s.cfg.AgentImages, s.cfg.Runner),
		envBuilderCheck(s.cfg.ImageBuilder != nil), llmProviderCheck(providerAccess),
	}, providerChecks...)
	// confinement_floor: the operator's configured floor vs what this runner
	// can actually enforce — see confinementFloorCheck.
	if chk, ok := confinementFloorCheck(rnr, s.cfg.DefaultPolicy.MinConfinementClass); ok {
		checks = append(checks, chk)
	}
	// k8s_egress_containment: the boot-time NetworkPolicy canary verdict —
	// absent (no row) on a non-k8s driver; see k8sEgressContainmentCheck.
	if chk, ok := k8sEgressContainmentCheck(rnr.Driver, k8sNetpolProven); ok {
		checks = append(checks, chk)
	}

	checks = append(checks, secretStoreChecks(s.cfg.SecretStoreExternal, s.cfg.SecretKeyService, s.cfg.AgeKeyDurable, s.cfg.OIDC != nil, s.cfg.PlatformKeySeparate, s.cfg.KEKRequired)...)
	checks = append(checks, hostProxyCheck(hostProxy, plat.Containerized && !setup.HostProxySeeded()))

	// sso_rbac / tls_cookie_posture: both OIDC-gated (mirror how every other
	// conditional check gates on its own applicability).
	oidcConfigured := s.cfg.OIDC != nil
	if chk, ok := ssoRBACCheck(oidcConfigured, s.cfg.OIDCRoleMapConfigured, s.consoleRoleMappingsPresent(ctx, oidcConfigured), oidcConfigured && s.cfg.OIDC.HasOperatorEmails(), s.oidcDefaultRoleIsAdmin(oidcConfigured)); ok {
		checks = append(checks, chk)
	}
	if chk, ok := tlsCookiePostureCheck(oidcConfigured, s.cfg.OIDCRedirectURL, s.cfg.OIDCSecureCookies); ok {
		checks = append(checks, chk)
	}

	// Filled from the site-config read below, not a second one. A read failure
	// leaves it false, which is the conservative direction: it opens the funnel
	// rather than hiding it.
	onboardingComplete := false
	// From the hoisted read above — one site-config read per status call, not
	// two. It also feeds the agent roster below (setupHarnessTools); a failed
	// read leaves the zero value, which reads as legacy open mode.
	if siteCfgOK {
		checks = siteConfigStatusChecks(checks, siteCfg, present)
		onboardingComplete = siteCfg.OnboardingCompletedAt != nil
	}
	if s.cfg.Store != nil {
		// permissions_posture (#19b): non-blocking/informational, so a read
		// failure here is skipped rather than surfaced as a setup/status 500 —
		// unlike secrets/site-config above, nothing else on this page depends
		// on the enforcement map.
		if enf, err := s.cfg.Store.GetCapabilityEnforcement(ctx); err == nil {
			checks = append(checks, permissionsPostureCheck(enf))
		}
	}

	checks = append(checks, scmProviderCheck(sec.GitHubApp, secretNames, scmPosture))

	// github_ref_ruleset: the only row that leaves the machine. Gated on the App
	// being configured, cached, short-timeout, and never worse than "warn" — see
	// githubRefRulesetCheck.
	if chk, ok := s.githubRefRulesetCheck(ctx, sec.GitHubApp); ok {
		checks = append(checks, chk)
	}
	checks = append(checks, platformChecks(plat)...)

	hasRuns := s.setupHasRuns(ctx)

	// ready: CONSERVATIVE — false when the runner is nil / has no live class, so
	// the wizard opens rather than hiding a half-configured bootstrap.
	// Credentials are warnings, not readiness gates.
	ready := s.cfg.Runner != nil && len(rnr.ConfinementClasses) > 0

	resp := SetupStatus{
		Ready:              ready,
		Checks:             checks,
		Auth:               SetupAuth{Mode: authMode, LocalLoopback: s.cfg.LocalLoopback},
		Runner:             rnr,
		Providers:          providers,
		Secrets:            sec,
		AgeKey:             SetupAgeKey{Durable: s.cfg.AgeKeyDurable},
		CredentialStorage:  credentialStorageMode(s.cfg.SecretStoreExternal, s.cfg.SecretKeyService),
		HasRuns:            hasRuns,
		OnboardingComplete: onboardingComplete,
		Platform:           SetupPlatform{OS: plat.OS, WSL: plat.WSL, KVM: plat.KVM},
		HostProxy:          hostProxy,
		SCM:                scmPosture,
		// The harness and model-provider lists hold only what this caller may use
		// (capVisible; model providers on the roster line, funlen ratchet);
		// llmReady stays the deployment fact.
		Integrations: s.integrationsWithCapabilities(ctx, present),
		Harnesses:    capVisible(ctx, s, capAgent, setupHarnessTools(siteCfg, s.cfg.AgentImages, modelProviders), setupHarnessToolID), ModelProviders: modelProviders, ProviderAccess: providerAccess,
		LLMReady:  llmReady,
		SCMAccess: s.scmAccessValue(ctx, siteCfg, oidcHumanFromContext(ctx)), // #386: absent -> zero value
		// A count derived from the SAME PEM string TrustedCAPEM's doc comment
		// describes — no second boot-time field to keep in sync. 0 when unset.
		TrustedCACerts: strings.Count(s.cfg.TrustedCAPEM, "-----BEGIN CERTIFICATE-----"),
	}
	// DELIBERATELY isOperator (three-tier doctrine, internal/auth/oidc's
	// RoleSecurityAdmin): what this redaction drops is the DEPLOYER's funnel —
	// the environment/credential checklist, resident-CLI login detection,
	// secret names, runner detail — every row of it actionable only through a
	// setup mutation, which stays super-only. A security admin sees the same
	// summary a member does because there is nothing here they could act on.
	if !s.isOperator(ctx) {
		resp = redactSetupStatusForUser(resp)
	}
	writeJSON(w, http.StatusOK, resp)
}

// setupHasRuns is the has_runs EXISTENCE check (LIMIT 1, the Pager idiom
// firstBrokeredRepoFromRuns already uses — never the whole run table). Split
// out of handleSetupStatus (funlen ratchet), like oidcDefaultRoleIsAdmin.
func (s *Server) setupHasRuns(ctx context.Context) bool {
	if s.cfg.Store == nil {
		return false
	}
	var runs []types.AgentRun
	var err error
	if pg, ok := s.cfg.Store.(store.Pager); ok {
		runs, err = pg.ListRunsPage(ctx, store.Page{Limit: 1})
	} else {
		runs, err = s.cfg.Store.ListRuns(ctx)
	}
	return err == nil && len(runs) > 0
}

// consoleRoleMappingsPresent reports whether any console role-mapping rows
// exist, for ssoRBACCheck's merged-map presence input. It follows the SAME
// nil-Store guard the permissions_posture read applies: a nil Store or a
// failed read reports false, the conservative direction — it surfaces the
// sso_rbac warning rather than silently hiding it behind a People-step row
// this call could not actually confirm exists.
func (s *Server) consoleRoleMappingsPresent(ctx context.Context, oidcConfigured bool) bool {
	if !oidcConfigured || s.cfg.Store == nil {
		return false
	}
	rows, err := s.cfg.Store.ListRoleMappings(ctx)
	return err == nil && len(rows) > 0
}

// oidcDefaultRoleIsAdmin reports whether WARDYN_OIDC_DEFAULT_ROLE resolves to
// admin — ssoRBACCheck's defaultRoleAdmin input (#491). Split out of
// handleSetupStatus (which is otherwise inline) to keep it under the gocyclo
// gate; s.cfg.OIDC already carries the boot-validated DefaultRole, so no new
// Config field is needed. Deciding "is this the admin role" is
// oidc.Authenticator's own call (DefaultRoleIsAdmin), not a bare == RoleAdmin
// comparison here — internal/api/refusal_test.go's roleComparisons guard
// stays shrink-only.
func (s *Server) oidcDefaultRoleIsAdmin(oidcConfigured bool) bool {
	return oidcConfigured && s.cfg.OIDC.DefaultRoleIsAdmin()
}

// redactSetupStatusForUser drops the operator/admin-facing DIAGNOSTIC detail
// a member has no route to act on — the environment/credential checklist rows,
// resident-CLI login detection, and secret NAMES — the explicit drop list
// (checks/providers/secret names/runner detail), plus the integration rows' OWN
// credential refs, egress hosts and operator config, which are secret names by
// another name and were shipping in the same body — while keeping everything a
// member's own console needs: Ready/Auth (App.tsx's reachability gate) and
// HasRuns, plus every field the run-launch UI reads (Bedrock.Ready,
// Harness*, Integrations, Platform, AgeKey) so a member can still launch runs
// normally. This only ZEROES fields on an already-computed,
// already-200 response — it can never itself produce an error state (no
// non-401 error is possible for a member here, by construction).
//
// Runner.ConfinementClasses survives redaction: it is not diagnostic detail,
// it is the barrier-count signal ui/lib/readiness.ts's deriveReadiness reads
// verbatim to compute barrierReady, which gates the keyless demos' Start
// button (demo-screen.tsx) for every role. Dropping it zeroed barrierReady
// for every member regardless of the real runner state. Only Driver and the
// per-class ConfinementSubstrates map — genuine diagnostic detail — are
// dropped. So is EphemeralDiskEnforcement: which word binds a run's disk_mib
// is an operator's sizing answer, actionable only on the providers/setup
// surfaces a member has no route to. Kubernetes survives as a bare boolean
// (#1238) — the anonymous /healthz already names the runner, so it discloses
// nothing new. The strip is structural
// (the SetupRunner below is rebuilt from ConfinementClasses and the Kubernetes bit alone, so a field
// added later is dropped by default rather than by a line somebody remembered
// to write); TestRedactSetupStatusForMember_DropsHostCredentialPosture pins it.
// Secrets.Present keeps demoSecretNames' presence bits (#850): those are the
// console demo catalog's own seed-secret names
// (demo-catalog-secrets.ts's needsSecret values, e.g. "wardyn-demo-key"),
// already public in the shipped client bundle — knowing one is stored says
// nothing about the deployment's real credential posture, unlike a real
// provider secret name. Without this, walkableDemos/stepOrder
// (setup/steps.ts) never offer a demo whose secret an admin has in fact
// stored, because their only signal is this same, otherwise fully redacted,
// list.
func redactSetupStatusForUser(st SetupStatus) SetupStatus {
	st.Checks = []SetupCheck{}
	// Say the strip happened, so a reader never takes [] for "nothing is wired".
	st.ChecksRedacted = true
	st.Providers = []SetupProvider{}
	st.Secrets = SetupSecrets{Present: demoSecretPresence(st.Secrets.Present)}
	st.Runner = SetupRunner{ConfinementClasses: st.Runner.ConfinementClasses, Kubernetes: st.Runner.Kubernetes}
	// Host credential/environment posture — a description of the OPERATOR'S
	// MACHINE, not of anything a member can act on, and the last place a member
	// could read it off this endpoint. SCM names which git credentials sit on
	// the wardynd host's disk (a gh session, ~/.git-credentials, ~/.netrc, a
	// plaintext-ish "store"/"cache" helper); HostProxy carries the corporate
	// proxy topology, host:port and a "the operator's proxy credentials live
	// here" flag.
	st.SCM = setup.SCMPosture{}
	st.HostProxy = setup.HostProxyDetection{}
	// The same rows /integrations publishes, and the same projection. Dropping
	// SetupSecrets.Present as "secret NAMES" while shipping
	// integrations[].secrets[].secret_name in the SAME response body was the
	// contradiction: one credential-ref list withheld, an equivalent one beside
	// it passed through, together with the internal egress hosts and the
	// operator's connection config.
	st.Integrations = userSafeIntegrations(st.Integrations)
	return st
}

// demoSecretNames are ui/src/app/components/screens/demos/demo-catalog-secrets.ts's
// needsSecret values verbatim — the console demo catalog's own seed-secret
// names, kept here as the one server-side spelling so a renamed or added demo
// secret is a single-line diff in both places, not a drift risk.
var demoSecretNames = map[string]bool{
	"wardyn-demo-key":       true,
	"wardyn-demo-api-token": true,
	"wardyn-demo-pat":       true,
	"wardyn-demo-ssh-key":   true,
}

// demoSecretPresence narrows a secret-name list to the ones demoSecretNames
// lists — see redactSetupStatusForUser's own comment for why this subset
// alone survives redaction.
func demoSecretPresence(present []string) []string {
	out := make([]string, 0, len(present))
	for _, n := range present {
		if demoSecretNames[n] {
			out = append(out, n)
		}
	}
	return out
}

// setupProviders detects the resident coding-agent CLIs on the wardynd host.
func (s *Server) setupProviders() []SetupProvider {
	provs := setup.DetectCLIProviders()
	providers := make([]SetupProvider, 0, len(provs))
	for _, p := range provs {
		providers = append(providers, SetupProvider{Tool: p.Tool, Installed: p.Installed})
	}
	return providers
}

// siteConfigSnapshot reads the one site-config document, reporting whether the
// read SUCCEEDED — as opposed to a zero document, which is a legitimate stored
// value. A nil store or a failed read is (zero, false): every consumer treats
// that as legacy open mode rather than as configuration.
func (s *Server) siteConfigSnapshot(ctx context.Context) (types.SiteConfig, bool) {
	if s.cfg.Store == nil {
		return types.SiteConfig{}, false
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return types.SiteConfig{}, false
	}
	return sc, true
}

// setupRunnerInfo reports the live runner selection for /setup/status, copying
// handleHealthz's Capabilities pattern: a nil runner (or one whose Capabilities
// call fails) reads honestly as "none" with no classes rather than claiming
// isolation this host cannot deliver.
//
// The second return value is the k8s substrate's boot-time egress-canary
// verdict ("enforced"/"unenforced"/"") — computed here (it needs the SAME
// Capabilities() call this function already makes) but deliberately NOT part
// of the SetupRunner struct: nothing on the wire reads it, only the caller's
// k8sEgressContainmentCheck (setup_checks.go), fed directly, a local value is
// enough. "" covers three real cases, not two: a non-k8s driver; a k8s daemon
// build that predates this computation; AND a genuine k8s driver whose
// Capabilities() call itself just errored (the `err != nil` return below) —
// that third case is not hypothetical, it is this very function's own
// early-return path, which already leaves Driver "k8s" with nothing else
// filled in. k8sEgressContainmentCheck grades all three the same honest way
// (Indeterminate/FAIL, never a silent Enforcing).
func setupRunnerInfo(ctx context.Context, rn runner.Runner) (SetupRunner, string) {
	out := SetupRunner{Driver: "none", ConfinementClasses: []string{}}
	if rn == nil {
		return out, ""
	}
	out.Driver = rn.Name()
	out.Kubernetes = out.Driver == "k8s"
	c, err := rn.Capabilities(ctx)
	if err != nil {
		return out, ""
	}
	for _, cc := range c.ConfinementClasses {
		out.ConfinementClasses = append(out.ConfinementClasses, string(cc))
	}
	if len(c.Resolved) > 0 {
		out.ConfinementSubstrates = make(map[string]string, len(c.Resolved))
		for k, v := range c.Resolved {
			out.ConfinementSubstrates[string(k)] = v
		}
	}
	out.EphemeralDiskEnforcement = c.EphemeralDiskEnforcement
	return out, k8sNetpolVerdict(out.Driver, c)
}

// k8sNetpolVerdict grades a runner's aggregated NetworkPolicy signals into the
// three live-daemon verdicts, or "" on any non-k8s driver. Shared by
// setupRunnerInfo (admin-only /setup/status, its return value here is a k8s
// egress-containment checklist row) and handleHealthz (anonymous /healthz's
// "network_policy" field) — both already hold a driver name and a
// runner.Capabilities from a successful Capabilities() call; a driver whose
// Capabilities() itself errored never reaches this function, so that case
// grades "" the same way a non-k8s driver does, at the caller.
func k8sNetpolVerdict(driver string, caps runner.Capabilities) string {
	if driver != "k8s" {
		return ""
	}
	// caps.NetworkPolicy / caps.NetworkPolicyAcknowledged are the
	// orchestrator-aggregated ClassSupport signals; a genuinely indeterminate
	// canary (no ack, no override) refuses to boot entirely (internal/runner/
	// k8s's newWithClient), so a LIVE daemon can only ever report one of these
	// three. Acknowledged checked first: WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY
	// produces a driver that never set NetworkPolicy true (it is not proof), so
	// the two are mutually exclusive in practice, but acknowledged-not-proven
	// must never read as the stronger "enforced" claim if that ever changed.
	switch {
	case caps.NetworkPolicyAcknowledged:
		return "acknowledged"
	case caps.NetworkPolicy:
		return "enforced"
	default:
		return "unenforced"
	}
}

// refRulesetTTL is how long one github_ref_ruleset answer is reused. The wizard
// polls /setup/status; without this every poll would be an api.github.com round
// trip and, on a busy installation, a rate-limit.
const refRulesetTTL = 5 * time.Minute

// refRulesetTimeout bounds the whole outbound probe (a token mint, up to two
// rule reads, one ruleset read per ruleset found, and a token revoke). Short on
// purpose: this row is advisory, and handleSetupStatus is a page load.
const refRulesetTimeout = 5 * time.Second

// githubRefRulesetCheck asks GitHub whether the App is actually ref-confined on
// a granted repo, and caches the answer for refRulesetTTL.
//
// This is the ONLY setup check that leaves the machine — every other row is
// local inspection (secret NAMES, env/file detection, the control plane's own
// Postgres, a runner capability probe). Three things keep that from being a
// regression:
//
//   - It is skipped entirely unless a GitHub App is configured AND a verifier is
//     wired AND a concrete repo is known — see firstBrokeredRepo for the two
//     places that can come from. A deployment with neither signal never makes
//     the call and never sees the row.
//   - Every failure — timeout, rate limit, 403 on the permission, a repo the
//     installation cannot see — grades "info"/unknown. A network blip must not
//     read as a security regression.
//   - The result is cached, so the wizard's polling cannot amplify it.
//
// It never grades "fail": like scmProviderCheck, it is not a gate. The gate is
// the opt-in broker.envRequireRefRuleset, and it lives in the mint path.
func (s *Server) githubRefRulesetCheck(ctx context.Context, githubApp bool) (SetupCheck, bool) {
	if !githubApp || s.cfg.GitHubRulesets == nil {
		return SetupCheck{}, false
	}
	s.refRulesetMu.Lock()
	defer s.refRulesetMu.Unlock()
	if !s.refRulesetAt.IsZero() && s.cfg.Now().Sub(s.refRulesetAt) < refRulesetTTL {
		return s.refRulesetRow, s.refRulesetShow
	}

	repo := s.firstBrokeredRepo(ctx)
	if repo == "" {
		s.refRulesetAt, s.refRulesetRow, s.refRulesetShow = s.cfg.Now(), SetupCheck{}, false
		return SetupCheck{}, false
	}
	probeCtx, cancel := context.WithTimeout(ctx, refRulesetTimeout)
	defer cancel()
	confined, detail, err := s.cfg.GitHubRulesets.VerifyRefRuleset(probeCtx, repo)

	s.refRulesetAt = s.cfg.Now()
	s.refRulesetRow = refRulesetCheck(repo, confined, detail, err)
	s.refRulesetShow = true
	return s.refRulesetRow, true
}

// firstBrokeredRepo returns the first "owner/name" Wardyn can name a concrete
// repo for. It tries the default policy, then any stored policy, for a
// github_token grant whose scope.repos is non-empty — a deliberate,
// admin-authored signal, but rare in practice: shipped example policies
// (examples/policies/*.json) all carry "repos": [] because eligible_grants
// are TEMPLATES the run fills in. Falls back to firstBrokeredRepoFromRuns
// (setup_checks.go) for the common case that leaves: a repo declared on an
// actual run, not a static policy field. "" when neither source has
// anything: a fresh install has nothing to probe, and the row is omitted
// rather than guessed at.
func (s *Server) firstBrokeredRepo(ctx context.Context) string {
	specs := []types.RunPolicySpec{s.cfg.DefaultPolicy}
	if s.cfg.Store != nil {
		if pols, err := s.cfg.Store.ListPolicies(ctx); err == nil {
			for _, p := range pols {
				specs = append(specs, p.Spec)
			}
		}
	}
	for _, spec := range specs {
		for _, g := range spec.EligibleGrants {
			if g.Kind != types.GrantGitHubToken {
				continue
			}
			if repos := githubScopeRepos(g.Scope); len(repos) > 0 {
				return repos[0]
			}
		}
	}
	return s.firstBrokeredRepoFromRuns(ctx)
}
