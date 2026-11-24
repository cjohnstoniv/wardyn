// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A Sandbox's Where survives the wire unchanged, and a Sandbox without one carries no Where key.
func TestSandboxWhere_RoundTrip(t *testing.T) {
	in := Sandbox{Ref: "p", Where: &SandboxWhere{Node: "n", RuntimeClass: "kata", RuntimeHandler: "kata-qemu"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Sandbox
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round trip = %+v, want %+v", out, in)
	}
}

func TestSandboxWhere_NilIsOmitted(t *testing.T) {
	b, err := json.Marshal(Sandbox{Ref: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Where") {
		t.Errorf("marshaled %s, want no Where key", b)
	}
}
