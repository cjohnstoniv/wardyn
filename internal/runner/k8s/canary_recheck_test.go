// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// recheckCluster is a fake cluster the canary can run on any number of times: a canary pod exits with
// phaseA while no canary deny-all policy is in its namespace and with phaseB while one is. Set either
// mid-test to change what the next check finds.
type recheckCluster struct {
	cs     *fake.Clientset
	phaseA atomic.Int32
	phaseB atomic.Int32
	pods   atomic.Int32 // canary pods created
}

func newRecheckCluster(t *testing.T) *recheckCluster {
	t.Helper()
	c := &recheckCluster{cs: fake.NewClientset()}
	c.phaseB.Store(1) // enforced
	var mu sync.Mutex
	denyAll := map[string]int{}
	// The policy reactors only record and fall through, so the tracker still stores and deletes the object.
	c.cs.PrependReactor("create", "networkpolicies", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if np, ok := a.(clienttesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy); ok && strings.HasPrefix(np.Name, "wardyn-egress-canary-netpol-") {
			mu.Lock()
			denyAll[a.GetNamespace()]++
			mu.Unlock()
		}
		return false, nil, nil
	})
	c.cs.PrependReactor("delete", "networkpolicies", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if strings.HasPrefix(a.(clienttesting.DeleteAction).GetName(), "wardyn-egress-canary-netpol-") {
			mu.Lock()
			denyAll[a.GetNamespace()]--
			mu.Unlock()
		}
		return false, nil, nil
	})
	c.cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if p, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod); ok && strings.HasPrefix(p.Name, "wardyn-egress-canary-") {
			c.pods.Add(1)
		}
		return false, nil, nil
	})
	c.cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := a.(clienttesting.GetAction).GetName()
		if !strings.HasPrefix(name, "wardyn-egress-canary-") {
			return false, nil, nil
		}
		mu.Lock()
		phaseB := denyAll[a.GetNamespace()] > 0
		mu.Unlock()
		code := c.phaseA.Load()
		if phaseB {
			code = c.phaseB.Load()
		}
		return true, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.GetNamespace()},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  canaryContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}},
			}}},
		}, nil
	})
	return c
}

// boot constructs a Driver on the cluster as New does, without the loop, on a clock the test moves.
func (c *recheckCluster) boot(t *testing.T, cfg Config) (*Driver, *time.Time) {
	t.Helper()
	if len(cfg.Namespaces) == 0 {
		cfg.Namespaces = []string{testNamespace}
	}
	cfg.ProxyImage = "wardyn/wardyn-proxy:test"
	d, err := newWithClient(context.Background(), c.cs, testRestConfig(), cfg)
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	clock := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return clock }
	return d, &clock
}

func networkPolicyClaim(t *testing.T, d *Driver) (enforced, acknowledged bool) {
	t.Helper()
	cls, err := d.Classes(context.Background())
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	return cls.NetworkPolicy, cls.NetworkPolicyAcknowledged
}

// TestCanaryRecheck_ThreeFailedChecksRefuseNewSandboxes: a cluster that stops enforcing NetworkPolicy
// keeps its proven verdict for two failed checks, loses it on the third (Classes reads false and new
// sandboxes are refused), and gets it back on the first check that proves enforcement again.
func TestCanaryRecheck_ThreeFailedChecksRefuseNewSandboxes(t *testing.T) {
	c := newRecheckCluster(t)
	d, clock := c.boot(t, Config{})
	if err := d.netPolRefusal(testNamespace); err != nil {
		t.Fatalf("after a proving boot: refusal %v, want none", err)
	}

	c.phaseB.Store(0) // the CNI stops enforcing
	for i := 1; i <= canaryRecheckStrikes; i++ {
		*clock = clock.Add(canaryRecheckInterval)
		d.recheckOnce(context.Background())
		err := d.netPolRefusal(testNamespace)
		enforced, _ := networkPolicyClaim(t, d)
		if i < canaryRecheckStrikes && (err != nil || !enforced) {
			t.Fatalf("after %d failed check(s): refusal %v, NetworkPolicy %v; want the proven verdict kept", i, err, enforced)
		}
		if i == canaryRecheckStrikes && (!errors.Is(err, errNetPolNotProven) || enforced) {
			t.Fatalf("after %d failed checks: refusal %v, NetworkPolicy %v; want refused and false", i, err, enforced)
		}
	}
	if got := d.netPol.Load().byNS[testNamespace]; got.state != netPolUnenforced || !got.checkedAt.Equal(*clock) {
		t.Errorf("verdict %v checked %v, want %v checked %v", got.state, got.checkedAt, netPolUnenforced, *clock)
	}
	if err := d.netPolRefusal(testNamespace); !strings.Contains(err.Error(), clock.Format(time.RFC3339)) {
		t.Errorf("refusal %q does not name the check time %s", err, clock.Format(time.RFC3339))
	}

	c.phaseB.Store(1) // fixed
	d.recheckOnce(context.Background())
	if err := d.netPolRefusal(testNamespace); err != nil {
		t.Errorf("after a proving check: refusal %v, want none", err)
	}
	if enforced, _ := networkPolicyClaim(t, d); !enforced {
		t.Error("after a proving check: NetworkPolicy false, want true")
	}
	assertCanaryCleanedUp(t, c.cs)
}

// TestCanaryRecheck_AFlakeBetweenProofsChangesNothing: failures that never reach three in a row never
// move the verdict.
func TestCanaryRecheck_AFlakeBetweenProofsChangesNothing(t *testing.T) {
	c := newRecheckCluster(t)
	d, _ := c.boot(t, Config{})
	for _, phaseB := range []int32{0, 0, 1, 0, 0, 1, 0, 0} {
		c.phaseB.Store(phaseB)
		d.recheckOnce(context.Background())
		if err := d.netPolRefusal(testNamespace); err != nil {
			t.Fatalf("refusal %v after phase B exit %d, want none: no three failures in a row", err, phaseB)
		}
	}
}

// TestCanaryRecheck_IndeterminateBecomesNotProven: checks that cannot tell keep the verdict for three
// checks, then read as not proven, which refuses even under WARDYN_K8S_ALLOW_UNENFORCED_NETPOL (boot
// refuses an indeterminate canary under it too).
func TestCanaryRecheck_IndeterminateBecomesNotProven(t *testing.T) {
	c := newRecheckCluster(t)
	d, _ := c.boot(t, Config{AllowUnenforcedNetPol: true})
	c.phaseB.Store(128) // neither 0 nor 1: indeterminate
	for i := 0; i < canaryRecheckStrikes; i++ {
		if err := d.netPolRefusal(testNamespace); err != nil {
			t.Fatalf("after %d indeterminate check(s): refusal %v, want the proven verdict kept", i, err)
		}
		d.recheckOnce(context.Background())
	}
	if got := d.netPol.Load().byNS[testNamespace].state; got != netPolNotProven {
		t.Fatalf("verdict %v, want %v", got, netPolNotProven)
	}
	if err := d.netPolRefusal(testNamespace); !errors.Is(err, errNetPolNotProven) {
		t.Errorf("refusal %v, want errNetPolNotProven", err)
	}
	if enforced, acked := networkPolicyClaim(t, d); enforced || acked {
		t.Errorf("NetworkPolicy %v, acknowledged %v; want both false", enforced, acked)
	}
}

// TestCanaryRecheck_OverrideNeverCoversAgentSandbox: under WARDYN_K8S_ALLOW_UNENFORCED_NETPOL an
// unenforced verdict still lets a plain sandbox start, as boot does, but never an agent-sandbox one.
func TestCanaryRecheck_OverrideNeverCoversAgentSandbox(t *testing.T) {
	c := newRecheckCluster(t)
	d, _ := c.boot(t, Config{AllowUnenforcedNetPol: true})
	c.phaseB.Store(0)
	for i := 0; i < canaryRecheckStrikes; i++ {
		d.recheckOnce(context.Background())
	}
	if err := d.netPolRefusal(testNamespace); err != nil {
		t.Errorf("plain sandbox under the override: refusal %v, want none", err)
	}
	d.cfg.AgentSandbox = true // boot refuses agent-sandbox unless proven; the check alone is under test
	if err := d.netPolRefusal(testNamespace); !errors.Is(err, errNetPolNotProven) {
		t.Errorf("agent-sandbox under the override: refusal %v, want errNetPolNotProven", err)
	}
}

// TestCanaryRecheck_AcknowledgedRefusesAgentSandboxOnly: an acknowledged ambient default-deny lets a
// plain sandbox start, as boot does, but an agent-sandbox one needs proof.
func TestCanaryRecheck_AcknowledgedRefusesAgentSandboxOnly(t *testing.T) {
	c := newRecheckCluster(t)
	d, _ := c.boot(t, Config{AckAmbientDefaultDeny: true})
	c.phaseA.Store(1) // an ambient default-deny appears
	for i := 0; i < canaryRecheckStrikes; i++ {
		d.recheckOnce(context.Background())
	}
	if enforced, acked := networkPolicyClaim(t, d); enforced || !acked {
		t.Fatalf("NetworkPolicy %v, acknowledged %v; want false and true", enforced, acked)
	}
	if err := d.netPolRefusal(testNamespace); err != nil {
		t.Errorf("plain sandbox: refusal %v, want none", err)
	}
	d.cfg.AgentSandbox = true
	if err := d.netPolRefusal(testNamespace); !errors.Is(err, errNetPolNotProven) {
		t.Errorf("agent-sandbox: refusal %v, want errNetPolNotProven", err)
	}
}

// TestCanaryRecheck_PerNamespace: a failing namespace refuses its own new sandboxes only, health's
// aggregate reads false, and the aggregate check time is the oldest namespace's.
func TestCanaryRecheck_PerNamespace(t *testing.T) {
	c := newRecheckCluster(t)
	d, clock := c.boot(t, Config{Namespaces: []string{"runs-a", "runs-b"}})
	booted := *clock
	if !d.netPol.Load().enforced() {
		t.Fatal("boot did not prove both namespaces")
	}
	next := &netPolVerdict{byNS: map[string]nsVerdict{
		"runs-a": {state: netPolProven, checkedAt: booted},
		"runs-b": {state: netPolUnenforced, checkedAt: booted.Add(time.Minute)},
	}}
	d.netPol.Store(next)
	if err := d.netPolRefusal("runs-a"); err != nil {
		t.Errorf("runs-a: refusal %v, want none", err)
	}
	if err := d.netPolRefusal("runs-b"); !errors.Is(err, errNetPolNotProven) {
		t.Errorf("runs-b: refusal %v, want errNetPolNotProven", err)
	}
	if err := d.netPolRefusal("never-checked"); !errors.Is(err, errNetPolNotProven) {
		t.Errorf("unknown namespace: refusal %v, want errNetPolNotProven", err)
	}
	if enforced, _ := networkPolicyClaim(t, d); enforced {
		t.Error("NetworkPolicy true with one namespace unenforced")
	}
	if got := next.checkedAt(); !got.Equal(booted) {
		t.Errorf("checkedAt %v, want the oldest %v", got, booted)
	}
}

// TestCanaryRecheck_AcknowledgedNeverHidesAFailure: a namespace that failed its re-check is never
// reported as the milder "acknowledged" because another namespace is acknowledged.
func TestCanaryRecheck_AcknowledgedNeverHidesAFailure(t *testing.T) {
	v := &netPolVerdict{byNS: map[string]nsVerdict{"a": {state: netPolAcknowledged}, "b": {state: netPolNotProven}}}
	if v.acknowledged() || v.enforced() {
		t.Errorf("acknowledged %v enforced %v, want both false", v.acknowledged(), v.enforced())
	}
	var none *netPolVerdict
	if none.enforced() || none.acknowledged() || !none.checkedAt().IsZero() {
		t.Error("a nil verdict must prove nothing")
	}
}

// TestCanaryRecheck_LoopRunsUntilStopped: the loop re-runs the canary on its interval in every
// namespace, and stopping it returns once it has ended.
func TestCanaryRecheck_LoopRunsUntilStopped(t *testing.T) {
	old := canaryRecheckInterval
	canaryRecheckInterval = 5 * time.Millisecond
	t.Cleanup(func() { canaryRecheckInterval = old })

	c := newRecheckCluster(t)
	d, _ := c.boot(t, Config{})
	booted := c.pods.Load()
	d.startCanaryRecheck()
	deadline := time.Now().Add(30 * time.Second)
	for c.pods.Load() < booted+4 { // two re-checks, two phases each
		if time.Now().After(deadline) {
			d.stopCanaryRecheck()
			t.Fatalf("canary pods %d after 30s, want at least %d", c.pods.Load(), booted+4)
		}
		time.Sleep(time.Millisecond)
	}
	stopped := make(chan struct{})
	go func() { d.stopCanaryRecheck(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("stopCanaryRecheck did not return")
	}
	if err := d.netPolRefusal(testNamespace); err != nil {
		t.Errorf("refusal %v after proving re-checks, want none", err)
	}
}
