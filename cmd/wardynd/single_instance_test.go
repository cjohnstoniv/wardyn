// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// captureSlog routes the default logger into a buffer for the test.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// lazyPool builds a pool that never dials (pgxpool connects on first use), so
// the boot refusal is testable without a database.
func lazyPool(t *testing.T, maxConns int) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig("postgres://wardyn:wardyn@127.0.0.1:1/wardyn")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = int32(maxConns)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// WARDYN_HA takes no lock and puts no floor on the pool: the sweeper leader
// elects on a connection of its own, so a pool of one still fences.
func TestClaimSingleInstance_HAAcceptsAPoolOfOne(t *testing.T) {
	release, err := claimSingleInstance(context.Background(), lazyPool(t, 1), true)
	if err != nil {
		t.Fatalf("WARDYN_HA was refused on pool_max_conns=1: %v", err)
	}
	release()
}

// The boot warning is about the two tick locks only: below 4 it fires, names
// the threshold and the sign-in note, and has nothing to say about the rotator,
// which no longer borrows from the pool.
func TestWarnSmallPool_BelowFourAndNoRotatorBranch(t *testing.T) {
	buf := captureSlog(t)
	warnSmallPool(4)
	if buf.Len() != 0 {
		t.Fatalf("pool_max_conns=4 warned:\n%s", buf.String())
	}
	warnSmallPool(3)
	out := buf.String()
	for _, want := range []string{"level=WARN", "pool_max_conns below 4", "pool_max_conns=3", "Below 6", "auth.signin_unserialized"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning never says %q:\n%s", want, out)
		}
	}
	for _, gone := range []string{"rotator", "single-instance", "sweeper leader"} {
		if strings.Contains(out, gone) {
			t.Errorf("warning still names %q, which holds no pool connection:\n%s", gone, out)
		}
	}
	// The call is unconditional: it must not sit inside the rotator's branch.
	src := readRepo(t, "cmd/wardynd/boot_serve.go")
	call, branch := strings.Index(src, "warnSmallPool(pool.Config().MaxConns)"), strings.Index(src, `os.Getenv("WARDYN_GROUNDTRUTH_TOKEN_FILE")`)
	if call < 0 || branch < 0 || call > branch {
		t.Errorf("warnSmallPool is not called ahead of the rotator branch (call at %d, branch at %d)", call, branch)
	}
}
