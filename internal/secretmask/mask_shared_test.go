// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask_test

import (
	"bytes"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// refMask is Mask as it was before the first-byte skip and the shared global set:
// every secret is searched for, and occurrences that overlap are one run.
func refMask(secrets [][]byte, p []byte) []byte {
	var occ [][2]int
	for _, s := range secrets {
		if len(s) < secretmask.MinLen {
			continue
		}
		for i := 0; ; {
			j := bytes.Index(p[i:], s)
			if j < 0 {
				break
			}
			occ = append(occ, [2]int{i + j, i + j + len(s)})
			i += j + 1
		}
	}
	slices.SortFunc(occ, func(a, b [2]int) int { return a[0] - b[0] })
	var out []byte
	last := 0
	for i := 0; i < len(occ); {
		start, end := occ[i][0], occ[i][1]
		for i++; i < len(occ) && occ[i][0] < end; i++ {
			end = max(end, occ[i][1])
		}
		out = append(append(out, p[last:start]...), "<secret-hidden>"...)
		last = end
	}
	return append(out, p[last:]...)
}

func randValue(rng *rand.Rand) []byte {
	const alphabet = "ab\"\n<\\&"
	v := make([]byte, secretmask.MinLen+rng.Intn(5))
	for i := range v {
		v[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return v
}

// randData is noise with corpus entries spliced in, so most inputs hit something.
func randData(rng *rand.Rand, corpus [][]byte) []byte {
	var p []byte
	for range rng.Intn(8) {
		p = append(p, randValue(rng)[:rng.Intn(4)]...)
		if len(corpus) > 0 && rng.Intn(2) == 0 {
			v := corpus[rng.Intn(len(corpus))]
			p = append(p, v[:rng.Intn(len(v))+1]...)
		}
	}
	return p
}

func TestMaskMatchesSearchingEverySecret(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for range 300 {
		var secrets [][]byte
		for range rng.Intn(12) {
			secrets = append(secrets, randValue(rng))
		}
		m := secretmask.NewMasker(secrets)
		for range 20 {
			p := randData(rng, secrets)
			if got, want := m.Mask(p), refMask(secrets, p); !bytes.Equal(got, want) {
				t.Fatalf("Mask(%q) over %q = %q, want %q", p, secrets, got, want)
			}
		}
	}
}

func TestMaskSkipsSecretsWhoseFirstByteIsAbsent(t *testing.T) {
	var secrets [][]byte
	for _, first := range "ABCDEFGHIJ" {
		secrets = append(secrets, []byte(string(first)+"-secret-value"))
	}
	m := secretmask.NewMasker(secrets)
	searched := secretmask.CountSearched(t)
	if got := m.Mask([]byte("mno pqr xyz")); string(got) != "mno pqr xyz" {
		t.Fatalf("Mask changed data holding no secret: %q", got)
	}
	if *searched != 0 {
		t.Fatalf("searched %d secrets in data holding none of their first bytes, want 0", *searched)
	}
	// Only 'E' is in the data, so only the secret starting with it is worth a search.
	if got := m.Mask([]byte("mno E-secret-value")); string(got) != "mno <secret-hidden>" {
		t.Fatalf("Mask = %q", got)
	}
	if *searched != 1 {
		t.Fatalf("searched %d secrets, want 1", *searched)
	}
}

func TestRegistryMaskersMatchTheWholeCorpusMasker(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for range 100 {
		r := secretmask.NewRegistry()
		var globals [][]byte
		for range rng.Intn(6) {
			globals = append(globals, randValue(rng))
		}
		if err := r.AddGlobal(0, "", "cred", time.Now(), globals...); err != nil {
			t.Fatal(err)
		}
		run := uuid.New()
		for range rng.Intn(4) {
			v := randValue(rng)
			if len(globals) > 0 && rng.Intn(2) == 0 {
				v = globals[rng.Intn(len(globals))]
			}
			if err := r.Add(run, v); err != nil {
				t.Fatal(err)
			}
		}
		snap := r.Snapshot(run)
		variants := secretmask.JSONEscapedVariants(snap)
		for range 20 {
			p := randData(rng, variants)
			if got, want := r.Masker(run).Mask(p), refMask(snap, p); !bytes.Equal(got, want) {
				t.Fatalf("plain Mask(%q) = %q, want %q", p, got, want)
			}
			if got, want := r.JSONVariantMasker(run).Mask(p), refMask(variants, p); !bytes.Equal(got, want) {
				t.Fatalf("variant Mask(%q) = %q, want %q", p, got, want)
			}
		}
	}
}

// secretOf returns the Masker's own copy of v, to compare by address.
func secretOf(t *testing.T, m secretmask.Masker, v string) []byte {
	t.Helper()
	for _, s := range m.Secrets() {
		if string(s) == v {
			return s
		}
	}
	t.Fatalf("%q is not in the masking set", v)
	return nil
}

func TestRegistryRunsShareTheDerivedGlobals(t *testing.T) {
	r := secretmask.NewRegistry()
	const global = "line-one\nline-two-of-a-pem-key"
	if err := r.AddGlobal(0, "", "cred", time.Now(), []byte(global)); err != nil {
		t.Fatal(err)
	}
	const escaped = `line-one\nline-two-of-a-pem-key`
	a, b := uuid.New(), uuid.New()
	r.Add(a, []byte("a-secret-only-run-a-holds"))
	first := r.JSONVariantMasker(a)
	other := r.JSONVariantMasker(b)
	if &secretOf(t, first, escaped)[0] != &secretOf(t, other, escaped)[0] || &secretOf(t, first, global)[0] != &secretOf(t, other, global)[0] {
		t.Error("two runs derived the global values and their escaped variants separately")
	}

	// A run's own registration bumps the generation but changes no global, so what was
	// derived for the globals is reused by the rebuild.
	r.Add(b, []byte("a-secret-only-run-b-holds"))
	if &secretOf(t, r.JSONVariantMasker(a), escaped)[0] != &secretOf(t, first, escaped)[0] {
		t.Error("a run's registration made the global variants be derived again")
	}
	if !bytes.Contains(r.JSONVariantMasker(a).Mask([]byte("x a-secret-only-run-b-holds y")), []byte("b-holds")) {
		t.Error("a run's masker holds another run's value")
	}
}
