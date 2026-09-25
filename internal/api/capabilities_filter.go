// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"slices"
)

// capFilter is the list half of the one grant resolver: the ids of `kind` the
// caller may use, in input order. Each id is answered by capBatch.allowed, the
// very call a door refuses with, so a list can never offer what the launch door
// would refuse, nor hide what it would allow.
//
// One batch answers the whole list (capBatchFor): a caller-sized list costs the
// same reads as one value. An error means the batch could not be read at all —
// never a per-id answer — and the caller gets no ids with it.
func (s *Server) capFilter(ctx context.Context, kind string, ids []string) ([]string, error) {
	b := s.capBatchFor(ctx)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		ok, err := b.allowed(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, id)
		}
	}
	return out, nil
}

// capVisible keeps the rows of a per-person list carrier whose id capFilter
// keeps. A refused row is dropped whole, so the list reads exactly as it would
// if the resource did not exist: no placeholder, no count, no reason.
//
// Fails closed. A resolver error hides every row rather than failing the
// response: the carriers are read-only views a console polls (GET /setup/status
// is its reachability gate), the launch doors still refuse on their own, and an
// empty list is the one answer that can neither grant nor leak.
func capVisible[T any](ctx context.Context, s *Server, kind string, rows []T, id func(T) string) []T {
	if len(rows) == 0 {
		return rows
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = id(row)
	}
	keep, err := s.capFilter(ctx, kind, ids)
	if err != nil {
		slog.Warn("api: capability list filter failed; hiding every row", "capability", kind, "err", err)
		return rows[:0:0]
	}
	out := make([]T, 0, len(keep))
	for i, row := range rows {
		if slices.Contains(keep, ids[i]) {
			out = append(out, row)
		}
	}
	return out
}
