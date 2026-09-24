// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/azurekv"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/vaultkv"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maintenanceMode runs the one-shot maintenance mode the flags select, if any
// (-rotate-age-key, -migrate-secrets, -reconcile, -rewrap), and reports
// whether one ran.
func maintenanceMode(f *bootFlags) (bool, error) {
	if p := strings.TrimSpace(*f.rotateAgeKey); p != "" {
		return true, rotateAgeKeyMode(f, p)
	}
	if *f.migrateSecrets || *f.reconcile || *f.rewrap {
		return true, secretStoreMaintenance(f)
	}
	return false, nil
}

// secretStoreMaintenance runs `wardynd -migrate-secrets -to=<target>`,
// `wardynd -reconcile` or `wardynd -rewrap` (credential-storage design
// §2.3a.9, §2.3a.1, §2.3) and exits. Each opens the store exactly as a serving
// boot does (the same conversion and refusals), serves nothing, and is safe
// beside a serving daemon: a read follows each row wherever it points, and
// the migrator and the rewrap move one locked row at a time.
func secretStoreMaintenance(f *bootFlags) error {
	if strings.TrimSpace(*f.dsn) == "" {
		return fmt.Errorf("refusing to run: -migrate-secrets, -reconcile and -rewrap need the secret store's database; set WARDYN_PG_DSN")
	}
	ctx := context.Background()
	connCtx, cancel := context.WithTimeout(ctx, rekeyConnectTimeout)
	defer cancel()
	pool, err := db.Connect(connCtx, *f.dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	maskReg := secretmask.NewRegistry()
	rec, fan, _, _, err := buildAuditChain(ctx, *f.auditSinks, *f.auditSpool, *f.auditSource, pool, maskReg)
	if err != nil {
		return err
	}
	if fan != nil {
		defer func() { _ = fan.Close() }()
	}
	// The store bare (newSecretStore), not audited: no mode reads through Get.
	// Migrate's reads are recorded one by one (migrateMode), Reconcile reads
	// store metadata only, and Rewrap moves data keys, never opening a value.
	c, err := buildStoreClients(ctx, f)
	if err != nil {
		return err
	}
	s, err := newSecretStore(ctx, pool, *f.ageKey, *f.secretStoreSel, c, rec)
	if err != nil {
		return err
	}
	ps, ok := s.(*secretstorepg.Store)
	if !ok {
		return fmt.Errorf("refusing to run: secret store %q has no rows to migrate, reconcile or rewrap", s.Name())
	}
	if *f.reconcile {
		return reconcileMode(ctx, ps, rec)
	}
	if *f.rewrap {
		return rewrapMode(ctx, ps, rec)
	}
	return migrateMode(ctx, ps, rec, strings.TrimSpace(*f.migrateTo))
}

func migrateMode(ctx context.Context, ps *secretstorepg.Store, rec audit.Recorder, to string) error {
	var from string
	switch to {
	case secretstorepg.MigrateLocal:
		from = ps.ExternalName()
	case vaultkv.Name, azurekv.Name:
		from = "pg"
	default:
		return fmt.Errorf("refusing to migrate: -to must be %q, %q or %q, not %q", vaultkv.Name, azurekv.Name, secretstorepg.MigrateLocal, to)
	}
	// Migrate opens every value it moves without a Get, so its context carries
	// the purpose the read guard requires, and each read is recorded here.
	mctx := secretstore.WithPurpose(ctx, secretstore.PurposeMigrate)
	res, err := ps.Migrate(mctx, to, func(owner, name string) {
		// One secret.read per value read on the way (design §2.3a.9).
		emitMaintenanceAudit(ctx, rec, migrateActor, "secret.read", name, "success", map[string]any{"purpose": string(secretstore.PurposeMigrate), "owner": owner, "to": to})
	})
	data := map[string]any{"from": from, "to": to, "count": res.Moved}
	if res.SoftDeleted > 0 {
		data["soft_deleted"] = res.SoftDeleted
		slog.Warn("wardynd: old copies were deleted but not purged; the organisation can recover them until the vault's retention ends (`wardynd -reconcile` lists them)",
			slog.String("store", from), slog.Int("soft_deleted", res.SoftDeleted))
	}
	if err != nil {
		// Every abort is recorded, with how many rows were committed before it;
		// the error itself (which names the row) goes to the operator only.
		data["reason"] = "aborted"
		emitMaintenanceAudit(ctx, rec, migrateActor, "secret.migrate", to, "failure", data)
		return err
	}
	emitMaintenanceAudit(ctx, rec, migrateActor, "secret.migrate", to, "success", data)
	slog.Info("wardynd: stored secrets migrated", slog.String("to", to), slog.Int("moved", res.Moved), slog.Int("soft_deleted", res.SoftDeleted))
	return nil
}

func reconcileMode(ctx context.Context, ps *secretstorepg.Store, rec audit.Recorder) error {
	rep, err := ps.Reconcile(ctx)
	data := map[string]any{"checked": rep.Checked, "dangling": len(rep.Dangling), "orphans": len(rep.Orphans)}
	outcome := "success"
	switch {
	case err != nil:
		outcome, data["reason"] = "failure", "aborted"
	case len(rep.Dangling)+len(rep.Orphans) > 0:
		outcome, data["reason"] = "failure", "drift"
	}
	emitMaintenanceAudit(ctx, rec, "wardyn/reconcile", "secret.reconcile", vaultkv.Name, outcome, data)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "checked %d pointer rows\n", rep.Checked)
	for _, d := range rep.Dangling {
		fmt.Fprintf(os.Stdout, "pointer without a value: %s\n", d)
	}
	for _, o := range rep.Orphans {
		fmt.Fprintf(os.Stdout, "value without a pointer: %s (owner %q, name %q)\n", o.Ref, o.Owner, o.Name)
	}
	for _, d := range rep.SoftDeleted {
		left := "until the vault purges it"
		if d.RecoverableDays > 0 {
			left = fmt.Sprintf("for %d more days", d.RecoverableDays)
		}
		fmt.Fprintf(os.Stdout, "soft-deleted, recoverable by your organisation %s: %s (owner %q, name %q)\n", left, d.Ref, d.Owner, d.Name)
	}
	if len(rep.Dangling)+len(rep.Orphans) > 0 {
		return fmt.Errorf("reconcile: %d pointers without a value, %d values without a pointer; nothing was changed", len(rep.Dangling), len(rep.Orphans))
	}
	return nil
}

// rewrapMode runs `wardynd -rewrap`: every sealed row's data key moves to the
// key WARDYN_KEK selects, at its latest version. One secret.rewrap row records
// the key, the version and the count — never a name or a value. An abort is
// recorded as a failure with the count committed before it, as migrateMode's is.
func rewrapMode(ctx context.Context, ps *secretstorepg.Store, rec audit.Recorder) error {
	res, err := ps.Rewrap(ctx)
	data := map[string]any{"kek_id": res.KEK, "count": res.Rewrapped}
	if res.KeyVersion > 0 {
		data["key_version"] = res.KeyVersion
	}
	if err != nil {
		data["reason"] = "aborted"
		emitMaintenanceAudit(ctx, rec, rewrapActor, "secret.rewrap", res.KEK, "failure", data)
		return err
	}
	emitMaintenanceAudit(ctx, rec, rewrapActor, "secret.rewrap", res.KEK, "success", data)
	slog.Info("wardynd: every sealed secret's data key is wrapped under the configured key", slog.String("kek_id", res.KEK), slog.Int("rewrapped", res.Rewrapped))
	if res.KeyVersion > 0 {
		fmt.Fprintf(os.Stdout, "every sealed secret is wrapped under %s version %d; raising the Transit key's min_decryption_version to %d now retires the older versions\n", res.KEK, res.KeyVersion, res.KeyVersion)
	}
	return nil
}

// The audit actors of the maintenance modes: one-shot processes, not the
// serving daemon, with no authenticated human principal.
const (
	migrateActor = "wardyn/migrate-secrets"
	rewrapActor  = "wardyn/rewrap"
)

// emitMaintenanceAudit writes one audit row from a maintenance mode. Data
// carries names, owners and counts, never a value.
func emitMaintenanceAudit(ctx context.Context, rec audit.Recorder, actor, action, target, outcome string, data map[string]any) {
	raw, _ := json.Marshal(data)
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		ActorType: types.ActorSystem,
		Actor:     actor,
		Action:    action,
		Target:    target,
		Outcome:   outcome,
		Data:      json.RawMessage(raw),
	}
	if err := rec.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}
}
