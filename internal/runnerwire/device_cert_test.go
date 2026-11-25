// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/devicecert"
)

// The report's JSON names are the contract between the helper's output and the organisation's decoder.
func TestDeviceCertReport_JSONShape(t *testing.T) {
	at := time.Unix(1795600000, 0).UTC()
	in := DeviceCertReport{Proofs: []DeviceCertProof{{Chain: [][]byte{{1, 2}, {3}}, SignedAt: at, Signature: []byte{9}}}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"proofs":[{"chain":["AQI=","Aw=="],"signed_at":"` + at.Format(time.RFC3339) + `","signature":"CQ=="}]}`
	if string(b) != want {
		t.Fatalf("json %s, want %s", b, want)
	}
	var out DeviceCertReport
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil || len(out.Proofs) != 1 || !out.Proofs[0].SignedAt.Equal(at) || !slices.Equal(out.Proofs[0].Signature, []byte{9}) {
		t.Fatalf("decoded %+v, %v", out, err)
	}
}

// device_cert is a kind the organisation ingests, so a runner may send it.
func TestEventKinds_HoldsDeviceCert(t *testing.T) {
	if EventDeviceCert != "device_cert" || !slices.Contains(EventKinds, EventDeviceCert) {
		t.Fatalf("EventDeviceCert %q, EventKinds %v", EventDeviceCert, EventKinds)
	}
}

// The helper derives the runner's fingerprint from runner.key itself; it must be the one the runner prints and
// the organisation stores.
func TestDeviceCertFingerprintMatchesRunnerwire(t *testing.T) {
	for i := 0; i < 3; i++ {
		pub, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "runner.key"), key, 0600); err != nil {
			t.Fatal(err)
		}
		got, err := devicecert.FingerprintFromStateDir(dir)
		if err != nil || got != Fingerprint(pub) {
			t.Fatalf("devicecert fingerprint %q, %v; runnerwire %q", got, err, Fingerprint(pub))
		}
	}
}
