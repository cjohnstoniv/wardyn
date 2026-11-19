// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditsign

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
)

var t0 = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func mustNewKey(t *testing.T) []byte {
	t.Helper()
	raw, err := NewKey(t0)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	return raw
}

func mustLoad(t *testing.T, raw []byte) *Key {
	t.Helper()
	k, err := Load(raw)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return k
}

func TestKey_NewLoadSignVerify(t *testing.T) {
	raw := mustNewKey(t)
	k := mustLoad(t, raw)
	if got, want := k.KeyID(), auditreceipt.KeyID(k.PublicKey()); got != want {
		t.Fatalf("kid %q, want %q", got, want)
	}
	msg := []byte(`{"format":"x"}`)
	sig := k.Sign(msg)
	if !ed25519.Verify(k.PublicKey(), msg, sig) {
		t.Fatal("signature does not verify under the public key")
	}
	if !k.Verify(k.KeyID(), msg, sig) {
		t.Fatal("Verify refused the key's own signature")
	}
	if k.Verify(k.KeyID(), []byte("other"), sig) || k.Verify(strings.Repeat("0", 32), msg, sig) {
		t.Fatal("Verify accepted a wrong message or an unknown kid")
	}
	if strings.Contains(string(raw), "PRIVATE") || !strings.Contains(string(raw), `"seed"`) {
		t.Fatalf("stored value is not the documented JSON shape: %s", raw)
	}
	keys := k.PublicKeys()
	if len(keys) != 1 || keys[0].Kid != k.KeyID() || keys[0].Retired || keys[0].ReplacedAt != nil {
		t.Fatalf("public keys of a fresh key = %+v", keys)
	}
	if keys[0].Fingerprint != auditreceipt.Fingerprint(k.PublicKey()) || !keys[0].CreatedAt.Equal(t0) {
		t.Fatalf("fingerprint or created_at wrong: %+v", keys[0])
	}
	block, _ := pem.Decode([]byte(keys[0].PublicKeyPEM))
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatalf("public_key_pem is not a PUBLIC KEY block: %q", keys[0].PublicKeyPEM)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil || !ed25519.PublicKey(keys[0].PublicKey).Equal(pub) {
		t.Fatalf("PEM does not hold the same key: %v", err)
	}
}

func TestKey_LoadRefusesBadShapes(t *testing.T) {
	good := mustNewKey(t)
	var doc map[string]any
	if err := json.Unmarshal(good, &doc); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"empty":         ``,
		"not json":      `seed`,
		"unknown field": `{"current":{"seed":"AAAA","created_at":"2026-11-07T12:00:00Z"},"previous":[],"extra":1}`,
		"short seed":    `{"current":{"seed":"AAAA","created_at":"2026-11-07T12:00:00Z"},"previous":[]}`,
		"no seed":       `{"current":{"created_at":"2026-11-07T12:00:00Z"},"previous":[]}`,
		"prev kid mismatch": `{"current":` + string(mustJSON(t, doc["current"])) +
			`,"previous":[{"public_key":"` + b64(make([]byte, 32)) + `","kid":"00000000000000000000000000000000","created_at":"2026-11-07T12:00:00Z","replaced_at":"2026-11-07T12:00:00Z","retired":false}]}`,
		"prev short key": `{"current":` + string(mustJSON(t, doc["current"])) +
			`,"previous":[{"public_key":"AAAA","kid":"00000000000000000000000000000000","created_at":"2026-11-07T12:00:00Z","replaced_at":"2026-11-07T12:00:00Z","retired":false}]}`,
	} {
		if _, err := Load([]byte(raw)); err == nil {
			t.Errorf("%s: Load accepted %s", name, raw)
		}
	}
}

// TestRotate_KeepsPreviousPublic: every replaced key stays in the public set forever, newest first, so a
// checkpoint it signed years ago still verifies; the private half never survives a rotation.
func TestRotate_KeepsPreviousPublic(t *testing.T) {
	raw := mustNewKey(t)
	first := mustLoad(t, raw)
	t1 := t0.Add(time.Hour)
	raw2, err := Rotate(raw, false, t1)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	second := mustLoad(t, raw2)
	raw3, err := Rotate(raw2, false, t1.Add(time.Hour))
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	third := mustLoad(t, raw3)

	keys := third.PublicKeys()
	if len(keys) != 3 {
		t.Fatalf("public keys after two rotations = %d, want 3", len(keys))
	}
	for i, want := range []*Key{third, second, first} {
		if keys[i].Kid != want.KeyID() || keys[i].Retired {
			t.Errorf("key %d = %s retired=%v, want %s not retired", i, keys[i].Kid, keys[i].Retired, want.KeyID())
		}
	}
	if keys[0].ReplacedAt != nil || keys[1].ReplacedAt == nil || !keys[1].ReplacedAt.Equal(t1.Add(time.Hour)) || !keys[2].ReplacedAt.Equal(t1) {
		t.Errorf("replaced_at wrong: %+v", keys)
	}
	msg := []byte("statement")
	if !third.Verify(first.KeyID(), msg, first.Sign(msg)) {
		t.Error("the ring does not verify the first key's signature after two rotations")
	}
	if third.KeyID() == second.KeyID() || second.KeyID() == first.KeyID() {
		t.Error("a rotation kept the same key")
	}
	if strings.Count(string(raw3), `"seed"`) != 1 {
		t.Errorf("a replaced key's seed survived the rotation: %s", raw3)
	}
}

// TestRotate_RetirePreviousMarksRetired: -retire-previous marks only the key being replaced; an older key
// keeps its own flag, and a retired key is still listed (verifiers fail its checkpoints by the flag).
func TestRotate_RetirePreviousMarksRetired(t *testing.T) {
	raw := mustNewKey(t)
	raw2, err := Rotate(raw, false, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	stolen := mustLoad(t, raw2)
	raw3, err := Rotate(raw2, true, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	keys := mustLoad(t, raw3).PublicKeys()
	if len(keys) != 3 {
		t.Fatalf("keys = %d, want 3", len(keys))
	}
	if keys[0].Retired || !keys[1].Retired || keys[1].Kid != stolen.KeyID() || keys[2].Retired {
		t.Errorf("retired flags = %v %v %v, want false true false", keys[0].Retired, keys[1].Retired, keys[2].Retired)
	}
	if _, err := Rotate([]byte(`{}`), false, t0); err == nil {
		t.Error("Rotate accepted a value with no current key")
	}
}

func TestCheckInterval(t *testing.T) {
	for d, ok := range map[time.Duration]bool{
		0: true, time.Second: false, 9 * time.Second: false, 9999 * time.Millisecond: false,
		10 * time.Second: true, time.Minute: true, -time.Second: false,
	} {
		if err := CheckInterval(d); (err == nil) != ok {
			t.Errorf("CheckInterval(%v) = %v, want ok=%v", d, err, ok)
		}
	}
}
