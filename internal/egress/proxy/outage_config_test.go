// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestLoadConfig_OutageBounds: the outage key loads with an allowance of 30 minutes to 24 hours and refuses one outside.
func TestLoadConfig_OutageBounds(t *testing.T) {
	for _, tc := range []struct {
		allowance int64
		ok        bool
	}{{1799, false}, {1800, true}, {86400, true}, {86401, false}} {
		cfg, err := LoadConfigBytes([]byte(`{"run_id":"` + uuid.NewString() + `","control_plane_url":"http://127.0.0.1:8443","run_token":"RUNTOK",` +
			`"policy":{"allowed_domains":[]},"outage":{"allowance_s":` + strconv.FormatInt(tc.allowance, 10) + `,"strict":true}}`))
		if (err == nil) != tc.ok {
			t.Fatalf("allowance_s %d: err %v, want ok %v", tc.allowance, err, tc.ok)
		}
		if tc.ok && (cfg.Outage == nil || cfg.Outage.AllowanceS != tc.allowance || !cfg.Outage.Strict) {
			t.Fatalf("allowance_s %d decoded as %+v", tc.allowance, cfg.Outage)
		}
	}
	cfg, err := LoadConfigBytes([]byte(`{"run_id":"` + uuid.NewString() + `","control_plane_url":"http://127.0.0.1:8443","run_token":"RUNTOK","policy":{"allowed_domains":[]}}`))
	if err != nil || cfg.Outage != nil {
		t.Fatalf("a config without the key = %+v %v, want no outage terms", cfg, err)
	}
}

// TestLoadConfig_OutageBoundsBesideALegacyADOGrant: the bound is checked first in LoadConfigBytes' switch, so a
// one-entry ado_grants list, whose case assigns and does not return, never skips it.
func TestLoadConfig_OutageBoundsBesideALegacyADOGrant(t *testing.T) {
	certPEM, keyPEM := genTestCA(t)
	load := func(allowance int64) (*Config, error) {
		return LoadConfigBytes(baseConfigJSON(t, map[string]any{
			"mitm_ca_cert_pem": string(certPEM), "mitm_ca_key_pem": string(keyPEM),
			"ado_grants": []any{map[string]any{"organization": "acme", "capabilities": []string{"code_read"}, "hosts": []string{"dev.azure.com"}}},
			"outage":     map[string]any{"allowance_s": allowance},
		}))
	}
	if _, err := load(1799); err == nil || !strings.Contains(err.Error(), "outage.allowance_s") {
		t.Fatalf("one legacy grant and allowance_s 1799: err = %v, want the outage bound's refusal", err)
	}
	cfg, err := load(1800)
	if err != nil || cfg.Outage == nil || cfg.Outage.AllowanceS != 1800 || cfg.ADOGrant == nil {
		t.Fatalf("one legacy grant and allowance_s 1800 = %+v %v, want both kept", cfg, err)
	}
}
