// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoSharedRetireMarker is the boot_once row (migration 0103) that makes the
// sweep run once: pending while done_at is null.
const adoSharedRetireMarker = "ado_shared_credential_retire"

// sweepRetiredADOSharedCredentials deletes the stored shared Azure DevOps
// credentials (api.RetiredADOSharedSecretNames: git-pat-<host>, ssh-key-<host>
// and known-hosts-<host> for every Azure DevOps host) from every namespace —
// the operator's and each person's, since a person could store their own copy
// and the git broker reads the owner's row first (#1429). It audits once per
// namespace that held any.
//
// It runs ONCE, at the first start after migration 0103, and never again: the
// marker row is pending until this finishes and is then set, so a token a person
// stores under one of these names afterwards (their own, under a Server row) is
// never swept. A host a GitHub row (or github.com) also names is skipped and
// logged, since the name could be that forge's credential.
//
// The deletion is irreversible: a typed secret is write-only and cannot be
// exported first. A boot sweep rather than a migration because SQL cannot
// delete a store-mode secret's value. A store, marker or site config that cannot
// answer refuses boot rather than leave a retired credential in place.
//
// Under WARDYN_HA several replicas boot at once and each serves once its own
// sweep returns, so a replica that read the marker pending and deleted after
// another had finished would delete a token a person saved in between. The
// marker row stays locked from that read until it is set, so a second boot
// waits and then finds the sweep done. The lock is a transaction on a session
// of its own, not a pooled one: the sweep uses the pool while it is held.
func sweepRetiredADOSharedCredentials(ctx context.Context, pool *pgxpool.Pool, st secretstore.Store, sc types.SiteConfig, rec audit.Recorder) error {
	// A boot that finds the sweep done, as every boot after the first does, takes no lock.
	var pending bool
	err := pool.QueryRow(ctx, `SELECT done_at IS NULL FROM boot_once WHERE name = $1`, adoSharedRetireMarker).Scan(&pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("refusing to start: read the Azure DevOps credential sweep marker: %w", err)
	}
	if !pending {
		return nil
	}
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("refusing to start: connect to lock the Azure DevOps credential sweep marker: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx)) //nolint:errcheck // best-effort; closing the session ends the lock
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("refusing to start: lock the Azure DevOps credential sweep marker: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	// Read again under the lock: another boot may have finished the sweep while this one waited.
	err = tx.QueryRow(ctx, `SELECT done_at IS NULL FROM boot_once WHERE name = $1 FOR UPDATE`, adoSharedRetireMarker).Scan(&pending)
	if err != nil {
		return fmt.Errorf("refusing to start: read the Azure DevOps credential sweep marker: %w", err)
	}
	if !pending {
		return nil
	}
	afterADOSweepPendingRead()
	if err := deleteRetiredADOSharedCredentials(ctx, st, sc, rec); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE boot_once SET done_at = now() WHERE name = $1`, adoSharedRetireMarker); err != nil {
		return fmt.Errorf("refusing to start: mark the Azure DevOps credential sweep done: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("refusing to start: mark the Azure DevOps credential sweep done: %w", err)
	}
	return nil
}

// afterADOSweepPendingRead runs once a boot has read the marker pending, with
// the marker locked; a test holds one boot there.
var afterADOSweepPendingRead = func() {}

func deleteRetiredADOSharedCredentials(ctx context.Context, st secretstore.Store, sc types.SiteConfig, rec audit.Recorder) error {
	names, skipped := api.RetiredADOSharedSecretNames(sc)
	for _, host := range skipped {
		slog.WarnContext(ctx, "Azure DevOps shared credentials for this host were NOT deleted: another forge's provider row names it too, so git-pat-, ssh-key- and known-hosts- names for it may be that forge's own; delete a shared Azure DevOps credential there by hand", "host", host)
	}
	holders, err := st.Holders(ctx, names)
	if err != nil {
		return fmt.Errorf("refusing to start: list retired Azure DevOps shared credentials: %w", err)
	}
	if len(holders) == 0 {
		return nil
	}
	byNamespace := map[string][]string{}
	for name, owners := range holders {
		for _, owner := range owners {
			byNamespace[owner] = append(byNamespace[owner], name)
		}
	}
	if _, err := st.DeleteEverywhere(ctx, names); err != nil {
		return fmt.Errorf("refusing to start: delete retired Azure DevOps shared credentials: %w", err)
	}
	for _, owner := range slices.Sorted(maps.Keys(byNamespace)) {
		namespace := owner
		if owner == "" {
			namespace = "operator"
		}
		slices.Sort(byNamespace[owner])
		data, _ := json.Marshal(map[string]any{
			"namespace": namespace,
			"count":     len(byNamespace[owner]),
			"names":     byNamespace[owner],
		})
		ev := types.AuditEvent{
			ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "wardynd",
			Action: "ado_shared_credential.retire", Target: st.Name(), Outcome: "success", Data: data,
		}
		if err := rec.Record(ctx, ev); err != nil {
			audit.LogWriteFailure(ctx, ev, err)
		}
	}
	return nil
}
