// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// CorrelationKey is the key a run's correlation ID rides under in the data of every run-scoped event a
// sink receives. It is stamped on the way to the sinks only: the stored row, its hash chain and the
// spool never carry it, and it is a top-level audit field nowhere.
const CorrelationKey = "correlation_id"

// RunCorrelationLookup reads a run's correlation ID. found is false for a run it does not hold yet.
type RunCorrelationLookup func(ctx context.Context, runID uuid.UUID) (id string, found bool, err error)

const (
	correlationCacheMax     = 4096
	correlationLookupBudget = 2 * time.Second
)

// correlationEnricher stamps a run's correlation ID into the data of its events. A run's ID is written
// once at create, so a hit never goes stale; a run without one is cached as "" so it costs one read.
// ponytail: a full cache is emptied whole; an LRU if a deployment's live run count makes refills show.
type correlationEnricher struct {
	lookup RunCorrelationLookup
	mu     sync.Mutex
	ids    map[uuid.UUID]string
}

func (c *correlationEnricher) apply(ctx context.Context, ev types.AuditEvent) types.AuditEvent {
	if ev.RunID == nil {
		return ev
	}
	id, ok := c.idOf(ctx, *ev.RunID)
	if !ok || id == "" {
		return ev
	}
	if data, ok := stampCorrelation(ev.Data, id); ok {
		ev.Data = data // a new slice: the caller's event, which the store already took, is untouched
	}
	return ev
}

func (c *correlationEnricher) idOf(ctx context.Context, runID uuid.UUID) (string, bool) {
	c.mu.Lock()
	id, hit := c.ids[runID]
	c.mu.Unlock()
	if hit {
		return id, true
	}
	ctx, cancel := context.WithTimeout(ctx, correlationLookupBudget)
	defer cancel()
	id, found, err := c.lookup(ctx, runID)
	if err != nil {
		slog.WarnContext(ctx, "sinks: reading a run's correlation id failed; its event goes out without it", slog.Any("err", err))
		return "", false
	}
	if !found {
		return "", false
	}
	c.mu.Lock()
	if len(c.ids) >= correlationCacheMax {
		clear(c.ids)
	}
	c.ids[runID] = id
	c.mu.Unlock()
	return id, true
}

// stampCorrelation returns data with CorrelationKey set to id. A payload that is not a JSON object is
// left alone (false): the sinks must still get the event.
func stampCorrelation(data json.RawMessage, id string) (json.RawMessage, bool) {
	obj := map[string]json.RawMessage{}
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
		if err := json.Unmarshal(trimmed, &obj); err != nil || obj == nil {
			return data, false
		}
	}
	quoted, err := json.Marshal(id)
	if err != nil {
		return data, false
	}
	obj[CorrelationKey] = quoted
	out, err := json.Marshal(obj)
	if err != nil {
		return data, false
	}
	return out, true
}

// EnrichRuns has Emit stamp each run-scoped event's data with that run's correlation ID, so every child
// sink sees data.correlation_id and no emit site changes. Call it once at boot, before the first Emit.
func (f *Fanout) EnrichRuns(lookup RunCorrelationLookup) {
	f.enrich = &correlationEnricher{lookup: lookup, ids: map[uuid.UUID]string{}}
}
