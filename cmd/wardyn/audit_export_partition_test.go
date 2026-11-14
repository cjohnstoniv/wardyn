// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"io/fs"
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

// An answer that is not a whole export, such as the empty 200 the server gives a caller below the
// security tier, is an error: no file is kept at -o, and nothing says it was written.
func TestAuditExportPartitionRefusesAnIncompleteExport(t *testing.T) {
	const manifest, row, footer = `{"type":"manifest"}` + "\n", `{"type":"row","seq":1}` + "\n", `{"type":"footer","digest":"abc"}` + "\n"
	for name, body := range map[string]string{
		"empty":                    "",
		"no footer":                manifest + row,
		"a row after the footer":   manifest + footer + row,
		"no manifest":              row + footer,
		"a footer with no newline": manifest + row + `{"type":"footer"}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(body))
		}))
		out := filepath.Join(t.TempDir(), "p.ndjson")
		var stderr strings.Builder
		root := rootCmd()
		root.SetArgs([]string{"audit", "export-partition", "audit_events_p202610", "-o", out, "--url", srv.URL, "--token", "tok"})
		root.SetOut(&strings.Builder{})
		root.SetErr(&stderr)
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "did not complete") {
			t.Errorf("%s: -o err = %v, want the export refused", name, err)
		}
		if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) || strings.Contains(stderr.String(), "wrote") {
			t.Errorf("%s: the file is there (%v) or stderr says written: %q", name, err, stderr.String())
		}
		if err := execCmd(t, "audit", "export-partition", "audit_events_p202610", "--url", srv.URL, "--token", "tok"); err == nil {
			t.Errorf("%s: stdout export succeeded, want it refused", name)
		}
		srv.Close()
	}
}
