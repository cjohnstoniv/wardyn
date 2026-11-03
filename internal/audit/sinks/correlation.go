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

// RunCoverage reports whether the run's masking is provably whole here. Only then is its correlation id
// added: a fenced or erased corpus, or an answer that cannot be confirmed, is false.
type RunCoverage func(runID uuid.UUID) bool

const (
	correlationCacheMax     = 4096
	correlationCacheTTL     = 30 * time.Second
	correlationLookupBudget = 2 * time.Second
)

// correlationEnricher stamps a run's correlation ID into the data of its events. A run's ID is written
// at create and cleared only by a person's erasure, so an entry lives correlationCacheTTL: another
// replica's erasure reaches this one within that, and this replica's own is applied at once by forget.
// A run without an ID is cached as "" so it costs one read. The cache holds the raw id and mask runs on
// every emit, so a secret registered after the first hit still covers it.
// ponytail: a full cache is emptied whole; an LRU if a deployment's live run count makes refills show.
type correlationEnricher struct {
	lookup RunCorrelationLookup
	mask   RunValueMasker
	covers RunCoverage
	now    func() time.Time
	mu     sync.Mutex
	ids    map[uuid.UUID]cachedCorrelation
	gen    uint64 // bumped by forget: a read that began before it must not be cached
}

type cachedCorrelation struct {
	id      string
	expires time.Time
}

// forget drops runs' entries and invalidates reads in flight, so the next emit of each reads the row again.
func (c *correlationEnricher) forget(runIDs []uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	for _, id := range runIDs {
		delete(c.ids, id)
	}
}

func (c *correlationEnricher) apply(ctx context.Context, ev types.AuditEvent) types.AuditEvent {
	if ev.RunID == nil {
		return ev
	}
	if c.covers != nil && !c.covers(*ev.RunID) {
		return ev // asked on every emit, before the cache: a fence applies at once, warm or cold
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
	// Again, now that the id is masked: an erasure that fenced the run and evicted its corpus while the read
	// or the masking ran left the masker empty. The fence is durable and set before the eviction (maskmanifest
	// FenceSubject and Forget drop the held manifest, then the registry's values), and a fenced run is never
	// held again, so a run still held here was masked by its whole corpus.
	if c.covers != nil && !c.covers(*ev.RunID) {
		return ev
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
	hit, ok := c.ids[runID]
	gen := c.gen
	c.mu.Unlock()
	if ok && c.now().Before(hit.expires) {
		return hit.id, true
	}
	started := c.now() // the entry's clock starts before the read, so a slow read cannot stretch the bound after an erasure
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
	defer c.mu.Unlock()
	if c.gen != gen {
		return "", false // an erasure ran while this read was in flight: what it read may be the id it cleared
	}
	if len(c.ids) >= correlationCacheMax {
		clear(c.ids)
	}
	c.ids[runID] = cachedCorrelation{id: id, expires: started.Add(correlationCacheTTL)}
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
// emit, and covers, when set, gates it: a run it does not vouch for gets no id. Call it once at boot,
// before the first Emit.
func (f *Fanout) EnrichRuns(lookup RunCorrelationLookup, mask RunValueMasker, covers RunCoverage) {
	f.enrich = &correlationEnricher{lookup: lookup, mask: mask, covers: covers, now: time.Now, ids: map[uuid.UUID]cachedCorrelation{}}
}

// ForgetRuns drops what EnrichRuns cached for runIDs, for a person's erasure of their runs' correlation IDs
// on this replica: call it after the rows are cleared. Other replicas follow within the cache's 30 seconds.
func (f *Fanout) ForgetRuns(runIDs []uuid.UUID) {
	if f.enrich != nil {
		f.enrich.forget(runIDs)
	}
}
