// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A promoted run whose stored authority cannot be read is ended, not started as the zero authority,
// which the owner checks read as the admin token.
func TestNewQueuedStart_UnreadableAuthorityEndsTheRun(t *testing.T) {
	run := types.AgentRun{ID: uuid.New(), Agent: "claude-code"}
	for _, auth := range []string{"", "not json"} {
		_, err := newQueuedStart(context.Background(), context.Background(), store.PromotedRun{Run: run,
			Entry: store.RunQueueEntry{RunID: run.ID, Authority: []byte(auth), Launch: []byte(`{"request":{}}`)}})
		if err == nil {
			t.Errorf("authority %q: want an error that ends the run", auth)
		}
	}
}
