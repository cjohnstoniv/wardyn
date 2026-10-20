// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A Linux VM run that reaches the Kubernetes driver is refused before any object exists: a Pod
// must never start in a VM's place.
func TestCreateSandboxRefusesALinuxVM(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	before := len(cs.Actions()) // the driver's own start-up canary
	spec := runner.SandboxSpec{RunID: uuid.New(), Image: "img", MachineKind: types.MachineLinuxVM, ConfinementClass: types.CC1}
	if _, err := d.CreateSandbox(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "registered runner") {
		t.Fatalf("err = %v", err)
	}
	assertNoCreates(t, cs, before)
}

func assertNoCreates(t *testing.T, cs *fake.Clientset, from int) {
	t.Helper()
	for _, a := range cs.Actions()[from:] {
		if a.GetVerb() == "create" {
			t.Fatalf("a refused VM created %s", a.GetResource().Resource)
		}
	}
}

// vmDriver is a driver whose CC3 pin names a Kata RuntimeClass, with Linux VMs turned on.
func vmDriver(t *testing.T, cfg Config) (*Driver, *fake.Clientset) {
	t.Helper()
	cfg.ConfinementRuntimes = map[types.ConfinementClass]string{types.CC3: "kata"}
	d, cs := newTestDriver(t, cfg)
	mustCreateRuntimeClass(t, cs, "kata", "kata-qemu")
	if _, err := d.EnableLinuxVM(context.Background()); err != nil {
		t.Fatalf("EnableLinuxVM: %v", err)
	}
	return d, cs
}

// EnableLinuxVM offers VMs only when the CC3 pin resolves to a RuntimeClass that exists and boots a
// guest kernel; until it succeeds, a VM is refused.
func TestEnableLinuxVMNeedsTheCC3RuntimeClass(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pin     string
		handler string // "" = the RuntimeClass does not exist
		want    string
	}{
		{"no CC3 pin", "", "", "none is configured"},
		{"pinned class missing", "kata", "", "does not exist"},
		{"shared-kernel handler", "kata", "runc", "refusing to downgrade"},
		{"gVisor handler", "kata", "runsc", "refusing to downgrade"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{}
			if tc.pin != "" {
				cfg.ConfinementRuntimes = map[types.ConfinementClass]string{types.CC3: tc.pin}
			}
			d, cs := newTestDriver(t, cfg)
			if tc.handler != "" {
				mustCreateRuntimeClass(t, cs, tc.pin, tc.handler)
			}
			if _, err := d.EnableLinuxVM(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("EnableLinuxVM err = %v, want %q", err, tc.want)
			}
			before := len(cs.Actions())
			spec := testSandboxSpec()
			spec.MachineKind = types.MachineLinuxVM
			if _, err := d.CreateSandbox(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "registered runner") {
				t.Fatalf("CreateSandbox err = %v", err)
			}
			assertNoCreates(t, cs, before)
		})
	}
	t.Run("kata", func(t *testing.T) {
		d, cs := newTestDriver(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{types.CC3: "kata"}})
		mustCreateRuntimeClass(t, cs, "kata", "kata-qemu")
		m, err := d.EnableLinuxVM(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !m.Complete() || m.MachineType != "runtimeclass:kata-qemu" || m != vmMachine("kata-qemu") {
			t.Fatalf("machine = %+v, want every field runtimeclass:kata-qemu", m)
		}
	})
}

// A VM pod is the CC3 container pod for the same spec, field for field: the same NetworkPolicies
// (egress only to this run's proxy, no ingress), proxy pod and agent pod, under the pinned Kata
// RuntimeClass whatever class the run named. So it reaches exactly what a CC3 run reaches today, every
// confinement probe on that pod holds for it, and no Service exposes it.
func TestLinuxVMPodIsTheCC3Pod(t *testing.T) {
	ctx := context.Background()
	runID := uuid.New()
	create := func(d *Driver, cs *fake.Clientset, mutate func(*runner.SandboxSpec)) runner.Sandbox {
		t.Helper()
		installProxyIPReactor(t, cs, "10.244.0.31")
		installAgentRunningReactor(t, cs)
		spec := testSandboxSpec()
		spec.RunID = runID
		mutate(&spec)
		sb, err := d.CreateSandbox(ctx, spec)
		if err != nil {
			t.Fatalf("CreateSandbox: %v", err)
		}
		return sb
	}
	cd, ccs := vmDriver(t, Config{})
	csb := create(cd, ccs, func(s *runner.SandboxSpec) { s.ConfinementClass = types.CC3 })
	vd, vcs := vmDriver(t, Config{})
	vsb := create(vd, vcs, func(s *runner.SandboxSpec) { s.MachineKind = types.MachineLinuxVM }) // the run named CC1

	if vsb.EnforcedClass != types.CC3 || vsb.Ref != csb.Ref {
		t.Fatalf("VM sandbox = %+v, want CC3 at %s", vsb, csb.Ref)
	}
	if want := vmMachine("kata-qemu"); vsb.Machine == nil || *vsb.Machine != want {
		t.Fatalf("VM machine = %+v, want %+v", vsb.Machine, want)
	}
	if csb.Machine != nil {
		t.Fatalf("container sandbox carries a machine %+v", csb.Machine)
	}
	vpod := probeGetPod(t, vcs, vsb.Ref)
	if vpod.Spec.RuntimeClassName == nil || *vpod.Spec.RuntimeClassName != "kata" {
		t.Fatalf("VM pod RuntimeClassName = %v, want kata", vpod.Spec.RuntimeClassName)
	}
	for _, name := range []string{vsb.Ref, proxyPodName(runID)} {
		c, v := probeGetPod(t, ccs, name), probeGetPod(t, vcs, name)
		if diff := cmp.Diff(c.Spec, v.Spec); diff != "" || !cmp.Equal(c.Labels, v.Labels) {
			t.Errorf("pod %s differs from the CC3 run's (-cc3 +vm):\n%s labels %v vs %v", name, diff, c.Labels, v.Labels)
		}
	}
	cnp, err := ccs.NetworkingV1().NetworkPolicies(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	vnp, err := vcs.NetworkingV1().NetworkPolicies(testNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(vnp.Items) == 0 || len(vnp.Items) != len(cnp.Items) {
		t.Fatalf("VM run has %d NetworkPolicies, the CC3 run %d", len(vnp.Items), len(cnp.Items))
	}
	for i := range vnp.Items {
		if diff := cmp.Diff(cnp.Items[i].Spec, vnp.Items[i].Spec); diff != "" || cnp.Items[i].Name != vnp.Items[i].Name {
			t.Errorf("NetworkPolicy %s differs from the CC3 run's (-cc3 +vm):\n%s", vnp.Items[i].Name, diff)
		}
	}
	np := probeGetNetPol(t, vcs, agentNetPolName(runID))
	if len(np.Spec.Ingress) != 0 || len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].To) != 1 || np.Spec.Egress[0].To[0].PodSelector == nil ||
		!cmp.Equal(np.Spec.Egress[0].To[0].PodSelector.MatchLabels, wardynLabels(runID, componentProxy, nil)) {
		t.Fatalf("VM agent NetworkPolicy = %+v, want no ingress and egress only to this run's proxy", np.Spec)
	}
	if svcs, err := vcs.CoreV1().Services(testNamespace).List(ctx, metav1.ListOptions{}); err != nil || len(svcs.Items) != 0 {
		t.Fatalf("VM run Services = %v (%v), want none: VNC is reached only through the exec relay", svcs, err)
	}
	for _, c := range vpod.Spec.Containers {
		if len(c.Ports) != 0 {
			t.Fatalf("VM pod container %s declares ports %v", c.Name, c.Ports)
		}
	}
	if vpod.Spec.HostNetwork {
		t.Fatal("VM pod is on the host network")
	}
}

// What a VM does not honour in this release is refused before any object exists.
func TestLinuxVMRefusalsCreateNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record bool
		mutate func(*runner.SandboxSpec)
		want   string
	}{
		{"unknown kind", false, func(s *runner.SandboxSpec) { s.MachineKind = "macos_vm" }, "unknown machine kind"},
		{"drives", false, func(s *runner.SandboxSpec) {
			s.Drives = []types.DriveMount{{ObjectName: "c"}}
		}, "no drives"},
		{"recording", true, func(*runner.SandboxSpec) {}, "not recorded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := vmDriver(t, Config{Record: tc.record})
			before := len(cs.Actions())
			spec := testSandboxSpec()
			spec.MachineKind = types.MachineLinuxVM
			tc.mutate(&spec)
			if _, err := d.CreateSandbox(context.Background(), spec); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			assertNoCreates(t, cs, before)
		})
	}
}
