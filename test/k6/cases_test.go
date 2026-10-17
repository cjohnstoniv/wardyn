// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k6_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/keystore"
)

var bg = context.Background()

func short() string { return uuid.NewString()[:8] }

func statusCode(err error) int32 {
	var s apierrors.APIStatus
	if errors.As(err, &s) {
		return s.Status().Code
	}
	return 0
}

func jsonPatch(t *testing.T, ops ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// testPod is a PSS-restricted busybox pod in the runs namespace, placed by the scheduling label.
func (e *env) testPod(name, sa string, finalizers []string, script string) *corev1.Pod {
	no, yes, uid, grace := false, true, int64(65534), int64(2)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.runsNS, Finalizers: finalizers, Labels: map[string]string{"k6.wardyn.dev/test": "true"}},
		Spec: corev1.PodSpec{
			ServiceAccountName: sa, AutomountServiceAccountToken: &no, TerminationGracePeriodSeconds: &grace,
			NodeSelector: map[string]string{schedLabel: "on"},
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &yes, RunAsUser: &uid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{{Name: "c", Image: e.agentImage, Command: []string{"sh", "-c", script},
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}},
		},
	}
}

func (e *env) dropTestPod(t *testing.T, name string) {
	t.Cleanup(func() {
		zero := int64(0)
		_, _ = e.admin.CoreV1().Pods(e.runsNS).Patch(bg, name, k8stypes.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{})
		_ = e.admin.CoreV1().Pods(e.runsNS).Delete(bg, name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	})
}

// stopRun is a normal end: StopProxy, then label teardown, then every object gone.
func (e *env) stopRun(t *testing.T, id uuid.UUID) {
	t.Helper()
	if err := e.main.StopProxy(bg, ref(id)); err != nil {
		t.Fatalf("StopProxy %s: %v", id, err)
	}
	if err := e.main.StopSandbox(bg, ref(id)); err != nil {
		t.Fatalf("StopSandbox %s: %v", id, err)
	}
	e.waitRunGone(t, id, goneWait)
}

// --- cases --------------------------------------------------------------------------------------

func caseNodeIdentity(t *testing.T, e *env) {
	nodes, err := e.admin.CoreV1().Nodes().List(bg, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	machine := map[string]string{}
	uuids := map[string]bool{}
	var versions []string
	for _, n := range nodes.Items {
		info := n.Status.NodeInfo
		if info.MachineID == "" {
			t.Errorf("node %s reports no machineID", n.Name)
		}
		if prev, dup := machine[info.MachineID]; dup {
			t.Errorf("nodes %s and %s share machineID %s", prev, n.Name, info.MachineID)
		}
		machine[info.MachineID] = n.Name
		uuids[info.SystemUUID] = true
		versions = append(versions, n.Name+"="+info.KubeletVersion)
	}
	note(t, "%d nodes, machineIDs distinct; %d distinct systemUUIDs (kind may share them); kubelets %s",
		len(nodes.Items), len(uuids), strings.Join(versions, ","))
}

func caseBootChecks(t *testing.T, e *env) {
	d := e.main
	if err := d.CheckRunnerAccess(bg, e.ownNS); err != nil {
		t.Errorf("CheckRunnerAccess under the rendered Role: %v", err)
	}
	if err := d.CheckKubeletFloor(bg, e.arch); err != nil {
		t.Errorf("CheckKubeletFloor: %v", err)
	}
	if err := d.CheckForeignObjects(bg); err != nil {
		t.Errorf("CheckForeignObjects on an empty runs namespace: %v", err)
	}
	pol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "k6-foreign", Labels: map[string]string{"wardyn.managed": "true", runner.LabelRunnerID: uuid.NewString()}},
		Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"k6": "none"}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}},
	}
	if _, err := e.admin.NetworkingV1().NetworkPolicies(e.runsNS).Create(bg, pol, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	err := d.CheckForeignObjects(bg)
	_ = e.admin.NetworkingV1().NetworkPolicies(e.runsNS).Delete(bg, pol.Name, metav1.DeleteOptions{})
	if err == nil || !strings.Contains(err.Error(), "networkpolicy/k6-foreign") {
		t.Errorf("another runner's NetworkPolicy was not refused by name: %v", err)
	}
	if err := d.CheckForeignObjects(bg); err != nil {
		t.Errorf("CheckForeignObjects after the foreign object went: %v", err)
	}
	// A pod naming runnerd's ServiceAccount in the runs namespace is refused.
	probe := e.testPod("k6-sa-probe-"+short(), e.saName, nil, "sleep 5")
	if p, err := e.sa.CoreV1().Pods(e.runsNS).Create(bg, probe, metav1.CreateOptions{}); err == nil {
		e.dropTestPod(t, p.Name)
		t.Errorf("a pod naming runnerd's ServiceAccount %s was admitted in %s", e.saName, e.runsNS)
	} else {
		note(t, "pod naming runnerd's SA refused (%d)", statusCode(err))
	}
	// What the rendered Role must not grant (design §8 security checklist; Fable item 7).
	for _, a := range []authorizationv1.ResourceAttributes{
		{Namespace: e.runsNS, Verb: "get", Resource: "secrets"},
		{Namespace: e.runsNS, Verb: "list", Resource: "secrets"},
		{Namespace: e.ownNS, Verb: "list", Resource: "secrets"},
		{Namespace: e.runsNS, Verb: "update", Resource: "pods"},
		{Namespace: "kube-system", Verb: "list", Resource: "pods"},
		{Verb: "create", Group: "rbac.authorization.k8s.io", Resource: "clusterrolebindings"},
		{Verb: "patch", Resource: "nodes"},
	} {
		r, err := e.sa.AuthorizationV1().SelfSubjectAccessReviews().Create(bg, &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &a}}, metav1.CreateOptions{})
		if err != nil {
			t.Errorf("SSAR %+v: %v", a, err)
			continue
		}
		if r.Status.Allowed {
			t.Errorf("runnerd's Role allows %s %s in %q", a.Verb, a.Resource, a.Namespace)
		}
	}
	note(t, "SSAR, foreign-object and kubelet-floor checks under runnerd's own token; 7 denials held")
}

func caseKeystore(t *testing.T, e *env) {
	ks := keystore.NewK8s(e.sa, e.ownNS, e.runnerID)
	acct := "k6-" + short()
	for _, v := range []string{"v1", "v2"} {
		if err := ks.Set(acct, v); err != nil {
			t.Fatalf("Set %s under the Role: %v", v, err)
		}
		got, err := ks.Get(acct)
		if err != nil || got != v {
			t.Fatalf("Get = %q, %v; want %q", got, err, v)
		}
	}
	name := keystore.SecretName(acct)
	sec, err := e.admin.CoreV1().Secrets(e.ownNS).Get(bg, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("keystore Secret %s not in runnerd's namespace: %v", name, err)
	}
	if sec.Labels[runner.LabelRunnerID] != e.runnerID {
		t.Errorf("keystore Secret labels %v", sec.Labels)
	}
	if err := ks.Delete(acct); err != nil {
		t.Fatalf("Delete under the Role: %v", err)
	}
	if _, err := e.admin.CoreV1().Secrets(e.ownNS).Get(bg, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("keystore Secret still present after Delete: %v", err)
	}
	if _, err := ks.Get(acct); err == nil {
		t.Errorf("Get after Delete returned a value")
	}
	note(t, "Set (create, then replace), Get, Delete under the own-namespace Role")
}

func caseFinalizerAPI(t *testing.T, e *env) {
	e.setSched(t, e.pin)
	name := "k6-fin-" + short()
	if _, err := e.admin.CoreV1().Pods(e.runsNS).Create(bg, e.testPod(name, "", []string{stopProof, thirdPartyFinal}, "sleep 3600"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.dropTestPod(t, name)
	running := e.waitPod(t, name, 2*time.Minute, "Running", func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodRunning })
	zero := int64(0)
	if err := e.sa.CoreV1().Pods(e.runsNS).Delete(bg, name, metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
		t.Fatalf("force delete under the Role: %v", err)
	}
	held := e.waitPod(t, name, time.Minute, "marked for deletion", func(p *corev1.Pod) bool { return p.DeletionTimestamp != nil })
	note(t, "force delete left the object held (finalizers %v)", held.Finalizers)

	remove0 := map[string]any{"op": "remove", "path": "/metadata/finalizers/0"}
	_, err := e.sa.CoreV1().Pods(e.runsNS).Patch(bg, name, k8stypes.JSONPatchType,
		jsonPatch(t, map[string]any{"op": "test", "path": "/metadata/uid", "value": "not-" + string(running.UID)}, remove0), metav1.PatchOptions{})
	if !apierrors.IsInvalid(err) {
		t.Errorf("failed uid test op: want 422, got %d (%v)", statusCode(err), err)
	}
	_, err = e.sa.CoreV1().Pods(e.runsNS).Patch(bg, name, k8stypes.JSONPatchType,
		jsonPatch(t, map[string]any{"op": "test", "path": "/metadata/resourceVersion", "value": running.ResourceVersion}, remove0), metav1.PatchOptions{})
	if !apierrors.IsInvalid(err) {
		t.Errorf("stale resourceVersion test op: want 422, got %d (%v)", statusCode(err), err)
	}
	_, err = e.sa.CoreV1().Pods(e.runsNS).Patch(bg, name, k8stypes.MergePatchType,
		[]byte(fmt.Sprintf(`{"metadata":{"resourceVersion":%q,"finalizers":[%q]}}`, running.ResourceVersion, thirdPartyFinal)), metav1.PatchOptions{})
	if !apierrors.IsConflict(err) {
		t.Errorf("changed version precondition: want 409, got %d (%v)", statusCode(err), err)
	}
	cur, err := e.admin.CoreV1().Pods(e.runsNS).Get(bg, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	idx := slices.Index(cur.Finalizers, stopProof)
	at := fmt.Sprintf("/metadata/finalizers/%d", idx)
	if _, err := e.sa.CoreV1().Pods(e.runsNS).Patch(bg, name, k8stypes.JSONPatchType, jsonPatch(t,
		map[string]any{"op": "test", "path": "/metadata/uid", "value": string(cur.UID)},
		map[string]any{"op": "test", "path": "/metadata/resourceVersion", "value": cur.ResourceVersion},
		map[string]any{"op": "test", "path": at, "value": stopProof},
		map[string]any{"op": "remove", "path": at}), metav1.PatchOptions{}); err != nil {
		t.Fatalf("the driver-shaped guarded patch was refused: %v", err)
	}
	after, err := e.admin.CoreV1().Pods(e.runsNS).Get(bg, name, metav1.GetOptions{})
	if err != nil || !slices.Equal(after.Finalizers, []string{thirdPartyFinal}) {
		t.Fatalf("after the guarded patch: finalizers %v (%v), want only the third party's", after.Finalizers, err)
	}
	note(t, "uid test 422, stale-version test 422, version precondition 409, third-party finalizer kept")
}

func caseNormalStop(t *testing.T, e *env) {
	d := e.main
	id := uuid.New()
	e.cleanupRun(t, id)
	e.setSched(t, e.pin)
	if _, err := d.CreateSandbox(bg, e.spec(id)); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	for _, n := range []string{proxyName(id), ref(id)} {
		p, err := e.admin.CoreV1().Pods(e.runsNS).Get(bg, n, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if a := p.Spec.AutomountServiceAccountToken; a == nil || *a {
			t.Errorf("pod %s may mount a ServiceAccount token", n)
		}
		for _, v := range p.Spec.Volumes {
			if v.Projected != nil {
				for _, s := range v.Projected.Sources {
					if s.ServiceAccountToken != nil {
						t.Errorf("pod %s mounts a ServiceAccount token volume", n)
					}
				}
			}
		}
		if !slices.Contains(p.Finalizers, stopProof) {
			t.Errorf("pod %s lacks the stop-proof finalizer: %v", n, p.Finalizers)
		}
	}
	if _, err := e.admin.CoreV1().Pods(e.runsNS).Patch(bg, proxyName(id), k8stypes.JSONPatchType,
		jsonPatch(t, map[string]any{"op": "add", "path": "/metadata/finalizers/-", "value": thirdPartyFinal}), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	pw := e.watchProxy(t, id)
	t0 := time.Now()
	if err := d.StopProxy(bg, ref(id)); err != nil {
		t.Fatalf("StopProxy on a healthy node: %v", err)
	}
	took := time.Since(t0)
	if took > normalStopBound+5*time.Second {
		t.Errorf("normal stop took %s, past the 20 s bound", took)
	}
	ev := e.evidence(t, id)
	p := ev.Pods[proxyComponent]
	if p == nil || p.Witness == nil || p.Witness.Arm != "terminal" || !p.FinalizerOff {
		t.Fatalf("proxy evidence after a normal stop: %+v", p)
	}
	for _, c := range p.Witness.Containers {
		if c.Tier == "" || c.ContainerID == "" {
			t.Errorf("container %s: tier %q, containerID %q (a real exit carries its containerID)", c.Name, c.Tier, c.ContainerID)
		}
	}
	held, err := e.admin.CoreV1().Pods(e.runsNS).Get(bg, proxyName(id), metav1.GetOptions{})
	if err != nil || !slices.Equal(held.Finalizers, []string{thirdPartyFinal}) {
		t.Errorf("third party's finalizer: %v (%v)", held.Finalizers, err)
	}
	_, last := pw.snapshot()
	if last != nil {
		for _, cs := range last.Status.ContainerStatuses {
			if cs.State.Terminated == nil || cs.State.Terminated.ContainerID == "" {
				t.Errorf("kubelet status for %s: %+v, want terminated with a containerID", cs.Name, cs.State)
			}
		}
	}
	if ip, ok := e.fences.ipOf(id); !ok || ip != p.PodIP {
		t.Errorf("relay fence: %q (recorded %v), want the proxy IP %s", ip, ok, p.PodIP)
	}
	if !e.fences.wasReleased(id) || e.fences.fenced(p.PodIP) {
		t.Errorf("the IP refusal for %s was not released at witness save (Fable H2)", p.PodIP)
	}
	e.stoppedIP = p.PodIP
	note(t, "StopProxy %s, arm terminal, assoc %s, kubelet %s", took.Round(100*time.Millisecond), p.Assoc, e.kubelet(t))
	if _, err := e.admin.CoreV1().Pods(e.runsNS).Patch(bg, proxyName(id), k8stypes.JSONPatchType, jsonPatch(t,
		map[string]any{"op": "test", "path": "/metadata/finalizers/0", "value": thirdPartyFinal},
		map[string]any{"op": "remove", "path": "/metadata/finalizers/0"}), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.StopSandbox(bg, ref(id)); err != nil {
		t.Fatalf("StopSandbox: %v", err)
	}
	e.waitRunGone(t, id, goneWait)
	if n := e.sink.goneCount(id); n != 1 {
		t.Errorf("producer settled %d times, want once", n)
	}
}

func (e *env) kubelet(t *testing.T) string {
	n, err := e.admin.CoreV1().Nodes().Get(bg, e.pin, metav1.GetOptions{})
	if err != nil {
		return "?"
	}
	return n.Status.NodeInfo.KubeletVersion
}

// casePodIPReuse is Fable item 3: after a normal stop the next proxy on the node may receive the
// freed IP and still starts (the refusal was released at witness save); a run still pending keeps
// its refusal, so a proxy that receives a pending run's IP is refused with relay_ip_fenced.
func casePodIPReuse(t *testing.T, e *env) {
	if e.stoppedIP == "" {
		inconclusive(t, "normal-stop left no stopped proxy IP")
	}
	e.setSched(t, e.pin)
	if !e.reuseIP(t, e.stoppedIP, false) {
		inconclusive(t, fmt.Sprintf("no later proxy on %s received the stopped IP %s in 6 runs", e.pin, e.stoppedIP))
	}
	// A pending run: a third party strips the proxy's finalizer and force-deletes it, so the object is
	// gone without a witness. NotFound is not evidence, so the run stays pending and fenced.
	id := uuid.New()
	e.cleanupRun(t, id)
	if _, err := e.main.CreateSandbox(bg, e.spec(id)); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	ip := e.evidence(t, id).Pods[proxyComponent].PodIP
	zero := int64(0)
	if _, err := e.admin.CoreV1().Pods(e.runsNS).Patch(bg, proxyName(id), k8stypes.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := e.admin.CoreV1().Pods(e.runsNS).Delete(bg, proxyName(id), metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
		t.Fatal(err)
	}
	e.podGone(t, proxyName(id), time.Minute)
	ctx, cancel := context.WithTimeout(bg, 90*time.Second)
	defer cancel()
	if err := e.main.StopProxy(ctx, ref(id)); err == nil {
		t.Fatalf("StopProxy completed on a NotFound proxy")
	}
	if !e.fences.fenced(ip) {
		t.Fatalf("the pending run's IP %s is not refused", ip)
	}
	if !e.reuseIP(t, ip, true) {
		note(t, "pending half inconclusive: no later proxy received the pending run's IP %s in 6 runs", ip)
	}
}

// reuseIP creates runs on the pin node until a proxy receives ip (at most 6). With refused false
// that run must start; with refused true it must fail with the relay fence. It reports whether
// the IP came back at all.
func (e *env) reuseIP(t *testing.T, ip string, refused bool) bool {
	t.Helper()
	for i := range 6 {
		id := uuid.New()
		e.cleanupRun(t, id)
		_, err := e.main.CreateSandbox(bg, e.spec(id))
		got := ""
		if p := e.evidence(t, id).Pods[proxyComponent]; p != nil {
			got = p.PodIP
		}
		switch {
		case err != nil && strings.Contains(err.Error(), ip) && refused:
			note(t, "run %d's proxy received the pending run's IP %s and was refused: %v", i, ip, err)
			e.waitRunGone(t, id, goneWait)
			return true
		case err != nil && strings.Contains(err.Error(), ip):
			t.Fatalf("run %d: the next proxy received the stopped run's IP %s and was refused: %v (Fable H2)", i, ip, err)
		case err != nil:
			t.Fatalf("run %d: CreateSandbox: %v", i, err)
		case got == ip && refused:
			t.Fatalf("run %d: a proxy on the pending run's IP %s started", i, ip)
		case got == ip:
			note(t, "run %d's proxy reused the stopped IP %s and started", i, ip)
			e.stopRun(t, id)
			return true
		}
		e.stopRun(t, id)
	}
	return false
}

func caseH1Window(t *testing.T, e *env) {
	e.setSched(t, e.pin)
	id := uuid.New()
	e.cleanupRun(t, id)
	ctx, cancel := context.WithTimeout(bg, createWait)
	defer cancel()
	ch := e.startRun(ctx, e.main, id)
	seen := map[runner.ProxyObservation]int{}
	var createErr error
loop:
	for {
		select {
		case createErr = <-ch:
			break loop
		default:
		}
		obs, err := e.main.ObserveProxies(ctx, []uuid.UUID{id})
		if err == nil {
			if o, ok := obs[id]; ok {
				seen[o]++
			} else {
				seen["absent"]++
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if createErr != nil {
		t.Fatalf("CreateSandbox under concurrent 30 s lists: %v", createErr)
	}
	note(t, "observations during create: %v", seen)
	if seen[runner.ProxyNotFound] > 0 {
		t.Errorf("the 30 s list answered not_found %d times for a run mid-create; the manager would contain it (Fable H1)", seen[runner.ProxyNotFound])
	}
	e.stopRun(t, id)
}

// waitDeleting waits until the watch has seen the proxy pod marked for deletion.
func waitDeleting(t *testing.T, pw *podWatch, within time.Duration) {
	t.Helper()
	if err := poll(bg, within, func(context.Context) (bool, error) {
		obs, _ := pw.snapshot()
		return slices.ContainsFunc(obs, func(o podObs) bool { return o.deleting }), nil
	}); err != nil {
		t.Fatalf("the proxy pod was never marked for deletion within %s", within)
	}
}

func caseBindDeleteSerial(t *testing.T, e *env) {
	id := uuid.New()
	e.cleanupRun(t, id)
	e.setSched(t, "") // no node satisfies the selector
	t.Cleanup(func() { e.setSched(t, e.pin) })
	pw := e.watchProxy(t, id)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := e.startRun(ctx, e.main, id)
	e.waitPod(t, proxyName(id), time.Minute, "created and unbound", func(p *corev1.Pod) bool { return p.Spec.NodeName == "" })
	cancel() // runnerd deletes the pending pod
	waitDeleting(t, pw, time.Minute)
	e.setSched(t, e.pin) // then the selector becomes satisfiable
	err := waitErr(t, ch, createWait, "CreateSandbox")
	e.waitRunGone(t, id, goneWait)
	obs, _ := pw.snapshot()
	for _, o := range obs {
		if o.node != "" {
			t.Errorf("the deleted pod acquired nodeName %s", o.node)
		}
	}
	ev := e.evidence(t, id)
	if ev.arm(proxyComponent) != "unbound_deleted" || !ev.Pods[proxyComponent].FinalizerOff {
		t.Errorf("proxy arm %q (finalizer off %v), want unbound_deleted", ev.arm(proxyComponent), ev.Pods[proxyComponent].FinalizerOff)
	}
	note(t, "%d reads, never bound, arm %s, finalizer removed, objects gone (create: %v)", len(obs), ev.arm(proxyComponent), err)
}

func caseBindDeleteOverlap(t *testing.T, e *env) {
	deltas := []time.Duration{-200, -100, -50, -20, 0, 20, 50, 100, 200, 400}
	seen := map[string]int{}
	for i := range overlapIterations {
		if seen["delete-first"] > 0 && seen["bind-first"] > 0 && i >= 4 {
			break
		}
		order := e.overlapOnce(t, i, deltas[i%len(deltas)]*time.Millisecond)
		if t.Failed() {
			return
		}
		seen[order]++
	}
	note(t, "winning orders %v", seen)
	if seen["delete-first"] == 0 || seen["bind-first"] == 0 {
		t.Errorf("both winning orders were not seen in %d iterations: %v", overlapIterations, seen)
	}
}

// overlapOnce races the scheduler's Binding (the pin node is uncordoned at t0) with runnerd's delete
// (CreateSandbox cancelled at t0+delta). The node is in the before-list from the start, so a pod that
// binds has a causal association.
func (e *env) overlapOnce(t *testing.T, i int, delta time.Duration) string {
	t.Helper()
	id := uuid.New()
	e.cleanupRun(t, id)
	e.setSched(t, e.pin)
	e.cordon(t, e.pin, true)
	defer e.cordon(t, e.pin, false)
	pw := e.watchProxy(t, id)
	defer pw.stop()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := e.startRun(ctx, e.main, id)
	e.waitPod(t, proxyName(id), time.Minute, "created and unbound", func(p *corev1.Pod) bool { return p.Spec.NodeName == "" })
	if delta >= 0 {
		e.cordon(t, e.pin, false)
		time.Sleep(delta)
		cancel()
	} else {
		cancel()
		time.Sleep(-delta)
		e.cordon(t, e.pin, false)
	}
	if err := waitErr(t, ch, createWait, "CreateSandbox"); err == nil {
		e.stopRun(t, id) // the bind won by a wide margin and the run started
	}
	e.waitRunGone(t, id, goneWait)
	obs, _ := pw.snapshot()
	order, violation := classify(obs)
	ev := e.evidence(t, id)
	arm := ev.arm(proxyComponent)
	assoc := ""
	if p := ev.Pods[proxyComponent]; p != nil {
		assoc = p.Assoc
	}
	note(t, "iter %d delta %s: %s, arm %s, assoc %s, %d reads", i, delta, order, arm, assoc, len(obs))
	if violation {
		t.Errorf("iter %d: a read showed deletionTimestamp without nodeName, then a later nodeName", i)
	}
	switch order {
	case "delete-first":
		if arm != "unbound_deleted" {
			t.Errorf("iter %d: delete won but arm is %q", i, arm)
		}
	case "bind-first":
		if arm == "" || arm == "unbound_deleted" {
			t.Errorf("iter %d: bind won but arm is %q", i, arm)
		}
	default:
		t.Errorf("iter %d: no read of the proxy pod was observed", i)
	}
	return order
}

// containerdRef is the proxy image as the node's containerd names it.
func (e *env) containerdRef() string {
	if first, _, ok := strings.Cut(e.proxyImage, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return e.proxyImage
	}
	return "docker.io/" + e.proxyImage
}

// withoutProxyImage removes the proxy image from the pin node, so its next pull fails as a wrong tag
// would, and loads it back when the case ends.
func (e *env) withoutProxyImage(t *testing.T) {
	sh(t, "docker", "exec", e.pin, "crictl", "rmi", e.containerdRef())
	t.Cleanup(func() { sh(t, "kind", "load", "docker-image", e.proxyImage, "--name", e.cluster, "--nodes", e.pin) })
}

func (e *env) assertNeverInit(t *testing.T, id uuid.UUID) {
	t.Helper()
	ev := e.evidence(t, id)
	p := ev.Pods[proxyComponent]
	if p == nil || ev.arm(proxyComponent) != "never_initialized" || !p.FinalizerOff {
		t.Errorf("proxy evidence: arm %q, %+v; want never_initialized with the finalizer off", ev.arm(proxyComponent), p)
		return
	}
	note(t, "arm never_initialized, assoc %s", p.Assoc)
}

func caseNeverInitImagePull(t *testing.T, e *env) {
	e.setSched(t, e.pin)
	e.withoutProxyImage(t)
	id := uuid.New()
	e.cleanupRun(t, id)
	err := waitErr(t, e.startRun(bg, e.main, id), createWait, "CreateSandbox")
	if err == nil {
		t.Fatal("CreateSandbox succeeded with the proxy image missing")
	}
	note(t, "create refused: %v", err)
	e.waitRunGone(t, id, goneWait)
	e.assertNeverInit(t, id)
}

func caseNeverInitCancel(t *testing.T, e *env) {
	e.setSched(t, e.pin)
	e.withoutProxyImage(t)
	id := uuid.New()
	e.cleanupRun(t, id)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := e.startRun(ctx, e.main, id)
	p := e.waitPod(t, proxyName(id), time.Minute, "bound", func(p *corev1.Pod) bool { return p.Spec.NodeName != "" })
	cancel()
	var at []string
	for _, s := range p.Status.InitContainerStatuses {
		if s.State.Waiting != nil {
			at = append(at, s.Name+":"+s.State.Waiting.Reason)
		}
	}
	note(t, "cancelled at bind; init containers waiting %v (the pull fails rather than runs slow on kind)", at)
	if err := waitErr(t, ch, createWait, "CreateSandbox"); err == nil {
		t.Fatal("CreateSandbox succeeded after the cancel")
	}
	e.waitRunGone(t, id, goneWait)
	e.assertNeverInit(t, id)
}

func caseRestartWhileUnbound(t *testing.T, e *env) {
	id := uuid.New()
	e.cleanupRun(t, id)
	v1 := e.store.view(nil)
	d1 := e.newDriver(t, v1)
	e.cordon(t, e.pin, true) // the node is in the before-list, but nothing binds yet
	t.Cleanup(func() { e.cordon(t, e.pin, false) })
	ctx1, cancel1 := context.WithCancel(bg)
	defer cancel1()
	ch := e.startRun(ctx1, d1, id)
	e.waitPod(t, proxyName(id), time.Minute, "created and unbound", func(p *corev1.Pod) bool { return p.Spec.NodeName == "" })
	if err := poll(bg, time.Minute, func(context.Context) (bool, error) {
		p := e.evidence(t, id).Pods[proxyComponent]
		if p == nil || p.Before == nil {
			return false, nil
		}
		b, _ := json.Marshal(p.Before)
		return strings.Contains(string(b), e.pin), nil
	}); err != nil {
		t.Fatalf("the persisted before-list never named %s", e.pin)
	}
	v1.dead.Store(true) // runnerd stops while the pod is unbound
	cancel1()
	_ = waitErr(t, ch, time.Minute, "the stopped CreateSandbox")
	if p, err := e.admin.CoreV1().Pods(e.runsNS).Get(bg, proxyName(id), metav1.GetOptions{}); err != nil || p.DeletionTimestamp != nil {
		t.Fatalf("the stopped runnerd touched the pod: %v", err)
	}
	e.cordon(t, e.pin, false) // it binds to a node already in the persisted map
	e.waitPod(t, proxyName(id), 2*time.Minute, "bound and running", func(p *corev1.Pod) bool {
		return p.Spec.NodeName == e.pin && p.Status.Phase == corev1.PodRunning
	})
	if err := e.main.ReplayFences([]uuid.UUID{id}); err != nil { // the restarted runnerd
		t.Fatal(err)
	}
	if err := e.main.StopProxy(bg, ref(id)); err != nil {
		t.Fatalf("StopProxy after the restart: %v", err)
	}
	p := e.evidence(t, id).Pods[proxyComponent]
	if p.Assoc != "causal" || p.Witness == nil || p.Witness.Arm != "terminal" {
		t.Fatalf("assoc %q, witness %+v; want causal and terminal", p.Assoc, p.Witness)
	}
	for _, c := range p.Witness.Containers {
		if c.Tier != "tier1" {
			t.Errorf("container %s completed on %q, want tier1", c.Name, c.Tier)
		}
	}
	if err := e.main.StopSandbox(bg, ref(id)); err != nil {
		t.Fatalf("StopSandbox: %v", err)
	}
	e.waitRunGone(t, id, goneWait)
	note(t, "restart between the unbound read and the bind: causal, normal stop on tier1")
}

// crashGap kills runnerd in one gap of the step order on path, restarts it (e.main has never seen
// the run: a process with no memory of it), and checks the run finishes with nothing done twice.
func (e *env) crashGap(t *testing.T, path, gap string) {
	t.Helper()
	id := uuid.New()
	e.cleanupRun(t, id)
	trigger := func(rid uuid.UUID, ev *evDoc) bool {
		p := ev.Pods[proxyComponent]
		if rid != id || p == nil {
			return false
		}
		switch gap {
		case "witness":
			return p.Witness != nil && !p.FinalizerOff
		case "finalizer":
			return p.FinalizerOff
		}
		return false
	}
	v1 := e.store.view(trigger)
	d1 := e.newDriver(t, v1)
	if _, err := d1.CreateSandbox(bg, e.spec(id)); err != nil {
		t.Fatalf("%s/%s: CreateSandbox: %v", path, gap, err)
	}
	var err error
	switch path {
	case "stop-proxy":
		err = d1.StopProxy(bg, ref(id))
	case "teardown":
		err = d1.StopSandbox(bg, ref(id))
	case "sweep":
		_, err = d1.SweepOrphanedSandboxes(bg, 0, func(r uuid.UUID) bool { return r == id })
	}
	if gap == "stamp" {
		if err != nil {
			t.Fatalf("%s/%s: %v", path, gap, err)
		}
	} else if !v1.dead.Load() {
		t.Fatalf("%s/%s: the gap was never reached (%v)", path, gap, err)
	}
	finalizers := "gone"
	if p, gerr := e.admin.CoreV1().Pods(e.runsNS).Get(bg, proxyName(id), metav1.GetOptions{}); gerr == nil {
		finalizers = fmt.Sprint(p.Finalizers)
	}
	if err := e.main.ReplayFences([]uuid.UUID{id}); err != nil {
		t.Fatal(err)
	}
	switch path {
	case "stop-proxy":
		if err := e.main.StopProxy(bg, ref(id)); err != nil {
			t.Errorf("%s/%s: StopProxy after restart: %v", path, gap, err)
		}
		if err := e.main.StopSandbox(bg, ref(id)); err != nil {
			t.Errorf("%s/%s: StopSandbox after restart: %v", path, gap, err)
		}
	case "teardown":
		if err := e.main.StopSandbox(bg, ref(id)); err != nil {
			t.Errorf("%s/%s: StopSandbox after restart: %v", path, gap, err)
		}
	case "sweep":
		if _, err := e.main.SweepOrphanedSandboxes(bg, 0, func(r uuid.UUID) bool { return r == id }); err != nil {
			t.Errorf("%s/%s: sweep after restart: %v", path, gap, err)
		}
	}
	e.waitRunGone(t, id, goneWait)
	if n := e.sink.goneCount(id); n != 1 {
		t.Errorf("%s/%s: producer settled %d times, want once", path, gap, n)
	}
	note(t, "%s: proxy finalizers at the crash %s; finished after restart", gap, finalizers)
}

func caseCrashGapsStopProxy(t *testing.T, e *env) {
	e.setSched(t, e.pin)
	for _, gap := range []string{"witness", "finalizer", "stamp"} {
		e.crashGap(t, "stop-proxy", gap)
	}
}

func caseCrashGapsTeardown(t *testing.T, e *env) {
	e.setSched(t, e.pin)
	for _, gap := range []string{"witness", "finalizer"} {
		e.crashGap(t, "teardown", gap)
	}
}

func caseCrashGapsSweep(t *testing.T, e *env) {
	e.setSched(t, e.pin)
	for _, gap := range []string{"witness", "finalizer"} {
		e.crashGap(t, "sweep", gap)
	}
}

func caseHardDenyConfig(t *testing.T, e *env) {
	e.setSched(t, e.pin)
	id := uuid.New()
	e.cleanupRun(t, id)
	if _, err := e.main.CreateSandbox(bg, e.spec(id)); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	secs, err := e.admin.CoreV1().Secrets(e.runsNS).List(bg, metav1.ListOptions{LabelSelector: labelRun + "=" + id.String()})
	if err != nil {
		t.Fatal(err)
	}
	var hd *struct {
		Suffixes []string `json:"suffixes"`
		Hosts    []string `json:"hosts"`
		CIDRs    []string `json:"cidrs"`
	}
	for _, s := range secs.Items {
		for _, v := range s.Data {
			var cfg map[string]json.RawMessage
			if json.Unmarshal(v, &cfg) == nil && cfg["hard_deny"] != nil {
				if err := json.Unmarshal(cfg["hard_deny"], &hd); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if hd == nil {
		t.Fatal("no proxy config carries hard_deny")
	}
	if !slices.ContainsFunc(hd.Suffixes, func(s string) bool { return strings.Contains(s, "svc") }) {
		t.Errorf("hard_deny suffixes %v lack the service suffix", hd.Suffixes)
	}
	if !slices.Contains(hd.Hosts, e.relayIP) {
		t.Errorf("hard_deny hosts %v lack the relay ClusterIP %s", hd.Hosts, e.relayIP)
	}
	note(t, "hard_deny suffixes %v hosts %v (the apiserver entry here is the suite's out-of-cluster address; runnerd's in-cluster boot resolves its own)", hd.Suffixes, hd.Hosts)
	e.stopRun(t, id)
}

// caseRetainedPolicy is Fable L4: a force-deleted sandbox pod held by the finalizer keeps its
// NetworkPolicy, then the finalizer is stripped and the CNI's behaviour is recorded.
func caseRetainedPolicy(t *testing.T, e *env) {
	e.setSched(t, e.pin)
	lname := "k6-listener-" + short()
	if _, err := e.admin.CoreV1().Pods(e.runsNS).Create(bg, e.testPod(lname, "", nil,
		`while true; do nc -l -p 8080 </dev/null >/dev/null 2>&1 && echo conn; done`), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.dropTestPod(t, lname)
	l := e.waitPod(t, lname, 2*time.Minute, "Running", func(p *corev1.Pod) bool { return p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" })
	conns := func() int {
		b, err := e.admin.CoreV1().Pods(e.runsNS).GetLogs(lname, &corev1.PodLogOptions{}).DoRaw(bg)
		if err != nil {
			t.Fatalf("listener logs: %v", err)
		}
		return strings.Count(string(b), "conn")
	}
	id := uuid.New()
	e.cleanupRun(t, id)
	if _, err := e.main.CreateSandbox(bg, e.spec(id)); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	probe := fmt.Sprintf(`(while true; do nc -w 1 %s 8080 </dev/null; sleep 0.3; done) >/dev/null 2>&1 &`, l.Status.PodIP)
	sh(t, "kubectl", "--kubeconfig", os.Getenv("K6_ADMIN_KUBECONFIG"), "-n", e.runsNS, "exec", ref(id), "-c", "agent", "--", "sh", "-c", probe)
	time.Sleep(5 * time.Second)
	if n := conns(); n != 0 {
		t.Fatalf("the agent reached an in-cluster pod %d times while its policy held", n)
	}
	zero := int64(0)
	if err := e.admin.CoreV1().Pods(e.runsNS).Delete(bg, ref(id), metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil {
		t.Fatal(err)
	}
	e.waitPod(t, ref(id), time.Minute, "held by the finalizer", func(p *corev1.Pod) bool { return p.DeletionTimestamp != nil })
	time.Sleep(5 * time.Second)
	held := conns()
	if held != 0 {
		t.Errorf("a force-deleted sandbox held by the finalizer reached an in-cluster pod %d times", held)
	}
	if _, err := e.admin.CoreV1().Pods(e.runsNS).Patch(bg, ref(id), k8stypes.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	e.podGone(t, ref(id), time.Minute)
	time.Sleep(5 * time.Second)
	stripped := conns() - held
	// With the object gone the run is not complete: NotFound is never evidence (§3.1).
	ctx, cancel := context.WithTimeout(bg, 60*time.Second)
	defer cancel()
	if err := e.main.StopSandbox(ctx, ref(id)); err == nil {
		t.Errorf("label teardown completed on a NotFound agent pod")
	}
	note(t, "policy held before and after the force delete (0 connections); finalizer stripped: %d connections afterwards (recorded: the kubelet kills the process on a force delete, so this kind CNI shows no window); teardown stays pending on NotFound", stripped)
}

// caseNodeReuse: a Node a run used is deleted and its name comes back on a new Node object; the 30 s
// tick records the reuse, and tier 1 then refuses (owner #8). It also records the pod GC's mark.
func caseNodeReuse(t *testing.T, e *env) {
	e.setSched(t, e.other)
	t.Cleanup(func() { e.setSched(t, e.pin) })
	id := uuid.New()
	e.cleanupRun(t, id)
	if _, err := e.main.CreateSandbox(bg, e.spec(id)); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	oldUID := e.nodeUID(t, e.other)
	sh(t, "docker", "stop", e.other)
	if err := e.admin.CoreV1().Nodes().Delete(bg, e.other, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	gc := "not seen"
	_ = poll(bg, 3*time.Minute, func(ctx context.Context) (bool, error) {
		p, err := e.admin.CoreV1().Pods(e.runsNS).Get(ctx, proxyName(id), metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.DisruptionTarget && c.Reason == "DeletionByPodGC" {
				gc = fmt.Sprintf("phase %s, DeletionByPodGC, deleting %v", p.Status.Phase, p.DeletionTimestamp != nil)
				return true, nil
			}
		}
		return false, nil
	})
	sh(t, "docker", "start", e.other)
	if err := poll(bg, 4*time.Minute, func(ctx context.Context) (bool, error) {
		n, err := e.admin.CoreV1().Nodes().Get(ctx, e.other, metav1.GetOptions{})
		if err != nil || string(n.UID) == oldUID {
			return false, nil
		}
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatalf("node %s did not come back under a new UID", e.other)
	}
	_, terr := e.main.ObserveProxies(bg, []uuid.UUID{id}) // the 30 s tick
	ev := e.evidence(t, id)
	if len(ev.Reuse) == 0 {
		t.Errorf("the tick list did not record the name reuse (tick err %v)", terr)
	}
	ctx, cancel := context.WithTimeout(bg, 90*time.Second)
	defer cancel()
	err := e.main.StopProxy(ctx, ref(id))
	var pending *runner.ContainmentPendingError
	switch {
	case err == nil:
		t.Errorf("StopProxy completed after a recorded name reuse")
	case errors.As(err, &pending):
		if !pending.NameReused {
			t.Errorf("pending error does not carry the reuse: %+v", pending)
		}
	}
	if arm := e.evidence(t, id).arm(proxyComponent); arm != "" {
		t.Errorf("proxy completed on arm %q after a reuse", arm)
	}
	note(t, "pod GC mark: %s; reuse recorded on the tick; StopProxy refused: %v", gc, err)
}
