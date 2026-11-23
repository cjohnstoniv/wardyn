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
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrAssistantSessionLive reports a create that met a live session: one per run, and one per person and
// purpose.
var ErrAssistantSessionLive = errors.New("store: a live assistant session already exists")

// AssistantSessionStore keeps assistant sessions and their transcripts. Optional like
// UnitModelUsageReader: the api answers 501 from a store without it.
type AssistantSessionStore interface {
	// CreateAssistantSession inserts s in the state it carries, starting with a run id or fetching without
	// one. It returns ErrAssistantSessionLive when the person already has a live session for the purpose or
	// the run already holds one.
	CreateAssistantSession(ctx context.Context, s types.AssistantSession) (types.AssistantSession, error)
	// ReplaceAssistantSession ends the live session old as replaced and creates s, in one transaction. A
	// non-nil reset is written as the new session's first event, numbered after the old session's last, so
	// one run's turns keep one count.
	ReplaceAssistantSession(ctx context.Context, old uuid.UUID, s types.AssistantSession, reset []byte) (types.AssistantSession, error)
	// AttachAssistantRun moves a fetching session to starting on run.
	AttachAssistantRun(ctx context.Context, session, run uuid.UUID) error
	// GetAssistantSession is the session id when principal owns it, else ErrNotFound. An empty principal
	// owns nothing.
	GetAssistantSession(ctx context.Context, id uuid.UUID, principal string) (types.AssistantSession, error)
	// LiveAssistantSession is the person's live session for the purpose, ErrNotFound when none.
	LiveAssistantSession(ctx context.Context, principal string, purpose types.AssistantPurpose) (types.AssistantSession, error)
	// LiveAssistantSessionForRun is the run's live session, ErrNotFound when none.
	LiveAssistantSessionForRun(ctx context.Context, run uuid.UUID) (types.AssistantSession, error)
	// LatestAssistantSessionForRun is the run's newest session, live or ended, ErrNotFound when none.
	LatestAssistantSessionForRun(ctx context.Context, run uuid.UUID) (types.AssistantSession, error)
	// AssistantRunIDs is those of runs that ever had a session.
	AssistantRunIDs(ctx context.Context, runs []uuid.UUID) ([]uuid.UUID, error)
	// AssistantRunIDsCreated is the ids of every run created in [since, until) that ever had a session.
	AssistantRunIDsCreated(ctx context.Context, since, until time.Time) ([]uuid.UUID, error)
	// AppendAssistantEvent writes an event with the next seq of its session, taken under the session row's
	// lock. ErrNotFound when the session does not exist.
	AppendAssistantEvent(ctx context.Context, session uuid.UUID, kind string, payload []byte) (types.AssistantEvent, error)
	// ListAssistantEvents is up to limit events with seq above after, oldest first.
	ListAssistantEvents(ctx context.Context, session uuid.UUID, after int64, limit int) ([]types.AssistantEvent, error)
	// NextAssistantTurn is the first event above after that the relay must act on: a person's message or a
	// reset. first reports that no earlier turn of the session exists, so the turn carries the records.
	// ErrNotFound when there is none.
	NextAssistantTurn(ctx context.Context, session uuid.UUID, after int64) (ev types.AssistantEvent, first bool, err error)
	// AssistantTurnCount is how many turn_end events the session holds.
	AssistantTurnCount(ctx context.Context, session uuid.UUID) (int, error)
	// SetAssistantDraft replaces the working draft of a live session.
	SetAssistantDraft(ctx context.Context, session uuid.UUID, draft []byte) error
	// TouchAssistantSession sets last_turn_at to now, and moves starting to ready when ready is set.
	TouchAssistantSession(ctx context.Context, session uuid.UUID, ready bool) error
	// EndAssistantSession ends the session with reason; it is idempotent and keeps the first reason. It
	// reports whether this call ended it.
	EndAssistantSession(ctx context.Context, session uuid.UUID, reason string) (bool, error)
	// EraseAssistantSessionPersonalFields ends every live session of principals as erased and returns their
	// run ids so the caller stops those runs; deletes the transcript text of every session of principals;
	// clears the letters, draft, groups and e-mail; and blanks the principal. It returns the sessions it
	// touched.
	EraseAssistantSessionPersonalFields(ctx context.Context, principals []string) (runs []uuid.UUID, sessions int, err error)
	// SweepEndedAssistantSessions deletes the events of sessions ended before before and clears their
	// payload, in batches of batch sessions and one transaction each. It returns the sessions cleaned.
	SweepEndedAssistantSessions(ctx context.Context, before time.Time, batch int) (int, error)
}

var _ AssistantSessionStore = PG{}

const assistantSessionCols = `id, run_id, principal, email, role, user_type, groups, groups_truncated, purpose, target_kind,
	target_key, workspace_id, repo, provider_id, provider_uid, model, state, draft, person_letters, preview_digest,
	created_at, last_turn_at, ended_at, end_reason`

func scanAssistantSession(r pgx.Row) (types.AssistantSession, error) {
	var s types.AssistantSession
	var purpose string
	var letters []byte
	var endReason *string
	err := r.Scan(&s.ID, &s.RunID, &s.Principal, &s.Email, &s.Role, &s.UserType, &s.Groups, &s.GroupsTruncated, &purpose,
		&s.TargetKind, &s.TargetKey, &s.WorkspaceID, &s.Repo, &s.Binding.ProviderID, &s.Binding.ProviderUID, &s.Binding.Model,
		&s.State, &s.Draft, &letters, &s.PreviewDigest, &s.CreatedAt, &s.LastTurnAt, &s.EndedAt, &endReason)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return s, ErrNotFound
		}
		return s, fmt.Errorf("store: scan assistant session: %w", err)
	}
	s.Purpose = types.AssistantPurpose(purpose)
	if endReason != nil {
		s.EndReason = *endReason
	}
	if len(letters) > 0 {
		if err := json.Unmarshal(letters, &s.PersonLetters); err != nil {
			return s, fmt.Errorf("store: decode assistant letters: %w", err)
		}
	}
	return s, nil
}

func assistantSessionArgs(s types.AssistantSession) ([]any, error) {
	var letters []byte
	if s.PersonLetters != nil {
		b, err := json.Marshal(s.PersonLetters)
		if err != nil {
			return nil, fmt.Errorf("store: encode assistant letters: %w", err)
		}
		letters = b
	}
	groups := s.Groups
	if groups == nil {
		groups = []string{}
	}
	return []any{s.ID, s.RunID, s.Principal, s.Email, s.Role, s.UserType, groups, s.GroupsTruncated, string(s.Purpose),
		s.TargetKind, s.TargetKey, s.WorkspaceID, s.Repo, s.Binding.ProviderID, s.Binding.ProviderUID, s.Binding.Model,
		s.State, nullJSON(s.Draft), nullJSON(letters), s.PreviewDigest}, nil
}

// nullJSON is b as a jsonb argument, NULL when empty.
func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

const insertAssistantSessionSQL = `INSERT INTO assistant_sessions (id, run_id, principal, email, role, user_type, groups,
		groups_truncated, purpose, target_kind, target_key, workspace_id, repo, provider_id, provider_uid, model, state, draft,
		person_letters, preview_digest)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
	RETURNING ` + assistantSessionCols

func insertAssistantSession(ctx context.Context, q Querier, s types.AssistantSession) (types.AssistantSession, error) {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	args, err := assistantSessionArgs(s)
	if err != nil {
		return s, err
	}
	out, err := scanAssistantSession(q.QueryRow(ctx, insertAssistantSessionSQL, args...))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return s, ErrAssistantSessionLive
	}
	return out, err
}

func (s PG) CreateAssistantSession(ctx context.Context, sess types.AssistantSession) (types.AssistantSession, error) {
	return insertAssistantSession(ctx, s.Pool, sess)
}

func (s PG) ReplaceAssistantSession(ctx context.Context, old uuid.UUID, sess types.AssistantSession, reset []byte) (types.AssistantSession, error) {
	var out types.AssistantSession
	err := s.inTx(ctx, func(q Querier) error {
		var lastSeq int64
		if err := q.QueryRow(ctx, `SELECT coalesce((SELECT max(seq) FROM assistant_session_events WHERE session_id = $1), 0)
			FROM assistant_sessions WHERE id = $1 FOR UPDATE`, old).Scan(&lastSeq); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("store: lock assistant session: %w", err)
		}
		if _, err := q.Exec(ctx, `UPDATE assistant_sessions SET state = 'ended', ended_at = now(), end_reason = $2
			WHERE id = $1 AND state <> 'ended'`, old, types.AssistantEndReplaced); err != nil {
			return fmt.Errorf("store: end replaced assistant session: %w", err)
		}
		created, err := insertAssistantSession(ctx, q, sess)
		if err != nil {
			return err
		}
		out = created
		if reset == nil {
			return nil
		}
		if _, err := q.Exec(ctx, `INSERT INTO assistant_session_events (session_id, seq, kind, payload) VALUES ($1, $2, $3, $4)`,
			created.ID, lastSeq+1, types.AssistantEventState, reset); err != nil {
			return fmt.Errorf("store: write assistant reset: %w", err)
		}
		return nil
	})
	return out, err
}

func (s PG) AttachAssistantRun(ctx context.Context, session, run uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE assistant_sessions SET run_id = $2, state = 'starting' WHERE id = $1 AND state = 'fetching'`, session, run)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrAssistantSessionLive
		}
		return fmt.Errorf("store: attach assistant run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s PG) GetAssistantSession(ctx context.Context, id uuid.UUID, principal string) (types.AssistantSession, error) {
	if principal == "" {
		return types.AssistantSession{}, ErrNotFound
	}
	return scanAssistantSession(s.Pool.QueryRow(ctx, `SELECT `+assistantSessionCols+` FROM assistant_sessions WHERE id = $1 AND principal = $2`, id, principal))
}

func (s PG) LiveAssistantSession(ctx context.Context, principal string, purpose types.AssistantPurpose) (types.AssistantSession, error) {
	if principal == "" {
		return types.AssistantSession{}, ErrNotFound
	}
	return scanAssistantSession(s.Pool.QueryRow(ctx, `SELECT `+assistantSessionCols+` FROM assistant_sessions
		WHERE principal = $1 AND purpose = $2 AND state <> 'ended'`, principal, string(purpose)))
}

func (s PG) LiveAssistantSessionForRun(ctx context.Context, run uuid.UUID) (types.AssistantSession, error) {
	return scanAssistantSession(s.Pool.QueryRow(ctx, `SELECT `+assistantSessionCols+` FROM assistant_sessions
		WHERE run_id = $1 AND state <> 'ended'`, run))
}

func (s PG) LatestAssistantSessionForRun(ctx context.Context, run uuid.UUID) (types.AssistantSession, error) {
	return scanAssistantSession(s.Pool.QueryRow(ctx, `SELECT `+assistantSessionCols+` FROM assistant_sessions
		WHERE run_id = $1 ORDER BY created_at DESC, id LIMIT 1`, run))
}

func scanUUID(r pgx.Row) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.Scan(&id)
	return id, err
}

func (s PG) AssistantRunIDs(ctx context.Context, runs []uuid.UUID) ([]uuid.UUID, error) {
	if len(runs) == 0 {
		return []uuid.UUID{}, nil
	}
	return collect(ctx, s.Pool, "read", "assistant run ids", `SELECT DISTINCT run_id FROM assistant_sessions WHERE run_id = ANY($1)`,
		[]any{runs}, scanUUID)
}

func (s PG) AssistantRunIDsCreated(ctx context.Context, since, until time.Time) ([]uuid.UUID, error) {
	return collect(ctx, s.Pool, "read", "assistant run ids", `SELECT DISTINCT s.run_id FROM assistant_sessions s
		JOIN agent_runs r ON r.id = s.run_id WHERE r.created_at >= $1 AND r.created_at < $2`, []any{since, until}, scanUUID)
}

func scanAssistantEvent(r pgx.Row) (types.AssistantEvent, error) {
	var e types.AssistantEvent
	err := r.Scan(&e.SessionID, &e.Seq, &e.Kind, &e.Payload, &e.CreatedAt)
	return e, err
}

func (s PG) AppendAssistantEvent(ctx context.Context, session uuid.UUID, kind string, payload []byte) (types.AssistantEvent, error) {
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	var ev types.AssistantEvent
	err := s.inTx(ctx, func(q Querier) error {
		var locked uuid.UUID
		if err := q.QueryRow(ctx, `SELECT id FROM assistant_sessions WHERE id = $1 FOR UPDATE`, session).Scan(&locked); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("store: lock assistant session: %w", err)
		}
		e, err := scanAssistantEvent(q.QueryRow(ctx, `INSERT INTO assistant_session_events (session_id, seq, kind, payload)
			SELECT $1, coalesce(max(seq), 0) + 1, $2, $3 FROM assistant_session_events WHERE session_id = $1
			RETURNING session_id, seq, kind, payload, created_at`, session, kind, payload))
		if err != nil {
			return fmt.Errorf("store: append assistant event: %w", err)
		}
		ev = e
		return nil
	})
	return ev, err
}

func (s PG) ListAssistantEvents(ctx context.Context, session uuid.UUID, after int64, limit int) ([]types.AssistantEvent, error) {
	return collect(ctx, s.Pool, "read", "assistant events", `SELECT session_id, seq, kind, payload, created_at
		FROM assistant_session_events WHERE session_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, []any{session, after, limit}, scanAssistantEvent)
}

func (s PG) NextAssistantTurn(ctx context.Context, session uuid.UUID, after int64) (types.AssistantEvent, bool, error) {
	var e types.AssistantEvent
	var first bool
	err := s.Pool.QueryRow(ctx, `SELECT e.session_id, e.seq, e.kind, e.payload, e.created_at,
			NOT EXISTS (SELECT 1 FROM assistant_session_events p WHERE p.session_id = e.session_id AND p.seq < e.seq
				AND (p.kind = 'user_message' OR (p.kind = 'state' AND p.payload->>'turn' = 'reset')))
		FROM assistant_session_events e
		WHERE e.session_id = $1 AND e.seq > $2 AND (e.kind = 'user_message' OR (e.kind = 'state' AND e.payload->>'turn' = 'reset'))
		ORDER BY e.seq LIMIT 1`, session, after).Scan(&e.SessionID, &e.Seq, &e.Kind, &e.Payload, &e.CreatedAt, &first)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, false, ErrNotFound
	}
	if err != nil {
		return e, false, fmt.Errorf("store: read assistant turn: %w", err)
	}
	return e, first, nil
}

func (s PG) AssistantTurnCount(ctx context.Context, session uuid.UUID) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM assistant_session_events WHERE session_id = $1 AND kind = $2`,
		session, types.AssistantEventTurnEnd).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count assistant turns: %w", err)
	}
	return n, nil
}

func (s PG) SetAssistantDraft(ctx context.Context, session uuid.UUID, draft []byte) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE assistant_sessions SET draft = $2 WHERE id = $1 AND state <> 'ended'`, session, nullJSON(draft))
	if err != nil {
		return fmt.Errorf("store: set assistant draft: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s PG) TouchAssistantSession(ctx context.Context, session uuid.UUID, ready bool) error {
	_, err := s.Pool.Exec(ctx, `UPDATE assistant_sessions SET last_turn_at = now(),
			state = CASE WHEN $2 AND state = 'starting' THEN 'ready' ELSE state END
		WHERE id = $1 AND state <> 'ended'`, session, ready)
	if err != nil {
		return fmt.Errorf("store: touch assistant session: %w", err)
	}
	return nil
}

func (s PG) EndAssistantSession(ctx context.Context, session uuid.UUID, reason string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE assistant_sessions SET state = 'ended', ended_at = now(), end_reason = $2
		WHERE id = $1 AND state <> 'ended'`, session, reason)
	if err != nil {
		return false, fmt.Errorf("store: end assistant session: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s PG) EraseAssistantSessionPersonalFields(ctx context.Context, principals []string) ([]uuid.UUID, int, error) {
	if len(principals) == 0 {
		return nil, 0, nil
	}
	runs := []uuid.UUID{}
	n := 0
	err := s.inTx(ctx, func(q Querier) error {
		ended, err := collect(ctx, q, "end", "assistant sessions", `UPDATE assistant_sessions SET state = 'ended', ended_at = now(), end_reason = $2
			WHERE principal = ANY($1) AND state <> 'ended' RETURNING run_id`, []any{principals, types.AssistantEndErased},
			func(r pgx.Row) (*uuid.UUID, error) {
				var id *uuid.UUID
				return id, r.Scan(&id)
			})
		if err != nil {
			return err
		}
		for _, id := range ended {
			if id != nil {
				runs = append(runs, *id)
			}
		}
		if _, err := q.Exec(ctx, `DELETE FROM assistant_session_events WHERE kind IN ('user_message', 'person_edit', 'text')
			AND session_id IN (SELECT id FROM assistant_sessions WHERE principal = ANY($1))`, principals); err != nil {
			return fmt.Errorf("store: erase assistant transcripts: %w", err)
		}
		tag, err := q.Exec(ctx, `UPDATE assistant_sessions SET person_letters = NULL, draft = NULL, groups = '{}', email = '', principal = $2
			WHERE principal = ANY($1)`, principals, types.AssistantErasedPrincipal)
		if err != nil {
			return fmt.Errorf("store: erase assistant sessions: %w", err)
		}
		n = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return runs, n, nil
}

func (s PG) SweepEndedAssistantSessions(ctx context.Context, before time.Time, batch int) (int, error) {
	if batch <= 0 {
		batch = 100
	}
	total := 0
	for {
		n := 0
		err := s.inTx(ctx, func(q Querier) error {
			ids, err := collect(ctx, q, "read", "swept assistant sessions", `SELECT id FROM assistant_sessions s
				WHERE s.state = 'ended' AND s.ended_at < $1
				AND (s.draft IS NOT NULL OR s.person_letters IS NOT NULL OR s.groups <> '{}' OR s.email <> ''
					OR EXISTS (SELECT 1 FROM assistant_session_events e WHERE e.session_id = s.id))
				ORDER BY s.ended_at LIMIT $2 FOR UPDATE SKIP LOCKED`, []any{before, batch}, scanUUID)
			if err != nil || len(ids) == 0 {
				return err
			}
			if _, err := q.Exec(ctx, `DELETE FROM assistant_session_events WHERE session_id = ANY($1)`, ids); err != nil {
				return fmt.Errorf("store: sweep assistant events: %w", err)
			}
			if _, err := q.Exec(ctx, `UPDATE assistant_sessions SET draft = NULL, person_letters = NULL, groups = '{}', email = ''
				WHERE id = ANY($1)`, ids); err != nil {
				return fmt.Errorf("store: sweep assistant sessions: %w", err)
			}
			n = len(ids)
			return nil
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < batch {
			return total, nil
		}
	}
}
