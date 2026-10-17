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
