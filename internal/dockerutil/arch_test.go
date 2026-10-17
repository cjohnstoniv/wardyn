// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package dockerutil

import "testing"

func TestNormalizeArch(t *testing.T) {
	for in, want := range map[string]string{
		"x86_64": "amd64", "amd64": "amd64", "AMD64": "amd64", " x86_64\n": "amd64",
		"aarch64": "arm64", "arm64": "arm64", "ARM64": "arm64",
		"": "", "armv7l": "", "riscv64": "", "s390x": "", "i386": "",
	} {
		if got := NormalizeArch(in); got != want {
			t.Errorf("NormalizeArch(%q) = %q, want %q", in, got, want)
		}
	}
}
