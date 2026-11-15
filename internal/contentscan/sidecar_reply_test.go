// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A custom detector's 200 is a reply only when it is one JSON object with a findings field.
// Anything else is a sidecar error, which on_scanner_error decides on, never a clean scan.
// {"findings":null} names the field (a Go sidecar's empty slice encodes to it) and stays clean.
func TestSidecarReplyMustBeOneObjectWithFindings(t *testing.T) {
	for _, reply := range []struct {
		name, body string
		clean      bool
	}{
		{"null", `null`, false},
		{"empty object", `{}`, false},
		{"error object", `{"error":"model not loaded"}`, false},
		{"trailing document", `{"findings":[]} {"findings":[]}`, false},
		{"findings null", `{"findings":null}`, true},
		{"findings empty", "{\"findings\":[]}\n", true},
		{"capitalised field, empty", `{"Findings":[]}`, true},
	} {
		for _, cfg := range []struct {
			mode, onErr string
			wantBlock   bool
		}{{"block", "", false}, {"block", "block", true}, {"alert", "block", false}} {
			t.Run(reply.name+"/"+cfg.mode+"/on_scanner_error="+cfg.onErr, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.WriteString(w, reply.body)
				}))
				t.Cleanup(srv.Close)
				eng, err := NewEngine(types.LLMInspectionSpec{Mode: cfg.mode, OnScannerError: cfg.onErr, DetectorSidecarURL: srv.URL}, nil)
				if err != nil || eng == nil {
					t.Fatalf("NewEngine: %v", err)
				}
				res, _, err := eng.ScanRequest(ChannelGeneric, []byte(`{"q":"some prompt text"}`))
				if err != nil {
					t.Fatalf("scan: %v", err)
				}
				if reply.clean {
					if res.Skipped || eng.ShouldBlock(res) {
						t.Fatalf("reply %s: %+v, block %v; want a clean scan", reply.body, res, eng.ShouldBlock(res))
					}
					return
				}
				if res.SkipReason != "sidecar_error" || eng.ShouldBlock(res) != cfg.wantBlock {
					t.Fatalf("reply %s: skip %q, block %v; want sidecar_error, block %v", reply.body, res.SkipReason, eng.ShouldBlock(res), cfg.wantBlock)
				}
			})
		}
	}
	// The field is matched in any letter case, as the decoder always did: a detector writing "Findings" still blocks.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Findings":[{"Type":"x","Severity":"high"}]}`)
	}))
	t.Cleanup(srv.Close)
	eng, err := NewEngine(types.LLMInspectionSpec{Mode: "block", DetectorSidecarURL: srv.URL}, nil)
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if res, _, err := eng.ScanRequest(ChannelGeneric, []byte(`{"q":"some prompt text"}`)); err != nil || len(res.Findings) == 0 || !eng.ShouldBlock(res) {
		t.Fatalf("reply with a capitalised Findings field: %+v, err %v; want its finding to block", res, err)
	}
}
