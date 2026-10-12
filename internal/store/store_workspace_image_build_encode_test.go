// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A record that cannot be encoded must fail the write before the swap runs. The PG
// here has no pool: reaching the statement would panic, so a pass proves it never got there.
func TestSetWorkspaceBuiltImage_UnencodableRecordFailsBeforeTheSwap(t *testing.T) {
	rec := &types.ImageBuild{Inputs: types.ImageBuildInputs{
		Features: []types.ImageBuildFeature{{Ref: "f", Options: map[string]any{"v": math.NaN()}}},
	}}
	applied, err := store.PG{}.SetWorkspaceBuiltImage(context.Background(), uuid.New(), "", "img", "key", rec)
	if err == nil || applied {
		t.Fatalf("SetWorkspaceBuiltImage = (%v, %v), want an error and no swap", applied, err)
	}
}
