// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/directory"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
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
//
// W28-S1-4: connect and migrate get SEPARATE budgets, not one shared deadline.
// connectTimeout bounds the (fast) pool-open + ping calls; migrateTimeout bounds
// db.Migrate alone, on its own context derived from rootCtx — a slow migration
// (a new index on the unbounded audit_events table, say) gets minutes to finish
// rather than crash-looping the upgrade against the same budget a TCP connect
// needs seconds for. rootCtx is the signal-bound context (main's SIGINT/SIGTERM),
// not the 30s-bounded one — WithTimeout takes the EARLIEST of parent and its own
// deadline, so deriving migrateCtx from an already-30s-bounded parent would have
// silently kept the old cap.
func connectAndMigrate(rootCtx context.Context, dsn, migrateDSN string, connectTimeout, migrateTimeout time.Duration) (*pgxpool.Pool, error) {
	connectCtx, cancelConnect := context.WithTimeout(rootCtx, connectTimeout)
	defer cancelConnect()
	pool, err := db.Connect(connectCtx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect db: %w", err)
	}
	migrateCtx, cancelMigrate := context.WithTimeout(rootCtx, migrateTimeout)
	defer cancelMigrate()
	if mDSN := strings.TrimSpace(migrateDSN); mDSN != "" {
		mpool, merr := db.Connect(connectCtx, mDSN)
		if merr != nil {
			pool.Close()
			return nil, fmt.Errorf("connect migrate db: %w", merr)
		}
		merr = db.Migrate(migrateCtx, mpool)
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
		protected, perr := db.AuditDDLProtected(connectCtx, pool)
		if perr != nil {
			pool.Close()
			return nil, fmt.Errorf("verify audit ddl protection: %w", perr)
		}
		if protected {
			slog.InfoContext(rootCtx, "wardynd: migrations applied via WARDYN_PG_MIGRATE_DSN (owner/migrator role); app role is a verified non-owner of audit_events — the append-only guard is DDL-protected")
		} else {
			slog.WarnContext(rootCtx, "wardynd: WARDYN_PG_MIGRATE_DSN is set but the app role (WARDYN_PG_DSN) still owns audit_events or is a superuser — DDL protection is NOT in effect; connect wardynd as a distinct non-owner role that has only INSERT/SELECT on audit_events")
		}
		return pool, nil
	}
	if err := db.Migrate(migrateCtx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	slog.InfoContext(rootCtx, "wardynd: NOTICE single-DSN mode — wardynd's DB role owns audit_events, so DROP TRIGGER / ALTER TABLE ... DISABLE TRIGGER / DROP TABLE bypass the append-only guard. Set WARDYN_PG_MIGRATE_DSN to a separate owner/migrator role (wardynd then connects as a non-owner app role) for DDL protection.")
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
//
// It also returns the *api.AuditSpool and the RAW store.Recorder so the API
// server can start the background drain that replays spooled events back into the
// store once it recovers. The drain MUST target the raw store recorder —
// NOT the returned masking/spooling chain — or a replay that hit a still-down
// store would re-spool (and re-enter the spool lock) instead of retrying later.
func buildAuditChain(rootCtx context.Context, sinksJSON, spoolPath, source string, pool *pgxpool.Pool, maskReg *secretmask.Registry) (audit.Recorder, *sinks.Fanout, *api.AuditSpool, audit.Recorder, error) {
	// #10 WARDYN_AUDIT_SOURCE: set once, before any sink is constructed/starts
	// emitting — see sinks.Source's doc comment. A no-op (empty) is
	// byte-identical to before this field existed.
	sinks.Source = strings.TrimSpace(source)
	storeRec := store.Recorder{Pool: pool}
	var auditRec audit.Recorder = storeRec
	fan, ferr := buildAuditFanout(rootCtx, sinksJSON)
	if ferr != nil {
		return nil, nil, nil, nil, ferr
	}
	if fan != nil {
		auditRec = fanoutRecorder{primary: storeRec, fanout: fan}
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
	return masked, fan, auditFallback, storeRec, nil
}

// buildRunnerFromFlags resolves the optional sandbox runner: "none" (nil runner,
// headless API-only) or any substrate registered in the substrate registry
// (internal/runner/substrate). Substrates SELF-REGISTER at init() like every
// other pluggable seam: the OCI/Docker substrate registers only under the
// "docker" build tag (parity rule: the control plane carries zero
// target-specific code by default), so a tagless `-runner docker` fails closed
// at resolve with a "not registered" error, and adding a substrate needs no
// edit here. Confinement substrate/runtime pins (pluggable CC3) are parsed
// fail-closed so a typo never silently downgrades isolation. Returns the
// orchestrator-wrapped runner (nil for "none") and the resolved runner target
// advertised on /healthz — reflect the ACTUAL resolved substrate, not a
// hardcoded "docker".
//
// refs is the durable ref->substrate RefStore (store.PG in production) wired
// into the orchestrator so lifecycle routing — and therefore the kill switch —
// survives a control-plane restart; nil keeps the in-memory-only behavior.
func buildRunnerFromFlags(f *bootFlags, refs orchestrator.RefStore) (runner.Runner, string, error) {
	confRuntimes, err := parseConfinementMap(*f.confinementMap)
	if err != nil {
		return nil, "", err
	}
	if sel := *f.runnerSel; sel == "none" || sel == "" {
		slog.Info("wardynd: no runner selected; runs stay PENDING (headless API-only)")
		return nil, "none", nil
	}
	sub, err := substrate.New(*f.runnerSel, substrate.Deps{
		ProxyImage:          *f.proxyImage,
		ConfinementRuntimes: confRuntimes,
	})
	if err != nil {
		// W27-S1-3: discriminate WHY substrate.New failed before printing the
		// same headline for both. A typo'd -runner or a substrate not compiled
		// into this build (e.g. "docker" without -tags docker) never reaches the
		// registry at all — Resolve's error is the right one for that. But a
		// REGISTERED substrate (e.g. "k8s") can still fail to CONSTRUCT — the
		// canary's flagship refuse-to-construct chief among them — and that
		// failure has nothing to do with -tags docker; printing the build-tag
		// headline over it sent every k8s boot refusal down the wrong
		// troubleshooting path.
		if !slices.Contains(substrate.Names(), *f.runnerSel) {
			return nil, "", fmt.Errorf("unknown -runner %q (want \"none\" or a registered substrate; the docker substrate requires a wardynd built with -tags docker): %w", *f.runnerSel, err)
		}
		return nil, "", fmt.Errorf("-runner %q failed to start: %w", *f.runnerSel, err)
	}
	slog.Info("wardynd: runner enabled", slog.String("substrate", sub.Name()), slog.String("proxy_image", *f.proxyImage))
	return orchestrator.New(sub).WithRefStore(refs), sub.Name(), nil
}

// optionalFeatures groups the off-by-default subsystems run() wires into
// api.Config: recording replay, human SSO, devcontainer builds, the
// subscription/managed LLM credential providers, and the advisory AI scan
// fallback. Each is nil/zero when unconfigured (fail closed / feature off),
// exactly as before the extraction.
type optionalFeatures struct {
	recStore         recording.Store
	authn            *oidc.Authenticator
	imgBuilder       api.ImageBuilder
	subToken         subscription.Provider
	disableSubInject bool
	managedToken     subscription.Provider
	scanAdvisor      func(context.Context, workspacescan.ScanFacts, workspacescan.WorkspaceProfile) workspacescan.WorkspaceProfile
	// sshHostKey is the SSH gateway's ed25519 host key, loaded/generated ONLY
	// when -ssh-listen is set — nil (the zero value) otherwise, matching
	// "empty = off = no listener, no new surface" all the way down to never
	// minting the secret in the first place.
	sshHostKey ed25519.PrivateKey
	// uiSessionKey signs the UI-sandbox gateway's relay cookie, loaded/generated
	// ONLY when -ui-sandbox-listen is set — nil otherwise, the same
	// never-mint-a-secret-for-a-disabled-feature discipline as sshHostKey.
	uiSessionKey []byte
	// dir is the §I directory connector, nil unless WARDYN_DIRECTORY_PROVIDER is
	// set. nil is the ABSENT mode all the way down: the search endpoint answers
	// its distinct 503 and every "who" field stays free text.
	dir directory.Directory
}

// buildOptionalFeatures wires every optional subsystem from its flags. Extracted
// verbatim from run() — construction order and log lines are unchanged.
func buildOptionalFeatures(rootCtx, bootCtx context.Context, f *bootFlags, pool *pgxpool.Pool, secrets secretstore.Store, secureCookies bool, subPostureOK bool) (optionalFeatures, error) {
	var of optionalFeatures

	// Recording store (pluggable seam; default "pg" — see boot_flags.go). pg
	// makes a cast saved by one replica visible to replay on ANY replica; the fs
	// store's directory is per-pod, so a cross-replica replay 404s.
	// WARDYN_RECORDING_STORE=fs still selects that old on-disk behavior. Empty
	// -recording-dir disables the fs store specifically (recording.Deps' Dir
	// doc); it has no effect on pg, which is disabled only by an absent pool
	// (never the case once wardynd has booted — Postgres is the one required
	// dependency).
	recStore, rerr := recording.New(*f.recordingSel, recording.Deps{Dir: *f.recordingDir, Pool: pool})
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
	// hasRoleMap survives past the OIDC-configured block below (roleMap itself
	// is scoped to it) — validateOperatorPosture needs it after the block closes.
	var hasRoleMap bool
	if *f.oidcIssuer != "" {
		sessKey, kerr := loadOrCreateSessionKey(bootCtx, secrets)
		if kerr != nil {
			return of, kerr
		}
		// Role derivation config (WARDYN_OIDC_ROLE_MAP / WARDYN_OIDC_DEFAULT_ROLE):
		// parsed and validated here, gated on OIDC being configured like the
		// operator-allowlist rules below, and fails boot closed on a typo'd role
		// value rather than letting it silently reach a session cookie later.
		roleMap, rerr := oidc.ParseRoleMap(*f.oidcRoleMap)
		if rerr != nil {
			return of, fmt.Errorf("parse WARDYN_OIDC_ROLE_MAP: %w", rerr)
		}
		hasRoleMap = len(roleMap) > 0
		defaultRole := strings.TrimSpace(*f.oidcDefaultRole)
		if defaultRole != "" && !validDefaultRole(defaultRole) {
			return of, fmt.Errorf("invalid WARDYN_OIDC_DEFAULT_ROLE %q: want %q or %q (%q is a MAPPED tier only — name the App Role, group or email that should hold it in WARDYN_OIDC_ROLE_MAP; it is refused as a fallthrough default)",
				defaultRole, oidc.RoleAdmin, oidc.RoleMember, oidc.RoleSecurityAdmin)
		}
		// bootCtx (30s), not rootCtx: the ctx is used ONLY for the discovery
		// HTTP round trip (go-oidc's Provider.Verifier fetches JWKS on a
		// background ctx per its doc), so an unreachable/stalled IdP must fail
		// boot loudly inside the boot budget instead of hanging wardynd forever.
		authn, err := oidc.New(bootCtx, oidc.Config{
			IssuerURL:           *f.oidcIssuer,
			InternalIssuerURL:   *f.oidcInternalIss,
			ClientID:            *f.oidcClientID,
			ClientSecret:        *f.oidcClientSecret,
			RedirectURL:         *f.oidcRedirectURL,
			AllowedEmailDomains: splitCSV(*f.oidcEmailDomains),
			SecureCookies:       secureCookies,
			RoleMap:             roleMap,
			DefaultRole:         defaultRole,
			// Legacy source: a 0.4.5 deployment's WARDYN_OIDC_OPERATOR_EMAILS
			// keeps working as an admin allowlist with zero re-configuration
			// once it adopts WARDYN_OIDC_ROLE_MAP (see deriveRole).
			LegacyAdminEmails: splitCSV(*f.oidcOperatorEmails),
			// D16: revoke-a-human-now over the pg-backed cutoff table. Always
			// wired whenever OIDC is (pool is already required), unlike the
			// jti-level identity_revocations store which is a separate
			// concern — see pgSessionRevocations' doc comment.
			Revocations: &pgSessionRevocations{pool: pool},
			// RoleMappings (Phase 2 lane A, migration 0051): the console's
			// Getting Started -> People role-mapping store, merged with the
			// chart's WARDYN_OIDC_ROLE_MAP at every login (see mergeRoleMaps).
			// Wired unconditionally the same way Revocations is — pool is
			// already required whenever OIDC boots at all.
			RoleMappings: roleMappingsFor(pool),
			// OnLogin (migration 0046): every successful login re-stamps
			// role+role_checked_at on every ssh_public_keys row this principal
			// owns — the bounded-stale re-check sshAuth's admin-override path
			// reads (WARDYN_SSH_ROLE_TTL). store.NewPG(pool) is a cheap value
			// wrapper (constructed the same way elsewhere in this file), not a
			// connection of its own. Best-effort: a store hiccup here logs and
			// the login still succeeds — see oidc.Config.OnLogin's own doc for
			// why that contract lives on the callback side, not here.
			OnLogin: func(ctx context.Context, sub, role string) {
				if err := store.NewPG(pool).RefreshSSHKeyRoles(ctx, sub, role, time.Now().UTC()); err != nil {
					slog.Warn("wardynd: ssh key role refresh at login failed", slog.String("err", err.Error()))
				}
			},
		}, sessKey)
		if err != nil {
			return of, fmt.Errorf("oidc: %w", err)
		}
		of.authn = authn
		slog.Info("wardynd: OIDC SSO enabled", slog.String("issuer", *f.oidcIssuer))
		// The posture line differs by whether the operator allowlist is set, so
		// the log never overstates OR understates what the deployment enforces.
		// Log only the COUNT — the list itself is not disclosed.
		if ops := splitCSV(*f.oidcOperatorEmails); len(ops) > 0 {
			slog.Info("wardynd: NOTE a first-class packaged team deployment (SAML/SCIM, per-user tokens) does not exist yet, but admin/member RBAC does. "+
				"WARDYN_OIDC_OPERATOR_EMAILS is set: signed-in humans outside that list are MEMBERS (unless a WARDYN_OIDC_ROLE_MAP entry raises them to admin) — owner-scoped: they launch/kill runs and "+
				"read their OWN runs/approvals/audit (a foreign resource is a 404), but get 403 on configuring the deployment (harness-credential, "+
				"policy, workspace, site-config writes), on secret writes/deletes, and on admin-only credential/tool_call approvals (a member may still "+
				"decide egress_domain approvals on their own runs). admin/member is the only role tier — everything else, incl. the admin token, is always admin",
				slog.Int("operator_emails", len(ops)))
			// The allowlist matches the IdP's email claim, and email_verified is
			// only enforced when the domains list is set — without it, an IdP
			// that lets users self-assert email lets them claim an operator's
			// address. Warn, don't fail: failing would break the additive
			// unset-changes-nothing guarantee for the domains knob.
			if len(splitCSV(*f.oidcEmailDomains)) == 0 {
				slog.Warn("wardynd: WARDYN_OIDC_OPERATOR_EMAILS is set but WARDYN_OIDC_EMAIL_DOMAINS is not — email_verified is NOT enforced, so operator status rides an unverified IdP claim; set the domains list too")
			}
		} else {
			slog.Warn("wardynd: NOTE a first-class packaged team deployment (SAML/SCIM, per-user tokens) does not exist yet; " +
				"the console offers the 'Sign in with SSO' link and WARDYN_OIDC_OPERATOR_EMAILS is unset, so — absent a WARDYN_OIDC_ROLE_MAP — every SSO human would have the same power as the admin token — " +
				"boot continues past this ONLY with WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST set (set the operator list instead to make everyone else a member)")
		}
		warnRoleMapPosture(roleMap, f, defaultRole)
	}

	// The second boot refusal (validateConfig, main.go, is the first): SSO
	// configured with no operator allowlist makes every signed-in human
	// admin-equivalent. Checked HERE rather than in validateConfig because the
	// authenticator only exists this far into boot; of.authn is the resolved
	// "OIDC is configured" fact, so this cannot drift from what actually mounted.
	if err := validateOperatorPosture(of.authn != nil, splitCSV(*f.oidcOperatorEmails), *f.allowOIDCNoOperatorList, hasRoleMap); err != nil {
		return of, err
	}

	// Directory autocomplete (§I / PF-29), opt-in. Its own function because the
	// refusal-plus-construct-plus-announce shape is a unit, and buildOptionalFeatures
	// is at the cyclomatic ceiling.
	dir, derr := buildDirectoryConnector(f)
	if derr != nil {
		return of, derr
	}
	of.dir = dir

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

	// Subscription OAuth token provider: yields the operator's LIVE Anthropic
	// access token from the resident ~/.claude so subscription runs are
	// credentialed PROXY-SIDE (the sandbox holds an inert sentinel that never
	// goes stale) instead of a copy whose refresh token rotates out from under it.
	// Constructed unconditionally; it only reads/refreshes when a subscription run
	// resolves its injection. Escape hatch: WARDYN_SUBSCRIPTION_INJECT=off keeps
	// the legacy resident-copy behavior.
	//
	// NOT constructed at all when subscriptionInjectPosture (boot_posture.go) says
	// this deployment may not share one operator's credential. Refusing at the sink
	// would be enough to stop a run getting the token, but refusing to CONSTRUCT is
	// what makes "this deployment cannot read or refresh the operator's ~/.claude"
	// a statement about the process rather than a boolean someone can chase through
	// call sites — subscription.Provider.Current() shells out to the resident
	// `claude` and ROTATES that file, so a provider that exists is a provider that
	// can mutate the operator's personal credential.
	of.subToken = newSubscriptionProvider(subPostureOK)
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
	// Posture-gated for the same reason as subToken above: the managed lane is a
	// DEFAULT FALLBACK for every claude-code run (runs_dispatch_llm.go), needing no
	// policy, no integration id and no flag, so on a multi-user stack it is the
	// broadest sharing path of the two — and WARDYN_SUBSCRIPTION_INJECT never
	// covered it.
	if subPostureOK {
		of.managedToken = api.NewManagedCredProvider(secrets, "anthropic")
	}

	// Advisory AI scan fallback (opt-in): wired to the fail-open
	// workspacescan.AdviseProfile with a bounded timeout so a slow/hung CLI can
	// never stall — let alone fail — the sidecar's scan upload. nil = OFF.
	if *f.scanAIAdvisor {
		of.scanAdvisor = func(ctx context.Context, facts workspacescan.ScanFacts, base workspacescan.WorkspaceProfile) workspacescan.WorkspaceProfile {
			return workspacescan.AdviseProfile(ctx, facts, base, workspacescan.AIOptions{Timeout: 60 * time.Second})
		}
		slog.Info("wardynd: advisory AI workspace-scan fallback ENABLED (WARDYN_SCAN_AI_ADVISOR); advisory-only + fail-open, needs a resident read-only claude CLI on PATH")
	}

	// SSH gateway (C2/C3), optional: the host key is loaded/generated ONLY
	// when the gateway is actually enabled, so a deployment with SSH off never
	// mints this secret at all — the same "no new surface" discipline as the
	// listener itself (api.Server.ServeSSHGateway's own no-op-when-empty
	// guard). loadOrCreateSSHHostKey follows the identical loadOrCreateSecret
	// pattern as the signing/session keys above.
	if *f.sshListen != "" {
		hostKey, herr := loadOrCreateSSHHostKey(bootCtx, secrets)
		if herr != nil {
			return of, herr
		}
		of.sshHostKey = hostKey
		slog.Info("wardynd: ssh gateway enabled", slog.String("listen", *f.sshListen), slog.String("advertise", *f.sshAdvertise))
		if *f.sshAdvertise == "" {
			slog.Warn("wardynd: WARDYN_SSH_ADVERTISE is unset — the run-detail SSH pane has no reachable host[:port] to show; set it to this deployment's externally-reachable address")
		}
	}

	// UI-sandbox gateway (pillar 4), optional: the relay cookie's HMAC key is
	// minted ONLY when the gateway is enabled, the same discipline as the SSH
	// host key above.
	if *f.uiListen != "" {
		uiKey, kerr := loadOrCreateUISessionKey(bootCtx, secrets)
		if kerr != nil {
			return of, kerr
		}
		of.uiSessionKey = uiKey
		slog.Info("wardynd: ui-sandbox gateway enabled",
			slog.String("listen", *f.uiListen),
			slog.String("advertise", *f.uiAdvertise),
			slog.Bool("host_mode", *f.uiOriginTemplate != ""),
		)
		if *f.uiAdvertise == "" && *f.uiOriginTemplate == "" {
			slog.Warn("wardynd: WARDYN_UI_SANDBOX_ADVERTISE is unset — /healthz will advertise the raw bind address, which is wrong for any deployment whose bind is not its reachable address; set it (or WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE) whenever the gateway is enabled")
		}
		if *f.uiOriginTemplate == "" {
			slog.Warn("wardynd: ui-sandbox gateway is in SHARED-ORIGIN mode — every run's app is served from one origin, separated only by a path-scoped cookie; set WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE (wildcard DNS) to give each run its own origin")
		}
	}

	return of, nil
}

// warnRoleMapPosture emits the three non-fatal WARNINGs about how roles will be
// derived from SSO. All three are warnings by design: each describes a posture
// that is unusual rather than wrong, every one of them is administrable through
// the admin token or local mode, and a chart map is edited by `helm upgrade` —
// refusing would break an upgrade over a shape that merely deserves a second
// look. Only the CHART map is considered, because console-managed rows are read
// per login and do not exist at boot; that is also the only source a helm
// operator can act on from a boot log.
func warnRoleMapPosture(roleMap map[string]string, f *bootFlags, defaultRole string) {
	// Role-map posture (independent knob from the operator-emails split
	// above; a later lane unifies the two — see internal/auth/oidc's
	// deriveRole doc). With no roleMap, deriveRole derives roles from the
	// WARDYN_OIDC_OPERATOR_EMAILS allowlist alone (listed = admin, everyone
	// else = member); only when the allowlist is ALSO empty does every
	// signed-in human become admin — and that case is the loud else-branch
	// warning above, so here we only nudge an allowlist-split operator toward
	// a role map for claim-based members.
	if len(roleMap) == 0 {
		if len(splitCSV(*f.oidcOperatorEmails)) > 0 {
			slog.Warn("wardynd: no WARDYN_OIDC_ROLE_MAP set — roles come only from the WARDYN_OIDC_OPERATOR_EMAILS allowlist (listed = admin, everyone else = member); set a role map to derive admin/member from SSO roles/groups instead")
		}
	} else if len(splitCSV(*f.oidcEmailDomains)) == 0 {
		// Same warning shape as the WARDYN_OIDC_OPERATOR_EMAILS one above
		// (~:270), fired independently since either var can be set without
		// the other: an email-keyed WARDYN_OIDC_ROLE_MAP entry is a SECOND
		// email-keyed privilege source riding an unverified IdP claim —
		// email_verified is enforced only when the domains list is set.
		for k := range roleMap {
			if strings.Contains(k, "@") {
				slog.Warn("wardynd: WARDYN_OIDC_ROLE_MAP has an email-keyed entry but WARDYN_OIDC_EMAIL_DOMAINS is not set — email_verified is NOT enforced, so that role assignment rides an unverified IdP claim; prefer roles/groups keys (IdP-signed), or set the domains list too")
				break
			}
		}
	}
	// Third-tier posture: a chart map that grants security_admin but names
	// no super admin at all. WARN, never a refusal — the admin token and
	// local mode both pass every gate on the no-OIDC-human arm
	// (internal/api's isOperator), so this deployment is administrable, and
	// a chart map is edited by helm upgrade: refusing would break an
	// upgrade for a posture that is merely unusual. Console-managed rows
	// are NOT considered here and cannot be — they are read per login, not
	// at boot — so this is honestly scoped to the CHART map, which is also
	// the only source a helm operator can act on from a boot log.
	if chartMapHasNoAdminPath(roleMap, splitCSV(*f.oidcOperatorEmails), defaultRole) {
		slog.Warn("wardynd: WARDYN_OIDC_ROLE_MAP grants " + oidc.RoleSecurityAdmin +
			" but no admin: no admin-valued entry, no WARDYN_OIDC_OPERATOR_EMAILS," +
			" and WARDYN_OIDC_DEFAULT_ROLE is not admin — a security admin governs" +
			" approvals/audit/permissions/governance profiles but never reaches another" +
			" human's run, credentials or host config, so SSO alone cannot administer" +
			" this deployment (the admin token and local mode still can; add an" +
			" admin-valued map entry or the operator allowlist to fix it)")
	}
}

// componentsInfo builds the pluggable-component selection advertised on
// /healthz. "selected" is the ACTUAL running impl; "available" is what each
// seam's registry has self-registered in THIS build (so a newly registered
// substrate/provider appears with no edit here — and a tagless build honestly
// shows sandbox.available=[]). Runtime facts only: the recommended-vs-shipped
// split lives in docs/PLUGGABILITY.md and ROADMAP.md, where a recommendation
// this build cannot yet run belongs. policy_engine has no registry yet, so it
// carries no "available".
//
// recStore is the ACTUAL constructed store (nil when disabled — e.g. the fs
// backend with no directory, the stock Helm install's default: persistence
// off, WARDYN_RECORDING_DIR empty). Without it, "recording" reported the
// *flag* (*f.recordingSel, e.g. "fs") regardless of whether that backend ever
// came up, so a stock deployment's /healthz claimed a live recording store
// while every run silently recorded nothing and the UI blamed "no session
// captured yet" — a broken promise, not a missing feature.
func componentsInfo(f *bootFlags, runnerTarget string, recStore recording.Store) map[string]api.ComponentInfo {
	sourceOf := func(selected, def string) string {
		if selected == def {
			return "default"
		}
		return "configured"
	}
	recInfo := api.ComponentInfo{Selected: *f.recordingSel, Available: recording.Names(), Source: sourceOf(*f.recordingSel, "pg")}
	if recStore == nil {
		recInfo = api.ComponentInfo{Selected: "none", Available: recording.Names(), Source: "disabled"}
	}
	return map[string]api.ComponentInfo{
		"identity":      {Selected: *f.identitySel, Available: identity.Names(), Source: sourceOf(*f.identitySel, "embedded")},
		"secret_store":  {Selected: *f.secretStoreSel, Available: secretstore.Names(), Source: sourceOf(*f.secretStoreSel, "pg")},
		"recording":     recInfo,
		"policy_engine": {Selected: "builtin"},
		"sandbox":       {Selected: runnerTarget, Available: substrate.Names(), Source: sourceOf(*f.runnerSel, "none")},
	}
}

// validDefaultRole reports whether role is admissible as
// WARDYN_OIDC_DEFAULT_ROLE. STRICTER than oidc.ValidRole on exactly one value:
// oidc.RoleSecurityAdmin is refused, and boot FAILS CLOSED on it.
//
// The default role is what a signed-in human falls through to when NOTHING in
// the merged role map matched them — i.e. the tier granted by accident, to
// everyone the operator never named. security_admin governs approvals, audit,
// permissions/capability grants and governance profiles; handing that to the
// unnamed population is the one configuration the tier's whole design (a
// MAPPED tier only, no allowlist twin — see oidc.RoleSecurityAdmin) exists to
// make unreachable. A deployment that wants a security admin names the App
// Role, group or email that holds it.
//
// Refused rather than warned because the failure is silent otherwise: the
// misconfiguration produces no error at any point, just a quietly over-powered
// org, discovered at audit time.
func validDefaultRole(role string) bool {
	return oidc.ValidRole(role) && role != oidc.RoleSecurityAdmin
}

// chartMapHasNoAdminPath reports whether the chart role map grants
// security_admin somewhere while offering NO route to the super-admin tier —
// no admin row of its own, no operator allowlist, and no admin default role.
// See its one caller for why that is a WARN and not a refusal.
func chartMapHasNoAdminPath(roleMap map[string]string, operatorEmails []string, defaultRole string) bool {
	if len(operatorEmails) > 0 || defaultRole == oidc.RoleAdmin {
		return false
	}
	sec := false
	for _, v := range roleMap {
		if v == oidc.RoleAdmin {
			return false
		}
		if v == oidc.RoleSecurityAdmin {
			sec = true
		}
	}
	return sec
}

// newSubscriptionProvider builds the resident-subscription token provider, or nil
// when this deployment's posture forbids sharing one operator's credential.
//
// Returning nil is the point, not an optimisation: subscription.Provider.Current()
// shells out to the resident `claude` and ROTATES the operator's own
// ~/.claude/.credentials.json, so a provider that exists is a provider that can
// mutate their personal credential. Not constructing it makes "this deployment
// cannot read or refresh that file" a property of the process.
func newSubscriptionProvider(postureOK bool) subscription.Provider {
	if !postureOK {
		return nil
	}
	p, err := subscription.New(subscription.Config{})
	if err != nil {
		slog.Warn("wardynd: subscription token provider unavailable; subscription runs fall back to the resident-copy behavior",
			slog.Any("err", err),
		)
		return nil
	}
	return p
}

// buildDirectoryConnector resolves the §I directory config and, when the feature
// is on, constructs the connector and announces the posture expansion. Returns a
// nil Directory when WARDYN_DIRECTORY_PROVIDER is unset — off is a zero config and
// a nil connector: no Graph reach, no new egress, no behaviour change anywhere.
func buildDirectoryConnector(f *bootFlags) (directory.Directory, error) {
	// Directory autocomplete (§I / PF-29), opt-in. The THIRD boot refusal, and
	// it lives here for the same reason the second one does: the resolution
	// depends on the OIDC flags this function has just consumed. Off (provider
	// unset) it is a zero config and a nil connector — no Graph reach, no new
	// egress, no behaviour change anywhere.
	dirCfg, derr := resolveDirectoryConfig(*f.dirProvider, *f.dirTenant, *f.dirClientID, *f.dirSecret,
		*f.oidcIssuer, *f.oidcClientID, *f.oidcClientSecret)
	if derr != nil {
		return nil, derr
	}
	if dirCfg.TenantID == "" {
		return nil, nil
	}
	dir, err := directory.NewEntra(dirCfg)
	if err != nil {
		// Unreachable given resolveDirectoryConfig's own completeness check
		// (NewEntra's only error is ErrUnconfigured, on an absent credential)
		// — surfaced rather than dropped so a connector that grows a second
		// error cannot silently disable itself.
		return nil, fmt.Errorf("directory connector: %w", err)
	}
	// Log the ENABLE, matching the OIDC line above. There is deliberately no
	// audit row for it: enabling is an env var read once at boot, with no
	// recorder wired yet and no runtime toggle to audit — the audited
	// directory event is the connector FAILURE, emitted per search from
	// internal/api (see auditDirectoryFailure). Naming the whole-directory
	// reach here is the point: it is a real posture expansion, and an
	// operator reading boot logs should see it stated, not inferred.
	slog.Info("wardynd: directory autocomplete enabled — wardynd may now READ THE WHOLE DIRECTORY "+
		"(users + groups, App Roles when consented) over daemon-side outbound HTTPS to graph.microsoft.com:443; "+
		"suggestions are cached in memory for 60s and never persisted. Unset WARDYN_DIRECTORY_PROVIDER to retract it",
		slog.String("provider", directoryProviderEntra),
		slog.Bool("dedicated_app", strings.TrimSpace(*f.dirClientID) != ""),
	)
	return dir, nil
}
