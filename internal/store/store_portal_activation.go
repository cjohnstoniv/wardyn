// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The one-way switch that turns issuer portals on, and the sample of other database clients that can veto it.
//
// The mark is a store-only key of the site_config document, like the service grants' keys. Neither
// types.SiteConfig nor the document the older server writes declares it, so every whole-document save keeps it
// (PutSiteConfigQ keeps any key it does not declare) and a request body that names it is refused. Only
// ActivatePortalIssuers writes it, and nothing clears it.
package store

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PortalIssuersActivatedKey is the site_config document key that holds the time issuer portals were switched
// on. It is exported for the write conditions of other lanes' statements, which refuse a write that would
// need the activation before it is set.
const PortalIssuersActivatedKey = "portal_issuers_activated_at"

// PortalSessionUnknown is the label and address UntaggedClientSessions reports for a client session whose
// details the calling role cannot read.
const PortalSessionUnknown = "unknown"

// PortalActivationStore is the activation's store surface, optional like the other portal stores.
type PortalActivationStore interface {
	PortalIssuersActivated(ctx context.Context) (bool, error)
	ActivatePortalIssuers(ctx context.Context, by string, override []string) error
	UntaggedClientSessions(ctx context.Context) ([]types.ClientSession, error)
}

var _ PortalActivationStore = PG{}

// PortalIssuersActivated reports whether issuer portals have been switched on. A deployment with no stored
// site config has not.
func (s PG) PortalIssuersActivated(ctx context.Context) (bool, error) {
	var on bool
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT config ? $1::text FROM site_config WHERE singleton), false)`, PortalIssuersActivatedKey).Scan(&on)
	if err != nil {
		return false, fmt.Errorf("store: read portal issuer activation: %w", err)
	}
	return on, nil
}

// ActivatePortalIssuers sets the activation mark in one statement: it creates the site_config row when none is
// stored (an UPDATE alone would report success and store nothing), and on a row that exists sets the key only
// when it is absent, so a second activation keeps the first time. Nothing clears it.
//
// by and override name the actor and the sessions the operator overrode. They belong to the audit rows the
// caller writes (portal_issuers.activate, portal_issuers.activate_override); the document keeps only the time,
// because it is read by code that knows no other key.
func (s PG) ActivatePortalIssuers(ctx context.Context, by string, override []string) error {
	_, _ = by, override
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO site_config (singleton, config, updated_at)
		VALUES (true, jsonb_build_object($1::text, to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')), now())
		ON CONFLICT (singleton) DO UPDATE
			SET config = site_config.config || EXCLUDED.config, updated_at = now()
			WHERE NOT (site_config.config ? $1::text)`, PortalIssuersActivatedKey); err != nil {
		return fmt.Errorf("store: activate portal issuers: %w", err)
	}
	return nil
}

// UntaggedClientSessions is every other client session on this database that does not carry a wardynd label of
// at least 0.10.2: an empty label, an older version, any other label, or a session whose label the caller
// cannot read. It reads pg_stat_activity, so it sees only the connections that exist when it runs, and a label
// is only what the client chose; the activation's veto and the hourly alarm are a report, not a proof.
//
// A role without pg_read_all_stats sees another role's session with its backend type, application name and
// address masked. Such a session is still a client, and nothing about it shows it is current, so it counts as
// untagged and is reported with the label and address "unknown" (PortalSessionUnknown).
func (s PG) UntaggedClientSessions(ctx context.Context) ([]types.ClientSession, error) {
	all, err := collect(ctx, s.Pool, "list", "client sessions", `
		SELECT COALESCE(application_name, $1), CASE WHEN application_name IS NULL THEN $1 ELSE COALESCE(client_addr::text, '') END
		  FROM pg_stat_activity
		 WHERE datname = current_database() AND pid <> pg_backend_pid() AND `+db.OtherClientSQL+`
		 ORDER BY 1, 2`, []any{PortalSessionUnknown}, func(r pgx.Row) (types.ClientSession, error) {
		var c types.ClientSession
		return c, r.Scan(&c.Label, &c.ClientAddr)
	})
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, c := range all {
		if !wardydLabelAtLeast0102(c.Label) {
			out = append(out, c)
		}
	}
	return out, nil
}

// The two label shapes a 0.10.2 wardynd sets as application_name (db.WithApplicationName): the server's
// wardynd/<version>/<replica> and a subcommand's wardynd-<tool>/<version>. Anything else is not one, however
// close: a label with no replica, with an extra component or with another tool shape is suspect.
var (
	serverLabelRE = regexp.MustCompile(`^wardynd/(` + labelVersion + `)/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	toolLabelRE   = regexp.MustCompile(`^wardynd-[a-z0-9][a-z0-9-]*/(` + labelVersion + `)$`)
)

// labelVersion is a version as internal/version.Version spells it: three numbers and an optional prerelease.
const labelVersion = `[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?`

// wardydLabelAtLeast0102 reports whether label is exactly one of the two shapes above with a version of
// 0.10.2 or later. A prerelease of 0.10.2 counts as 0.10.2.
func wardydLabelAtLeast0102(label string) bool {
	m := serverLabelRE.FindStringSubmatch(label)
	if m == nil {
		m = toolLabelRE.FindStringSubmatch(label)
	}
	if m == nil {
		return false
	}
	version, _, _ := strings.Cut(m[1], "-")
	var n [3]int
	for i, p := range strings.Split(version, ".") {
		v, err := strconv.Atoi(p)
		if err != nil {
			return false
		}
		n[i] = v
	}
	switch {
	case n[0] != 0:
		return n[0] > 0
	case n[1] != 10:
		return n[1] > 10
	default:
		return n[2] >= 2
	}
}
