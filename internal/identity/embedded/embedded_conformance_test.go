// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package embedded_test

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
	"github.com/cjohnstoniv/wardyn/internal/identity/identitytest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type noopRecorder struct{}

func (noopRecorder) Record(context.Context, types.AuditEvent) error { return nil }

// The blessed default (embedded) must pass the shared identity conformance suite.
func TestEmbedded_Conformance(t *testing.T) {
	identitytest.RunConformance(t, func(t *testing.T) identity.Provider {
		p, err := embedded.New(nil, "wardyn.local", embedded.NewMemRevocationStore(), noopRecorder{})
		if err != nil {
			t.Fatalf("embedded.New: %v", err)
		}
		return p
	})
}
