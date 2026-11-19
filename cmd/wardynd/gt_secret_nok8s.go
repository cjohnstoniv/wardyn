// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !k8s

package main

import "errors"

// newGroundtruthSecretPatch refuses: only a -tags k8s build talks to the
// Kubernetes API.
func newGroundtruthSecretPatch(_, _ string) (gtSecretPatch, error) {
	return nil, errors.New("WARDYN_GROUNDTRUTH_TOKEN_SECRET needs a wardynd built with Kubernetes support (-tags k8s)")
}
