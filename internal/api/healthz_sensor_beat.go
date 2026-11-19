// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sensorBeatCacheFor is how long one read of the sensor's latest beat answers
// every caller on this replica. /healthz is anonymous and every open console
// polls it, and the read can run to its bound passing over forwarded beats; a
// read cut off at that bound also drops its pooled connection. A failed read is
// kept as long, so a failing scan runs at most once per period.
const sensorBeatCacheFor = 30 * time.Second

// errSensorBeatReadBusy answers a caller that arrives while the read is
// running and no earlier answer is kept: the probes that call /healthz must not
// wait on another caller's read.
var errSensorBeatReadBusy = errors.New("api: sensor heartbeat read already running")

// groundtruthNoKernelEvents is the idle reason for a fresh beat that has seen no kernel events at all.
const groundtruthNoKernelEvents = "No kernel events observed"

// groundtruthNoneCorrelated is the idle reason for a fresh beat that has seen kernel events and tied none
// of them to a run.
const groundtruthNoneCorrelated = "Kernel events observed but none correlated to a run"

// groundtruthNoRecentEvents is the idle reason for a sensor whose node row
// shows no rise in its counts within store.GroundtruthEvidenceWindow.
const groundtruthNoRecentEvents = "No new kernel events in the last 15 minutes"

// groundtruthNotYetKnown is the idle reason for a fresh beat with counts but no
// rise in them on record: a first beat, a rise across a gap longer than the
// window, or no node row at all. Lifetime counts prove nothing about now.
const groundtruthNotYetKnown = "Whether new kernel events still arrive is not yet known"

// sensorCause is why one sensor reads as it does. A capture note is picked from
// the causes, never from the reason text, which with several node sensors only
// counts the nodes.
type sensorCause uint8

const (
	sensorFine        sensorCause = iota // healthy or partial
	sensorNoBeat                         // unavailable: no heartbeat on record
	sensorUnreadable                     // unavailable: the heartbeat read failed or is still running
	sensorStale                          // degraded
	sensorNoEvents                       // idle: zero mapped kernel events
	sensorNotYetKnown                    // idle: no recent rise in the counts is on record
	sensorQuiet                          // idle: no new kernel events within the window
)

// sensorCauses counts the live sensors by cause; the single sensor is one entry.
type sensorCauses map[sensorCause]int

// sensorBeatState is this replica's cache of the sensor beat read. Zero value
// is ready to use.
type sensorBeatState struct {
	mu            sync.Mutex
	reading, have bool
	at            time.Time // when the kept answer's read started
	ev            types.AuditEvent
	err           error
}

// latestSensorBeat is the sensor's own latest beat, read at most once per
// sensorBeatCacheFor and never waited on: a caller that arrives during the read
// gets the kept answer, or errSensorBeatReadBusy when there is none.
func (s *Server) latestSensorBeat(ctx context.Context) (types.AuditEvent, error) {
	c := &s.sensorBeat
	now := s.cfg.Now()
	c.mu.Lock()
	if c.have && (c.reading || now.Sub(c.at) < sensorBeatCacheFor) {
		ev, err := c.ev, c.err
		c.mu.Unlock()
		return ev, err
	}
	if c.reading {
		c.mu.Unlock()
		return types.AuditEvent{}, errSensorBeatReadBusy
	}
	c.reading = true
	c.mu.Unlock()
	// The answer is shared, so one caller going away must not end the read:
	// only the bound does.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sensorBeatReadTimeout)
	defer cancel()
	// Kept even when the read panics, as a failed read, so reading never stays set.
	ev, err := types.AuditEvent{}, error(errSensorBeatReadBusy)
	defer func() {
		c.mu.Lock()
		c.reading, c.have, c.at, c.ev, c.err = false, true, now, ev, err
		c.mu.Unlock()
	}()
	ev, err = s.cfg.Store.LatestOrganisationAuditEvent(rctx, groundtruth.ActionSensorHeartbeat, types.ActorSystem, groundtruth.SensorActor)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "api: read sensor heartbeat", slog.Any("err", err))
	}
	return ev, err
}
