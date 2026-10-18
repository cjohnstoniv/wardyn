// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"log/slog"

	"github.com/google/uuid"
)

// onSinkLapsed turns a validated lapsed frame into the run's evidence that its proxy's egress
// stopped and its ProxyExited obligation. The sink refuses a second proxy instance for a run at
// hello (K9 design question 5), so a lapsed frame always speaks for the run's one proxy.
func onSinkLapsed(noteLapse func(uuid.UUID) error, exited func(uuid.UUID, string) error) func(uuid.UUID, string) {
	return func(id uuid.UUID, reason string) {
		if err := errors.Join(noteLapse(id), exited(id, "lapsed:"+reason)); err != nil {
			slog.Error("runnerd: recording a proxy's lapse", slog.String("run_id", id.String()), slog.Any("err", err))
		}
	}
}
