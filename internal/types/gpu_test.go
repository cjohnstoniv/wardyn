// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

func TestGPUKindValid(t *testing.T) {
	for _, k := range []GPUKind{"", "nvidia", "render"} {
		if !k.Valid() {
			t.Errorf("%q should be valid", k)
		}
	}
	for _, k := range []GPUKind{"cuda", "NVIDIA", " nvidia"} {
		if k.Valid() {
			t.Errorf("%q should not be valid", k)
		}
	}
}
