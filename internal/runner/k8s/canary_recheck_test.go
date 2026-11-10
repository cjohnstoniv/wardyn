// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
)

// recheckCluster is a fake cluster the canary can run on any number of times: a canary pod exits with
// phaseA while no canary deny-all policy is in its namespace and with phaseB while one is. Set either
// mid-test to change what the next check finds.
type recheckCluster struct {
	cs     *fake.Clientset
	phaseA atomic.Int32
	phaseB atomic.Int32
	pods   atomic.Int32 // canary pods created
	// only, when set for a namespace, replaces phaseA and phaseB there.
	only sync.Map // namespace -> [2]int32
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
		codes := [2]int32{c.phaseA.Load(), c.phaseB.Load()}
		if v, ok := c.only.Load(a.GetNamespace()); ok {
			codes = v.([2]int32)
		}
		code := codes[0]
		if phaseB {
			code = codes[1]
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
	clock := time.Now().UTC().Truncate(time.Second)
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

// TestCanaryRecheck_FirstEscapeRefusesAtOnce: the first check showing that a deny-all policy leaks
// refuses new sandboxes, reads NetworkPolicy false and stops the image check at once, carrying its own
// time; the first check that proves enforcement again restores all three.
func TestCanaryRecheck_FirstEscapeRefusesAtOnce(t *testing.T) {
	c := newRecheckCluster(t)
	d, clock := c.boot(t, Config{})
	if err := d.netPolRefusal(testNamespace); err != nil {
		t.Fatalf("after a proving boot: refusal %v, want none", err)
	}

	c.phaseB.Store(0) // the CNI stops enforcing
	*clock = clock.Add(canaryRecheckInterval)
	d.recheckOnce(context.Background())
	err := d.netPolRefusal(testNamespace)
	if !errors.Is(err, errNetPolNotProven) || !strings.Contains(err.Error(), clock.Format(time.RFC3339)) {
		t.Fatalf("after one escape: refusal %v, want errNetPolNotProven naming %s", err, clock.Format(time.RFC3339))
	}
	if enforced, _ := networkPolicyClaim(t, d); enforced {
		t.Error("after one escape: NetworkPolicy true, want false")
	}
	if d.PrewrappedCheckUnavailable() == "" {
		t.Error("after one escape: the image check is available, want a reason")
	}
	if got, want := healthCheck(t, d), (runner.NetPolCheck{Verdict: runner.NetPolRefused, At: *clock}); got != want {
		t.Errorf("after one escape: health %+v, want %+v", got, want)
	}

	c.phaseB.Store(1) // fixed
	d.recheckOnce(context.Background())
	if err := d.netPolRefusal(testNamespace); err != nil {
		t.Errorf("after a proving check: refusal %v, want none", err)
	}
	if enforced, _ := networkPolicyClaim(t, d); !enforced || d.PrewrappedCheckUnavailable() != "" {
		t.Error("after a proving check: NetworkPolicy false or the image check unavailable, want both restored")
	}
	assertCanaryCleanedUp(t, c.cs)
}

// TestCanaryRecheck_AFlakeBetweenProofsChangesNothing: checks that could not tell, never three in a
// row, never move the verdict.
func TestCanaryRecheck_AFlakeBetweenProofsChangesNothing(t *testing.T) {
	c := newRecheckCluster(t)
	d, _ := c.boot(t, Config{})
	for _, phaseB := range []int32{128, 128, 1, 128, 128, 1, 128, 128} { // 128: neither 0 nor 1, indeterminate
		c.phaseB.Store(phaseB)
		d.recheckOnce(context.Background())
		if err := d.netPolRefusal(testNamespace); err != nil {
			t.Fatalf("refusal %v after phase B exit %d, want none: no three indeterminate checks in a row", err, phaseB)
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
	d.recheckOnce(context.Background())
	if err := d.netPolRefusal(testNamespace); err != nil {
		t.Errorf("plain sandbox under the override: refusal %v, want none", err)
	}
	if got := healthCheck(t, d).Verdict; got != runner.NetPolUnenforced {
		t.Errorf("plain sandbox under the override: health verdict %q, want %q", got, runner.NetPolUnenforced)
	}
	d.cfg.AgentSandbox = true // boot refuses agent-sandbox unless proven; the check alone is under test
	if err := d.netPolRefusal(testNamespace); !errors.Is(err, errNetPolNotProven) {
		t.Errorf("agent-sandbox under the override: refusal %v, want errNetPolNotProven", err)
	}
	// Health says what happens: the override covers no agent-sandbox create, so they are refused.
	if got := healthCheck(t, d).Verdict; got != runner.NetPolRefused {
		t.Errorf("agent-sandbox under the override: health verdict %q, want %q", got, runner.NetPolRefused)
	}
}

// TestCanaryRecheck_AcknowledgedRefusesAgentSandboxOnly: an acknowledged ambient default-deny lets a
// plain sandbox start, as boot does, but an agent-sandbox one needs proof.
func TestCanaryRecheck_AcknowledgedRefusesAgentSandboxOnly(t *testing.T) {
	c := newRecheckCluster(t)
	d, _ := c.boot(t, Config{AckAmbientDefaultDeny: true})
	c.phaseA.Store(1) // an ambient default-deny appears; after a proven boot it takes three checks to apply
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
	if got := healthCheck(t, d).Verdict; got != runner.NetPolNotProven {
		t.Errorf("agent-sandbox: health verdict %q, want %q", got, runner.NetPolNotProven)
	}
}

// TestCanaryRecheck_AcknowledgedNeverLiftsAnEscape: with the ambient default-deny acknowledged, no
// result after one that showed an escape lets new sandboxes start again, in plain and agent-sandbox
// mode alike, except a check that proves enforcement. Acknowledged results (phase A exits 1, as a
// pod-network blip also does) keep the refused verdict and its time for two checks, read not proven at
// the third, and stay not proven at the fourth and fifth. After three checks that cannot tell, an
// acknowledged result still refuses.
func TestCanaryRecheck_AcknowledgedNeverLiftsAnEscape(t *testing.T) {
	const ack, blocked, connected, neither = 1, 1, 0, 128 // pod exit codes: phase A, then phase B
	for _, agentSandbox := range []bool{false, true} {
		c := newRecheckCluster(t)
		d, clock := c.boot(t, Config{AckAmbientDefaultDeny: true})
		d.cfg.AgentSandbox = agentSandbox // the check alone is under test, as in OverrideNeverCoversAgentSandbox
		check := func(phaseA, phaseB int32) {
			c.phaseA.Store(phaseA)
			c.phaseB.Store(phaseB)
			*clock = clock.Add(canaryRecheckInterval)
			d.recheckOnce(context.Background())
		}
		refused := func(step string, want runner.NetPolCheck) {
			t.Helper()
			if err := d.netPolRefusal(testNamespace); !errors.Is(err, errNetPolNotProven) {
				t.Fatalf("agent-sandbox %v, %s: refusal %v, want errNetPolNotProven", agentSandbox, step, err)
			}
			if got := healthCheck(t, d); got != want {
				t.Fatalf("agent-sandbox %v, %s: health %+v, want %+v", agentSandbox, step, got, want)
			}
		}

		check(0, connected) // the CNI stops enforcing
		escaped := *clock
		for i := 1; i <= canaryRecheckStrikes+2; i++ {
			check(ack, blocked)
			want := runner.NetPolCheck{Verdict: runner.NetPolRefused, At: escaped}
			if i >= canaryRecheckStrikes {
				want = runner.NetPolCheck{Verdict: runner.NetPolNotProven, At: *clock}
			}
			refused(fmt.Sprintf("escape, then %d acknowledged check(s)", i), want)
		}

		check(0, connected) // a second escape, then checks that cannot tell
		for i := 0; i < canaryRecheckStrikes; i++ {
			check(0, neither)
		}
		check(ack, blocked)
		refused("escape, three checks that cannot tell, then an acknowledged one", runner.NetPolCheck{Verdict: runner.NetPolNotProven, At: *clock})

		check(0, blocked) // proof of enforcement, the only result that lifts it
		if err := d.netPolRefusal(testNamespace); err != nil {
			t.Errorf("agent-sandbox %v, after a proving check: refusal %v, want none", agentSandbox, err)
		}
		if got, want := healthCheck(t, d), (runner.NetPolCheck{Verdict: runner.NetPolEnforced, At: *clock}); got != want {
			t.Errorf("agent-sandbox %v, after a proving check: health %+v, want %+v", agentSandbox, got, want)
		}
	}
}

// TestCanaryRecheck_AcknowledgedBlipKeepsProof: booted proven with the ambient default-deny
// acknowledged, a phase A pod that exits 1 fewer than three times in a row keeps the proven verdict,
// its time and the image check, in plain and agent-sandbox mode alike; three make it acknowledged.
func TestCanaryRecheck_AcknowledgedBlipKeepsProof(t *testing.T) {
	for _, agentSandbox := range []bool{false, true} {
		c := newRecheckCluster(t)
		d, clock := c.boot(t, Config{AckAmbientDefaultDeny: true})
		d.cfg.AgentSandbox = agentSandbox // the check alone is under test, as in OverrideNeverCoversAgentSandbox
		d.recheckOnce(context.Background())
		proven := *clock
		c.phaseA.Store(1)
		for i := 1; i < canaryRecheckStrikes; i++ {
			*clock = clock.Add(canaryRecheckInterval)
			d.recheckOnce(context.Background())
			if err := d.netPolRefusal(testNamespace); err != nil {
				t.Fatalf("agent-sandbox %v, after %d acknowledged check(s): refusal %v, want none", agentSandbox, i, err)
			}
			if got, want := healthCheck(t, d), (runner.NetPolCheck{Verdict: runner.NetPolEnforced, At: proven}); got != want {
				t.Fatalf("agent-sandbox %v, after %d acknowledged check(s): health %+v, want %+v", agentSandbox, i, got, want)
			}
			if reason := d.PrewrappedCheckUnavailable(); reason != "" {
				t.Fatalf("agent-sandbox %v, after %d acknowledged check(s): image check unavailable (%s), want available", agentSandbox, i, reason)
			}
		}
		*clock = clock.Add(canaryRecheckInterval)
		d.recheckOnce(context.Background())
		// Health names what happens: agent-sandbox mode refuses on an acknowledged verdict, so it reads
		// not proven there, the same refusal the setup row and the console then show.
		want := runner.NetPolCheck{Verdict: runner.NetPolAcknowledged, At: *clock}
		if agentSandbox {
			want.Verdict = runner.NetPolNotProven
		}
		if got := healthCheck(t, d); got != want {
			t.Errorf("agent-sandbox %v, after %d acknowledged checks: health %+v, want %+v", agentSandbox, canaryRecheckStrikes, got, want)
		}
		if err := d.netPolRefusal(testNamespace); (err != nil) != agentSandbox {
			t.Errorf("agent-sandbox %v, after %d acknowledged checks: refusal %v, want one only in agent-sandbox mode", agentSandbox, canaryRecheckStrikes, err)
		}
	}
}

// TestCanaryRecheck_PerNamespace: a failing namespace refuses its own new sandboxes only, and health
// reads the worst namespace's verdict with that verdict's own time.
func TestCanaryRecheck_PerNamespace(t *testing.T) {
	c := newRecheckCluster(t)
	d, clock := c.boot(t, Config{Namespaces: []string{"runs-a", "runs-b"}})
	booted := *clock
	if !d.netPol.Load().enforced() {
		t.Fatal("boot did not prove both namespaces")
	}
	next := &netPolVerdict{byNS: map[string]nsVerdict{
		"runs-a": {state: netPolProven, verdictAt: booted},
		"runs-b": {state: netPolUnenforced, verdictAt: booted.Add(time.Minute)},
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
	if got, want := d.netPolCheck(), (runner.NetPolCheck{Verdict: runner.NetPolRefused, At: booted.Add(time.Minute)}); got != want {
		t.Errorf("health check %+v, want the worst namespace's %+v", got, want)
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
	if none.enforced() || none.acknowledged() {
		t.Error("a nil verdict must prove nothing")
	}
}

// TestCanaryRecheck_LoopStopsWithItsOwner: the loop re-runs the canary on its interval in every
// namespace, and the owner's Close (wardynd's orchestrator) returns once it has ended: no canary pod is
// created after it.
func TestCanaryRecheck_LoopStopsWithItsOwner(t *testing.T) {
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
			_ = d.Close()
			t.Fatalf("canary pods %d after 30s, want at least %d", c.pods.Load(), booted+4)
		}
		time.Sleep(time.Millisecond)
	}
	owner := orchestrator.New(d)
	stopped := make(chan error, 1)
	go func() { stopped <- owner.Close() }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("owner Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the owner's Close did not return")
	}
	after := c.pods.Load()
	time.Sleep(20 * canaryRecheckInterval)
	if got := c.pods.Load(); got != after {
		t.Errorf("canary pods %d after Close, then %d: the loop still runs", after, got)
	}
	if err := d.netPolRefusal(testNamespace); err != nil {
		t.Errorf("refusal %v after proving re-checks, want none", err)
	}
}

// TestCanaryRecheck_CreateSandboxRefusedAfterFailedRecheck: CreateSandbox asks the live verdict first,
// so after one check showing an escape it refuses before it creates a single object.
func TestCanaryRecheck_CreateSandboxRefusedAfterFailedRecheck(t *testing.T) {
	c := newRecheckCluster(t)
	d, _ := c.boot(t, Config{})
	c.phaseB.Store(0)
	d.recheckOnce(context.Background())
	if _, err := d.CreateSandbox(context.Background(), testSandboxSpec()); !errors.Is(err, errNetPolNotProven) {
		t.Fatalf("CreateSandbox: %v, want errNetPolNotProven", err)
	}
	ctx := context.Background()
	if nps, _ := c.cs.NetworkingV1().NetworkPolicies(testNamespace).List(ctx, metav1.ListOptions{}); len(nps.Items) != 0 {
		t.Errorf("%d NetworkPolicies left after a refused create, want none", len(nps.Items))
	}
	if secrets, _ := c.cs.CoreV1().Secrets(testNamespace).List(ctx, metav1.ListOptions{}); len(secrets.Items) != 0 {
		t.Errorf("%d Secrets left after a refused create, want none", len(secrets.Items))
	}
}

// healthCheck is what Classes reports for health.
func healthCheck(t *testing.T, d *Driver) runner.NetPolCheck {
	t.Helper()
	cls, err := d.Classes(context.Background())
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	return cls.NetworkPolicyCheck
}

// TestCanaryRecheck_VerdictKeepsTheTimeOfTheCheckThatProducedIt: proven at T1, two indeterminate checks
// at T2 and T3 leave it proven at T1; the third at T4 makes it not proven, at T4. A check that agrees
// moves the time.
func TestCanaryRecheck_VerdictKeepsTheTimeOfTheCheckThatProducedIt(t *testing.T) {
	c := newRecheckCluster(t)
	d, clock := c.boot(t, Config{})
	d.recheckOnce(context.Background())
	t1 := *clock
	if got, want := healthCheck(t, d), (runner.NetPolCheck{Verdict: runner.NetPolEnforced, At: t1}); got != want {
		t.Fatalf("at T1: %+v, want %+v", got, want)
	}
	c.phaseB.Store(128)
	for i := 2; i <= 3; i++ {
		*clock = clock.Add(canaryRecheckInterval)
		d.recheckOnce(context.Background())
		if got, want := healthCheck(t, d), (runner.NetPolCheck{Verdict: runner.NetPolEnforced, At: t1}); got != want {
			t.Fatalf("after the indeterminate check at T%d: %+v, want %+v", i, got, want)
		}
	}
	*clock = clock.Add(canaryRecheckInterval)
	d.recheckOnce(context.Background())
	if got, want := healthCheck(t, d), (runner.NetPolCheck{Verdict: runner.NetPolNotProven, At: *clock}); got != want {
		t.Fatalf("after the third indeterminate check at T4: %+v, want %+v", got, want)
	}
	c.phaseB.Store(1)
	*clock = clock.Add(canaryRecheckInterval)
	d.recheckOnce(context.Background())
	if got, want := healthCheck(t, d), (runner.NetPolCheck{Verdict: runner.NetPolEnforced, At: *clock}); got != want {
		t.Errorf("after a proving check: %+v, want %+v", got, want)
	}
}

// TestCanaryRecheck_NotProvenAfterThreeIndeterminate: three checks that could not tell read
// "not_proven" for health, at the third.
func TestCanaryRecheck_NotProvenAfterThreeIndeterminate(t *testing.T) {
	c := newRecheckCluster(t)
	d, clock := c.boot(t, Config{})
	c.phaseB.Store(128)
	for i := 0; i < canaryRecheckStrikes; i++ {
		*clock = clock.Add(canaryRecheckInterval)
		d.recheckOnce(context.Background())
	}
	if got, want := healthCheck(t, d), (runner.NetPolCheck{Verdict: runner.NetPolNotProven, At: *clock}); got != want {
		t.Errorf("%+v, want %+v", got, want)
	}
}

// TestCanaryRecheck_BootOverrideIsNotAFailedRecheck: an unenforced verdict under
// WARDYN_K8S_ALLOW_UNENFORCED_NETPOL reads "unenforced" (the operator's choice), never "refused".
func TestCanaryRecheck_BootOverrideIsNotAFailedRecheck(t *testing.T) {
	c := newRecheckCluster(t)
	c.phaseB.Store(0)
	d, _ := c.boot(t, Config{AllowUnenforcedNetPol: true})
	for i := 0; i < canaryRecheckStrikes; i++ {
		d.recheckOnce(context.Background())
	}
	cls, err := d.Classes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if cls.NetworkPolicy || cls.NetworkPolicyCheck.Verdict != runner.NetPolUnenforced {
		t.Errorf("NetworkPolicy %v, verdict %q; want false and %q", cls.NetworkPolicy, cls.NetworkPolicyCheck.Verdict, runner.NetPolUnenforced)
	}
}

// TestCanaryRecheck_AcknowledgedBesideUnenforcedReadsUnenforced: one namespace acknowledged and another
// unenforced under the override reads as unenforced, never the milder acknowledged.
func TestCanaryRecheck_AcknowledgedBesideUnenforcedReadsUnenforced(t *testing.T) {
	c := newRecheckCluster(t)
	c.only.Store("runs-a", [2]int32{1, 1}) // phase A blocked: an ambient default-deny, acknowledged
	c.only.Store("runs-b", [2]int32{0, 0}) // phase B connected: not enforced, overridden
	d, _ := c.boot(t, Config{Namespaces: []string{"runs-a", "runs-b"}, AckAmbientDefaultDeny: true, AllowUnenforcedNetPol: true})
	enforced, acked := networkPolicyClaim(t, d)
	if enforced || acked {
		t.Errorf("NetworkPolicy %v, acknowledged %v; want both false", enforced, acked)
	}
	if got := healthCheck(t, d).Verdict; got != runner.NetPolUnenforced {
		t.Errorf("health verdict %q, want %q", got, runner.NetPolUnenforced)
	}
}

// stallingCleanupAPI is an API server for the canary that creates objects and reports every canary pod
// exited 0, but takes each DELETE and never answers it until the client gives up.
func stallingCleanupAPI(t *testing.T) (kubernetes.Interface, *atomic.Int32) {
	t.Helper()
	var deletes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			deletes.Add(1)
			<-r.Context().Done()
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.Copy(w, r.Body) // the created object is what was sent
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pods/"):
			_ = json.NewEncoder(w).Encode(&corev1.Pod{
				TypeMeta:   metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
				ObjectMeta: metav1.ObjectMeta{Name: path.Base(r.URL.Path), Namespace: testNamespace},
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
					Name:  canaryContainerName,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
				}}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL, ContentConfig: rest.ContentConfig{
		ContentType: "application/json", AcceptContentTypes: "application/json",
	}})
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	return cs, &deletes
}

// TestCanaryCleanup_StalledDeleteIsBounded: an API server that takes the canary's cleanup deletes and
// never answers holds a check only canaryCleanupTimeout per delete, so the next check, and the
// driver's Close, are never held up for good.
func TestCanaryCleanup_StalledDeleteIsBounded(t *testing.T) {
	oldCleanup, oldInterval := canaryCleanupTimeout, canaryRecheckInterval
	canaryCleanupTimeout, canaryRecheckInterval = 50*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { canaryCleanupTimeout, canaryRecheckInterval = oldCleanup, oldInterval })

	c := newRecheckCluster(t)
	d, _ := c.boot(t, Config{})
	cs, deletes := stallingCleanupAPI(t)
	d.clientset = cs

	checked := make(chan struct{})
	go func() {
		d.recheckOnce(context.Background())
		close(checked)
	}()
	select {
	case <-checked:
	case <-time.After(30 * time.Second):
		t.Fatal("a check whose cleanup deletes never answer did not return")
	}
	if got := deletes.Load(); got < 3 { // phase A's pod, phase B's policy and pod
		t.Fatalf("cleanup deletes %d reached the server, want 3", got)
	}

	d.startCanaryRecheck()
	before := deletes.Load()
	deadline := time.Now().Add(30 * time.Second)
	for deletes.Load() == before { // a loop check is now inside a stalled delete
		if time.Now().After(deadline) {
			t.Fatal("the loop never reached a cleanup delete")
		}
		time.Sleep(time.Millisecond)
	}
	closed := make(chan struct{})
	go func() {
		_ = d.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return while a cleanup delete stalled")
	}
}
