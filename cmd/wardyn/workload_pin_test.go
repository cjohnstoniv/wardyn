// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestWorkloadPinAndUnpin: pin sends the digest to the workload's pin route and says what it pinned, unpin
// clears it, and a workload the server does not find is an error.
func TestWorkloadPinAndUnpin(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	var method, path, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, body = r.Method, r.URL.Path, string(b)
		if r.URL.Path != "/api/v1/workloads/web/policy-pin" {
			http.NotFound(w, r)
			return
		}
		out := sdk.Workload{Name: "web", Owner: "person:me", Source: sdk.WorkloadSourceAPI}
		if r.Method == http.MethodPut {
			out.PolicyPin = digest
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	rig := &routeRig{Server: srv}

	out, err := rig.run(t, "workload", "pin", "web", digest)
	if err != nil || method != http.MethodPut || path != "/api/v1/workloads/web/policy-pin" || body != `{"digest":"`+digest+`"}` ||
		!strings.Contains(out, "workload/web pinned to launch policy digest "+digest) {
		t.Errorf("pin: err = %v, sent %s %s %s, output:\n%s", err, method, path, body, out)
	}
	out, err = rig.run(t, "workload", "unpin", "web")
	if err != nil || method != http.MethodDelete || path != "/api/v1/workloads/web/policy-pin" || !strings.Contains(out, "workload/web unpinned") {
		t.Errorf("unpin: err = %v, sent %s %s, output:\n%s", err, method, path, out)
	}
	if _, err := rig.run(t, "workload", "pin", "other", digest); err == nil {
		t.Error("pin of a workload the server does not find: no error")
	}
}

// TestApplyDryRunPrintsTheLaunchPolicyDigest: a dry run names the launch policy digest the launch would
// resolve to, the digest `workload pin` takes, and prints no digest line when the preview carries none.
func TestApplyDryRunPrintsTheLaunchPolicyDigest(t *testing.T) {
	file := writeTemplateFile(t, "w.yaml", webYAML)
	pf := &sdk.PreflightResult{EnforcedConfinementClass: types.CC2, SetupItems: []sdk.PreflightItem{}}
	digest := strings.Repeat("ab", 32)
	for _, launch := range []sdk.WorkloadPreviewLaunch{
		{State: sdk.WorkloadPreviewWouldLaunch, Preflight: pf, PolicyDigest: digest},
		{State: sdk.WorkloadPreviewWouldLaunch, Preflight: pf},
	} {
		pv := previewOf(t, nil, "create", sdk.WorkloadCreated, launch)
		rig := newRouteRig(t, map[string]func(http.ResponseWriter, []byte){previewWeb: replyJSON(http.StatusOK, pv)})
		out, _, err := runRigWithStderr(t, rig, "apply", "-f", file, "--dry-run")
		want := launch.PolicyDigest != ""
		if err != nil || strings.Contains(out, "launch policy digest: "+digest+"\n") != want || strings.Contains(out, "launch policy digest") != want {
			t.Errorf("preview digest %q: err = %v, output:\n%s", launch.PolicyDigest, err, out)
		}
	}
}
