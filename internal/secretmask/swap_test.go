// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask_test

import (
	"bytes"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// countingDst records every Write the MaskingWriter forwards.
type countingDst struct {
	bytes.Buffer
	writes int
}

func (d *countingDst) Write(p []byte) (int, error) {
	d.writes++
	return d.Buffer.Write(p)
}

func masker(secrets ...string) secretmask.Masker {
	var set [][]byte
	for _, s := range secrets {
		set = append(set, []byte(s))
	}
	return secretmask.NewMasker(set)
}

// A tail held as the start of an old secret is kept, raw, across the swap, and a new secret that begins in it and
// ends after the swap is masked whole. The swap itself forwards nothing.
func TestMaskingWriter_SetMaskerMasksASecretSplitAcrossTheSwap(t *testing.T) {
	const old, added = "OLDSECRET-aaaa-1111", "OLDSECRET-NEWVALUE-9"
	var dst countingDst
	w := secretmask.NewMaskingWriter(&dst, masker(old))
	if _, err := w.Write([]byte("out: OLDSEC")); err != nil {
		t.Fatal(err)
	}
	if dst.String() != "out: " {
		t.Fatalf("forwarded before the swap = %q, want the bytes before the held start of a secret", dst.String())
	}
	writes := dst.writes
	w.SetMasker(masker(old, added))
	if dst.writes != writes {
		t.Fatalf("SetMasker forwarded %d writes, want none", dst.writes-writes)
	}
	if _, err := w.Write([]byte("RET-NEWVALUE-9 end")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := dst.String(); got != "out: <secret-hidden> end" {
		t.Fatalf("masked across the swap = %q", got)
	}
}

// The held bytes a forwarded placeholder already stands for stay hidden across the swap: a new secret that overlaps
// them joins that placeholder, and no byte of either is forwarded raw.
func TestMaskingWriter_SetMaskerKeepsTheOverlapState(t *testing.T) {
	const first, overlap, added = "tok-12345678", "5678-more-secret", "5678-zzzz-newval"
	var dst countingDst
	w := secretmask.NewMaskingWriter(&dst, masker(first, overlap))
	if _, err := w.Write([]byte("tok-12345678")); err != nil {
		t.Fatal(err)
	}
	if dst.String() != "<secret-hidden>" {
		t.Fatalf("forwarded before the swap = %q, want the placeholder for the whole first secret", dst.String())
	}
	w.SetMasker(masker(first, overlap, added))
	if _, err := w.Write([]byte("-zzzz-newval end")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := dst.String(); got != "<secret-hidden> end" {
		t.Fatalf("masked after the swap = %q, want the new secret joined to the placeholder", got)
	}
}

// A swap to a corpus without the overlapping secret (an erasure dropped it) still never forwards the held bytes the
// placeholder stands for.
func TestMaskingWriter_SetMaskerNeverRepeatsHiddenBytes(t *testing.T) {
	const first, overlap = "tok-12345678", "5678-more-secret"
	var dst countingDst
	w := secretmask.NewMaskingWriter(&dst, masker(first, overlap))
	if _, err := w.Write([]byte("tok-12345678")); err != nil {
		t.Fatal(err)
	}
	w.SetMasker(masker(first))
	if _, err := w.Write([]byte("-other")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := dst.String(); got != "<secret-hidden>-other" {
		t.Fatalf("masked after the swap = %q, want no byte of the first secret again", got)
	}
}

// The held tail is flushed through the swapped Masker at Close, not the one it was held under.
func TestMaskingWriter_SetMaskerAppliesAtClose(t *testing.T) {
	const old, added = "OLDSECRET-aaaa-1111", "SECRET-aa"
	var dst countingDst
	w := secretmask.NewMaskingWriter(&dst, masker(old))
	if _, err := w.Write([]byte("x OLDSECRET-aa")); err != nil {
		t.Fatal(err)
	}
	if dst.String() != "x " {
		t.Fatalf("forwarded before the swap = %q, want the start of the old secret held", dst.String())
	}
	w.SetMasker(masker(old, added))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := dst.String(); got != "x OLD<secret-hidden>" {
		t.Fatalf("closed after the swap = %q", got)
	}
}

// The Masker JSONVariantMaskerGen returns is the corpus at the generation it returns with it, JSON renderings
// included, and a later registration moves the generation.
func TestRegistry_JSONVariantMaskerGen(t *testing.T) {
	reg := secretmask.NewRegistry()
	run := uuid.New()
	if err := reg.Add(run, []byte("first-secret-value")); err != nil {
		t.Fatal(err)
	}
	m, gen := reg.JSONVariantMaskerGen(run)
	if gen != reg.Generation() {
		t.Fatalf("generation = %d, registry is at %d", gen, reg.Generation())
	}
	if got := string(m.Mask([]byte("a first-secret-value b"))); got != "a <secret-hidden> b" {
		t.Fatalf("masked = %q", got)
	}
	if again, g := reg.JSONVariantMaskerGen(run); g != gen || len(again.Secrets()) != len(m.Secrets()) {
		t.Fatalf("unchanged corpus gave generation %d (want %d) and %d secrets (want %d)", g, gen, len(again.Secrets()), len(m.Secrets()))
	}
	if err := reg.Add(run, []byte("second\nsecret-value")); err != nil {
		t.Fatal(err)
	}
	m2, gen2 := reg.JSONVariantMaskerGen(run)
	if gen2 == gen || gen2 != reg.Generation() {
		t.Fatalf("generation after a registration = %d, before %d, registry %d", gen2, gen, reg.Generation())
	}
	escaped := []byte(`"o","second\nsecret-value"`)
	if got := string(m2.Mask(escaped)); got != `"o","<secret-hidden>"` {
		t.Fatalf("new Masker left the escaped value: %q", got)
	}
	if got := string(m.Mask(escaped)); got != string(escaped) {
		t.Fatalf("old Masker changed with the registry: %q", got)
	}
}
