// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGConn opens a separate session for a test's competing database actor. It
// must not borrow the production path's pool: that would be a nested acquire
// by the test itself, obscuring what the pool guard is meant to prove.
func PGConn(t *testing.T, pool *pgxpool.Pool) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = conn.Close(ctx)
	})
	return conn
}

// SetBoundaryNoLimits puts the Wardyn boundary in no_limits with an empty spec and limits: the
// posture an upgraded install has. A test about that posture calls it right after db.Migrate; no
// test writes the boundary row any other way.
func SetBoundaryNoLimits(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO governance_boundary (singleton, mode, spec, limits) VALUES (true, 'no_limits', '{}', '{}')
		ON CONFLICT (singleton) DO UPDATE SET mode = 'no_limits', spec = '{}', limits = '{}', contact = NULL`); err != nil {
		t.Fatalf("set the boundary to no_limits: %v", err)
	}
}
