// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

func testDEK() []byte { return bytes.Repeat([]byte{7}, kek.DEKSize) }

// A wrap is bound to its row by associated_data: the same data key does not
// unwrap for another owner or another name, and the refusal is definitive.
func TestTransit_WrapIsBoundToItsRow(t *testing.T) {
	f := newFakeVault(t)
	tr := newFakeTransit(t, f)
	ctx := t.Context()
	if tr.ID() != "transit:transit/wardyn" {
		t.Fatalf("ID = %q", tr.ID())
	}
	// The {host} of SETUP_CHECK.KEK_SERVICE is the Transit address's host.
	if want := "Vault Transit at " + strings.TrimPrefix(f.srv.URL, "http://"); tr.Describe() != want {
		t.Fatalf("Describe = %q, want %q", tr.Describe(), want)
	}
	w, err := tr.Wrap(ctx, testDEK(), kek.Bind("alice", "pat"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(w), "vault:v1:") {
		t.Fatalf("wrap = %q, want a Transit ciphertext", w)
	}
	got, err := tr.Unwrap(ctx, w, kek.Bind("alice", "pat"))
	if err != nil || !bytes.Equal(got, testDEK()) {
		t.Fatalf("Unwrap = (%x, %v)", got, err)
	}
	for _, b := range []map[string]string{kek.Bind("bob", "pat"), kek.Bind("alice", "other"), kek.Bind("", "pat")} {
		_, err := tr.Unwrap(ctx, w, b)
		if err == nil || errors.Is(err, secretstore.ErrUnavailable) {
			t.Fatalf("Unwrap under %v = %v; want a definitive refusal", b, err)
		}
	}
	if _, err := tr.Wrap(ctx, testDEK(), map[string]string{kek.BindOwner: "alice"}); err == nil {
		t.Fatal("Wrap accepted a bind with no name")
	}
}

// Boot refuses a key that does not bind associated_data (a key type that
// ignores it): a database writer could otherwise move a data key between rows.
func TestTransit_BootRefusesAKeyThatIgnoresAssociatedData(t *testing.T) {
	f := newFakeVault(t)
	f.transitKey("wardyn")
	f.transit.ignoreAD = true
	_, err := openFakeTransit(t, f)
	if err == nil || !strings.Contains(err.Error(), "does not bind a wrap to its row") {
		t.Fatalf("NewTransit over a key that ignores associated_data = %v; want a refusal", err)
	}
}

// Boot fails closed on a missing key or an unreachable Vault.
func TestTransit_BootFailsClosed(t *testing.T) {
	f := newFakeVault(t)
	if _, err := openFakeTransit(t, f); err == nil || !strings.Contains(err.Error(), "encryption key not found") {
		t.Fatalf("NewTransit with no key = %v", err)
	}
	f.transitKey("wardyn")
	for _, bad := range []struct{ mount, key string }{{"transit", "a/b"}, {"transit", ""}, {"../x", "wardyn"}} {
		if _, err := NewTransit(t.Context(), Config{Addr: f.srv.URL, Auth: AuthTokenFile, TokenFile: writeFile(t, "x")}, bad.mount, bad.key); err == nil {
			t.Errorf("NewTransit(%q, %q) accepted a bad path", bad.mount, bad.key)
		}
	}
	f.srv.Close()
	if _, err := openFakeTransit(t, f); err == nil {
		t.Fatal("NewTransit against a closed server succeeded")
	}
}

// 5xx is transient (the K8 grace applies); 403 and a retired version are
// definitive, so revoking access or retiring a version bites at once.
func TestTransit_Classification(t *testing.T) {
	f := newFakeVault(t)
	tr := newFakeTransit(t, f)
	ctx := t.Context()
	w, err := tr.Wrap(ctx, testDEK(), kek.Bind("", "k"))
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.force = []int{503, 503, 503, 503}
	f.mu.Unlock()
	if _, err := tr.Unwrap(ctx, w, kek.Bind("", "k")); !errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("Unwrap while sealed = %v; want ErrUnavailable", err)
	}
	f.mu.Lock()
	f.revoked = true
	f.mu.Unlock()
	if _, err := tr.Unwrap(ctx, w, kek.Bind("", "k")); err == nil || errors.Is(err, secretstore.ErrUnavailable) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("Unwrap with the policy revoked = %v; want a definitive 403", err)
	}
	f.mu.Lock()
	f.revoked = false
	f.transit.minDecrypt = 2
	f.mu.Unlock()
	if _, err := tr.Unwrap(ctx, w, kek.Bind("", "k")); err == nil || errors.Is(err, secretstore.ErrUnavailable) || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("Unwrap of a retired version = %v; want a definitive refusal", err)
	}
	// A row that holds no Transit ciphertext never reaches Vault.
	before := f.callCount("POST transit/decrypt")
	if _, err := tr.Unwrap(ctx, []byte("local-wrap-bytes"), kek.Bind("", "k")); err == nil {
		t.Fatal("Unwrap of a non-Transit blob succeeded")
	}
	if f.callCount("POST transit/decrypt") != before {
		t.Fatal("Unwrap sent a non-Transit blob to Vault")
	}
}

// A rotation moves new wraps to the latest version; older wraps still unwrap
// until min_decryption_version retires them.
func TestTransit_Versions(t *testing.T) {
	f := newFakeVault(t)
	tr := newFakeTransit(t, f)
	ctx := t.Context()
	w1, _ := tr.Wrap(ctx, testDEK(), kek.Bind("", "k"))
	f.rotateTransit()
	if n, err := tr.LatestVersion(ctx); err != nil || n != 2 {
		t.Fatalf("LatestVersion = (%d, %v), want 2", n, err)
	}
	if n, err := tr.WrapVersion(w1); err != nil || n != 1 {
		t.Fatalf("WrapVersion(w1) = (%d, %v), want 1", n, err)
	}
	w2, _ := tr.Wrap(ctx, testDEK(), kek.Bind("", "k"))
	if n, _ := tr.WrapVersion(w2); n != 2 {
		t.Fatalf("a wrap after the rotation names v%d, want v2", n)
	}
	if _, err := tr.Unwrap(ctx, w1, kek.Bind("", "k")); err != nil {
		t.Fatalf("v1 wrap after a rotation: %v", err)
	}
	for _, bad := range []string{"vault:v0:x", "vault:vx:y", "vault:v3", "age-encryption.org/v1"} {
		if _, err := tr.WrapVersion([]byte(bad)); err == nil {
			t.Errorf("WrapVersion(%q) accepted it", bad)
		}
	}
}

// The request carries the row's binding as associated_data: base64 of
// kek.WrapAAD under this key's id.
func TestTransit_SendsAssociatedData(t *testing.T) {
	f := newFakeVault(t)
	tr := newFakeTransit(t, f)
	if _, err := tr.Wrap(t.Context(), testDEK(), kek.Bind("alice", "pat")); err != nil {
		t.Fatal(err)
	}
	want, _ := kek.WrapAAD(kek.Bind("alice", "pat"), tr.ID())
	f.mu.Lock()
	defer f.mu.Unlock()
	ads := f.transit.ads
	if len(ads) == 0 || ads[len(ads)-1] != base64.StdEncoding.EncodeToString(want) {
		t.Fatalf("associated_data sent = %q; want base64 of kek.WrapAAD", ads)
	}
}
