// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// staticKeyServiceCheck is the kek_service_static_key row: amber while the key service that wraps
// every data key is reached with a long-lived access key, which anyone who copies it can use from
// anywhere, where a role's credentials expire and are bound to the workload.
func staticKeyServiceCheck(keyService string, static bool) (SetupCheck, bool) {
	if keyService == "" || !static {
		return SetupCheck{}, false
	}
	return SetupCheck{
		ID: "kek_service_static_key", Label: "Credential key access", Status: "warn",
		Detail: "The key service (" + keyService + ") is reached with a long-lived AWS access key. Anyone who copies it can unlock every credential from anywhere until it is revoked.",
		Fix:    "Run wardynd with an IAM role instead: IRSA or EKS Pod Identity, so the credentials expire and belong to the workload.",
	}, true
}
