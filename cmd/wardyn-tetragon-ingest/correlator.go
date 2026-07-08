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
// throttled misses.
func (c *dockerCorrelator) Refresh(ctx context.Context) error {
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
