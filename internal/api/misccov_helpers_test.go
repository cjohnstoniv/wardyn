// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// miscCovClosedPool is a pool that has been closed: every statement against it fails at once, with no
// network and no timing. closedErr is the error the pool answers with, so a test can prove a store
// error was wrapped (errors.Is) and not replaced.
func miscCovClosedPool(t *testing.T) (pool *pgxpool.Pool, closedErr error) {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), "postgres://wardyn@127.0.0.1:1/wardyn?sslmode=disable")
	if err != nil {
		t.Fatalf("build the pool: %v", err)
	}
	pool.Close()
	closedErr = pool.Ping(t.Context())
	if closedErr == nil {
		t.Fatal("a closed pool answered a ping")
	}
	return pool, closedErr
}

// miscCovLogs records every slog record the default logger takes while a test runs.
type miscCovLogs struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (l *miscCovLogs) Enabled(context.Context, slog.Level) bool { return true }
func (l *miscCovLogs) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *miscCovLogs) WithGroup(string) slog.Handler            { return l }
func (l *miscCovLogs) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	l.recs = append(l.recs, r)
	l.mu.Unlock()
	return nil
}

// find returns the first record whose message contains text.
func (l *miscCovLogs) find(text string) (slog.Record, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if strings.Contains(r.Message, text) {
			return r, true
		}
	}
	return slog.Record{}, false
}

// attr is the value of key on r.
func (l *miscCovLogs) attr(r slog.Record, key string) (slog.Value, bool) {
	var v slog.Value
	var ok bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v, ok = a.Value, true
			return false
		}
		return true
	})
	return v, ok
}

func miscCovCaptureLogs(t *testing.T) *miscCovLogs {
	t.Helper()
	l := &miscCovLogs{}
	prev := slog.Default()
	slog.SetDefault(slog.New(l))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return l
}

// miscCovLocker is a db.Locker that takes every lock, or refuses with refuse, and counts the unlocks.
type miscCovLocker struct {
	refuse  error
	mu      sync.Mutex
	locks   int
	unlocks int
}

func (l *miscCovLocker) Lock(ctx context.Context, _ db.LockKey, _ time.Duration) (context.Context, func(), error) {
	if l.refuse != nil {
		return ctx, nil, l.refuse
	}
	l.mu.Lock()
	l.locks++
	l.mu.Unlock()
	return ctx, func() { l.mu.Lock(); l.unlocks++; l.mu.Unlock() }, nil
}

func (l *miscCovLocker) TryLock(ctx context.Context, k db.LockKey) (context.Context, func(), bool, error) {
	c, unlock, err := l.Lock(ctx, k, 0)
	return c, unlock, err == nil, err
}

func (l *miscCovLocker) counts() (locks, unlocks int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.locks, l.unlocks
}
