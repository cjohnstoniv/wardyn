// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestUnitModelUsageWire(t *testing.T) {
	unit := uuid.New()
	since := time.Now().UTC().Truncate(time.Second).Add(-72 * time.Hour)
	until, updated := since.Add(48*time.Hour), since.Add(time.Hour)
	var gotPath, gotSince, gotUntil string
	var sentSince, sentUntil bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		q := r.URL.Query()
		gotSince, gotUntil = q.Get("since"), q.Get("until")
		_, sentSince = q["since"]
		_, sentUntil = q["until"]
		writeJSON(w, http.StatusOK, map[string]any{
			"since": since, "until": until,
			"units": []any{map[string]any{
				"unit_id": unit, "unit_name": "platform",
				"totals": map[string]any{"input_tokens": 30, "output_tokens": 15, "cache_read_tokens": 3, "cache_write_tokens": 2,
					"reasoning_tokens": 4, "calls": 3, "uncounted_calls": 1, "partial_calls": 1},
				"by_model": []any{map[string]any{
					"provider": "corp-gw", "vendor": "anthropic", "host": "llm.corp.example", "model": "claude-x",
					"uncounted_reasons": map[string]int{"stream_without_usage": 1},
					"input_tokens":      30, "output_tokens": 15, "cache_read_tokens": 3, "cache_write_tokens": 2,
					"reasoning_tokens": 4, "calls": 3, "uncounted_calls": 1, "partial_calls": 1,
				}},
				"opaque_tunnels": 2, "updated_at": updated,
			}},
		})
	}))
	defer srv.Close()
	c := newTestClient(srv)

	got, err := c.UnitModelUsage(context.Background(), unit, since, until)
	if err != nil {
		t.Fatalf("UnitModelUsage: %v", err)
	}
	if gotPath != "GET /api/v1/governance/units/"+unit.String()+"/model-usage" || gotSince != since.Format(time.RFC3339) || gotUntil != until.Format(time.RFC3339) {
		t.Errorf("request = %s since=%q until=%q", gotPath, gotSince, gotUntil)
	}
	if !got.Since.Equal(since) || !got.Until.Equal(until) || len(got.Units) != 1 {
		t.Fatalf("response = %+v", got)
	}
	u := got.Units[0]
	if u.UnitID != unit || u.UnitName != "platform" || u.Totals.InputTokens != 30 || u.Totals.ReasoningTokens != 4 || u.Totals.PartialCalls != 1 ||
		u.OpaqueTunnels != 2 || u.UpdatedAt == nil || len(u.ByModel) != 1 {
		t.Fatalf("unit = %+v", u)
	}
	if m := u.ByModel[0]; m.Provider != "corp-gw" || m.Host != "llm.corp.example" || m.CacheWriteTokens != 2 || m.UncountedReasons["stream_without_usage"] != 1 {
		t.Errorf("by_model = %+v", m)
	}

	if _, err := c.UnitModelUsage(context.Background(), unit, time.Time{}, time.Time{}); err != nil {
		t.Fatalf("UnitModelUsage with no window: %v", err)
	}
	if sentSince || sentUntil {
		t.Errorf("zero times were sent: since=%v until=%v, want no query parameter", sentSince, sentUntil)
	}
}

// The SDK's usage types and the server's declare one body twice: their json tags must agree.
func TestUnitModelUsageWireTags(t *testing.T) {
	for _, tc := range []struct {
		name, file, server string
		clientDT           any
	}{
		{"UnitModelUsage", "internal/api/governance_unit_usage.go", "UnitModelUsageResponse", client.UnitModelUsage{}},
		{"ModelUsageTokens", "internal/api/run_model_usage.go", "ModelUsageTokens", client.ModelUsageTokens{}},
		{"ModelUsage", "internal/api/run_model_usage.go", "ModelUsageView", client.ModelUsage{}},
	} {
		want, got := serverJSONTags(t, tc.file, tc.server), clientJSONTags(t, tc.clientDT)
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(want, got) {
			t.Errorf("client.%s json tags %v, server %s %v", tc.name, got, tc.server, want)
		}
	}
}
