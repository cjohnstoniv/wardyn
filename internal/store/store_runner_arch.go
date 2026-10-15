// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import "context"

// RunnerArchCounts counts the non-revoked runners per reported CPU architecture. A runner that has not
// reported one is not counted.
func (s PG) RunnerArchCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.Pool.Query(ctx, `SELECT arch, count(*) FROM runners WHERE state <> 'revoked' AND arch <> '' GROUP BY arch`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var arch string
		var n int
		if err := rows.Scan(&arch, &n); err != nil {
			return nil, err
		}
		counts[arch] = n
	}
	return counts, rows.Err()
}
