// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
)

// The capability caps are not overlay knobs: a list meet would keep a run on a provider's default
// profile as it is, so the caps are met with each provider row's ceiling at the doors instead.

func TestBoundaryCapsNeverReachOverlay(t *testing.T) {
	ado, gh := []adoscope.Capability{adoscope.CapCodeRead}, []ghscope.Capability{ghscope.CapCodeRead}
	with := BoundarySpec{AzureDevOpsCapabilities: &ado, GitHubCapabilities: &gh}
	if got, want := with.Overlay(), (BoundarySpec{}).Overlay(); !reflect.DeepEqual(got, want) {
		t.Errorf("a boundary naming the caps overlays %+v, want the overlay of one that names none %+v", got, want)
	}
}

func TestRestrictionCapsNeverReachOverlay(t *testing.T) {
	ado, gh := []adoscope.Capability{adoscope.CapCodeRead}, []ghscope.Capability{ghscope.CapCodeRead}
	with := RestrictionSpec{AzureDevOpsCapabilities: &ado, GitHubCapabilities: &gh}
	if got, want := with.Overlay(), (RestrictionSpec{}).Overlay(); !reflect.DeepEqual(got, want) {
		t.Errorf("a restriction naming the caps overlays %+v, want the overlay of one that names none %+v", got, want)
	}
}

// Absent stays absent on the wire, and a present list round-trips as written (pr stays pr).
func TestVCSCapsWireShape(t *testing.T) {
	var b BoundarySpec
	if err := json.Unmarshal([]byte(`{"azure_devops_capabilities":["code_read","pr"],"github_capabilities":["code_read"]}`), &b); err != nil {
		t.Fatal(err)
	}
	if b.AzureDevOpsCapabilities == nil || !reflect.DeepEqual(*b.AzureDevOpsCapabilities, []adoscope.Capability{adoscope.CapCodeRead, adoscope.CapPR}) ||
		b.GitHubCapabilities == nil || !reflect.DeepEqual(*b.GitHubCapabilities, []ghscope.Capability{ghscope.CapCodeRead}) {
		t.Fatalf("decoded %+v", b)
	}
	for name, v := range map[string]any{"boundary": BoundarySpec{}, "restriction": RestrictionSpec{}} {
		out, _ := json.Marshal(v)
		if string(out) != "{}" {
			t.Errorf("%s with no cap marshals as %s, want {}", name, out)
		}
	}
	var r RestrictionSpec
	if err := json.Unmarshal([]byte(`{"azure_devops_capabilities":[],"github_capabilities":[]}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.AzureDevOpsCapabilities == nil || r.GitHubCapabilities == nil {
		t.Error("an empty list decodes as absent, so an empty cap could not be refused at write")
	}
}
