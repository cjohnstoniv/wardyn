// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestRelayStampPreservesViaAndOverridesPayloadRelay(t *testing.T) {
	id := uuid.New()
	ctx := WithRelay(t.Context(), id)
	for _, raw := range []string{``, `null`, ` null `, `{"relay":"forged","via":"upstream-proxy","observed_at":"then"}`, `[1]`, `"text"`} {
		got := StampRelay(ctx, json.RawMessage(raw))
		var data map[string]json.RawMessage
		if err := json.Unmarshal(got, &data); err != nil {
			t.Fatal(err)
		}
		var relay string
		if err := json.Unmarshal(data["relay"], &relay); err != nil || relay != "runner:"+id.String() {
			t.Fatalf("stamp=%s err=%v", got, err)
		}
		if raw == `[1]` || raw == `"text"` {
			if string(data["data"]) != raw {
				t.Fatalf("lost original %s: %s", raw, got)
			}
		}
		if raw == `{"relay":"forged","via":"upstream-proxy","observed_at":"then"}` && (string(data["via"]) != `"upstream-proxy"` || string(data["observed_at"]) != `"then"`) {
			t.Fatalf("lost provenance: %s", got)
		}
		if untouched := StampRelay(t.Context(), json.RawMessage(raw)); string(untouched) != raw {
			t.Fatalf("unrelayed data changed: %s", untouched)
		}
	}
}
