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

func TestRunnerLabelCmd(t *testing.T) {
	id := uuid.New()
	srv := newCmdServer(t, http.StatusOK, types.RunnerView{ID: id, Name: "eu-1-runner",
		Labels:         map[string]string{"cluster": "eu-1", "region": "west"},
		AssertedLabels: map[string]string{"region": "west"}})
	out, err := execCmdCapture(t, "runner", "label", id.String(), "cluster=eu-1", "--accept-proposed", "--url", srv.URL, "--token", "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.reqs) != 2 || srv.reqs[0].method != http.MethodGet || srv.reqs[0].path != "/api/v1/runners/"+id.String() {
		t.Fatalf("requests = %+v, want the runner read, then the write", srv.reqs)
	}
	var sent types.RunnerLabelsRequest
	req := srv.last()
	if req.method != http.MethodPut || req.path != "/api/v1/runners/"+id.String()+"/labels" || json.Unmarshal(req.body, &sent) != nil ||
		len(sent.Labels) != 2 || sent.Labels["cluster"] != "eu-1" || sent.Labels["region"] != "west" {
		t.Fatalf("write = %+v %+v", req, sent)
	}
	if want := "Runner eu-1-runner (" + id.String() + ") labels: cluster=eu-1,region=west"; !strings.Contains(out, want) {
		t.Errorf("printed %q, want %q", out, want)
	}

	// No arguments clears the set: an empty object, not null.
	clear := newCmdServer(t, http.StatusOK, types.RunnerView{ID: id, Name: "r"})
	out, err = execCmdCapture(t, "runner", "label", id.String(), "--url", clear.URL, "--token", "t")
	if req := clear.last(); err != nil || !strings.Contains(string(req.body), `"labels":{}`) || !strings.Contains(out, "labels: none") {
		t.Fatalf("clear: %v %s\n%s", err, req.body, out)
	}

	n := len(clear.reqs)
	for _, bad := range [][]string{{"label", id.String(), "cluster"}, {"label", "not-a-uuid", "a=b"}} {
		if err := execCmd(t, append(append([]string{"runner"}, bad...), "--url", clear.URL, "--token", "t")...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if len(clear.reqs) != n {
		t.Errorf("a bad argument sent %d requests", len(clear.reqs)-n)
	}
}
