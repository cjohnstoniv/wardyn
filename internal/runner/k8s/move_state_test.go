// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A Kubernetes runner sends no Caps.MoveImportPresent, which wardyn-runnerd derives from the Driver not being
// a runner.MoveImportPresenter: its kept or missing pod reads absent to the runner's observer, so a run
// moved to it would read gone before it starts.
func TestDriverIsNoMoveImportPresenter(t *testing.T) {
	if _, ok := any(&Driver{}).(runner.MoveImportPresenter); ok {
		t.Fatal("the k8s Driver is a runner.MoveImportPresenter; runnerd would advertise a destination whose imported pod reads absent")
	}
	if _, ok := any(&keepDriver{}).(runner.MoveImportPresenter); ok {
		t.Fatal("the k8s keep driver is a runner.MoveImportPresenter; its imported pod is held and reads stopped")
	}
}

// Only the keep driver moves a run: the bare Driver keeps no run whose files outlive its pod.
func TestDriverIsNoMoveStateSubstrate(t *testing.T) {
	if _, ok := any(&Driver{}).(runner.StateExporter); ok {
		t.Error("the bare k8s Driver is a runner.StateExporter")
	}
	if _, ok := any(&Driver{}).(runner.StateImporter); ok {
		t.Error("the bare k8s Driver is a runner.StateImporter")
	}
	if _, ok := any(&keepDriver{}).(runner.StateExporter); !ok {
		t.Error("the keep driver is not a runner.StateExporter")
	}
	if _, ok := any(&keepDriver{}).(runner.StateImporter); !ok {
		t.Error("the keep driver is not a runner.StateImporter")
	}
}

// An export refuses a carried root with no kept claim mounted at or under it: its files live only in the agent pod's
// own filesystem, which the helper cannot read, so the stream would silently lack them.
func TestMoveExport_RefusesRootWithoutClaim(t *testing.T) {
	k, cs, _, ref := keptRun(t)
	for _, name := range []string{proxyPodName(asRunID), agentPodName(asRunID)} {
		if err := cs.Tracker().Delete(podsGVR, testNamespace, name); err != nil && !isNotFound(err) {
			t.Fatal(err)
		}
	}
	set := runner.CarriedSet{Roots: []string{runner.ScratchWorkPath, "/home/agent/elsewhere"}}
	_, err := k.ExportState(context.Background(), ref, set)
	if err == nil || !strings.Contains(err.Error(), "the carried root /home/agent/elsewhere") {
		t.Fatalf("ExportState = %v, want the refusal naming /home/agent/elsewhere", err)
	}
}

func TestRootWithoutClaim(t *testing.T) {
	work := []helperClaim{{claim: "w", target: runner.ScratchWorkPath}}
	for _, tc := range []struct {
		name   string
		claims []helperClaim
		roots  []string
		want   string
	}{
		{"claim at the root", work, []string{runner.ScratchWorkPath}, ""},
		{"claim under the root", []helperClaim{{claim: "d", target: "/home/agent/a/b"}}, []string{"/home/agent/a"}, ""},
		{"claim above the root only", work, []string{runner.ScratchWorkPath + "/sub"}, runner.ScratchWorkPath + "/sub"},
		{"a sibling with the root as prefix", []helperClaim{{claim: "d", target: "/home/agent/ab"}}, []string{"/home/agent/a"}, "/home/agent/a"},
		{"second root uncovered", work, []string{runner.ScratchWorkPath, "/home/agent/other"}, "/home/agent/other"},
		{"no claims", nil, []string{runner.ScratchWorkPath}, runner.ScratchWorkPath},
	} {
		got, ok := rootWithoutClaim(tc.claims, tc.roots)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: rootWithoutClaim = %q, %v; want %q", tc.name, got, ok, tc.want)
		}
	}
}
