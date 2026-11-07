// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// Counting is on unless the admin says off; a value that says neither refuses to start.
func TestApplyModelUsageSwitch(t *testing.T) {
	for _, tc := range []struct {
		v        string
		wantCode int
		wantOff  bool
	}{
		{"", 0, false}, {"on", 0, false}, {"off", 0, true}, {"OFF", 2, false}, {"false", 2, false}, {"of", 2, false},
	} {
		cfg := &proxy.Config{RunID: uuid.New()}
		code := applyModelUsageSwitch(cfg, tc.v)
		if code != tc.wantCode || cfg.DisableModelUsage != tc.wantOff {
			t.Errorf("WARDYN_MODEL_USAGE=%q: code %d, off %v; want %d, %v", tc.v, code, cfg.DisableModelUsage, tc.wantCode, tc.wantOff)
		}
	}
}
