// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

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
