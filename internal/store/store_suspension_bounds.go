// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// SuspensionBounds is what a SCIM suspension's tail reads to reach only what it owed: the database's own
// insert time of runs, API tokens and SSH keys (inserted_at, migration 0281), a column default the application
// never writes, so on the same clock as an identity's deactivated_at; whether a session cutoff of a sub was
// written at or after a given instant while the identity is active again; and a step finish that a later
// suspension's ledger refuses. Optional, like LeaverStore: callers type-assert it.
type SuspensionBounds interface {
	RunsInsertedAfter(ctx context.Context, identity uuid.UUID, ids []uuid.UUID, at time.Time) (map[uuid.UUID]bool, error)
	APITokensInsertedAfter(ctx context.Context, identity uuid.UUID, ids []uuid.UUID, at time.Time) (map[uuid.UUID]bool, error)
	DeleteSSHKeysUpTo(ctx context.Context, identity uuid.UUID, principal string, at time.Time) (int, error)
	SubCutSince(ctx context.Context, identity uuid.UUID, sub string, at time.Time) (bool, error)
	FinishSuspensionJobAt(ctx context.Context, identity uuid.UUID, k JobKey, detail map[string]int, at time.Time) error
}

// PortalSuspensionLedger is the ledger writes of a portal suspension's tail, each conditional in its own statement on
// the suspension still being the identity's (the test FinishSuspensionJobAt applies) and, where a row is named, on
// the row's own id (JobKey.ID), so a tail that read an earlier suspension's ledger changes nothing of a later one's
// and claims nothing of it. A write it may not make is ErrSuperseded.
type PortalSuspensionLedger interface {
	// EnsureSuspensionJobsAt adds the keys as pending rows, leaving rows that exist as they are.
	EnsureSuspensionJobsAt(ctx context.Context, identity uuid.UUID, keys []JobKey, at time.Time) error
	// ReopenSuspensionJobAt makes the named row pending again (its audit pin cleared).
	ReopenSuspensionJobAt(ctx context.Context, identity uuid.UUID, k JobKey, at time.Time) error
	// ClaimSuspensionJobAt is the check a tail makes before it acts outside the database for a row: the row is
	// still the one it read, still pending, under the current suspension.
	ClaimSuspensionJobAt(ctx context.Context, identity uuid.UUID, k JobKey, at time.Time) error
	// ClaimSuspensionKillAt is the run's KILLED transition, and the delete intent of a kept machine when kill
	// asks for one, made in one transaction that first locks the named row: pending, of the identity's current
	// suspension, with the id the tail read. A row it may not claim is ErrSuperseded and nothing is written, so a
	// replacement cannot land between the check and the transition, and the intent is the one of the claim that won.
	// A run leaving the queue leaves it in that transaction. A run already KILLED (a repair) is claimed with only its
	// updated_at stamped, and only once no other claim of the row is live (ErrKillHeld); the claim also pins the row's
	// receipt id and time, so every claim of one occurrence writes the same run.kill event.
	ClaimSuspensionKillAt(ctx context.Context, identity uuid.UUID, k JobKey, at time.Time, kill RunKill) (KillClaim, error)
	// ReleaseSuspensionKillAt ends the claim a failed teardown leaves on the row, so its retry need not wait out
	// the claim's lease, only when the row's claim is the one named (KillClaim.ClaimID): a worker that never
	// acquired a claim, or whose claim another worker took over after its lease, releases nothing. A row that is
	// gone or replaced is left alone.
	ReleaseSuspensionKillAt(ctx context.Context, identity uuid.UUID, k JobKey, claim uuid.UUID) error
}

// KillClaimLease is how long a claim of a suspension's kill row holds off another claim of the same occurrence: the
// longest a kill's teardown and receipt take, with room. A claim older than it is a worker that stopped.
const KillClaimLease = 2 * time.Minute

// ErrKillHeld is a kill claim refused because another worker's claim of the same occurrence is still live: it owns
// the teardown and the receipt, and a crash it leaves is repaired once the lease ends.
var ErrKillHeld = errors.New("store: another worker holds this suspension's kill of the run")

// KillClaim is what ClaimSuspensionKillAt returns: whether the claim is the caller's, a kept machine's delete intent
// it made, and the id and time of the one run.kill event the occurrence writes.
//
// ClaimID names this acquisition (RunKill.ClaimID): release and finish apply only while it is the row's claim.
// From is the state the run was killed from, the original claim's on a repair: a run that left the queue is
// repaired as one that left the queue (its receipt says so and its unstarted drive is dropped).
type KillClaim struct {
	Applied   bool
	Intent    KeptMachineDeletion
	ReceiptID uuid.UUID
	ReceiptAt time.Time
	ClaimID   uuid.UUID
	From      types.RunState
}

// RunKill is the transition ClaimSuspensionKillAt makes: run RunID from state From to KILLED. With MarkKept the run's
// kept machine is marked for delete by By, as killed by Killer, unless Exempt owns it (KillRunMarkingKeptMachine).
// ReceiptID and ReceiptAt are the event the claim pins when the row has none; ClaimID names the acquisition.
type RunKill struct {
	RunID              uuid.UUID
	From               types.RunState
	MarkKept           bool
	By, Killer, Exempt string
	ReceiptID          uuid.UUID
	ReceiptAt          time.Time
	ClaimID            uuid.UUID
}

// suspensionCurrentSQL is the test FinishSuspensionJobAt applies, with the identity as $1 and the bound as the
// parameter named by at (for example "$5").
func suspensionCurrentSQL(at string) string {
	return `EXISTS (SELECT 1 FROM principal_identities i WHERE i.id = $1 AND (i.deactivated_at = ` + at + ` OR (i.deactivated_at IS NULL
		        AND EXISTS (SELECT 1 FROM deprovision_jobs c WHERE c.identity_id = $1 AND c.kind = 'suspend' AND c.step = 'cutoff' AND c.target = ''
		                     AND c.detail->'deactivated_at_us' = to_jsonb((extract(epoch FROM ` + at + `::timestamptz) * 1000000)::bigint)))))`
}

// personActiveSQL: the identity ($3) a suspension tail works for is active again. Only then is a row stamped
// after the tail's bound one a reactivated person made, and only then is a form's session cut not written
// again; while the person is deactivated nothing is spared, since not every insert is refused then (a service
// token's, or one under a principal no identity row binds).
const personActiveSQL = `EXISTS (SELECT 1 FROM principal_identities WHERE id = $3 AND deactivated_at IS NULL AND purged_at IS NULL)`

// RunsInsertedAfter is the subset of ids whose agent_runs row the database stamped strictly after at, read in
// the statement that finds the identity active again. A row stamped at at itself is not in it, nor is a row
// inserted before the stamp existed (NULL), and none is while the identity is deactivated.
func (s PG) RunsInsertedAfter(ctx context.Context, identity uuid.UUID, ids []uuid.UUID, at time.Time) (map[uuid.UUID]bool, error) {
	return insertedAfter(ctx, s, "runs", `SELECT id FROM agent_runs WHERE id = ANY($1::uuid[]) AND inserted_at > $2 AND `+personActiveSQL, identity, ids, at)
}

// APITokensInsertedAfter is RunsInsertedAfter for api_tokens rows.
func (s PG) APITokensInsertedAfter(ctx context.Context, identity uuid.UUID, ids []uuid.UUID, at time.Time) (map[uuid.UUID]bool, error) {
	return insertedAfter(ctx, s, "api tokens", `SELECT id FROM api_tokens WHERE id = ANY($1::uuid[]) AND inserted_at > $2 AND `+personActiveSQL, identity, ids, at)
}

func insertedAfter(ctx context.Context, s PG, noun, q string, identity uuid.UUID, ids []uuid.UUID, at time.Time) (map[uuid.UUID]bool, error) {
	after, err := collect(ctx, s.Pool, "read insert stamps of", noun, q, []any{ids, at, identity}, func(row pgx.Row) (uuid.UUID, error) {
		var id uuid.UUID
		return id, row.Scan(&id)
	})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(after))
	for _, id := range after {
		out[id] = true
	}
	return out, nil
}

// DeleteSSHKeysUpTo is DeleteSSHKeys for the keys the database stamped at or before at, and those inserted
// before the stamp existed (NULL), in one statement; every key of principal goes while the identity is
// deactivated.
func (s PG) DeleteSSHKeysUpTo(ctx context.Context, identity uuid.UUID, principal string, at time.Time) (int, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM ssh_public_keys WHERE ($1 = '' OR principal = $1) AND (inserted_at IS NULL OR inserted_at <= $2 OR NOT `+personActiveSQL+`)`,
		principal, at, identity)
	if err != nil {
		return 0, fmt.Errorf("store: delete ssh keys up to an instant: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// SubCutSince says sub carries a session cutoff written at or after at, read in the statement that finds the
// identity active again. While the identity is deactivated it is false, so the tail cuts again as before the
// bound existed, and a failed cut fails its step. Nothing deletes a cutoff row; a false answer leads the
// caller to write one, so a cutoff a racing write set back is cut again.
func (s PG) SubCutSince(ctx context.Context, identity uuid.UUID, sub string, at time.Time) (bool, error) {
	var cut bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM oidc_session_revocations WHERE sub = $1 AND revoked_at >= $2) AND `+personActiveSQL,
		sub, at, identity).Scan(&cut); err != nil {
		return false, fmt.Errorf("store: read session cutoff: %w", err)
	}
	return cut, nil
}

// FinishSuspensionJobAt is FinishDeprovisionJob for a suspension tail bound at at: in one statement, it closes the
// step only while the identity is still deactivated at at or, once the identity is active again, while the
// ledger's cutoff step still records at as the deactivation it left (SuspendIdentity writes the record in the
// transaction that resets the ledger, and a reactivation keeps it). Only a reactivation lets a later suspension
// reset the ledger, and that suspension records its own deactivation, so a tail bound to an earlier one closes
// none of its steps, also once the person is active again. Nor does it close one, while the identity stays
// deactivated or once it is active again, after a suspension of another identity deactivated it at another
// instant: that suspension keeps this ledger but drops its record, and a pass bound to that instant owes more than
// this tail did. A step the tail may not close, or whose row a later suspension replaced (k.ID, when set, names the
// row the tail read), is ErrSuperseded.
func (s PG) FinishSuspensionJobAt(ctx context.Context, identity uuid.UUID, k JobKey, detail map[string]int, at time.Time) error {
	return s.finishJob(ctx, identity, JobKindSuspend, k, detail, &at, true)
}

// EnsureSuspensionJobsAt — see PortalSuspensionLedger.
func (s PG) EnsureSuspensionJobsAt(ctx context.Context, identity uuid.UUID, keys []JobKey, at time.Time) error {
	for _, k := range keys {
		var current bool
		if err := s.Pool.QueryRow(ctx, `
			WITH cur AS (SELECT `+suspensionCurrentSQL("$4")+` AS ok),
			     ins AS (INSERT INTO deprovision_jobs (identity_id, kind, step, target)
			             SELECT $1::uuid, 'suspend', $2::text, $3::text FROM cur WHERE ok
			             ON CONFLICT (identity_id, kind, step, target) DO NOTHING RETURNING 1)
			SELECT ok FROM cur`, identity, k.Step, k.Target, at).Scan(&current); err != nil {
			return fmt.Errorf("store: ensure suspension job: %w", err)
		}
		if !current {
			return ErrSuperseded
		}
	}
	return nil
}

// ReopenSuspensionJobAt — see PortalSuspensionLedger.
func (s PG) ReopenSuspensionJobAt(ctx context.Context, identity uuid.UUID, k JobKey, at time.Time) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE deprovision_jobs SET state = 'pending', audit_event_id = NULL, audit_event_at = NULL, audit_event_payload = NULL, kill_claimed_at = NULL, kill_claim_id = NULL, kill_from = NULL, updated_at = now()
		 WHERE identity_id = $1 AND kind = 'suspend' AND step = $2 AND target = $3 AND ($4::uuid IS NULL OR id = $4)
		   AND `+suspensionCurrentSQL("$5"), identity, k.Step, k.Target, k.occurrence(), at)
	if err != nil {
		return fmt.Errorf("store: reopen suspension job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSuperseded
	}
	return nil
}

// ClaimSuspensionJobAt — see PortalSuspensionLedger.
func (s PG) ClaimSuspensionJobAt(ctx context.Context, identity uuid.UUID, k JobKey, at time.Time) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE deprovision_jobs SET updated_at = now()
		 WHERE identity_id = $1 AND kind = 'suspend' AND step = $2 AND target = $3 AND ($4::uuid IS NULL OR id = $4) AND state = 'pending'
		   AND `+suspensionCurrentSQL("$5"), identity, k.Step, k.Target, k.occurrence(), at)
	if err != nil {
		return fmt.Errorf("store: claim suspension job: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSuperseded
	}
	return nil
}

// ClaimSuspensionKillAt — see PortalSuspensionLedger.
func (s PG) ClaimSuspensionKillAt(ctx context.Context, identity uuid.UUID, k JobKey, at time.Time, kill RunKill) (KillClaim, error) {
	if kill.MarkKept && kill.By == "" {
		return KillClaim{}, errKeptMachineNoBy
	}
	var out KillClaim
	err := s.inTx(ctx, func(q Querier) error {
		var held bool
		var was *string
		err := q.QueryRow(ctx, `
			SELECT kill_claimed_at IS NOT NULL AND kill_claimed_at > now() - make_interval(secs => $6), kill_from
			  FROM deprovision_jobs
			 WHERE identity_id = $1 AND kind = 'suspend' AND step = $2 AND target = $3 AND ($4::uuid IS NULL OR id = $4) AND state = 'pending'
			   AND `+suspensionCurrentSQL("$5")+` FOR UPDATE`, identity, k.Step, k.Target, k.occurrence(), at, KillClaimLease.Seconds()).Scan(&held, &was)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSuperseded
		}
		if err != nil {
			return fmt.Errorf("store: lock suspension job: %w", err)
		}
		if kill.From == types.RunKilled && held {
			return ErrKillHeld
		}
		out.From = kill.From
		switch {
		case kill.From == types.RunKilled:
			// A repair: the run is already the occurrence's, so its state is not written again, but the claim stamps
			// it, which is the grace the kill-tail recovery sweep reads before it takes a KILLED run's teardown.
			var tag pgconn.CommandTag
			tag, err = q.Exec(ctx, `UPDATE agent_runs SET updated_at = now() WHERE id = $1 AND state = $2`, kill.RunID, string(types.RunKilled))
			out.Applied = err == nil && tag.RowsAffected() > 0
			if was != nil {
				out.From = types.RunState(*was) // killed from the state the first claim killed it from
			}
		case kill.From == types.RunQueued:
			err = q.QueryRow(ctx, `
				WITH moved AS (UPDATE agent_runs SET state = $1, updated_at = now(), ended_at = COALESCE(ended_at, now()) WHERE id = $2 AND state = $3 RETURNING id),
				     gone AS (DELETE FROM run_queue WHERE run_id IN (SELECT id FROM moved) RETURNING run_id)
				SELECT count(*) > 0 FROM moved`, string(types.RunKilled), kill.RunID, string(types.RunQueued)).Scan(&out.Applied)
		case kill.MarkKept:
			out.Applied, out.Intent, err = killRunMarkingKeptMachineQ(ctx, q, kill.RunID, kill.From, kill.By, kill.Killer, kill.Exempt)
		default:
			var tag pgconn.CommandTag
			tag, err = q.Exec(ctx, `UPDATE agent_runs SET state = $1, updated_at = now(), ended_at = COALESCE(ended_at, now()) WHERE id = $2 AND state = $3`,
				string(types.RunKilled), kill.RunID, string(kill.From))
			out.Applied = err == nil && tag.RowsAffected() > 0
		}
		if err != nil {
			return fmt.Errorf("store: claim a run's kill: %w", err)
		}
		if !out.Applied {
			return nil
		}
		err = q.QueryRow(ctx, `
			UPDATE deprovision_jobs SET kill_claimed_at = now(), kill_claim_id = $7, kill_from = COALESCE(kill_from, $8),
			       audit_event_id = COALESCE(audit_event_id, $3), audit_event_at = COALESCE(audit_event_at, $4)
			 WHERE identity_id = $1 AND kind = 'suspend' AND step = $5 AND target = $6 AND ($2::uuid IS NULL OR id = $2)
			RETURNING audit_event_id, audit_event_at`, identity, k.occurrence(), kill.ReceiptID, kill.ReceiptAt, k.Step, k.Target,
			kill.ClaimID, string(kill.From)).Scan(&out.ReceiptID, &out.ReceiptAt)
		out.ClaimID = kill.ClaimID
		if err != nil {
			return fmt.Errorf("store: pin a kill's receipt: %w", err)
		}
		return nil
	})
	if err != nil {
		return KillClaim{}, err
	}
	return out, nil
}

// ReleaseSuspensionKillAt — see PortalSuspensionLedger.
func (s PG) ReleaseSuspensionKillAt(ctx context.Context, identity uuid.UUID, k JobKey, claim uuid.UUID) error {
	if claim == uuid.Nil {
		return nil
	}
	if _, err := s.Pool.Exec(ctx, `
		UPDATE deprovision_jobs SET kill_claimed_at = NULL, kill_claim_id = NULL
		 WHERE identity_id = $1 AND kind = 'suspend' AND step = $2 AND target = $3 AND ($4::uuid IS NULL OR id = $4) AND state = 'pending'
		   AND kill_claim_id = $5`,
		identity, k.Step, k.Target, k.occurrence(), claim); err != nil {
		return fmt.Errorf("store: release a suspension kill claim: %w", err)
	}
	return nil
}

// PinnedEvent is the audit event a portal step writes, as its ledger row holds it: the id and time the first
// attempt fixed and the payload it fixed with them.
//
// Done is set when the row had already been finished: its event is the one written, nothing was pinned or
// stored, and its payload is gone.
type PinnedEvent struct {
	ID      uuid.UUID
	At      time.Time
	Payload []byte
	Done    bool
}

// PortalEventPinner pins a portal step's audit event on its ledger row.
type PortalEventPinner interface {
	// PinPortalEvent stores id, at and payload on the row k names (k.ID is required: the row's own id) where it has
	// none, and returns what the row holds, the first attempt's. With bound set (a suspension step) the row must
	// also belong to the suspension that deactivated the identity at bound, in the same statement. A row it may not
	// pin is ErrSuperseded and nothing is written.
	PinPortalEvent(ctx context.Context, identity uuid.UUID, kind string, k JobKey, id uuid.UUID, at time.Time, payload []byte, bound *time.Time) (PinnedEvent, error)
}

// PinPortalEvent — see PortalEventPinner. Only a pending row is written: a step another worker finished meanwhile
// answers what it holds with Done set, so a late retry neither pins again nor restores a payload the finish cleared.
func (s PG) PinPortalEvent(ctx context.Context, identity uuid.UUID, kind string, k JobKey, id uuid.UUID, at time.Time, payload []byte, bound *time.Time) (PinnedEvent, error) {
	if k.ID == uuid.Nil {
		return PinnedEvent{}, ErrSuperseded
	}
	var out PinnedEvent
	err := s.Pool.QueryRow(ctx, `
		WITH cur AS (SELECT id, state, audit_event_id, audit_event_at, audit_event_payload FROM deprovision_jobs
		              WHERE identity_id = $1 AND kind = $2 AND step = $3 AND target = $4 AND id = $8
		                AND ($9::timestamptz IS NULL OR `+suspensionCurrentSQL("$9")+`) FOR UPDATE),
		     upd AS (UPDATE deprovision_jobs d SET audit_event_id = COALESCE(d.audit_event_id, $5), audit_event_at = COALESCE(d.audit_event_at, $6),
		                    audit_event_payload = COALESCE(d.audit_event_payload, $7::jsonb)
		              FROM cur WHERE d.id = cur.id AND cur.state = 'pending'
		          RETURNING d.audit_event_id, d.audit_event_at, d.audit_event_payload)
		SELECT COALESCE(upd.audit_event_id, cur.audit_event_id), COALESCE(upd.audit_event_at, cur.audit_event_at), COALESCE(upd.audit_event_payload, cur.audit_event_payload), cur.state = 'done'
		  FROM cur LEFT JOIN upd ON true`,
		identity, kind, k.Step, k.Target, id, at, payload, k.ID, bound).Scan(&out.ID, &out.At, &out.Payload, &out.Done)
	if errors.Is(err, pgx.ErrNoRows) {
		return PinnedEvent{}, ErrSuperseded
	}
	if err != nil {
		return PinnedEvent{}, fmt.Errorf("store: pin portal event: %w", err)
	}
	return out, nil
}
