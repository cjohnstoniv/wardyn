// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestLoadConfigBytesRefusesUnknownKeys pins F029: the sidecar image is pinned
// by the OPERATOR, independently of wardynd, so a config written by a NEWER
// control plane routinely meets an OLDER proxy binary. A key this binary cannot
// honour must fail startup loudly rather than be silently discarded — the
// difference between "the corp CA is not configured" (an operator reads it) and
// "the corp CA is configured and inert" (nobody ever finds out).
//
// The keys named here are the ones 0.7 added to the sidecar's Config; a 0.6.6
// binary accepted a config carrying all of them with err == nil and dropped
// every one.
func TestLoadConfigBytesRefusesUnknownKeys(t *testing.T) {
	base := map[string]any{
		"run_id":            uuid.New().String(),
		"control_plane_url": "http://wardynd:8080",
		"run_token":         "tok",
	}
	// Sanity: the same config WITHOUT the unknown key loads.
	b, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigBytes(b); err != nil {
		t.Fatalf("baseline config must load: %v", err)
	}

	for _, key := range []string{
		// A key from a hypothetical future release: the shape this fix exists for.
		"upstream_proxy_split_horizon",
		// A typo an operator can make by hand in WARDYN_PROXY_CONFIG_JSON.
		"internal_host",
	} {
		t.Run(key, func(t *testing.T) {
			cfg := map[string]any{}
			for k, v := range base {
				cfg[k] = v
			}
			cfg[key] = "something the operator meant to take effect"
			raw, merr := json.Marshal(cfg)
			if merr != nil {
				t.Fatal(merr)
			}
			c, lerr := LoadConfigBytes(raw)
			if lerr == nil {
				t.Fatalf("LoadConfigBytes accepted an unknown key %q (config = %+v): a routing key this "+
					"binary cannot honour must fail the sidecar closed, not be silently dropped", key, c)
			}
			if !strings.Contains(lerr.Error(), key) {
				t.Fatalf("error %q does not name the offending key %q — the operator has to be able to "+
					"see WHICH key their sidecar is too old for", lerr, key)
			}
		})
	}
}

// TestLoadConfigBytesAcceptsEveryShippedKey is the other half of the pin: the
// strict decoder must not reject a config this binary DOES understand, so the
// full 0.7 key set has to round-trip.
func TestLoadConfigBytesAcceptsEveryShippedKey(t *testing.T) {
	cfg := map[string]any{
		"run_id":                  uuid.New().String(),
		"control_plane_url":       "http://wardynd:8080",
		"run_token":               "tok",
		"upstream_proxy_url":      "http://corp-proxy.internal:3128",
		"upstream_proxy_no_proxy": []string{"10.0.0.0/8", ".internal"},
		"internal_hosts":          []map[string]any{{"host_suffix": "artifacts.internal"}},
		"mitm_hosts":              []string{"artifacts.internal"},
		"llm_upstreams":           map[string]string{"api.openai.com": "https://gw.internal/openai"},
		"decision_buffer_size":    16,
		"listen":                  ":3128",
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfigBytes(raw)
	if err != nil {
		t.Fatalf("a config using only shipped keys must load, got: %v", err)
	}
	if len(c.UpstreamProxyNoProxy) != 2 || len(c.InternalHosts) != 1 || len(c.LLMUpstreams) != 1 {
		t.Fatalf("shipped keys did not round-trip: %+v", c)
	}
}
