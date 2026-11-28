// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

type saltStore struct {
	*memStore
	rows []types.FederatedAuditRow
}

func (s saltStore) ListFederatedAuditRowsAfterSeq(_ context.Context, seq int64, limit int) ([]types.FederatedAuditRow, error) {
	var out []types.FederatedAuditRow
	for _, r := range s.rows {
		if r.Seq > seq && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

// TestForwarder_PushesFormatAndSalt: a format 2 row's push body carries hash_format 2 and its salt, so the
// organisation can recompute the claim; a format 1 row's carries neither field.
func TestForwarder_PushesFormatAndSalt(t *testing.T) {
	salt := strings.Repeat("ab", 32)
	mk := func(seq int64, format int, salt string) types.FederatedAuditRow {
		r := types.FederatedAuditRow{HashFormat: format, HashSalt: salt}
		r.Seq, r.ID, r.Action, r.Outcome, r.ActorType = seq, uuid.New(), "run.create", "success", types.ActorSystem
		r.RowHash = "h" + string(rune('0'+seq))
		return r
	}
	st := saltStore{memStore: &memStore{}, rows: []types.FederatedAuditRow{mk(1, 0, ""), mk(2, 2, salt)}}
	cred := Credential{DeviceID: uuid.New(), Token: "wdd_test"}
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/audit") {
			body, _ = io.ReadAll(r.Body)
		}
		_ = json.NewEncoder(w).Encode(types.DeviceAck{AckedSeq: 2})
	}))
	defer srv.Close()
	f := NewForwarder(NewClient(srv.URL), st, cred, &memRecorder{})
	f.step(context.Background())

	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) != 2 {
		t.Fatalf("push body %q: %v", body, err)
	}
	if _, ok := raw[0]["hash_format"]; ok {
		t.Errorf("the format 1 row's push carries hash_format")
	}
	if _, ok := raw[0]["hash_salt"]; ok {
		t.Errorf("the format 1 row's push carries hash_salt")
	}
	if string(raw[1]["hash_format"]) != "2" || string(raw[1]["hash_salt"]) != `"`+salt+`"` {
		t.Errorf("the format 2 row's push = %s, want hash_format 2 and its salt", body)
	}
}
