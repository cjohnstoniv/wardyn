// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

type countingStore struct {
	*memStore
	reads int
}

func (c *countingStore) ListAuditEventsAfterSeq(ctx context.Context, seq int64, limit int) ([]types.FederatedAuditEvent, error) {
	c.reads++
	return c.memStore.ListAuditEventsAfterSeq(ctx, seq, limit)
}

func TestForwarder_HaltedAtCursorZeroReadsNoRows(t *testing.T) {
	st := &countingStore{memStore: &memStore{}}
	st.add(2, true)
	cred := Credential{DeviceID: uuid.New(), Token: "wdd_test"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/devices/"+cred.DeviceID.String()+"/audit" {
			http.Error(w, `{"error":"bad row"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(types.DeviceAck{})
	}))
	t.Cleanup(srv.Close)
	f := NewForwarder(NewClient(srv.URL), st, cred, &memRecorder{})
	ctx := context.Background()
	f.step(ctx)
	if !f.halted {
		t.Fatal("a 400 must halt")
	}
	before := st.reads
	for range 3 {
		f.step(ctx)
	}
	if st.reads != before {
		t.Fatalf("a halted forwarder at cursor 0 read the source %d times", st.reads-before)
	}
}
