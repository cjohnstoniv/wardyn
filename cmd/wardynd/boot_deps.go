// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// connectAndMigrate connects the runtime app pool and applies migrations.
//
// N4 (DDL protection via role separation): when migrateDSN (WARDYN_PG_MIGRATE_DSN)
// is set, run migrations through a SEPARATE owner/migrator pool and keep the main
// app pool on the least-privilege dsn (WARDYN_PG_DSN) role — so a compromised app
// role cannot DROP/DISABLE the audit_events append-only triggers. When unset,
// behavior is EXACTLY single-DSN mode, with an honest notice that audit_events is
// not DDL-protected without the role split. Extracted verbatim from run().
func connectAndMigrate(bootCtx context.Context, dsn, migrateDSN string) (*pgxpool.Pool, error) {
	pool, err := db.Connect(bootCtx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect db: %w", err)
	}
	if mDSN := strings.TrimSpace(migrateDSN); mDSN != "" {
		mpool, merr := db.Connect(bootCtx, mDSN)
		if merr != nil {
			pool.Close()
			return nil, fmt.Errorf("connect migrate db: %w", merr)
		}
		merr = db.Migrate(bootCtx, mpool)
		mpool.Close()
		if merr != nil {
			pool.Close()
			return nil, fmt.Errorf("migrate: %w", merr)
		}
		// Do NOT assume the split delivered protection: VERIFY the app role
		// (WARDYN_PG_DSN) is actually a non-owner, non-superuser of audit_events.
		// An operator who pointed WARDYN_PG_MIGRATE_DSN at the same (or another
		// owner/superuser) role gets no protection — logging "protected"
		// unconditionally would be an overclaim (invariant 5).
		protected, perr := db.AuditDDLProtected(bootCtx, pool)
		if perr != nil {
			pool.Close()
			return nil, fmt.Errorf("verify audit ddl protection: %w", perr)
		}
		if protected {
			slog.InfoContext(bootCtx, "wardynd: migrations applied via WARDYN_PG_MIGRATE_DSN (owner/migrator role); app role is a verified non-owner of audit_events — the append-only guard is DDL-protected")
		} else {
			slog.WarnContext(bootCtx, "wardynd: WARDYN_PG_MIGRATE_DSN is set but the app role (WARDYN_PG_DSN) still owns audit_events or is a superuser — DDL protection is NOT in effect; connect wardynd as a distinct non-owner role that has only INSERT/SELECT on audit_events")
		}
		return pool, nil
	}
	if err := db.Migrate(bootCtx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	slog.InfoContext(bootCtx, "wardynd: NOTICE single-DSN mode — wardynd's DB role owns audit_events, so DROP TRIGGER / ALTER TABLE ... DISABLE TRIGGER / DROP TABLE bypass the append-only guard. Set WARDYN_PG_MIGRATE_DSN to a separate owner/migrator role (wardynd then connects as a non-owner app role) for DDL protection.")
	return pool, nil
}

// buildAuditChain assembles the audit recorder chain:
// maskingRecorder → spoolingRecorder → (fanoutRecorder →) store.Recorder.
//
// The Postgres store is the source of truth. When audit sinks are configured,
// every persisted event ALSO fans out to file/webhook/syslog; the store write is
// authoritative and fanout failures never fail the primary record (invariant 6).
// Masking is outermost so the spool, the store, and the SIEM sinks all receive
// the already-masked event (the H9 fix). The durable spool (C1/H9) sits below
// masking and is shared by EVERY audit writer — API, broker, identity,
// approvals, sweeper. Best-effort: a spool that cannot be opened degrades to
// log-only, never blocking startup. Extracted verbatim from run(); the returned
// *sinks.Fanout (nil when unconfigured) must be Closed on shutdown.
func buildAuditChain(rootCtx context.Context, sinksJSON, spoolPath string, pool *pgxpool.Pool, maskReg *secretmask.Registry) (audit.Recorder, *sinks.Fanout, error) {
	var auditRec audit.Recorder = store.Recorder{Pool: pool}
	fan, ferr := buildAuditFanout(rootCtx, sinksJSON)
	if ferr != nil {
		return nil, nil, ferr
	}
	if fan != nil {
		auditRec = fanoutRecorder{primary: store.Recorder{Pool: pool}, fanout: fan}
		slog.Info("wardynd: audit fanout enabled")
	}
	var auditFallback *api.AuditSpool
	if strings.TrimSpace(spoolPath) != "" {
		af, aerr := api.NewAuditSpool(spoolPath)
		if aerr != nil {
			slog.Warn("wardynd: audit spool unavailable (failed audit writes will be logged only)",
				slog.String("path", spoolPath),
				slog.Any("err", aerr),
			)
		} else {
			auditFallback = af
			slog.Info("wardynd: audit fallback spool", slog.String("path", spoolPath))
		}
	}
	masked := maskingRecorder{inner: spoolingRecorder{inner: auditRec, spool: auditFallback}, reg: maskReg}
	return masked, fan, nil
}

// buildRunnerFromFlags resolves the optional sandbox runner: docker | none. The
// docker driver is compiled in only under the "docker" build tag (parity rule:
// the control plane carries zero target-specific code by default). Confinement
// substrate/runtime pins (pluggable CC3) are parsed fail-closed so a typo never
// silently downgrades isolation. Returns the runner (nil for "none") and the
// resolved runner target advertised on /healthz — M31: reflect the ACTUAL
// resolved runner, not a hardcoded "docker". Extracted verbatim from run().
func buildRunnerFromFlags(f *bootFlags) (runner.Runner, string, error) {
	confRuntimes, err := parseConfinementMap(*f.confinementMap)
	if err != nil {
		return nil, "", err
	}
	switch *f.runnerSel {
	case "docker":
		d, derr := newDockerRunner(*f.proxyImage, confRuntimes)
		if derr != nil {
			return nil, "", fmt.Errorf("docker runner: %w", derr)
		}
		slog.Info("wardynd: docker runner enabled", slog.String("proxy_image", *f.proxyImage))
		return d, "docker", nil
	case "none", "":
		slog.Info("wardynd: no runner selected; runs stay PENDING (headless API-only)")
		return nil, "none", nil
	default:
		return nil, "", fmt.Errorf("unknown -runner %q (want docker|none)", *f.runnerSel)
	}
}

// optionalFeatures groups the off-by-default subsystems run() wires into
// api.Config: recording replay, human SSO, devcontainer builds, the AI Run
// Composer, the subscription/managed LLM credential providers, and the advisory
// AI scan fallback. Each is nil/zero when unconfigured (fail closed / feature
// off), exactly as before the extraction.
type optionalFeatures struct {
	recStore         recording.Store
	authn            *oidc.Authenticator
	imgBuilder       api.ImageBuilder
	composerReg      *composer.Registry
	composerBackends []api.ComposerBackendReadiness
	subToken         subscription.Provider
	disableSubInject bool
	managedToken     subscription.Provider
	scanAdvisor      func(context.Context, workspacescan.ScanFacts, workspacescan.WorkspaceProfile) workspacescan.WorkspaceProfile
}

// buildOptionalFeatures wires every optional subsystem from its flags. Extracted
// verbatim from run() — construction order and log lines are unchanged.
func buildOptionalFeatures(rootCtx, bootCtx context.Context, f *bootFlags, secrets secretstore.Store, secureCookies bool) (optionalFeatures, error) {
	var of optionalFeatures

	// Recording store (pluggable seam; default "fs"). The fs store serves replays
	// and accepts wardyn-rec uploads. Empty -recording-dir => a nil store (replay
	// disabled), the same as before.
	recStore, rerr := recording.New(*f.recordingSel, recording.Deps{Dir: *f.recordingDir})
	if rerr != nil {
		return of, fmt.Errorf("recording store: %w", rerr)
	}
	of.recStore = recStore
	if recStore != nil {
		slog.Info("wardynd: recording store",
			slog.String("store", *f.recordingSel),
			slog.String("dir", *f.recordingDir),
		)
	}

	// Human SSO (OIDC), optional. The session-cookie HMAC key is loaded from the
	// secret store ("wardyn-session-key"), generated and persisted on first boot.
	if *f.oidcIssuer != "" {
		sessKey, kerr := loadOrCreateSessionKey(bootCtx, secrets)
		if kerr != nil {
			return of, kerr
		}
		authn, err := oidc.New(rootCtx, oidc.Config{
			IssuerURL:           *f.oidcIssuer,
			InternalIssuerURL:   *f.oidcInternalIss,
			ClientID:            *f.oidcClientID,
			ClientSecret:        *f.oidcClientSecret,
			RedirectURL:         *f.oidcRedirectURL,
			AllowedEmailDomains: splitCSV(*f.oidcEmailDomains),
			SecureCookies:       secureCookies,
		}, sessKey)
		if err != nil {
			return of, fmt.Errorf("oidc: %w", err)
		}
		of.authn = authn
		slog.Info("wardynd: OIDC SSO enabled", slog.String("issuer", *f.oidcIssuer))
		slog.Info("wardynd: NOTE human SSO / team mode is EXPERIMENTAL — a first-class team deployment is coming soon; the UI's 'Sign in with SSO' button is disabled, so use the admin token or the CLI for now")
	}

	// Devcontainer image builder (optional; docker build tag only). When -envbuild
	// is set but wardynd was not built with -tags docker, newEnvBuilder returns an
	// error so the misconfiguration fails closed at boot rather than silently.
	if *f.envbuild {
		b, berr := newEnvBuilder(*f.envbuildImg, *f.envbuildRepo)
		if berr != nil {
			return of, fmt.Errorf("envbuild: %w", berr)
		}
		of.imgBuilder = b
		slog.Info("wardynd: devcontainer builds enabled")
	}

	// AI Run Composer (optional): build the backend registry from -composer-config.
	// Nil when unconfigured, which disables the compose endpoints (fail closed).
	composerReg, composerReadiness, err := buildComposerRegistry(*f.composerCfg, secrets)
	if err != nil {
		return of, fmt.Errorf("composer: %w", err)
	}
	of.composerReg = composerReg
	// Map the boot-snapshot readiness onto the api wire type for /setup/status.
	// backends.BackendReadiness and api.ComposerBackendReadiness have identical
	// fields/types/order, so this is a plain Go struct conversion (tags ignored).
	for _, b := range composerReadiness {
		of.composerBackends = append(of.composerBackends, api.ComposerBackendReadiness(b))
	}
	if composerReg != nil && composerReg.Enabled() {
		names := make([]string, 0)
		for _, b := range composerReg.List() {
			names = append(names, b.Name)
		}
		slog.Info("wardynd: AI Run Composer enabled",
			slog.Any("backends", names),
			slog.String("default", composerReg.Default()),
		)
	}

	// Subscription OAuth token provider: yields the operator's LIVE Anthropic
	// access token from the resident ~/.claude so subscription runs are
	// credentialed PROXY-SIDE (the sandbox holds an inert sentinel that never
	// goes stale) instead of a copy whose refresh token rotates out from under it.
	// Constructed unconditionally; it only reads/refreshes when a subscription run
	// resolves its injection. Escape hatch: WARDYN_SUBSCRIPTION_INJECT=off keeps
	// the legacy resident-copy behavior.
	subToken, subErr := subscription.New(subscription.Config{})
	if subErr != nil {
		slog.Warn("wardynd: subscription token provider unavailable; subscription runs fall back to the resident-copy behavior",
			slog.Any("err", subErr),
		)
		subToken = nil
	}
	of.subToken = subToken
	// Default ON: unset (and the compose ${…:-off} passthrough when actually set
	// to a truthy) injects proxy-side. off/0/false/no disable it; garbage exits 2
	// via EnvBool rather than silently staying ON. (Previously only the literal
	// "off" disabled; 0/false/no silently left injection ON — the security gap.)
	of.disableSubInject = !cliutil.EnvBool("WARDYN_SUBSCRIPTION_INJECT", true)

	// Managed subscription token: a long-lived `claude setup-token` captured via
	// the container-login flow and stored age-encrypted. Serves subscription runs
	// PROXY-SIDE in deployments (compose) whose distroless wardynd has no host
	// ~/.claude for subToken above. Store-only (no Server dependency, no cycle);
	// nil when there is no secret store.
	of.managedToken = api.NewManagedCredProvider(secrets, "anthropic")

	// Advisory AI scan fallback (opt-in): wired to the fail-open
	// workspacescan.AdviseProfile with a bounded timeout so a slow/hung CLI can
	// never stall — let alone fail — the sidecar's scan upload. nil = OFF.
	if *f.scanAIAdvisor {
		of.scanAdvisor = func(ctx context.Context, facts workspacescan.ScanFacts, base workspacescan.WorkspaceProfile) workspacescan.WorkspaceProfile {
			return workspacescan.AdviseProfile(ctx, facts, base, workspacescan.AIOptions{Timeout: 60 * time.Second})
		}
		slog.Info("wardynd: advisory AI workspace-scan fallback ENABLED (WARDYN_SCAN_AI_ADVISOR); advisory-only + fail-open, needs a resident read-only claude CLI on PATH")
	}

	return of, nil
}

// componentsInfo builds the pluggable-component selection advertised on
// /healthz. "selected" is the ACTUAL running impl; "recommended_production" is
// the standard Wardyn recommends converging to (may differ from the shipped
// default — the honest recommended-vs-shipped split, see docs/PLUGGABILITY.md).
// Extracted verbatim from run().
func componentsInfo(f *bootFlags, runnerTarget string) map[string]api.ComponentInfo {
	sourceOf := func(selected, def string) string {
		if selected == def {
			return "default"
		}
		return "configured"
	}
	return map[string]api.ComponentInfo{
		"identity":      {Selected: *f.identitySel, RecommendedProduction: "spire", Source: sourceOf(*f.identitySel, "embedded")},
		"secret_store":  {Selected: *f.secretStoreSel, RecommendedProduction: "openbao", Source: sourceOf(*f.secretStoreSel, "pg")},
		"recording":     {Selected: *f.recordingSel, RecommendedProduction: "fs", Source: sourceOf(*f.recordingSel, "fs")},
		"policy_engine": {Selected: "builtin", RecommendedProduction: "opa"},
		"sandbox":       {Selected: runnerTarget, RecommendedProduction: "kata-cc3", Source: sourceOf(*f.runnerSel, "none")},
	}
}
