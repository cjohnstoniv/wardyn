// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// inflightCreates holds the cancel func of every run's in-flight
// CreateSandbox, so a kill of a STARTING run stops the create instead of
// leaving it to hold cluster room until its own readiness wait expires.
// Dispatch runs detached (context.WithoutCancel), so nothing else can reach it.
// Process-local like sshSessions: replicas>1 is refused by construction, so the
// kill always lands in the process that is dispatching. Zero value is ready.
type inflightCreates struct{ m sync.Map } // uuid.UUID -> context.CancelFunc

// track registers the run BEFORE dispatch's PENDING->STARTING claim, so a kill
// that wins the run at any point after the claim always finds it. The returned
// context is for CreateSandbox alone: the rollback and compensation after it
// must keep running on the uncancelled dispatch context.
func (c *inflightCreates) track(ctx context.Context, runID uuid.UUID) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	c.m.Store(runID, cancel)
	return ctx, func() {
		c.m.Delete(runID)
		cancel()
	}
}

// cancel stops the run's in-flight create, if it has one. Idempotent.
func (c *inflightCreates) cancel(runID uuid.UUID) {
	if f, ok := c.m.Load(runID); ok {
		f.(context.CancelFunc)()
	}
}
