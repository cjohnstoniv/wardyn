// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestDefaultConfigDecodesOnPreviousWire: the config a default run's proxy is handed, at dispatch and
// again at a revive, loads on the last release's strict decoder. A run's policy generation rides its
// run token, never a config key, and every other key added since that release is omitempty and set
// only by the feature that uses it, so the default config carries none of them. The previous wire
// struct is Config without every top-level key the release's key set lacks.
func TestDefaultConfigDecodesOnPreviousWire(t *testing.T) {
	known, err := os.ReadFile(configKeysDir + "v0.9.3.txt")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for k := range strings.FieldsSeq(string(known)) {
		keys[k] = true
	}
	var fields []reflect.StructField
	dropped := 0
	for f := range reflect.TypeFor[Config]().Fields() {
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); keys[name] {
			fields = append(fields, f)
		} else {
			dropped++
		}
	}
	if dropped == 0 {
		t.Fatal("every Config key is in the previous release's set; the previous wire struct proves nothing")
	}
	cfg := Config{
		RunID:             uuid.New(),
		ControlPlaneURL:   "https://wardynd:8443",
		ControlPlaneCAPEM: testCPCAPEM,
		RunToken:          "tok",
		Policy:            types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfigBytes(raw); err != nil {
		t.Fatalf("this tree's proxy refuses the default config: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(reflect.New(reflect.StructOf(fields)).Interface()); err != nil {
		t.Fatalf("the previous release's proxy refuses the default config: %v\n%s", err, raw)
	}
	for _, k := range []string{"policy_generation", "policy_digest", "disable_model_usage"} {
		if bytes.Contains(raw, []byte(`"`+k+`"`)) {
			t.Errorf("the default config carries %q", k)
		}
	}
}
