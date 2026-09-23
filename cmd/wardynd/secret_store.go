// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
)

// buildSecretStore constructs the secret store, readies its rows
// (convertSecretStore) and returns it wrapped in secretstore.Audited, so every
// read through it is recorded on rec. The age identity comes from -age-key; if
// empty one is generated and logged (operators MUST persist it across restarts
// to keep prior ciphertext readable).
func buildSecretStore(ctx context.Context, pool *pgxpool.Pool, ageKey, storeName string, rec audit.Recorder) (secretstore.Store, error) {
	var id *age.X25519Identity
	var err error
	if ageKey == "" {
		id, err = age.GenerateX25519Identity()
		if err != nil {
			return nil, fmt.Errorf("generate age identity: %w", err)
		}
		// F10: log the PUBLIC recipient as a fingerprint, never the secret identity.
		// The old message printed the full AGE-SECRET-KEY- to a log file created at
		// the default umask (~/.wardyn/host-wardynd.log), leaking the secret-store
		// master key. To persist, mint one with `wardynd -gen-age-key` (prints to
		// stdout by design) and set WARDYN_AGE_KEY — do not copy it out of this log.
		slog.Warn("wardynd: generated ephemeral age identity; secrets are LOST on restart. Persist one with `wardynd -gen-age-key` + set WARDYN_AGE_KEY",
			slog.String("public_recipient", id.Recipient().String()),
		)
	} else {
		if isKnownPublicAgeKey(ageKey) {
			return nil, fmt.Errorf("refusing to start: WARDYN_AGE_KEY is a publicly-known key (published in this repo's git history) — secrets encrypted under it are not protected; unset WARDYN_AGE_KEY to generate an ephemeral key, or mint your own with `wardynd -gen-age-key`")
		}
		id, err = age.ParseX25519Identity(ageKey)
		if err != nil {
			return nil, fmt.Errorf("parse age identity: %w", err)
		}
	}
	s, err := secretstore.New(storeName, secretstore.Deps{Pool: pool, AgeIdentity: id})
	if err != nil {
		return nil, fmt.Errorf("secret store: %w", err)
	}
	if err := convertSecretStore(ctx, s, id, ageKey == "", rec); err != nil {
		return nil, err
	}
	return secretstore.Audited(s, rec), nil
}

// convertSecretStore readies the pg store's rows before anything reads them —
// before loadOrCreateSecret above all, whose boot keys share the table. It
// converts every legacy (v0) row to envelope v1, aborting boot on one that will
// not decrypt; and it refuses an ephemeral age key while rows sealed under an
// age key exist, rather than let a fresh key strand them. An alternate backend
// keeps its own format and is left alone. Each converted row was a read of its
// value, recorded as a secret.read with purpose migrate.
func convertSecretStore(ctx context.Context, s secretstore.Store, id *age.X25519Identity, ephemeral bool, rec audit.Recorder) error {
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
	converted, err := ps.ConvertV0(ctx, id)
	if err != nil {
		return fmt.Errorf("refusing to start: %w", err)
	}
	for _, row := range converted {
		secretstore.RecordRead(ctx, rec, secretstore.PurposeMigrate, row.Owner, row, nil)
	}
	if len(converted) > 0 {
		slog.Info("wardynd: converted stored secrets to envelope v1; an older wardynd can no longer read them", slog.Int("secrets", len(converted)))
	}
	return nil
}
