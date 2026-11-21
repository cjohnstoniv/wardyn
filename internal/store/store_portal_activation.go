// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The one-way switch that turns issuer portals on, and the sample of other database clients that can veto it.
//
// The mark is a store-only key of the site_config document, like the service grants' keys. Neither
// types.SiteConfig nor the document the older server writes declares it, so every whole-document save keeps it
// (PutSiteConfigQ keeps any key it does not declare) and a request body that names it is refused. Only
// ActivatePortalIssuersAs writes it, and nothing clears it. Its sibling key, PortalActivationKey, holds the
// audit record the same way.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// PortalIssuersActivatedKey is the site_config document key that holds the time issuer portals were switched
// on. It is exported for the write conditions of other lanes' statements, which refuse a write that would
// need the activation before it is set.
const PortalIssuersActivatedKey = "portal_issuers_activated_at"

// PortalSessionUnknown is the label and address UntaggedClientSessions reports for a client session whose
// application name the calling role cannot read (NULL).
const PortalSessionUnknown = "unknown"

// PortalActivationKey is the sibling store-only key that holds who switched issuer portals on, the sessions
// they overrode and the two audit events' fixed ids and time, with audited_at empty until both events were
// delivered. It is written in the same statement as the mark, so a switch that is on always has its record.
const PortalActivationKey = "portal_issuers_activation"

// PortalActivation is the activation's audit record. The events' ids and time are fixed when the mark
// commits, so any replica can deliver them later and the audit table keeps one row each (WithAuditReplay).
// It is settled only once the audit table holds both events; settling drops everything but audited_at.
type PortalActivation struct {
	ActorType       string     `json:"actor_type"`
	Actor           string     `json:"actor"`
	Sessions        []string   `json:"sessions"`
	ActivateEventID uuid.UUID  `json:"activate_event_id"`
	OverrideEventID uuid.UUID  `json:"override_event_id"` // uuid.Nil when no session was overridden
	EventAt         time.Time  `json:"event_at"`
	AuditedAt       *time.Time `json:"audited_at"`
}

// PortalActivationStore is the activation's store surface, optional like the other portal stores.
type PortalActivationStore interface {
	PortalIssuersActivated(ctx context.Context) (bool, error)
	ActivatePortalIssuersAs(ctx context.Context, a PortalActivation) (activated bool, err error)
	PendingPortalActivation(ctx context.Context) (PortalActivation, bool, error)
	AuditEventsStored(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]time.Time, error)
	SettlePortalActivation(ctx context.Context) (bool, error)
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

// ActivatePortalIssuers is ActivatePortalIssuersAs for an actor the caller does not classify (a system
// actor); by and override name the actor and the sessions the operator overrode.
func (s PG) ActivatePortalIssuers(ctx context.Context, by string, override []string) (activated bool, err error) {
	return s.ActivatePortalIssuersAs(ctx, PortalActivation{ActorType: string(types.ActorSystem), Actor: by, Sessions: override})
}

// ActivatePortalIssuersAs sets the activation mark and the audit record beside it in one statement: it
// creates the site_config row when none is stored (an UPDATE alone would report success and store nothing),
// and on a row that exists sets both keys only when the mark is absent, so a second activation keeps the
// first one's time and record. Nothing clears them.
//
// activated is true only when this call set the mark, so of two concurrent activations exactly one is told it
// did and its record is the stored one. a carries the actor and the overridden sessions; the event ids and
// time are fixed here, and the caller delivers the rows (PendingPortalActivation, AuditEventsStored, SettlePortalActivation).
func (s PG) ActivatePortalIssuersAs(ctx context.Context, a PortalActivation) (activated bool, err error) {
	a.EventAt = time.Now().UTC().Truncate(time.Microsecond)
	a.ActivateEventID, a.OverrideEventID, a.AuditedAt = uuid.New(), uuid.Nil, nil
	if len(a.Sessions) > 0 {
		a.OverrideEventID = uuid.New()
	} else {
		a.Sessions = []string{}
	}
	rec, err := json.Marshal(a)
	if err != nil {
		return false, fmt.Errorf("store: encode portal activation: %w", err)
	}
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO site_config (singleton, config, updated_at)
		VALUES (true, jsonb_build_object($1::text, $2::text, $3::text, $4::text::jsonb), now())
		ON CONFLICT (singleton) DO UPDATE
			SET config = site_config.config || EXCLUDED.config, updated_at = now()
			WHERE NOT (site_config.config ? $1::text)`,
		PortalIssuersActivatedKey, a.EventAt.Format("2006-01-02T15:04:05.000000Z"), PortalActivationKey, string(rec))
	if err != nil {
		return false, fmt.Errorf("store: activate portal issuers: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// PendingPortalActivation is the committed activation whose events are not yet settled. ok is false
// when portals are off, when the record is delivered, and when the mark predates the record (an older
// server's mark has none and owes nothing).
func (s PG) PendingPortalActivation(ctx context.Context) (a PortalActivation, ok bool, err error) {
	var raw []byte
	err = s.Pool.QueryRow(ctx, `
		SELECT config -> $1::text FROM site_config
		 WHERE singleton AND config ? $2::text AND config -> $1::text ->> 'activate_event_id' IS NOT NULL
		   AND config -> $1::text ->> 'audited_at' IS NULL`, PortalActivationKey, PortalIssuersActivatedKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return PortalActivation{}, false, nil
	}
	if err != nil {
		return PortalActivation{}, false, fmt.Errorf("store: read pending portal activation: %w", err)
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return PortalActivation{}, false, fmt.Errorf("store: decode portal activation: %w", err)
	}
	return a, true, nil
}

// portalActivationRowSQL is the audit_events row of the record's event, found by the id and time the winning
// write fixed. The audit table is the only receipt: a local spool line, a recorder that said it accepted the
// event, and a replay that was a no-op are not.
func portalActivationRowSQL(idKey, action string) string {
	return `EXISTS (SELECT 1 FROM audit_events e WHERE e.id = (config -> $1::text ->> '` + idKey + `')::uuid
		AND e."time" = (config -> $1::text ->> 'event_at')::timestamptz AND e.action = '` + action + `')`
}

// SettlePortalActivation settles the activation's record, and reports whether this call did. The caller has
// already read both events with AuditEventsStored; the statement checks again, so it settles only when it finds the activate event, and the override event when sessions were overridden, in
// the audit table with the id and time the record fixed. Settling replaces the record with a delivery marker
// that holds the time only: the operator's principal, the sessions and the event ids leave the document in
// the same statement, so nothing in it can attribute the audit rows once their subject's key is erased.
func (s PG) SettlePortalActivation(ctx context.Context) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE site_config
		   SET config = jsonb_set(config, ARRAY[$1::text], jsonb_build_object('audited_at', to_jsonb(clock_timestamp())))
		 WHERE singleton AND config -> $1::text ->> 'activate_event_id' IS NOT NULL
		   AND config -> $1::text ->> 'audited_at' IS NULL
		   AND `+portalActivationRowSQL("activate_event_id", "portal_issuers.activate")+`
		   AND (jsonb_array_length(config -> $1::text -> 'sessions') = 0
		        OR `+portalActivationRowSQL("override_event_id", "portal_issuers.activate_override")+`)`, PortalActivationKey)
	if err != nil {
		return false, fmt.Errorf("store: settle portal activation: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// UntaggedClientSessions is every other client session on this database that does not carry a wardynd label of
// at least 0.10.2: an empty label, an older version, any other label, or a session whose label comes back
// NULL. It reads pg_stat_activity, so it sees only the connections that exist when it runs, and a label is
// only what the client chose; the activation's veto and the hourly alarm are a report, not a proof.
//
// A role without pg_read_all_stats sees another role's session with its backend type and address masked, but
// still reads its application name (PostgreSQL fills it in before its statistics-privilege check). Such a
// session is still a client and is judged by that label, reported with no address when it is not a current
// wardynd label. Only a session whose label comes back NULL is reported with the label and address "unknown"
// (PortalSessionUnknown); a client that sets no application name has an empty label and is reported with it.
func (s PG) UntaggedClientSessions(ctx context.Context) ([]types.ClientSession, error) {
	all, err := collect(ctx, s.Pool, "list", "client sessions", `
		SELECT COALESCE(application_name, $1), CASE WHEN application_name IS NULL THEN $1 ELSE COALESCE(host(client_addr), '') END
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

// labelVersion is a version as internal/version.Version spells it, by the SemVer 2.0 grammar: three numbers
// with no leading zeros and an optional prerelease of dot-separated identifiers, none empty, a numeric one
// with no leading zero.
const labelVersion = semverNum + `\.` + semverNum + `\.` + semverNum + `(?:-` + semverPre + `(?:\.` + semverPre + `)*)?`

const (
	semverNum = `(?:0|[1-9][0-9]*)`
	semverPre = `(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)`
)

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
