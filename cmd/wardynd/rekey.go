// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rekeyActor is the audit `actor` for the maintenance-mode rotation: a
// one-shot process, not the serving daemon, and no human principal is
// authenticated (the operator's proof is shell access to the key file).
const rekeyActor = "wardyn/rotate-age-key"

// rekeyConnectTimeout bounds the Postgres connect. The rekey transaction itself
// runs under the caller's context with no deadline: the whole point is that it
// either finishes or commits nothing, and a store large enough to outrun a
// timeout would then be un-rotatable.
const rekeyConnectTimeout = 30 * time.Second

// rotateAgeKeyMode is wardynd's `-rotate-age-key <path>` MAINTENANCE MODE: mint
// a new age identity, re-encrypt every stored secret from the old identity to it
// in one transaction, replace the key file at path, and exit. It never starts a
// server, never opens a listener and never touches the runner — so it also runs
// BEFORE validateConfig, whose TLS/bind posture rules are about serving and
// would otherwise refuse a perfectly good rotation on a compose environment.
//
// The daemon must be OFFLINE. A serving wardynd holds the OLD identity in memory
// for the life of the process: after this returns it would decrypt nothing and
// would write any new secret under the retired key. db.SecretRekeyLockKey
// refuses a second concurrent ROTATION but cannot see a serving daemon — see
// that constant's honest ceiling, and the runbook in docs/OPERATIONS.md.
//
// Deliberately NOT migrated on the way in (unlike the serving boot's
// connectAndMigrate): "rotate my key" must not be a disguised schema upgrade. A
// store predating the secrets table fails loudly at the SELECT instead.
func rotateAgeKeyMode(f *bootFlags, keyPath string) error {
	if strings.TrimSpace(*f.dsn) == "" {
		return fmt.Errorf("refusing to rotate: -rotate-age-key needs the secret store's database; set WARDYN_PG_DSN")
	}
	// An alternate secret-store backend keeps its keys in its own system
	// (OpenBao/Vault/KMS rotate there, not here), so silently rekeying the
	// Postgres `secrets` table would rotate a store nobody reads.
	if sel := strings.TrimSpace(*f.secretStoreSel); sel != "" && sel != "pg" {
		return fmt.Errorf("refusing to rotate: -rotate-age-key rotates the age-encrypted Postgres store, but -secret-store is %q; rotate that backend's keys in its own system", sel)
	}
	oldKey := strings.TrimSpace(*f.ageKey)
	if oldKey == "" {
		return fmt.Errorf("refusing to rotate: WARDYN_AGE_KEY (-age-key) is empty, so this deployment has no durable key to rotate FROM — an unset key means wardynd mints an ephemeral one per boot and no stored ciphertext is readable at all")
	}
	// NOTE: buildSecretStore's knownPublicAgeKeys refusal is deliberately NOT
	// applied to oldKey. Rotating OFF a published key is the single most
	// valuable use of this mode; refusing it would leave the one operator who
	// most needs the rotation unable to run it.
	oldID, err := age.ParseX25519Identity(oldKey)
	if err != nil {
		return fmt.Errorf("parse the current age identity (WARDYN_AGE_KEY): %w", err)
	}

	prev, err := readAgeKeyFile(keyPath)
	if err != nil {
		return err
	}
	// Guard against the obvious footgun: -rotate-age-key pointed at an env file
	// (deploy/compose/.env) or at some other deployment's key. The file this
	// replaces must already hold exactly the identity we are rotating FROM,
	// because after the rename it is the only copy of the new one.
	if prev != "" && prev != oldID.String() {
		return fmt.Errorf("refusing to rotate: %s exists but does not hold the identity WARDYN_AGE_KEY names — this mode REPLACES that file with the new key, so it must be the current key file (a bare AGE-SECRET-KEY-… line, `#` comments allowed), not an env file or another deployment's key", keyPath)
	}

	ctx := context.Background()
	connCtx, cancel := context.WithTimeout(ctx, rekeyConnectTimeout)
	defer cancel()
	pool, err := db.Connect(connCtx, *f.dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	release, ok, err := db.TryAdvisoryLock(ctx, pool, db.SecretRekeyLockKey)
	if err != nil {
		return fmt.Errorf("take the rekey lock: %w", err)
	}
	if !ok {
		return fmt.Errorf("refusing to rotate: another `wardynd -rotate-age-key` holds the rekey advisory lock on this database; wait for it to finish (this lock does NOT prove the daemon is stopped — stop it yourself first)")
	}
	defer release()

	maskReg := secretmask.NewRegistry()
	rec, fan, _, _, err := buildAuditChain(ctx, *f.auditSinks, *f.auditSpool, *f.auditSource, pool, maskReg)
	if err != nil {
		return err
	}
	if fan != nil {
		defer func() { _ = fan.Close() }()
	}

	newID, err := age.GenerateX25519Identity()
	if err != nil {
		return fmt.Errorf("generate the new age identity: %w", err)
	}
	// Staged BEFORE the transaction commits, so the new key is already durable
	// on disk (fsynced) by the time any ciphertext depends on it. If anything
	// after the commit fails, this file is the recovery copy the error names.
	staged := keyPath + ".new"
	if err := writeKeyFile(staged, newID.String()); err != nil {
		return fmt.Errorf("stage the new key file: %w", err)
	}

	n, err := secretstorepg.Rekey(ctx, pool, oldID, newID)
	if err != nil {
		_ = os.Remove(staged) // nothing was committed; the staged key is dead
		return err
	}
	// Emitted here, between the commit and the file swap, because THIS is the
	// point the database's truth changed: a swap failure below must still leave
	// an audit row saying which recipient the store is now encrypted to.
	emitRekeyAudit(ctx, rec, n, newID.Recipient().String())

	if prev != "" {
		// Rollback copy. Kept until the operator deletes it — restoring it is
		// the ONLY undo, and only alongside a restored database dump.
		if err := writeKeyFile(keyPath+".bak", prev); err != nil {
			return fmt.Errorf("the secret store IS rotated (%d rows) but the rollback copy could not be written: %w — the new key is staged at %s; save it before doing anything else", n, err, staged)
		}
	}
	if err := os.Rename(staged, keyPath); err != nil {
		return fmt.Errorf("the secret store IS rotated (%d rows) but %s could not be replaced: %w — the new key is at %s and is now the ONLY key that reads this store; move it into place before restarting wardynd", n, keyPath, err, staged)
	}
	if err := syncDir(keyPath); err != nil {
		return fmt.Errorf("the secret store IS rotated (%d rows) and %s now holds the new key, but the directory could not be fsynced: %w — copy the key out of that file before rebooting the host", n, keyPath, err)
	}

	slog.Info("wardynd: age key rotated; every stored secret is re-encrypted to the new identity. Restart wardynd with the new WARDYN_AGE_KEY.",
		slog.Int("secrets", n),
		slog.String("key_file", keyPath),
		slog.String("public_recipient", newID.Recipient().String()),
		slog.String("rollback_copy", backupNote(prev, keyPath)),
	)
	return nil
}

// emitRekeyAudit writes the secret.rekey event. Data carries the row COUNT and
// the new PUBLIC recipient, never a secret name: the sibling secret.write /
// secret.delete events each name one secret because each IS one secret, whereas
// a single event listing every name in the store would hand the whole inventory
// to every configured sink at once. The recipient is public by construction (it
// is what ciphertext is encrypted TO) and is what lets an operator confirm which
// key the store now answers to.
func emitRekeyAudit(ctx context.Context, rec audit.Recorder, count int, recipient string) {
	data, _ := json.Marshal(map[string]any{
		"secrets":          count,
		"public_recipient": recipient,
	})
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		ActorType: types.ActorSystem,
		Actor:     rekeyActor,
		Action:    "secret.rekey",
		Target:    "pg",
		Outcome:   "success",
		Data:      json.RawMessage(data),
	}
	if err := rec.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}
}

// readAgeKeyFile returns the age identity held in path, or "" when the file does
// not exist (a first rotation may create it). Comment and blank lines are
// skipped so `age-keygen` output — which leads with `# created:` / `# public
// key:` — is accepted verbatim. Anything else is an error rather than a silent
// overwrite, which is what makes the caller's "must match WARDYN_AGE_KEY" guard
// able to refuse an env file instead of destroying it.
func readAgeKeyFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the age key file %s: %w", path, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		id, perr := age.ParseX25519Identity(line)
		if perr != nil {
			return "", fmt.Errorf("%s is not an age key file: its first content line does not parse as an AGE-SECRET-KEY-… identity (%w)", path, perr)
		}
		return id.String(), nil
	}
	return "", nil // present but empty — treat like a missing file
}

// writeKeyFile writes an age identity 0600 via a sibling temp + rename, so a
// reader never sees a half-written key and the mode is never briefly wider.
// Deliberately not gt_rotator.go's writeTokenFileAtomic: that one deletes its
// temp file unconditionally, which is right for a regenerable token and wrong
// for the only copy of a master key.
func writeKeyFile(path, identity string) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(identity + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	// fsync before the rename: a crash must not leave the rename visible while
	// the key bytes are still only in the page cache.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(path)
}

// syncDir fsyncs the DIRECTORY holding path, making a rename durable. Without
// it the rename can still be lost to a power cut after it returned — which for
// this file means the key file reverts to the old identity while the database
// is already re-encrypted to the new one, i.e. every secret unreadable. A
// regenerable file would not be worth the syscall; the secret store's master
// key is.
func syncDir(path string) error {
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// backupNote names the rollback copy for the success log, or says there is none
// (a first rotation, where the key file did not exist before).
func backupNote(prev, keyPath string) string {
	if prev == "" {
		return "(none — the key file did not exist before)"
	}
	return keyPath + ".bak"
}
