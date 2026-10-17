// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package livebus carries the notices replicas send each other over Postgres NOTIFY (ha-l2.4):
// a run's lifecycle events, a kill that must reach the replica creating the run's sandbox, and
// the end of an attach lease. A notice is a hint, never a record. NOTIFY is not durable, so
// each use has a path that needs none: the events stream re-reads the store every beat, a lost
// kill is caught by the dispatch's STARTING to RUNNING compare, and an attach lease lapses or
// is found lost by its holder. Losing the listener loses only promptness.
//
// Each replica holds one connection of its own, outside the request pool, LISTENing on Channel.
// A notice carries ids and small vocabulary values, never a secret or output.
package livebus

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Channel is the NOTIFY channel every notice rides.
const Channel = "wardyn_live"

// maxPayload is under Postgres's 8000-byte NOTIFY limit.
const maxPayload = 7500

// listenRetry spaces the listener's reconnects.
const listenRetry = 2 * time.Second

// The notice kinds.
const (
	// KindRunEvent: a lifecycle event of a run, for the other replicas' event rings.
	KindRunEvent = "run_event"
	// KindRunKill: a run was killed; the replica creating its sandbox cancels the create.
	KindRunKill = "run_kill"
	// KindAttachEvict: a run's attach holder on another replica was taken over.
	KindAttachEvict = "attach_evict"
	// KindAttachFree: a run's attach lease was released; observers queued on any replica may take it.
	KindAttachFree = "attach_free"
)

// Message is one notice.
type Message struct {
	Kind   string          `json:"k"`
	Run    uuid.UUID       `json:"r"`
	Origin string          `json:"o"`
	Data   json.RawMessage `json:"d,omitempty"`
}

// Bus is one replica's end. Safe for concurrent use.
type Bus struct {
	pool   *pgxpool.Pool
	origin string

	mu       sync.Mutex
	handlers map[string][]func(Message)
}

// New returns a Bus over pool whose notices carry origin, this replica's identity.
func New(pool *pgxpool.Pool, origin string) *Bus {
	return &Bus{pool: pool, origin: origin, handlers: map[string][]func(Message){}}
}

// Origin is this replica's identity.
func (b *Bus) Origin() string { return b.origin }

// Handle registers fn for kind. It runs on the listener's goroutine, so it must return quickly,
// and it also receives the notices this replica published (compare Message.Origin).
func (b *Bus) Handle(kind string, fn func(Message)) {
	b.mu.Lock()
	b.handlers[kind] = append(b.handlers[kind], fn)
	b.mu.Unlock()
}

// Publish announces a notice of kind about run to every replica, this one included. An error
// means the notice was not sent; every caller has a path that does not need it.
func (b *Bus) Publish(ctx context.Context, kind string, run uuid.UUID, data any) error {
	m := Message{Kind: kind, Run: run, Origin: b.origin}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("livebus: encode a notice: %w", err)
		}
		m.Data = raw
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("livebus: encode a notice: %w", err)
	}
	if len(payload) > maxPayload {
		return fmt.Errorf("livebus: a %s notice of %d bytes is too large", kind, len(payload))
	}
	if _, err := b.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, Channel, string(payload)); err != nil {
		return fmt.Errorf("livebus: publish: %w", err)
	}
	return nil
}

// Start listens until ctx ends. It returns at once.
func (b *Bus) Start(ctx context.Context) { go b.listen(ctx) }

func (b *Bus) listen(ctx context.Context) {
	for ctx.Err() == nil {
		b.listenOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(listenRetry):
		}
	}
}

func (b *Bus) listenOnce(ctx context.Context) {
	conn, err := pgx.ConnectConfig(ctx, b.pool.Config().ConnConfig.Copy())
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+Channel); err != nil {
		return
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return
		}
		b.dispatch(ctx, n.Payload)
	}
}

func (b *Bus) dispatch(ctx context.Context, payload string) {
	var m Message
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		slog.WarnContext(ctx, "livebus: a notice did not parse; dropped", slog.Any("err", err))
		return
	}
	b.mu.Lock()
	fns := append([]func(Message){}, b.handlers[m.Kind]...)
	b.mu.Unlock()
	for _, fn := range fns {
		fn(m)
	}
}
