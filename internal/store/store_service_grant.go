// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A provider's service grant is stored apart from the provider rows, under two top-level keys of the
// site_config document that types.SiteConfig does not declare. A 0.10.1 server reads the document into its own
// type, which has no service_grant, and writes every key that type declares back; the merge in
// PutSiteConfigGrantsQ keeps every other top-level key, so a grant nested in a provider row would vanish at an
// old server's unrelated save, and one under its own key does not.
const (
	// ServiceGrantsKey holds the grants, an object keyed by the provider's uid.
	ServiceGrantsKey = "provider_service_grants"
	// ServiceGrantCleanupsKey holds the claims on the uids whose secret record is still to be deleted.
	ServiceGrantCleanupsKey = "provider_service_grant_cleanups"
	// ServiceGrantLiveWindow is how recent a replica's seen_at must be for the replica to count as running.
	ServiceGrantLiveWindow = 3 * time.Minute
	// ServiceGrantStatusKeep is how long a replica's row survives without being seen.
	ServiceGrantStatusKeep = 24 * time.Hour
)

// ErrServiceGrantsNotActivated is PutSiteConfigGrantsQ's answer when the document would carry a grant and
// the stored document does not yet hold the activation mark: nothing was written.
var ErrServiceGrantsNotActivated = errors.New("store: service grants are not activated")

// ErrServiceGrantCommitUnknown: COMMIT was sent and its answer was an error, so whether the row landed is unknown.
var ErrServiceGrantCommitUnknown = errors.New("store: the commit's outcome is unknown")

// ServiceGrantSecretRow names the secrets row whose value_rev is a service grant secret's revision.
type ServiceGrantSecretRow struct{ Owner, Name string }

// ServiceGrantPublish is one minted token's publication: the rows it share-locks and the decision made under them.
type ServiceGrantPublish struct {
	Secret *ServiceGrantSecretRow // nil: the store keeps no revision row; Decide gets secretRev nil
	// Decide runs inside the transaction once both rows are share-locked and read. It must do no I/O. It returns the
	// one row to write (already prepared) and whether that row publishes the token.
	Decide       func(sc types.SiteConfig, doc ServiceGrantDoc, secretRev *string) (ev types.AuditEvent, published bool)
	BeforeCommit func() // test seam: runs after the row is written and before COMMIT; nil in production
}

// ServiceGrantCleanup is a claim on one retired provider uid: its secret record is to be deleted. Revision is
// the record's revision when the claim was written, "" when the secret store keeps none. ID names this claim
// alone: a later claim on the same uid has another, so a settlement can tell its own claim from a replacement.
type ServiceGrantCleanup struct {
	ID         string    `json:"id"`
	ProviderID string    `json:"provider_id"`
	Reason     string    `json:"reason"`
	Revision   string    `json:"revision,omitempty"`
	Since      time.Time `json:"since"`
}

// ServiceGrantChange is what a provider write adds to the stored grant keys: the uids whose grant is dropped,
// and the clean-up claims written in the same statement. Claims are only ever added by a write.
type ServiceGrantChange struct {
	Drop   []string
	Claims map[string]ServiceGrantCleanup
}

// ServiceGrantDoc is the stored document's service-grant state as the document holds it, inert entries
// included: the grants by uid, the claims by uid, and whether the activation mark is set.
type ServiceGrantDoc struct {
	Grants    map[string]types.ProviderServiceGrant
	Cleanups  map[string]ServiceGrantCleanup
	Activated bool
}

// ServiceGrantStatusRow is one replica's row of provider_service_grant_status. Live is the database's
// reading of seen_at, filled on a read.
type ServiceGrantStatusRow struct {
	ProviderUID, ReplicaID, ReplicaHost, GrantDigest, ClientID string
	Outcome, Reason, OAuthError, Egress                        string
	HTTPStatus                                                 int
	AttemptedAt                                                time.Time
	LastOKAt, ValidUntil                                       *time.Time
	Live                                                       bool
}

// ServiceGrantStore is the optional seam over the grant keys and the status table. Callers type-assert it,
// as for the other optional stores; a store without it carries no grant.
type ServiceGrantStore interface {
	// PutSiteConfigGrants is PutSiteConfig with the grant change: it lifts each row's grant out into the
	// stored key, drops change.Drop, adds change.Claims, and answers ErrServiceGrantsNotActivated, writing
	// nothing, when it would lift a grant before the activation mark is stored.
	PutSiteConfigGrants(ctx context.Context, cfg types.SiteConfig, change ServiceGrantChange) (types.SiteConfig, error)
	// ServiceGrantDoc reads the stored grants, claims and mark, whatever rows exist.
	ServiceGrantDoc(ctx context.Context) (ServiceGrantDoc, error)
	// SettleServiceGrantCleanup removes the claim on uid when it still has this id, and reports whether this call
	// removed it, so only the attempt that did writes the settlement's audit row. A replacement claim on the
	// same uid, or an absent one, is left alone. The caller holds the site-config document lock.
	SettleServiceGrantCleanup(ctx context.Context, uid, claimID string) (bool, error)
	UpsertServiceGrantStatus(ctx context.Context, row ServiceGrantStatusRow) error
	TouchServiceGrantReplica(ctx context.Context, replicaID string) error
	ServiceGrantStatuses(ctx context.Context, uids []string) ([]ServiceGrantStatusRow, error)
	SweepServiceGrantStatuses(ctx context.Context) (int64, error)
	// PublishServiceGrantMint commits one minted token's mint row with the decision that the token is current. See PG's.
	PublishServiceGrantMint(ctx context.Context, p ServiceGrantPublish) (ev types.AuditEvent, published bool, seq int64, err error)
	// SettleServiceGrantMint records ev unless a row with its id and time exists, and returns the row the database holds.
	SettleServiceGrantMint(ctx context.Context, ev types.AuditEvent) (types.AuditEvent, error)
	// ServiceGrantSIEM is the sink that rows written past the recorder chain go to after their commit; nil for none.
	ServiceGrantSIEM() audit.Sink
}

var _ ServiceGrantStore = PG{}

// liftServiceGrants returns cfg with every row's grant removed, and the grants by uid. cfg itself, its
// provider slice and its oidc blocks are not changed.
func liftServiceGrants(cfg types.SiteConfig) (types.SiteConfig, map[string]types.ProviderServiceGrant, error) {
	lifted := map[string]types.ProviderServiceGrant{}
	if cfg.ModelProviders == nil {
		return cfg, lifted, nil
	}
	block := *cfg.ModelProviders
	block.Providers = slices.Clone(block.Providers)
	for i := range block.Providers {
		o := block.Providers[i].OIDC
		if o == nil || o.ServiceGrant == nil {
			continue
		}
		uid := block.Providers[i].UID
		if uid == "" {
			return cfg, nil, fmt.Errorf("store: provider %q has a service grant and no uid", block.Providers[i].ID)
		}
		lifted[uid] = *o.ServiceGrant
		stripped := *o
		stripped.ServiceGrant = nil
		block.Providers[i].OIDC = &stripped
	}
	cfg.ModelProviders = &block
	return cfg, lifted, nil
}

// attachServiceGrants sets each grant on the row with its uid, only when that row still has an oidc block;
// an entry with no such row is inert and attaches nothing.
func attachServiceGrants(cfg *types.SiteConfig, grants map[string]types.ProviderServiceGrant) {
	if cfg.ModelProviders == nil || len(grants) == 0 {
		return
	}
	for i := range cfg.ModelProviders.Providers {
		p := &cfg.ModelProviders.Providers[i]
		g, ok := grants[p.UID]
		if !ok || p.OIDC == nil || p.UID == "" {
			continue
		}
		g.ServiceIdentities = slices.Clone(g.ServiceIdentities)
		p.OIDC.ServiceGrant = &g
	}
}

// storedServiceGrants reads the grants key out of a stored document.
func storedServiceGrants(raw []byte) (map[string]types.ProviderServiceGrant, error) {
	if !bytes.Contains(raw, []byte(`"`+ServiceGrantsKey+`"`)) {
		return nil, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("store: unmarshal site config keys: %w", err)
	}
	var grants map[string]types.ProviderServiceGrant
	if v, ok := doc[ServiceGrantsKey]; ok {
		if err := json.Unmarshal(v, &grants); err != nil {
			return nil, fmt.Errorf("store: unmarshal %s: %w", ServiceGrantsKey, err)
		}
	}
	return grants, nil
}

// PutSiteConfigGrants is PutSiteConfig with a grant change (ServiceGrantStore).
func (s PG) PutSiteConfigGrants(ctx context.Context, cfg types.SiteConfig, change ServiceGrantChange) (types.SiteConfig, error) {
	return PutSiteConfigGrantsQ(ctx, s.Pool, cfg, change)
}

// putSiteConfigGrantsStmt: $1 the document without the grants, $2 the keys this binary declares, $3 the uids
// to drop, $4 the lifted grants, $5 how many were lifted, $6 and $7 the two keys, $8 the activation mark's
// key, $9 the claims to add. The refusal is a condition of this one statement: with no stored row a lifted
// grant inserts nothing, and on a stored row the update is skipped unless the mark is stored. Neither key is
// written while it is absent and has nothing to hold.
const putSiteConfigGrantsStmt = `
	INSERT INTO site_config (singleton, config, updated_at)
	SELECT true,
	       $1::jsonb || CASE WHEN $9::jsonb <> '{}'::jsonb THEN jsonb_build_object($7::text, $9::jsonb) ELSE '{}'::jsonb END,
	       now()
	 WHERE $5::int = 0 OR EXISTS (SELECT 1 FROM site_config)
	ON CONFLICT (singleton) DO UPDATE
		SET config = ((site_config.config - $2::text[] - $6::text - $7::text) || EXCLUDED.config)
		    || CASE WHEN jsonb_exists(site_config.config, $6::text) OR $4::jsonb <> '{}'::jsonb
		            THEN jsonb_build_object($6::text, (COALESCE(site_config.config -> $6::text, '{}'::jsonb) - $3::text[]) || $4::jsonb)
		            ELSE '{}'::jsonb END
		    || CASE WHEN jsonb_exists(site_config.config, $7::text) OR $9::jsonb <> '{}'::jsonb
		            THEN jsonb_build_object($7::text, COALESCE(site_config.config -> $7::text, '{}'::jsonb) || $9::jsonb)
		            ELSE '{}'::jsonb END,
		    updated_at = now()
		WHERE $5::int = 0 OR jsonb_exists(site_config.config, $8::text)
	RETURNING config`

// PutSiteConfigGrantsQ is PutSiteConfigQ with a grant change on q. It lifts each row's grant into the stored
// key as the stored entries less change.Drop, then the lifted ones over them, and adds change.Claims to the
// claims key, which it never shrinks.
func PutSiteConfigGrantsQ(ctx context.Context, q Querier, cfg types.SiteConfig, change ServiceGrantChange) (types.SiteConfig, error) {
	stripped, lifted, err := liftServiceGrants(cfg)
	if err != nil {
		return types.SiteConfig{}, err
	}
	raw, err := json.Marshal(stripped)
	if err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: marshal site config: %w", err)
	}
	liftedRaw, err := json.Marshal(lifted)
	if err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: marshal service grants: %w", err)
	}
	claims := change.Claims
	if claims == nil {
		claims = map[string]ServiceGrantCleanup{}
	}
	claimsRaw, err := json.Marshal(claims)
	if err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: marshal service grant cleanups: %w", err)
	}
	drop := slices.Clone(change.Drop)
	if drop == nil {
		drop = []string{}
	}
	var out []byte
	err = q.QueryRow(ctx, putSiteConfigGrantsStmt, raw, declaredJSONKeys(stripped), drop, liftedRaw, len(lifted),
		ServiceGrantsKey, ServiceGrantCleanupsKey, PortalIssuersActivatedKey, claimsRaw).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.SiteConfig{}, ErrServiceGrantsNotActivated
	}
	if err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: put site config: %w", err)
	}
	var saved types.SiteConfig
	if err := json.Unmarshal(out, &saved); err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: unmarshal saved site config: %w", err)
	}
	grants, err := storedServiceGrants(out)
	if err != nil {
		return types.SiteConfig{}, err
	}
	attachServiceGrants(&saved, grants)
	return saved, nil
}

// ServiceGrantDoc reads the stored grant state (ServiceGrantStore).
func (s PG) ServiceGrantDoc(ctx context.Context) (ServiceGrantDoc, error) {
	return ServiceGrantDocQ(ctx, s.Pool)
}

// ServiceGrantDocQ is ServiceGrantDoc on q. A deployment with no stored document has none of the three.
func ServiceGrantDocQ(ctx context.Context, q Querier) (ServiceGrantDoc, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT config FROM site_config WHERE singleton`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ServiceGrantDoc{}, nil
	}
	if err != nil {
		return ServiceGrantDoc{}, fmt.Errorf("store: get service grants: %w", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return ServiceGrantDoc{}, fmt.Errorf("store: unmarshal site config keys: %w", err)
	}
	var doc ServiceGrantDoc
	if v, ok := keys[ServiceGrantsKey]; ok {
		if err := json.Unmarshal(v, &doc.Grants); err != nil {
			return ServiceGrantDoc{}, fmt.Errorf("store: unmarshal %s: %w", ServiceGrantsKey, err)
		}
	}
	if v, ok := keys[ServiceGrantCleanupsKey]; ok {
		if err := json.Unmarshal(v, &doc.Cleanups); err != nil {
			return ServiceGrantDoc{}, fmt.Errorf("store: unmarshal %s: %w", ServiceGrantCleanupsKey, err)
		}
	}
	_, doc.Activated = keys[PortalIssuersActivatedKey]
	return doc, nil
}

// SettleServiceGrantCleanup removes the claim on uid (ServiceGrantStore).
func (s PG) SettleServiceGrantCleanup(ctx context.Context, uid, claimID string) (bool, error) {
	return SettleServiceGrantCleanupQ(ctx, s.Pool, uid, claimID)
}

// SettleServiceGrantCleanupQ removes one uid's claim, and no other, from the stored claims, only while the
// stored claim still has claimID. The caller holds
// the site-config document lock; the claims are not part of the document an ETag covers, so no writer's
// If-Match fails for it.
func SettleServiceGrantCleanupQ(ctx context.Context, q Querier, uid, claimID string) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE site_config
		   SET config = config #- ARRAY[$1::text, $2::text], updated_at = now()
		 WHERE singleton AND config -> $1::text -> $2::text ->> 'id' = $3::text`, ServiceGrantCleanupsKey, uid, claimID)
	if err != nil {
		return false, fmt.Errorf("store: settle service grant cleanup: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// UpsertServiceGrantStatus writes one replica's outcome for one provider. An older attempt never replaces a
// newer one of the same replica; the last success and its expiry survive later failures.
func (s PG) UpsertServiceGrantStatus(ctx context.Context, r ServiceGrantStatusRow) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO provider_service_grant_status AS t
		       (provider_uid, replica_id, replica_host, grant_digest, client_id, outcome, reason, oauth_error,
		        http_status, attempted_at, last_ok_at, valid_until, egress, seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9::int, 0), $10, $11, $12, $13, clock_timestamp())
		ON CONFLICT (provider_uid, replica_id) DO UPDATE
		   SET replica_host = EXCLUDED.replica_host, grant_digest = EXCLUDED.grant_digest, client_id = EXCLUDED.client_id,
		       outcome = EXCLUDED.outcome, reason = EXCLUDED.reason, oauth_error = EXCLUDED.oauth_error,
		       http_status = EXCLUDED.http_status, attempted_at = EXCLUDED.attempted_at,
		       last_ok_at = COALESCE(EXCLUDED.last_ok_at, t.last_ok_at),
		       valid_until = COALESCE(EXCLUDED.valid_until, t.valid_until),
		       egress = EXCLUDED.egress, seen_at = clock_timestamp()
		 WHERE EXCLUDED.attempted_at >= t.attempted_at`,
		r.ProviderUID, r.ReplicaID, r.ReplicaHost, r.GrantDigest, r.ClientID, r.Outcome, r.Reason, r.OAuthError,
		r.HTTPStatus, r.AttemptedAt, r.LastOKAt, r.ValidUntil, r.Egress)
	if err != nil {
		return fmt.Errorf("store: upsert service grant status: %w", err)
	}
	return nil
}

// TouchServiceGrantReplica sets seen_at, on the database clock, on every row of one replica in one statement.
func (s PG) TouchServiceGrantReplica(ctx context.Context, replicaID string) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE provider_service_grant_status SET seen_at = clock_timestamp() WHERE replica_id = $1`, replicaID); err != nil {
		return fmt.Errorf("store: touch service grant replica: %w", err)
	}
	return nil
}

// ServiceGrantStatuses returns every replica's row for each of uids, with Live read on the database clock.
func (s PG) ServiceGrantStatuses(ctx context.Context, uids []string) ([]ServiceGrantStatusRow, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT provider_uid, replica_id, replica_host, grant_digest, client_id, outcome, reason, oauth_error,
		       COALESCE(http_status, 0), attempted_at, last_ok_at, valid_until, egress,
		       seen_at > clock_timestamp() - make_interval(secs => $2::float8)
		  FROM provider_service_grant_status
		 WHERE provider_uid = ANY($1::text[])
		 ORDER BY provider_uid, replica_host, replica_id`, uids, ServiceGrantLiveWindow.Seconds())
	if err != nil {
		return nil, fmt.Errorf("store: list service grant statuses: %w", err)
	}
	defer rows.Close()
	var out []ServiceGrantStatusRow
	for rows.Next() {
		var r ServiceGrantStatusRow
		if err := rows.Scan(&r.ProviderUID, &r.ReplicaID, &r.ReplicaHost, &r.GrantDigest, &r.ClientID, &r.Outcome,
			&r.Reason, &r.OAuthError, &r.HTTPStatus, &r.AttemptedAt, &r.LastOKAt, &r.ValidUntil, &r.Egress, &r.Live); err != nil {
			return nil, fmt.Errorf("store: scan service grant status: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SweepServiceGrantStatuses deletes rows whose replica has not been seen for ServiceGrantStatusKeep.
func (s PG) SweepServiceGrantStatuses(ctx context.Context) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM provider_service_grant_status WHERE seen_at < clock_timestamp() - make_interval(secs => $1::float8)`,
		ServiceGrantStatusKeep.Seconds())
	if err != nil {
		return 0, fmt.Errorf("store: sweep service grant statuses: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PublishServiceGrantMint writes one mint row and the decision it carries in one read-committed transaction:
// the secret's row (when p.Secret is set) and the site-config row are share-locked and read, p.Decide picks the
// row, and the row is appended to the audit chain before COMMIT. Lock order: the rows first, the chain lock last
// (inside InsertAuditEventTx). A write to either row therefore commits either before the share locks are granted,
// and Decide sees it, or after this transaction ends, and the decision stands for it. seq is the audit
// high-water mark this transaction left, so it orders publications in commit order. A Commit error is
// ErrServiceGrantCommitUnknown: the row may or may not have landed. No sink copy is sent here.
func (s PG) PublishServiceGrantMint(ctx context.Context, p ServiceGrantPublish) (types.AuditEvent, bool, int64, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.AuditEvent{}, false, 0, fmt.Errorf("store: begin service grant publication: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return types.AuditEvent{}, false, 0, fmt.Errorf("store: bound the publication's lock waits: %w", err)
	}
	var secretRev *string
	if p.Secret != nil {
		var rev string
		err := tx.QueryRow(ctx, `SELECT value_rev::text FROM secrets WHERE owned_by=$1 AND name=$2 FOR SHARE`,
			p.Secret.Owner, p.Secret.Name).Scan(&rev)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return types.AuditEvent{}, false, 0, fmt.Errorf("store: lock the service grant secret's row: %w", err)
		}
		secretRev = &rev
	}
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM site_config WHERE singleton FOR SHARE`).Scan(&one); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return types.AuditEvent{}, false, 0, fmt.Errorf("store: lock the site config row: %w", err)
	}
	sc, err := GetSiteConfigQ(ctx, tx)
	if err != nil {
		return types.AuditEvent{}, false, 0, err
	}
	doc, err := ServiceGrantDocQ(ctx, tx)
	if err != nil {
		return types.AuditEvent{}, false, 0, err
	}
	ev, published := p.Decide(sc, doc, secretRev)
	if err := InsertAuditEventTx(ctx, tx, &ev); err != nil {
		return types.AuditEvent{}, false, 0, err
	}
	seq, err := highWaterSeq(ctx, tx)
	if err != nil {
		return types.AuditEvent{}, false, 0, err
	}
	if p.BeforeCommit != nil {
		p.BeforeCommit()
	}
	if err := tx.Commit(ctx); err != nil {
		return types.AuditEvent{}, false, 0, fmt.Errorf("%w: %w", ErrServiceGrantCommitUnknown, err)
	}
	return ev, published, seq, nil
}

// SettleServiceGrantMint records a mint row that its publication did not commit, or whose COMMIT answer was lost.
// Under the chain lock every earlier writer has ended, so a row with ev's id and time is the canonical one and is
// returned as the database holds it, hashes included; otherwise ev is appended and returned with its hashes.
// Every return ends the transaction, so the chain lock is never left held.
func (s PG) SettleServiceGrantMint(ctx context.Context, ev types.AuditEvent) (types.AuditEvent, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.AuditEvent{}, fmt.Errorf("store: begin service grant settle: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return types.AuditEvent{}, fmt.Errorf("store: bound audit chain lock wait: %w", err)
	}
	if _, err := tx.Exec(ctx, lockAuditChainSQL, db.AuditChainLockKey); err != nil {
		return types.AuditEvent{}, fmt.Errorf("store: lock audit chain (waited up to %s): %w", db.AuditChainLockTimeout, err)
	}
	var held types.AuditEvent
	var actorType string
	var data []byte
	err = tx.QueryRow(ctx, `SELECT `+auditCols+`, COALESCE(prev_hash,''), COALESCE(row_hash,'')
		FROM audit_events WHERE time = $1 AND id = $2`, ev.Time, ev.ID).
		Scan(&held.ID, &held.Time, &held.RunID, &actorType, &held.Actor, &held.Action, &held.Target, &held.Outcome,
			&held.SourceIP, &data, &held.PrevHash, &held.RowHash)
	switch {
	case err == nil:
		held.ActorType = types.ActorType(actorType)
		if len(data) > 0 {
			held.Data = data
		}
		held.DeviceID = FederatedDeviceID(held)
		return held, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return types.AuditEvent{}, fmt.Errorf("store: look up a settled mint row: %w", err)
	}
	if err := InsertAuditEventTx(ctx, tx, &ev); err != nil {
		return types.AuditEvent{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return types.AuditEvent{}, fmt.Errorf("%w: %w", ErrServiceGrantCommitUnknown, err)
	}
	return ev, nil
}

// ServiceGrantSIEM is the sink mint rows go to after their commit (ServiceGrantStore).
func (s PG) ServiceGrantSIEM() audit.Sink { return s.SIEM }
