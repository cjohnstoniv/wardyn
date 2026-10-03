// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
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
