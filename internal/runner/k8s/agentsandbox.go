// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// The optional agent-sandbox backend (Config.AgentSandbox, experimental, server mode only). The agent
// pod is made by a bare agents.x-k8s.io/v1beta1 Sandbox; the proxy pod, Secret and NetworkPolicies
// stay Wardyn's. The pod template carries a scheduling gate, and Wardyn removes it only after reading
// the admitted pod back and finding the pod it asked for. The gate holds unless the controller or an
// admission hook removes it: a pod read back without it is refused as one that may have started.

const (
	agentSandboxGroup      = "agents.x-k8s.io"
	agentSandboxAPIVersion = agentSandboxGroup + "/v1beta1"
	agentSandboxGate       = "wardyn.dev/verify"
	// agentSandboxMessageCap bounds the controller's own words quoted into a refusal: an API
	// validation message can print a whole struct.
	agentSandboxMessageCap    = 300
	agentSandboxDeleteTimeout = 30 * time.Second
	// ungateAttempts bounds the gate patches of one admission; each after the first follows a fresh
	// read and a full re-check.
	ungateAttempts = 3
)

var sandboxesGVR = schema.GroupVersionResource{Group: agentSandboxGroup, Version: "v1beta1", Resource: "sandboxes"}

// sandboxTemplatesGVR is the SandboxTemplate CRD of agent-sandbox v1.0.9's extensions manifest. Wardyn
// only reads it (WARDYN_K8S_AGENT_SANDBOX_TEMPLATE); no extensions controller is involved.
var sandboxTemplatesGVR = schema.GroupVersionResource{Group: "extensions." + agentSandboxGroup, Version: "v1beta1", Resource: "sandboxtemplates"}

// agentSandboxPodAppearWithin bounds the wait for the controller's pod. On kind with agent-sandbox
// v1.0.9 the pod appeared in the Sandbox's own second. A var so tests can shorten it.
var agentSandboxPodAppearWithin = 30 * time.Second

// newDynamicClient is the test seam for the one dynamic client the backend uses.
var newDynamicClient = func(c *rest.Config) (dynamic.Interface, error) { return dynamic.NewForConfig(c) }

var (
	errAgentSandboxRunner = errors.New("k8s: the agent-sandbox backend does not run under wardyn-runnerd; unset WARDYN_K8S_AGENT_SANDBOX")
	// errAgentSandboxNetPol: the gate releases a pod only into NetworkPolicies the canary proved hold.
	errAgentSandboxNetPol = errors.New("k8s: refusing to boot: WARDYN_K8S_AGENT_SANDBOX runs only where the egress canary proved NetworkPolicy enforced in every runs namespace")
)

// agentSandboxAccess is every verb the backend needs in a runs namespace: the Sandbox's create, read
// and delete, the orphan sweep's list, and the pod patch that removes the gate. A named template adds
// agentSandboxTemplateAccess.
var agentSandboxAccess = []authzv1.ResourceAttributes{
	{Group: agentSandboxGroup, Resource: "sandboxes", Verb: "create"},
	{Group: agentSandboxGroup, Resource: "sandboxes", Verb: "get"},
	{Group: agentSandboxGroup, Resource: "sandboxes", Verb: "list"},
	{Group: agentSandboxGroup, Resource: "sandboxes", Verb: "delete"},
	{Resource: "pods", Verb: "patch"},
}

var agentSandboxTemplateAccess = authzv1.ResourceAttributes{Group: sandboxTemplatesGVR.Group, Resource: sandboxTemplatesGVR.Resource, Verb: "get"}

// checkAgentSandboxTemplate refuses a template named without the backend, or a name no object can have.
func checkAgentSandboxTemplate(cfg Config) error {
	switch {
	case cfg.AgentSandboxTemplate == "":
		return nil
	case !cfg.AgentSandbox:
		return errors.New("k8s: refusing to boot: WARDYN_K8S_AGENT_SANDBOX_TEMPLATE needs WARDYN_K8S_AGENT_SANDBOX on")
	case len(validation.IsDNS1123Subdomain(cfg.AgentSandboxTemplate)) > 0:
		return fmt.Errorf("k8s: refusing to boot: WARDYN_K8S_AGENT_SANDBOX_TEMPLATE %q is not a valid object name", cfg.AgentSandboxTemplate)
	}
	return nil
}

// bootAgentSandbox refuses to boot the backend under runnerd, beside either NetworkPolicy override
// (before any API call), on a cluster that does not serve Sandboxes at the one API version Wardyn was
// tested with, or for a service account missing any verb in agentSandboxAccess in a runs namespace. A
// named template must be readable and acceptable in every runs namespace. A failed check refuses too.
// Otherwise it builds the dynamic client. New also requires the canary's verdict to be enforced
// (errAgentSandboxNetPol).
func bootAgentSandbox(ctx context.Context, cs kubernetes.Interface, restCfg *rest.Config, cfg Config) (dynamic.Interface, error) {
	switch {
	case cfg.RunnerID != "":
		return nil, errAgentSandboxRunner
	case cfg.AllowUnenforcedNetPol:
		return nil, errors.New("k8s: refusing to boot: WARDYN_K8S_AGENT_SANDBOX runs only where NetworkPolicy is proven enforced; unset WARDYN_K8S_ALLOW_UNENFORCED_NETPOL")
	case cfg.AckAmbientDefaultDeny:
		return nil, errors.New("k8s: refusing to boot: WARDYN_K8S_AGENT_SANDBOX runs only where NetworkPolicy is proven enforced; unset WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY")
	}
	if err := checkAgentSandboxTemplate(cfg); err != nil {
		return nil, err
	}
	access := agentSandboxAccess
	if cfg.AgentSandboxTemplate != "" {
		access = append(slices.Clone(access), agentSandboxTemplateAccess)
	}
	served, err := cs.Discovery().ServerResourcesForGroupVersion(agentSandboxAPIVersion)
	if err != nil {
		return nil, fmt.Errorf("k8s: refusing to boot: WARDYN_K8S_AGENT_SANDBOX is on, but the cluster does not serve %s (install agent-sandbox, or unset it): %w", agentSandboxAPIVersion, err)
	}
	if !slices.ContainsFunc(served.APIResources, func(r metav1.APIResource) bool { return r.Name == sandboxesGVR.Resource }) {
		return nil, fmt.Errorf("k8s: refusing to boot: WARDYN_K8S_AGENT_SANDBOX is on, but %s serves no sandboxes resource", agentSandboxAPIVersion)
	}
	for _, ns := range cfg.Namespaces {
		for _, ra := range access {
			ra.Namespace = ns
			review := &authzv1.SelfSubjectAccessReview{Spec: authzv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &ra}}
			got, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
			if err != nil {
				return nil, fmt.Errorf("k8s: refusing to boot: could not review the agent-sandbox permissions in %q: %w", ns, err)
			}
			if !got.Status.Allowed {
				return nil, fmt.Errorf("k8s: refusing to boot: WARDYN_K8S_AGENT_SANDBOX needs %s on %s in %q, which this service account does not hold",
					ra.Verb, strings.TrimSuffix(ra.Resource+"."+ra.Group, "."), ns)
			}
		}
	}
	dyn, err := newDynamicClient(restCfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: build dynamic client: %w", err)
	}
	if cfg.AgentSandboxTemplate != "" {
		for _, ns := range cfg.Namespaces {
			if _, err := readSandboxTemplate(ctx, dyn, ns, cfg.AgentSandboxTemplate); err != nil {
				return nil, fmt.Errorf("k8s: refusing to boot: WARDYN_K8S_AGENT_SANDBOX_TEMPLATE: %w", err)
			}
		}
	}
	return dyn, nil
}

// readSandboxTemplate reads the named SandboxTemplate in ns and parses what Wardyn takes from it.
func readSandboxTemplate(ctx context.Context, dyn dynamic.Interface, ns, name string) (sandboxTemplateBase, error) {
	obj, err := dyn.Resource(sandboxTemplatesGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return sandboxTemplateBase{}, fmt.Errorf("SandboxTemplate %s/%s could not be read (the CRD is agent-sandbox v1.0.9's, from extensions.yaml): %w", ns, name, err)
	}
	base, err := parseSandboxTemplate(obj.Object)
	if err != nil {
		return sandboxTemplateBase{}, fmt.Errorf("SandboxTemplate %s/%s: %w", ns, name, err)
	}
	return base, nil
}

// templatedAgentPod is pod with the named template laid on, read afresh for each run so an edit after boot
// is checked too. No template: pod itself.
func (d *Driver) templatedAgentPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	if d.cfg.AgentSandboxTemplate == "" {
		return pod, nil
	}
	base, err := readSandboxTemplate(ctx, d.dynamic, pod.Namespace, d.cfg.AgentSandboxTemplate)
	if err != nil {
		return nil, fmt.Errorf("k8s: agent sandbox: %w", err)
	}
	out := pod.DeepCopy()
	if err := base.applyTo(out); err != nil {
		return nil, fmt.Errorf("k8s: agent sandbox: SandboxTemplate %s/%s: %w", pod.Namespace, d.cfg.AgentSandboxTemplate, err)
	}
	return out, nil
}

// createAgentSandbox is CreateSandbox's agent step when the backend is on: create the Sandbox, wait
// for the controller's pod, refuse any pod but the one asked for, then remove the gate. A refusal
// deletes the Sandbox at once, before the caller's teardown, so a controller retrying a pod the API
// server refused stops (one was seen retrying thousands of times in five minutes).
func (d *Driver) createAgentSandbox(ctx context.Context, clock *startClock, pod *corev1.Pod) error {
	pod, err := d.templatedAgentPod(ctx, pod)
	if err != nil {
		return err
	}
	want, err := d.runtimeClassAdmitted(ctx, pod)
	if err != nil {
		return err
	}
	sb, err := agentSandboxObject(pod)
	if err != nil {
		return err
	}
	created, err := d.dynamic.Resource(sandboxesGVR).Namespace(pod.Namespace).Create(ctx, sb, metav1.CreateOptions{})
	if err != nil {
		// AlreadyExists is a Sandbox this run did not make: it is left alone and the run fails.
		return fmt.Errorf("k8s: create agent sandbox: %w", err)
	}
	if err := d.admitAgentSandboxPod(ctx, clock, want, created.GetUID()); err != nil {
		d.deleteAgentSandbox(pod.Namespace, pod.Name, created.GetUID())
		return err
	}
	return nil
}

// agentSandboxObject is the bare Sandbox for pod. The Sandbox takes the pod's name, namespace, labels
// and owner (the proxy pod, so deleting the proxy still collects it); its template takes the labels,
// annotations and spec, plus the gate. No Service, a failed pod kept for its phase, and nothing else:
// no lifecycle, no operating mode, and no volume claim templates, whose claims the Sandbox would own
// and delete with it.
func agentSandboxObject(pod *corev1.Pod) (*unstructured.Unstructured, error) {
	spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(gatedSpec(pod))
	if err != nil {
		return nil, fmt.Errorf("k8s: agent sandbox: convert the pod spec: %w", err)
	}
	meta := map[string]any{}
	for k, m := range map[string]map[string]string{"labels": pod.Labels, "annotations": pod.Annotations} {
		if len(m) > 0 {
			vals := make(map[string]any, len(m))
			for mk, v := range m {
				vals[mk] = v
			}
			meta[k] = vals
		}
	}
	sb := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": agentSandboxAPIVersion,
		"kind":       "Sandbox",
		"spec": map[string]any{
			"podTemplate":     map[string]any{"metadata": meta, "spec": spec},
			"service":         false,
			"failedPodPolicy": "Retain",
		},
	}}
	sb.SetName(pod.Name)
	sb.SetNamespace(pod.Namespace)
	sb.SetLabels(pod.Labels)
	sb.SetOwnerReferences(pod.OwnerReferences)
	return sb, nil
}

// runtimeClassAdmitted is pod as RuntimeClass admission admits it, which is what the read-back
// expects: the class's fixed overhead set, its node selector merged in, its tolerations added. Read
// before the Sandbox exists; the Sandbox's template never carries these.
func (d *Driver) runtimeClassAdmitted(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	want := pod.DeepCopy()
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName == "" {
		return want, nil
	}
	rc, err := d.clientset.NodeV1().RuntimeClasses().Get(ctx, *pod.Spec.RuntimeClassName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("k8s: agent sandbox: read RuntimeClass %q: %w", *pod.Spec.RuntimeClassName, err)
	}
	if rc.Overhead != nil && len(rc.Overhead.PodFixed) > 0 {
		want.Spec.Overhead = rc.Overhead.PodFixed.DeepCopy()
	}
	if rc.Scheduling == nil {
		return want, nil
	}
	if len(rc.Scheduling.NodeSelector) > 0 && want.Spec.NodeSelector == nil {
		want.Spec.NodeSelector = map[string]string{}
	}
	for k, v := range rc.Scheduling.NodeSelector {
		if _, set := want.Spec.NodeSelector[k]; !set {
			want.Spec.NodeSelector[k] = v
		}
	}
	for _, tol := range rc.Scheduling.Tolerations {
		if !slices.ContainsFunc(want.Spec.Tolerations, func(t corev1.Toleration) bool { return reflect.DeepEqual(t, tol) }) {
			want.Spec.Tolerations = append(want.Spec.Tolerations, tol)
		}
	}
	return want, nil
}

func gatedSpec(pod *corev1.Pod) *corev1.PodSpec {
	spec := pod.Spec.DeepCopy()
	spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: agentSandboxGate}}
	return spec
}

// admitAgentSandboxPod reads the controller's pod back and removes its gate only if the pod is still
// held and is the pod Wardyn asked for. The scheduler writes a SchedulingGated condition on a gated pod,
// which moves its resourceVersion, so a gate patch whose test ops failed reads the pod again, checks it
// again in full, and tries again, up to ungateAttempts patches in all.
func (d *Driver) admitAgentSandboxPod(ctx context.Context, clock *startClock, want *corev1.Pod, sandboxUID k8stypes.UID) error {
	got, err := d.awaitAgentSandboxPod(ctx, clock, want.Namespace, want.Name)
	if err != nil {
		return err
	}
	if err := checkAgentSandboxPod(want, got, sandboxUID); err != nil {
		return err
	}
	if hook := d.cfg.AgentSandboxBeforeUngate; hook != nil {
		if err := hook(ctx, got.Namespace, got.Name); err != nil {
			return fmt.Errorf("k8s: agent sandbox: before removing the gate: %w", err)
		}
	}
	for attempt := 1; ; attempt++ {
		perr := d.ungate(ctx, got)
		if perr == nil {
			return nil
		}
		if attempt == ungateAttempts || !(apierrors.IsInvalid(perr) || apierrors.IsConflict(perr)) {
			return fmt.Errorf("k8s: agent sandbox: this run did not start: its pod changed while Wardyn checked it, or its gate could not be removed: %w", perr)
		}
		fresh, err := d.readPod(ctx, got.Namespace, got.Name)
		if err != nil {
			return fmt.Errorf("k8s: agent sandbox: this run did not start: its pod could not be read again after its gate patch failed: %w", err)
		}
		if fresh.UID != got.UID {
			if err := heldAgentSandboxPod(fresh); err != nil {
				return err
			}
			return errors.New("k8s: agent sandbox: this run did not start: its pod was replaced while Wardyn checked it (metadata.uid). Nothing ran. Ask your administrator")
		}
		if err := checkAgentSandboxPod(want, fresh, sandboxUID); err != nil {
			return err
		}
		got = fresh
	}
}

// heldAgentSandboxPod refuses a pod that may have run (gateNotHeld), never saying nothing ran.
func heldAgentSandboxPod(pod *corev1.Pod) error {
	if field := gateNotHeld(pod); field != "" {
		return fmt.Errorf("k8s: agent sandbox: this run did not start: its pod was not held for Wardyn's check (%s), so it may have started before Wardyn removed it. Ask your administrator", field)
	}
	return nil
}

// checkAgentSandboxPod is the read-back: got must still be held (gateNotHeld) and be the pod Wardyn
// asked for (readBackDiff).
func checkAgentSandboxPod(want, got *corev1.Pod, sandboxUID k8stypes.UID) error {
	if err := heldAgentSandboxPod(got); err != nil {
		return err
	}
	if field := readBackDiff(want, got, sandboxUID); field != "" {
		return fmt.Errorf("k8s: agent sandbox: this run did not start: the cluster changed the pod Wardyn asked for (%s). Nothing ran. Ask your administrator", field)
	}
	return nil
}

// awaitAgentSandboxPod polls for the controller's pod inside the run's start clock. While the pod is
// missing, the Sandbox says whether the controller's pod create was refused; that refusal, a deleted
// Sandbox, or no pod within agentSandboxPodAppearWithin ends the wait.
func (d *Driver) awaitAgentSandboxPod(ctx context.Context, clock *startClock, ns, name string) (*corev1.Pod, error) {
	deadline := time.Now().Add(agentSandboxPodAppearWithin)
	var pod *corev1.Pod
	var lastGetErr error
	err := clock.poll(ctx, func(pollCtx context.Context) (bool, bool, error) {
		p, gerr := d.readPod(pollCtx, ns, name)
		switch {
		case gerr == nil:
			pod = p
			return true, false, nil
		case tolerateGetError(gerr, &lastGetErr):
			return false, false, nil
		case !isNotFound(gerr):
			return false, false, gerr
		}
		if err := d.agentSandboxRefused(pollCtx, ns, name); err != nil {
			return false, false, err
		}
		if time.Now().After(deadline) {
			return false, false, fmt.Errorf("k8s: agent sandbox: this run did not start: the sandbox controller made no pod within %s. Nothing ran. Ask your administrator whether agent-sandbox is running", agentSandboxPodAppearWithin)
		}
		return false, false, nil
	})
	if err != nil {
		return nil, withLastGetError(err, lastGetErr)
	}
	return pod, nil
}

// agentSandboxRefused reads the Sandbox while its pod is missing. A deleted Sandbox, or a Ready
// condition False with reason InvalidConfiguration (how the controller reports a pod create the API
// server or an admission policy refused), is a refusal quoting the controller. A failed read is not.
func (d *Driver) agentSandboxRefused(ctx context.Context, ns, name string) error {
	sb, err := d.dynamic.Resource(sandboxesGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if isNotFound(err) {
		return errors.New("k8s: agent sandbox: this run did not start: its Sandbox was deleted before the controller made its pod. Nothing ran")
	}
	if err != nil {
		return nil
	}
	conds, _, _ := unstructured.NestedSlice(sb.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Ready" && m["status"] == "False" && m["reason"] == "InvalidConfiguration" {
			msg, _ := m["message"].(string)
			if len(msg) > agentSandboxMessageCap {
				msg = strings.ToValidUTF8(msg[:agentSandboxMessageCap], "") + "..."
			}
			return fmt.Errorf("k8s: agent sandbox: this run did not start: the sandbox controller could not create its pod: %s. Nothing ran. Ask your administrator", msg)
		}
	}
	return nil
}

// gateNotHeld names the first sign that pod may have run: Wardyn's gate gone, a node bound, phase
// past Pending, a PodScheduled condition, or a started container. "" while the pod is held.
func gateNotHeld(pod *corev1.Pod) string {
	switch {
	case !slices.ContainsFunc(pod.Spec.SchedulingGates, func(g corev1.PodSchedulingGate) bool { return g.Name == agentSandboxGate }):
		return "spec.schedulingGates"
	case pod.Spec.NodeName != "":
		return "spec.nodeName"
	case pod.Status.Phase != corev1.PodPending:
		return "status.phase"
	case slices.ContainsFunc(pod.Status.Conditions, func(c corev1.PodCondition) bool {
		return c.Type == corev1.PodScheduled && c.Status == corev1.ConditionTrue
	}):
		return "status.conditions[PodScheduled]"
	case slices.ContainsFunc(slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses), func(s corev1.ContainerStatus) bool {
		return s.State.Running != nil || s.State.Terminated != nil
	}):
		return "status.containerStatuses"
	}
	return ""
}

// ungate removes the gate in one guarded patch: test ops pin the pod's UID, the resourceVersion the
// read-back saw and the gate list itself, so a pod changed or replaced since the check stays held.
func (d *Driver) ungate(ctx context.Context, pod *corev1.Pod) error {
	patch, err := json.Marshal([]jsonPatchOp{
		{Op: "test", Path: "/metadata/uid", Value: string(pod.UID)},
		{Op: "test", Path: "/metadata/resourceVersion", Value: pod.ResourceVersion},
		{Op: "test", Path: "/spec/schedulingGates", Value: []corev1.PodSchedulingGate{{Name: agentSandboxGate}}},
		{Op: "remove", Path: "/spec/schedulingGates"},
	})
	if err != nil {
		return err
	}
	_, err = d.clientset.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, k8stypes.JSONPatchType, patch, metav1.PatchOptions{})
	return err
}

// deleteAgentSandbox deletes this run's Sandbox, and only it (a UID precondition), when a create is
// refused. A failure is logged: the caller's teardown still deletes the proxy pod that owns it.
func (d *Driver) deleteAgentSandbox(ns, name string, uid k8stypes.UID) {
	ctx, cancel := context.WithTimeout(context.Background(), agentSandboxDeleteTimeout)
	defer cancel()
	err := d.dynamic.Resource(sandboxesGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if err != nil && !isNotFound(err) {
		slog.Error("wardynd: k8s substrate: could not delete a refused agent sandbox; deleting its proxy pod collects it",
			slog.String("namespace", ns), slog.String("sandbox", name), slog.String("error", err.Error()))
	}
}

// readBackAllowance is a value the read-back accepts where Wardyn set none. Key is the field path
// with "[]" for any list item; values are the accepted values (nil: any). A "[]" key admits extra
// list items equal to one of values.
type readBackAllowance struct {
	key    string
	values []any
	reason string
}

// defaultToleration is one of the two tolerations the DefaultTolerationSeconds admission plugin adds,
// in the converter's form.
func defaultToleration(key string) map[string]any {
	return map[string]any{"key": key, "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": int64(300)}
}

// readBackAllowed is every field the admitted pod carried that Wardyn's template did not, as read on
// kind v1.30 with agent-sandbox v1.0.9 (a gated pod in a PSS-restricted namespace). Anything else
// Wardyn left empty is refused, as is any other value here.
var readBackAllowed = []readBackAllowance{
	{"metadata.labels.agents.x-k8s.io/sandbox-name-hash", nil,
		"controller: its own pod selector (the Sandbox's status.selector), in its reserved group; no Wardyn NetworkPolicy selects it"},
	{"metadata.annotations.agents.x-k8s.io/propagated-labels", nil,
		"controller: its record of the template labels it copied; nothing on the node reads it"},
	{"metadata.annotations.agents.x-k8s.io/propagated-annotations", nil,
		"controller: its record of the template annotations it copied; nothing on the node reads it"},
	{"metadata.ownerReferences[].blockOwnerDeletion", []any{true},
		"controller: set on the controlling reference to this run's Sandbox, which the read-back requires"},
	{"spec.containers[].imagePullPolicy", []any{"IfNotPresent", "Always"},
		"API default from the image reference: IfNotPresent for a tag, Always for :latest or none"},
	{"spec.containers[].terminationMessagePath", []any{"/dev/termination-log"}, "API default"},
	{"spec.containers[].terminationMessagePolicy", []any{"File"}, "API default"},
	{"spec.initContainers[].imagePullPolicy", []any{"IfNotPresent", "Always"},
		"API default from the image reference, as for containers"},
	{"spec.initContainers[].terminationMessagePath", []any{"/dev/termination-log"}, "API default"},
	{"spec.initContainers[].terminationMessagePolicy", []any{"File"}, "API default"},
	{"spec.schedulerName", []any{"default-scheduler"}, "API default; any other scheduler is a change"},
	{"spec.serviceAccountName", []any{"default"},
		"ServiceAccount admission names the namespace's default; the required automountServiceAccountToken=false keeps its token out"},
	{"spec.serviceAccount", []any{"default"}, "the deprecated mirror of serviceAccountName"},
	{"spec.terminationGracePeriodSeconds", []any{int64(30)}, "API default"},
	{"spec.priority", nil, "Priority admission resolves it from priorityClassName, which the read-back holds to Wardyn's value"},
	{"spec.preemptionPolicy", []any{"PreemptLowerPriority", "Never"},
		"Priority admission copies it from the same class; it decides whether this pod may evict others, not what it can reach"},
	{"spec.tolerations[]", []any{defaultToleration("node.kubernetes.io/not-ready"), defaultToleration("node.kubernetes.io/unreachable")},
		"DefaultTolerationSeconds admission: stay 300s on a failing node; NoExecute only, so no new node is opened to the pod"},
	{"spec.volumes[].secret.defaultMode", []any{int64(420)}, "API default (0644) of a Secret volume"},
}

func readBackAllowedAt(key string) (readBackAllowance, bool) {
	i := slices.IndexFunc(readBackAllowed, func(a readBackAllowance) bool { return a.key == key })
	if i < 0 {
		return readBackAllowance{}, false
	}
	return readBackAllowed[i], true
}

// readBackDiff names the first field where got, the admitted pod, is not want, the pod Wardyn asked
// for as RuntimeClass admission admits it: want's spec with the gate, its labels and annotations, and
// one controlling owner reference to this run's Sandbox. Every value Wardyn set must be there and equal; an empty object or list equals
// an absent one; a list takes no extra item and a field Wardyn left empty no value, except as
// readBackAllowed names. "" when they agree.
func readBackDiff(want, got *corev1.Pod, sandboxUID k8stypes.UID) string {
	owner := []metav1.OwnerReference{{APIVersion: agentSandboxAPIVersion, Kind: "Sandbox", Name: want.Name, UID: sandboxUID, Controller: boolPtr(true)}}
	w, werr := readBackView(want, owner, gatedSpec(want))
	g, gerr := readBackView(got, got.OwnerReferences, &got.Spec)
	if werr != nil || gerr != nil {
		return "the pod could not be converted for the comparison"
	}
	return diffValue("", "", w, g)
}

func readBackView(p *corev1.Pod, owners []metav1.OwnerReference, spec *corev1.PodSpec) (map[string]any, error) {
	return runtime.DefaultUnstructuredConverter.ToUnstructured(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: p.Labels, Annotations: p.Annotations, OwnerReferences: owners},
		Spec:       *spec,
	})
}

// diffValue compares one value. key is the allowlist's form of its path (a list item is "[]");
// field is the form a refusal shows (a named item is "[name]").
func diffValue(key, field string, want, got any) string {
	if isEmptyValue(want) {
		return extraValue(key, field, got)
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return field
		}
		for _, k := range slices.Sorted(maps.Keys(w)) {
			if f := diffValue(joinPath(key, k), joinPath(field, k), w[k], g[k]); f != "" {
				return f
			}
		}
		for _, k := range slices.Sorted(maps.Keys(g)) {
			if _, set := w[k]; set {
				continue
			}
			if f := extraValue(joinPath(key, k), joinPath(field, k), g[k]); f != "" {
				return f
			}
		}
		return ""
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) < len(w) {
			return field
		}
		if key == "spec.tolerations" {
			return diffUnordered(key, field, w, g)
		}
		for i := range w {
			if f := diffValue(key+"[]", field+itemLabel(w[i], i), w[i], g[i]); f != "" {
				return f
			}
		}
		for i := len(w); i < len(g); i++ {
			if !allowedItem(key+"[]", g[i]) {
				return field + itemLabel(g[i], i)
			}
		}
		return ""
	default:
		if reflect.DeepEqual(want, got) || quantityEqual(key, want, got) {
			return ""
		}
		return field
	}
}

// diffUnordered compares a list whose order admission does not keep (tolerations: Wardyn's, the
// RuntimeClass's and DefaultTolerationSeconds' land in plugin order): each wanted item exactly once,
// and every other item allowlisted.
func diffUnordered(key, field string, want, got []any) string {
	used := make([]bool, len(got))
	for i, w := range want {
		j := -1
		for k := range got {
			if !used[k] && reflect.DeepEqual(w, got[k]) {
				j = k
				break
			}
		}
		if j < 0 {
			return field + itemLabel(w, i)
		}
		used[j] = true
	}
	for j, g := range got {
		if !used[j] && !allowedItem(key+"[]", g) {
			return field + itemLabel(g, j)
		}
	}
	return ""
}

// extraValue checks a value where Wardyn set none: empty, allowlisted, or made only of allowlisted
// fields and list items.
func extraValue(key, field string, got any) string {
	if isEmptyValue(got) {
		return ""
	}
	if a, ok := readBackAllowedAt(key); ok {
		if a.values == nil || slices.ContainsFunc(a.values, func(v any) bool { return reflect.DeepEqual(v, got) }) {
			return ""
		}
		return field
	}
	switch g := got.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(g)) {
			if f := extraValue(joinPath(key, k), joinPath(field, k), g[k]); f != "" {
				return f
			}
		}
		return ""
	case []any:
		for i, item := range g {
			if !allowedItem(key+"[]", item) {
				return field + itemLabel(item, i)
			}
		}
		return ""
	}
	return field
}

func allowedItem(key string, item any) bool {
	a, ok := readBackAllowedAt(key)
	return ok && (a.values == nil || slices.ContainsFunc(a.values, func(v any) bool { return reflect.DeepEqual(v, item) }))
}

func isEmptyValue(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(v) == 0
	case []any:
		return len(v) == 0
	}
	return false
}

// quantityEqual compares two resource quantities by value (Quantity.Cmp), so "1000m" and "1" agree.
func quantityEqual(key string, want, got any) bool {
	if !strings.Contains(key, ".resources.limits.") && !strings.Contains(key, ".resources.requests.") &&
		!strings.HasSuffix(key, ".sizeLimit") && !strings.HasPrefix(key, "spec.overhead.") {
		return false
	}
	ws, wok := want.(string)
	gs, gok := got.(string)
	if !wok || !gok {
		return false
	}
	wq, werr := resource.ParseQuantity(ws)
	gq, gerr := resource.ParseQuantity(gs)
	return werr == nil && gerr == nil && wq.Cmp(gq) == 0
}

func joinPath(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

// itemLabel shows a list item by its name when it has one, else by index.
func itemLabel(item any, i int) string {
	if m, ok := item.(map[string]any); ok {
		if name, _ := m["name"].(string); name != "" {
			return "[" + name + "]"
		}
	}
	return fmt.Sprintf("[%d]", i)
}
