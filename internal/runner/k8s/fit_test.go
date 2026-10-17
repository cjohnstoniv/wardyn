// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// rl builds a ResourceList: cpu in millicores, memory in MiB.
func rl(cpuMillis, memMiB int64) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(cpuMillis, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(memMiB*1024*1024, resource.BinarySI),
	}
}

func quota(name string, hard, used corev1.ResourceList) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       corev1.ResourceQuotaSpec{Hard: hard},
		Status:     corev1.ResourceQuotaStatus{Hard: hard, Used: used},
	}
}

func seed(t *testing.T, cs *fake.Clientset, objs ...runtime.Object) {
	t.Helper()
	for _, o := range objs {
		if err := cs.Tracker().Add(o); err != nil {
			t.Fatalf("seed %T: %v", o, err)
		}
	}
}

// fitRes is a run of 1 CPU and 2Gi. With the proxy pod's 500m/256Mi the run asks for 1500m and
// 2304Mi of requests.
var fitRes = runner.Resources{CPUMillis: 1000, MemoryMiB: 2048}

func axisOf(q runner.QuotaFit, key string) (runner.QuotaAxis, bool) {
	for _, a := range q.Axes {
		if a.Key == key {
			return a, true
		}
	}
	return runner.QuotaAxis{}, false
}

// TestCheckFit_TwoApplicableQuotas: every quota that applies to a pod is reported, each with what
// BOTH pods add to it; a quota whose scope selects no run pod is not.
func TestCheckFit_TwoApplicableQuotas(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	limits := rl(8000, 16384)
	seed(t, cs,
		quota("zz-compute", corev1.ResourceList{
			corev1.ResourceRequestsCPU: limits[corev1.ResourceCPU], corev1.ResourceRequestsMemory: limits[corev1.ResourceMemory],
		}, corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("5"), corev1.ResourceRequestsMemory: resource.MustParse("4Gi")}),
		quota("aa-limits", corev1.ResourceList{
			corev1.ResourceLimitsCPU: limits[corev1.ResourceCPU], corev1.ResourcePods: *resource.NewQuantity(10, resource.DecimalSI),
		}, corev1.ResourceList{corev1.ResourceLimitsCPU: resource.MustParse("1"), corev1.ResourcePods: resource.MustParse("3")}),
		func() runtime.Object {
			q := quota("terminating-only", rl(1000, 1024), rl(0, 0))
			q.Spec.Scopes = []corev1.ResourceQuotaScope{corev1.ResourceQuotaScopeTerminating}
			return q
		}(),
		quota("unrelated-objects", corev1.ResourceList{"count/secrets": resource.MustParse("5")}, nil),
	)

	fit, err := d.CheckFit(context.Background(), fitRes)
	if err != nil {
		t.Fatal(err)
	}
	if fit.Quotas != runner.ReadOK || fit.Nodes != runner.ReadSkipped {
		t.Fatalf("states = %q/%q, want ok/skipped (the node read is off)", fit.Quotas, fit.Nodes)
	}
	if len(fit.Quota) != 2 || fit.Quota[0].Name != "aa-limits" || fit.Quota[1].Name != "zz-compute" {
		t.Fatalf("applicable quotas = %+v, want aa-limits then zz-compute (Terminating-only and the object-count quota do not apply)", fit.Quota)
	}
	compute := fit.Quota[1]
	if a, _ := axisOf(compute, "requests.cpu"); a.Need != 1500 || a.Left != 3000 || a.Hard != 8000 {
		t.Errorf("requests.cpu = %+v, want need 1500 (agent+proxy), left 3000, hard 8000", a)
	}
	if a, _ := axisOf(compute, "requests.memory"); a.Need != (2048+256)*mi || a.Left != (16384-4096)*mi {
		t.Errorf("requests.memory = %+v, want need 2304Mi, left 12288Mi", a)
	}
	lim := fit.Quota[0]
	if a, _ := axisOf(lim, "limits.cpu"); a.Need != 1500 || a.Left != 7000 {
		t.Errorf("limits.cpu = %+v, want need 1500, left 7000", a)
	}
	if a, _ := axisOf(lim, "pods"); a.Need != 2 || a.Left != 7 {
		t.Errorf("pods = %+v, want need 2 (the agent and the proxy), left 7", a)
	}
}

const mi = int64(1) << 20

// TestCheckFit_PriorityClassScope: a scopeSelector on PriorityClass applies only to a run whose
// placement sets that class.
func TestCheckFit_PriorityClassScope(t *testing.T) {
	sel := func(name string, vals ...string) *corev1.ResourceQuota {
		q := quota(name, rl(4000, 8192), rl(0, 0))
		q.Spec.ScopeSelector = &corev1.ScopeSelector{MatchExpressions: []corev1.ScopedResourceSelectorRequirement{
			{ScopeName: corev1.ResourceQuotaScopePriorityClass, Operator: corev1.ScopeSelectorOpIn, Values: vals},
		}}
		q.Spec.Hard = corev1.ResourceList{corev1.ResourceRequestsCPU: q.Spec.Hard[corev1.ResourceCPU]}
		q.Status.Hard = q.Spec.Hard
		return q
	}
	d, cs := newTestDriver(t, Config{SandboxPlacement: `{"priorityClassName":"sandbox-low"}`})
	seed(t, cs, sel("matches", "sandbox-low"), sel("other-class", "system-x"))
	fit, err := d.CheckFit(context.Background(), fitRes)
	if err != nil {
		t.Fatal(err)
	}
	if len(fit.Quota) != 1 || fit.Quota[0].Name != "matches" {
		t.Fatalf("quotas = %+v, want only the one scoped to sandbox-low", fit.Quota)
	}

	d, cs = newTestDriver(t, Config{})
	seed(t, cs, sel("matches", "sandbox-low"))
	if fit, _ = d.CheckFit(context.Background(), fitRes); len(fit.Quota) != 0 {
		t.Fatalf("quotas = %+v, want none for a run with no PriorityClass", fit.Quota)
	}
}

// TestCheckFit_EmptyForbiddenUnavailableAreDistinct is the "each reported as itself" property for
// the quota read.
func TestCheckFit_EmptyForbiddenUnavailableAreDistinct(t *testing.T) {
	list := func(err error) runner.Fit {
		d, cs := newTestDriver(t, Config{})
		if err != nil {
			cs.PrependReactor("list", "resourcequotas", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, err })
		}
		fit, cerr := d.CheckFit(context.Background(), fitRes)
		if cerr != nil {
			t.Fatalf("CheckFit must report a read failure as a state, got error %v", cerr)
		}
		return fit
	}
	gr := schema.GroupResource{Resource: "resourcequotas"}
	if f := list(nil); f.Quotas != runner.ReadOK || len(f.Quota) != 0 {
		t.Errorf("empty: %+v, want ok with no quotas", f)
	}
	if f := list(apierrors.NewForbidden(gr, "", errors.New("no list"))); f.Quotas != runner.ReadForbidden || len(f.Quota) != 0 {
		t.Errorf("forbidden: %+v, want forbidden", f)
	}
	if f := list(apierrors.NewServiceUnavailable("etcd down")); f.Quotas != runner.ReadUnavailable {
		t.Errorf("unavailable: %+v, want unavailable", f)
	}
}

func node(name string, cpuMillis, memMiB int64, labels map[string]string, taints ...corev1.Taint) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       corev1.NodeSpec{Taints: taints},
		Status:     corev1.NodeStatus{Allocatable: rl(cpuMillis, memMiB)},
	}
}

// TestCheckFit_NodesForbiddenIsItsOwnResult: the node read is off unless asked for; when on, a
// forbidden list is reported as such and does not touch the quota half.
func TestCheckFit_NodesForbiddenIsItsOwnResult(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	nodeLists := 0
	cs.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) { nodeLists++; return false, nil, nil })
	if fit, _ := d.CheckFit(context.Background(), fitRes); fit.Nodes != runner.ReadSkipped || nodeLists != 0 {
		t.Fatalf("flag off: nodes=%q lists=%d, want skipped and no node list", fit.Nodes, nodeLists)
	}

	d, cs = newTestDriver(t, Config{ReadNodes: true})
	cs.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("no list"))
	})
	fit, err := d.CheckFit(context.Background(), fitRes)
	if err != nil {
		t.Fatal(err)
	}
	if fit.Nodes != runner.ReadForbidden || fit.NodeShortfall != nil || fit.Quotas != runner.ReadOK {
		t.Fatalf("nodes forbidden: %+v, want nodes=forbidden, quotas=ok, no shortfall", fit)
	}
}

// TestCheckFit_NodeShortfallHonoursPlacement: size is judged only on the nodes the placement
// allows, a pod no allowed node holds is reported, and the node list is read once per TTL.
func TestCheckFit_NodeShortfallHonoursPlacement(t *testing.T) {
	// A big node the placement excludes (wrong label, and tainted), and a small allowed one.
	big := node("big", 32000, 65536, map[string]string{"pool": "general"}, corev1.Taint{Key: "gpu", Effect: corev1.TaintEffectNoSchedule})
	small := node("small", 1200, 4096, map[string]string{"pool": "sandbox"})
	d, cs := newTestDriver(t, Config{ReadNodes: true, SandboxPlacement: `{"nodeSelector":{"pool":"sandbox"}}`})
	lists := 0
	cs.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) { lists++; return false, nil, nil })
	seed(t, cs, big, small)

	fit, err := d.CheckFit(context.Background(), fitRes)
	if err != nil {
		t.Fatal(err)
	}
	if fit.Nodes != runner.ReadOK {
		t.Fatalf("nodes = %q, want ok", fit.Nodes)
	}
	// The agent asks 1000m/2048Mi and the proxy 500m/256Mi: the one allowed node, small, holds each.
	if fit.NodeShortfall != nil {
		t.Fatalf("shortfall = %+v, want none: the small allowed node holds both pods", fit.NodeShortfall)
	}

	// A run bigger than every allowed node, though the excluded node is large enough.
	d.nodeCache.at = time.Time{}
	fit, _ = d.CheckFit(context.Background(), runner.Resources{CPUMillis: 4000, MemoryMiB: 8192})
	if fit.NodeShortfall == nil || fit.NodeShortfall.CPUMillis != 4000 || fit.NodeShortfall.MemoryBytes != 8192*mi {
		t.Fatalf("shortfall = %+v, want the agent's 4000m/8192Mi", fit.NodeShortfall)
	}

	// Within the TTL a second check reuses the list.
	before := lists
	d.CheckFit(context.Background(), fitRes) //nolint:errcheck // only the list count is read
	if lists != before {
		t.Fatalf("node lists = %d after a cached check, want %d", lists, before)
	}
}

// TestNodeHolds_TolerationsAndAffinity: a taint blocks unless tolerated, and required node
// affinity is honoured.
func TestNodeHolds_TolerationsAndAffinity(t *testing.T) {
	pod := podDemand{requests: rl(500, 256)}
	tainted := nodeInfo{name: "n", labels: map[string]string{"zone": "a"}, allocatable: rl(4000, 8192),
		taints: []corev1.Taint{{Key: "sandbox", Value: "true", Effect: corev1.TaintEffectNoSchedule}}}
	if tainted.holds(pod, Placement{}) {
		t.Error("an untolerated NoSchedule taint admitted the pod")
	}
	tol := Placement{Tolerations: []corev1.Toleration{{Key: "sandbox", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule}}}
	if !tainted.holds(pod, tol) {
		t.Error("a tolerated taint refused the pod")
	}
	aff := func(zone string) Placement {
		return Placement{Tolerations: tol.Tolerations, Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{
				{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{zone}}}},
			}}}}}
	}
	if !tainted.holds(pod, aff("a")) || tainted.holds(pod, aff("b")) {
		t.Error("required node affinity was not honoured")
	}
}
