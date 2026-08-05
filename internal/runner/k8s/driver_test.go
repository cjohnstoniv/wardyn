// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"testing"

	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestName(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	if got := d.Name(); got != "k8s" {
		t.Errorf("Name() = %q, want %q", got, "k8s")
	}
}

// TestClasses_CC1Unconditional covers the always-CC1 floor: reaching a
// constructed Driver already proves the canary passed (or opted out).
func TestClasses_CC1Unconditional(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	cls, err := d.Classes(context.Background())
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	if !containsClass(cls.Classes, types.CC1) {
		t.Errorf("Classes = %v, want it to contain CC1", cls.Classes)
	}
}

// TestClasses_CC2_NoPin covers the k8s-specific default: unlike docker (which
// has a real built-in runtime-family default), a RuntimeClass object name
// carries no platform convention Wardyn can safely guess — CC2 is simply not
// advertised without an explicit WARDYN_CONFINEMENT_MAP pin.
func TestClasses_CC2_NoPin(t *testing.T) {
	d, _ := newTestDriver(t, Config{})
	cls, err := d.Classes(context.Background())
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	if containsClass(cls.Classes, types.CC2) {
		t.Errorf("Classes = %v, want it to NOT contain CC2 with no pin", cls.Classes)
	}
}

// TestClasses_CC2_PinnedAndPresent_RunscHandler covers the floor guard's
// happy path: a pinned RuntimeClass exists and its .Handler is runsc-prefixed.
func TestClasses_CC2_PinnedAndPresent_RunscHandler(t *testing.T) {
	d, cs := newTestDriver(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{types.CC2: "gvisor"}})
	mustCreateRuntimeClass(t, cs, "gvisor", "runsc")

	cls, err := d.Classes(context.Background())
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	if !containsClass(cls.Classes, types.CC2) {
		t.Errorf("Classes = %v, want it to contain CC2 (pinned RuntimeClass %q has handler runsc)", cls.Classes, "gvisor")
	}
	if got := cls.Resolved[types.CC2]; got != "k8s/runsc" {
		t.Errorf("Resolved[CC2] = %q, want %q", got, "k8s/runsc")
	}
}

// TestClasses_CC2_PinnedButWrongHandler_NeverDowngrades covers invariant 5: a
// pinned RuntimeClass whose Handler is NOT runsc-prefixed (e.g. plain runc)
// must never silently grant CC2 — the floor guard refuses it.
func TestClasses_CC2_PinnedButWrongHandler_NeverDowngrades(t *testing.T) {
	d, cs := newTestDriver(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{types.CC2: "not-really-sandboxed"}})
	mustCreateRuntimeClass(t, cs, "not-really-sandboxed", "runc")

	cls, err := d.Classes(context.Background())
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	if containsClass(cls.Classes, types.CC2) {
		t.Errorf("Classes = %v, want it to NOT contain CC2 (handler %q is not runsc-prefixed)", cls.Classes, "runc")
	}
}

// TestClasses_CC2_PinnedButAbsent_NotAdvertised covers "exists" being part of
// the gate: a pin naming a RuntimeClass that isn't registered in the cluster
// must fail closed (omit the class), not error the whole Classes() call.
func TestClasses_CC2_PinnedButAbsent_NotAdvertised(t *testing.T) {
	d, _ := newTestDriver(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{types.CC2: "does-not-exist"}})
	cls, err := d.Classes(context.Background())
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	if containsClass(cls.Classes, types.CC2) {
		t.Errorf("Classes = %v, want it to NOT contain CC2 (pinned RuntimeClass does not exist)", cls.Classes)
	}
}

// TestClasses_CC3_RefusesKnownNonVaultHandlers covers the CC3 floor guard's
// NEGATIVE (denylist) shape, distinct from CC2's positive allowlist: any
// handler NOT positively known to be shared-kernel/userspace-kernel is
// accepted (bring-your-own microVM), but runc/crun/sysbox/runsc are refused.
func TestClasses_CC3_RefusesKnownNonVaultHandlers(t *testing.T) {
	for _, handler := range []string{"runc", "crun", "sysbox-runc", "runsc"} {
		t.Run(handler, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{types.CC3: "pinned-cc3"}})
			mustCreateRuntimeClass(t, cs, "pinned-cc3", handler)

			cls, err := d.Classes(context.Background())
			if err != nil {
				t.Fatalf("Classes: %v", err)
			}
			if containsClass(cls.Classes, types.CC3) {
				t.Errorf("Classes = %v, want it to NOT contain CC3 (handler %q is known non-vault)", cls.Classes, handler)
			}
		})
	}
}

// TestClasses_CC3_AcceptsBringYourOwnVaultHandler covers CC3's permissive
// side: a handler NOT on the known-non-vault list (a bring-your-own microVM
// the operator vouches for) is accepted, mirroring docker's resolveRuntime.
func TestClasses_CC3_AcceptsBringYourOwnVaultHandler(t *testing.T) {
	d, cs := newTestDriver(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{types.CC3: "my-microvm"}})
	mustCreateRuntimeClass(t, cs, "my-microvm", "firecracker")

	cls, err := d.Classes(context.Background())
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	if !containsClass(cls.Classes, types.CC3) {
		t.Errorf("Classes = %v, want it to contain CC3 (handler %q is not a known-non-vault runtime)", cls.Classes, "firecracker")
	}
	if got := cls.Resolved[types.CC3]; got != "k8s/firecracker" {
		t.Errorf("Resolved[CC3] = %q, want %q", got, "k8s/firecracker")
	}
}

func containsClass(classes []types.ConfinementClass, want types.ConfinementClass) bool {
	for _, c := range classes {
		if c == want {
			return true
		}
	}
	return false
}

func mustCreateRuntimeClass(t *testing.T, cs kubernetes.Interface, name, handler string) {
	t.Helper()
	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Handler: handler}
	if _, err := cs.NodeV1().RuntimeClasses().Create(context.Background(), rc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create RuntimeClass %q: %v", name, err)
	}
}
