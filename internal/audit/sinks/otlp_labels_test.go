// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func labelAttrsOf(t *testing.T, data string) []string {
	t.Helper()
	rec := logRecord(types.AuditEvent{Action: "run.create", Outcome: "success", Time: time.Unix(1, 0), Data: json.RawMessage(data)}, false)
	var out []string
	for _, a := range rec.Attributes {
		if strings.HasPrefix(a.Key, "wardyn.runner_label.") {
			out = append(out, a.Key+"="+a.Value.StringValue)
		}
	}
	return out
}

// A row's runner_labels are exported one attribute each, sorted by key.
func TestOTLP_RunnerLabelAttributes(t *testing.T) {
	t.Parallel()
	got := labelAttrsOf(t, `{"runner_labels":{"region":"west","cluster":"eu-1"}}`)
	want := []string{"wardyn.runner_label.cluster=eu-1", "wardyn.runner_label.region=west"}
	if !slices.Equal(got, want) {
		t.Errorf("attributes = %v, want %v", got, want)
	}
	if got := labelAttrsOf(t, `{"other":"x"}`); len(got) != 0 {
		t.Errorf("a row without runner_labels exported %v", got)
	}

	many := map[string]string{}
	for i := range types.MaxRunnerLabels + 1 {
		many[fmt.Sprintf("k%d", i)] = "v"
	}
	enc, err := json.Marshal(map[string]any{"runner_labels": many})
	if err != nil {
		t.Fatal(err)
	}
	if got := labelAttrsOf(t, string(enc)); len(got) != 0 {
		t.Errorf("a row with %d labels exported %v", len(many), got)
	}
	for _, bad := range []string{`{"runner_labels":{"cluster":"https://x"}}`, `{"runner_labels":{"a b":"v"}}`, `{"runner_labels":"x"}`} {
		if got := labelAttrsOf(t, bad); len(got) != 0 {
			t.Errorf("%s exported %v", bad, got)
		}
	}
}
