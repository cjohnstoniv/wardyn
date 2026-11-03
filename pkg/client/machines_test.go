// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestRunMachineWireNames(t *testing.T) {
	b, err := json.Marshal(client.CreateRunRequest{Agent: "a", Repo: "r"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["machine"]; ok {
		t.Fatalf("a request with no machine sends one: %s", b)
	}
	b, err = json.Marshal(client.RunMachine{Keep: true, Name: "n", Resume: "r", Fresh: true})
	if err != nil || string(b) != `{"keep":true,"name":"n","resume":"r","fresh":true}` {
		t.Fatalf("RunMachine JSON %s, %v", b, err)
	}
	b, err = json.Marshal(client.RunMachine{})
	if err != nil || string(b) != `{}` {
		t.Fatalf("zero RunMachine JSON %s, %v", b, err)
	}
}
