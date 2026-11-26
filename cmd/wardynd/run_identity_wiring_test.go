// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunIdentityWrapsEveryDelegationRecorder: every DelegationRecorder this binary builds sits directly inside
// a RunIdentityRecorder, so a row written under a run token is stamped before it is masked, sealed or stored.
func TestRunIdentityWrapsEveryDelegationRecorder(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v (%d files)", err, len(files))
	}
	var built, wrapped int
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		built += strings.Count(string(src), "audit.DelegationRecorder{")
		wrapped += strings.Count(string(src), "audit.RunIdentityRecorder{Inner: audit.DelegationRecorder{")
	}
	if built == 0 || built != wrapped {
		t.Fatalf("%d DelegationRecorder literals, %d inside a RunIdentityRecorder; want equal and non-zero", built, wrapped)
	}
}
