// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// modelTunnelRows returns the CONNECT allow rows the sink wrote for host, as raw JSON objects so the test
// reads the wire key and not a Go field.
func modelTunnelRows(t *testing.T, out string, host string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			continue
		}
		req, _ := m["request"].(map[string]any)
		if req["Host"] == host && m["decision"] == "allow" {
			rows = append(rows, m)
		}
	}
	return rows
}

// TestConnect_ModelHostMarkedWithInspectionOff: an opaque CONNECT to a model host carries model_tunnel on its
// own allow row whatever the content scanner is doing. The scanner is off here (no llm_inspection block, the
// default), and the host is a private Bedrock endpoint name; a non-model host is not marked.
func TestConnect_ModelHostMarkedWithInspectionOff(t *testing.T) {
	const bedrock = "vpce-0abc123-xyz.bedrock-runtime.us-east-1.vpce.amazonaws.com"
	const plain = "tls.test"
	p, buf := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{bedrock, plain}}, startEcho(t), nil, nil)
	if p.scanner != nil {
		t.Fatalf("scanner = %v; the test needs inspection off", p.scanner)
	}
	srv := httptest.NewServer(p)
	defer srv.Close()

	for _, host := range []string{bedrock, plain} {
		conn, status := connectThrough(t, srv.URL, host+":443")
		conn.Close()
		if !strings.Contains(status, "200") {
			t.Fatalf("CONNECT %s = %q, want 200", host, status)
		}
	}

	rows := modelTunnelRows(t, buf.String(), bedrock)
	if len(rows) != 1 {
		t.Fatalf("allow rows for %s = %d, want 1: %s", bedrock, len(rows), buf.String())
	}
	if rows[0]["model_tunnel"] != true {
		t.Errorf("model host row model_tunnel = %v, want true: %v", rows[0]["model_tunnel"], rows[0])
	}
	other := modelTunnelRows(t, buf.String(), plain)
	if len(other) != 1 {
		t.Fatalf("allow rows for %s = %d, want 1", plain, len(other))
	}
	if _, has := other[0]["model_tunnel"]; has {
		t.Errorf("non-model host row carries model_tunnel: %v", other[0])
	}
}
