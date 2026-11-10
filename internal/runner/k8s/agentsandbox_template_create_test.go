// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"strings"
	"testing"

	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

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

// The template's annotation reaches the Sandbox's pod template, the read-back holds the pod to it, and the
// gate comes off once. Placement and every Wardyn-owned field are Wardyn's alone.
func TestAgentSandboxTemplate_AnnotationReachesThePodAndPassesReadBack(t *testing.T) {
	d, cs, dyn := newTemplateRig(t, allowedTemplate(), controllerMaking(t, nil))
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
	if pod.Annotations[sandboxTemplateAnnotationPrefix+"cost-center"] != "ml" || pod.Annotations["example.com/note"] != "kept" {
		t.Errorf("Sandbox pod annotations = %v", pod.Annotations)
	}
	if len(s.NodeSelector) != 1 || s.NodeSelector["pool"] != "sandbox" || s.Affinity != nil || len(s.TopologySpreadConstraints) != 0 ||
		s.PriorityClassName != "" || s.SchedulerName != "" || s.RuntimeClassName != nil {
		t.Errorf("placement is not exactly Wardyn's placement: %+v", s)
	}
	if s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken || s.DNSPolicy != corev1.DNSNone || len(s.HostAliases) == 0 || len(s.SchedulingGates) != 1 {
		t.Errorf("a Wardyn-owned field changed under the template: %+v", s)
	}
	if p := podPatches(cs); len(p) != 1 {
		t.Errorf("pod patches = %d, want the one gate removal", len(p))
	}
}

// The controller dropping the template's annotation is a changed pod, refused like any other read-back miss.
func TestAgentSandboxTemplate_ReadBackHoldsTheTemplateAnnotation(t *testing.T) {
	drop := func(p *corev1.Pod) { delete(p.Annotations, sandboxTemplateAnnotationPrefix+"cost-center") }
	d, cs, dyn := newTemplateRig(t, allowedTemplate(), controllerMaking(t, drop))
	if _, err := d.CreateSandbox(context.Background(), asSpec()); err == nil || !strings.Contains(err.Error(), "cost-center") {
		t.Fatalf("CreateSandbox err = %v, want the read-back to refuse the dropped annotation", err)
	}
	assertRefusedAndTornDown(t, cs, dyn)
}

// installNamedTemplateAccess answers every SelfSubjectAccessReview allowed, except sandboxtemplates, which
// is allowed only for get on allowName in testNamespace, as the chart's Role (resourceNames) grants it. An
// unnamed sandboxtemplates review is refused.
func installNamedTemplateAccess(cs *fake.Clientset, allowName string) {
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
		in := a.(clienttesting.CreateAction).GetObject().(*authzv1.SelfSubjectAccessReview).DeepCopy()
		ra := in.Spec.ResourceAttributes
		in.Status.Allowed = ra.Resource != "sandboxtemplates" ||
			(ra.Group == sandboxTemplatesGVR.Group && ra.Verb == "get" && ra.Name == allowName && ra.Name != "" && ra.Namespace == testNamespace)
		return true, in, nil
	})
}

// bootWithTemplate boots the backend with WARDYN_K8S_AGENT_SANDBOX_TEMPLATE=asTemplate against stored (nil:
// none) and an authorizer that holds sandboxtemplates to allowName.
func bootWithTemplate(t *testing.T, stored map[string]any, allowName string) error {
	t.Helper()
	cs := fake.NewClientset()
	installCanaryReactor(t, cs, false)
	cs.Resources = servedSandboxes()
	installNamedTemplateAccess(cs, allowName)
	dyn := newFakeDynamic()
	if stored != nil {
		if err := dyn.Tracker().Create(sandboxTemplatesGVR, &unstructured.Unstructured{Object: stored}, testNamespace); err != nil {
			t.Fatalf("store the template: %v", err)
		}
	}
	useDynamicClient(t, dyn)
	cfg := Config{AgentSandbox: true, AgentSandboxTemplate: asTemplate, Namespaces: []string{testNamespace}, ProxyImage: "p"}
	_, err := newWithClient(context.Background(), cs, testRestConfig(), cfg)
	return err
}

func TestAgentSandboxTemplateBoot_Refusals(t *testing.T) {
	cases := []struct {
		name      string
		stored    map[string]any // nil: no template stored
		allowName string
		want      string
	}{
		{"get on another template only", allowedTemplate(), "other", "needs get on sandboxtemplates.extensions.agents.x-k8s.io"},
		{"template missing", nil, asTemplate, "could not be read"},
		{"refused field", sandboxTemplateObj(map[string]any{"nodeName": "node-1"}, nil, nil), asTemplate, "spec.podTemplate.spec.nodeName"},
		{"refused placement", sandboxTemplateObj(map[string]any{"nodeSelector": map[string]any{"zone": "a"}}, nil, nil), asTemplate, "spec.podTemplate.spec.nodeSelector"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := bootWithTemplate(t, tc.stored, tc.allowName); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("boot err = %v; want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// Every refused field refuses boot too, naming the field: nothing is silently dropped.
func TestAgentSandboxTemplateBoot_RefusesEveryRefusedField(t *testing.T) {
	for _, tc := range refusedTemplates() {
		t.Run(tc.name, func(t *testing.T) {
			if err := bootWithTemplate(t, tc.obj, asTemplate); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("boot err = %v; want a refusal naming %s", err, tc.field)
			}
		})
	}
}

// Boot passes under the chart's least-privilege Role: get on the one named template, no unnamed get.
func TestAgentSandboxTemplateBoot_AcceptsTheNamedGrant(t *testing.T) {
	if err := bootWithTemplate(t, allowedTemplate(), asTemplate); err != nil {
		t.Fatalf("boot: %v", err)
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
