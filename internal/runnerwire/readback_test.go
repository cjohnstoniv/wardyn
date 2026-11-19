// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A create answer keeps the proxy config hash and address across the wire, and an older runner's
// empty replace_proxy answer decodes as no hash.
func TestReadback_WireFields(t *testing.T) {
	in := CreateSandboxResult{Sandbox: runner.Sandbox{Ref: "r", ProxyConfigSHA256: "abc", ProxyAddr: "10.0.0.2"}}
	b, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out CreateSandboxResult
	if err := Unmarshal(b, &out); err != nil || out.Sandbox.ProxyConfigSHA256 != "abc" || out.Sandbox.ProxyAddr != "10.0.0.2" {
		t.Fatalf("round trip %s -> %+v (%v)", b, out.Sandbox, err)
	}
	b, _ = Marshal(ReplaceProxyResult{ConfigSHA256: "def"})
	if string(b) != `{"config_sha256":"def"}` {
		t.Fatalf("replace_proxy answer %s", b)
	}
	var old ReplaceProxyResult
	if err := json.Unmarshal([]byte("null"), &old); err != nil || old.ConfigSHA256 != "" {
		t.Fatalf("an older runner's answer: %+v %v", old, err)
	}
}
