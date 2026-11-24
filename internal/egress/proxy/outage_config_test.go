// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"strconv"
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
