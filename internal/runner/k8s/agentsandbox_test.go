// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/uuid"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	asSandboxUID = k8stypes.UID("sandbox-uid-1")
	asPodUID     = k8stypes.UID("agent-pod-uid-1")
	asPodRV      = "626"
	asProxyUID   = k8stypes.UID("proxy-uid-1")
	// asPlacement gives the agent a node selector, a toleration and an annotation for the read-back to hold.
	asPlacement = `{"nodeSelector":{"pool":"sandbox"},"tolerations":[{"key":"sandbox","operator":"Exists","effect":"NoSchedule"}],"podAnnotations":{"example.com/note":"kept"}}`
)

var asRunID = uuid.MustParse("1b4e28ba-2fa1-41d2-883f-0016d3cca427")

// asSpec is a run with every kind of value the read-back holds: plain and secretKeyRef env, a
// delivered secret file (its init container, Secret volumes and mounts), a disk budget, a deadline.
func asSpec() runner.SandboxSpec {
	spec := secretSandboxSpec()
	spec.RunID = asRunID
	spec.SecretEnv = map[string]string{"API_KEY": "secret-value"}
	spec.Resources.LifetimeCap = 2 * time.Hour
	return spec
}

// sandboxAnswer is the fake controller: given the created Sandbox, the Sandbox to store (nil: none)
// and the pod it makes (nil: none).
type sandboxAnswer func(sb *unstructured.Unstructured) (*unstructured.Unstructured, *corev1.Pod)

// controllerMaking answers like agent-sandbox v1.0.9 did on kind: a pod of the Sandbox's name from its
// template, gated, with the controller's label, annotations and controlling owner reference and the API
// server's defaults (the lane's evidence-pod-gated.yaml), then mutate.
func controllerMaking(t *testing.T, mutate func(*corev1.Pod)) sandboxAnswer {
	return func(sb *unstructured.Unstructured) (*unstructured.Unstructured, *corev1.Pod) {
		pod := podTemplateOf(t, sb)
		pod.Name, pod.Namespace = sb.GetName(), sb.GetNamespace()
		pod.UID, pod.ResourceVersion = asPodUID, asPodRV
		propagated := map[string]string{
			"agents.x-k8s.io/propagated-labels":      strings.Join(slices.Sorted(maps.Keys(pod.Labels)), ","),
			"agents.x-k8s.io/propagated-annotations": strings.Join(slices.Sorted(maps.Keys(pod.Annotations)), ","),
		}
		pod.Labels["agents.x-k8s.io/sandbox-name-hash"] = "0f4f3dff"
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		maps.Copy(pod.Annotations, propagated)
		pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: agentSandboxAPIVersion, Kind: "Sandbox", Name: sb.GetName(), UID: sb.GetUID(), Controller: boolPtr(true), BlockOwnerDeletion: boolPtr(true)}}
		applyAPIDefaults(pod)
		pod.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "SchedulingGated", Message: "Scheduling is blocked due to non-empty scheduling gates",
		}}}
		if mutate != nil {
			mutate(pod)
		}
		return sb, pod
	}
}

// applyAPIDefaults sets what kind v1.30's API server and admission added to the gated pod. The
// API server defaults a container's pull policy and termination message fields only where they are
// empty: the stage-secrets init container keeps the FallbackToLogsOnError Wardyn sets.
func applyAPIDefaults(pod *corev1.Pod) {
	for _, cs := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range cs {
			if cs[i].ImagePullPolicy == "" {
				cs[i].ImagePullPolicy = corev1.PullIfNotPresent
			}
			if cs[i].TerminationMessagePath == "" {
				cs[i].TerminationMessagePath = corev1.TerminationMessagePathDefault
			}
			if cs[i].TerminationMessagePolicy == "" {
				cs[i].TerminationMessagePolicy = corev1.TerminationMessageReadFile
			}
		}
	}
	grace, priority, preempt := int64(30), int32(0), corev1.PreemptLowerPriority
	pod.Spec.SchedulerName = corev1.DefaultSchedulerName
	pod.Spec.ServiceAccountName, pod.Spec.DeprecatedServiceAccount = "default", "default"
	pod.Spec.TerminationGracePeriodSeconds = &grace
	pod.Spec.Priority, pod.Spec.PreemptionPolicy = &priority, &preempt
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, defaultTolerationTyped("node.kubernetes.io/not-ready"), defaultTolerationTyped("node.kubernetes.io/unreachable"))
	for i := range pod.Spec.Volumes {
		if s := pod.Spec.Volumes[i].Secret; s != nil && s.DefaultMode == nil {
			mode := int32(420)
			s.DefaultMode = &mode
		}
	}
}

func defaultTolerationTyped(key string) corev1.Toleration {
	secs := int64(300)
	return corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &secs}
}

func podTemplateOf(t *testing.T, sb *unstructured.Unstructured) *corev1.Pod {
	t.Helper()
	tmpl, _, err := unstructured.NestedMap(sb.Object, "spec", "podTemplate")
	if err != nil {
		t.Fatalf("read the Sandbox's podTemplate: %v", err)
	}
	pod := &corev1.Pod{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(tmpl, pod); err != nil {
		t.Fatalf("convert the Sandbox's podTemplate: %v", err)
	}
	return pod
}

func servedSandboxes() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{{GroupVersion: agentSandboxAPIVersion, APIResources: []metav1.APIResource{{Name: "sandboxes", Namespaced: true, Kind: "Sandbox"}}}}
}

// installAccessReactor answers every SelfSubjectAccessReview allowed, except deny ("verb resource"),
// or fails them all with failErr.
func installAccessReactor(cs *fake.Clientset, deny string, failErr error) {
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if failErr != nil {
			return true, nil, failErr
		}
		in := a.(clienttesting.CreateAction).GetObject().(*authzv1.SelfSubjectAccessReview).DeepCopy()
		ra := in.Spec.ResourceAttributes
		in.Status.Allowed = ra.Verb+" "+ra.Resource != deny
		return true, in, nil
	})
}

func useDynamicClient(t *testing.T, dyn dynamic.Interface) {
	t.Helper()
	prev := newDynamicClient
	newDynamicClient = func(*rest.Config) (dynamic.Interface, error) { return dyn, nil }
	t.Cleanup(func() { newDynamicClient = prev })
}

func newFakeDynamic() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{sandboxesGVR: "SandboxList"})
}

// newSandboxRig is an on Driver against fakes: Sandboxes served, every verb allowed, the proxy given an
// IP and a UID, answer as the controller, and the agent running once its gate is gone.
func newSandboxRig(t *testing.T, cfg Config, answer sandboxAnswer) (*Driver, *fake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	cs := fake.NewClientset()
	installCanaryReactor(t, cs, false)
	installDeleteCollectionSupport(t, cs)
	cs.Resources = servedSandboxes()
	installAccessReactor(cs, "", nil)
	dyn := newFakeDynamic()
	useDynamicClient(t, dyn)
	cfg.AgentSandbox = true
	cfg.Namespaces = []string{testNamespace}
	cfg.ProxyImage = "wardyn/wardyn-proxy:test"
	d, err := newWithClient(context.Background(), cs, testRestConfig(), cfg)
	if err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
	installProxyIPReactor(t, cs, "10.244.0.7")
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pod, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod)
		if !ok || !strings.HasPrefix(pod.Name, "wardyn-proxy-") {
			return false, nil, nil
		}
		pod = pod.DeepCopy()
		pod.UID = asProxyUID
		if err := cs.Tracker().Create(podsGVR, pod, a.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, pod, nil
	})
	cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := a.(clienttesting.GetAction)
		if !ok || !strings.HasPrefix(ga.GetName(), agentPodNamePrefix) {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(podsGVR, ga.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		if len(pod.Spec.SchedulingGates) == 0 && pod.Status.Phase == corev1.PodPending {
			pod.Spec.NodeName, pod.Status.Phase = "node-1", corev1.PodRunning
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: mainContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		}
		return true, pod, nil
	})
	dyn.PrependReactor("create", "sandboxes", func(a clienttesting.Action) (bool, runtime.Object, error) {
		sb := a.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		sb.SetUID(asSandboxUID)
		stored, pod := answer(sb)
		if stored != nil {
			if err := dyn.Tracker().Create(sandboxesGVR, stored, sb.GetNamespace()); err != nil {
				return true, nil, err
			}
		}
		if pod != nil {
			if err := cs.Tracker().Create(podsGVR, pod, pod.Namespace); err != nil {
				return true, nil, err
			}
		}
		return true, sb, nil
	})
	return d, cs, dyn
}

func podPatches(cs *fake.Clientset) []clienttesting.PatchAction {
	var out []clienttesting.PatchAction
	for _, a := range cs.Actions() {
		if pa, ok := a.(clienttesting.PatchAction); ok && a.GetVerb() == "patch" && a.GetResource().Resource == "pods" {
			out = append(out, pa)
		}
	}
	return out
}

func sandboxActions(dyn *dynamicfake.FakeDynamicClient, verb string) int {
	n := 0
	for _, a := range dyn.Actions() {
		if a.GetVerb() == verb && a.GetResource() == sandboxesGVR {
			n++
		}
	}
	return n
}

// assertRefusedAndTornDown is every refusal's aftermath: the Sandbox deleted at once, the gate never
// removed, and the run's other objects gone.
func assertRefusedAndTornDown(t *testing.T, cs *fake.Clientset, dyn *dynamicfake.FakeDynamicClient) {
	t.Helper()
	if n := sandboxActions(dyn, "delete"); n != 1 {
		t.Errorf("Sandbox deletes = %d, want 1: a refusal deletes the Sandbox at once", n)
	}
	if _, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, agentPodName(asRunID)); !isNotFound(err) {
		t.Errorf("the refused Sandbox is still there (get err %v)", err)
	}
	if p := podPatches(cs); len(p) != 0 {
		t.Errorf("a refused pod was patched %d time(s); its gate must stay", len(p))
	}
	assertRunObjectsGone(t, cs, asRunID)
}

func findEnv(c *corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

func TestAgentSandbox_CreatesTheSandboxAndUngatesTheAdmittedPod(t *testing.T) {
	d, cs, dyn := newSandboxRig(t, Config{SandboxPlacement: asPlacement}, controllerMaking(t, nil))
	cs.ClearActions()
	spec := asSpec()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if sb.Ref != agentPodName(spec.RunID) {
		t.Errorf("Ref = %q, want %q", sb.Ref, agentPodName(spec.RunID))
	}

	obj, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, sb.Ref)
	if err != nil {
		t.Fatalf("get Sandbox: %v", err)
	}
	u := obj.(*unstructured.Unstructured)
	sbSpec, _, _ := unstructured.NestedMap(u.Object, "spec")
	if got, want := slices.Sorted(maps.Keys(sbSpec)), []string{"failedPodPolicy", "podTemplate", "service"}; !slices.Equal(got, want) {
		t.Errorf("Sandbox spec keys = %v, want %v: no lifecycle, operatingMode or volumeClaimTemplates", got, want)
	}
	if sbSpec["service"] != false || sbSpec["failedPodPolicy"] != "Retain" {
		t.Errorf("Sandbox spec service=%v failedPodPolicy=%v, want false and Retain", sbSpec["service"], sbSpec["failedPodPolicy"])
	}
	owners := u.GetOwnerReferences()
	if len(owners) != 1 || owners[0].Kind != "Pod" || owners[0].Name != proxyPodName(spec.RunID) || owners[0].UID != asProxyUID {
		t.Errorf("Sandbox owners = %+v, want the proxy pod, so deleting the proxy collects it", owners)
	}
	if u.GetLabels()[labelComponent] != componentAgent || u.GetLabels()[labelRun] != spec.RunID.String() {
		t.Errorf("Sandbox labels = %v, want the agent's wardyn labels", u.GetLabels())
	}
	if gates := podTemplateOf(t, u).Spec.SchedulingGates; len(gates) != 1 || gates[0].Name != agentSandboxGate {
		t.Errorf("template gates = %+v, want exactly %s", gates, agentSandboxGate)
	}

	patches := podPatches(cs)
	if len(patches) != 1 {
		t.Fatalf("pod patches = %d, want exactly the one that removes the gate", len(patches))
	}
	if patches[0].GetName() != sb.Ref || patches[0].GetPatchType() != k8stypes.JSONPatchType {
		t.Errorf("patch on %q type %q, want a JSON patch on %q", patches[0].GetName(), patches[0].GetPatchType(), sb.Ref)
	}
	var ops []jsonPatchOp
	if err := json.Unmarshal(patches[0].GetPatch(), &ops); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	want := []jsonPatchOp{
		{Op: "test", Path: "/metadata/uid", Value: string(asPodUID)},
		{Op: "test", Path: "/metadata/resourceVersion", Value: asPodRV},
		{Op: "test", Path: "/spec/schedulingGates", Value: []any{map[string]any{"name": agentSandboxGate}}},
		{Op: "remove", Path: "/spec/schedulingGates"},
	}
	if diff := cmp.Diff(want, ops); diff != "" {
		t.Errorf("gate patch (-want +got):\n%s", diff)
	}
	for _, a := range cs.Actions() {
		if ca, ok := a.(clienttesting.CreateAction); ok && a.GetResource().Resource == "pods" {
			if name := ca.GetObject().(metav1.Object).GetName(); name == sb.Ref {
				t.Errorf("Wardyn created the agent pod %q itself; the Sandbox's controller makes it", name)
			}
		}
	}
	if n := sandboxActions(dyn, "delete"); n != 0 {
		t.Errorf("an admitted run's Sandbox was deleted %d time(s)", n)
	}
}

// The on template is exactly the pod the off path creates, plus the gate: turning the backend on
// changes who makes the pod, never what it is.
func TestAgentSandbox_TemplateIsTheOffPodPlusTheGate(t *testing.T) {
	ctx := context.Background()
	spec := asSpec()
	off, offCS := newTestDriver(t, Config{SandboxPlacement: asPlacement})
	installProxyIPReactor(t, offCS, "10.244.0.7")
	installAgentRunningReactor(t, offCS)
	if _, err := off.CreateSandbox(ctx, spec); err != nil {
		t.Fatalf("off CreateSandbox: %v", err)
	}
	offPod, err := offCS.Tracker().Get(podsGVR, testNamespace, agentPodName(spec.RunID))
	if err != nil {
		t.Fatalf("off agent pod: %v", err)
	}
	on, _, dyn := newSandboxRig(t, Config{SandboxPlacement: asPlacement}, controllerMaking(t, nil))
	if _, err := on.CreateSandbox(ctx, spec); err != nil {
		t.Fatalf("on CreateSandbox: %v", err)
	}
	var sb *unstructured.Unstructured
	for _, a := range dyn.Actions() {
		if ca, ok := a.(clienttesting.CreateAction); ok && a.GetResource() == sandboxesGVR {
			sb = ca.GetObject().(*unstructured.Unstructured)
		}
	}
	if sb == nil {
		t.Fatal("no Sandbox created")
	}
	tmpl := podTemplateOf(t, sb)
	tmpl.Spec.SchedulingGates = nil
	want := offPod.(*corev1.Pod)
	if diff := cmp.Diff(want.Spec, tmpl.Spec, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("template spec differs from the off pod (-off +on):\n%s", diff)
	}
	if diff := cmp.Diff(want.Labels, tmpl.Labels); diff != "" {
		t.Errorf("template labels (-off +on):\n%s", diff)
	}
	if diff := cmp.Diff(want.Annotations, tmpl.Annotations, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("template annotations (-off +on):\n%s", diff)
	}
}

// One case per read-back field: the controller (or a hook) changes what Wardyn asked for, and the run
// is refused naming the field, the Sandbox deleted, the gate kept, everything torn down.
//
//nolint:funlen // one table row per read-back field
func TestAgentSandbox_ReadBackRefusesEveryChangedField(t *testing.T) {
	agent := func(p *corev1.Pod) *corev1.Container { return &p.Spec.Containers[0] }
	stage := func(p *corev1.Pod) *corev1.Container { return &p.Spec.InitContainers[0] }
	managed := managedFileVolumeName(0)
	owner := "metadata.ownerReferences[" + agentPodName(asRunID) + "]"
	cases := []struct {
		name   string
		mutate func(*corev1.Pod)
		field  string
	}{
		{"container image", func(p *corev1.Pod) { agent(p).Image = "attacker/agent:1" }, "spec.containers[agent].image"},
		{"container name", func(p *corev1.Pod) { agent(p).Name = "renamed" }, "spec.containers[agent].name"},
		{"container command", func(p *corev1.Pod) { agent(p).Command = []string{"sh"} }, "spec.containers[agent].command"},
		{"container args added", func(p *corev1.Pod) { agent(p).Args = []string{"-x"} }, "spec.containers[agent].args[0]"},
		{"env value", func(p *corev1.Pod) { findEnv(agent(p), "HTTP_PROXY").Value = "http://elsewhere:3128" }, "spec.containers[agent].env[HTTP_PROXY].value"},
		{"env secretKeyRef", func(p *corev1.Pod) { findEnv(agent(p), "API_KEY").ValueFrom.SecretKeyRef.Name = "other" }, "spec.containers[agent].env[API_KEY].valueFrom.secretKeyRef.name"},
		{"env added", func(p *corev1.Pod) { agent(p).Env = append(agent(p).Env, corev1.EnvVar{Name: "INJECTED", Value: "1"}) }, "spec.containers[agent].env[INJECTED]"},
		{"mount dropped", func(p *corev1.Pod) { c := agent(p); c.VolumeMounts = c.VolumeMounts[:len(c.VolumeMounts)-1] }, "spec.containers[agent].volumeMounts"},
		{"mount made writable", func(p *corev1.Pod) {
			for i := range agent(p).VolumeMounts {
				if agent(p).VolumeMounts[i].Name == managed {
					agent(p).VolumeMounts[i].ReadOnly = false
				}
			}
		}, "spec.containers[agent].volumeMounts[" + managed + "].readOnly"},
		{"securityContext privileged", func(p *corev1.Pod) { agent(p).SecurityContext.Privileged = boolPtr(true) }, "spec.containers[agent].securityContext.privileged"},
		{"securityContext escalation", func(p *corev1.Pod) { agent(p).SecurityContext.AllowPrivilegeEscalation = boolPtr(true) }, "spec.containers[agent].securityContext.allowPrivilegeEscalation"},
		{"resources", func(p *corev1.Pod) { agent(p).Resources.Limits[corev1.ResourceMemory] = resource.MustParse("64Gi") }, "spec.containers[agent].resources.limits.memory"},
		{"image pull policy", func(p *corev1.Pod) { agent(p).ImagePullPolicy = corev1.PullNever }, "spec.containers[agent].imagePullPolicy"},
		{"container added", func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar", Image: "mesh/proxy:1"})
		}, "spec.containers[sidecar]"},
		{"init image", func(p *corev1.Pod) { stage(p).Image = "attacker/stage:1" }, "spec.initContainers[" + stageAgentSecretsName + "].image"},
		{"init command", func(p *corev1.Pod) { stage(p).Command = []string{"true"} }, "spec.initContainers[" + stageAgentSecretsName + "].command"},
		{"init args added", func(p *corev1.Pod) { stage(p).Args = []string{"-x"} }, "spec.initContainers[" + stageAgentSecretsName + "].args[0]"},
		{"init mount dropped", func(p *corev1.Pod) { c := stage(p); c.VolumeMounts = c.VolumeMounts[:len(c.VolumeMounts)-1] }, "spec.initContainers[" + stageAgentSecretsName + "].volumeMounts"},
		{"init source mount made writable", func(p *corev1.Pod) { stage(p).VolumeMounts[0].ReadOnly = false }, "spec.initContainers[" + stageAgentSecretsName + "].volumeMounts[" + agentSecretsSourceVolume + "].readOnly"},
		{"init securityContext privileged", func(p *corev1.Pod) { stage(p).SecurityContext.Privileged = boolPtr(true) }, "spec.initContainers[" + stageAgentSecretsName + "].securityContext.privileged"},
		{"init terminationMessagePolicy", func(p *corev1.Pod) { stage(p).TerminationMessagePolicy = corev1.TerminationMessageReadFile }, "spec.initContainers[" + stageAgentSecretsName + "].terminationMessagePolicy"},
		{"init container added", func(p *corev1.Pod) {
			p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{Name: "injected", Image: "x"})
		}, "spec.initContainers[injected]"},
		{"volume source", func(p *corev1.Pod) { v, _ := findVolume(p.Spec.Volumes, managed); v.Secret.SecretName = "other" }, "spec.volumes[" + managed + "].secret.secretName"},
		{"volume mode", func(p *corev1.Pod) {
			mode := int32(0o777)
			v, _ := findVolume(p.Spec.Volumes, managed)
			v.Secret.DefaultMode = &mode
		}, "spec.volumes[" + managed + "].secret.defaultMode"},
		{"volume added", func(p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		}, "spec.volumes[host]"},
		{"volume dropped", func(p *corev1.Pod) { p.Spec.Volumes = p.Spec.Volumes[:len(p.Spec.Volumes)-1] }, "spec.volumes"},
		{"hostAliases", func(p *corev1.Pod) { p.Spec.HostAliases[0].IP = "10.0.0.1" }, "spec.hostAliases[0].ip"},
		{"dnsPolicy", func(p *corev1.Pod) { p.Spec.DNSPolicy = corev1.DNSClusterFirst }, "spec.dnsPolicy"},
		{"dnsConfig", func(p *corev1.Pod) { p.Spec.DNSConfig.Nameservers = []string{"10.96.0.10"} }, "spec.dnsConfig.nameservers[0]"},
		{"automountServiceAccountToken", func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = boolPtr(true) }, "spec.automountServiceAccountToken"},
		{"enableServiceLinks", func(p *corev1.Pod) { p.Spec.EnableServiceLinks = boolPtr(true) }, "spec.enableServiceLinks"},
		{"runtimeClassName", func(p *corev1.Pod) { rc := "unconfined"; p.Spec.RuntimeClassName = &rc }, "spec.runtimeClassName"},
		{"restartPolicy", func(p *corev1.Pod) { p.Spec.RestartPolicy = corev1.RestartPolicyAlways }, "spec.restartPolicy"},
		{"activeDeadlineSeconds", func(p *corev1.Pod) { *p.Spec.ActiveDeadlineSeconds = 1 << 30 }, "spec.activeDeadlineSeconds"},
		{"serviceAccountName", func(p *corev1.Pod) { p.Spec.ServiceAccountName = "builder" }, "spec.serviceAccountName"},
		{"pod securityContext", func(p *corev1.Pod) { gid := int64(0); p.Spec.SecurityContext.FSGroup = &gid }, "spec.securityContext.fsGroup"},
		{"nodeSelector", func(p *corev1.Pod) { p.Spec.NodeSelector["pool"] = "general" }, "spec.nodeSelector.pool"},
		{"toleration changed", func(p *corev1.Pod) { p.Spec.Tolerations[0].Key = "anything" }, "spec.tolerations[0]"},
		{"toleration added", func(p *corev1.Pod) {
			p.Spec.Tolerations = append(p.Spec.Tolerations, corev1.Toleration{Operator: corev1.TolerationOpExists})
		}, "spec.tolerations[3]"},
		{"default toleration seconds", func(p *corev1.Pod) { *p.Spec.Tolerations[1].TolerationSeconds = 5 }, "spec.tolerations[1]"},
		{"affinity added", func(p *corev1.Pod) {
			p.Spec.Affinity = &corev1.Affinity{PodAffinity: &corev1.PodAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "kubernetes.io/hostname"}}}}
		}, "spec.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution[0]"},
		{"priorityClassName added", func(p *corev1.Pod) { p.Spec.PriorityClassName = "system-node-critical" }, "spec.priorityClassName"},
		{"schedulerName", func(p *corev1.Pod) { p.Spec.SchedulerName = "other-scheduler" }, "spec.schedulerName"},
		{"terminationGracePeriodSeconds", func(p *corev1.Pod) { *p.Spec.TerminationGracePeriodSeconds = 5 }, "spec.terminationGracePeriodSeconds"},
		{"hostNetwork", func(p *corev1.Pod) { p.Spec.HostNetwork = true }, "spec.hostNetwork"},
		{"hostPID", func(p *corev1.Pod) { p.Spec.HostPID = true }, "spec.hostPID"},
		{"hostIPC", func(p *corev1.Pod) { p.Spec.HostIPC = true }, "spec.hostIPC"},
		{"shareProcessNamespace", func(p *corev1.Pod) { p.Spec.ShareProcessNamespace = boolPtr(true) }, "spec.shareProcessNamespace"},
		{"extra gate, still held", func(p *corev1.Pod) {
			p.Spec.SchedulingGates = append(p.Spec.SchedulingGates, corev1.PodSchedulingGate{Name: "example.com/hold"})
		}, "spec.schedulingGates[example.com/hold]"},
		{"selected label changed", func(p *corev1.Pod) { p.Labels[labelComponent] = componentProxy }, "metadata.labels.wardyn.component"},
		{"selected label removed", func(p *corev1.Pod) { delete(p.Labels, labelRun) }, "metadata.labels.wardyn.run-id"},
		{"label added", func(p *corev1.Pod) { p.Labels["example.com/allow-egress"] = "true" }, "metadata.labels.example.com/allow-egress"},
		{"annotation added", func(p *corev1.Pod) {
			p.Annotations["container.apparmor.security.beta.kubernetes.io/agent"] = "unconfined"
		}, "metadata.annotations.container.apparmor.security.beta.kubernetes.io/agent"},
		{"placement annotation dropped", func(p *corev1.Pod) { delete(p.Annotations, "example.com/note") }, "metadata.annotations.example.com/note"},
		{"owner is another Sandbox", func(p *corev1.Pod) { p.OwnerReferences[0].UID = "foreign" }, owner + ".uid"},
		{"owner not controlling", func(p *corev1.Pod) { p.OwnerReferences[0].Controller = boolPtr(false) }, owner + ".controller"},
		{"owner blockOwnerDeletion off", func(p *corev1.Pod) { p.OwnerReferences[0].BlockOwnerDeletion = boolPtr(false) }, owner + ".blockOwnerDeletion"},
		{"second owner", func(p *corev1.Pod) {
			p.OwnerReferences = append(p.OwnerReferences, metav1.OwnerReference{APIVersion: "v1", Kind: "Pod", Name: "someone", UID: "x"})
		}, "metadata.ownerReferences[someone]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, cs, dyn := newSandboxRig(t, Config{SandboxPlacement: asPlacement}, controllerMaking(t, tc.mutate))
			_, err := d.CreateSandbox(context.Background(), asSpec())
			if err == nil {
				t.Fatal("CreateSandbox admitted a changed pod")
			}
			msg := err.Error()
			if !strings.Contains(msg, "("+tc.field+")") || !strings.Contains(msg, "Nothing ran") {
				t.Errorf("error = %q, want the field (%s) and \"Nothing ran\"", msg, tc.field)
			}
			assertRefusedAndTornDown(t, cs, dyn)
		})
	}
}

// The gate must still hold: Wardyn's gate present, no node, Pending, not scheduled, nothing started.
// Otherwise the run is refused as one that may have started, never with "Nothing ran".
func TestAgentSandbox_RefusesAPodNotHeldByTheGate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*corev1.Pod)
		field  string
	}{
		{"hook dropped the gate and it ran", func(p *corev1.Pod) {
			p.Spec.SchedulingGates, p.Spec.NodeName, p.Status.Phase = nil, "node-1", corev1.PodRunning
		}, "spec.schedulingGates"},
		{"bound to a node", func(p *corev1.Pod) { p.Spec.NodeName = "node-1" }, "spec.nodeName"},
		{"past Pending", func(p *corev1.Pod) { p.Status.Phase = corev1.PodRunning }, "status.phase"},
		{"scheduled", func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
		}, "status.conditions[PodScheduled]"},
		{"init container started", func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: stageAgentSecretsName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		}, "status.containerStatuses"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, cs, dyn := newSandboxRig(t, Config{}, controllerMaking(t, tc.mutate))
			_, err := d.CreateSandbox(context.Background(), asSpec())
			if err == nil {
				t.Fatal("CreateSandbox admitted a pod the gate did not hold")
			}
			msg := err.Error()
			if !strings.Contains(msg, "("+tc.field+")") || !strings.Contains(msg, "may have started") || strings.Contains(msg, "Nothing ran") {
				t.Errorf("error = %q, want the field (%s), \"may have started\", and never \"Nothing ran\"", msg, tc.field)
			}
			assertRefusedAndTornDown(t, cs, dyn)
		})
	}
}

// A controller whose pod create the API server refuses retries it fast; Wardyn reads the refusal off
// the Sandbox and deletes it at the next poll, not at the end of a timeout, and never creates it twice.
func TestAgentSandbox_ControllerRefusalDeletesTheSandboxAtOnce(t *testing.T) {
	quoted := `pods "wardyn-agent-x" is forbidden: violates PodSecurity "restricted:v1.30": ` + strings.Repeat("x", 1000)
	refused := func(sb *unstructured.Unstructured) (*unstructured.Unstructured, *corev1.Pod) {
		cond := map[string]any{"type": "Ready", "status": "False", "reason": "InvalidConfiguration", "message": quoted}
		if err := unstructured.SetNestedSlice(sb.Object, []any{cond}, "status", "conditions"); err != nil {
			t.Fatalf("set status: %v", err)
		}
		return sb, nil
	}
	d, cs, dyn := newSandboxRig(t, Config{}, refused)
	start := time.Now()
	_, err := d.CreateSandbox(context.Background(), asSpec())
	if err == nil {
		t.Fatal("CreateSandbox succeeded without a pod")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("refusal took %s; the Sandbox's condition, not the %s bound, must end the wait", elapsed, agentSandboxPodAppearWithin)
	}
	msg := err.Error()
	if !strings.Contains(msg, "could not create its pod") || !strings.Contains(msg, "violates PodSecurity") {
		t.Errorf("error = %q, want the controller's refusal quoted", msg)
	}
	if strings.Contains(msg, strings.Repeat("x", agentSandboxMessageCap)) {
		t.Errorf("error quotes %d bytes of the controller's message, want at most %d", len(msg), agentSandboxMessageCap)
	}
	if n := sandboxActions(dyn, "create"); n != 1 {
		t.Errorf("Sandbox creates = %d, want 1: Wardyn never retries", n)
	}
	assertRefusedAndTornDown(t, cs, dyn)
}

func TestAgentSandbox_NoPodWithinTheBoundIsRefused(t *testing.T) {
	prev := agentSandboxPodAppearWithin
	agentSandboxPodAppearWithin = 300 * time.Millisecond
	t.Cleanup(func() { agentSandboxPodAppearWithin = prev })
	silent := func(sb *unstructured.Unstructured) (*unstructured.Unstructured, *corev1.Pod) { return sb, nil }
	d, cs, dyn := newSandboxRig(t, Config{}, silent)
	_, err := d.CreateSandbox(context.Background(), asSpec())
	if err == nil || !strings.Contains(err.Error(), "made no pod within") {
		t.Fatalf("CreateSandbox err = %v, want the no-pod refusal", err)
	}
	assertRefusedAndTornDown(t, cs, dyn)
}

func TestAgentSandbox_DeletedSandboxIsRefused(t *testing.T) {
	gone := func(*unstructured.Unstructured) (*unstructured.Unstructured, *corev1.Pod) { return nil, nil }
	d, cs, _ := newSandboxRig(t, Config{}, gone)
	_, err := d.CreateSandbox(context.Background(), asSpec())
	if err == nil || !strings.Contains(err.Error(), "Sandbox was deleted") {
		t.Fatalf("CreateSandbox err = %v, want the deleted-Sandbox refusal", err)
	}
	assertRunObjectsGone(t, cs, asRunID)
}

// A Sandbox of the run's name that Wardyn did not make fails the create and is left alone.
func TestAgentSandbox_ForeignSandboxFailsAndIsLeftAlone(t *testing.T) {
	d, cs, dyn := newSandboxRig(t, Config{}, controllerMaking(t, nil))
	foreign := &unstructured.Unstructured{Object: map[string]any{"apiVersion": agentSandboxAPIVersion, "kind": "Sandbox"}}
	foreign.SetName(agentPodName(asRunID))
	foreign.SetNamespace(testNamespace)
	foreign.SetUID("foreign")
	if err := dyn.Tracker().Create(sandboxesGVR, foreign, testNamespace); err != nil {
		t.Fatalf("seed foreign Sandbox: %v", err)
	}
	_, err := d.CreateSandbox(context.Background(), asSpec())
	if err == nil || !strings.Contains(err.Error(), "create agent sandbox") {
		t.Fatalf("CreateSandbox err = %v, want the create to fail", err)
	}
	if n := sandboxActions(dyn, "delete"); n != 0 {
		t.Errorf("a foreign Sandbox was deleted %d time(s)", n)
	}
	if _, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, agentPodName(asRunID)); err != nil {
		t.Errorf("foreign Sandbox gone: %v", err)
	}
	assertRunObjectsGone(t, cs, asRunID)
}

// The seam between read-back and ungate sees the pod still held behind both NetworkPolicies.
func TestAgentSandbox_BeforeUngateSeesAHeldPodBehindItsNetworkPolicies(t *testing.T) {
	var cs *fake.Clientset
	called := false
	hook := func(ctx context.Context, ns, name string) error {
		called = true
		pod, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if len(pod.Spec.SchedulingGates) != 1 || pod.Spec.NodeName != "" {
			t.Errorf("at the seam the pod has gates %+v and node %q, want held", pod.Spec.SchedulingGates, pod.Spec.NodeName)
		}
		for _, np := range []string{agentNetPolName(asRunID), proxyNetPolName(asRunID)} {
			if _, err := cs.NetworkingV1().NetworkPolicies(ns).Get(ctx, np, metav1.GetOptions{}); err != nil {
				t.Errorf("NetworkPolicy %s missing at the seam: %v", np, err)
			}
		}
		if p := podPatches(cs); len(p) != 0 {
			t.Errorf("the gate was patched before the seam")
		}
		return nil
	}
	d, rigCS, _ := newSandboxRig(t, Config{AgentSandboxBeforeUngate: hook}, controllerMaking(t, nil))
	cs = rigCS
	if _, err := d.CreateSandbox(context.Background(), asSpec()); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if !called {
		t.Error("the seam was never called")
	}
}

func TestAgentSandbox_BeforeUngateErrorRefusesTheRun(t *testing.T) {
	hook := func(context.Context, string, string) error { return errors.New("held by the test") }
	d, cs, dyn := newSandboxRig(t, Config{AgentSandboxBeforeUngate: hook}, controllerMaking(t, nil))
	_, err := d.CreateSandbox(context.Background(), asSpec())
	if err == nil || !strings.Contains(err.Error(), "held by the test") {
		t.Fatalf("CreateSandbox err = %v, want the seam's error", err)
	}
	assertRefusedAndTornDown(t, cs, dyn)
}

// failGatePatches makes the first n gate patches fail as the API server fails them, after change has
// run on the stored pod (call is 1-based); later patches reach the fake's own JSON patch.
func failGatePatches(t *testing.T, cs *fake.Clientset, n int, fail func(call int) error, change func(call int, p *corev1.Pod)) {
	t.Helper()
	calls := 0
	cs.PrependReactor("patch", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if !strings.HasPrefix(a.(clienttesting.PatchAction).GetName(), agentPodNamePrefix) {
			return false, nil, nil
		}
		calls++
		if calls > n {
			return false, nil, nil
		}
		changeAgentPod(t, cs, agentPodName(asRunID), func(p *corev1.Pod) { change(calls, p) })
		return true, nil, fail(calls)
	})
}

// testOpFailed is what the API server answers when a JSON patch's test op fails: 422 Invalid.
func testOpFailed(int) error {
	return apierrors.NewGenericServerResponse(http.StatusUnprocessableEntity, "", schema.GroupResource{}, "", "testing value /metadata/resourceVersion failed", 0, false)
}

// The scheduler's SchedulingGated condition write moves a gated pod's resourceVersion between the
// read-back and the gate patch. The failed patch is followed by a fresh read, the full check, and a
// patch pinned to the new resourceVersion.
func TestAgentSandbox_UngateRetriesAfterAResourceVersionBump(t *testing.T) {
	d, cs, dyn := newSandboxRig(t, Config{SandboxPlacement: asPlacement}, controllerMaking(t, nil))
	failGatePatches(t, cs, 1, testOpFailed, func(_ int, p *corev1.Pod) { p.ResourceVersion = "627" })
	cs.ClearActions()
	if _, err := d.CreateSandbox(context.Background(), asSpec()); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	patches := podPatches(cs)
	if len(patches) != 2 {
		t.Fatalf("pod patches = %d, want 2: the failed one and its retry", len(patches))
	}
	var ops []jsonPatchOp
	if err := json.Unmarshal(patches[1].GetPatch(), &ops); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if len(ops) != 4 || ops[0].Value != string(asPodUID) || ops[1].Path != "/metadata/resourceVersion" || ops[1].Value != "627" {
		t.Errorf("retry patch = %+v, want the UID and the new resourceVersion 627 pinned", ops)
	}
	pod, err := cs.Tracker().Get(podsGVR, testNamespace, agentPodName(asRunID))
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	if gates := pod.(*corev1.Pod).Spec.SchedulingGates; len(gates) != 0 {
		t.Errorf("gates after the retry = %+v, want none", gates)
	}
	if n := sandboxActions(dyn, "delete"); n != 0 {
		t.Errorf("an admitted run's Sandbox was deleted %d time(s)", n)
	}
}

// A pod changed in a checked field between attempts is refused by the re-check, naming the field.
func TestAgentSandbox_UngateRetryRechecksThePod(t *testing.T) {
	d, cs, dyn := newSandboxRig(t, Config{SandboxPlacement: asPlacement}, controllerMaking(t, nil))
	failGatePatches(t, cs, 1, testOpFailed, func(_ int, p *corev1.Pod) {
		p.ResourceVersion = "627"
		p.Labels["example.com/allow-egress"] = "true"
	})
	cs.ClearActions()
	_, err := d.CreateSandbox(context.Background(), asSpec())
	if err == nil || !strings.Contains(err.Error(), "(metadata.labels.example.com/allow-egress)") || !strings.Contains(err.Error(), "Nothing ran") {
		t.Fatalf("CreateSandbox err = %v, want the changed field and \"Nothing ran\"", err)
	}
	if n := len(podPatches(cs)); n != 1 {
		t.Errorf("pod patches = %d, want 1: no retry of a pod that fails the check", n)
	}
	if n := sandboxActions(dyn, "delete"); n != 1 {
		t.Errorf("Sandbox deletes = %d, want 1", n)
	}
	assertRunObjectsGone(t, cs, asRunID)
}

// A pod replaced under the same name between attempts is refused. "Nothing ran" only when the new
// pod is proven still held; an ungated or running one may have started.
func TestAgentSandbox_UngateRetryRefusesAReplacedPod(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*corev1.Pod)
		want   string
	}{
		{"still held", func(*corev1.Pod) {}, "(metadata.uid). Nothing ran"},
		{"gate gone", func(p *corev1.Pod) { p.Spec.SchedulingGates = nil }, "(spec.schedulingGates), so it may have started"},
		{"running", func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: mainContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		}, "(status.containerStatuses), so it may have started"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, cs, dyn := newSandboxRig(t, Config{SandboxPlacement: asPlacement}, controllerMaking(t, nil))
			failGatePatches(t, cs, 1, testOpFailed, func(_ int, p *corev1.Pod) {
				p.UID = "replaced-pod-uid"
				tc.change(p)
			})
			_, err := d.CreateSandbox(context.Background(), asSpec())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateSandbox err = %v, want %q", err, tc.want)
			}
			if strings.Contains(tc.want, "may have started") && strings.Contains(err.Error(), "Nothing ran") {
				t.Errorf("err = %q says nothing ran for a pod that may have started", err)
			}
			if n := sandboxActions(dyn, "delete"); n != 1 {
				t.Errorf("Sandbox deletes = %d, want 1", n)
			}
		})
	}
}

// Three failed gate patches are a refusal, as one was before.
func TestAgentSandbox_UngateGivesUpAfterThreeAttempts(t *testing.T) {
	d, cs, dyn := newSandboxRig(t, Config{SandboxPlacement: asPlacement}, controllerMaking(t, nil))
	conflict := func(int) error {
		return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, agentPodName(asRunID), errors.New("changed"))
	}
	failGatePatches(t, cs, ungateAttempts, conflict, func(call int, p *corev1.Pod) { p.ResourceVersion = fmt.Sprint(626 + call) })
	cs.ClearActions()
	_, err := d.CreateSandbox(context.Background(), asSpec())
	if err == nil || !strings.Contains(err.Error(), "its gate could not be removed") {
		t.Fatalf("CreateSandbox err = %v, want the gate-patch refusal", err)
	}
	if n := len(podPatches(cs)); n != ungateAttempts {
		t.Errorf("pod patches = %d, want %d", n, ungateAttempts)
	}
	if n := sandboxActions(dyn, "delete"); n != 1 {
		t.Errorf("Sandbox deletes = %d, want 1", n)
	}
	assertRunObjectsGone(t, cs, asRunID)
}

func TestReadBack_EveryAllowanceHasAReason(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range readBackAllowed {
		if a.reason == "" {
			t.Errorf("allowance %q has no reason", a.key)
		}
		if seen[a.key] {
			t.Errorf("allowance %q listed twice", a.key)
		}
		seen[a.key] = true
	}
}

// A placement key in the controller's reserved groups never reaches the backend: placement refuses
// every key under k8s.io, which covers agents.x-k8s.io and extensions.agents.x-k8s.io.
func TestAgentSandbox_PlacementRefusesControllerReservedKeys(t *testing.T) {
	for _, p := range []string{
		`{"podLabels":{"agents.x-k8s.io/sandbox-name-hash":"x"}}`,
		`{"podAnnotations":{"extensions.agents.x-k8s.io/claim":"x"}}`,
	} {
		if _, err := parsePlacement(p); err == nil {
			t.Errorf("parsePlacement(%s) accepted a controller-reserved key", p)
		}
	}
}

func TestAgentSandboxBoot_Refusals(t *testing.T) {
	cases := []struct {
		name      string
		served    []*metav1.APIResourceList
		deny      string
		reviewErr error
		want      string
	}{
		{"API not served", nil, "", nil, "does not serve " + agentSandboxAPIVersion},
		{"no sandboxes resource", []*metav1.APIResourceList{{GroupVersion: agentSandboxAPIVersion, APIResources: []metav1.APIResource{{Name: "sandboxtemplates"}}}}, "", nil, "serves no sandboxes resource"},
		{"pods patch denied", servedSandboxes(), "patch pods", nil, "needs patch on pods"},
		{"sandboxes create denied", servedSandboxes(), "create sandboxes", nil, "needs create on sandboxes.agents.x-k8s.io"},
		{"sandboxes list denied", servedSandboxes(), "list sandboxes", nil, "needs list on sandboxes.agents.x-k8s.io"},
		{"review fails", servedSandboxes(), "", errors.New("selfsubjectaccessreviews is forbidden"), "could not review"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset()
			installCanaryReactor(t, cs, false)
			cs.Resources = tc.served
			installAccessReactor(cs, tc.deny, tc.reviewErr)
			useDynamicClient(t, newFakeDynamic())
			d, err := newWithClient(context.Background(), cs, testRestConfig(), Config{AgentSandbox: true, Namespaces: []string{testNamespace}, ProxyImage: "p"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("newWithClient = %v, %v; want a refusal containing %q", d, err, tc.want)
			}
		})
	}
}

// Either NetworkPolicy override is refused when the backend is on, before any API call.
func TestAgentSandboxBoot_RefusesNetworkPolicyOverrides(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Config
		setting string
	}{
		{"allow unenforced", Config{AllowUnenforcedNetPol: true}, "WARDYN_K8S_ALLOW_UNENFORCED_NETPOL"},
		{"ack ambient default deny", Config{AckAmbientDefaultDeny: true}, "WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset()
			installCanaryReactor(t, cs, true)
			cs.Resources = servedSandboxes()
			installAccessReactor(cs, "", nil)
			useDynamicClient(t, newFakeDynamic())
			cfg := tc.cfg
			cfg.AgentSandbox, cfg.Namespaces, cfg.ProxyImage = true, []string{testNamespace}, "p"
			_, err := newWithClient(context.Background(), cs, testRestConfig(), cfg)
			if err == nil || !strings.Contains(err.Error(), tc.setting) {
				t.Fatalf("newWithClient err = %v, want a refusal naming %s", err, tc.setting)
			}
			if a := cs.Actions(); len(a) != 0 {
				t.Errorf("the refusal came after %d API call(s), first %s %s; want none", len(a), a[0].GetVerb(), a[0].GetResource().Resource)
			}
		})
	}
}

// With no override set, every canary verdict but enforced refuses to boot the backend.
func TestAgentSandboxBoot_RequiresAnEnforcedCanaryVerdict(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		phaseAExit, phaseBExit int32
	}{
		{"unenforced", 0, 0},
		{"ambient default-deny shape", 1, 1},
		{"indeterminate", 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset()
			installCanaryReactorExitCodes(t, cs, tc.phaseAExit, tc.phaseBExit)
			cs.Resources = servedSandboxes()
			installAccessReactor(cs, "", nil)
			useDynamicClient(t, newFakeDynamic())
			d, err := newWithClient(context.Background(), cs, testRestConfig(), Config{AgentSandbox: true, Namespaces: []string{testNamespace}, ProxyImage: "p"})
			if err == nil || d != nil {
				t.Fatalf("newWithClient booted the backend on a %s canary", tc.name)
			}
		})
	}
	cs := fake.NewClientset()
	installCanaryReactor(t, cs, false)
	cs.Resources = servedSandboxes()
	installAccessReactor(cs, "", nil)
	dyn := newFakeDynamic()
	useDynamicClient(t, dyn)
	d, err := newWithClient(context.Background(), cs, testRestConfig(), Config{AgentSandbox: true, Namespaces: []string{testNamespace}, ProxyImage: "p"})
	if err != nil {
		t.Fatalf("enforced canary: newWithClient: %v", err)
	}
	if d.dynamic != dynamic.Interface(dyn) {
		t.Error("an on driver does not hold the dynamic client")
	}
}

func TestAgentSandboxBoot_RefusesRunnerMode(t *testing.T) {
	_, err := bootAgentSandbox(context.Background(), fake.NewClientset(), testRestConfig(), Config{AgentSandbox: true, RunnerID: "runner-1"})
	if !errors.Is(err, errAgentSandboxRunner) {
		t.Fatalf("bootAgentSandbox under a runner id = %v, want errAgentSandboxRunner", err)
	}
}

func TestBuildConfig_AgentSandboxFromEnv(t *testing.T) {
	t.Setenv("WARDYN_K8S_AGENT_SANDBOX", "")
	if buildConfig(substrate.Deps{}).AgentSandbox {
		t.Error("AgentSandbox is on with WARDYN_K8S_AGENT_SANDBOX unset")
	}
	t.Setenv("WARDYN_K8S_AGENT_SANDBOX", "true")
	if !buildConfig(substrate.Deps{}).AgentSandbox {
		t.Error("AgentSandbox is off with WARDYN_K8S_AGENT_SANDBOX=true")
	}
}

func TestNewRunner_RefusesTheAgentSandboxBackend(t *testing.T) {
	t.Setenv("WARDYN_K8S_NAMESPACE", "")
	t.Setenv("WARDYN_K8S_AGENT_SANDBOX", "true")
	applied := false
	_, err := NewRunner(substrate.Deps{}, func(*Config) { applied = true })
	if !errors.Is(err, errAgentSandboxRunner) {
		t.Fatalf("NewRunner = %v, want errAgentSandboxRunner", err)
	}
	if applied {
		t.Error("the runner's own fields were applied before the refusal")
	}
}

var (
	asOverhead   = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("160Mi")}
	asClassNodes = map[string]string{"runtime.example.com/gvisor": "true"}
	asClassTols  = []corev1.Toleration{{Key: "runtime.example.com/gvisor", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
)

func asRuntimeClass(overhead corev1.ResourceList, nodes map[string]string, tols []corev1.Toleration) *nodev1.RuntimeClass {
	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "gvisor"}, Handler: "runsc"}
	if overhead != nil {
		rc.Overhead = &nodev1.Overhead{PodFixed: overhead}
	}
	if nodes != nil || tols != nil {
		rc.Scheduling = &nodev1.Scheduling{NodeSelector: nodes, Tolerations: tols}
	}
	return rc
}

// admitRuntimeClass does what RuntimeClass admission does to a pod of rc's class, after
// DefaultTolerationSeconds: overhead set, node selector merged, tolerations appended last.
func admitRuntimeClass(rc *nodev1.RuntimeClass) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		if rc.Overhead != nil {
			p.Spec.Overhead = rc.Overhead.PodFixed.DeepCopy()
		}
		if s := rc.Scheduling; s != nil {
			if p.Spec.NodeSelector == nil {
				p.Spec.NodeSelector = map[string]string{}
			}
			maps.Copy(p.Spec.NodeSelector, s.NodeSelector)
			p.Spec.Tolerations = append(p.Spec.Tolerations, s.Tolerations...)
		}
	}
}

// runRuntimeClassCase runs a CC2 run whose class is rc and whose admitted pod is the template with
// admitted's admission additions, then mutate.
func runRuntimeClassCase(t *testing.T, rc, admitted *nodev1.RuntimeClass, mutate func(*corev1.Pod)) (*fake.Clientset, *dynamicfake.FakeDynamicClient, error) {
	t.Helper()
	answer := controllerMaking(t, func(p *corev1.Pod) {
		admitRuntimeClass(admitted)(p)
		if mutate != nil {
			mutate(p)
		}
	})
	d, cs, dyn := newSandboxRig(t, Config{ConfinementRuntimes: map[types.ConfinementClass]string{types.CC2: rc.Name}}, answer)
	if _, err := cs.NodeV1().RuntimeClasses().Create(context.Background(), rc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create RuntimeClass: %v", err)
	}
	spec := asSpec()
	spec.ConfinementClass = types.CC2
	_, err := d.CreateSandbox(context.Background(), spec)
	return cs, dyn, err
}

// RuntimeClass admission's additions are expected exactly from the class: its overhead, its node
// selector, its tolerations. The Sandbox's template carries none of them.
func TestAgentSandbox_RuntimeClassAdditionsAreExpectedFromTheClass(t *testing.T) {
	for _, tc := range []struct {
		name string
		rc   *nodev1.RuntimeClass
	}{
		{"overhead", asRuntimeClass(asOverhead, nil, nil)},
		{"node selector", asRuntimeClass(nil, asClassNodes, nil)},
		{"toleration", asRuntimeClass(nil, nil, asClassTols)},
		{"all three", asRuntimeClass(asOverhead, asClassNodes, asClassTols)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, dyn, err := runRuntimeClassCase(t, tc.rc, tc.rc, nil)
			if err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			if p := podPatches(cs); len(p) != 1 {
				t.Errorf("pod patches = %d, want the one ungate", len(p))
			}
			obj, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, agentPodName(asRunID))
			if err != nil {
				t.Fatalf("get Sandbox: %v", err)
			}
			tmpl := podTemplateOf(t, obj.(*unstructured.Unstructured))
			if tmpl.Spec.RuntimeClassName == nil || *tmpl.Spec.RuntimeClassName != tc.rc.Name {
				t.Errorf("template runtimeClassName = %v, want %q", tmpl.Spec.RuntimeClassName, tc.rc.Name)
			}
			if len(tmpl.Spec.Overhead) != 0 || len(tmpl.Spec.NodeSelector) != 0 || len(tmpl.Spec.Tolerations) != 0 {
				t.Errorf("template carries admission's additions: overhead %v, nodeSelector %v, tolerations %v", tmpl.Spec.Overhead, tmpl.Spec.NodeSelector, tmpl.Spec.Tolerations)
			}
		})
	}
}

func TestAgentSandbox_RuntimeClassAdditionsAreComparedExactly(t *testing.T) {
	full := asRuntimeClass(asOverhead, asClassNodes, asClassTols)
	cpuOnly := asRuntimeClass(corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}, nil, nil)
	plain := asRuntimeClass(nil, nil, nil)
	for _, tc := range []struct {
		name         string
		rc, admitted *nodev1.RuntimeClass
		mutate       func(*corev1.Pod)
		field        string
	}{
		{"overhead differs", full, full, func(p *corev1.Pod) { p.Spec.Overhead[corev1.ResourceCPU] = resource.MustParse("500m") }, "spec.overhead.cpu"},
		{"overhead extra value", cpuOnly, asRuntimeClass(asOverhead, nil, nil), nil, "spec.overhead.memory"},
		{"overhead with none in the class", plain, cpuOnly, nil, "spec.overhead.cpu"},
		{"node selector differs", full, full, func(p *corev1.Pod) { p.Spec.NodeSelector["runtime.example.com/gvisor"] = "false" }, "spec.nodeSelector.runtime.example.com/gvisor"},
		{"node selector extra", full, full, func(p *corev1.Pod) { p.Spec.NodeSelector["pool"] = "any" }, "spec.nodeSelector.pool"},
		{"toleration differs", full, full, func(p *corev1.Pod) {
			p.Spec.Tolerations[len(p.Spec.Tolerations)-1].Effect = corev1.TaintEffectNoExecute
		}, "spec.tolerations[0]"},
		{"toleration extra", full, full, func(p *corev1.Pod) {
			p.Spec.Tolerations = append(p.Spec.Tolerations, corev1.Toleration{Key: "dedicated", Operator: corev1.TolerationOpExists})
		}, "spec.tolerations[3]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, dyn, err := runRuntimeClassCase(t, tc.rc, tc.admitted, tc.mutate)
			if err == nil {
				t.Fatal("CreateSandbox admitted a pod that differs from its RuntimeClass")
			}
			if !strings.Contains(err.Error(), "("+tc.field+")") {
				t.Errorf("error = %q, want the field (%s)", err, tc.field)
			}
			assertRefusedAndTornDown(t, cs, dyn)
		})
	}
}
