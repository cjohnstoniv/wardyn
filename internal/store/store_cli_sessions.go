// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// CLI sign-in requests and CLI sessions (migration 0198_cli_sign_in). The secrets never reach SQL:
// the code K and the wdc_ session secret are stored as hashToken digests, like api_tokens.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// CLISignInStore is the store half of CLI sign-in. Every time is the app clock the caller passes, so
// expiry is compared on one clock. ErrNotFound covers unknown, expired, decided and spent alike.
type CLISignInStore interface {
	// CreateCLISignIn inserts a request unless maxPending requests are already waiting (false).
	CreateCLISignIn(ctx context.Context, si CLISignIn, maxPending int, now time.Time) (bool, error)
	GetCLISignIn(ctx context.Context, id uuid.UUID) (CLISignIn, error)
	// DecideCLISignIn records Approve (with the approver's snapshot and K's hash) or Deny on a request
	// that is still waiting and unexpired.
	DecideCLISignIn(ctx context.Context, id uuid.UUID, d CLISignInDecision) (CLISignIn, error)
	// RedeemCLISignIn spends K: once, before it expires, on an approved request.
	RedeemCLISignIn(ctx context.Context, id uuid.UUID, code string, now time.Time) (CLISignIn, error)
	// CreateCLISession inserts the session and its first access token in one transaction.
	CreateCLISession(ctx context.Context, sess CLISessionRow, secret string, tok types.APIToken, raw string) (types.APIToken, error)
	GetCLISession(ctx context.Context, id uuid.UUID) (CLISessionRow, error)
	// RotateCLISession moves a live session from generation to generation+1 with newSecret and inserts
	// the new access token, in one transaction. ErrConflict when the generation already moved or the
	// session ended: a second refresh with the same secret lost the race.
	RotateCLISession(ctx context.Context, id uuid.UUID, generation int64, newSecret string, now time.Time, tok types.APIToken, raw string) (types.APIToken, error)
	// CreateCLIAdminToken inserts a cli_admin token for a session that is still live.
	CreateCLIAdminToken(ctx context.Context, sessionID uuid.UUID, tok types.APIToken, raw string) (types.APIToken, error)
	// EndCLISession ends one live session and revokes its tokens. principal scopes it when non-empty.
	EndCLISession(ctx context.Context, id uuid.UUID, principal, reason string, now time.Time) (CLISessionRow, error)
	// ListCLISessions is principal's live sessions, newest first.
	ListCLISessions(ctx context.Context, principal string, now time.Time) ([]CLISessionRow, error)
	// CountLiveCLISessions counts the live sessions a cut naming name ends ("" = every one).
	CountLiveCLISessions(ctx context.Context, name string) (int, error)
}

var _ CLISignInStore = PG{}

// CLIIdentity is the identity snapshot a sign-in approval takes and a CLI session keeps: the columns
// an api_tokens row carries, plus the approving browser session's revocation sequence and authority
// epoch, which the session is checked against at every refresh.
type CLIIdentity struct {
	Principal, Email, Role, UserType string
	Groups                           []string
	GroupsTruncated                  *bool
	RevSeq, AuthorityEpoch           int64
}

// CLISignIn is one cli_sign_ins row.
type CLISignIn struct {
	ID                   uuid.UUID
	UserCode, Challenge  string
	Port                 int
	Machine, Mode, Store string
	PublicKey            []byte
	Admin                bool
	SessionID            *uuid.UUID
	RequestAddr          string
	CreatedAt, ExpiresAt time.Time
	Decision             string
	DecidedAt            *time.Time
	Approver             CLIIdentity // set once approved
	AuthorizedAt         *time.Time
	CodeExpiresAt        *time.Time
	RedeemedAt           *time.Time
}

// CLISignInDecision is DecideCLISignIn's input. Approver, AuthorizedAt, Code and CodeExpiresAt are
// read for an approval only.
type CLISignInDecision struct {
	Decision                         string
	Approver                         CLIIdentity
	AuthorizedAt, CodeExpiresAt, Now time.Time
	Code                             string
}

// CLISessionRow is one cli_sessions row. SecretSHA256 is the current secret's digest, read only to
// compare a presented secret against it. AdminExpiresAt is set by ListCLISessions alone.
type CLISessionRow struct {
	ID                                  uuid.UUID
	Identity                            CLIIdentity
	IdentityStampedAt                   time.Time
	Machine, Store                      string
	PublicKey                           []byte
	SecretSHA256                        string
	Generation                          int64
	CreatedAt, LastRefreshAt, ExpiresAt time.Time
	EndedAt                             *time.Time
	EndReason                           string
	AdminExpiresAt                      *time.Time
}

// CLISecretMatches reports whether secret is the session's current one.
func (r CLISessionRow) CLISecretMatches(secret string) bool {
	return hashToken(secret) == r.SecretSHA256
}

const cliSignInCols = `id, user_code, challenge, COALESCE(port, 0), machine, public_key, mode, store, admin, session_id,
	request_addr, created_at, expires_at, COALESCE(decision, ''), decided_at, COALESCE(principal, ''), COALESCE(email, ''),
	COALESCE(role, ''), COALESCE(user_type, ''), groups, groups_truncated, COALESCE(rev_seq, 0), COALESCE(authority_epoch, 0),
	authorized_at, code_expires_at, redeemed_at`

func scanCLISignIn(row pgx.Row) (CLISignIn, error) {
	var si CLISignIn
	var groups []byte
	err := row.Scan(&si.ID, &si.UserCode, &si.Challenge, &si.Port, &si.Machine, &si.PublicKey, &si.Mode, &si.Store, &si.Admin,
		&si.SessionID, &si.RequestAddr, &si.CreatedAt, &si.ExpiresAt, &si.Decision, &si.DecidedAt, &si.Approver.Principal,
		&si.Approver.Email, &si.Approver.Role, &si.Approver.UserType, &groups, &si.Approver.GroupsTruncated,
		&si.Approver.RevSeq, &si.Approver.AuthorityEpoch, &si.AuthorizedAt, &si.CodeExpiresAt, &si.RedeemedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CLISignIn{}, ErrNotFound
	}
	if err != nil {
		return CLISignIn{}, fmt.Errorf("store: scan cli sign-in: %w", err)
	}
	if groups != nil {
		if err := json.Unmarshal(groups, &si.Approver.Groups); err != nil {
			return CLISignIn{}, fmt.Errorf("store: scan cli sign-in groups: %w", err)
		}
	}
	return si, nil
}

// CreateCLISignIn inserts si, timed from now. The pending cap is read in the same statement.
//
// ponytail: two concurrent creates can both pass the count and overshoot the cap by one each; it
// bounds a flood, not an exact number.
func (s PG) CreateCLISignIn(ctx context.Context, si CLISignIn, maxPending int, now time.Time) (bool, error) {
	var port *int
	if si.Port != 0 {
		port = &si.Port
	}
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO cli_sign_ins (id, user_code, challenge, port, machine, public_key, mode, store, admin, session_id, request_addr, created_at, expires_at)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
		WHERE (SELECT count(*) FROM cli_sign_ins WHERE expires_at > $12 AND redeemed_at IS NULL AND decision IS DISTINCT FROM 'denied') < $14`,
		si.ID, si.UserCode, si.Challenge, port, si.Machine, si.PublicKey, si.Mode, si.Store, si.Admin, si.SessionID, si.RequestAddr,
		now, now.Add(types.CLISignInRequestLife), maxPending)
	if err != nil {
		return false, fmt.Errorf("store: create cli sign-in: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// GetCLISignIn reads one request, whatever its state.
func (s PG) GetCLISignIn(ctx context.Context, id uuid.UUID) (CLISignIn, error) {
	return scanCLISignIn(s.Pool.QueryRow(ctx, `SELECT `+cliSignInCols+` FROM cli_sign_ins WHERE id = $1`, id))
}

// DecideCLISignIn records d on a waiting, unexpired request.
func (s PG) DecideCLISignIn(ctx context.Context, id uuid.UUID, d CLISignInDecision) (CLISignIn, error) {
	if d.Decision != types.CLISignInApproved {
		return scanCLISignIn(s.Pool.QueryRow(ctx, `
			UPDATE cli_sign_ins SET decision = $2, decided_at = $3
			WHERE id = $1 AND decision IS NULL AND expires_at > $3
			RETURNING `+cliSignInCols, id, d.Decision, d.Now))
	}
	groups, err := marshalGroups(d.Approver.Groups)
	if err != nil {
		return CLISignIn{}, err
	}
	a := d.Approver
	return scanCLISignIn(s.Pool.QueryRow(ctx, `
		UPDATE cli_sign_ins SET decision = $2, decided_at = $3, principal = $4, email = $5, role = $6, user_type = $7,
		       groups = $8, groups_truncated = $9, rev_seq = $10, authority_epoch = $11, authorized_at = $12,
		       code_sha256 = $13, code_expires_at = $14
		WHERE id = $1 AND decision IS NULL AND expires_at > $3
		RETURNING `+cliSignInCols, id, d.Decision, d.Now, a.Principal, a.Email, a.Role, a.UserType, groups, a.GroupsTruncated,
		a.RevSeq, a.AuthorityEpoch, d.AuthorizedAt, hashToken(d.Code), d.CodeExpiresAt))
}

// RedeemCLISignIn spends code on id: the UPDATE is the single-use check.
func (s PG) RedeemCLISignIn(ctx context.Context, id uuid.UUID, code string, now time.Time) (CLISignIn, error) {
	return scanCLISignIn(s.Pool.QueryRow(ctx, `
		UPDATE cli_sign_ins SET redeemed_at = $3
		WHERE id = $1 AND decision = 'approved' AND redeemed_at IS NULL AND code_sha256 = $2 AND code_expires_at > $3
		RETURNING `+cliSignInCols, id, hashToken(code), now))
}

const cliSessionCols = `id, principal, email, role, user_type, groups, groups_truncated, rev_seq, authority_epoch,
	identity_stamped_at, machine, store, public_key, secret_sha256, generation, created_at, last_refresh_at, expires_at,
	ended_at, COALESCE(end_reason, '')`

func scanCLISession(row pgx.Row, extra ...any) (CLISessionRow, error) {
	var r CLISessionRow
	var groups []byte
	dest := append([]any{&r.ID, &r.Identity.Principal, &r.Identity.Email, &r.Identity.Role, &r.Identity.UserType, &groups,
		&r.Identity.GroupsTruncated, &r.Identity.RevSeq, &r.Identity.AuthorityEpoch, &r.IdentityStampedAt, &r.Machine,
		&r.Store, &r.PublicKey, &r.SecretSHA256, &r.Generation, &r.CreatedAt, &r.LastRefreshAt, &r.ExpiresAt, &r.EndedAt,
		&r.EndReason}, extra...)
	if err := row.Scan(dest...); errors.Is(err, pgx.ErrNoRows) {
		return CLISessionRow{}, ErrNotFound
	} else if err != nil {
		return CLISessionRow{}, fmt.Errorf("store: scan cli session: %w", err)
	}
	if groups != nil {
		if err := json.Unmarshal(groups, &r.Identity.Groups); err != nil {
			return CLISessionRow{}, fmt.Errorf("store: scan cli session groups: %w", err)
		}
	}
	return r, nil
}

// cliTx runs fn on one transaction that, when ctx carries an IdentityGuard (guardOwner), first holds
// the owner's identity rows as guarded does, so a token is not written for a deactivated person.
func (s PG) cliTx(ctx context.Context, fn func(q pgx.Tx) error) error {
	g, guard := identityGuardFrom(ctx)
	var since int64
	if guard {
		var err error
		if since, err = ReadRevocationSeq(ctx, s.Pool); err != nil {
			return err
		}
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("store: begin cli session write: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if guard {
		if err := checkIdentityGuard(ctx, tx, g, since); err != nil {
			return err
		}
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateCLISession inserts sess (secret hashed) and tok, which must name sess.ID as its CLI session.
func (s PG) CreateCLISession(ctx context.Context, sess CLISessionRow, secret string, tok types.APIToken, raw string) (types.APIToken, error) {
	groups, err := marshalGroups(sess.Identity.Groups)
	if err != nil {
		return types.APIToken{}, err
	}
	var out types.APIToken
	err = s.cliTx(ctx, func(tx pgx.Tx) error {
		id := sess.Identity
		if _, err := tx.Exec(ctx, `
			INSERT INTO cli_sessions (id, principal, email, role, user_type, groups, groups_truncated, rev_seq, authority_epoch,
			                          identity_stamped_at, machine, store, public_key, secret_sha256, created_at, last_refresh_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $15, $16)`,
			sess.ID, id.Principal, id.Email, id.Role, id.UserType, groups, id.GroupsTruncated, id.RevSeq, id.AuthorityEpoch,
			sess.IdentityStampedAt, sess.Machine, sess.Store, sess.PublicKey, hashToken(secret), sess.CreatedAt, sess.ExpiresAt); err != nil {
			return fmt.Errorf("store: create cli session: %w", err)
		}
		var e error
		out, e = s.insertAPIToken(ctx, tx, tok, raw)
		return e
	})
	return out, err
}

// GetCLISession reads one session, whatever its state.
func (s PG) GetCLISession(ctx context.Context, id uuid.UUID) (CLISessionRow, error) {
	return scanCLISession(s.Pool.QueryRow(ctx, `SELECT `+cliSessionCols+` FROM cli_sessions WHERE id = $1`, id))
}

// RotateCLISession is the refresh's write.
func (s PG) RotateCLISession(ctx context.Context, id uuid.UUID, generation int64, newSecret string, now time.Time, tok types.APIToken, raw string) (types.APIToken, error) {
	var out types.APIToken
	err := s.cliTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE cli_sessions SET secret_sha256 = $3, generation = generation + 1, last_refresh_at = $4
			WHERE id = $1 AND generation = $2 AND ended_at IS NULL`, id, generation, hashToken(newSecret), now)
		if err != nil {
			return fmt.Errorf("store: rotate cli session: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		out, err = s.insertAPIToken(ctx, tx, tok, raw)
		return err
	})
	return out, err
}

// CreateCLIAdminToken holds the session row while it inserts tok, so an end that commits first refuses
// the mint (ErrNotFound), and one that commits after it revokes the token.
func (s PG) CreateCLIAdminToken(ctx context.Context, sessionID uuid.UUID, tok types.APIToken, raw string) (types.APIToken, error) {
	var out types.APIToken
	err := s.cliTx(ctx, func(tx pgx.Tx) error {
		var live bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cli_sessions WHERE id = $1 AND ended_at IS NULL FOR SHARE)`,
			sessionID).Scan(&live); err != nil {
			return fmt.Errorf("store: read cli session: %w", err)
		}
		if !live {
			return ErrNotFound
		}
		var err error
		out, err = s.insertAPIToken(ctx, tx, tok, raw)
		return err
	})
	return out, err
}

// endCLISessionsSQL ends every live CLI session a cut naming $1 reaches ("" = every one), matched as
// the cut's own check matches (the subject exactly, the email ignoring case), and revokes the tokens of
// those sessions, stamping $2 as the reason. Every writer of oidc_session_revocations and
// oidc_session_cuts runs it in the same transaction as its cut (cmd/wardynd's census guard pins that),
// so a revoke or a sign-out ends the person's CLI sessions at once rather than at their next refresh.
const endCLISessionsSQL = `
	WITH ended AS (
		UPDATE cli_sessions SET ended_at = clock_timestamp(), end_reason = $2
		WHERE ended_at IS NULL AND ($1 = '' OR principal = $1 OR lower(email) = lower($1))
		RETURNING id)
	UPDATE api_tokens SET revoked_at = clock_timestamp()
	WHERE revoked_at IS NULL AND cli_session_id IN (SELECT id FROM ended)`

// endCLISessionsOf runs endCLISessionsSQL on q, the cut's own transaction.
func endCLISessionsOf(ctx context.Context, q Querier, name, reason string) error {
	if _, err := q.Exec(ctx, endCLISessionsSQL, name, reason); err != nil {
		return fmt.Errorf("store: end cli sessions: %w", err)
	}
	return nil
}

// EndCLISession ends id and revokes its tokens.
func (s PG) EndCLISession(ctx context.Context, id uuid.UUID, principal, reason string, now time.Time) (CLISessionRow, error) {
	var out CLISessionRow
	err := s.inTx(ctx, func(q Querier) error {
		var err error
		out, err = scanCLISession(q.QueryRow(ctx, `
			UPDATE cli_sessions SET ended_at = $4, end_reason = $3
			WHERE id = $1 AND ended_at IS NULL AND ($2 = '' OR principal = $2)
			RETURNING `+cliSessionCols, id, principal, reason, now))
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE api_tokens SET revoked_at = $2 WHERE cli_session_id = $1 AND revoked_at IS NULL`, id, now); err != nil {
			return fmt.Errorf("store: revoke cli session tokens: %w", err)
		}
		return nil
	})
	return out, err
}

// ListCLISessions is principal's live sessions with the expiry of a live cli_admin token on each.
func (s PG) ListCLISessions(ctx context.Context, principal string, now time.Time) ([]CLISessionRow, error) {
	return collect(ctx, s.Pool, "list", "cli sessions", `
		SELECT `+cliSessionCols+`,
		       (SELECT max(t.expires_at) FROM api_tokens t
		         WHERE t.cli_session_id = cli_sessions.id AND t.kind = 'cli_admin' AND t.revoked_at IS NULL AND t.expires_at > $2)
		FROM cli_sessions WHERE principal = $1 AND ended_at IS NULL AND expires_at > $2
		ORDER BY created_at DESC, id`, []any{principal, now}, func(row pgx.Row) (CLISessionRow, error) {
		var admin *time.Time
		r, err := scanCLISession(row, &admin)
		r.AdminExpiresAt = admin
		return r, err
	})
}

// CountLiveCLISessions counts what endCLISessionsSQL would end for name.
func (s PG) CountLiveCLISessions(ctx context.Context, name string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM cli_sessions
		WHERE ended_at IS NULL AND ($1 = '' OR principal = $1 OR lower(email) = lower($1))`, name).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count cli sessions: %w", err)
	}
	return n, nil
}
