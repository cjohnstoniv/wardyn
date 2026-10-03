// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// With no ratio the agent pod's requests equal its limits (Guaranteed); with one they are the
// ratio's fraction (Burstable). The proxy pod stays Guaranteed either way.
func TestResourceRequirementsRatio(t *testing.T) {
	t.Cleanup(func() { _ = runner.SetRequestRatio(0) })
	res := runner.Resources{CPUMillis: 1000, MemoryMiB: 2048}
	r := resourceRequirements(res)
	if !r.Requests.Cpu().Equal(*r.Limits.Cpu()) || !r.Requests.Memory().Equal(*r.Limits.Memory()) {
		t.Errorf("no ratio: requests %v != limits %v", r.Requests, r.Limits)
	}
	if err := runner.SetRequestRatio(0.5); err != nil {
		t.Fatal(err)
	}
	r = resourceRequirements(res)
	if r.Requests.Cpu().MilliValue() != 500 || r.Requests.Memory().Value() != 1024*1024*1024 {
		t.Errorf("ratio 0.5: requests = %v", r.Requests)
	}
	if r.Limits.Cpu().MilliValue() != 1000 || r.Limits.Memory().Value() != 2048*1024*1024 {
		t.Errorf("ratio 0.5: limits moved: %v", r.Limits)
	}
	p := proxyResources()
	if !p.Requests.Cpu().Equal(*p.Limits.Cpu()) || !p.Requests.Memory().Equal(*p.Limits.Memory()) {
		t.Errorf("proxy requests %v != limits %v with the ratio set", p.Requests, p.Limits)
	}
}
