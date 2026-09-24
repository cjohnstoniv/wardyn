// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
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

	before, err := buildSecretStore(ctx, pool, id.String(), nil, "", nil, 0, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	orig, err := loadOrCreateSigningKey(ctx, before)
	if err != nil {
		t.Fatal(err)
	}

	split, err := buildSecretStore(ctx, pool, id.String(), platform, "", nil, 0, &capturingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateSigningKey(ctx, split); err == nil || !strings.Contains(err.Error(), "wardynd -rewrap") {
		t.Fatalf("boot with the platform key over a pre-split signing key = %v, want a refusal naming -rewrap", err)
	}

	if n, err := secretstorepg.Rewrap(ctx, pool, id, platform); err != nil || n != 1 {
		t.Fatalf("Rewrap = (%d, %v)", n, err)
	}
	got, err := loadOrCreateSigningKey(ctx, split)
	if err != nil || !got.Equal(orig) {
		t.Fatalf("boot after -rewrap = %v; want the same signing key", err)
	}
}
