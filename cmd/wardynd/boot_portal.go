// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Boot wiring of issuer portals: the verifier set, and the record of the console issuer this process signs
// people in with, which no issuer portal may share.

// portalInternalHostList is WARDYN_PORTAL_INTERNAL_ISSUER_HOSTS as a list: comma-separated host[:port], blanks
// dropped.
func portalInternalHostList(raw string) []string {
	var out []string
	for _, h := range strings.Split(raw, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// portalIssuerOptions is the verifier set's deployment side: the reserved-name predicate every human door
// asks (api.PortalReservedCheck, for this server's local operator), the listed internal hosts and their CA
// bundle, and failed refreshes counted on counter. An unreadable or empty CA file refuses the boot: the
// operator named a bundle, and verifying against fewer roots than they meant must not pass silently.
func portalIssuerOptions(hosts, caFile, localOperator string, counter *api.PortalRefreshFailures) (oidc.PortalIssuersOptions, error) {
	opts := oidc.PortalIssuersOptions{
		Reserved:         api.PortalReservedCheck(localOperator),
		InternalHosts:    portalInternalHostList(hosts),
		OnRefreshFailure: counter.Record,
	}
	if caFile = strings.TrimSpace(caFile); caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return oidc.PortalIssuersOptions{}, fmt.Errorf("refusing to start: read WARDYN_PORTAL_INTERNAL_ISSUER_CA_FILE: %w", err)
		}
		opts.InternalCAPEM = pem
	}
	return opts, nil
}

// buildPortalIssuers builds the verifier set and the counter its refresh failures land on.
func buildPortalIssuers(f *bootFlags, localOperator string) (*oidc.PortalIssuers, *api.PortalRefreshFailures, error) {
	counter := &api.PortalRefreshFailures{}
	opts, err := portalIssuerOptions(*f.portalInternalHosts, *f.portalInternalCAFile, localOperator, counter)
	if err != nil {
		return nil, nil, err
	}
	p, err := oidc.NewPortalIssuers(opts)
	if err != nil {
		return nil, nil, fmt.Errorf("refusing to start: issuer portals: %w", err)
	}
	return p, counter, nil
}

// errConsoleIssuerIsPortal is the boot refusal of a console issuer that equals a portal binding's issuer.
var errConsoleIssuerIsPortal = errors.New("refusing to start")

// recordConsoleIssuer is the boot step of a replica with console sign-in configured: under the registry lock
// that every portal registration, approval and retirement takes, it refuses to start when the console issuer
// equals, under norm, the issuer of any portal binding, and otherwise records it. The record only grows, so a
// console issuer once served can never become a portal issuer, whichever replica is still running it. A
// replica with no console sign-in records nothing and checks nothing.
func recordConsoleIssuer(ctx context.Context, pool *pgxpool.Pool, issuer, replica string) error {
	if strings.TrimSpace(issuer) == "" {
		return nil
	}
	st := store.NewPG(pool)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("record the console issuer: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.LockPortalIssuerRegistry(ctx, tx); err != nil {
		return err
	}
	bindings, err := store.PortalIssuersQ(ctx, tx)
	if err != nil {
		return fmt.Errorf("read the issuer portal bindings: %w", err)
	}
	var clashes []string
	for _, b := range bindings {
		if types.NormIssuerURL(b.Issuer) == types.NormIssuerURL(issuer) {
			clashes = append(clashes, b.Issuer)
		}
	}
	if len(clashes) > 0 {
		return fmt.Errorf("%w: the console sign-in issuer %q is also the issuer of an issuer portal binding (%s); an issuer is either the console's or a portal's, never both",
			errConsoleIssuerIsPortal, issuer, strings.Join(clashes, ", "))
	}
	if err := st.RecordConsoleIssuerQ(ctx, tx, issuer, replica); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record the console issuer: %w", err)
	}
	return nil
}
