// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"strings"
	"testing"

	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
)

// TestPG_BootWithAPlatformKeyNeedsRewrapOnce is the upgrade path of design
// §2.13 c through the real boot: a signing key minted under the age key alone
// stops a boot that sets WARDYN_PLATFORM_KEY_FILE (it is refused, never read as
// missing and minted over), `-rewrap`'s body moves it, and the next boot reads
// the SAME key under the platform key.
func TestPG_BootWithAPlatformKeyNeedsRewrapOnce(t *testing.T) {
	pool := envelopeDB(t)
	ctx := t.Context()
	id, platform := mustAgeIdentity(t), mustAgeIdentity(t)

	before, err := buildSecretStore(ctx, pool, id.String(), nil, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	orig, err := loadOrCreateSigningKey(ctx, unlocked(before))
	if err != nil {
		t.Fatal(err)
	}

	split, err := buildSecretStore(ctx, pool, id.String(), platform, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateSigningKey(ctx, unlocked(split)); err == nil || !strings.Contains(err.Error(), "wardynd -rewrap") {
		t.Fatalf("boot with the platform key over a pre-split signing key = %v, want a refusal naming -rewrap", err)
	}

	if n, err := secretstorepg.Rewrap(ctx, pool, id, platform, false); err == nil || n != 0 || !errors.Is(err, secretstorepg.ErrAdoptNotRequested) {
		t.Fatalf("Rewrap without -rewrap-adopt-boot-keys = (%d, %v), want it refused", n, err)
	}
	if n, err := secretstorepg.Rewrap(ctx, pool, id, platform, true); err != nil || n != 1 {
		t.Fatalf("Rewrap = (%d, %v)", n, err)
	}
	got, err := loadOrCreateSigningKey(ctx, unlocked(split))
	if err != nil || !got.Equal(orig) {
		t.Fatalf("boot after -rewrap = %v; want the same signing key", err)
	}
}

// A pre-envelope signing key written beside the platform key file, after the
// real one was deleted, is not converted under the platform key by the next
// boot: the boot refuses, names the row, and the table is unchanged. (With no
// platform key set the same row converts: TestPG_BootConvertsV0BeforeBootKeysAreRead.)
func TestPG_BootRefusesAForgedV0BootKeyBesideAPlatformKey(t *testing.T) {
	pool := envelopeDB(t)
	ctx := t.Context()
	id, platform := mustAgeIdentity(t), mustAgeIdentity(t)
	split, err := buildSecretStore(ctx, pool, id.String(), platform, "", storeClients{}, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateSigningKey(ctx, unlocked(split)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM secrets WHERE owned_by='' AND name='wardyn-signing-key'`); err != nil {
		t.Fatal(err)
	}
	seedV0Row(t, pool, id, "wardyn-signing-key", []byte("forged"))
	before := rewrapRows(t, pool)

	_, err = buildSecretStore(ctx, pool, id.String(), platform, "", storeClients{}, &capturingRecorder{})
	if !errors.Is(err, secretstorepg.ErrV0BootKey) || !strings.Contains(err.Error(), "wardyn-signing-key") {
		t.Fatalf("boot over a forged v0 boot key beside a platform key = %v, want a refusal naming the row", err)
	}
	if rewrapRows(t, pool) != before {
		t.Fatal("a refused boot changed rows")
	}
}
