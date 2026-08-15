// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
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
// It implements groundtruth.Correlator. The index is refreshed periodically AND
// can be force-refreshed on a container-id miss (a freshly-started run's
// container may not be in the last snapshot yet).
// dockerRefreshTimeout bounds a single `docker ps` query so a wedged docker
// daemon wedges only this call, not the caller (RunForContainer runs inline
// on the tail loop via maybeRefresh).
const dockerRefreshTimeout = 10 * time.Second

type dockerCorrelator struct {
	mu          sync.RWMutex
	byContainer map[string]uuid.UUID // full + truncated (12-char) ids -> run
	lastRefresh time.Time
	// minRefreshGap throttles on-miss refreshes so a flood of unmapped events
	// cannot spawn a `docker ps` storm.
	minRefreshGap time.Duration

	docker dockerLister // injectable for tests
}

// dockerLister abstracts the docker query so tests can avoid a real daemon.
type dockerLister interface {
	// managedContainers returns the wardyn.managed=true agent containers as a
	// slice of (id, runID) pairs. Proxy containers are excluded (only the agent
	// container's run-id is what kernel events should correlate to).
	managedContainers(ctx context.Context) ([]managedContainer, error)
}

type managedContainer struct {
	ID    string // full container id
	RunID uuid.UUID
}

func newDockerCorrelator(lister dockerLister, minRefreshGap time.Duration) *dockerCorrelator {
	if lister == nil {
		lister = cliDockerLister{}
	}
	return &dockerCorrelator{
		byContainer:   map[string]uuid.UUID{},
		minRefreshGap: minRefreshGap,
		docker:        lister,
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
	if r, ok := c.byContainer[id]; ok {
		return r, true
	}
	// Tetragon often emits a 12-char short id; index both forms, but also try a
	// prefix match against full ids as a last resort.
	for full, r := range c.byContainer {
		if strings.HasPrefix(full, id) || strings.HasPrefix(id, full) {
			return r, true
		}
	}
	return uuid.Nil, false
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
	idx := make(map[string]uuid.UUID, len(containers)*2)
	for _, mc := range containers {
		idx[mc.ID] = mc.RunID
		if len(mc.ID) >= 12 {
			idx[mc.ID[:12]] = mc.RunID // short-id form Tetragon commonly emits
		}
	}
	c.mu.Lock()
	c.byContainer = idx
	c.lastRefresh = time.Now()
	c.mu.Unlock()
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
	cmd := exec.CommandContext(ctx, "docker", "ps", "--no-trunc",
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
		labels := parseLabels(row.Labels)
		// Only the agent container: kernel events for the proxy sidecar are not
		// the agent's behaviour and would mis-attribute. Drop everything else
		// (HOST SENSOR sees ALL containers — we keep only wardyn agent ones).
		if labels["wardyn.component"] != "agent" {
			continue
		}
		runStr := labels["wardyn.run-id"]
		runID, perr := uuid.Parse(runStr)
		if perr != nil {
			continue
		}
		managed = append(managed, managedContainer{ID: row.ID, RunID: runID})
	}
	return managed, nil
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
}

// MapLine delegates to inner, then drops an unmapped result unless opted in —
// exactly like an unrecorded event kind (ok=false: not counted, not
// forwarded). See the gatedMapper doc above for why this exists.
func (g *gatedMapper) MapLine(line []byte) (types.AuditEvent, bool) {
	ev, ok := g.inner.MapLine(line)
	if !ok || g.allowUnmapped || ev.RunID != nil {
		return ev, ok
	}
	return types.AuditEvent{}, false
}
