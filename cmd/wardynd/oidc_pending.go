// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Booting with the identity provider down. A deployment that has booted with an issuer before (it has a row in
// console_issuers) starts when that issuer's provider does not answer: the authenticator is non-nil, so every
// protection that reads "SSO is configured" stays on, and only sign-in is closed until a background retry
// reaches the provider. Anything else still refuses boot: a first boot, an issuer the database does not know,
// and a provider that answered with a configuration problem.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// issuerKnownBudget bounds the console_issuers read.
	issuerKnownBudget = 5 * time.Second
	// discoverAttemptBudget bounds each background retry, and discoverBackoffFloor and discoverBackoffCap its
	// wait.
	discoverAttemptBudget = 30 * time.Second
	discoverBackoffFloor  = 2 * time.Second
	discoverBackoffCap    = 60 * time.Second
)

// firstDiscoverBudget is how long boot waits for the provider on a known issuer, a variable so a test can
// shorten it. It leaves most of bootCtx (30 s) for the steps after it.
var firstDiscoverBudget = 10 * time.Second

// issuerKnown reports whether this deployment has booted with issuer before.
type issuerKnown func(ctx context.Context, issuer string) (bool, error)

// consoleIssuerKnown reads console_issuers. A nil pool knows nothing.
func consoleIssuerKnown(pool *pgxpool.Pool) issuerKnown {
	return func(ctx context.Context, issuer string) (bool, error) {
		if pool == nil {
			return false, nil
		}
		rows, err := store.NewPG(pool).ConsoleIssuersQ(ctx, pool)
		if err != nil {
			return false, err
		}
		want := types.NormIssuerURL(issuer)
		for _, r := range rows {
			if r.IssuerNorm == want {
				return true, nil
			}
		}
		return false, nil
	}
}

// consoleAuthenticator builds the console's authenticator. On a known issuer whose provider does not answer
// (no response, a 5xx or 429) it returns a pending authenticator and keeps trying in the background under
// rootCtx; on every other failure, and for an issuer it cannot show was booted with before, it fails as
// oidc.New always did.
func consoleAuthenticator(rootCtx, bootCtx context.Context, known issuerKnown, cfg oidc.Config, key []byte) (*oidc.Authenticator, error) {
	kctx, cancel := context.WithTimeout(rootCtx, issuerKnownBudget)
	k, kerr := known(kctx, cfg.IssuerURL)
	cancel()
	if kerr != nil {
		slog.Warn("wardynd: could not read the console issuers; treating the OIDC issuer as not booted with before", "issuer", cfg.IssuerURL, "error", kerr)
		k = false
	}
	if !k {
		// bootCtx (30s), not rootCtx: an unreachable or stalled IdP must fail boot loudly inside the boot
		// budget instead of hanging wardynd forever.
		a, err := oidc.New(bootCtx, cfg, key)
		if err != nil {
			return nil, err
		}
		slog.Info("wardynd: OIDC SSO enabled", slog.String("issuer", cfg.IssuerURL))
		return a, nil
	}
	a, err := oidc.NewPending(cfg, key)
	if err != nil {
		return nil, err
	}
	dctx, dcancel := context.WithTimeout(bootCtx, firstDiscoverBudget)
	err = a.Discover(dctx)
	dcancel()
	switch {
	case err == nil:
		slog.Info("wardynd: OIDC SSO enabled", slog.String("issuer", cfg.IssuerURL))
		return a, nil
	case errors.Is(err, oidc.ErrProviderUnreachable):
		url := cfg.IssuerURL
		if cfg.InternalIssuerURL != "" {
			url = cfg.InternalIssuerURL
		}
		slog.Error("wardynd: the identity provider did not answer; starting with sign-in closed and retrying in the background",
			"issuer", cfg.IssuerURL, "discovery_url", url, "error", err)
		go discoverUntilReady(rootCtx, a, sleepContext)
		return a, nil
	default:
		return nil, err
	}
}

// sleepContext waits d, and reports false when ctx ends first.
func sleepContext(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// discoverUntilReady retries Discover until it succeeds or ctx ends. A cut that could not be written retries at
// the 2 s floor. A provider that does not answer, signing keys that do not answer and an attempt that ran out
// of time wait 2 s, doubling to 60 s. Any other error (the provider answered, and its document or scopes are
// wrong) is logged at ERROR and retried at the cap: boot is past, so sign-in stays closed. When ctx ends with
// logouts still held, each is recorded best effort.
func discoverUntilReady(ctx context.Context, a *oidc.Authenticator, wait func(context.Context, time.Duration) bool) {
	backoff := discoverBackoffFloor
	for attempt := 1; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, discoverAttemptBudget)
		err := a.Discover(actx)
		cancel()
		if err == nil {
			slog.Info("wardynd: the identity provider answered; sign-in is open", "attempts", attempt)
			return
		}
		var next time.Duration
		switch {
		case errors.Is(err, oidc.ErrLogoutCutFailed):
			next = discoverBackoffFloor
		case errors.Is(err, oidc.ErrProviderUnreachable), errors.Is(err, oidc.ErrLogoutKeysUnavailable), errors.Is(err, oidc.ErrLogoutDrainIncomplete):
			next = backoff
			backoff = min(backoff*2, discoverBackoffCap)
		default:
			slog.Error("wardynd: the identity provider answered but its configuration is refused; sign-in stays closed", "attempt", attempt, "error", err)
			next = discoverBackoffCap
		}
		slog.Warn("wardynd: the identity provider is not ready; retrying", "attempt", attempt, "wait", next.String(), "error", err)
		if !wait(ctx, next) {
			oidc.ReportLogoutsLostAtShutdown(a)
			return
		}
	}
}
