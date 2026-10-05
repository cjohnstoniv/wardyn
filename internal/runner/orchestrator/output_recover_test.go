// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recoveringSubstrate is a fakeSubstrate that can read a run's output back.
type recoveringSubstrate struct{ *fakeSubstrate }

func (recoveringSubstrate) RecoverOutput(_ context.Context, ref string, w io.Writer) error {
	_, err := io.WriteString(w, "output of "+ref)
	return err
}

// RecoverOutput goes to the substrate that owns ref; one that cannot read
// output back has nothing to give.
func TestOrchestrator_RecoverOutput(t *testing.T) {
	plain := New(&fakeSubstrate{name: "plain", classes: []types.ConfinementClass{types.CC1}})
	if err := plain.RecoverOutput(context.Background(), "ref-1", io.Discard); !errors.Is(err, runner.ErrOutputUnrecoverable) {
		t.Fatalf("a substrate without the capability = %v, want ErrOutputUnrecoverable", err)
	}
	var out strings.Builder
	rec := New(recoveringSubstrate{&fakeSubstrate{name: "rec", classes: []types.ConfinementClass{types.CC1}}})
	if err := rec.RecoverOutput(context.Background(), "ref-1", &out); err != nil || out.String() != "output of ref-1" {
		t.Fatalf("RecoverOutput = %q, %v", out.String(), err)
	}
}
