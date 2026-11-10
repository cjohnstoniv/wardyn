// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// sandboxTemplateCRDPodSpecKeys is every PodSpec field the agent-sandbox v1.0.9 SandboxTemplate CRD
// accepts under spec.podTemplate.spec (extensions.yaml, sha256 02e8ff69...; the lane's TRIAGE.md).
var sandboxTemplateCRDPodSpecKeys = []string{
	"activeDeadlineSeconds", "affinity", "automountServiceAccountToken", "containers", "dnsConfig", "dnsPolicy",
	"enableServiceLinks", "ephemeralContainers", "evictionResponders", "hostAliases", "hostIPC", "hostNetwork",
	"hostPID", "hostUsers", "hostname", "hostnameOverride", "imagePullSecrets", "initContainers", "nodeName",
	"nodeSelector", "os", "overhead", "preemptionPolicy", "priority", "priorityClassName", "readinessGates",
	"resourceClaims", "resources", "restartPolicy", "runtimeClassName", "schedulerName", "schedulingGates",
	"schedulingGroup", "securityContext", "serviceAccount", "serviceAccountName", "setHostnameAsFQDN",
	"shareProcessNamespace", "subdomain", "terminationGracePeriodSeconds", "tolerations",
	"topologySpreadConstraints", "volumes",
}

func templateContainers(name string) []any {
	return []any{map[string]any{"name": name, "image": "example.com/x"}}
}

// refusedTemplatePodSpec is one non-empty value per PodSpec field: a template may set none. false and 0
// are settings (hostUsers: false asks for a user namespace), so they are refused too.
var refusedTemplatePodSpec = map[string]any{
	"activeDeadlineSeconds":         int64(60),
	"automountServiceAccountToken":  true,
	"containers":                    templateContainers("main"),
	"dnsConfig":                     map[string]any{"nameservers": []any{"10.96.0.10"}},
	"dnsPolicy":                     "ClusterFirst",
	"enableServiceLinks":            true,
	"ephemeralContainers":           templateContainers("debug"),
	"evictionResponders":            []any{map[string]any{"name": "x"}},
	"hostAliases":                   []any{map[string]any{"ip": "10.0.0.1", "hostnames": []any{"wardyn-proxy"}}},
	"hostIPC":                       true,
	"hostNetwork":                   true,
	"hostPID":                       true,
	"hostUsers":                     false,
	"hostname":                      "agent",
	"hostnameOverride":              "agent",
	"imagePullSecrets":              []any{map[string]any{"name": "pull"}},
	"initContainers":                templateContainers("init"),
	"nodeName":                      "node-1",
	"os":                            map[string]any{"name": "linux"},
	"overhead":                      map[string]any{"cpu": "1"},
	"preemptionPolicy":              "Never",
	"priority":                      int64(1000),
	"readinessGates":                []any{map[string]any{"conditionType": "example.com/ready"}},
	"resourceClaims":                []any{map[string]any{"name": "gpu"}},
	"resources":                     map[string]any{"limits": map[string]any{"cpu": "2"}},
	"restartPolicy":                 "Always",
	"schedulingGates":               []any{map[string]any{"name": "example.com/other"}},
	"schedulingGroup":               map[string]any{"name": "g"},
	"securityContext":               map[string]any{"runAsUser": int64(0)},
	"serviceAccount":                "default",
	"serviceAccountName":            "default",
	"setHostnameAsFQDN":             true,
	"shareProcessNamespace":         true,
	"subdomain":                     "agents",
	"terminationGracePeriodSeconds": int64(0),
	"volumes":                       []any{map[string]any{"name": "host", "hostPath": map[string]any{"path": "/"}}},
	// Placement and runtime: the boot canary proves NetworkPolicy enforcement on Wardyn's placement only.
	"nodeSelector":              map[string]any{"zone": "a"},
	"tolerations":               []any{map[string]any{"key": "agents", "operator": "Exists", "effect": "NoSchedule"}},
	"topologySpreadConstraints": []any{map[string]any{"maxSkew": int64(1), "topologyKey": "zone", "whenUnsatisfiable": "ScheduleAnyway"}},
	"priorityClassName":         "agents",
	"schedulerName":             "platform-scheduler",
	"runtimeClassName":          "gvisor",
	"affinity": map[string]any{"nodeAffinity": map[string]any{"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
		"nodeSelectorTerms": []any{map[string]any{"matchExpressions": []any{map[string]any{"key": "tier", "operator": "In", "values": []any{"agents"}}}}},
	}}},
}

// sandboxTemplateObj is a SandboxTemplate as the API server returns it (defaults set, containers: [] as
// the CRD requires), with podSpec and the template's metadata, then mutate.
func sandboxTemplateObj(podSpec, meta map[string]any, mutate func(spec map[string]any)) map[string]any {
	ps := map[string]any{"containers": []any{}}
	maps.Copy(ps, podSpec)
	tmpl := map[string]any{"spec": ps}
	if meta != nil {
		tmpl["metadata"] = meta
	}
	spec := map[string]any{"podTemplate": tmpl}
	maps.Copy(spec, sandboxTemplateSpecDefaults)
	if mutate != nil {
		mutate(spec)
	}
	return map[string]any{
		"apiVersion": "extensions.agents.x-k8s.io/v1beta1",
		"kind":       "SandboxTemplate",
		"metadata":   map[string]any{"name": "platform"},
		"spec":       spec,
	}
}

// refusedTemplate is one adversarial template and the field its refusal must name.
type refusedTemplate struct {
	name, field string
	obj         map[string]any
}

// refusedTemplates is one template per refused field: every PodSpec field outside the allowlist, pod
// labels, an annotation outside the prefix, and each template-level field away from its default.
func refusedTemplates() []refusedTemplate {
	var out []refusedTemplate
	for _, k := range slices.Sorted(maps.Keys(refusedTemplatePodSpec)) {
		out = append(out, refusedTemplate{"podSpec/" + k, "spec.podTemplate.spec." + k, sandboxTemplateObj(map[string]any{k: refusedTemplatePodSpec[k]}, nil, nil)})
	}
	out = append(out,
		refusedTemplate{"labels", "spec.podTemplate.metadata.labels", sandboxTemplateObj(nil, map[string]any{"labels": map[string]any{"egress": "allowed"}}, nil)},
		refusedTemplate{"annotation/apparmor", "container.apparmor.security.beta.kubernetes.io/main", sandboxTemplateObj(nil, map[string]any{"annotations": map[string]any{"container.apparmor.security.beta.kubernetes.io/main": "unconfined"}}, nil)},
		refusedTemplate{"annotation/runtime", "io.katacontainers.config.hypervisor.kernel_params", sandboxTemplateObj(nil, map[string]any{"annotations": map[string]any{"io.katacontainers.config.hypervisor.kernel_params": "x"}}, nil)},
		refusedTemplate{"annotation/bare-prefix", sandboxTemplateAnnotationPrefix, sandboxTemplateObj(nil, map[string]any{"annotations": map[string]any{sandboxTemplateAnnotationPrefix: "x"}}, nil)},
	)
	specFields := map[string]any{
		"envVarsInjectionPolicy":     "Allowed",
		"failedPodPolicy":            "Recreate",
		"networkPolicyManagement":    "Unmanaged",
		"volumeClaimTemplatesPolicy": "Allowed",
		"service":                    true,
		"networkPolicy":              map[string]any{"egress": []any{map[string]any{}}},
		"volumeClaimTemplates":       []any{map[string]any{"metadata": map[string]any{"name": "work"}}},
		"lifecycle":                  map[string]any{"shutdownPolicy": "Delete"},
	}
	for _, k := range slices.Sorted(maps.Keys(specFields)) {
		v := specFields[k]
		out = append(out, refusedTemplate{"spec/" + k, "spec." + k, sandboxTemplateObj(nil, nil, func(s map[string]any) { s[k] = v })})
	}
	return out
}

func TestSandboxTemplate_EveryCRDPodSpecFieldIsRefused(t *testing.T) {
	refused := slices.Sorted(maps.Keys(refusedTemplatePodSpec))
	if !slices.Equal(refused, sandboxTemplateCRDPodSpecKeys) {
		t.Errorf("refused cases    = %v\nCRD PodSpec keys = %v", refused, sandboxTemplateCRDPodSpecKeys)
	}
}

func TestSandboxTemplate_RefusesEveryFieldOutsideTheAllowlist(t *testing.T) {
	for _, tc := range refusedTemplates() {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSandboxTemplate(tc.obj)
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("parse err = %v, want a refusal naming %s", err, tc.field)
			}
		})
	}
}

// allowedTemplate sets the one thing Wardyn takes: an annotation under the prefix.
func allowedTemplate() map[string]any {
	return sandboxTemplateObj(nil, map[string]any{"annotations": map[string]any{sandboxTemplateAnnotationPrefix + "cost-center": "ml"}, "labels": map[string]any{}}, nil)
}

func TestSandboxTemplate_TakesTheAnnotationsOnly(t *testing.T) {
	base, err := parseSandboxTemplate(allowedTemplate())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	spec := corev1.PodSpec{NodeSelector: map[string]string{"pool": "sandbox"}, ServiceAccountName: "wardyn-agent"}
	pod := &corev1.Pod{Spec: *spec.DeepCopy()}
	pod.Annotations = map[string]string{"example.com/note": "kept"}
	if err := base.applyTo(pod); err != nil {
		t.Fatalf("applyTo: %v", err)
	}
	if !reflect.DeepEqual(pod.Spec, spec) {
		t.Errorf("the template changed the pod spec: %+v, want %+v", pod.Spec, spec)
	}
	if pod.Annotations[sandboxTemplateAnnotationPrefix+"cost-center"] != "ml" || pod.Annotations["example.com/note"] != "kept" {
		t.Errorf("annotations = %v", pod.Annotations)
	}
	if len(pod.Labels) != 0 {
		t.Errorf("labels = %v, want none from a template", pod.Labels)
	}
}

func TestSandboxTemplate_NeverChangesAnAnnotationWardynSet(t *testing.T) {
	base, err := parseSandboxTemplate(allowedTemplate())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	key := sandboxTemplateAnnotationPrefix + "cost-center"
	if err := base.applyTo(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{key: "ops"}}}); err == nil {
		t.Error("applyTo over Wardyn's own annotation succeeded, want a refusal")
	}
	if err := base.applyTo(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{key: "ml"}}}); err != nil {
		t.Errorf("a template repeating Wardyn's annotation was refused: %v", err)
	}
}

func TestSandboxTemplate_EmptyValuesSetNothing(t *testing.T) {
	obj := sandboxTemplateObj(map[string]any{"nodeName": "", "nodeSelector": map[string]any{}, "volumes": []any{}, "securityContext": map[string]any{}, "dnsConfig": nil}, nil, func(s map[string]any) {
		s["volumeClaimTemplates"] = []any{}
	})
	if _, err := parseSandboxTemplate(obj); err != nil {
		t.Errorf("a template whose other fields are empty was refused: %v", err)
	}
}
