// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// A custom resource's launch carries its namespace binding into the authority its queued run is stored
// with, through the stored JSON, so the start can hold to it; any other launch carries none.
func TestQueueAuthority_CarriesTheLaunchsBinding(t *testing.T) {
	b := uuid.New()
	raw, err := json.Marshal(captureQueueAuthority(withLaunchBinding(context.Background(), b)))
	if err != nil {
		t.Fatal(err)
	}
	var back queueAuthority
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Binding == nil || *back.Binding != b {
		t.Fatalf("stored authority's binding = %v, want %s", back.Binding, b)
	}
	if a := captureQueueAuthority(context.Background()); a.Binding != nil {
		t.Fatalf("a launch under no binding stored binding %s", *a.Binding)
	}
}
