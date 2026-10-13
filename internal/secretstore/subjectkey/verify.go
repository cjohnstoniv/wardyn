// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package subjectkey

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// countLiveErr is the read of principal_keys failing. A missing table is definitive and has a
// remedy, where unavailable would read as a Postgres outage: this runs from -rotate-age-key and
// -rewrap, which do not migrate, against a database no serving boot of this release has reached yet.
func countLiveErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return errors.New("subjectkey: principal_keys does not exist: this database is not migrated to this release; " +
			"run `wardynd -migrate-only` (or boot the server once) first")
	}
	return unavailable("count the live keys per domain", err)
}

// Verify is the key-domain boot check over the live generations (those not
// destroyed, superseded ones included, because their rows are still read): none
// may name a domain other than DomainDefault and the declared ones, and, when
// reach is not nil, none may name a kek_id its domain's reach refuses.
//
// Both refusals count the rows per domain and name the remedy; the caller
// prefixes what it is refusing. It is never to delete rows, and never an
// erasure made to get past the check: keep the domain declared, and remove it
// only once no live key names it. Every purpose counts, so a person's
// audit-seal key keeps a domain live after their credentials are erased.
// Destroyed generations hold no key and are not counted.
func Verify(ctx context.Context, pool *pgxpool.Pool, declared []string, reach func(domain, kekID string) (kek.KEK, error)) error {
	rows, err := pool.Query(ctx, `SELECT domain, kek_id, count(*) FROM principal_keys WHERE destroyed_at IS NULL GROUP BY domain, kek_id ORDER BY domain, kek_id`)
	if err != nil {
		return countLiveErr(err)
	}
	live, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Domain, KEKID string
		N             int
	}])
	if err != nil {
		return countLiveErr(err)
	}
	undeclared := map[string]int{}
	var unreachable []string
	for _, l := range live {
		if l.Domain != DomainDefault && !slices.Contains(declared, l.Domain) {
			undeclared[l.Domain] += l.N
			continue
		}
		if reach == nil {
			continue
		}
		if _, err := reach(l.Domain, l.KEKID); err != nil {
			unreachable = append(unreachable, fmt.Sprintf("%d live principal keys in domain %q name key %q (%v)", l.N, l.Domain, l.KEKID, err))
		}
	}
	var problems []string
	for _, d := range slices.Sorted(maps.Keys(undeclared)) {
		problems = append(problems, fmt.Sprintf("%d live principal keys name key domain %q, which WARDYN_KEY_DOMAINS_FILE does not declare", undeclared[d], d))
	}
	problems = append(problems, unreachable...)
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; ") + ". " + domainRemedy)
}

// domainRemedy is the one instruction a domain refusal gives. It leads with
// the recovery that erases nothing, and tells a person's two keys apart:
// credential erasure destroys the credential key only, and the audit-seal key
// goes only with an irreversible erasure that a retention decision must allow.
// It never suggests deleting rows: a row deleted is a key lost, and what it
// sealed with it.
const domainRemedy = "Keep the domain declared: re-declare it (or restore the key it names), which erases nothing. " +
	"Remove the domain only once no live key names it. DELETE /api/v1/people/{principal}/credentials destroys a person's credential key " +
	"and leaves their audit-seal key live; that key is destroyed only by the audit_personal_fields erasure scope " +
	"(POST /api/v1/people/{principal}/erasure), which is irreversible: every sealed audit field of theirs then reads [erased]. " +
	"Use it only where your retention decisions allow it; otherwise keep the domain declared. Never delete the rows"
