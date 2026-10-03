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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// Verify is the key-domain boot check over the live generations (those not
// destroyed, superseded ones included, because their rows are still read): none
// may name a domain other than DomainDefault and the declared ones, and, when
// reach is not nil, none may name a kek_id its domain's reach refuses.
//
// Both refusals count the rows per domain and name the remedy; the caller
// prefixes what it is refusing. It is never to
// delete rows: re-declare the domain, erase or destroy those subjects through
// the API, then remove the domain. Destroyed generations hold no key and are not
// counted.
func Verify(ctx context.Context, pool *pgxpool.Pool, declared []string, reach func(domain, kekID string) (kek.KEK, error)) error {
	rows, err := pool.Query(ctx, `SELECT domain, kek_id, count(*) FROM principal_keys WHERE destroyed_at IS NULL GROUP BY domain, kek_id ORDER BY domain, kek_id`)
	if err != nil {
		return unavailable("count the live keys per domain", err)
	}
	live, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Domain, KEKID string
		N             int
	}])
	if err != nil {
		return unavailable("count the live keys per domain", err)
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

// domainRemedy is the one instruction a domain refusal gives. It never suggests
// deleting rows: a row deleted is a key lost, and what it sealed with it.
const domainRemedy = "Re-declare the domain (or restore the key it names), then erase or destroy those subjects through the API " +
	"(DELETE /api/v1/people/{principal}/credentials), and only then remove the domain. Never delete the rows"
