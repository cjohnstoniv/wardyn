// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"

	"filippo.io/age"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
)

// convertSecretStore readies the pg store's rows before anything reads them —
// before loadOrCreateSecret above all, whose boot keys share the table. It
// converts every legacy (v0) row to envelope v1, aborting boot on one that will
// not decrypt; and it refuses an ephemeral age key while rows sealed under an
// age key exist, rather than let a fresh key strand them. An alternate backend
// keeps its own format and is left alone.
func convertSecretStore(ctx context.Context, s secretstore.Store, id *age.X25519Identity, ephemeral bool) error {
	ps, ok := s.(*secretstorepg.Store)
	if !ok {
		return nil
	}
	if ephemeral {
		n, err := ps.LocalRows(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("refusing to start: WARDYN_AGE_KEY is unset, but %d stored secrets are sealed under an age key — an ephemeral key would make every one unreadable; set WARDYN_AGE_KEY to the key they were written with", n)
		}
		return nil
	}
	n, err := ps.ConvertV0(ctx, id)
	if err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}
	if n > 0 {
		slog.Info("wardynd: converted stored secrets to envelope v1; an older wardynd can no longer read them", slog.Int("secrets", n))
	}
	return nil
}
