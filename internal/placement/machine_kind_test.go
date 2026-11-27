// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

var (
	trustClasses = []types.RunnerTrustClass{types.RunnerTrustPersonal, types.RunnerTrustManagedDevice, types.RunnerTrustOrgCluster}
	machineKinds = []types.MachineKind{"", types.MachineLinuxVM, types.MachineMacOSVM}
)

func kindPlan(trust types.RunnerTrustClass, kind types.MachineKind, denials ...types.MachineKindDenial) LocalPlan {
	return LocalPlan{Spec: runner.SandboxSpec{Image: "alpine", MachineKind: kind}, Trust: trust, MachineKind: kind, MachineKindDenials: denials}
}

// Each cell of the 3x3 matrix is independent: one denial refuses its cell and no other. A macOS VM off
// managed_device is refused by the floor whatever the list says, so those cells are checked for the
// refusal's reason too.
func TestMachineKindRefusal_DenialsMatrix(t *testing.T) {
	for _, dc := range trustClasses {
		for _, dk := range machineKinds {
			denial := types.MachineKindDenial{Trust: dc, MachineKind: dk}
			for _, c := range trustClasses {
				for _, k := range machineKinds {
					r := machineKindRefusal(kindPlan(c, k, denial))
					floor := k == types.MachineMacOSVM && c != types.RunnerTrustManagedDevice
					switch {
					case floor:
						if r == nil || r.Reason != ReasonRunnerTrustRequired {
							t.Errorf("denial %v, plan %s/%q: refusal = %v, want the trust floor", denial, c, k, r)
						}
					case c == dc && k.Normalize() == dk.Normalize():
						if r == nil || r.Reason != ReasonPlacementDenied {
							t.Errorf("denial %v, plan %s/%q: refusal = %v, want placement_denied", denial, c, k, r)
						}
					default:
						if r != nil {
							t.Errorf("denial %v refused another cell, plan %s/%q: %v", denial, c, k, r)
						}
					}
				}
			}
		}
	}
	// "container" and "" are one cell.
	if r := machineKindRefusal(kindPlan(types.RunnerTrustPersonal, types.MachineContainer, types.MachineKindDenial{Trust: types.RunnerTrustPersonal, MachineKind: ""})); r == nil {
		t.Error("a container denial stored as \"\" did not refuse a \"container\" plan")
	}
	if r := machineKindRefusal(kindPlan(types.RunnerTrustPersonal, "", types.MachineKindDenial{Trust: types.RunnerTrustPersonal, MachineKind: types.MachineContainer})); r == nil {
		t.Error("a container denial stored as \"container\" did not refuse a \"\" plan")
	}
}

// A macOS VM needs managed_device at every door: personal, org_cluster and an unknown class are refused
// with an empty list and with any list.
func TestMachineKindRefusal_MacOSNeedsManagedDevice(t *testing.T) {
	lists := map[string][]types.MachineKindDenial{
		"no list":        nil,
		"another cell":   {{Trust: types.RunnerTrustManagedDevice, MachineKind: types.MachineLinuxVM}},
		"its own cell":   {{Trust: types.RunnerTrustManagedDevice, MachineKind: types.MachineMacOSVM}},
		"unrelated cell": {{Trust: types.RunnerTrustPersonal, MachineKind: ""}},
	}
	for name, list := range lists {
		for _, c := range []types.RunnerTrustClass{types.RunnerTrustPersonal, types.RunnerTrustOrgCluster, "", "laptop"} {
			r := machineKindRefusal(kindPlan(c, types.MachineMacOSVM, list...))
			if r == nil || r.Reason != ReasonRunnerTrustRequired {
				t.Errorf("%s, class %q: refusal = %v, want runner_trust_required", name, c, r)
			}
		}
	}
	if r := machineKindRefusal(kindPlan(types.RunnerTrustManagedDevice, types.MachineMacOSVM)); r != nil {
		t.Errorf("a macOS VM on managed_device with no list: %v", r)
	}
}

// At create the managed-Mac gate runs later and answers with its own reason, so nothing here judges that run:
// not the floor, and not the list's arms either.
func TestMachineKindRefusal_CreateLeavesMacOSToTheHostGate(t *testing.T) {
	for _, c := range []types.RunnerTrustClass{types.RunnerTrustPersonal, types.RunnerTrustOrgCluster, ""} {
		for name, list := range map[string][]types.MachineKindDenial{
			"no list":           nil,
			"a list denying it": {{Trust: c, MachineKind: types.MachineMacOSVM}, {Trust: types.RunnerTrustPersonal, MachineKind: types.MachineMacOSVM}},
		} {
			later := kindPlan(c, types.MachineMacOSVM, list...)
			later.MacOSHostGateLater = true
			if r := machineKindRefusal(later); r != nil {
				t.Errorf("%s, class %q, gate later: refusal = %v, want nil", name, c, r)
			}
			now := later
			now.MacOSHostGateLater = false
			if r := machineKindRefusal(now); r == nil || r.Reason != ReasonRunnerTrustRequired {
				t.Errorf("%s, class %q, floor here: refusal = %v, want runner_trust_required", name, c, r)
			}
		}
	}
}

func TestMachineKindRefusal_UnknownTrustWithAList(t *testing.T) {
	list := []types.MachineKindDenial{{Trust: types.RunnerTrustOrgCluster, MachineKind: types.MachineLinuxVM}}
	for _, c := range []types.RunnerTrustClass{"", "laptop"} {
		for _, k := range []types.MachineKind{"", types.MachineLinuxVM} {
			r := machineKindRefusal(kindPlan(c, k, list...))
			if r == nil || r.Reason != ReasonRunnerTrustRequired {
				t.Errorf("class %q kind %q with a list: refusal = %v, want runner_trust_required", c, k, r)
			}
		}
		if r := machineKindRefusal(kindPlan(c, "")); r != nil {
			t.Errorf("class %q with an empty list, a container: %v", c, r)
		}
	}
}

func TestMachineKindRefusal_SentenceIsTheDetail(t *testing.T) {
	r := machineKindRefusal(kindPlan(types.RunnerTrustPersonal, types.MachineLinuxVM, types.MachineKindDenial{Trust: types.RunnerTrustPersonal, MachineKind: types.MachineLinuxVM}))
	if r == nil {
		t.Fatal("no refusal")
	}
	if want := "Your organisation does not allow Linux VM sandboxes on personal runners."; r.Sentence() != want {
		t.Fatalf("Sentence() = %q, want %q", r.Sentence(), want)
	}
	if strings.Contains(r.Sentence(), "machine_kind:") {
		t.Fatalf("Sentence() carries the field prefix: %q", r.Sentence())
	}
}

func TestLocalEligibility_RefusesMacOSOffManagedDevice(t *testing.T) {
	for _, c := range []types.RunnerTrustClass{types.RunnerTrustPersonal, types.RunnerTrustOrgCluster, ""} {
		if _, r := LocalEligibility(kindPlan(c, types.MachineMacOSVM)); r == nil || r.Reason != ReasonRunnerTrustRequired || r.Field != "machine_kind" {
			t.Errorf("class %q: refusal = %v, want runner_trust_required on machine_kind", c, r)
		}
	}
	if _, r := LocalEligibility(kindPlan(types.RunnerTrustManagedDevice, types.MachineMacOSVM)); r != nil {
		t.Errorf("a macOS VM on a managed device: %v", r)
	}
}

func TestLocalEligibility_RefusesADeniedKind(t *testing.T) {
	denial := types.MachineKindDenial{Trust: types.RunnerTrustPersonal, MachineKind: types.MachineLinuxVM}
	if _, r := LocalEligibility(kindPlan(types.RunnerTrustPersonal, types.MachineLinuxVM, denial)); r == nil || r.Reason != ReasonPlacementDenied {
		t.Fatalf("a denied Linux VM: refusal = %v, want placement_denied", r)
	}
	if _, r := LocalEligibility(kindPlan(types.RunnerTrustManagedDevice, types.MachineLinuxVM, denial)); r != nil {
		t.Fatalf("the same kind on another class: %v", r)
	}
}

// A local run with none of the new settings is still eligible: the new plan fields are classified.
func TestLocalEligibility_AdmitsAPlainLocalRun(t *testing.T) {
	if got := Unclassified(); len(got) != 0 {
		t.Fatalf("Unclassified() = %v, want none", got)
	}
	plain := LocalPlan{Spec: runner.SandboxSpec{Image: "alpine"}, Trust: types.RunnerTrustPersonal}
	if _, r := LocalEligibility(plain); r != nil {
		t.Fatalf("a plain container plan: %v", r)
	}
	plain.MachineKindDenials = []types.MachineKindDenial{{Trust: types.RunnerTrustPersonal, MachineKind: types.MachineLinuxVM}}
	if _, r := LocalEligibility(plain); r != nil {
		t.Fatalf("a container plan with a list denying another kind: %v", r)
	}
}

// I11: a denial keyed to a site only takes targets away. Over every trust class (an unknown one included),
// kind and site of the target, with and without the create's deferred macOS gate, adding one site-keyed
// denial to a list never admits a plan the list alone refused, and any plan it newly refuses is at its site.
// A site on the plan never admits what the same plan at no site is refused.
func TestSitePlaceRuleOnlyRemovesTargets(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	sites := []*uuid.UUID{nil, &a, &b}
	classes := append([]types.RunnerTrustClass{""}, trustClasses...)
	bases := [][]types.MachineKindDenial{nil}
	for _, c := range trustClasses {
		for _, k := range machineKinds {
			bases = append(bases, []types.MachineKindDenial{{Trust: c, MachineKind: k}})
		}
	}
	for _, rc := range trustClasses {
		for _, rk := range machineKinds {
			for _, rs := range []*uuid.UUID{&a, &b} {
				rule := types.MachineKindDenial{Trust: rc, MachineKind: rk, SiteID: rs.String()}
				for _, base := range bases {
					with := append(slices.Clone(base), rule)
					for _, c := range classes {
						for _, k := range machineKinds {
							for _, site := range sites {
								for _, later := range []bool{false, true} {
									plan := kindPlan(c, k, base...)
									plan.MacOSHostGateLater = later
									unsited := machineKindRefusal(plan)
									plan.SiteID, plan.SiteName = site, "Office"
									before := machineKindRefusal(plan)
									if unsited != nil && before == nil {
										t.Fatalf("a site admitted %s/%q at %v that %v refused at no site: %v", c, k, site, base, unsited)
									}
									plan.MachineKindDenials = with
									after := machineKindRefusal(plan)
									if before != nil && after == nil {
										t.Fatalf("rule %v admitted %s/%q at %v that %v refused: %v", rule, c, k, site, base, before)
									}
									if before == nil && after != nil && (site == nil || *site != *rs) {
										t.Fatalf("rule %v at %s refused %s/%q at %v, off its site: %v", rule, rs, c, k, site, after)
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

// A denial keyed to a site refuses its cell at that site, naming the site, and nowhere else; an unkeyed denial
// of the same cell keeps its sentence, with no site named.
func TestMachineKindRefusal_SiteRuleNamesTheSite(t *testing.T) {
	office, branch := uuid.New(), uuid.New()
	rule := types.MachineKindDenial{Trust: types.RunnerTrustPersonal, MachineKind: types.MachineLinuxVM, SiteID: office.String()}
	plan := func(site *uuid.UUID, name string, ds ...types.MachineKindDenial) LocalPlan {
		p := kindPlan(types.RunnerTrustPersonal, types.MachineLinuxVM, ds...)
		p.SiteID, p.SiteName = site, name
		return p
	}
	r := machineKindRefusal(plan(&office, "Leeds office", rule))
	if want := "Your organisation does not allow Linux VM sandboxes on personal runners at Leeds office."; r == nil || r.Reason != ReasonPlacementDenied || r.Sentence() != want {
		t.Fatalf("at the rule's site: %v, want placement_denied %q", r, want)
	}
	if r := machineKindRefusal(plan(&office, "", rule)); r == nil || !strings.HasSuffix(r.Sentence(), " at this run's site.") {
		t.Fatalf("at the rule's site with no name read: %v", r)
	}
	if r := machineKindRefusal(plan(&branch, "Branch", rule)); r != nil {
		t.Fatalf("at another site: %v, want admitted", r)
	}
	if r := machineKindRefusal(plan(nil, "", rule)); r != nil {
		t.Fatalf("with no site: %v, want admitted", r)
	}
	everywhere := types.MachineKindDenial{Trust: types.RunnerTrustPersonal, MachineKind: types.MachineLinuxVM}
	if r := machineKindRefusal(plan(&office, "Leeds office", everywhere, rule)); r == nil || r.Sentence() != "Your organisation does not allow Linux VM sandboxes on personal runners." {
		t.Fatalf("an unkeyed denial of the same cell at the site: %v", r)
	}
	// An unknown class at another site is not refused by a list that holds only at the rule's site.
	unknown := plan(&branch, "Branch", rule)
	unknown.Trust = ""
	if r := machineKindRefusal(unknown); r != nil {
		t.Fatalf("an unknown class off the rule's site: %v, want admitted", r)
	}
}
