// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// staticKeyServiceCheck is the kek_service_static_key row: amber while the key service that wraps
// every data key is reached with a long-lived access key, which anyone who copies it can use from
// anywhere, where a role's credentials expire and are bound to the workload. The node's instance role
// expires but belongs to the whole node, so every pod or process on it can unlock the credentials: the same row.
func staticKeyServiceCheck(keyService string, static, nodeRole bool) (SetupCheck, bool) {
	if keyService == "" || !static && !nodeRole {
		return SetupCheck{}, false
	}
	if !static {
		return SetupCheck{
			ID: "kek_service_static_key", Label: "Credential key access", Status: "warn",
			Detail: "The key service (" + keyService + ") is reached with the node's instance role, which every pod or process on that machine can use to unlock every credential.",
			Fix:    "Run wardynd with a role of its own instead: IRSA or EKS Pod Identity on Kubernetes, or a role only wardynd can assume elsewhere, so the credentials belong to the workload alone.",
		}, true
	}
	return SetupCheck{
		ID: "kek_service_static_key", Label: "Credential key access", Status: "warn",
		Detail: "The key service (" + keyService + ") is reached with a long-lived AWS access key. Anyone who copies it can unlock every credential from anywhere until it is revoked.",
		Fix:    "Run wardynd with an IAM role instead: IRSA or EKS Pod Identity, so the credentials expire and belong to the workload.",
	}, true
}
