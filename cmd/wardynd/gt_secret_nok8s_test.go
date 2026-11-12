// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !k8s

package main

import "testing"

// A build without Kubernetes support cannot write a Secret, so asking it to
// refuses boot rather than leaving the sensor without a token.
func TestGroundtruthTokenDelivery_SecretNeedsK8sBuild(t *testing.T) {
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_FILE", "")
	t.Setenv("WARDYN_GROUNDTRUTH_TOKEN_SECRET", "ns/name")
	if _, _, err := groundtruthTokenDelivery(nil); err == nil {
		t.Fatal("a build without -tags k8s must refuse WARDYN_GROUNDTRUTH_TOKEN_SECRET")
	}
}
