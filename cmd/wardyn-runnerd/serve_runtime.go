// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"

	"golang.org/x/sync/errgroup"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func runRunnerServices(ctx context.Context, wire, containment func(context.Context) error) error {
	group, life := errgroup.WithContext(ctx)
	group.Go(func() error {
		err := wire(life)
		if errors.Is(err, runnerwire.ErrRevoked) {
			// A durable revocation latch can still have transient Docker
			// containment failures. Keep enforcing locally without redialing.
			slog.Warn("runner identity revoked; local containment remains active")
			<-life.Done()
			return errors.Join(err, life.Err())
		}
		if err == nil {
			return errors.New("runner connection loop stopped unexpectedly")
		}
		return err
	})
	group.Go(func() error {
		if err := containment(life); err != nil {
			return err
		}
		return errors.New("runner containment monitor stopped unexpectedly")
	})
	return group.Wait()
}
