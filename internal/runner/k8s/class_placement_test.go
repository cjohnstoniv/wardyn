// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func isolatedToleration() corev1.Toleration {
	return corev1.Toleration{Key: "isolated", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule}
}

// poolScheduling is a RuntimeClass's scheduling for an isolated pool: its selector and its toleration.
func poolScheduling() *nodev1.Scheduling {
	return &nodev1.Scheduling{NodeSelector: map[string]string{"pool": "isolated"}, Tolerations: []corev1.Toleration{isolatedToleration()}}
}

func schedClass(name string, s *nodev1.Scheduling, overhead corev1.ResourceList) *nodev1.RuntimeClass {
	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Handler: "kata", Scheduling: s}
	if len(overhead) > 0 {
		rc.Overhead = &nodev1.Overhead{PodFixed: overhead}
	}
	return rc
}

// bootWith boots a Driver on a fake clientset holding objs, pinning CC3 (and CC2, when cfg names none) to
// the RuntimeClass "kata-sim", and returns what newWithClient answered.
func bootWith(t *testing.T, cfg Config, objs ...runtime.Object) (*Driver, *fake.Clientset, error) {
	t.Helper()
	cs := fake.NewClientset(objs...)
	installCanaryReactor(t, cs, false)
	installDeleteCollectionSupport(t, cs)
	if cfg.ConfinementRuntimes == nil {
		cfg.ConfinementRuntimes = map[types.ConfinementClass]string{types.CC3: "kata-sim"}
	}
	cfg.Namespaces, cfg.ProxyImage = []string{testNamespace}, "wardyn/wardyn-proxy:test"
	d, err := newWithClient(context.Background(), cs, testRestConfig(), cfg)
	return d, cs, err
}

func placementJSON(t *testing.T, p Placement) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestClassPlacement_MergesSchedulingAndOverhead(t *testing.T) {
	overhead := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("160Mi")}
	rcObj := schedClass("kata-sim", poolScheduling(), overhead)
	placement := Placement{NodeSelector: map[string]string{"zone": "a"}, Tolerations: []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "batch", Effect: corev1.TaintEffectNoSchedule}}}
	d, _, err := bootWith(t, Config{SandboxPlacement: placementJSON(t, placement)}, rcObj)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	got, err := d.classPlacement(context.Background(), types.CC3)
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeSelector["zone"] != "a" || got.NodeSelector["pool"] != "isolated" || len(got.NodeSelector) != 2 {
		t.Errorf("selector = %v, want the runner's zone=a plus pool=isolated", got.NodeSelector)
	}
	if len(got.Tolerations) != 2 || got.Tolerations[0].Key != "dedicated" || got.Tolerations[1] != isolatedToleration() {
		t.Errorf("tolerations = %v, want the runner's then the class's", got.Tolerations)
	}
	if cpu, mem := got.overhead[corev1.ResourceCPU], got.overhead[corev1.ResourceMemory]; cpu.Cmp(resource.MustParse("250m")) != 0 || mem.Cmp(resource.MustParse("160Mi")) != 0 {
		t.Errorf("overhead = %v, want 250m and 160Mi", got.overhead)
	}
	if d.placement.NodeSelector["pool"] != "" || len(d.placement.Tolerations) != 1 {
		t.Errorf("the runner's own placement changed: %+v", d.placement)
	}
}

func TestClassPlacement_CC1IsTheRunnersPlacement(t *testing.T) {
	placement := Placement{NodeSelector: map[string]string{"zone": "a"}}
	d, cs, err := bootWith(t, Config{SandboxPlacement: placementJSON(t, placement), ConfinementRuntimes: map[types.ConfinementClass]string{}})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	pinned, pcs, err := bootWith(t, Config{SandboxPlacement: placementJSON(t, placement)}, schedClass("kata-sim", poolScheduling(), nil))
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	cs.ClearActions()
	pcs.ClearActions()
	for _, tc := range []struct {
		name  string
		d     *Driver
		class types.ConfinementClass
	}{{"empty class", pinned, ""}, {"CC1", pinned, types.CC1}, {"CC3 with no pin", d, types.CC3}} {
		got, err := tc.d.classPlacement(context.Background(), tc.class)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(got.NodeSelector) != 1 || got.NodeSelector["zone"] != "a" || len(got.Tolerations) != 0 || len(got.overhead) != 0 {
			t.Errorf("%s: placement = %+v overhead %v, want the runner's own", tc.name, got.Placement, got.overhead)
		}
	}
	for _, c := range []*fake.Clientset{cs, pcs} {
		for _, a := range c.Actions() {
			if a.GetResource().Resource == "runtimeclasses" {
				t.Errorf("a RuntimeClass was read: %v", a)
			}
		}
	}
}

func TestCheckClassPlacement_RefusesASelectorConflict(t *testing.T) {
	arch := `{"nodeSelector":{"kubernetes.io/arch":"amd64"}}`
	conflict := schedClass("kata-sim", &nodev1.Scheduling{NodeSelector: map[string]string{"kubernetes.io/arch": "arm64"}, Tolerations: []corev1.Toleration{isolatedToleration()}}, nil)
	_, _, err := bootWith(t, Config{SandboxPlacement: arch}, conflict)
	if !errors.Is(err, errClassPlacement) || !strings.Contains(err.Error(), "sandbox.arch") {
		t.Fatalf("a class selecting another architecture: err = %v, want errClassPlacement naming sandbox.arch", err)
	}
	other := schedClass("kata-sim", &nodev1.Scheduling{NodeSelector: map[string]string{"pool": "b"}}, nil)
	if _, _, err := bootWith(t, Config{SandboxPlacement: `{"nodeSelector":{"pool":"a"}}`}, other); !errors.Is(err, errClassPlacement) || !strings.Contains(err.Error(), "k8s.sandbox.nodeSelector") {
		t.Fatalf("a conflicting key: err = %v, want errClassPlacement", err)
	}
	if _, _, err := bootWith(t, Config{SandboxPlacement: arch}, schedClass("kata-sim", poolScheduling(), nil)); err != nil {
		t.Fatalf("a free key must boot: %v", err)
	}
}

func tol(key string, op corev1.TolerationOperator, value string, effect corev1.TaintEffect) corev1.Toleration {
	return corev1.Toleration{Key: key, Operator: op, Value: value, Effect: effect}
}

func TestCheckClassPlacement_RefusesASharedTolerationKey(t *testing.T) {
	const eq, ex = corev1.TolerationOpEqual, corev1.TolerationOpExists
	const ns, ne = corev1.TaintEffectNoSchedule, corev1.TaintEffectNoExecute
	for _, tc := range []struct {
		name          string
		runner, class corev1.Toleration
		overlap       bool
	}{
		{"same key and value", tol("isolated", eq, "true", ns), tol("isolated", eq, "true", ns), true},
		{"runner Exists with an empty key", tol("", ex, "", ""), tol("isolated", eq, "true", ns), true},
		{"class Exists with an empty key", tol("dedicated", eq, "batch", ns), tol("", ex, "", ""), true},
		{"runner Exists on the class's key", tol("isolated", ex, "", ns), tol("isolated", eq, "true", ns), true},
		{"a comparison operator on a shared key", tol("gpu", corev1.TolerationOpGt, "8", ns), tol("gpu", eq, "16", ns), true},
		{"an empty effect tolerates every effect", tol("isolated", eq, "true", ""), tol("isolated", eq, "true", ne), true},
		{"same key, other value", tol("dedicated", eq, "batch", ns), tol("dedicated", eq, "barrier", ns), false},
		{"same key and value, other effects", tol("isolated", eq, "true", ns), tol("isolated", eq, "true", ne), false},
		{"a different key", tol("dedicated", eq, "batch", ns), tol("isolated", eq, "true", ns), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tolerationsOverlap(tc.runner, tc.class); got != tc.overlap {
				t.Errorf("tolerationsOverlap = %v, want %v", got, tc.overlap)
			}
			if got := tolerationsOverlap(tc.class, tc.runner); got != tc.overlap {
				t.Errorf("tolerationsOverlap reversed = %v, want %v", got, tc.overlap)
			}
			class := schedClass("kata-sim", &nodev1.Scheduling{NodeSelector: map[string]string{"pool": "isolated"}, Tolerations: []corev1.Toleration{tc.class}}, nil)
			_, _, err := bootWith(t, Config{SandboxPlacement: placementJSON(t, Placement{Tolerations: []corev1.Toleration{tc.runner}})}, class)
			if tc.overlap {
				if !errors.Is(err, errClassPlacement) {
					t.Fatalf("err = %v, want errClassPlacement", err)
				}
			} else if err != nil {
				t.Fatalf("boot: %v", err)
			}
		})
	}
}

func TestCheckClassPlacement_MissingClassBootsAndUnreadableRefuses(t *testing.T) {
	d, _, err := bootWith(t, Config{})
	if err != nil {
		t.Fatalf("a pinned class that does not exist must boot: %v", err)
	}
	if len(d.anyNodeClasses) != 0 || len(d.untaintedPools) != 0 {
		t.Errorf("a missing class reported any-node %v untainted %v", d.anyNodeClasses, d.untaintedPools)
	}

	cs := fake.NewClientset()
	installCanaryReactor(t, cs, false)
	cs.PrependReactor("get", "runtimeclasses", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "runtimeclasses"}, "kata-sim", errors.New("no get"))
	})
	_, err = newWithClient(context.Background(), cs, testRestConfig(), Config{Namespaces: []string{testNamespace}, ProxyImage: "p:t",
		ConfinementRuntimes: map[types.ConfinementClass]string{types.CC3: "kata-sim"}})
	if err == nil || !strings.Contains(err.Error(), "read the CC3 RuntimeClass") {
		t.Fatalf("err = %v, want a refusal naming the unreadable CC3 RuntimeClass", err)
	}
}

func TestRunPods_AgentOnClassPlacementProxyOnRunners(t *testing.T) {
	overhead := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("160Mi")}
	d, _, err := bootWith(t, Config{SandboxPlacement: `{"nodeSelector":{"zone":"a"}}`}, schedClass("kata-sim", poolScheduling(), overhead))
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	base := resourceRequirements(fitRes)
	res := fitRes
	res.ConfinementClass = types.CC3
	pods, err := d.runPods(context.Background(), res)
	if err != nil || len(pods) != 2 {
		t.Fatalf("runPods = %v, %v", pods, err)
	}
	agent, proxy := pods[0], pods[1]
	if agent.placement.NodeSelector["pool"] != "isolated" || len(agent.placement.Tolerations) != 1 {
		t.Errorf("agent placement = %+v, want the class's pool and toleration", agent.placement)
	}
	wantCPU, wantMem := base.Requests[corev1.ResourceCPU], base.Requests[corev1.ResourceMemory]
	wantCPU.Add(resource.MustParse("250m"))
	wantMem.Add(resource.MustParse("160Mi"))
	if got := agent.requests[corev1.ResourceCPU]; got.Cmp(wantCPU) != 0 {
		t.Errorf("agent cpu request = %v, want %v", got.String(), wantCPU.String())
	}
	if got := agent.requests[corev1.ResourceMemory]; got.Cmp(wantMem) != 0 {
		t.Errorf("agent memory request = %v, want %v", got.String(), wantMem.String())
	}
	if proxy.placement.NodeSelector["pool"] != "" || len(proxy.placement.Tolerations) != 0 || proxy.placement.NodeSelector["zone"] != "a" {
		t.Errorf("proxy placement = %+v, want the runner's own", proxy.placement)
	}

	// withOverhead: a request is raised where the pod sets none; a limit only where the pod sets one.
	req, lim := withOverhead(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}, corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}, overhead)
	if m := req[corev1.ResourceMemory]; m.Cmp(resource.MustParse("160Mi")) != 0 {
		t.Errorf("memory request with none set = %v, want the overhead's 160Mi", m.String())
	}
	if c := lim[corev1.ResourceCPU]; c.Cmp(resource.MustParse("2250m")) != 0 {
		t.Errorf("cpu limit = %v, want 2250m", c.String())
	}
	if _, set := lim[corev1.ResourceMemory]; set {
		t.Errorf("a memory limit the pod did not set was created: %v", lim)
	}
}

func TestCheckFit_IsolatedPoolFullShowsAShortfall(t *testing.T) {
	d, cs, err := bootWith(t, Config{ReadNodes: true}, schedClass("kata-sim", poolScheduling(), nil))
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	seed(t, cs,
		readyNode(node("general", 16000, 32768, map[string]string{})),
		readyNode(node("isolated", 500, 1024, map[string]string{"pool": "isolated"}, corev1.Taint{Key: "isolated", Value: "true", Effect: corev1.TaintEffectNoSchedule})))
	d.nodeCache.at = time.Time{}
	cc3 := fitRes
	cc3.ConfinementClass = types.CC3
	fit, err := d.CheckFit(context.Background(), cc3)
	if err != nil || fit.Nodes != runner.ReadOK || fit.NodeShortfall == nil || fit.NodeShortfall.CPUMillis != 1000 {
		t.Fatalf("CC3 fit = %+v, %v; want a shortfall of the agent's 1000m on the too-small isolated node", fit, err)
	}
	cc1 := fitRes
	cc1.ConfinementClass = types.CC1
	if fit, err = d.CheckFit(context.Background(), cc1); err != nil || fit.NodeShortfall != nil {
		t.Fatalf("CC1 fit = %+v, %v; want no shortfall: the general node holds both pods", fit, err)
	}
}

func TestStartClock_AgentPodWithClassSelectorMatchingNoNodeIsUnplaceable(t *testing.T) {
	pod := &corev1.Pod{
		Spec:   corev1.PodSpec{NodeSelector: map[string]string{"pool": "isolated"}, Tolerations: []corev1.Toleration{isolatedToleration()}},
		Status: unschedulableStatus("0/1 nodes are available: Insufficient cpu."),
	}
	general := readyNode(node("general", 16000, 32768, map[string]string{}))
	isolated := readyNode(node("isolated", 16000, 32768, map[string]string{"pool": "isolated"}, corev1.Taint{Key: "isolated", Value: "true", Effect: corev1.TaintEffectNoSchedule}))
	for _, tc := range []struct {
		name        string
		nodes       []*corev1.Node
		wantWaiting bool
	}{
		{"no node carries the class's pool", []*corev1.Node{general}, false},
		{"an isolated node exists and is only full", []*corev1.Node{general, isolated}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs, err := bootWith(t, Config{ReadNodes: true, CapacityWait: time.Minute}, schedClass("kata-sim", poolScheduling(), nil))
			if err != nil {
				t.Fatalf("boot: %v", err)
			}
			for _, n := range tc.nodes {
				seed(t, cs, n)
			}
			d.nodeCache.at = time.Time{}
			if got := d.newStartClock().observe(pod); got != tc.wantWaiting {
				t.Errorf("observe = %v, want %v", got, tc.wantWaiting)
			}
		})
	}
}

func TestProxyAndCanaryCarryNoClassToleration(t *testing.T) {
	d, cs, err := bootWith(t, Config{SandboxPlacement: `{"nodeSelector":{"zone":"a"}}`}, schedClass("kata-sim", poolScheduling(), nil))
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	clean := func(what string, spec corev1.PodSpec) {
		t.Helper()
		if _, set := spec.NodeSelector["pool"]; set || spec.NodeSelector["zone"] != "a" {
			t.Errorf("%s node selector = %v, want the runner's own", what, spec.NodeSelector)
		}
		for _, tl := range spec.Tolerations {
			if tl.Key == "isolated" {
				t.Errorf("%s carries the class's toleration %v", what, tl)
			}
		}
	}
	canaries := 0
	for _, a := range cs.Actions() {
		if ca, ok := a.(clienttesting.CreateAction); ok && ca.GetResource().Resource == "pods" {
			if p, ok := ca.GetObject().(*corev1.Pod); ok && strings.HasPrefix(p.Name, "wardyn-egress-canary-") {
				clean("canary pod", p.Spec)
				canaries++
			}
		}
	}
	if canaries == 0 {
		t.Fatal("no canary pod was created at boot")
	}
	proxy, err := d.buildProxyPod(proxyPodInputs{RunID: uuid.New(), Namespace: testNamespace, SecretName: "s", ControlPlaneURL: "https://wardyn.example:8443"})
	if err != nil {
		t.Fatal(err)
	}
	clean("proxy pod", proxy.Spec)
}

func TestCheckClassPlacement_ToleratedPoolWithNoSelectorRefuses(t *testing.T) {
	for _, class := range []types.ConfinementClass{types.CC2, types.CC3} {
		cfg := Config{ConfinementRuntimes: map[types.ConfinementClass]string{class: "kata-sim"}}
		noSel := schedClass("kata-sim", &nodev1.Scheduling{Tolerations: []corev1.Toleration{isolatedToleration()}}, nil)
		_, _, err := bootWith(t, cfg, noSel)
		if !errors.Is(err, errClassPlacement) || !strings.Contains(err.Error(), "scheduling.nodeSelector") || !strings.Contains(err.Error(), "kubernetes.io/os: linux") {
			t.Fatalf("%s: err = %v, want errClassPlacement naming scheduling.nodeSelector and kubernetes.io/os: linux", class, err)
		}
	}
	for _, tc := range []struct {
		name  string
		s     *nodev1.Scheduling
		wantN int
	}{{"nil scheduling", nil, 1}, {"empty scheduling", &nodev1.Scheduling{}, 1}, {"a selector", &nodev1.Scheduling{NodeSelector: map[string]string{"kubernetes.io/os": "linux"}}, 0}} {
		d, _, err := bootWith(t, Config{}, schedClass("kata-sim", tc.s, nil))
		if err != nil {
			t.Fatalf("%s: boot: %v", tc.name, err)
		}
		if len(d.anyNodeClasses) != tc.wantN || (tc.wantN == 1 && d.anyNodeClasses[0] != types.CC3) {
			t.Errorf("%s: anyNodeClasses = %v, want %d entry", tc.name, d.anyNodeClasses, tc.wantN)
		}
	}
}

func TestUntaintedPools_LabelledUntaintedNodeWarns(t *testing.T) {
	pool := map[string]string{"pool": "isolated"}
	taint := corev1.Taint{Key: "isolated", Value: "true", Effect: corev1.TaintEffectNoSchedule}
	class := schedClass("kata-sim", poolScheduling(), nil)
	for _, tc := range []struct {
		name string
		cfg  Config
		node *corev1.Node
		want int
	}{
		{"labelled, no taint", Config{ReadNodes: true}, readyNode(node("n", 4000, 8192, pool)), 1},
		{"labelled and tainted", Config{ReadNodes: true}, readyNode(node("n", 4000, 8192, pool, taint)), 0},
		{"node read off", Config{}, readyNode(node("n", 4000, 8192, pool)), 0},
	} {
		d, _, err := bootWith(t, tc.cfg, class, tc.node)
		if err != nil {
			t.Fatalf("%s: boot: %v", tc.name, err)
		}
		if len(d.untaintedPools) != tc.want || (tc.want == 1 && d.untaintedPools[0] != types.CC3) {
			t.Errorf("%s: untaintedPools = %v, want %d entry", tc.name, d.untaintedPools, tc.want)
		}
	}

	// The runner's own pods cannot use a node its selector excludes, so that pool is no warning. A selector
	// on the pool's own key with another value is refused at boot instead (TestCheckClassPlacement_RefusesASelectorConflict).
	d, _, err := bootWith(t, Config{ReadNodes: true, SandboxPlacement: `{"nodeSelector":{"zone":"general"}}`}, class, readyNode(node("n", 4000, 8192, pool)))
	if err != nil || len(d.untaintedPools) != 0 {
		t.Errorf("a node the runner's selector excludes: err %v untaintedPools %v, want none", err, d.untaintedPools)
	}

	// An unreadable node list is no answer, never a warning.
	cs := fake.NewClientset(class)
	installCanaryReactor(t, cs, false)
	cs.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("no list"))
	})
	d, err = newWithClient(context.Background(), cs, testRestConfig(), Config{Namespaces: []string{testNamespace}, ProxyImage: "p:t", ReadNodes: true,
		ConfinementRuntimes: map[types.ConfinementClass]string{types.CC3: "kata-sim"}})
	if err != nil || len(d.untaintedPools) != 0 {
		t.Errorf("forbidden node read: err %v untaintedPools %v, want none", err, d.untaintedPools)
	}
}

func TestClasses_ReportsUntaintedPools(t *testing.T) {
	pool := map[string]string{"pool": "isolated"}
	taint := corev1.Taint{Key: "isolated", Value: "true", Effect: corev1.TaintEffectNoSchedule}
	for _, tc := range []struct {
		name               string
		s                  *nodev1.Scheduling
		node               *corev1.Node
		wantPool, wantOpen bool
	}{
		{"untainted pool", poolScheduling(), readyNode(node("n", 4000, 8192, pool)), true, false},
		{"tainted pool", poolScheduling(), readyNode(node("n", 4000, 8192, pool, taint)), false, false},
		{"no scheduling", nil, readyNode(node("n", 4000, 8192, pool)), false, true},
	} {
		d, _, err := bootWith(t, Config{ReadNodes: true}, schedClass("kata-sim", tc.s, nil), tc.node)
		if err != nil {
			t.Fatalf("%s: boot: %v", tc.name, err)
		}
		cls, err := d.Classes(context.Background())
		if err != nil {
			t.Fatalf("%s: Classes: %v", tc.name, err)
		}
		if got := containsClass(cls.UntaintedPools, types.CC3); got != tc.wantPool {
			t.Errorf("%s: UntaintedPools = %v, want CC3 present: %v", tc.name, cls.UntaintedPools, tc.wantPool)
		}
		if got := containsClass(cls.AnyNodeClasses, types.CC3); got != tc.wantOpen {
			t.Errorf("%s: AnyNodeClasses = %v, want CC3 present: %v", tc.name, cls.AnyNodeClasses, tc.wantOpen)
		}
	}
}
