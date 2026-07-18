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
	secretSigningKey   = "wardyn-signing-key" // embedded identity ES256 PEM
	secretGitHubAppID  = "github-app-id"      // GitHub App numeric id
	secretGitHubAppKey = "github-app-key"     // GitHub App PEM private key
	secretSessionKey   = "wardyn-session-key" // OIDC session-cookie HMAC key (32 bytes)
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
	posture, err := validateConfig(*f.dsn, *f.tlsCert, *f.tlsKey, *f.tlsTerminated)
	if err != nil {
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
	// connectAndMigrate for the WARDYN_PG_MIGRATE_DSN role-split (DDL protection).
	bootCtx, cancel := context.WithTimeout(rootCtx, 30*time.Second)
	defer cancel()
	pool, err := connectAndMigrate(bootCtx, *f.dsn, *f.migrateDSN)
	if err != nil {
		return err
	}
	defer pool.Close()

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

	// Approval FSM service (adapter over internal/approval + internal/store).
	// FIX #5: wired with maskedRec (masked + SIEM fanout), matching idp/broker —
	// approval.decide events now reach file/webhook/syslog sinks, not just Postgres.
	approvals := &approvalService{pool: pool, rec: maskedRec}

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

	// Optional subsystems (recording replay, OIDC SSO, devcontainer builds, the
	// AI Run Composer, subscription/managed LLM credential providers, advisory AI
	// scan fallback) — each nil/off when unconfigured; see buildOptionalFeatures.
	feats, err := buildOptionalFeatures(rootCtx, bootCtx, f, secrets, posture.secureCookies)
	if err != nil {
		return err
	}

	srv := api.New(api.Config{
		Store:     store.NewPG(pool),
		Identity:  idp,
		Approvals: approvals,
		Broker:    brk,
		Audit:     maskedRec,
		// hand the raw spool + raw store recorder to the server so it starts
		// the background drain that replays spooled events back into the store once
		// PG recovers (both nil when no spool is configured => drain is a no-op).
		AuditSpool:                auditSpool,
		AuditDrainRecorder:        auditDrainRec,
		Runner:                    run,
		AdminToken:                *f.adminToken,
		LocalMode:                 lm.enabled,
		LocalOperator:             lm.operator,
		TrustDomain:               *f.trustDomain,
		DefaultPolicy:             defaultPolicy,
		RunnerTarget:              runnerTarget,
		UIDir:                     *f.uiDir,
		ControlPlaneURL:           *f.controlURL,
		RecordingStore:            feats.recStore,
		OIDC:                      feats.authn,
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
		Composer:                  feats.composerReg,
		Components:                componentsInfo(f, runnerTarget),
		ScanAIAdvisor:             feats.scanAdvisor,
		// First-run setup readiness inputs (GET /api/v1/setup/status).
		AgeKeyDurable:       strings.TrimSpace(*f.ageKey) != "",
		LocalLoopback:       lm.loopback,
		LocalTrustForwarder: *f.localTrustFwd,
		ComposerBackends:    feats.composerBackends,
		// rootCtx is the daemon-lifetime base context for detached background
		// work (the run completion watcher) that must outlive the create-run
		// request. It is cancelled on SIGINT/SIGTERM at shutdown.
		BaseCtx: rootCtx,
	})

	// Periodic goroutines (lifecycle reaper, groundtruth token rotator, approval
	// expiry sweeper) + the boot-time reconciliation pass (C3).
	startBackgroundWorkers(rootCtx, f, srv, run, pool, idp, brk, maskedRec)

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
func validateConfig(dsn, tlsCert, tlsKey string, tlsTerminated bool) (tlsPosture, error) {
	if dsn == "" {
		return tlsPosture{}, errors.New("missing -dsn / WARDYN_PG_DSN")
	}
	if (tlsCert != "") != (tlsKey != "") {
		return tlsPosture{}, errors.New("TLS misconfigured: set BOTH -tls-cert/WARDYN_TLS_CERT and -tls-key/WARDYN_TLS_KEY, or neither")
	}
	tlsEnabled := tlsCert != "" && tlsKey != ""
	return tlsPosture{
		tlsEnabled:    tlsEnabled,
		secureCookies: tlsEnabled || tlsTerminated,
	}, nil
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
	// Committed default of scripts/run-local.sh + scripts/e2e-backend.sh. Those
	// scripts now mint an ephemeral per-boot key via `wardynd -gen-age-key`, so
	// nothing legitimate uses this one.
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

// flagEnv/flagBool/flagDuration/splitCSV are shared with cmd/wardyn-tetragon-ingest
// via internal/cliutil (mirrored duplicates there previously).
var (
	flagEnv      = cliutil.FlagEnv
	flagBool     = cliutil.FlagBool
	flagDuration = cliutil.FlagDuration
	splitCSV     = cliutil.SplitCSV
)
