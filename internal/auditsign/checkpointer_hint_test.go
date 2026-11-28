// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditsign

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestCheckpointLoop_BreakNamesTheLiveChainCheck: the error a signing break logs names the chain check an
// operator can run, the security-tier route and its ok field, and no CLI command that does not exist.
func TestCheckpointLoop_BreakNamesTheLiveChainCheck(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := context.Background()
	st := &fakeStore{}
	st.appendRows(2)
	s, _ := newTestSigner(t, st)
	s.Tick(ctx)
	st.appendRows(3)
	st.rows[len(st.rows)-2].prev = fakeHash(999)
	s.Tick(ctx)

	out := buf.String()
	if !strings.Contains(out, "signing stopped") {
		t.Fatalf("no signing-break line was logged: %q", out)
	}
	if strings.Contains(out, "wardyn audit chain verify") || !strings.Contains(out, "GET /api/v1/audit/chain/verify") || !strings.Contains(out, "ok field") {
		t.Errorf("the break's remedy does not name the live chain check and its ok field: %q", out)
	}
}
