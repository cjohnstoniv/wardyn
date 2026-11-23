// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RunSecretFacts is the OPTIONAL store behind a snapshot's secrets check. Every fact it reads is a row committed
// before the secret it stands for can reach the sandbox: never a best-effort audit row, and never server config
// read at check time. A store without it cannot answer, and the check then refuses.
type RunSecretFacts interface {
	// ReadRunSecretFacts reads every fact of runID in one statement. harnessTask is the task a harness sign-in
	// run carries. Mints are read up to mintsUpTo, or all of them when it is nil. ErrNotFound when the run is
	// gone.
	ReadRunSecretFacts(ctx context.Context, runID uuid.UUID, harnessTask string, mintsUpTo *time.Time) (SecretFacts, error)
	// RecordRunDispatchSecretFacts writes the dispatch's row for runID. Each boolean is ORed into what is stored,
	// so a later write never clears one, and the jti is replaced.
	RecordRunDispatchSecretFacts(ctx context.Context, runID uuid.UUID, f DispatchSecretFacts) error
	// RecordRevivedSecretFacts updates runID's row for a revive: gitTokenResident is ORed in, the Bedrock fact
	// is kept, and the jti is replaced. A run with no row stays without one.
	RecordRevivedSecretFacts(ctx context.Context, runID uuid.UUID, gitTokenResident bool, jti string) error
}

var _ RunSecretFacts = PG{}

// DispatchSecretFacts is what a dispatch or a revive decided about two credentials that have no row of their
// own, and the id of the run token it gave the run's proxy.
type DispatchSecretFacts struct {
	// BedrockResident: the run's Bedrock lane is ready in a mode whose AWS role credentials live in the sandbox.
	BedrockResident bool
	// GitTokenResident: the run has a git_pat grant and its token is handed to the sandbox's helper.
	GitTokenResident bool
	// RecordedJTI is the jti of the run token the writer gave the proxy; "" when it could not be read.
	RecordedJTI string
}

// SecretComponent is one run_components row as the secrets check reads it.
type SecretComponent struct {
	// Org is an organisation's row (owner '').
	Org bool `json:"org"`
	// Cleared is a person's row whose definition an erasure cleared.
	Cleared bool `json:"cleared"`
	// Deliveries are the delivery modes of the definition's secrets, as stored.
	Deliveries []string `json:"modes"`
}

// SecretFacts is one run's facts as of the statement that read them.
type SecretFacts struct {
	// Now is the database's clock when the statement ran.
	Now time.Time
	// RunCreatedAt is the run row's created_at.
	RunCreatedAt time.Time
	// GrantKinds counts the run's grant rows by their spec's kind ("" for a row that names none).
	GrantKinds map[string]int
	// Components are the run's run_components rows, in order.
	Components []SecretComponent
	// TakeoverGiven: a human_takeover approval of the run was given (APPROVED, or a holder is set).
	TakeoverGiven bool
	// SupportSessionAccepted: a support grant of the run was accepted, whenever, ended or not.
	SupportSessionAccepted bool
	// SigninBegun: a desktop sign-in of the run began.
	SigninBegun bool
	// HarnessLogin: the run's task is the harness sign-in task.
	HarnessLogin bool
	// Dispatch is the run's run_dispatch_secret_facts row; nil when it has none.
	Dispatch *DispatchSecretFacts
	// NewestMints is the newest successful credential.mint of the run by its grant's kind ("" when the grant row
	// is gone), up to the asked time.
	NewestMints map[string]time.Time
	// AuditRowsDropped: an audit retention drop removed rows recorded at or after the run was created, so a mint
	// of the run may be gone.
	AuditRowsDropped bool
}

// The statement reads each source with its own subquery, so one table's error fails the whole read. Mints are
// matched to their grant row through the grant id their data names; a mint whose grant row is gone reads kind "".
const readRunSecretFactsSQL = `
SELECT now(), r.created_at, COALESCE(r.task = $2, false),
  COALESCE((SELECT jsonb_object_agg(g.k, g.n) FROM (
      SELECT COALESCE(cg.spec->>'kind', '') AS k, count(*) AS n FROM credential_grants cg WHERE cg.run_id = r.id GROUP BY 1
    ) g), '{}'::jsonb),
  COALESCE((SELECT jsonb_agg(jsonb_build_object(
      'org', COALESCE(rc.owner = '', false),
      'cleared', rc.owner IS NULL,
      'modes', COALESCE(jsonb_path_query_array(rc.definition, '$.secrets[*].delivery.mode'), '[]'::jsonb)
    ) ORDER BY rc.ordinal) FROM run_components rc WHERE rc.run_id = r.id), '[]'::jsonb),
  EXISTS (SELECT 1 FROM approvals ap WHERE ap.run_id = r.id AND ap.kind = 'human_takeover'
      AND (ap.state = 'APPROVED' OR ap.takeover_holder <> '')),
  EXISTS (SELECT 1 FROM run_support_grants sg WHERE sg.run_id = r.id AND sg.accepted_at IS NOT NULL),
  EXISTS (SELECT 1 FROM desktop_signins ds WHERE ds.run_id = r.id),
  f.run_id IS NOT NULL, COALESCE(f.bedrock_resident, false), COALESCE(f.git_token_resident, false),
  COALESCE(f.recorded_jti, ''),
  COALESCE((SELECT jsonb_object_agg(m.k, m.at) FROM (
      SELECT COALESCE(cg.spec->>'kind', '') AS k, max(e.recorded_at) AS at
        FROM audit_events e
        LEFT JOIN credential_grants cg ON cg.run_id = e.run_id AND cg.id::text = e.data->>'grant_id'
       WHERE e.run_id = r.id AND e.action = 'credential.mint' AND e.outcome = 'success'
         AND ($3::timestamptz IS NULL OR e.recorded_at <= $3)
       GROUP BY 1
    ) m), '{}'::jsonb),
  EXISTS (SELECT 1 FROM audit_chain_anchors an WHERE an.kind = 'drop' AND COALESCE(an.row_count, 1) > 0
      AND (an.recorded_hi IS NULL OR an.recorded_hi >= r.created_at))
FROM agent_runs r
LEFT JOIN run_dispatch_secret_facts f ON f.run_id = r.id
WHERE r.id = $1`

// ReadRunSecretFacts — see RunSecretFacts.
func (s PG) ReadRunSecretFacts(ctx context.Context, runID uuid.UUID, harnessTask string, mintsUpTo *time.Time) (SecretFacts, error) {
	var (
		out                  SecretFacts
		grants, comps, mints []byte
		hasRow, bedrock, git bool
		jti                  string
	)
	err := s.Pool.QueryRow(ctx, readRunSecretFactsSQL, runID, harnessTask, mintsUpTo).Scan(
		&out.Now, &out.RunCreatedAt, &out.HarnessLogin, &grants, &comps,
		&out.TakeoverGiven, &out.SupportSessionAccepted, &out.SigninBegun,
		&hasRow, &bedrock, &git, &jti, &mints, &out.AuditRowsDropped)
	if errors.Is(err, pgx.ErrNoRows) {
		return SecretFacts{}, ErrNotFound
	}
	if err != nil {
		return SecretFacts{}, fmt.Errorf("store: read run secret facts: %w", err)
	}
	if err := json.Unmarshal(grants, &out.GrantKinds); err != nil {
		return SecretFacts{}, fmt.Errorf("store: decode run grant kinds: %w", err)
	}
	if err := json.Unmarshal(comps, &out.Components); err != nil {
		return SecretFacts{}, fmt.Errorf("store: decode run components: %w", err)
	}
	if err := json.Unmarshal(mints, &out.NewestMints); err != nil {
		return SecretFacts{}, fmt.Errorf("store: decode run mints: %w", err)
	}
	if hasRow {
		out.Dispatch = &DispatchSecretFacts{BedrockResident: bedrock, GitTokenResident: git, RecordedJTI: jti}
	}
	return out, nil
}

// RecordRunDispatchSecretFacts — see RunSecretFacts.
func (s PG) RecordRunDispatchSecretFacts(ctx context.Context, runID uuid.UUID, f DispatchSecretFacts) error {
	const q = `INSERT INTO run_dispatch_secret_facts AS f (run_id, bedrock_resident, git_token_resident, recorded_jti)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (run_id) DO UPDATE SET
			bedrock_resident = f.bedrock_resident OR EXCLUDED.bedrock_resident,
			git_token_resident = f.git_token_resident OR EXCLUDED.git_token_resident,
			recorded_jti = EXCLUDED.recorded_jti,
			recorded_at = now()`
	if _, err := s.Pool.Exec(ctx, q, runID, f.BedrockResident, f.GitTokenResident, f.RecordedJTI); err != nil {
		return fmt.Errorf("store: record run dispatch secret facts: %w", err)
	}
	return nil
}

// RecordRevivedSecretFacts — see RunSecretFacts. An UPDATE only: a run whose dispatch wrote no row has no facts
// to carry, and a revive must not make it look as though it had.
func (s PG) RecordRevivedSecretFacts(ctx context.Context, runID uuid.UUID, gitTokenResident bool, jti string) error {
	const q = `UPDATE run_dispatch_secret_facts SET
			git_token_resident = git_token_resident OR $2,
			recorded_jti = $3,
			recorded_at = now()
		WHERE run_id = $1`
	if _, err := s.Pool.Exec(ctx, q, runID, gitTokenResident, jti); err != nil {
		return fmt.Errorf("store: record revived secret facts: %w", err)
	}
	return nil
}
