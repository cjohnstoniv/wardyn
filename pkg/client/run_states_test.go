// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"os"
	"regexp"
	"testing"
)

// Every RunState the server can answer has an SDK constant, so a caller switching on state is never
// left without a name for QUEUED or whatever comes next. Both sides are read from source.
func TestRunStateConstantsCoverEveryServerState(t *testing.T) {
	server, err := os.ReadFile("../../internal/types/types.go")
	if err != nil {
		t.Fatal(err)
	}
	sdk, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatal(err)
	}
	names := regexp.MustCompile(`\b(Run\w+)\s+RunState\s*=\s*"[A-Z_]+"`).FindAllStringSubmatch(string(server), -1)
	if len(names) < 10 {
		t.Fatalf("found %d RunState constants in internal/types; the scan is stale", len(names))
	}
	for _, m := range names {
		if !regexp.MustCompile(`\b` + m[1] + `\s+= types\.` + m[1] + `\b`).Match(sdk) {
			t.Errorf("client has no constant %s = types.%s", m[1], m[1])
		}
	}
}
