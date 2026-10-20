// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package certbundle

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// runnerdAnchorFileCases is the runner's org CA check's copy of this package's anchor-file table.
var runnerdAnchorFileCases = filepath.Join("..", "..", "cmd", "wardyn-runnerd", "testdata", "anchor-file-cases.json")

// TestAnchorFileCasesMatchRunnerd: both loaders of CA anchors run the same file rule, each from its own
// copy of the table. A row changed in one copy only would let the two loaders disagree with every test green.
func TestAnchorFileCasesMatchRunnerd(t *testing.T) {
	ours, err := os.ReadFile(filepath.Join("testdata", "anchor-file-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := os.ReadFile(runnerdAnchorFileCases)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ours, theirs) {
		t.Fatalf("testdata/anchor-file-cases.json differs from %s; change both copies together", runnerdAnchorFileCases)
	}
}
