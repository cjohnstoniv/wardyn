// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestExportAuditPartition(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("partition") == "audit_events_p202610" {
			_, _ = w.Write([]byte("{\"type\":\"manifest\"}\n"))
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"open","reason":"audit_partition_open"}`))
	}))
	t.Cleanup(srv.Close)
	c := &client.Client{BaseURL: srv.URL, Token: "t"}

	rc, err := c.ExportAuditPartition(context.Background(), "audit_events_p202610", true)
	if err != nil {
		t.Fatalf("ExportAuditPartition: %v", err)
	}
	if b, _ := io.ReadAll(rc); string(b) != "{\"type\":\"manifest\"}\n" {
		t.Errorf("body = %q", b)
	}
	rc.Close()

	_, err = c.ExportAuditPartition(context.Background(), "audit_events_p202611", false)
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Reason != "audit_partition_open" {
		t.Errorf("error = %v, want a 409 audit_partition_open APIError", err)
	}
}

func TestAuditRetentionAndDrop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/audit/retention":
			_, _ = w.Write([]byte(`{"policy":{"days":0,"effective_days":0,"pending_days":90,"pending_effective_at":"2026-11-01T00:00:00Z"},` +
				`"cutover":"2026-10-01T00:00:00Z","partitions":[{"name":"audit_events_legacy","rows":5,"state":"closed","eligible":false,"refusal":"audit_retention_inside_window"}],"months_ahead":12}`))
		case "POST /api/v1/audit/retention/drop":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["digest"] != "abc" {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"no","reason":"audit_retention_digest_mismatch"}`))
				return
			}
			_, _ = w.Write([]byte(`{"partition":"` + body["partition"] + `","rows":5,"digest":"abc","event_seq":9}`))
		}
	}))
	t.Cleanup(srv.Close)
	c := &client.Client{BaseURL: srv.URL, Token: "t"}

	st, err := c.AuditRetention(context.Background())
	if err != nil || st.MonthsAhead != 12 || st.Policy.PendingDays == nil || *st.Policy.PendingDays != 90 ||
		len(st.Partitions) != 1 || st.Partitions[0].Refusal != "audit_retention_inside_window" {
		t.Fatalf("AuditRetention = %+v, %v", st, err)
	}
	d, err := c.DropAuditPartition(context.Background(), "audit_events_legacy", "abc")
	if err != nil || d.Partition != "audit_events_legacy" || d.EventSeq != 9 {
		t.Fatalf("DropAuditPartition = %+v, %v", d, err)
	}
	_, err = c.DropAuditPartition(context.Background(), "audit_events_legacy", "bad")
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Reason != "audit_retention_digest_mismatch" {
		t.Errorf("error = %v, want a 409 audit_retention_digest_mismatch APIError", err)
	}
}
