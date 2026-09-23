// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package kektest is the conformance suite every key-encryption key is held
// to (credential-storage design §2.2, §2.3): the local key, Vault Transit
// against the fake and against real Vault and OpenBao servers. A provider
// passes when a wrap is bound to its row, a failure is classified the way the
// sink acts on it (only an unreachable service is transient; everything else
// drops the credential at once), and a versioned key rotates and retires.
package kektest

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// Hooks act on the key at its service. A nil hook skips the cases that need it.
type Hooks struct {
	// Rotate makes a new key version the one every wrap uses.
	Rotate func(t *testing.T)
	// Retire refuses unwraps under versions below minVersion
	// (Transit's min_decryption_version).
	Retire func(t *testing.T, minVersion int)
	// Unreachable stops the service answering and returns the func that
	// brings it back.
	Unreachable func(t *testing.T) (restore func())
	// Disable makes the key unusable at its service for good. It runs last.
	Disable func(t *testing.T)
}

// Run holds the KEK newKEK returns to the contract. newKEK is called more than
// once and each call must return a KEK over the same key: a second instance
// (a restarted wardynd) must unwrap what the first wrapped.
func Run(t *testing.T, newKEK func(t *testing.T) kek.KEK, h Hooks) {
	k := newKEK(t)
	if k.ID() == "" {
		t.Fatal("ID is empty: rows could not name the key that wrapped them")
	}
	t.Run("roundtrip_and_restart", func(t *testing.T) { roundtrip(t, k, newKEK(t)) })
	t.Run("bound_to_its_row", func(t *testing.T) { boundToRow(t, k) })
	t.Run("refuses_bad_input", func(t *testing.T) { badInput(t, k) })
	t.Run("tampered_wrap_is_definitive", func(t *testing.T) { tampered(t, k) })
	if v, ok := k.(kek.Versioned); ok && h.Rotate != nil {
		t.Run("rotate_then_retire", func(t *testing.T) { rotateRetire(t, v, h) })
	}
	if h.Unreachable != nil {
		t.Run("unreachable_is_transient", func(t *testing.T) { unreachable(t, k, h.Unreachable) })
	}
	if h.Disable != nil {
		t.Run("disabled_is_definitive", func(t *testing.T) { disabled(t, k, h.Disable) })
	}
}

// DEK is a fresh random data key.
func DEK(t *testing.T) []byte {
	t.Helper()
	d := make([]byte, kek.DEKSize)
	if _, err := rand.Read(d); err != nil {
		t.Fatal(err)
	}
	return d
}

// Wrap wraps dek under bind, failing the test on error.
func Wrap(t *testing.T, k kek.KEK, dek []byte, bind map[string]string) []byte {
	t.Helper()
	w, err := k.Wrap(t.Context(), dek, bind)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	return w
}

// MustUnwrap unwraps w under bind and checks it opens to dek.
func MustUnwrap(t *testing.T, k kek.KEK, w, dek []byte, bind map[string]string) {
	t.Helper()
	got, err := k.Unwrap(t.Context(), w, bind)
	if err != nil {
		t.Fatalf("Unwrap under its own row: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("Unwrap opened to different bytes")
	}
}

// Definitive fails the test unless err is a refusal the sink drops the
// credential on at once: not nil, not transient, never not-found.
func Definitive(t *testing.T, what string, err error) {
	t.Helper()
	switch {
	case err == nil:
		t.Fatalf("%s succeeded; want a definitive refusal", what)
	case errors.Is(err, secretstore.ErrUnavailable):
		t.Fatalf("%s = %v; want a definitive refusal, not a transient one (the proxy would keep the credential for the grace)", what, err)
	case errors.Is(err, secretstore.ErrNotFound):
		t.Fatalf("%s = %v; want a refusal, never not-found (boot would mint over it)", what, err)
	}
}

func roundtrip(t *testing.T, k, again kek.KEK) {
	dek, bind := DEK(t), kek.Bind("alice@example.com", "pat")
	w1 := Wrap(t, k, dek, bind)
	w2 := Wrap(t, k, dek, bind)
	if bytes.Equal(w1, w2) {
		t.Fatal("two wraps of one data key are identical: the wrap is not randomised")
	}
	if bytes.Contains(w1, dek) {
		t.Fatal("the wrap carries the data key in the clear")
	}
	MustUnwrap(t, k, w1, dek, bind)
	if again.ID() != k.ID() {
		t.Fatalf("a second instance over the same key has ID %q, want %q", again.ID(), k.ID())
	}
	MustUnwrap(t, again, w2, dek, bind)
}

func boundToRow(t *testing.T, k kek.KEK) {
	dek := DEK(t)
	w := Wrap(t, k, dek, kek.Bind("alice@example.com", "pat"))
	for _, b := range []map[string]string{
		kek.Bind("bob@example.com", "pat"),
		kek.Bind("alice@example.com", "other"),
		kek.Bind("", "pat"), // the operator's row of the same name
		kek.Bind("alice@example.compat", ""),
	} {
		_, err := k.Unwrap(t.Context(), w, b)
		Definitive(t, "Unwrap under another row's binding", err)
	}
	_, err := k.Unwrap(t.Context(), w, map[string]string{kek.BindOwner: "alice@example.com"})
	Definitive(t, "Unwrap with no name in the binding", err)
	// The operator's row binds as tightly as a person's.
	op := Wrap(t, k, dek, kek.Bind("", "github-app-key"))
	_, err = k.Unwrap(t.Context(), op, kek.Bind("alice@example.com", "github-app-key"))
	Definitive(t, "Unwrap of the operator's wrap under a person's row", err)
}

func badInput(t *testing.T, k kek.KEK) {
	for _, n := range []int{0, 16, kek.DEKSize + 1} {
		if _, err := k.Wrap(t.Context(), make([]byte, n), kek.Bind("", "k")); err == nil {
			t.Errorf("Wrap of a %d-byte data key succeeded; want a refusal", n)
		}
	}
	if _, err := k.Wrap(t.Context(), DEK(t), map[string]string{kek.BindName: "k"}); err == nil {
		t.Error("Wrap with no owner in the binding succeeded; a zero value would seal to the wrong row")
	}
}

func tampered(t *testing.T, k kek.KEK) {
	bind := kek.Bind("", "k")
	w := Wrap(t, k, DEK(t), bind)
	for name, bad := range map[string][]byte{
		"last byte flipped": flip(w, len(w)-1),
		"middle flipped":    flip(w, len(w)/2),
		"truncated":         w[:len(w)-4],
		"empty":             {},
	} {
		_, err := k.Unwrap(t.Context(), bad, bind)
		Definitive(t, "Unwrap of a wrap with its "+name, err)
	}
}

// flip returns a copy of b with byte i changed to another byte that stays
// printable, so a text wrap (Transit's base64) is still well-formed text.
func flip(b []byte, i int) []byte {
	out := bytes.Clone(b)
	if out[i] == 'A' {
		out[i] = 'B'
	} else {
		out[i] = 'A'
	}
	return out
}

func rotateRetire(t *testing.T, v kek.Versioned, h Hooks) {
	ctx := t.Context()
	bind := kek.Bind("", "rotated")
	dek1 := DEK(t)
	w1 := Wrap(t, v, dek1, bind)
	n1, err := v.WrapVersion(w1)
	if err != nil {
		t.Fatalf("WrapVersion of a fresh wrap: %v", err)
	}
	if latest, err := v.LatestVersion(ctx); err != nil || latest != n1 {
		t.Fatalf("LatestVersion = (%d, %v), want %d (the version the last wrap named)", latest, err, n1)
	}
	h.Rotate(t)
	latest, err := v.LatestVersion(ctx)
	if err != nil || latest != n1+1 {
		t.Fatalf("LatestVersion after a rotation = (%d, %v), want %d", latest, err, n1+1)
	}
	dek2 := DEK(t)
	w2 := Wrap(t, v, dek2, bind)
	if n, _ := v.WrapVersion(w2); n != n1+1 {
		t.Fatalf("a wrap after the rotation names v%d, want v%d", n, n1+1)
	}
	MustUnwrap(t, v, w1, dek1, bind) // the old version still opens until it is retired
	if h.Retire == nil {
		return
	}
	h.Retire(t, n1+1)
	t.Cleanup(func() { h.Retire(t, 1) })
	_, err = v.Unwrap(ctx, w1, bind)
	Definitive(t, "Unwrap under a retired version", err)
	MustUnwrap(t, v, w2, dek2, bind)
}

func unreachable(t *testing.T, k kek.KEK, stop func(t *testing.T) func()) {
	bind := kek.Bind("", "outage")
	dek := DEK(t)
	w := Wrap(t, k, dek, bind)
	restore := stop(t)
	_, err := k.Unwrap(t.Context(), w, bind)
	if !errors.Is(err, secretstore.ErrUnavailable) {
		restore()
		t.Fatalf("Unwrap with the key service unreachable = %v; want ErrUnavailable (the sink's transient 503)", err)
	}
	_, err = k.Wrap(t.Context(), dek, bind)
	restore()
	if !errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("Wrap with the key service unreachable = %v; want ErrUnavailable", err)
	}
	MustUnwrap(t, k, w, dek, bind)
}

func disabled(t *testing.T, k kek.KEK, disable func(t *testing.T)) {
	bind := kek.Bind("", "disabled")
	w := Wrap(t, k, DEK(t), bind)
	disable(t)
	_, err := k.Unwrap(t.Context(), w, bind)
	Definitive(t, "Unwrap with the key disabled", err)
	_, err = k.Wrap(t.Context(), DEK(t), bind)
	Definitive(t, "Wrap with the key disabled", err)
}
