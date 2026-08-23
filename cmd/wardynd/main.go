// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardynd is the Wardyn control plane: REST API, embedded web UI,
// policy engine, approval FSM, token broker, and audit ingest. Postgres is the
// ONLY required dependency. It contains zero target-specific code — sandboxes
// are dispatched through the runner.Runner interface (docker driver optional).
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"os/user"
	"slices"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	_ "github.com/cjohnstoniv/wardyn/internal/secretstore/pg" // register "pg" secret store
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Secret names seeded/used at boot.
const (
	secretSigningKey   = "wardyn-signing-key"    // embedded identity ES256 PEM
	secretGitHubAppID  = "github-app-id"         // GitHub App numeric id
	secretGitHubAppKey = "github-app-key"        // GitHub App PEM private key
	secretSessionKey   = "wardyn-session-key"    // OIDC session-cookie HMAC key (32 bytes)
	secretSSHHostKey   = "wardyn-ssh-host-key"   // SSH gateway ed25519 host key PEM
	secretUISessionKey = "wardyn-ui-session-key" // UI-sandbox relay cookie HMAC key (32 bytes)
)

// Host-sensor (eBPF ground-truth) token parameters. The audience MUST match the
// api package's groundtruthAudience (kept in sync as a literal because that
// const is unexported). The sentinel run id is fixed: the host sensor is
// host-scoped, not per-run, and the ground-truth auth middleware verifies only
// the audience (it ignores the run claims).
const (
	groundtruthAudience  = "wardyn-groundtruth"
	groundtruthSensorSub = "wardyn-tetragon-ingest"
)

// groundtruthSensorRunID is the fixed sentinel run id the host-sensor token is
// bound to (uuid.Nil): the sensor is host-scoped, not per-run.
var groundtruthSensorRunID = uuid.Nil

func main() {
	if err := run(); err != nil {
		slog.Error("wardynd: fatal", slog.Any("err", err))
		os.Exit(1)
	}
}

// run is wardynd's boot sequence: one linear ordered chain (validate config →
// connect+migrate → build secrets/identity/broker/approvals/runner →
// construct the Server → start background workers → serve) where each
// phase's ORDER is load-bearing (e.g. the runner must exist before the
// Server is constructed with it). Each phase already lives in its own helper
// (validateConfig, connectAndMigrate, buildAuditChain, buildSecretStore,
// buildRunnerFromFlags, buildOptionalFeatures, startBackgroundWorkers,
// startSSHGateway, serveAndShutdown, …) — this function is the one place the
// sequencing itself can be audited top-to-bottom. Low branching (passes
// gocyclo/gocognit), just long.
//
//nolint:funlen // Deliberate: see the doc comment above — one linear ordered boot sequence kept in one scope on purpose, each phase already extracted into its own helper.
func run() error {
	f := parseBootFlags()

	// -gen-age-key: EARLY EXIT before validateConfig / any DB or pool work, so it
	// needs no DSN. Mirrors the -print-groundtruth-token early-exit pattern.
	if *f.genAgeKey {
		return genAndPrintAgeKey(os.Stdout)
	}

	// Validate + derive the TLS/DSN posture from the resolved flag/env values.
	// Extracted into a pure helper (validateConfig) so the fail-closed rules —
	// DSN required, TLS cert+key both-or-neither, Secure-cookie derivation — are
	// unit-testable without standing up the whole daemon.
	posture, err := validateConfig(*f.dsn, *f.tlsCert, *f.tlsKey, *f.listen, *f.tlsTerminated, *f.allowPlaintextListen)
	if err != nil {
		return err
	}
	if err := validateUISandboxConfig(*f.uiListen, *f.listen, *f.sshListen, *f.uiOriginTemplate, posture, *f.allowPlaintextListen); err != nil {
		return err
	}

	// Parse the agent images map at boot so a malformed value fails closed
	// immediately rather than silently using the convention for all agents.
	var agentImages map[string]string
	if *f.agentImagesJSON != "" {
		if err := json.Unmarshal([]byte(*f.agentImagesJSON), &agentImages); err != nil {
			return fmt.Errorf("parse WARDYN_AGENT_IMAGES: %w", err)
		}
		slog.Info("wardynd: agent image overrides", slog.Any("images", agentImages))
	}

	// LOCAL HOST MODE posture (fail closed on a routable no-auth bind); see
	// resolveLocalMode for the full rules + the host-mode Bedrock auto-detect.
	lm, err := resolveLocalMode(f)
	if err != nil {
		return err
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Connect + migrate (Postgres is the only required dependency). See
	// connectAndMigrate for the WARDYN_PG_MIGRATE_DSN role-split (DDL protection)
	// and the separate connect/migrate timeout budgets (W28-S1-4) — a fixed 30s
	// bounds the connect, -migrate-timeout/WARDYN_MIGRATE_TIMEOUT (default 5m)
	// bounds db.Migrate so a slow migration doesn't crash-loop the upgrade.
	pool, err := connectAndMigrate(rootCtx, *f.dsn, *f.migrateDSN, 30*time.Second, *f.migrateTimeout)
	if err != nil {
		return err
	}
	defer pool.Close()

	// bootCtx bounds the REST of the boot sequence (signing-key load, optional
	// features) — unrelated to the connect/migrate split above, which now runs
	// under its own two budgets rather than sharing this one.
	bootCtx, cancel := context.WithTimeout(rootCtx, 30*time.Second)
	defer cancel()

	// SecretRegistry: process-wide secret masking registry. Minted github_token
	// values and resolved api_key injection values are registered here so they
	// can be masked ("<secret-hidden>") from PTY/asciicast captures and audit
	// event fields before they leave the control-plane process. Constructed early
	// so the identity provider and broker mask their audit events too.
	maskReg := secretmask.NewRegistry()
	// The masked + fanned-out + spooling recorder chain shared by EVERY audit
	// writer (API, broker, identity, approvals, sweeper) — see buildAuditChain.
	maskedRec, fan, auditSpool, auditDrainRec, err := buildAuditChain(rootCtx, *f.auditSinks, *f.auditSpool, pool, maskReg)
	if err != nil {
		return err
	}

	// Secret store (pluggable seam; default "pg" = age-encrypted Postgres column).
	secrets, err := buildSecretStore(pool, *f.ageKey, *f.secretStoreSel)
	if err != nil {
		return err
	}

	// Embedded identity provider: signing key persisted in the secret store,
	// generated on first boot. The pg-backed revocation store is the kill-switch
	// denylist (identity_revocations).
	signKey, err := loadOrCreateSigningKey(bootCtx, secrets)
	if err != nil {
		return err
	}
	// Identity provider (pluggable seam; default "embedded"). pgRevocations is the
	// pg-backed kill-switch denylist, supplied to whichever provider is selected.
	idp, err := identity.New(*f.identitySel, identity.Deps{
		SigningKey:  signKey,
		TrustDomain: *f.trustDomain,
		Revocations: &pgRevocations{pool: pool},
		Audit:       maskedRec,
	})
	if err != nil {
		return fmt.Errorf("identity provider: %w", err)
	}

	// Host-sensor token minting (-print-groundtruth-token). Mint a token bound
	// to the SEPARATE aud="wardyn-groundtruth" so the eBPF/Tetragon ingest
	// sidecar can authenticate to POST /api/v1/internal/groundtruth. The token
	// is audit-write-only by construction: the mint/approval endpoints verify
	// aud="wardyn-internal" and reject this audience. We bind it to a fixed
	// sentinel run id (it is host-scoped, not per-run); the ground-truth auth
	// middleware ignores the run claims and checks only the audience. Print and
	// exit so this slots cleanly into a compose token-seeding step.
	if *f.printGroundtruthToken {
		mintCtx, mintCancel := context.WithTimeout(rootCtx, 10*time.Second)
		defer mintCancel()
		ri, merr := idp.MintRunIdentity(mintCtx, groundtruthSensorRunID, groundtruthSensorSub, groundtruthSensorSub, groundtruthAudience)
		if merr != nil {
			return fmt.Errorf("mint groundtruth token: %w", merr)
		}
		fmt.Println(ri.Token)
		return nil
	}

	// Token broker: GitHub minter only when the App credentials are present;
	// otherwise github_token grants fail closed at mint with a clear error.
	// The broker shares maskedRec so its credential.* events fan out to SIEM.
	gh := buildGitHubMinter(secrets)
	brk := broker.New(broker.NewPgxStore(pool), secrets, maskedRec, idp, gh).WithMaskRegistry(maskReg)
	// D29: the credential.mint SUCCESS event is written in-tx (durable/atomic) and
	// fanned to SIEM post-commit via this sink — so keep SIEM continuity without
	// double-writing the primary store. Guarded: a typed-nil Fanout would be a
	// non-nil interface that panics on Emit.
	if fan != nil {
		brk = brk.WithSIEM(fan)
	}

	// Approval FSM service (adapter over internal/approval + internal/store).
	// FIX #5: wired with maskedRec (masked + SIEM fanout), matching idp/broker —
	// approval.decide events now reach file/webhook/syslog sinks, not just Postgres.
	approvals := &approvalService{st: approvalStore{PG: store.NewPG(pool), rec: maskedRec}}

	// Runner (optional): "none" or a self-registered substrate (the docker
	// substrate registers itself only under the "docker" build tag), with
	// fail-closed confinement pins. The pg-backed RefStore makes the
	// orchestrator's ref->substrate routing (and thus the kill switch) durable
	// across control-plane restarts.
	run, runnerTarget, err := buildRunnerFromFlags(f, store.NewPG(pool))
	if err != nil {
		return err
	}

	defaultPolicy, err := api.LoadPolicySpec(*f.policyPath)
	if err != nil {
		return err
	}

	if *f.adminToken == "" && !lm.enabled {
		slog.Warn("wardynd: admin token unset; the public API is DISABLED (only /healthz responds). Set WARDYN_ADMIN_TOKEN, enable OIDC, or use -local-mode for single-developer localhost use.")
	}

	// Optional subsystems (recording replay, OIDC SSO, devcontainer builds,
	// subscription/managed LLM credential providers, advisory AI scan
	// fallback) — each nil/off when unconfigured; see buildOptionalFeatures.
	feats, err := buildOptionalFeatures(rootCtx, bootCtx, f, pool, secrets, posture.secureCookies)
	if err != nil {
		return err
	}

	srv := api.New(api.Config{
		Store:     store.NewPG(pool),
		Identity:  idp,
		Approvals: approvals,
		Broker:    brk,
		// Same minter, second use: the setup checklist asks it whether GitHub
		// confines the App to the run branch namespace. nil when no App is
		// configured, which omits the row.
		GitHubRulesets: gh,
		Audit:          maskedRec,
		// hand the raw spool + raw store recorder to the server so it starts
		// the background drain that replays spooled events back into the store once
		// PG recovers (both nil when no spool is configured => drain is a no-op).
		AuditSpool:         auditSpool,
		AuditDrainRecorder: auditDrainRec,
		AuditSinkDrops:     sinkDropsReporter(fan),
		Runner:             run,
		AdminToken:         *f.adminToken,
		LocalMode:          lm.enabled,
		LocalOperator:      lm.operator,
		TrustDomain:        *f.trustDomain,
		DefaultPolicy:      defaultPolicy,
		RunnerTarget:       runnerTarget,
		UIDir:              *f.uiDir,
		ControlPlaneURL:    *f.controlURL,
		RecordingStore:     feats.recStore,
		OIDC:               feats.authn,
		// D16: same store buildOptionalFeatures wired into oidc.Config.Revocations
		// (the read side Middleware checks), given here to internal/api so the
		// admin revoke-sessions endpoint has the write side. nil exactly when
		// OIDC is unconfigured — sessionsRevocable's own nil-safe gate on both.
		SessionRevocations:        sessionRevocationsFor(feats.authn, pool),
		OperatorEmails:            splitCSV(*f.oidcOperatorEmails),
		ImageBuilder:              feats.imgBuilder,
		AgentImages:               agentImages,
		AgentAnthropicModel:       *f.agentModel,
		BedrockRegion:             *f.bedrockRegion,
		BedrockModel:              *f.bedrockModel,
		BedrockAWSConfigDir:       *f.bedrockAWSDir,
		BedrockAWSProfile:         *f.bedrockAWSProfile,
		BedrockAWSSSORegion:       *f.bedrockAWSSSORegion,
		ProxyURL:                  *f.proxyURL,
		Secrets:                   secrets,
		MaskRegistry:              maskReg,
		SubscriptionToken:         feats.subToken,
		ManagedToken:              feats.managedToken,
		DisableSubscriptionInject: feats.disableSubInject,
		Components:                componentsInfo(f, runnerTarget, feats.recStore),
		ScanAIAdvisor:             feats.scanAdvisor,
		// First-run setup readiness inputs (GET /api/v1/setup/status).
		AgeKeyDurable:         strings.TrimSpace(*f.ageKey) != "",
		LocalLoopback:         lm.loopback,
		LocalTrustForwarder:   *f.localTrustFwd,
		OIDCRoleMapConfigured: strings.TrimSpace(*f.oidcRoleMap) != "",
		OIDCRedirectURL:       *f.oidcRedirectURL,
		OIDCSecureCookies:     posture.secureCookies,
		// SSH gateway (C2/C3): SSHHostKey is nil unless -ssh-listen is set
		// (buildOptionalFeatures), which is also the sole gate ServeSSHGateway
		// itself checks below — belt and suspenders, "empty = off" holds either
		// way this Config is constructed.
		SSHListenAddr:    *f.sshListen,
		SSHAdvertiseAddr: *f.sshAdvertise,
		SSHHostKey:       feats.sshHostKey,
		// UI-sandbox gateway (pillar 4): same "empty = off" shape as SSH above —
		// UISessionKey is nil unless -ui-sandbox-listen is set, and the gateway
		// checks both.
		UIListenAddr:     *f.uiListen,
		UIAdvertiseURL:   *f.uiAdvertise,
		UIOriginTemplate: *f.uiOriginTemplate,
		UISessionKey:     feats.uiSessionKey,
		// rootCtx is the daemon-lifetime base context for detached background
		// work (the run completion watcher) that must outlive the create-run
		// request. It is cancelled on SIGINT/SIGTERM at shutdown.
		BaseCtx: rootCtx,
	})

	// Periodic goroutines (lifecycle reaper, groundtruth token rotator, approval
	// expiry sweeper) + the boot-time reconciliation pass (C3).
	startBackgroundWorkers(rootCtx, f, srv, run, pool, idp, brk, maskedRec, feats.recStore)

	// SSH gateway accept loop (own goroutine, like the periodic workers above,
	// and extracted the same way — see startSSHGateway's own doc comment).
	startSSHGateway(rootCtx, f, srv)

	// UI-sandbox gateway: a SECOND HTTP listener on its own origin (see
	// startUISandboxGateway; a no-op when -ui-sandbox-listen is empty).
	startUISandboxGateway(rootCtx, f, posture, srv)

	// Serve until signal/error, then drain: HTTP first, audit sinks last.
	return serveAndShutdown(rootCtx, f, posture, srv.Handler(), idp.Name(), fan)
}

// tlsPosture is the validated TLS/cookie posture derived from the resolved
// config. tlsEnabled is true only when wardynd serves built-in TLS (cert+key both
// set); secureCookies is true when the connection is TLS-protected end to end
// (built-in TLS OR an upstream TLS-terminating proxy via WARDYN_TLS_TERMINATED).
type tlsPosture struct {
	tlsEnabled    bool
	secureCookies bool
}

// validateConfig applies the boot-time fail-closed configuration rules and
// derives the TLS/cookie posture. It is a pure function of the already-resolved
// (flag-or-env) values so it can be unit-tested in isolation:
//
//   - dsn is REQUIRED (Postgres is the only mandatory dependency).
//   - TLS cert and key are both-or-neither: setting exactly one is a
//     misconfiguration that fails closed (a half-configured TLS posture would
//     silently fall back to plain HTTP, which is worse than a loud error).
//   - secureCookies is true when TLS protects the connection end to end —
//     either wardynd serves built-in TLS, or TLS terminates at an upstream proxy
//     (tlsTerminated). When neither holds it MUST stay false: Secure cookies are
//     never sent over plain HTTP and would break login.
//   - plaintext HTTP (no built-in TLS, no tlsTerminated) on a SPECIFIC
//     non-loopback bind fails closed too, same refuse-vs-warn split as the
//     demo-admin-token and -local-trust-forwarder gates below: loopback and the
//     unspecified bind (":8080", the compose 0.0.0.0-in-container topology) stay
//     warn-only (boot_serve.go), since the unspecified bind is indistinguishable
//     from a safe compose 127.0.0.1-publish from inside the container.
//     allowPlaintextListen is the explicit escape hatch (WARDYN_ALLOW_PLAINTEXT_LISTEN).
func validateConfig(dsn, tlsCert, tlsKey, listen string, tlsTerminated, allowPlaintextListen bool) (tlsPosture, error) {
	if dsn == "" {
		return tlsPosture{}, errors.New("missing -dsn / WARDYN_PG_DSN")
	}
	if (tlsCert != "") != (tlsKey != "") {
		return tlsPosture{}, errors.New("TLS misconfigured: set BOTH -tls-cert/WARDYN_TLS_CERT and -tls-key/WARDYN_TLS_KEY, or neither")
	}
	tlsEnabled := tlsCert != "" && tlsKey != ""
	posture := tlsPosture{
		tlsEnabled:    tlsEnabled,
		secureCookies: tlsEnabled || tlsTerminated,
	}
	if err := refusePlaintextListen("-listen", listen, posture, allowPlaintextListen); err != nil {
		return tlsPosture{}, err
	}
	return posture, nil
}

// refusePlaintextListen is the plaintext-on-a-specific-routable-bind rule
// itself, shared because wardynd serves TWO listeners off ONE TLS posture: the
// console (-listen) and the UI-sandbox gateway (-ui-sandbox-listen, which
// reuses the same cert/key or the same upstream terminator). A deployment with
// neither makes BOTH cleartext, and the gateway's `wardyn_ui_sess` cookie is a
// bearer credential for a run exactly as the console's session is for the
// control plane — 8h, `Secure=false` in that posture, and readable off the wire
// by any LAN peer. Guarding only the console would leave the second listener
// serving the very thing the first one refuses to.
//
// The carve-outs are deliberately identical for both: loopback and the
// unspecified bind (the compose 0.0.0.0-in-container topology) stay warn-only,
// and WARDYN_ALLOW_PLAINTEXT_LISTEN is the one explicit escape hatch.
func refusePlaintextListen(flagName, listen string, posture tlsPosture, allowPlaintextListen bool) error {
	if posture.secureCookies || allowPlaintextListen || !listenBindsSpecificRoutable(listen) {
		return nil
	}
	return fmt.Errorf("refusing to start: serving plaintext HTTP but the %s address %q binds a specific non-loopback interface — "+
		"every credential and cookie it speaks would travel in cleartext to any LAN/WAN peer; "+
		"configure WARDYN_TLS_CERT/WARDYN_TLS_KEY for built-in TLS, set WARDYN_TLS_TERMINATED=true behind a TLS-terminating reverse proxy, "+
		"or explicitly set WARDYN_ALLOW_PLAINTEXT_LISTEN=true to override", flagName, listen)
}

// validateUISandboxConfig is the UI-sandbox gateway's boot-time fail-closed
// rule, kept beside validateConfig and pure for the same reason.
//
// The refusal that matters is the SAME-ADDRESS one. Everything the gateway
// relays is the sandbox's OWN code, and the only thing keeping that code away
// from the console's session storage and admin actions is that it arrives on a
// different browser origin. Bound to the console's address, the gateway would
// not merely fail to listen twice — the feature's entire security argument
// would be false. So it is refused loudly here, in terms of what breaks, rather
// than surfacing as "address already in use". The SSH gateway's address is
// checked too: a shared port there is a plain misconfiguration, but it is one
// boot can name instead of leaving to a bind error.
//
// The origin template, when set, must carry {run} — a template without it would
// hand EVERY run the same host, silently turning per-run isolation back into
// the shared origin it exists to replace.
//
// The TLS posture is taken rather than re-derived so this listener answers to
// the SAME plaintext refusal the console does (refusePlaintextListen): the
// relay session cookie is a bearer credential, and it travels on this address.
func validateUISandboxConfig(uiListen, listen, sshListen, originTemplate string, posture tlsPosture, allowPlaintextListen bool) error {
	if uiListen == "" {
		return nil // off: nothing to validate, no listener, no new surface
	}
	if err := refusePlaintextListen("-ui-sandbox-listen", uiListen, posture, allowPlaintextListen); err != nil {
		return err
	}
	if sameListenAddress(uiListen, listen) {
		return fmt.Errorf("refusing to start: -ui-sandbox-listen %q is the same address as -listen — "+
			"the UI-sandbox gateway relays the SANDBOX's own pages, and serving them on the console's origin would let that "+
			"sandbox-authored code read the console session and drive every admin action the operator can; "+
			"give the gateway its own address (e.g. \":8081\") or unset WARDYN_UI_SANDBOX_LISTEN to disable it", uiListen)
	}
	if sshListen != "" && sameListenAddress(uiListen, sshListen) {
		return fmt.Errorf("refusing to start: -ui-sandbox-listen %q is the same address as -ssh-listen; give each gateway its own address", uiListen)
	}
	if originTemplate != "" && !strings.Contains(originTemplate, "{run}") {
		return fmt.Errorf("refusing to start: WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE %q has no {run} placeholder — "+
			"every run would share one origin while the deployment claims per-run isolation; "+
			"use e.g. \"https://run-{run}.ui.example.com\", or unset it for the documented shared-origin mode", originTemplate)
	}
	return nil
}

// sameListenAddress reports whether two listen addresses land on the same
// browser ORIGIN, which is the question the refusal above actually asks — not
// whether the two binds would collide at the socket layer.
//
// Ports must match. Given that, the hosts are the same origin when they are
// literally equal, when either is the "everything" bind (empty, 0.0.0.0, ::),
// or when either is a LOOPBACK form. That last one is the whole point: a
// one-box daemon is reached as "localhost:8080", and localhost resolves to
// 127.0.0.1 or [::1] depending on what the resolver answers first — so
// `-listen 127.0.0.1:8080 -ui-sandbox-listen [::1]:8080` is two binds that
// both succeed and ONE origin the browser cannot tell apart, which is exactly
// the same-origin collapse this rule exists to prevent.
//
// Only two SPECIFIC, non-loopback hosts on one port are genuinely two origins
// (two NICs, two names), and those still pass.
func sameListenAddress(a, b string) bool {
	ah, ap := listenHost(a), listenPort(a)
	bh, bp := listenHost(b), listenPort(b)
	if ap != bp || ap == "" {
		return false
	}
	if strings.EqualFold(ah, bh) {
		return true
	}
	return listenHostIsUnspecified(ah) || listenHostIsUnspecified(bh) ||
		listenIsLoopback(a) || listenIsLoopback(b)
}

// listenPort is listenHost's twin: the port half of a listen address, or "".
func listenPort(listen string) string {
	if _, port, err := net.SplitHostPort(listen); err == nil {
		return strings.TrimSpace(port)
	}
	return ""
}

// listenHostIsUnspecified reports whether host binds every interface.
func listenHostIsUnspecified(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// validateOperatorPosture is the second boot-time fail-closed rule, kept beside
// validateConfig (and pure, for the same reason) but applied later: OIDC is not
// built until boot_deps.go, well after validateConfig runs at the top of run().
//
// Configuring SSO IS the declaration that more than one human exists, so an
// empty operator allowlist is not a default — it is an ambiguity in which every
// person the IdP lets in silently holds the admin token's power. Refuse, with
// WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST as the explicit override (the
// WARDYN_ALLOW_PLAINTEXT_LISTEN precedent). UNCONDITIONAL — not conditioned on
// the bind address the way the plaintext rule is: a loopback bind bounds who can
// reach the port, not who the IdP authenticates.
//
// No OIDC => nothing to decide: the admin token and local mode are a single
// shared credential with no human identity to key a role off, so they are always
// operators and this rule never fires.
//
// hasRoleMap also satisfies the rule: WARDYN_OIDC_ROLE_MAP switches deriveRole
// (internal/auth/oidc) to claim-based admin/member derivation that no longer
// depends on the operator allowlist at all (an unmatched claim falls through to
// WARDYN_OIDC_DEFAULT_ROLE or is denied) — so a role-map-only deployment, the
// recipe .claude/skills/wardyn-k8s-setup/SKILL.md documents, is not the
// every-human-is-admin ambiguity this refusal exists to catch.
func validateOperatorPosture(oidcConfigured bool, operatorEmails []string, allowNoOperatorList bool, hasRoleMap bool) error {
	if !oidcConfigured || len(operatorEmails) > 0 || allowNoOperatorList || hasRoleMap {
		return nil
	}
	return errors.New("refusing to start: OIDC SSO is configured but the operator allowlist is empty — " +
		"EVERY human the IdP signs in would be admin-equivalent (rewrite policies/workspaces/site-config, connect the shared harness credential, " +
		"write and delete secrets, decide approvals, and open an interactive shell in any running sandbox) — absent a role map — " +
		"set WARDYN_OIDC_OPERATOR_EMAILS to the humans who may do that — everyone else becomes a member who reads their OWN runs and can launch runs — " +
		"or set WARDYN_OIDC_ROLE_MAP for claim-based roles instead, " +
		"or explicitly set WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST=true to override")
}

// knownPublicAgeKeys are age identities this repository has published — each was
// once a committed default, so it lives in git history forever and any secret
// encrypted under one is effectively public. wardynd refuses to start with ANY of
// them (invariant 5, fail closed): unset WARDYN_AGE_KEY to generate an ephemeral
// key, or mint your own with `wardynd -gen-age-key`.
//
// Add an entry here whenever a key is published, never remove one: a key cannot
// be un-published, and the denylist is what keeps a stale copy-pasted .env from
// silently encrypting a real secret store under a key anyone can read.
var knownPublicAgeKeys = []string{
	// Baked-in default of earlier Compose files (deploy/compose).
	"AGE-SECRET-KEY-1YGHJK4A24GHQGAL2U2ZU7M05080VNWSZ0EU9KRM3DVYKDN0XYSTS3TK3YR",
	// Committed default of scripts/e2e-backend.sh (and a since-removed
	// run-local.sh). That script now mints an ephemeral per-boot key via
	// `wardynd -gen-age-key`, so nothing legitimate uses this one.
	"AGE-SECRET-KEY-1CMRQ5GEN2G4NKWXQQ4DKK7GSMJDZXXW69W9QN3ALX8Y49CF6RLYS7Y6KHF",
}

// isKnownPublicAgeKey reports whether ageKey is one of the published identities.
func isKnownPublicAgeKey(ageKey string) bool {
	return slices.Contains(knownPublicAgeKeys, strings.TrimSpace(ageKey))
}

// parseConfinementMap parses WARDYN_CONFINEMENT_MAP — a ";"-separated list of
// CLASS=runtime (or CLASS=substrate:runtime) pins selecting which substrate
// runtime backs each Confinement Class. It is the operator knob that makes CC3
// runtime-pluggable (e.g. "CC3=kata-qemu" to pin QEMU Kata, "CC2=runsc"). Empty
// => nil (the driver's built-in default mapping). FAIL CLOSED: an unknown class,
// malformed entry, empty runtime, or a non-"oci" substrate is a startup error,
// so a typo can never silently downgrade isolation.
func parseConfinementMap(s string) (map[types.ConfinementClass]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	out := map[types.ConfinementClass]string{}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" {
			return nil, fmt.Errorf("WARDYN_CONFINEMENT_MAP: malformed entry %q (want CLASS=runtime)", part)
		}
		class := types.ConfinementClass(strings.TrimSpace(kv[0]))
		val := strings.TrimSpace(kv[1])
		// Optional "substrate:runtime"; only the OCI substrate exists today.
		if i := strings.Index(val, ":"); i >= 0 {
			if sub := strings.TrimSpace(val[:i]); sub != "" && sub != "oci" {
				return nil, fmt.Errorf("WARDYN_CONFINEMENT_MAP: substrate %q for %s is not supported (only %q today; non-OCI VMM substrates are a future runner driver)", sub, class, "oci")
			}
			val = strings.TrimSpace(val[i+1:])
		}
		switch class {
		case types.CC1, types.CC2, types.CC3:
		default:
			return nil, fmt.Errorf("WARDYN_CONFINEMENT_MAP: unknown confinement class %q (want CC1|CC2|CC3)", class)
		}
		if val == "" {
			return nil, fmt.Errorf("WARDYN_CONFINEMENT_MAP: empty runtime for %s", class)
		}
		out[class] = val
	}
	return out, nil
}

// genAndPrintAgeKey writes a freshly-generated age X25519 identity
// (AGE-SECRET-KEY-...) to w. It is the body of the -gen-age-key early-exit flag,
// extracted so it is unit-testable without standing up the daemon. The printed
// key is what an operator sets as WARDYN_AGE_KEY to make the secret store durable
// (buildSecretStore parses it via age.ParseX25519Identity).
func genAndPrintAgeKey(w io.Writer) error {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return fmt.Errorf("generate age identity: %w", err)
	}
	_, err = fmt.Fprintln(w, id.String())
	return err
}

// buildSecretStore constructs the age-encrypted Postgres secret store. The age
// identity comes from -age-key; if empty one is generated and logged (operators
// MUST persist it across restarts to keep prior ciphertext readable).
func buildSecretStore(pool *pgxpool.Pool, ageKey, storeName string) (secretstore.Store, error) {
	var id *age.X25519Identity
	var err error
	if ageKey == "" {
		id, err = age.GenerateX25519Identity()
		if err != nil {
			return nil, fmt.Errorf("generate age identity: %w", err)
		}
		// F10: log the PUBLIC recipient as a fingerprint, never the secret identity.
		// The old message printed the full AGE-SECRET-KEY- to a log file created at
		// the default umask (~/.wardyn/host-wardynd.log), leaking the secret-store
		// master key. To persist, mint one with `wardynd -gen-age-key` (prints to
		// stdout by design) and set WARDYN_AGE_KEY — do not copy it out of this log.
		slog.Warn("wardynd: generated ephemeral age identity; secrets are LOST on restart. Persist one with `wardynd -gen-age-key` + set WARDYN_AGE_KEY",
			slog.String("public_recipient", id.Recipient().String()),
		)
	} else {
		if isKnownPublicAgeKey(ageKey) {
			return nil, fmt.Errorf("refusing to start: WARDYN_AGE_KEY is a publicly-known key (published in this repo's git history) — secrets encrypted under it are not protected; unset WARDYN_AGE_KEY to generate an ephemeral key, or mint your own with `wardynd -gen-age-key`")
		}
		id, err = age.ParseX25519Identity(ageKey)
		if err != nil {
			return nil, fmt.Errorf("parse age identity: %w", err)
		}
	}
	s, err := secretstore.New(storeName, secretstore.Deps{Pool: pool, AgeIdentity: id})
	if err != nil {
		return nil, fmt.Errorf("secret store: %w", err)
	}
	return s, nil
}

// secretKeyStore is the minimal secret-store surface loadOrCreateSecret needs.
// Narrowing the dependency to Get/Put makes the load-or-create control flow
// unit-testable with a hand-rolled fake (cmd/wardynd/main_test.go) and documents
// that key bootstrap touches nothing else. secretstore.Store satisfies it.
type secretKeyStore interface {
	Get(ctx context.Context, name string) ([]byte, error)
	Put(ctx context.Context, name string, value []byte) error
}

// loadOrCreateSecret is the shared, fail-closed bootstrap for the two boot keys
// (the embedded-identity signing key and the OIDC session key).
//
// SECURITY (boot-key destruction): the previous per-key logic treated ANY
// Get error as "key not present" and then generated + Put a fresh key,
// OVERWRITING whatever ciphertext was already there. The pg secret store
// distinguishes a TRUE not-found (it wraps pgx.ErrNoRows) from an age-decrypt
// failure (a generic error). Conflating the two meant a single transient/
// permanent decrypt error silently rotated the key, invalidating every issued
// SVID and every active session cookie. We now regenerate ONLY when the key is
// genuinely absent or present-but-invalid; on any other error we FAIL CLOSED —
// return the error and never Put, so the existing ciphertext is preserved.
//
//   - valid reports whether an existing raw value is usable as-is.
//   - generate produces fresh key material to persist (called only when the key
//     is absent or invalid).
func loadOrCreateSecret(
	ctx context.Context,
	secrets secretKeyStore,
	name string,
	valid func(raw []byte) bool,
	generate func() ([]byte, error),
) ([]byte, error) {
	raw, err := secrets.Get(ctx, name)
	switch {
	case err == nil:
		if valid(raw) {
			return raw, nil
		}
		// Present but unusable (e.g. a legacy too-short session key): fall
		// through to regenerate. This is safe — the stored value cannot serve
		// its purpose anyway.
	case errors.Is(err, pgx.ErrNoRows):
		// TRUE not-found (first boot): generate + persist below.
	default:
		// Decrypt failure or any other Get error: FAIL CLOSED. Do NOT generate
		// or Put — overwriting here would destroy the existing key.
		return nil, fmt.Errorf("load secret %q: %w", name, err)
	}

	val, gerr := generate()
	if gerr != nil {
		return nil, fmt.Errorf("generate secret %q: %w", name, gerr)
	}
	if perr := secrets.Put(ctx, name, val); perr != nil {
		return nil, fmt.Errorf("persist secret %q: %w", name, perr)
	}
	return val, nil
}

// loadOrCreateSigningKey returns the embedded identity ES256 key, persisting a
// freshly-generated one into the secret store on first boot. The key never
// enters a sandbox; it lives only in the broker/control-plane process memory
// and the encrypted secret column. A decrypt error fails closed (see
// loadOrCreateSecret) rather than minting a fresh key over the old one.
func loadOrCreateSigningKey(ctx context.Context, secrets secretKeyStore) (*ecdsa.PrivateKey, error) {
	raw, err := loadOrCreateSecret(ctx, secrets, secretSigningKey,
		func(b []byte) bool { return len(b) > 0 },
		func() ([]byte, error) {
			key, gerr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if gerr != nil {
				return nil, fmt.Errorf("generate signing key: %w", gerr)
			}
			pemBytes, merr := marshalECPrivateKeyPEM(key)
			if merr != nil {
				return nil, merr
			}
			slog.Info("wardynd: generated and persisted embedded identity signing key")
			return pemBytes, nil
		},
	)
	if err != nil {
		return nil, err
	}
	key, perr := parseECPrivateKeyPEM(raw)
	if perr != nil {
		return nil, fmt.Errorf("parse stored signing key: %w", perr)
	}
	return key, nil
}

// loadOrCreateSessionKey returns the 32-byte OIDC session-cookie HMAC key,
// persisting a freshly-generated one into the secret store on first boot. Like
// the signing key it never enters a sandbox; it lives only in process memory
// and the encrypted secret column. Returning the key is safe — the caller is
// the OIDC authenticator, which never logs it. A decrypt error fails closed
// (see loadOrCreateSecret) rather than rotating every session out from under
// logged-in users.
func loadOrCreateSessionKey(ctx context.Context, secrets secretKeyStore) ([]byte, error) {
	return loadOrCreateSecret(ctx, secrets, secretSessionKey,
		func(b []byte) bool { return len(b) >= 32 },
		func() ([]byte, error) {
			key := make([]byte, 32)
			if _, gerr := rand.Read(key); gerr != nil {
				return nil, fmt.Errorf("generate session key: %w", gerr)
			}
			slog.Info("wardynd: generated and persisted OIDC session key")
			return key, nil
		},
	)
}

// loadOrCreateUISessionKey returns the UI-sandbox gateway's relay-cookie HMAC
// key, persisted in the secret store and generated on first boot — the same
// loadOrCreateSecret pattern as the signing/session/SSH-host keys. It is
// SEPARATE from the OIDC session key on purpose: the two cookies live on
// different origins and authorize different things, so one key must never be
// able to forge the other's cookie.
func loadOrCreateUISessionKey(ctx context.Context, secrets secretKeyStore) ([]byte, error) {
	return loadOrCreateSecret(ctx, secrets, secretUISessionKey,
		func(b []byte) bool { return len(b) >= 32 },
		func() ([]byte, error) {
			key := make([]byte, 32)
			if _, gerr := rand.Read(key); gerr != nil {
				return nil, fmt.Errorf("generate ui session key: %w", gerr)
			}
			slog.Info("wardynd: generated and persisted UI-sandbox relay cookie key")
			return key, nil
		},
	)
}

// loadOrCreateSSHHostKey returns the SSH gateway's ed25519 host key,
// persisting a freshly-generated one into the secret store on first boot —
// the same loadOrCreateSecret pattern as the signing/session keys above,
// cloned for the one new field this key needs (ed25519 has no "is this a
// valid key of the right size" shortcut as cheap as the session key's length
// check, so validity is "does it parse", checked by the generate/persist
// round-trip itself; a corrupt stored value fails the parse below and
// loadOrCreateSecret's caller sees that as a startup error, never a silent
// re-mint over a key clients have already pinned).
func loadOrCreateSSHHostKey(ctx context.Context, secrets secretKeyStore) (ed25519.PrivateKey, error) {
	raw, err := loadOrCreateSecret(ctx, secrets, secretSSHHostKey,
		func(b []byte) bool { return len(b) > 0 },
		func() ([]byte, error) {
			_, priv, gerr := ed25519.GenerateKey(rand.Reader)
			if gerr != nil {
				return nil, fmt.Errorf("generate ssh host key: %w", gerr)
			}
			pemBytes, merr := marshalEd25519PrivateKeyPEM(priv)
			if merr != nil {
				return nil, merr
			}
			slog.Info("wardynd: generated and persisted ssh gateway host key")
			return pemBytes, nil
		},
	)
	if err != nil {
		return nil, err
	}
	key, perr := parseEd25519PrivateKeyPEM(raw)
	if perr != nil {
		return nil, fmt.Errorf("parse stored ssh host key: %w", perr)
	}
	return key, nil
}

// goSafe runs fn with panic recovery so a panic in a DETACHED background
// goroutine (reaper, approval sweeper, completion watcher) logs and is contained
// instead of crashing the whole control plane — which would take every governed
// run and the kill-switch down with it. Use as `go goSafe("name", func(){ ... })`.
func goSafe(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("wardynd: PANIC in background goroutine (contained)",
				slog.String("goroutine", name),
				slog.Any("panic", r),
			)
		}
	}()
	fn()
}

// defaultLocalOperator is the local-host-mode operator principal: "local:<os-user>",
// falling back to "local:operator" when the OS user is unavailable.
func defaultLocalOperator() string {
	if u, err := user.Current(); err == nil {
		if name := strings.TrimSpace(u.Username); name != "" {
			return "local:" + name
		}
	}
	return "local:operator"
}

// listenHost extracts the host portion of a listen address, tolerating a bare
// host, a bare ":port", or "host:port".
func listenHost(listen string) string {
	if host, _, err := net.SplitHostPort(listen); err == nil {
		return strings.TrimSpace(host)
	}
	return strings.TrimSpace(listen)
}

// listenIsLoopback reports whether the listen address binds ONLY the loopback
// interface (127.0.0.0/8, ::1, or host "localhost"). An empty host (":8080") or
// 0.0.0.0/[::] binds all interfaces and is NOT loopback.
func listenIsLoopback(listen string) bool {
	host := listenHost(listen)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// listenIsRoutablePublic reports whether the listen address binds a SPECIFIC,
// globally-routable public IP (not loopback, not private/RFC1918, not link-local,
// and not the unspecified all-interfaces bind). It is the fail-closed gate for
// LocalMode: a no-auth public API must never be served on a public IP. The
// unspecified bind (":8080"/0.0.0.0) is treated as non-public here — it MIGHT
// include a public IP, so it earns a loud warning rather than a refusal (refusing
// it would block the common docker-bridge/compose single-host case).
func listenIsRoutablePublic(listen string) bool {
	host := listenHost(listen)
	if host == "" || strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // a hostname we can't classify — don't refuse
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	return ip.IsGlobalUnicast()
}

// listenBindsSpecificRoutable reports whether the listen address binds a
// SPECIFIC non-loopback interface — a private/RFC1918, link-local, or public IP
// a LAN peer can reach directly. It EXCLUDES loopback (peers are already local)
// and the unspecified all-interfaces bind (0.0.0.0/[::]), which from inside a
// container is indistinguishable from the safe compose 127.0.0.1-publish
// topology. It is the fail-closed gate for -local-trust-forwarder, which
// disables the loopback-PEER check and is therefore safe ONLY on a loopback or
// unspecified/compose bind. Unlike listenIsRoutablePublic this DELIBERATELY
// catches private and link-local too: with the peer gate disabled, those are
// LAN-reachable no-auth surfaces as well.
func listenBindsSpecificRoutable(listen string) bool {
	host := listenHost(listen)
	if host == "" || strings.EqualFold(host, "localhost") {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // a hostname we can't classify — don't refuse
	}
	return !ip.IsLoopback() && !ip.IsUnspecified()
}

func marshalECPrivateKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal ec key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func parseECPrivateKeyPEM(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block in signing key")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// marshalEd25519PrivateKeyPEM / parseEd25519PrivateKeyPEM mirror
// marshalECPrivateKeyPEM / parseECPrivateKeyPEM above for the SSH gateway's
// host key. ed25519 has no dedicated x509.MarshalECPrivateKey-style helper —
// PKCS8 is the standard-library encoding for it (x509.ParsePKCS8PrivateKey
// returns `any`; the type assertion below is the "is this really an ed25519
// key" check).
func marshalEd25519PrivateKeyPEM(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal ed25519 key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func parseEd25519PrivateKeyPEM(pemBytes []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block in ssh host key")
	}
	raw, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := raw.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("stored ssh host key is a %T, not ed25519", raw)
	}
	return key, nil
}

// buildGitHubMinter arms the LAZY GitHub minter: it reads the App credentials
// (github-app-id / github-app-key) on the FIRST mint, not here, so adding those
// secrets after boot no longer needs a wardynd restart before github_token
// grants can mint. A github_token grant that reaches mint with the secrets still
// absent fails closed with a clear error. Construction only validates the secret
// NAMES; an error there is logged, not fatal.
func buildGitHubMinter(secrets secretstore.Store) broker.GitHubMinter {
	gh, err := broker.NewGitHubMinter(secrets, broker.GitHubMinterConfig{
		AppIDSecret:      secretGitHubAppID,
		PrivateKeySecret: secretGitHubAppKey,
	})
	if err != nil {
		slog.Warn("wardynd: github minter unavailable (github_token grants will fail closed)", slog.Any("err", err))
		return nil
	}
	slog.Info("wardynd: lazy github minter armed; App credentials are read on first mint (no restart needed after adding them)",
		slog.String("app_id_secret", secretGitHubAppID),
		slog.String("app_key_secret", secretGitHubAppKey),
	)
	return gh
}

// flagEnv/flagBool/flagDuration/flagIntEnv/splitCSV are shared with
// cmd/wardyn-tetragon-ingest via internal/cliutil (mirrored duplicates there
// previously).
var (
	flagEnv      = cliutil.FlagEnv
	envOr        = cliutil.EnvOr
	flagBool     = cliutil.FlagBool
	flagDuration = cliutil.FlagDuration
	flagIntEnv   = cliutil.FlagIntEnv
	splitCSV     = cliutil.SplitCSV
)
