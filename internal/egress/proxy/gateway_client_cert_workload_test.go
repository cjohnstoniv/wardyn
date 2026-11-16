// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
)

// check takes a leaf naming this run under the workload that launched it as this run's, and refuses a
// leaf naming another run of that workload, the run with no run segment, or an owner not in its
// written form.
func TestGatewayClientCert_CheckTakesAWorkloadsRun(t *testing.T) {
	raw, err := gatewayidentity.RotateAuthority(nil, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := gatewayidentity.NewAuthority(raw)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	h := &gatewayCertHolder{run: "11111111-1111-1111-1111-111111111111"}
	issue := func(id string) string {
		leaf, _, err := ca.Issue(&key.PublicKey, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		return string(leaf)
	}
	now := time.Now()
	if _, _, err := h.check(issue("spiffe://wardyn.local/workload/service_3Aci-bot/nightly/run/"+h.run), now.Add(time.Minute), &key.PublicKey, now); err != nil {
		t.Fatalf("a leaf naming this run under its workload: %v", err)
	}
	for name, id := range map[string]string{
		"another run of the workload": "spiffe://wardyn.local/workload/service_3Aci-bot/nightly/run/22222222-2222-2222-2222-222222222222",
		"no run segment":              "spiffe://wardyn.local/workload/service_3Aci-bot/nightly/" + h.run,
		"an owner in lowercase hex":   "spiffe://wardyn.local/workload/service_3aci-bot/nightly/run/" + h.run,
	} {
		if _, _, err := h.check(issue(id), now.Add(time.Minute), &key.PublicKey, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
