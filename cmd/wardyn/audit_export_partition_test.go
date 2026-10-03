// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `wardyn audit export-partition` asks for the partition by name, the raw archive form on --raw, and
// writes the file whole or not at all.
func TestAuditExportPartition(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/audit/export" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"manifest"}` + "\n" + `{"type":"footer","digest":"abc"}` + "\n"))
	}))
	t.Cleanup(srv.Close)

	out := filepath.Join(t.TempDir(), "p.ndjson")
	if err := execCmd(t, "audit", "export-partition", "audit_events_p202610", "--raw", "-o", out, "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("export-partition: %v", err)
	}
	if gotQuery != "form=raw&partition=audit_events_p202610" {
		t.Errorf("query = %q, want the partition by name and form=raw", gotQuery)
	}
	if b, err := os.ReadFile(out); err != nil || !strings.Contains(string(b), `"footer"`) {
		t.Errorf("output file = %q, %v; want the whole export", b, err)
	}
	if err := execCmd(t, "audit", "export-partition", "audit_events_p202610", "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatalf("export-partition to stdout: %v", err)
	}
	if gotQuery != "partition=audit_events_p202610" {
		t.Errorf("query = %q, want no form for the readable default", gotQuery)
	}
}
