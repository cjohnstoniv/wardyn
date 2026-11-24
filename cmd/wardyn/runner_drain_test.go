// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestRunnerDrainCmd(t *testing.T) {
	id := uuid.New()
	view := types.RunnerView{ID: id, Name: "node-1", Usage: types.RunnerUsage{InUse: types.RunnerTally{Runs: 2}}}
	srv := newCmdServer(t, http.StatusOK, view)
	out, err := execCmdCapture(t, "runner", "drain", id.String(), "--reason", "node pool upgrade", "--url", srv.URL, "--token", "t")
	var body types.RunnerDrainRequest
	if req := srv.last(); err != nil || req.method != http.MethodPut || req.path != "/api/v1/runners/"+id.String()+"/drain" || json.Unmarshal(req.body, &body) != nil ||
		body.Reason != "node pool upgrade" || !strings.Contains(out, "Runner node-1 ("+id.String()+") is drained: it takes no new sandbox. 2 running.") {
		t.Fatalf("drain: %v %+v %+v\n%s", err, req, body, out)
	}
	out, err = execCmdCapture(t, "runner", "undrain", id.String(), "--url", srv.URL, "--token", "t")
	if req := srv.last(); err != nil || req.method != http.MethodDelete || req.path != "/api/v1/runners/"+id.String()+"/drain" ||
		!strings.Contains(out, "Runner node-1 ("+id.String()+") takes runs again.") {
		t.Fatalf("undrain: %v %+v\n%s", err, req, out)
	}
	n := len(srv.reqs)
	for _, bad := range [][]string{
		{"drain", id.String()},
		{"drain", "not-a-uuid", "--reason", "r"},
		{"undrain", "not-a-uuid"},
	} {
		if err := execCmd(t, append(append([]string{"runner"}, bad...), "--url", srv.URL, "--token", "t")...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if len(srv.reqs) != n {
		t.Error("an invalid drain command reached the server")
	}
}
