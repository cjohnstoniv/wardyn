// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCreateSandbox_PinsTheRuntimeClassPerTier is the enforcement half of the
// k8s confinement tiers. Classes (driver_test.go) only covers what the
// substrate ADVERTISES; this covers what CreateSandbox actually DOES with a
// Wall (CC2) or Vault (CC3) request: the agent pod carries the pinned
// RuntimeClass and EnforcedClass is the class asked for, or the call refuses
// with errRuntimeClassUnavailable before a single object exists in the
// namespace. A pod admitted without its RuntimeClass runs on the node's
// default runtime (runc) while the run reads as sandboxed.
func TestCreateSandbox_PinsTheRuntimeClassPerTier(t *testing.T) {
	t.Run("pinned", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			class   types.ConfinementClass
			rcName  string
			handler string
		}{
			{"CC2 gVisor", types.CC2, "gvisor", "runsc"},
			{"CC2 gVisor KVM platform", types.CC2, "gvisor-kvm", "runsc-kvm"},
			{"CC3 Kata", types.CC3, "kata", "kata-qemu"},
			{"CC3 bring-your-own microVM", types.CC3, "fc", "firecracker"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				d, cs := newTestDriver(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{tc.class: tc.rcName}})
				mustCreateRuntimeClass(t, cs, tc.rcName, tc.handler)
				installProxyIPReactor(t, cs, "10.244.0.21")
				installAgentRunningReactor(t, cs)

				spec := testSandboxSpec()
				spec.ConfinementClass = tc.class
				sb, err := d.CreateSandbox(context.Background(), spec)
				if err != nil {
					t.Fatalf("CreateSandbox(%s): %v", tc.class, err)
				}
				if sb.EnforcedClass != tc.class {
					t.Errorf("EnforcedClass = %q, want %q", sb.EnforcedClass, tc.class)
				}
				pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("get agent pod: %v", err)
				}
				if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != tc.rcName {
					t.Fatalf("agent pod RuntimeClassName = %v, want %q — a %s pod without it runs on the node's default runtime",
						pod.Spec.RuntimeClassName, tc.rcName, tc.class)
				}
			})
		}
	})

	// CC1 pins nothing: the node's default runtime IS the CC1 tier, and a pin
	// here would name a RuntimeClass the operator never configured.
	t.Run("CC1 pins no RuntimeClass", func(t *testing.T) {
		d, cs := newTestDriver(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{types.CC2: "gvisor"}})
		mustCreateRuntimeClass(t, cs, "gvisor", "runsc")
		installProxyIPReactor(t, cs, "10.244.0.22")
		installAgentRunningReactor(t, cs)

		sb, err := d.CreateSandbox(context.Background(), testSandboxSpec())
		if err != nil {
			t.Fatalf("CreateSandbox(CC1): %v", err)
		}
		if sb.EnforcedClass != types.CC1 {
			t.Errorf("EnforcedClass = %q, want CC1", sb.EnforcedClass)
		}
		pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get agent pod: %v", err)
		}
		if pod.Spec.RuntimeClassName != nil {
			t.Errorf("CC1 agent pod RuntimeClassName = %q, want none", *pod.Spec.RuntimeClassName)
		}
	})

	t.Run("refused", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			class   types.ConfinementClass
			pin     string // "" = no WARDYN_CONFINEMENT_MAP entry
			handler string // "" = the pinned RuntimeClass does not exist
		}{
			{"CC2 unpinned", types.CC2, "", ""},
			{"CC2 pinned but absent", types.CC2, "gvisor", ""},
			{"CC2 on runc", types.CC2, "gvisor", "runc"},
			{"CC2 on crun", types.CC2, "gvisor", "crun"},
			{"CC2 on kata", types.CC2, "gvisor", "kata-qemu"},
			{"CC3 unpinned", types.CC3, "", ""},
			{"CC3 pinned but absent", types.CC3, "kata", ""},
			{"CC3 on runc", types.CC3, "kata", "runc"},
			{"CC3 on crun", types.CC3, "kata", "crun"},
			{"CC3 on runsc", types.CC3, "kata", "runsc"},
			{"CC3 on sysbox", types.CC3, "kata", "sysbox-runc"},
			{"unknown class", types.ConfinementClass("CC9"), "", ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cfg := Config{}
				if tc.pin != "" {
					cfg.ConfinementRuntimes = map[types.ConfinementClass]string{tc.class: tc.pin}
				}
				d, cs := newTestDriver(t, cfg)
				if tc.handler != "" {
					mustCreateRuntimeClass(t, cs, tc.pin, tc.handler)
				}
				assertRefusedBeforeAnyWrite(t, d, cs, tc.class, true)
			})
		}
	})

	// The capabilities a scheduler read at boot can be stale by the time a run
	// dispatches: an operator deletes the RuntimeClass, or repoints its name at
	// a weaker handler. CreateSandbox must re-resolve per run and refuse, never
	// trust the class list it once advertised.
	t.Run("stale capabilities", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			class      types.ConfinementClass
			handler    string
			newHandler string // "" = the RuntimeClass is deleted
		}{
			{"CC2 RuntimeClass deleted", types.CC2, "runsc", ""},
			{"CC2 RuntimeClass repointed at runc", types.CC2, "runsc", "runc"},
			{"CC3 RuntimeClass deleted", types.CC3, "kata-qemu", ""},
			{"CC3 RuntimeClass repointed at runsc", types.CC3, "kata-qemu", "runsc"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				d, cs := newTestDriver(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{tc.class: "pinned"}})
				mustCreateRuntimeClass(t, cs, "pinned", tc.handler)
				cls, err := d.Classes(context.Background())
				if err != nil {
					t.Fatalf("Classes: %v", err)
				}
				if !containsClass(cls.Classes, tc.class) {
					t.Fatalf("precondition: Classes = %v, want %s advertised", cls.Classes, tc.class)
				}

				rcs := cs.NodeV1().RuntimeClasses()
				if tc.newHandler == "" {
					if err := rcs.Delete(context.Background(), "pinned", metav1.DeleteOptions{}); err != nil {
						t.Fatalf("delete RuntimeClass: %v", err)
					}
				} else {
					rc, err := rcs.Get(context.Background(), "pinned", metav1.GetOptions{})
					if err != nil {
						t.Fatalf("get RuntimeClass: %v", err)
					}
					rc.Handler = tc.newHandler
					if _, err := rcs.Update(context.Background(), rc, metav1.UpdateOptions{}); err != nil {
						t.Fatalf("update RuntimeClass: %v", err)
					}
				}
				assertRefusedBeforeAnyWrite(t, d, cs, tc.class, true)
			})
		}
	})

	// A RuntimeClass the control plane cannot READ (RBAC, a flaky apiserver) is
	// not a pin it can vouch for: refuse, and still create nothing.
	t.Run("RuntimeClass unreadable", func(t *testing.T) {
		d, cs := newTestDriver(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{types.CC2: "gvisor"}})
		mustCreateRuntimeClass(t, cs, "gvisor", "runsc")
		cs.PrependReactor("get", "runtimeclasses", func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "node.k8s.io", Resource: "runtimeclasses"}, "gvisor", errors.New("test: RBAC"))
		})
		assertRefusedBeforeAnyWrite(t, d, cs, types.CC2, false)
	})
}

// assertRefusedBeforeAnyWrite runs CreateSandbox at class and requires a
// refusal that issued no create of ANY kind: no pod, Secret, NetworkPolicy or
// claim. Reading the recorded actions, not listing survivors, is the point — a
// refusal that created objects and rolled them back still put the run's
// credentials in the namespace for a moment. wantSentinel additionally
// requires errRuntimeClassUnavailable in the chain.
func assertRefusedBeforeAnyWrite(t *testing.T, d *Driver, cs *fake.Clientset, class types.ConfinementClass, wantSentinel bool) {
	t.Helper()
	installProxyIPReactor(t, cs, "10.244.0.23")
	installAgentRunningReactor(t, cs)
	cs.ClearActions()

	spec := testSandboxSpec()
	spec.ConfinementClass = class
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err == nil {
		t.Fatalf("CreateSandbox(%s) = %+v, nil; want a refusal", class, sb)
	}
	if wantSentinel && !errors.Is(err, errRuntimeClassUnavailable) {
		t.Errorf("CreateSandbox(%s) error = %v, want errRuntimeClassUnavailable in the chain", class, err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			t.Errorf("refused CreateSandbox(%s) still issued create %s", class, a.GetResource().Resource)
		}
	}
	assertRunObjectsGone(t, cs, spec.RunID)
}
