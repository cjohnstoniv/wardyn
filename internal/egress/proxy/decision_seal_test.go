// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/google/uuid"
)

func TestRetiredSpoolRefusesProxyReopen(t *testing.T) {
	dir, id := filepath.Join(t.TempDir(), "spool"), uuid.New()
	sink, err := newLocalDecisionSink(dir, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	spool, err := runnerspool.Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	proof, err := spool.Receipt(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if sealed, err := spool.Seal(t.Context(), proof); err != nil || sealed {
		t.Fatal("live proxy producer sealed")
	}
	sink.close(t.Context())
	if sealed, err := spool.Seal(t.Context(), proof); err != nil || !sealed {
		t.Fatalf("closed producer seal: %v %v", sealed, err)
	}
	reopened, err := newLocalDecisionSink(dir, id, nil)
	if reopened != nil {
		reopened.close(t.Context())
		t.Fatal("sealed proxy producer reopened")
	}
	if !errors.Is(err, runnerspool.ErrSealed) {
		t.Fatalf("startup refusal: %v", err)
	}
}
