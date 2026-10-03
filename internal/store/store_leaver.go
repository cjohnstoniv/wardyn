// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The store half of leaver deprovisioning: the SCIM projection on an identity row, the suspension
// and reactivation transactions, and the durable step ledger (migration 0118).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrIdentityPurged is a reactivation's refusal: a purged identity is a permanent tombstone.
var ErrIdentityPurged = errors.New("store: identity purged")

// Job kinds and the one step name every kind shares.
const (
	JobKindSuspend = "suspend"
	// JobStepCutoff is step 1 of a suspension: the cutoff, the deactivation and the epoch bump.
	JobStepCutoff = "cutoff"
)

// IdentityUpdate is one SCIM write to an identity row. A projection update never touches a binding
// column; ExternalID is set only when the caller has already checked it against the binding.
type IdentityUpdate struct {
	ExternalID string
	UserName   string
	Emails     []string
	// Reactivate clears the deactivation and the purge schedule on the identity and on every row
	// bound to the same principal. A purged identity is ErrIdentityPurged.
	Reactivate bool
	// PeoplePrincipals are the people rows whose deactivation a reactivation clears.
	PeoplePrincipals []string
}

// SuspendPlan is what step 1 of a suspension writes, all in one transaction.
type SuspendPlan struct {
	IdentityID uuid.UUID
	// Principals are the principals whose identity rows are deactivated with IdentityID's and whose
	// people rows get deactivated_at.
	Principals []string
	// CutoffSubs are every sub and email form a session cutoff is written for.
	CutoffSubs []string
	// PurgeAfter schedules the automatic purge that long after the deactivation. Zero schedules
	// none; an earlier schedule is kept.
	PurgeAfter time.Duration
}

// SuspendResult is step 1's outcome. WasActive says the identity was active before it, so this
// was a new suspension and not the repair of an old one.
type SuspendResult struct {
	WasActive bool
	Epoch     int64
}

// DeprovisionJob is one row of the step ledger.
type DeprovisionJob struct {
	IdentityID uuid.UUID
	Kind       string
	Step       string
	Target     string
	Done       bool
	Attempts   int
	LastError  string
	Detail     map[string]int
}

// JobKey names a ledger row within one identity and kind.
type JobKey struct{ Step, Target string }

// LeaverStore is optional, like PrincipalIdentityStore: callers type-assert it.
type LeaverStore interface {
	GetIdentity(ctx context.Context, id uuid.UUID) (PrincipalIdentity, error)
	SearchIdentities(ctx context.Context, issuer, attr, value string, limit int) ([]PrincipalIdentity, error)
	IdentitiesByAlias(ctx context.Context, issuer, valueLower string) ([]PrincipalIdentity, error)
	IdentityAliasValues(ctx context.Context, id uuid.UUID) ([]string, error)
	CreateScimIdentity(ctx context.Context, issuer, tenantID, objectID string, u IdentityUpdate, now time.Time) (PrincipalIdentity, bool, error)
	ApplyIdentityUpdate(ctx context.Context, id uuid.UUID, u IdentityUpdate, now time.Time) (PrincipalIdentity, error)
	SuspendIdentity(ctx context.Context, p SuspendPlan) (SuspendResult, error)
	PrincipalsByEmail(ctx context.Context, emails []string) ([]string, error)
	ListNonTerminalRunsBy(ctx context.Context, createdBy string) ([]types.AgentRun, error)
	EnsureDeprovisionJobs(ctx context.Context, identityID uuid.UUID, kind string, keys []JobKey) error
	ListDeprovisionJobs(ctx context.Context, identityID uuid.UUID, kind string) ([]DeprovisionJob, error)
	FinishDeprovisionJob(ctx context.Context, identityID uuid.UUID, kind string, k JobKey, detail map[string]int) error
	FailDeprovisionJob(ctx context.Context, identityID uuid.UUID, kind string, k JobKey, cause error) error
	ReopenDeprovisionJob(ctx context.Context, identityID uuid.UUID, kind string, k JobKey) error
	MarkIdentityPurged(ctx context.Context, id uuid.UUID, requireDue bool) (bool, error)
	PurgeDueIdentities(ctx context.Context, limit int) ([]uuid.UUID, error)
	PendingLeavers(ctx context.Context, idleFor time.Duration, limit int) ([]PendingLeaver, error)
	DeleteUserSubjectRows(ctx context.Context, subjects []string) (grants, assignments int64, err error)
}

var _ LeaverStore = PG{}

// SearchIdentities attrs, as SCIM spells them.
const (
	SearchExternalID = "externalId"
	SearchUserName   = "userName"
	SearchEmail      = "emails.value"
)

// SearchIdentities is the identity rows a SCIM filter `attr eq value` selects under issuer, at most
// limit. externalId matches the SCIM projection or the bound object id; userName and emails.value
// match the projection, the login email and every alias the identity has been seen under.
func (s PG) SearchIdentities(ctx context.Context, issuer, attr, value string, limit int) ([]PrincipalIdentity, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	var where string
	switch attr {
	case SearchExternalID:
		where = `(lower(scim_external_id) = $2 OR object_id = $2)`
	case SearchUserName:
		where = `(lower(scim_user_name) = $2 OR EXISTS (SELECT 1 FROM principal_identity_aliases a
			WHERE a.identity_id = principal_identities.id AND a.value_lower = $2))`
	case SearchEmail:
		where = `(email_lower = $2 OR EXISTS (SELECT 1 FROM principal_identity_aliases a
			WHERE a.identity_id = principal_identities.id AND a.kind = 'email' AND a.value_lower = $2))`
	default:
		return nil, fmt.Errorf("store: search identities: unknown attribute %q", attr)
	}
	return collect(ctx, s.Pool, "search", "principal identities", `SELECT `+principalIdentityCols+`
		FROM principal_identities WHERE issuer = $1 AND `+where+` ORDER BY created_at, id LIMIT $3`,
		[]any{issuer, v, limit}, scanPrincipalIdentity)
}

// IdentitiesByAlias is the rows under issuer that have no object id and were seen under valueLower
// as an email or a userName: the removal-only link a SCIM user with no object-id match falls to.
func (s PG) IdentitiesByAlias(ctx context.Context, issuer, valueLower string) ([]PrincipalIdentity, error) {
	return collect(ctx, s.Pool, "list", "principal identities by alias", `SELECT `+principalIdentityCols+`
		FROM principal_identities WHERE issuer = $1 AND object_id = '' AND (email_lower = $2 OR EXISTS (
			SELECT 1 FROM principal_identity_aliases a WHERE a.identity_id = principal_identities.id AND a.value_lower = $2))
		ORDER BY created_at, id`, []any{issuer, valueLower}, scanPrincipalIdentity)
}

// GetIdentity is the row stored under id, or ErrNotFound.
func (s PG) GetIdentity(ctx context.Context, id uuid.UUID) (PrincipalIdentity, error) {
	return scanPrincipalIdentity(s.Pool.QueryRow(ctx, `SELECT `+principalIdentityCols+` FROM principal_identities WHERE id = $1`, id))
}

// IdentityAliasValues is every alias value (email and userName) the identity has been seen under,
// sorted.
func (s PG) IdentityAliasValues(ctx context.Context, id uuid.UUID) ([]string, error) {
	return collect(ctx, s.Pool, "list", "identity aliases", `SELECT DISTINCT value_lower FROM principal_identity_aliases
		WHERE identity_id = $1 ORDER BY value_lower`, []any{id}, func(r pgx.Row) (string, error) {
		var v string
		return v, r.Scan(&v)
	})
}

// CreateScimIdentity records a SCIM user as an identity row with no principal, keyed by the object
// id, or finds the row already holding it (created=false). The first sign-in binds the principal.
func (s PG) CreateScimIdentity(ctx context.Context, issuer, tenantID, objectID string, u IdentityUpdate, now time.Time) (PrincipalIdentity, bool, error) {
	if issuer == "" || objectID == "" {
		return PrincipalIdentity{}, false, errors.New("store: scim identity needs an issuer and an object id")
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return PrincipalIdentity{}, false, fmt.Errorf("store: create scim identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id uuid.UUID
	created := true
	err = tx.QueryRow(ctx, `
		INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, created_at)
		VALUES (NULL, $1, $2, $3, $4) ON CONFLICT DO NOTHING RETURNING id`, issuer, tenantID, objectID, now).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		created = false
		err = tx.QueryRow(ctx, `SELECT id FROM principal_identities WHERE issuer = $1 AND tenant_id = $2 AND object_id = $3`,
			issuer, tenantID, objectID).Scan(&id)
	}
	if err != nil {
		return PrincipalIdentity{}, false, fmt.Errorf("store: create scim identity: %w", err)
	}
	out, err := applyIdentityUpdateTx(ctx, tx, id, u, now)
	if err != nil {
		return PrincipalIdentity{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return PrincipalIdentity{}, false, fmt.Errorf("store: commit scim identity: %w", err)
	}
	return out, created, nil
}

// ApplyIdentityUpdate applies u to the identity in one transaction: the projection, then the
// reactivation when asked. A purged identity refuses a reactivation and nothing else changes.
func (s PG) ApplyIdentityUpdate(ctx context.Context, id uuid.UUID, u IdentityUpdate, now time.Time) (PrincipalIdentity, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return PrincipalIdentity{}, fmt.Errorf("store: update identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, err := applyIdentityUpdateTx(ctx, tx, id, u, now)
	if err != nil {
		return PrincipalIdentity{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return PrincipalIdentity{}, fmt.Errorf("store: commit identity update: %w", err)
	}
	return out, nil
}

func applyIdentityUpdateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, u IdentityUpdate, now time.Time) (PrincipalIdentity, error) {
	row, err := scanPrincipalIdentity(tx.QueryRow(ctx, `SELECT `+principalIdentityCols+` FROM principal_identities WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return PrincipalIdentity{}, err
	}
	if u.Reactivate && row.PurgedAt != nil {
		return PrincipalIdentity{}, ErrIdentityPurged
	}
	if u.ExternalID != "" {
		if _, err = tx.Exec(ctx, `UPDATE principal_identities SET scim_external_id = $2 WHERE id = $1`, id, u.ExternalID); err != nil {
			return PrincipalIdentity{}, fmt.Errorf("store: set scim external id: %w", err)
		}
	}
	if name := strings.ToLower(strings.TrimSpace(u.UserName)); name != "" {
		if _, err = tx.Exec(ctx, `UPDATE principal_identities SET scim_user_name = $2 WHERE id = $1`, id, strings.TrimSpace(u.UserName)); err != nil {
			return PrincipalIdentity{}, fmt.Errorf("store: set scim user name: %w", err)
		}
		if err = upsertScimAlias(ctx, tx, id, "user_name", name, now); err != nil {
			return PrincipalIdentity{}, err
		}
	}
	for _, e := range u.Emails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if err = upsertScimAlias(ctx, tx, id, "email", e, now); err != nil {
			return PrincipalIdentity{}, err
		}
		if _, err = tx.Exec(ctx, `UPDATE principal_identities SET email_lower = $2 WHERE id = $1 AND email_lower = ''`, id, e); err != nil {
			return PrincipalIdentity{}, fmt.Errorf("store: set identity email: %w", err)
		}
	}
	if u.Reactivate {
		if _, err = tx.Exec(ctx, `
			UPDATE principal_identities SET deactivated_at = NULL, purge_after = NULL
			 WHERE purged_at IS NULL AND (id = $1 OR ($2 <> '' AND principal = $2))`, id, row.Principal); err != nil {
			return PrincipalIdentity{}, fmt.Errorf("store: reactivate identity: %w", err)
		}
		if _, err = tx.Exec(ctx, `UPDATE people SET deactivated_at = NULL WHERE principal = ANY($1::text[])`,
			nonEmptyStrings(append([]string{row.Principal}, u.PeoplePrincipals...))); err != nil {
			return PrincipalIdentity{}, fmt.Errorf("store: reactivate person: %w", err)
		}
	}
	return scanPrincipalIdentity(tx.QueryRow(ctx, `SELECT `+principalIdentityCols+` FROM principal_identities WHERE id = $1`, id))
}

func upsertScimAlias(ctx context.Context, tx pgx.Tx, id uuid.UUID, kind, value string, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO principal_identity_aliases (identity_id, kind, value_lower, source, first_seen, last_seen)
		VALUES ($1, $2, $3, 'scim', $4, $4)
		ON CONFLICT (identity_id, kind, value_lower) DO UPDATE SET last_seen = EXCLUDED.last_seen`, id, kind, value, now); err != nil {
		return fmt.Errorf("store: record %s alias: %w", kind, err)
	}
	return nil
}

func nonEmptyStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// SuspendIdentity is step 1 of a suspension, one transaction. It takes every row it will change
// FOR UPDATE, in id order (so a sign-in holding one of them finishes first or is refused), writes
// the session cutoff for every sub and email form, deactivates the identity rows and bumps their
// authority epoch, stamps the people rows, and records the step done. A suspension of an active
// identity first clears the ledger of any earlier one.
func (s PG) SuspendIdentity(ctx context.Context, p SuspendPlan) (SuspendResult, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return SuspendResult{}, fmt.Errorf("store: suspend identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	principals := nonEmptyStrings(p.Principals)
	rows, err := tx.Query(ctx, `
		SELECT id, deactivated_at IS NULL FROM principal_identities
		 WHERE id = $1 OR principal = ANY($2::text[]) ORDER BY id FOR UPDATE`, p.IdentityID, principals)
	if err != nil {
		return SuspendResult{}, fmt.Errorf("store: lock identities: %w", err)
	}
	var res SuspendResult
	found := false
	for rows.Next() {
		var id uuid.UUID
		var active bool
		if err = rows.Scan(&id, &active); err != nil {
			rows.Close()
			return SuspendResult{}, fmt.Errorf("store: lock identities: %w", err)
		}
		if id == p.IdentityID {
			found, res.WasActive = true, active
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return SuspendResult{}, fmt.Errorf("store: lock identities: %w", err)
	}
	if !found {
		return SuspendResult{}, ErrNotFound
	}
	// Stamped by the database at the statement, after the locks: a sign-in that held a row finished
	// before this reads the clock, so its cookie's issued-at is at or before the cutoff.
	if _, err = tx.Exec(ctx, `
		UPDATE principal_identities
		   SET deactivated_at = COALESCE(deactivated_at, clock_timestamp()), authority_epoch = authority_epoch + 1,
		       purge_after = COALESCE(purge_after, CASE WHEN $3::bigint > 0 THEN clock_timestamp() + $3::bigint * interval '1 microsecond' END)
		 WHERE id = $1 OR principal = ANY($2::text[])`, p.IdentityID, principals, p.PurgeAfter.Microseconds()); err != nil {
		return SuspendResult{}, fmt.Errorf("store: deactivate identities: %w", err)
	}
	for _, sub := range nonEmptyStrings(p.CutoffSubs) {
		if _, err = tx.Exec(ctx, `
			INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ($1, clock_timestamp())
			ON CONFLICT (sub) DO UPDATE SET revoked_at = EXCLUDED.revoked_at`, sub); err != nil {
			return SuspendResult{}, fmt.Errorf("store: session cutoff: %w", err)
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE people SET deactivated_at = COALESCE(deactivated_at, clock_timestamp()) WHERE principal = ANY($1::text[])`,
		principals); err != nil {
		return SuspendResult{}, fmt.Errorf("store: deactivate person: %w", err)
	}
	if res.WasActive {
		if _, err = tx.Exec(ctx, `DELETE FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2`, p.IdentityID, JobKindSuspend); err != nil {
			return SuspendResult{}, fmt.Errorf("store: reset deprovision jobs: %w", err)
		}
	}
	detail, _ := json.Marshal(map[string]int{"sessions_cut": len(nonEmptyStrings(p.CutoffSubs))})
	if _, err = tx.Exec(ctx, `
		INSERT INTO deprovision_jobs (identity_id, kind, step, target, state, attempts, detail)
		VALUES ($1, $2, $3, '', 'done', 1, $4)
		ON CONFLICT (identity_id, kind, step, target) DO UPDATE
		   SET state = 'done', attempts = deprovision_jobs.attempts + 1, detail = EXCLUDED.detail, updated_at = now()`,
		p.IdentityID, JobKindSuspend, JobStepCutoff, detail); err != nil {
		return SuspendResult{}, fmt.Errorf("store: record cutoff step: %w", err)
	}
	if err = tx.QueryRow(ctx, `SELECT authority_epoch FROM principal_identities WHERE id = $1`, p.IdentityID).Scan(&res.Epoch); err != nil {
		return SuspendResult{}, fmt.Errorf("store: read authority epoch: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return SuspendResult{}, fmt.Errorf("store: commit suspension: %w", err)
	}
	return res, nil
}

// PrincipalsByEmail is the distinct subs the emails resolve to through api_tokens(principal, email)
// and people, matched case-insensitively: how a suspension reaches someone no identity row binds.
func (s PG) PrincipalsByEmail(ctx context.Context, emails []string) ([]string, error) {
	lowered := make([]string, 0, len(emails))
	for _, e := range emails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			lowered = append(lowered, e)
		}
	}
	return collect(ctx, s.Pool, "list", "principals by email", `
		SELECT principal FROM (
			SELECT principal FROM api_tokens WHERE lower(email) = ANY($1::text[])
			UNION SELECT principal FROM people WHERE lower(email) = ANY($1::text[])
		) p WHERE principal <> '' ORDER BY principal`, []any{lowered}, func(r pgx.Row) (string, error) {
		var v string
		return v, r.Scan(&v)
	})
}

// ListNonTerminalRunsBy is createdBy's runs that have not ended.
func (s PG) ListNonTerminalRunsBy(ctx context.Context, createdBy string) ([]types.AgentRun, error) {
	return collect(ctx, s.Pool, "list", "runs by creator", `SELECT `+runCols+` FROM agent_runs
		WHERE created_by = $1 AND state = ANY($2) ORDER BY created_at, id`,
		[]any{createdBy, nonTerminalStateNames()}, scanRun)
}

// EnsureDeprovisionJobs adds each key as a pending ledger row, leaving rows that exist as they are.
func (s PG) EnsureDeprovisionJobs(ctx context.Context, identityID uuid.UUID, kind string, keys []JobKey) error {
	for _, k := range keys {
		if _, err := s.Pool.Exec(ctx, `
			INSERT INTO deprovision_jobs (identity_id, kind, step, target) VALUES ($1, $2, $3, $4)
			ON CONFLICT (identity_id, kind, step, target) DO NOTHING`, identityID, kind, k.Step, k.Target); err != nil {
			return fmt.Errorf("store: ensure deprovision job: %w", err)
		}
	}
	return nil
}

// ListDeprovisionJobs is the ledger of one identity and kind, oldest first.
func (s PG) ListDeprovisionJobs(ctx context.Context, identityID uuid.UUID, kind string) ([]DeprovisionJob, error) {
	return collect(ctx, s.Pool, "list", "deprovision jobs", `
		SELECT identity_id, kind, step, target, state = 'done', attempts, last_error, detail
		  FROM deprovision_jobs WHERE identity_id = $1 AND kind = $2 ORDER BY created_at, step, target`,
		[]any{identityID, kind}, func(r pgx.Row) (DeprovisionJob, error) {
			var j DeprovisionJob
			var raw []byte
			if err := r.Scan(&j.IdentityID, &j.Kind, &j.Step, &j.Target, &j.Done, &j.Attempts, &j.LastError, &raw); err != nil {
				return DeprovisionJob{}, err
			}
			_ = json.Unmarshal(raw, &j.Detail)
			return j, nil
		})
}

// FinishDeprovisionJob marks a step done, storing the counts it produced.
func (s PG) FinishDeprovisionJob(ctx context.Context, identityID uuid.UUID, kind string, k JobKey, detail map[string]int) error {
	if detail == nil {
		detail = map[string]int{}
	}
	raw, _ := json.Marshal(detail)
	if _, err := s.Pool.Exec(ctx, `
		UPDATE deprovision_jobs SET state = 'done', attempts = attempts + 1, last_error = '', detail = $5, updated_at = now()
		 WHERE identity_id = $1 AND kind = $2 AND step = $3 AND target = $4`, identityID, kind, k.Step, k.Target, raw); err != nil {
		return fmt.Errorf("store: finish deprovision job: %w", err)
	}
	return nil
}

// FailDeprovisionJob records a failed attempt and leaves the step pending.
func (s PG) FailDeprovisionJob(ctx context.Context, identityID uuid.UUID, kind string, k JobKey, cause error) error {
	msg := cause.Error()
	if len(msg) > 512 {
		msg = strings.ToValidUTF8(msg[:512], "")
	}
	if _, err := s.Pool.Exec(ctx, `
		UPDATE deprovision_jobs SET attempts = attempts + 1, last_error = $5, updated_at = now()
		 WHERE identity_id = $1 AND kind = $2 AND step = $3 AND target = $4`, identityID, kind, k.Step, k.Target, msg); err != nil {
		return fmt.Errorf("store: fail deprovision job: %w", err)
	}
	return nil
}

// ReopenDeprovisionJob makes a finished step pending again: a run the ledger counted done that is
// found live once more.
func (s PG) ReopenDeprovisionJob(ctx context.Context, identityID uuid.UUID, kind string, k JobKey) error {
	if _, err := s.Pool.Exec(ctx, `
		UPDATE deprovision_jobs SET state = 'pending', updated_at = now()
		 WHERE identity_id = $1 AND kind = $2 AND step = $3 AND target = $4`, identityID, kind, k.Step, k.Target); err != nil {
		return fmt.Errorf("store: reopen deprovision job: %w", err)
	}
	return nil
}
