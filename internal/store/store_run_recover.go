// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

const (
	// RecoveryLimit is how long after its expiry a run token may still be recovered; the owner revives after that.
	RecoveryLimit = 24 * time.Hour
	// OutageRecoveryGrace is how long after the window covering its expiry ends a run is held for its proxy to
	// recover (it asks about once a minute) before it is treated as gone.
	OutageRecoveryGrace = 25 * time.Minute
	// TokenCutPrefix marks a lineage cut at a stopped pause: token_jti holds TokenCutPrefix plus the pause generation,
	// which matches no real jti. A cut lineage is never recovered, held for recovery or stamped by the recovery door.
	TokenCutPrefix = "wardyn-cut:"
)

// TokenLineage is a run's token lineage as the recover door checks it. JTI is token_jti exactly as stored: a cut
// sentinel stays a cut sentinel and is never read as "" (an empty lineage accepts its presenter).
type TokenLineage struct {
	JTI, PrevJTI string    // "" when none is recorded
	RenewedAt    time.Time // token_renewed_at, the database's stamp
}

// RecoveryCheck is one run's recovery facts on the database's clock.
type RecoveryCheck struct {
	Held          bool // recoveryHeldExpr: the sweeps and the STATE judge leave the run unmarked
	Covered       bool // a recorded window covers its token's expiry (OutageCoversSQL), however long ago it ended
	StrictSpent   bool // strict terms, and the allowance from the last renew has run out
	EndUnattended bool // the terms end an unattended run after an outage
	Cut           bool // the lineage is cut (TokenCutPrefix): no recovery, no hold, no outage end; the caller's own action
}

// RunTokenRecoverer is the recovery door's and the sweeps' store.
type RunTokenRecoverer interface {
	// RunRecoveryLineage is id's lineage. Named apart from PAUSE-2's RunTokenLineage(ctx, id) (bound, jti, prev, err),
	// which a later lane adds to this package.
	RunRecoveryLineage(ctx context.Context, id uuid.UUID) (TokenLineage, error)
	// StampRunTokenRecovered records a recovery: the run must be RUNNING and not lost, and presented must be its
	// current token (or it has none recorded). The presented token leaves the lineage: token_prev_jti is cleared, so it
	// can never be presented again. False, with no error, when any condition fails.
	StampRunTokenRecovered(ctx context.Context, id uuid.UUID, jti, presented string) (bool, error)
	RunRecoveryCheck(ctx context.Context, id uuid.UUID, storeRecovering bool) (RecoveryCheck, error)
	SetRunProxyPauses(ctx context.Context, id uuid.UUID, pauses bool) error
}

var _ RunTokenRecoverer = PG{}

// storeRecoveringTerm is where recoveryHeldExpr puts its caller's storeRecovering expression.
const storeRecoveringTerm = "<store_recovering>"

// tokenExpirySQL is E, a run token's expiry: its database stamp plus the token's lifetime.
var tokenExpirySQL = fmt.Sprintf(`a.token_renewed_at + interval '%d seconds'`, int(identity.RunTokenTTL/time.Second))

// recoveryHeldSQL is the one hold predicate with every duration filled in at package init, over agent_runs a LEFT JOIN
// run_outage_terms t. A run is held while its proxy pauses, its lineage is not cut, its strict allowance (if strict)
// is not spent, its token expired under 24 h ago, its runner does not hold it contained, and an outage covers its
// expiry: the store tracker recovering, a recorded window that ended under OutageRecoveryGrace ago, or a heartbeat gap
// still open that began OutageCoverLead before the expiry (the cp_outage window the next beat writes).
var recoveryHeldSQL = fmt.Sprintf(`COALESCE((COALESCE(t.proxy_pauses, false)
	AND COALESCE(a.token_jti, '') NOT LIKE '%[1]s%%'
	AND (NOT t.strict OR clock_timestamp() <= a.token_renewed_at + make_interval(secs => t.allowance_s))
	AND clock_timestamp() <= a.token_renewed_at + interval '%[2]d seconds'
	AND NOT (%[3]s)
	AND (%[4]s
	     OR EXISTS (SELECT 1 FROM outage_windows w WHERE %[5]s
	        AND COALESCE(w.ended_at, clock_timestamp()) > clock_timestamp() - interval '%[6]d seconds')
	     OR EXISTS (SELECT 1 FROM cp_heartbeat hb WHERE hb.beat_at < clock_timestamp() - interval '%[7]d seconds'
	        AND hb.beat_at <= %[8]s - interval '%[9]d seconds'
	        AND clock_timestamp() >= %[8]s - interval '%[10]d seconds'))), false)`,
	TokenCutPrefix,
	int((identity.RunTokenTTL+RecoveryLimit)/time.Second),
	runnerHeldContainedSQL("a.id"),
	storeRecoveringTerm,
	OutageCoversSQL(tokenExpirySQL, "a.runner_id", "a.placement"),
	int(OutageRecoveryGrace/time.Second),
	int(ControlPlaneGapOver/time.Second),
	"("+tokenExpirySQL+")",
	int(OutageCoverLead/time.Second),
	int(OutageCoverSlack/time.Second))

// recoveryHeldExpr is the one hold predicate (recoveryHeldSQL), with storeRecovering the SQL boolean for "OUT-1's
// store tracker is recovering": a placeholder, or "false" where the tracker is not known (the STATE judge).
func recoveryHeldExpr(storeRecovering string) string {
	return strings.Replace(recoveryHeldSQL, storeRecoveringTerm, storeRecovering, 1)
}

func (s PG) RunRecoveryLineage(ctx context.Context, id uuid.UUID) (TokenLineage, error) {
	var l TokenLineage
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(token_jti, ''), COALESCE(token_prev_jti, ''), token_renewed_at FROM agent_runs WHERE id = $1`, id).
		Scan(&l.JTI, &l.PrevJTI, &l.RenewedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenLineage{}, ErrNotFound
	}
	if err != nil {
		return TokenLineage{}, fmt.Errorf("store: read run token lineage: %w", err)
	}
	return l, nil
}

// stampRunTokenRecoveredSQL is StampRunTokenRecovered's statement. Its cut term repeats what the equality already
// implies (a verified jti is never a sentinel), so the rule reads the same as the renew stamp's.
var stampRunTokenRecoveredSQL = `UPDATE agent_runs SET token_renewed_at = now(), token_jti = $2, token_prev_jti = NULL
	WHERE id = $1 AND state = 'RUNNING' AND lost_at IS NULL AND COALESCE(token_jti, '') NOT LIKE '` + TokenCutPrefix + `%'
	AND (COALESCE(token_jti, '') = '' OR token_jti = $3)`

func (s PG) StampRunTokenRecovered(ctx context.Context, id uuid.UUID, jti, presented string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, stampRunTokenRecoveredSQL, id, jti, presented)
	if err != nil {
		return false, fmt.Errorf("store: stamp run token recovered: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s PG) RunRecoveryCheck(ctx context.Context, id uuid.UUID, storeRecovering bool) (RecoveryCheck, error) {
	var c RecoveryCheck
	err := s.Pool.QueryRow(ctx, `SELECT `+recoveryHeldExpr("$2::boolean")+`,
		EXISTS (SELECT 1 FROM outage_windows w WHERE `+OutageCoversSQL(tokenExpirySQL, "a.runner_id", "a.placement")+`),
		COALESCE(t.strict AND clock_timestamp() > a.token_renewed_at + make_interval(secs => t.allowance_s), false),
		COALESCE(t.end_unattended, false),
		COALESCE(a.token_jti, '') LIKE '`+TokenCutPrefix+`%'
	FROM agent_runs a LEFT JOIN run_outage_terms t ON t.run_id = a.id WHERE a.id = $1`, id, storeRecovering).
		Scan(&c.Held, &c.Covered, &c.StrictSpent, &c.EndUnattended, &c.Cut)
	if errors.Is(err, pgx.ErrNoRows) {
		return RecoveryCheck{}, ErrNotFound
	}
	if err != nil {
		return RecoveryCheck{}, fmt.Errorf("store: read run recovery check: %w", err)
	}
	return c, nil
}

func (s PG) SetRunProxyPauses(ctx context.Context, id uuid.UUID, pauses bool) error {
	// The insert arm serves a run dispatched before outage terms were kept: its values are the defaults.
	_, err := s.Pool.Exec(ctx, `INSERT INTO run_outage_terms (run_id, allowance_s, strict, end_unattended, proxy_pauses) VALUES
		($1, 3600, false, false, $2) ON CONFLICT (run_id) DO UPDATE SET proxy_pauses = EXCLUDED.proxy_pauses`, id, pauses)
	if err != nil {
		return fmt.Errorf("store: record run proxy pauses: %w", err)
	}
	return nil
}
