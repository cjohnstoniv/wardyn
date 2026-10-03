// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// audit_seal.go is WARDYN_AUDIT_SEAL: the recorder that seals a row's personal
// fields under the person's own key before the spool, the store or a sink sees
// them (internal/audit's Sealer does the sealing), and the same recorder in
// replay mode on the spool drain, which re-seals the rows that waited under the
// platform pending key. The chain is built before the secret store, which holds
// the subject keys, so the keys are armed late, as maskScope's manifests are.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/maskstore"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// secretAuditPendingKey seals the personal fields of a row whose subject key
// could not be had, so the row waits in the spool instead of being dropped or
// written in the clear (32 bytes).
const secretAuditPendingKey = "wardyn-audit-pending-key"

// auditSealMode is WARDYN_AUDIT_SEAL as this release serves it: full (the actor
// as well) is ar-l1.5's, so it refuses to boot rather than quietly seal less
// than was asked.
func auditSealMode(f *bootFlags) (audit.SealMode, error) {
	mode, err := audit.ParseSealMode(*f.auditSeal)
	if err != nil {
		return "", fmt.Errorf("refusing to start: %w", err)
	}
	if mode == audit.SealFull {
		return "", errors.New("refusing to start: WARDYN_AUDIT_SEAL is full, which also seals the actor and is not available in this release; use fields")
	}
	return mode, nil
}

// sealModeOf is the mode validateBootPosture already accepted.
func sealModeOf(f *bootFlags) audit.SealMode {
	mode, _ := audit.ParseSealMode(*f.auditSeal)
	return mode
}

// armSubjectKeyed wires what sits under the secret store's per-subject keys,
// built after the store: the run masking manifests, and the keys of the sealed
// audit fields.
func armSubjectKeyed(ctx context.Context, pool *pgxpool.Pool, secrets secretstore.Store, reg *secretmask.Registry, scope *maskScope, src *auditSealSource, bootKeys bootKeyStore) (*maskmanifest.Manifests, *maskstore.Store, error) {
	m, store, err := buildMaskManifests(ctx, pool, secrets, reg, scope)
	if err != nil {
		return nil, nil, err
	}
	return m, store, armAuditSeal(ctx, src, pool, secrets, bootKeys)
}

// auditSealSource is the sealing state the recorder chain and the spool drain
// share. The Sealer is armed once the secret store exists; until then a row
// that needs sealing is refused rather than written in the clear.
type auditSealSource struct {
	mode   audit.SealMode
	sealer atomic.Pointer[audit.Sealer]
}

func newAuditSealSource(mode audit.SealMode) *auditSealSource { return &auditSealSource{mode: mode} }

func (a *auditSealSource) arm(s *audit.Sealer) {
	if a != nil {
		a.sealer.Store(s)
	}
}

// unsealer is what reads use: the armed Sealer, or nil.
func (a *auditSealSource) unsealer() audit.Unsealer {
	if a == nil {
		return nil
	}
	if s := a.sealer.Load(); s != nil {
		return s
	}
	return nil
}

// sealingRecorder seals a row's personal fields, between maskingRecorder and
// spoolingRecorder, so the row hash, the spool, the store and every sink all
// carry ciphertext. In replay mode (the spool drain's recorder, over the raw
// store recorder) it only re-seals the rows that waited under the pending key.
type sealingRecorder struct {
	inner  audit.Recorder
	src    *auditSealSource
	spool  *api.AuditSpool // where a pending row waits; live mode only
	replay bool
}

var _ audit.Recorder = sealingRecorder{}

func (r sealingRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if r.replay {
		return r.record(ctx, ev)
	}
	if r.src == nil || r.src.mode != audit.SealFields || !audit.SealsAction(ev.Action) {
		return r.inner.Record(ctx, ev)
	}
	sealer := r.src.sealer.Load()
	if sealer == nil {
		return fmt.Errorf("audit seal: %s not recorded: the sealing keys are not available yet and the row has personal fields; it is never written in the clear", ev.Action)
	}
	sealed, pending, err := sealer.Seal(ctx, ev)
	if err != nil {
		return fmt.Errorf("audit seal: %s not recorded: %w", ev.Action, err)
	}
	if !pending {
		return r.inner.Record(ctx, sealed)
	}
	// A pending row goes to the spool and nowhere else: the store holds fields
	// sealed under their subjects' keys only. The drain re-seals it.
	if r.spool == nil {
		return fmt.Errorf("audit seal: %s not recorded: a subject key could not be had and there is no spool to hold the row", ev.Action)
	}
	slog.WarnContext(ctx, "wardynd: a subject's audit key could not be had; the row is held in the audit spool sealed under the pending key and is re-sealed by the drain",
		slog.String("action", ev.Action))
	return r.spool.Append(sealed)
}

func (r sealingRecorder) record(ctx context.Context, ev types.AuditEvent) error {
	if audit.IsPending(ev) {
		var sealer *audit.Sealer
		if r.src != nil {
			sealer = r.src.sealer.Load()
		}
		if sealer == nil {
			return fmt.Errorf("%w: the sealing keys are not available", audit.ErrReplayDeferred)
		}
		var err error
		if ev, err = sealer.Reseal(ctx, ev); err != nil {
			return fmt.Errorf("%w: %w", audit.ErrReplayDeferred, err)
		}
	}
	return r.inner.Record(ctx, ev)
}

// subjectSealKeys adapts the secret store's subject keys to audit.SealKeys: a
// destroyed generation is audit.ErrKeyErased.
type subjectSealKeys struct{ m *subjectkey.Manager }

func (k subjectSealKeys) Current(ctx context.Context, owner, purpose string) (int, []byte, error) {
	return k.m.Current(ctx, owner, purpose)
}

func (k subjectSealKeys) Key(ctx context.Context, owner, purpose string, version int) ([]byte, error) {
	key, err := k.m.Key(ctx, owner, purpose, version)
	if errors.Is(err, subjectkey.ErrDataLoss) {
		return nil, fmt.Errorf("%w: %w", audit.ErrKeyErased, err)
	}
	return key, err
}

// armAuditSeal gives src its keys: the secret store's subject keys, the person
// directory that resolves aliases, and the pending key. The pending key is
// created when sealing is on; with it off, one that exists is still loaded, so
// rows already waiting in the spool can be re-sealed.
func armAuditSeal(ctx context.Context, src *auditSealSource, pool *pgxpool.Pool, secrets secretstore.Store, bootKeys bootKeyStore) error {
	keys := subjectKeysOf(secrets)
	if keys == nil {
		if src.mode == audit.SealOff {
			return nil
		}
		return errors.New("refusing to start: WARDYN_AUDIT_SEAL is on and the secret store has no per-subject keys to seal under")
	}
	pending, err := loadAuditPendingKey(ctx, bootKeys, src.mode != audit.SealOff)
	if err != nil {
		return err
	}
	src.arm(newAuditSealer(subjectSealKeys{keys}, pool, pending))
	return nil
}

// newAuditSealer is the Sealer over keys, with aliases resolved through the
// person directory and the pending key (nil: none) held in memory.
func newAuditSealer(keys audit.SealKeys, pool *pgxpool.Pool, pending []byte) *audit.Sealer {
	st := store.NewPG(pool)
	return &audit.Sealer{
		Keys:    keys,
		Pending: func() []byte { return pending },
		Resolve: st.PrincipalForName,
		GoneSince: func(ctx context.Context, subject string, since time.Time) (bool, error) {
			return st.SubjectKeyDestroyedSince(ctx, subject, audit.SealPurpose, since)
		},
	}
}

// loadAuditPendingKey returns the pending key; nil when create is false and
// none exists.
func loadAuditPendingKey(ctx context.Context, secrets bootKeyStore, create bool) ([]byte, error) {
	valid := func(b []byte) bool { return len(b) == 32 }
	if !create {
		raw, ok, err := loadBootKey(ctx, secrets, secretAuditPendingKey, valid)
		if err != nil || !ok {
			return nil, err
		}
		return raw, nil
	}
	return loadOrCreateSecret(ctx, secrets, secretAuditPendingKey, valid,
		func() ([]byte, error) {
			key := make([]byte, 32)
			if _, gerr := rand.Read(key); gerr != nil {
				return nil, fmt.Errorf("generate audit pending key: %w", gerr)
			}
			slog.Info("wardynd: generated and persisted the audit pending key")
			return key, nil
		})
}
