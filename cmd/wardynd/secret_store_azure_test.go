// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"
)

func strp(s string) *string { return &s }

func testExternalFlags(vaultAddr, kvURL string) (vaultFlags, azureFlags) {
	timeout, versions, maxV := 5*time.Second, 1, 100
	v := vaultFlags{
		addr: strp(vaultAddr), namespace: strp(""), auth: strp("token-file"), authMount: strp("kubernetes"), role: strp(""),
		k8sTokenFile: strp(""), tokenFile: strp("/nonexistent"), caCertFile: strp(""), kvMount: strp("wardyn"), kvPrefix: strp("wardyn"),
		maxVersions: &versions, timeout: &timeout,
	}
	az := azureFlags{
		vaultURL: strp(kvURL), auth: strp("workload-identity"), tenantID: strp("tenant-1"), clientID: strp("client-1"),
		federatedTokenFile: strp("/nonexistent"), authorityHost: strp("https://login.microsoftonline.com"), prefix: strp("wardyn"),
		purge: strp("auto"), maxVersions: &maxV,
	}
	return v, az
}

// One external store at a time: a pointer row names one store, and a
// migration between two goes through local.
func TestBuildExternalStore_RefusesTwoStores(t *testing.T) {
	v, az := testExternalFlags("https://vault.example:8200", "https://kv.example.vault.azure.net")
	if _, err := buildExternalStore(t.Context(), v, az, ""); err == nil || !strings.Contains(err.Error(), "configure one external secret store") {
		t.Fatalf("both stores configured = %v; want a refusal", err)
	}
}

// A Key Vault client that cannot be built fails boot, naming why.
func TestBuildExternalStore_AzureFailsClosed(t *testing.T) {
	v, az := testExternalFlags("", "http://kv.example")
	if _, err := buildExternalStore(t.Context(), v, az, ""); err == nil || !strings.Contains(err.Error(), "refusing to start") || !strings.Contains(err.Error(), "plain http://") {
		t.Fatalf("plain-http Key Vault = %v; want boot refused", err)
	}
	v, az = testExternalFlags("", "https://kv.example.vault.azure.net")
	if _, err := buildExternalStore(t.Context(), v, az, ""); err == nil || !strings.Contains(err.Error(), "federated token file") {
		t.Fatalf("unreadable federated token = %v; want boot refused", err)
	}
}
