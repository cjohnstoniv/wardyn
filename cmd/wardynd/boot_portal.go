// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

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
func buildPortalIssuers(f *bootFlags, localOperator string, st store.PG) (*oidc.PortalIssuers, *api.PortalRefreshFailures, error) {
	counter := &api.PortalRefreshFailures{Portals: livePortalsOn(st)}
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

// bootPortalIssuers is the portal half of the boot sequence: when console sign-in is configured its issuer is
// recorded, and refused when a portal binding already uses it, and the verifier set is then built.
func bootPortalIssuers(ctx context.Context, pool *pgxpool.Pool, f *bootFlags, localOperator string, st store.PG, consoleSignIn *oidc.Authenticator) (*oidc.PortalIssuers, *api.PortalRefreshFailures, error) {
	if consoleSignIn != nil {
		if err := recordConsoleIssuer(ctx, pool, consoleSignIn.Issuer(), replicaID()); err != nil {
			return nil, nil, err
		}
	}
	return buildPortalIssuers(f, localOperator, st)
}

// livePortalsOn names, by delegate id, the portals registered and not revoked on an issuer, for the refresh
// failure counter and its log line: two portals sharing an issuer share its failures, and the operator needs
// to see which registrations they hit. A read that fails attributes to none.
func livePortalsOn(st store.PG) func(issuer string) []string {
	return func(issuer string) []string {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		all, err := st.ListDelegates(ctx)
		if err != nil {
			return nil
		}
		var ids []string
		for _, d := range all {
			if d.Issuer == issuer && d.RevokedAt == nil {
				ids = append(ids, d.ID.String())
			}
		}
		sort.Strings(ids)
		return ids
	}
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
	var clashes, keys []string
	for _, b := range bindings {
		if types.NormIssuerURL(b.Issuer) == types.NormIssuerURL(issuer) {
			clashes = append(clashes, b.Issuer)
			keys = append(keys, b.IssuerKey)
		}
	}
	if len(clashes) > 0 {
		// A diagnostic only, read through this transaction: the boot is refused whatever this read finds, a
		// binding no registration names and a failed read included.
		names := "the portal registrations could not be read"
		if total, regs, err := portalRegistrationsOn(ctx, tx, keys); err == nil {
			names = portalRegistrationsText(total, regs)
		}
		return fmt.Errorf("%w: the console sign-in issuer %q is also the issuer of an issuer portal binding (%s; %s); an issuer is either the console's or a portal's, never both",
			errConsoleIssuerIsPortal, issuer, strings.Join(clashes, ", "), names)
	}
	if err := st.RecordConsoleIssuerQ(ctx, tx, issuer, replica); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("record the console issuer: %w", err)
	}
	return nil
}

// portalRegistrationMax bounds how many registrations the boot refusal names.
const portalRegistrationMax = 5

// portalReg is one portal registration the boot refusal names.
type portalReg struct {
	id, name string
	revoked  bool
}

// portalRegistrationsOn reads through tx how many portal registrations name the bindings keys, and the first
// portalRegistrationMax of them by id.
func portalRegistrationsOn(ctx context.Context, tx pgx.Tx, keys []string) (total int, regs []portalReg, err error) {
	rows, err := tx.Query(ctx, `
		SELECT count(*) OVER (), id::text, name, revoked_at IS NOT NULL
		  FROM delegates
		 WHERE issuer_key = ANY($1)
		 ORDER BY id
		 LIMIT $2`, keys, portalRegistrationMax)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r portalReg
		if err := rows.Scan(&total, &r.id, &r.name, &r.revoked); err != nil {
			return 0, nil, err
		}
		regs = append(regs, r)
	}
	return total, regs, rows.Err()
}

// portalRegistrationsText names regs by id and name, revoked ones marked, and counts the rest of total.
func portalRegistrationsText(total int, regs []portalReg) string {
	if len(regs) == 0 {
		return "no portal registration names it"
	}
	named := make([]string, 0, len(regs))
	for _, r := range regs {
		n := fmt.Sprintf("%s %q", r.id, r.name)
		if r.revoked {
			n += " (revoked)"
		}
		named = append(named, n)
	}
	out := "portal registrations: " + strings.Join(named, ", ")
	if more := total - len(regs); more > 0 {
		out += fmt.Sprintf(" and %d more", more)
	}
	return out
}
