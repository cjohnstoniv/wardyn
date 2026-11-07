// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"syscall"
	"time"
)

// reapSignal and reapGrace are the reap's signal and its wait between SIGTERM and SIGKILL (tests).
var (
	reapSignal = syscall.Kill
	reapGrace  = 10 * time.Second
)

// driftNow is checkAgent's clock (tests).
var driftNow = time.Now

// reapOrphanHelpers ends the helpers an earlier runnerd started that never claimed their entry.
func (m *macOSVMs) reapOrphanHelpers(ctx context.Context) {}
