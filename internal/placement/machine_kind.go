// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import "github.com/cjohnstoniv/wardyn/internal/types"

// machineKindLabels and trustClassLabels name a kind and a class in a sentence a person reads.
var (
	machineKindLabels = map[types.MachineKind]string{"": "container", types.MachineLinuxVM: "Linux VM", types.MachineMacOSVM: "macOS VM"}
	trustClassLabels  = map[types.RunnerTrustClass]string{
		types.RunnerTrustPersonal:      "personal",
		types.RunnerTrustManagedDevice: "company-managed",
		types.RunnerTrustOrgCluster:    "organisation cluster",
	}
)

func labelOr[K comparable](labels map[K]string, k K, fallback string) string {
	if l, ok := labels[k]; ok {
		return l
	}
	return fallback
}

// machineKindRefusal is where a plan's machine kind may run (#2950), judged at the plan's trust class.
//   - A macOS VM runs only on a runner a security operator marked managed_device: any other class, an
//     unknown one included, is refused whatever the organisation's list says, since the list only denies.
//     Where the create's managed-Mac gate runs later (MacOSHostGateLater), it is left to that gate, which
//     refuses with its own reason and the operator's fix, and nothing here judges that run.
//   - With the organisation's runners.machine_kind_denials set, a class Wardyn does not know is refused
//     for every kind: the list cannot be judged at it, and a refusal is the safe answer.
//   - Then the list: a kind it denies at this class is refused.
func machineKindRefusal(p LocalPlan) *Refusal {
	kind := p.MachineKind.Normalize()
	var r *Refusal
	switch {
	case kind.IsMacOSVM() && p.Trust != types.RunnerTrustManagedDevice && p.MacOSHostGateLater:
		return nil
	case kind.IsMacOSVM() && p.Trust != types.RunnerTrustManagedDevice:
		r = refuse(ReasonRunnerTrustRequired, "machine_kind",
			"A macOS VM sandbox runs only on a Mac marked as company-managed. Ask a security admin to mark it as a managed device.")
	case len(p.MachineKindDenials) > 0 && !p.Trust.Valid():
		r = refuse(ReasonRunnerTrustRequired, "machine_kind",
			"Your organisation limits machine kinds by runner trust class, and this runner has no trust class Wardyn knows. Ask a security admin to set it.")
	case types.DeniesMachineKind(p.MachineKindDenials, p.Trust, kind):
		r = refuse(ReasonPlacementDenied, "machine_kind", "Your organisation does not allow "+
			labelOr(machineKindLabels, kind, string(kind))+" sandboxes on "+labelOr(trustClassLabels, p.Trust, string(p.Trust))+" runners.")
	default:
		return nil
	}
	r.named = true
	return r
}
