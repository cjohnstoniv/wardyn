// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runnerTrustRequired is the reason the managed-device requirement refuses with.
const runnerTrustRequired = Reason("runner_trust_required")

func managedDevicePlan(trust types.RunnerTrustClass, require bool) LocalPlan {
	return LocalPlan{Spec: runner.SandboxSpec{Image: "alpine"}, Trust: trust, RequireManagedDevice: require}
}

// With the organisation's requirement on, a plan judged at a personal runner is refused, naming the
// runner's class; managed_device and org_cluster runners take it. Off, every class takes it.
func TestLocalEligibilityRequiresManagedDevice(t *testing.T) {
	_, r := LocalEligibility(managedDevicePlan(types.RunnerTrustPersonal, true))
	if r == nil || r.Reason != runnerTrustRequired || r.Field != "trust_class" || !strings.Contains(r.Detail, "not marked as company-managed") {
		t.Fatalf("personal, required: %+v, want runner_trust_required on trust_class", r)
	}
	if got := runnerTrustRequired.Status(); got != 422 {
		t.Errorf("runner_trust_required status = %d, want 422", got)
	}
	for _, trust := range []types.RunnerTrustClass{types.RunnerTrustManagedDevice, types.RunnerTrustOrgCluster} {
		if _, r := LocalEligibility(managedDevicePlan(trust, true)); r != nil {
			t.Errorf("%s, required: %v, want admitted", trust, r)
		}
	}
	for _, trust := range allTrust {
		if _, r := LocalEligibility(managedDevicePlan(trust, false)); r != nil {
			t.Errorf("%s, not required: %v, want admitted (the default is off)", trust, r)
		}
	}
}

// An unknown trust class is not a personal one: the requirement does not change what it already got.
func TestLocalEligibilityRequiresManagedDevice_UnknownClassUnchanged(t *testing.T) {
	_, off := LocalEligibility(managedDevicePlan("", false))
	_, on := LocalEligibility(managedDevicePlan("", true))
	if (off == nil) != (on == nil) || (off != nil && off.Reason != on.Reason) {
		t.Errorf("unknown class: off %+v, on %+v, want the same answer", off, on)
	}
}

// An organisation's cluster runner is not a laptop: the requirement never refuses it, with a credential
// plan too.
func TestOrgClusterUnaffected(t *testing.T) {
	p := residentPlan(ClassPolicy{Mode: ModeRefuse}, nil)
	p.Delivery.ByTrust = map[types.RunnerTrustClass]TrustPolicy{types.RunnerTrustOrgCluster: {Classes: map[string]ClassPolicy{ClassEnvSecret: {Mode: ModeRunnerResident}}}}
	p.Trust = types.RunnerTrustOrgCluster
	_, before := LocalEligibility(p)
	p.RequireManagedDevice = true
	_, after := LocalEligibility(p)
	if before != nil || after != nil {
		t.Errorf("org_cluster: off %v, required %v, want admitted both ways", before, after)
	}
}

// The managed-device refusal is read by the person who started the run, so it says what to ask for in plain
// words: the role that marks a runner (PUT /runners/{id}/trust is the security tier's, a security admin or a
// super admin) and "a managed device", never the stored class names.
func TestManagedDeviceRefusalPlainWords(t *testing.T) {
	_, r := LocalEligibility(managedDevicePlan(types.RunnerTrustPersonal, true))
	const want = "this runner is not marked as company-managed; ask a security admin to mark it as a managed device"
	if r == nil || r.Detail != want {
		t.Fatalf("refusal %+v, want detail %q", r, want)
	}
}
