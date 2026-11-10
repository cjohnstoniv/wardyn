// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

const asTemplate = "platform"

// newTemplateRig is newSandboxRig with WARDYN_K8S_AGENT_SANDBOX_TEMPLATE naming obj, stored in the runs
// namespace as the platform team would create it. Boot reads it too; its refusals are tested apart.
func newTemplateRig(t *testing.T, obj map[string]any, answer sandboxAnswer) (*Driver, *fake.Clientset, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	d, cs, dyn := newSandboxRig(t, Config{SandboxPlacement: asPlacement}, answer)
	d.cfg.AgentSandboxTemplate = asTemplate
	if err := dyn.Tracker().Create(sandboxTemplatesGVR, &unstructured.Unstructured{Object: obj}, testNamespace); err != nil {
		t.Fatalf("store the SandboxTemplate: %v", err)
	}
	cs.ClearActions()
	dyn.ClearActions()
	return d, cs, dyn
}

// Every refused field refuses the run with zero Sandbox creates: the controller never sees a pod that
// would carry it, and the run's other objects are torn down.
func TestAgentSandboxTemplate_RefusedFieldCreatesNoSandbox(t *testing.T) {
	for _, tc := range refusedTemplates() {
		t.Run(tc.name, func(t *testing.T) {
			d, cs, dyn := newTemplateRig(t, tc.obj, controllerMaking(t, nil))
			_, err := d.CreateSandbox(context.Background(), asSpec())
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("CreateSandbox err = %v, want a refusal naming %s", err, tc.field)
			}
			if n := sandboxActions(dyn, "create"); n != 0 {
				t.Errorf("Sandbox creates = %d, want 0", n)
			}
			assertRunObjectsGone(t, cs, asRunID)
		})
	}
}

// A template whose value would change one Wardyn set (here the placement's node selector) is refused the
// same way.
func TestAgentSandboxTemplate_ConflictWithPlacementCreatesNoSandbox(t *testing.T) {
	obj := sandboxTemplateObj(map[string]any{"nodeSelector": map[string]any{"pool": "general"}}, nil, nil)
	d, cs, dyn := newTemplateRig(t, obj, controllerMaking(t, nil))
	if _, err := d.CreateSandbox(context.Background(), asSpec()); err == nil || !strings.Contains(err.Error(), `"pool"`) {
		t.Fatalf("CreateSandbox err = %v, want the nodeSelector conflict", err)
	}
	if n := sandboxActions(dyn, "create"); n != 0 {
		t.Errorf("Sandbox creates = %d, want 0", n)
	}
	assertRunObjectsGone(t, cs, asRunID)
}

// A template deleted after boot refuses the run, again before any Sandbox.
func TestAgentSandboxTemplate_MissingAtRunCreatesNoSandbox(t *testing.T) {
	d, _, dyn := newTemplateRig(t, allowedTemplate(), controllerMaking(t, nil))
	if err := dyn.Tracker().Delete(sandboxTemplatesGVR, testNamespace, asTemplate); err != nil {
		t.Fatalf("delete the template: %v", err)
	}
	if _, err := d.CreateSandbox(context.Background(), asSpec()); err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("CreateSandbox err = %v, want the unreadable template", err)
	}
	if n := sandboxActions(dyn, "create"); n != 0 {
		t.Errorf("Sandbox creates = %d, want 0", n)
	}
}

// The allowlisted fields reach the Sandbox's pod template, the read-back holds the pod to them, and the
// gate comes off once. Wardyn's own fields are unchanged.
func TestAgentSandboxTemplate_AllowedFieldsReachThePodAndPassReadBack(t *testing.T) {
	// A real API server keeps a schedulerName that is set; the fake controller's defaults overwrite it.
	keepScheduler := func(p *corev1.Pod) { p.Spec.SchedulerName = "platform-scheduler" }
	d, cs, dyn := newTemplateRig(t, allowedTemplate(), controllerMaking(t, keepScheduler))
	if _, err := cs.NodeV1().RuntimeClasses().Create(context.Background(), &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "gvisor"}, Handler: "runsc"}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create RuntimeClass: %v", err)
	}
	sb, err := d.CreateSandbox(context.Background(), asSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	obj, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, sb.Ref)
	if err != nil {
		t.Fatalf("get Sandbox: %v", err)
	}
	pod := podTemplateOf(t, obj.(*unstructured.Unstructured))
	s := pod.Spec
	if s.NodeSelector["zone"] != "a" || s.NodeSelector["pool"] != "sandbox" || s.Affinity == nil || len(s.TopologySpreadConstraints) != 1 ||
		s.PriorityClassName != "agents" || s.SchedulerName != "platform-scheduler" || s.RuntimeClassName == nil || *s.RuntimeClassName != "gvisor" {
		t.Errorf("Sandbox pod spec missed a template field: %+v", s)
	}
	if pod.Annotations[sandboxTemplateAnnotationPrefix+"cost-center"] != "ml" || pod.Annotations["example.com/note"] != "kept" {
		t.Errorf("Sandbox pod annotations = %v", pod.Annotations)
	}
	if s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken || s.DNSPolicy != corev1.DNSNone || len(s.HostAliases) == 0 || len(s.SchedulingGates) != 1 {
		t.Errorf("a Wardyn-owned field changed under the template: %+v", s)
	}
	if p := podPatches(cs); len(p) != 1 {
		t.Errorf("pod patches = %d, want the one gate removal", len(p))
	}
}

// The controller dropping a template field is a changed pod, refused like any other read-back miss.
func TestAgentSandboxTemplate_ReadBackHoldsTheTemplateFields(t *testing.T) {
	dropZone := func(p *corev1.Pod) {
		p.Spec.SchedulerName = "platform-scheduler"
		delete(p.Spec.NodeSelector, "zone")
	}
	d, cs, dyn := newTemplateRig(t, allowedTemplate(), controllerMaking(t, dropZone))
	if _, err := cs.NodeV1().RuntimeClasses().Create(context.Background(), &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "gvisor"}, Handler: "runsc"}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create RuntimeClass: %v", err)
	}
	if _, err := d.CreateSandbox(context.Background(), asSpec()); err == nil || !strings.Contains(err.Error(), "nodeSelector") {
		t.Fatalf("CreateSandbox err = %v, want the read-back to refuse the dropped nodeSelector key", err)
	}
	assertRefusedAndTornDown(t, cs, dyn)
}

func TestAgentSandboxTemplateBoot_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		stored map[string]any // nil: no template stored
		deny   string
		want   string
	}{
		{"get sandboxtemplates denied", allowedTemplate(), "get sandboxtemplates", "needs get on sandboxtemplates.extensions.agents.x-k8s.io"},
		{"template missing", nil, "", "could not be read"},
		{"refused field", sandboxTemplateObj(map[string]any{"nodeName": "node-1"}, nil, nil), "", "spec.podTemplate.spec.nodeName"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset()
			installCanaryReactor(t, cs, false)
			cs.Resources = servedSandboxes()
			installAccessReactor(cs, tc.deny, nil)
			dyn := newFakeDynamic()
			if tc.stored != nil {
				if err := dyn.Tracker().Create(sandboxTemplatesGVR, &unstructured.Unstructured{Object: tc.stored}, testNamespace); err != nil {
					t.Fatalf("store the template: %v", err)
				}
			}
			useDynamicClient(t, dyn)
			cfg := Config{AgentSandbox: true, AgentSandboxTemplate: asTemplate, Namespaces: []string{testNamespace}, ProxyImage: "p"}
			d, err := newWithClient(context.Background(), cs, testRestConfig(), cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("newWithClient = %v, %v; want a refusal containing %q", d, err, tc.want)
			}
		})
	}
}

func TestAgentSandboxTemplateBoot_AcceptsAnAllowedTemplate(t *testing.T) {
	cs := fake.NewClientset()
	installCanaryReactor(t, cs, false)
	cs.Resources = servedSandboxes()
	installAccessReactor(cs, "", nil)
	dyn := newFakeDynamic()
	if err := dyn.Tracker().Create(sandboxTemplatesGVR, &unstructured.Unstructured{Object: allowedTemplate()}, testNamespace); err != nil {
		t.Fatalf("store the template: %v", err)
	}
	useDynamicClient(t, dyn)
	cfg := Config{AgentSandbox: true, AgentSandboxTemplate: asTemplate, Namespaces: []string{testNamespace}, ProxyImage: "p"}
	if _, err := newWithClient(context.Background(), cs, testRestConfig(), cfg); err != nil {
		t.Fatalf("newWithClient: %v", err)
	}
}

func TestAgentSandboxTemplate_ConfigChecks(t *testing.T) {
	cases := []struct {
		cfg  Config
		want string
	}{
		{Config{AgentSandboxTemplate: asTemplate}, "needs WARDYN_K8S_AGENT_SANDBOX on"},
		{Config{AgentSandbox: true, AgentSandboxTemplate: "Not_A_Name"}, "not a valid object name"},
	}
	for _, tc := range cases {
		if err := checkAgentSandboxTemplate(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("checkAgentSandboxTemplate(%+v) = %v, want %q", tc.cfg, err, tc.want)
		}
	}
	if err := checkAgentSandboxTemplate(Config{}); err != nil {
		t.Errorf("no template: %v", err)
	}
	t.Setenv("WARDYN_K8S_AGENT_SANDBOX_TEMPLATE", asTemplate)
	if got := buildConfig(substrate.Deps{}).AgentSandboxTemplate; got != asTemplate {
		t.Errorf("AgentSandboxTemplate = %q, want %q from WARDYN_K8S_AGENT_SANDBOX_TEMPLATE", got, asTemplate)
	}
}
