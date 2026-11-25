// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// WARDYN_IDENTITY=spire boots only beside the k8s runner.
func TestRefuseSpireOffKubernetes(t *testing.T) {
	for _, r := range []string{"none", "docker", "remote", ""} {
		if refuseSpireOffKubernetes(r) == nil {
			t.Errorf("runner %q: spire must refuse to boot off Kubernetes", r)
		}
	}
	if err := refuseSpireOffKubernetes("k8s"); err != nil {
		t.Fatalf("runner k8s: %v", err)
	}
}

// The proxy SPIFFE mount is refused under any provider but spire.
func TestNewIdentityProviderRefusesSPIFFEMountOffSpire(t *testing.T) {
	for _, k := range []string{"WARDYN_K8S_SPIFFE_CSI_DRIVER", "WARDYN_K8S_SPIFFE_PROXY_SERVICE_ACCOUNT"} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, "x")
			if p, err := newIdentityProvider("embedded", "k8s", identity.Deps{}); err == nil || p != nil {
				t.Fatalf("embedded with %s set = (%v, %v), want a refusal", k, p, err)
			}
		})
	}
}
