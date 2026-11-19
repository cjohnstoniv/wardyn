// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auditsign"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A decrypt failure (any Get error but not-found) never mints a key over the stored one, and a stored row
// that does not parse is never replaced either: both leave signing off with the row untouched.
func TestAuditReceiptKey_DecryptErrorNeverMints(t *testing.T) {
	ctx := context.Background()
	s := &testGatewayKeyStore{getErr: errors.New("age: no identity matched")}
	if k, err := loadOrCreateAuditReceiptKey(ctx, s.boot()); err == nil || k != nil {
		t.Fatalf("decrypt error: key %v, err %v; want a refusal", k, err)
	}
	if s.puts != 0 || s.createLocked != 0 {
		t.Fatalf("decrypt error wrote %d times (create lock taken %d times)", s.puts, s.createLocked)
	}

	s = &testGatewayKeyStore{row: []byte("not the key json")}
	if k, err := loadOrCreateAuditReceiptKey(ctx, s.boot()); err == nil || k != nil {
		t.Fatalf("corrupt row: key %v, err %v; want a refusal", k, err)
	}
	if s.puts != 0 || string(s.row) != "not the key json" {
		t.Fatalf("a corrupt row was written over (%d puts)", s.puts)
	}

	s = &testGatewayKeyStore{}
	first, err := loadOrCreateAuditReceiptKey(ctx, s.boot())
	if err != nil || first == nil || s.puts != 1 {
		t.Fatalf("first boot: key %v, err %v, puts %d", first, err, s.puts)
	}
	again, err := loadOrCreateAuditReceiptKey(ctx, s.boot())
	if err != nil || again.KeyID() != first.KeyID() || s.puts != 1 {
		t.Fatalf("second boot made a new key: %v, %v, puts %d", again, err, s.puts)
	}
	for _, name := range s.touched {
		if name != secretAuditReceiptKey {
			t.Errorf("touched %q, want only %q", name, secretAuditReceiptKey)
		}
	}
}

// Upgrading never makes wardynd refuse to start: a key that cannot be had leaves signing off (no keys for the
// receipt routes) and counts key_unavailable, and the call returns rather than failing the boot.
func TestCheckpointLoop_KeyUnavailableNeverBlocksBoot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &testGatewayKeyStore{getErr: errors.New("vault: permission denied")}
	keys, status := startAuditCheckpoints(ctx, time.Minute, s.boot(), nil, &capturingRecorder{})
	if keys != nil {
		t.Fatal("receipt keys published with no key")
	}
	if status == nil || status.Snapshot().Failures[auditsign.ReasonKeyUnavailable] != 1 {
		t.Fatalf("status = %+v, want key_unavailable 1", status)
	}

	s = &testGatewayKeyStore{}
	keys, status = startAuditCheckpoints(ctx, 0, s.boot(), nil, &capturingRecorder{})
	if keys != nil || status != nil || s.gets != 0 {
		t.Fatalf("interval 0 read the key (%d gets) or reported a signer", s.gets)
	}
}

func TestRotateAuditReceiptKey_KeepsPreviousPublic(t *testing.T) {
	ctx := context.Background()
	s := &testGatewayKeyStore{}
	rec := &capturingRecorder{}
	if err := s.boot().rotateAuditReceipt(ctx, rec, false, time.Now()); err == nil || s.puts != 0 {
		t.Fatalf("rotated an install with no key (err %v, puts %d)", err, s.puts)
	}
	first, err := loadOrCreateAuditReceiptKey(ctx, s.boot())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.boot().rotateAuditReceipt(ctx, rec, false, time.Now()); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	next, err := loadOrCreateAuditReceiptKey(ctx, s.boot())
	if err != nil || next.KeyID() == first.KeyID() {
		t.Fatalf("after rotation: %v, %v", next, err)
	}
	keys := next.PublicKeys()
	if len(keys) != 2 || keys[1].Kid != first.KeyID() || keys[1].Retired {
		t.Fatalf("public keys after rotation = %+v", keys)
	}
	if len(rec.got) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rec.got))
	}
	ev := rec.got[0]
	if ev.Action != "secret.rekey" || ev.Actor != "wardyn/rotate-audit-receipt-key" || ev.Target != secretAuditReceiptKey || ev.ActorType != types.ActorSystem {
		t.Fatalf("rekey row = %+v", ev)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["kid"] != next.KeyID() || data["replaced_kid"] != first.KeyID() || data["retired_previous"] != false {
		t.Errorf("rekey data = %v", data)
	}
	if strings.Contains(string(ev.Data), "seed") {
		t.Errorf("rekey row carries key material: %s", ev.Data)
	}
}

func TestRotateAuditReceiptKey_RetirePreviousMarksRetired(t *testing.T) {
	ctx := context.Background()
	s := &testGatewayKeyStore{}
	rec := &capturingRecorder{}
	first, err := loadOrCreateAuditReceiptKey(ctx, s.boot())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.boot().rotateAuditReceipt(ctx, rec, true, time.Now()); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	next, err := loadOrCreateAuditReceiptKey(ctx, s.boot())
	if err != nil {
		t.Fatal(err)
	}
	keys := next.PublicKeys()
	if len(keys) != 2 || keys[1].Kid != first.KeyID() || !keys[1].Retired || keys[0].Retired {
		t.Fatalf("public keys after -retire-previous = %+v", keys)
	}
	var data map[string]any
	if err := json.Unmarshal(rec.got[0].Data, &data); err != nil || data["retired_previous"] != true {
		t.Fatalf("rekey data = %v (%v)", data, err)
	}
	// The gateway key is never touched by this mode.
	for _, name := range s.touched {
		if name != secretAuditReceiptKey {
			t.Errorf("touched %q", name)
		}
	}
}

// Each refusal fires before anything opens.
func TestRotateAuditReceiptKeyModeRefusals(t *testing.T) {
	const unreachable = "postgres://nobody@127.0.0.1:1/nope?connect_timeout=1"
	for name, tt := range map[string]struct {
		set  func(f *bootFlags)
		want string
	}{
		"beside gateway rotate": {func(f *bootFlags) { *f.rotateGatewayIdentityKey = true }, "mode of its own"},
		"beside rewrap":         {func(f *bootFlags) { *f.rewrap = true }, "mode of its own"},
		"no database":           {func(f *bootFlags) { *f.dsn = "" }, "WARDYN_PG_DSN"},
		"unreachable":           {func(*bootFlags) {}, "connect"},
	} {
		t.Run(name, func(t *testing.T) {
			f := testGatewayFlags(unreachable)
			*f.rotateGatewayIdentityKey, *f.rotateAuditReceiptKey = false, true
			tt.set(f)
			err := rotateAuditReceiptKeyMode(f)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to name %q", err, tt.want)
			}
		})
	}
	// -retire-previous alone names both modes it belongs to.
	f := testGatewayFlags(unreachable)
	*f.rotateGatewayIdentityKey, *f.retirePrevious = false, true
	if _, err := f.maintenance(); err == nil || !strings.Contains(err.Error(), "-rotate-audit-receipt-key") {
		t.Errorf("retire alone: err = %v, want it to name -rotate-audit-receipt-key", err)
	}
}
