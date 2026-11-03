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

// RunValueMasker masks the secrets registered for runID out of b, the way the audit recorder masks an
// event's data. The id the enricher adds comes from the run row, after that masking has run, so it
// passes through here on every emit.
type RunValueMasker func(runID uuid.UUID, b []byte) []byte

const (
	correlationCacheMax     = 4096
	correlationLookupBudget = 2 * time.Second
)

// correlationEnricher stamps a run's correlation ID into the data of its events. A run's ID is written
// once at create, so a hit never goes stale; a run without one is cached as "" so it costs one read.
// The cache holds the raw id and mask runs on every emit, so a secret registered after the first hit
// still covers it.
// ponytail: a full cache is emptied whole; an LRU if a deployment's live run count makes refills show.
type correlationEnricher struct {
	lookup RunCorrelationLookup
	mask   RunValueMasker
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
	if c.mask != nil {
		id = c.masked(*ev.RunID, id)
		if id == "" {
			return ev
		}
	}
	if data, ok := stampCorrelation(ev.Data, id); ok {
		ev.Data = data // a new slice: the caller's event, which the store already took, is untouched
	}
	return ev
}

// masked is id with the run's registered secrets replaced, "" when what comes back is not a JSON string
// (the key is then left off rather than sent unmasked).
func (c *correlationEnricher) masked(runID uuid.UUID, id string) string {
	quoted, err := json.Marshal(id)
	if err != nil {
		return ""
	}
	quoted = c.mask(runID, quoted)
	var out string
	if json.Unmarshal(quoted, &out) != nil {
		return ""
	}
	return out
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
// sink sees data.correlation_id and no emit site changes. mask, when set, is applied to the id on every
// emit. Call it once at boot, before the first Emit.
func (f *Fanout) EnrichRuns(lookup RunCorrelationLookup, mask RunValueMasker) {
	f.enrich = &correlationEnricher{lookup: lookup, mask: mask, ids: map[uuid.UUID]string{}}
}
