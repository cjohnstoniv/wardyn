// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// dockerCorrelator maps container ids to Wardyn run ids by listing docker
// containers labelled wardyn.managed=true.
//
// DEPENDENCY CHOICE: this SHELLS OUT to the `docker` CLI rather than importing
// github.com/docker/docker/client. The runner's docker driver already lives
// behind a `docker` build tag (parity rule); pulling that client into a host
// sidecar binary would either drag the heavy client graph into the default
// build or force a second build tag here. The sidecar already needs the docker
// socket and runs on a host with docker present, so `docker ps`/`inspect` is
// the lighter, honest path — and it keeps this binary in the default build with
// zero new module dependencies.
//
// It implements groundtruth.Correlator. The index is fed by TWO sources: a
// periodic (and on-miss, throttled) `docker ps` snapshot, and — the load-bearing
// one — a `docker events` stream that adds a container the moment it is CREATED.
//
// FINDING (E1, 0.6 "frozen counter"): a poll alone can NEVER correlate a
// short-lived run. `docker ps` listed only RUNNING containers and Refresh
// REPLACED the index wholesale, while the Tetragon export is tailed with lag
// (the shipped policy writes thousands of events/s). A container that started
// and exited between two snapshots — or that exited before the tail caught up —
// resolved unmapped, was dropped by gatedMapper, and moved no counter at all:
// the filed symptom (ROADMAP "frozen control-plane counter"). The event stream
// removes the race (docker emits `create` before the container's first exec),
// `-a` covers an exited-but-not-yet-removed container on a cold start, and index
// entries now SURVIVE their container by containerRetention so a lagging tail
// still correlates.

// dockerRefreshTimeout bounds a single `docker ps` query so a wedged docker
// daemon wedges only this call, not the caller (RunForContainer runs inline
// on the tail loop via maybeRefresh).
const dockerRefreshTimeout = 10 * time.Second

// containerRetention is how long a container->run mapping stays resolvable
// after the container stops being listed (exit, then teardown removal). It
// bounds the tail lag we can absorb: an event mapped later than this correlates
// as unmapped again — visibly, via dropped_unmapped.
//
// ponytail: a time-based sweep on the refresh tick, no size cap — the index only
// ever holds wardyn AGENT containers seen in the last 15 minutes. If a
// deployment churns enough runs for that to matter, cap the map here (evict
// oldest seen).
const containerRetention = 15 * time.Minute

// watchRestartDelay is the pause before re-attaching to `docker events` after
// the stream ends (daemon restart, socket blip). The refresh ticker keeps the
// index reconciled across the gap.
const watchRestartDelay = 2 * time.Second

type dockerCorrelator struct {
	mu          sync.RWMutex
	byContainer map[string]indexEntry // full + truncated (12-char) ids -> run
	lastRefresh time.Time
	// minRefreshGap throttles on-miss refreshes so a flood of unmapped events
	// cannot spawn a `docker ps` storm.
	minRefreshGap time.Duration
	// retain is containerRetention; a field so tests can shorten it.
	retain time.Duration

	docker  dockerLister  // injectable for tests
	watcher dockerWatcher // injectable for tests; nil disables Watch
}

// indexEntry is one container->run mapping plus the last time the container was
// seen (listed by `docker ps -a` or announced by `docker events`). seen drives
// retention: an entry whose container has vanished is kept for
// containerRetention so events the tail delivers late still correlate.
type indexEntry struct {
	run  uuid.UUID
	seen time.Time
}

// dockerLister abstracts the docker query so tests can avoid a real daemon.
type dockerLister interface {
	// managedContainers returns the wardyn.managed=true agent containers as a
	// slice of (id, runID) pairs. Proxy containers are excluded (only the agent
	// container's run-id is what kernel events should correlate to).
	managedContainers(ctx context.Context) ([]managedContainer, error)
}

// dockerWatcher abstracts the `docker events` stream so tests can avoid a real
// daemon. It is what makes a short-lived container correlatable at all.
type dockerWatcher interface {
	// watchContainers streams container lifecycle events, calling add for every
	// wardyn-managed AGENT container it sees, until ctx is done or the stream
	// fails (the returned error).
	watchContainers(ctx context.Context, add func(managedContainer)) error
}

type managedContainer struct {
	ID    string // full container id
	RunID uuid.UUID
}

func newDockerCorrelator(lister dockerLister, watcher dockerWatcher, minRefreshGap time.Duration) *dockerCorrelator {
	if lister == nil {
		lister = cliDockerLister{}
		if watcher == nil {
			watcher = cliDockerLister{}
		}
	}
	return &dockerCorrelator{
		byContainer:   map[string]indexEntry{},
		minRefreshGap: minRefreshGap,
		retain:        containerRetention,
		docker:        lister,
		watcher:       watcher,
	}
}

// RunForContainer resolves a container id (Tetragon may emit a truncated id) to
// a run. On a miss it triggers a throttled refresh and retries once, so a
// just-started run is picked up promptly.
func (c *dockerCorrelator) RunForContainer(id string) (uuid.UUID, bool) {
	if id == "" {
		return uuid.Nil, false
	}
	if r, ok := c.lookupContainer(id); ok {
		return r, true
	}
	// Miss: maybe the index is stale (new run). Refresh (throttled) and retry.
	if c.maybeRefresh(context.Background()) {
		if r, ok := c.lookupContainer(id); ok {
			return r, true
		}
	}
	return uuid.Nil, false
}

func (c *dockerCorrelator) lookupContainer(id string) (uuid.UUID, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if e, ok := c.byContainer[id]; ok {
		return e.run, true
	}
	// Tetragon emits a truncated id (31 chars on the v1.1.2 export this ships
	// against); index the full and 12-char forms, but also try a prefix match
	// against full ids — in practice that IS the path every event takes.
	for full, e := range c.byContainer {
		if strings.HasPrefix(full, id) || strings.HasPrefix(id, full) {
			return e.run, true
		}
	}
	return uuid.Nil, false
}

// add records a container->run mapping (both id forms) and stamps it seen now.
// Called by the event watcher and by Refresh.
func (c *dockerCorrelator) add(mc managedContainer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addLocked(mc, time.Now())
}

func (c *dockerCorrelator) addLocked(mc managedContainer, now time.Time) {
	c.byContainer[mc.ID] = indexEntry{run: mc.RunID, seen: now}
	if len(mc.ID) >= 12 {
		c.byContainer[mc.ID[:12]] = indexEntry{run: mc.RunID, seen: now} // short-id form
	}
}

// Watch keeps the index fed from the docker event stream until ctx is done,
// re-attaching after the stream ends. This is the source that makes a container
// resolvable BEFORE its first kernel event — see the type doc.
func (c *dockerCorrelator) Watch(ctx context.Context) {
	if c.watcher == nil {
		return
	}
	for ctx.Err() == nil {
		err := c.watcher.watchContainers(ctx, c.add)
		if ctx.Err() != nil {
			return
		}
		slog.WarnContext(ctx, "wardyn-tetragon-ingest: docker event stream ended; re-attaching (short-lived containers may correlate as unmapped until it is back)",
			slog.Any("err", err),
		)
		if sleepCtx(ctx, watchRestartDelay) {
			return
		}
	}
}

// Refresh rebuilds the index from the docker daemon. Called on a ticker and on
// throttled misses (the latter with context.Background(), which has no
// deadline of its own) — bound every call here so a wedged docker daemon can
// only stall ingestion for dockerRefreshTimeout, not forever.
func (c *dockerCorrelator) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, dockerRefreshTimeout)
	defer cancel()
	containers, err := c.docker.managedContainers(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, mc := range containers {
		c.addLocked(mc, now)
	}
	// Merge, never replace: entries added by the event watcher (or by an earlier
	// snapshot) belong to containers that may already be gone, and their events
	// are still in flight down the tail. Expire them on age instead.
	for id, e := range c.byContainer {
		if now.Sub(e.seen) > c.retain {
			delete(c.byContainer, id)
		}
	}
	c.lastRefresh = now
	return nil
}

// maybeRefresh refreshes if minRefreshGap has elapsed since the last refresh.
// Returns true if a refresh actually ran.
func (c *dockerCorrelator) maybeRefresh(ctx context.Context) bool {
	c.mu.RLock()
	stale := time.Since(c.lastRefresh) >= c.minRefreshGap
	c.mu.RUnlock()
	if !stale {
		return false
	}
	_ = c.Refresh(ctx)
	return true
}

var _ groundtruth.Correlator = (*dockerCorrelator)(nil)

// ── docker CLI lister ────────────────────────────────────────────────────────

// cliDockerLister lists managed agent containers via `docker ps`. It reads the
// run id from the wardyn.run-id label and the component from wardyn.component,
// keeping only agent containers.
type cliDockerLister struct{}

// dockerPSLine is one row of `docker ps --format '{{json .}}'`. We only need
// the id and the labels string.
type dockerPSLine struct {
	ID     string `json:"ID"`
	Labels string `json:"Labels"`
}

func (cliDockerLister) managedContainers(ctx context.Context) ([]managedContainer, error) {
	// --no-trunc so we get full ids; filter to wardyn-managed at the daemon.
	// -a includes EXITED containers: a run's agent container outlives its
	// process until teardown removes it, and its kernel events are still coming
	// down the tail. The label filter keeps this to wardyn's own containers.
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a", "--no-trunc",
		"--filter", "label=wardyn.managed=true",
		"--format", "{{json .}}")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var managed []managedContainer
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row dockerPSLine
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if mc, ok := managedFromLabels(row.ID, parseLabels(row.Labels)); ok {
			managed = append(managed, mc)
		}
	}
	return managed, nil
}

// managedFromLabels turns a container id + its labels into an index entry.
// Only the agent container qualifies: kernel events for the proxy sidecar are
// not the agent's behaviour and would mis-attribute. Everything else is dropped
// (HOST SENSOR sees ALL containers — we keep only wardyn agent ones).
func managedFromLabels(id string, labels map[string]string) (managedContainer, bool) {
	if id == "" || labels["wardyn.component"] != "agent" {
		return managedContainer{}, false
	}
	runID, err := uuid.Parse(labels["wardyn.run-id"])
	if err != nil {
		return managedContainer{}, false
	}
	return managedContainer{ID: id, RunID: runID}, true
}

// dockerEventLine is one row of `docker events --format '{{json .}}'`. The
// container labels ride on Actor.Attributes, so a create event carries
// everything the index needs — no follow-up inspect (which would race the
// container's removal) is required.
type dockerEventLine struct {
	ID    string `json:"id"`
	Actor struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

func (cliDockerLister) watchContainers(ctx context.Context, add func(managedContainer)) error {
	cmd := exec.CommandContext(ctx, "docker", "events",
		"--filter", "type=container",
		"--filter", "label=wardyn.managed=true",
		"--format", "{{json .}}")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		var row dockerEventLine
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			continue
		}
		id := row.ID
		if id == "" {
			id = row.Actor.ID
		}
		// Every container event (create/start/die/destroy) re-stamps the entry:
		// the mapping is immutable, so re-adding is idempotent and keeps
		// retention counting from the LAST sign of life.
		if mc, ok := managedFromLabels(id, row.Actor.Attributes); ok {
			add(mc)
		}
	}
	return cmd.Wait()
}

// parseLabels parses docker's "k1=v1,k2=v2" Labels string into a map.
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// ── unmapped-event forwarding gate (W24-S1-1) ───────────────────────────────

// mapLiner is the minimal surface tailExport/processLine (main.go) need from
// the Tetragon->AuditEvent mapper. *groundtruth.Mapper satisfies it directly;
// gatedMapper below wraps one to add the unmapped-host-event gate.
type mapLiner interface {
	MapLine(line []byte) (types.AuditEvent, bool)
}

// gatedMapper wraps a mapLiner and enforces the unmapped-host-event opt-in.
//
// FINDING (HIGH, secret-leak, W24-S1-1): Tetragon is a HOST sensor — it
// observes every process on the box, not only Wardyn's. managedContainers
// (cliDockerLister, above) filters the CORRELATION INDEX down to
// wardyn.managed=true agent containers, but that index only decides whether a
// container id RESOLVES; on its own it does nothing to stop a kernel event
// from a container/process that is NOT in the index from being mapped and
// forwarded with run_id NULL + correlation="unmapped". Left ungated, that
// meant every other container's exec argv, sensitive file writes, and
// connect tuples — plus the bare host's own processes — landed in the
// control plane's undeletable audit log and fanned out to every SIEM sink: a
// secret/PII leak of workloads Wardyn does not own, contradicting this
// sidecar's former "drops the rest" doc claim (see main.go's package doc,
// corrected alongside this fix).
//
// A container/process that DOES correlate to a Wardyn run (RunID != nil) is
// NEVER gated — it is always forwarded. Heartbeat and kernel.sensor.blind
// events are also unaffected: they are built directly
// (internal/groundtruth/sensor.go) and emitted straight to the sink, never
// through a mapLiner.
//
// allowUnmapped defaults false (its zero value): secure by default. Set via
// -forward-unmapped-host-events / WARDYN_GROUNDTRUTH_FORWARD_UNMAPPED_HOST_EVENTS
// (see run() in main.go) to opt a deployment back into full-host detection
// coverage, accepting the leak as a deliberate tradeoff.
type gatedMapper struct {
	inner         mapLiner
	allowUnmapped bool
	// dropped counts events this gate refused to forward. Without it a gated
	// event moved NO counter at all (not observed, not dropped): "the sensor saw
	// nothing" and "the sensor saw 4,812 and correlated none" were the same
	// observed_total:0 on /healthz. It rides the heartbeat as dropped_unmapped.
	dropped atomic.Uint64
}

// MapLine delegates to inner, then drops an unmapped result unless opted in —
// exactly like an unrecorded event kind (ok=false: not counted, not
// forwarded) — but counts the drop. See the gatedMapper doc above.
func (g *gatedMapper) MapLine(line []byte) (types.AuditEvent, bool) {
	ev, ok := g.inner.MapLine(line)
	if !ok || g.allowUnmapped || ev.RunID != nil {
		return ev, ok
	}
	g.dropped.Add(1)
	return types.AuditEvent{}, false
}

// droppedUnmappedCount is the cumulative gated-drop count for the heartbeat.
func (g *gatedMapper) droppedUnmappedCount() uint64 { return g.dropped.Load() }
