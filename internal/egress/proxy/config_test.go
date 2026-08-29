// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func baseConfigJSON(t *testing.T, extra map[string]any) []byte {
	t.Helper()
	m := map[string]any{
		"run_id":            uuid.New().String(),
		"control_plane_url": "http://wardynd:8080",
		"run_token":         "tok",
	}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestProxyConfig_InternalHosts_RoundTrip: a declared internal host round-trips
// through LoadConfigBytes byte-for-byte, and an empty config carries none —
// the negative control that byte-identical-when-unset holds.
func TestProxyConfig_InternalHosts_RoundTrip(t *testing.T) {
	cfg, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{
		"internal_hosts": []types.InternalHost{
			{HostSuffix: "corp.internal", CIDRs: []string{"10.40.0.0/16"}},
		},
	}))
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	if len(cfg.InternalHosts) != 1 || cfg.InternalHosts[0].HostSuffix != "corp.internal" {
		t.Fatalf("InternalHosts did not round-trip: %+v", cfg.InternalHosts)
	}

	cfg, err = LoadConfigBytes(baseConfigJSON(t, nil))
	if err != nil {
		t.Fatalf("LoadConfigBytes (empty): %v", err)
	}
	if len(cfg.InternalHosts) != 0 {
		t.Fatalf("unset internal_hosts must round-trip empty, got %+v", cfg.InternalHosts)
	}
}

// TestProxyConfig_InternalHosts_RejectsNonLiftableCIDR: fail-closed parse-check
// mirrors validateInternalHosts (internal/api) — a config authored outside
// that write path (e.g. a hand-edited file) cannot smuggle a wider CIDR.
func TestProxyConfig_InternalHosts_RejectsNonLiftableCIDR(t *testing.T) {
	_, err := LoadConfigBytes(baseConfigJSON(t, map[string]any{
		"internal_hosts": []types.InternalHost{
			{HostSuffix: "corp.internal", CIDRs: []string{"169.254.0.0/16"}},
		},
	}))
	if err == nil || !strings.Contains(err.Error(), "must lie inside") {
		t.Fatalf("non-liftable CIDR must be rejected at config load, got err=%v", err)
	}
}
