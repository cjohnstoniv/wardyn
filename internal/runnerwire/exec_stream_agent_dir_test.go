// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// An exec's folder (runner.ExecSpec.AgentDir) rides exec_stream as one more member, left out when empty, so every
// other exec keeps its wire form and a runner that predates the member reads the call as before.
func TestExecStreamArgs_AgentDirIsOneOptionalMember(t *testing.T) {
	for _, tc := range []struct {
		spec   runner.ExecSpec
		golden string
	}{
		{runner.ExecSpec{Argv: []string{"cat"}},
			`{"ref":"sbx","spec":{"Argv":["cat"],"Env":null,"TTY":false,"Cols":0,"Rows":0}}`},
		{runner.ExecSpec{Argv: []string{"cat"}, AgentDir: "/home/agent/work"},
			`{"ref":"sbx","spec":{"Argv":["cat"],"Env":null,"TTY":false,"Cols":0,"Rows":0,"AgentDir":"/home/agent/work"}}`},
	} {
		b, err := Marshal(ExecStreamArgs{Ref: "sbx", Spec: tc.spec})
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.golden {
			t.Fatalf("wire form %s, want %s", b, tc.golden)
		}
		var back ExecStreamArgs
		if err := Unmarshal(b, &back); err != nil || back.Spec.AgentDir != tc.spec.AgentDir {
			t.Fatalf("round trip = %+v, %v; want AgentDir %q", back, err, tc.spec.AgentDir)
		}
		// A runner without the member decodes the same bytes without an error.
		var older struct {
			Ref  string `json:"ref"`
			Spec struct {
				Argv, Env  []string
				TTY        bool
				Cols, Rows uint16
			} `json:"spec"`
		}
		if err := Unmarshal(b, &older); err != nil || older.Ref != "sbx" || len(older.Spec.Argv) != 1 {
			t.Fatalf("an older runner's decode = %+v, %v", older, err)
		}
	}
}
