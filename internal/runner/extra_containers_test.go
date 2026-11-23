// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSandboxSpecContainersOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(SandboxSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"containers"`) {
		t.Fatalf("empty spec carries a containers key: %s", b)
	}
	b, err = json.Marshal(SandboxSpec{Containers: []ExtraContainer{{Name: "db", Image: "db@sha256:abc", CPUMillis: 100, MemoryMiB: 64}}})
	if err != nil {
		t.Fatal(err)
	}
	const want = `"containers":[{"name":"db","image":"db@sha256:abc","cpu_millis":100,"memory_mib":64}]`
	if !strings.Contains(string(b), want) {
		t.Fatalf("spec = %s, want it to contain %s", b, want)
	}
}
