// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// scriptImageCheckPods answers every Get of an image check pod with the status status returns for its
// architecture, and records each create in order (kind/name).
func scriptImageCheckPods(t *testing.T, cs *fake.Clientset, status func(arch string) corev1.PodStatus) *[]string {
	t.Helper()
	var mu sync.Mutex
	var created []string
	cs.PrependReactor("create", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(clienttesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		if m, err := metaName(ca.GetObject()); err == nil && strings.HasPrefix(m, "wardyn-imagecheck-") {
			mu.Lock()
			created = append(created, action.GetResource().Resource+"/"+m)
			mu.Unlock()
		}
		return false, nil, nil
	})
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok || !strings.HasPrefix(ga.GetName(), "wardyn-imagecheck-") {
			return false, nil, nil
		}
		arch := ga.GetName()[strings.LastIndexByte(ga.GetName(), '-')+1:]
		return true, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: ga.GetName(), Namespace: action.GetNamespace()}, Status: status(arch)}, nil
	})
	return &created
}

func metaName(obj runtime.Object) (string, error) {
	m, ok := obj.(metav1.Object)
	if !ok {
		return "", nil
	}
	return m.GetName(), nil
}

func terminated(code int32, msg string) corev1.PodStatus {
	return corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  imageCheckContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Message: msg}},
	}}}
}

const checkRef = "ghcr.io/acme/sdk-agent:2.3@sha256:4f1c2a7e0d9b8c6f5e4d3c2b1a0f9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b9be0"

func TestCheckPrewrappedImage_PodSpecPolicyFirstAndTeardown(t *testing.T) {
	d, cs := newTestDriver(t, Config{ImagePullSecret: "regcred"})
	created := scriptImageCheckPods(t, cs, func(string) corev1.PodStatus { return terminated(0, "") })
	var pods []*corev1.Pod
	var mu sync.Mutex
	cs.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if p, ok := action.(clienttesting.CreateAction).GetObject().(*corev1.Pod); ok && strings.HasPrefix(p.Name, "wardyn-imagecheck-") {
			mu.Lock()
			pods = append(pods, p.DeepCopy())
			mu.Unlock()
		}
		return false, nil, nil
	})
	attempt := uuid.New()

	got := d.CheckPrewrappedImage(context.Background(), attempt, checkRef)

	if len(got) != 2 || got[0] != (runner.ImageArchVerdict{Arch: "amd64", Code: runner.ImageCheckPassed}) ||
		got[1] != (runner.ImageArchVerdict{Arch: "arm64", Code: runner.ImageCheckPassed}) {
		t.Fatalf("verdicts = %+v, want both passed", got)
	}
	if len(*created) != 3 || !strings.HasPrefix((*created)[0], "networkpolicies/") {
		t.Fatalf("creates = %v, want the attempt's NetworkPolicy first, then two pods", *created)
	}
	if len(pods) != 2 {
		t.Fatalf("created %d check pods, want 2", len(pods))
	}
	for _, p := range pods {
		arch := p.Name[strings.LastIndexByte(p.Name, '-')+1:]
		c := p.Spec.Containers[0]
		switch {
		case p.Spec.NodeSelector[corev1.LabelArchStable] != arch:
			t.Errorf("%s: node selector %v, want %s pinned", p.Name, p.Spec.NodeSelector, arch)
		case len(p.Spec.ImagePullSecrets) != 1 || p.Spec.ImagePullSecrets[0].Name != "regcred":
			t.Errorf("%s: pull secrets %v, want regcred", p.Name, p.Spec.ImagePullSecrets)
		case c.Image != checkRef:
			t.Errorf("%s: image %q, want the pinned ref as is", p.Name, c.Image)
		case c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != runner.AgentUID:
			t.Errorf("%s: security context %+v, want the agent's numeric UID", p.Name, c.SecurityContext)
		case c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation:
			t.Errorf("%s: privilege escalation allowed", p.Name)
		case p.Spec.AutomountServiceAccountToken == nil || *p.Spec.AutomountServiceAccountToken:
			t.Errorf("%s: service account token mounted", p.Name)
		case p.Spec.ActiveDeadlineSeconds == nil || *p.Spec.ActiveDeadlineSeconds != 600:
			t.Errorf("%s: activeDeadlineSeconds %v, want 600", p.Name, p.Spec.ActiveDeadlineSeconds)
		case p.Spec.RestartPolicy != corev1.RestartPolicyNever:
			t.Errorf("%s: restart policy %s", p.Name, p.Spec.RestartPolicy)
		case len(c.Command) != 3 || c.Command[2] != runner.ImageCheckScript:
			t.Errorf("%s: command %v, want sh -c the check script", p.Name, c.Command)
		case c.Resources.Limits.Memory().IsZero() || c.Resources.Limits.Cpu().IsZero():
			t.Errorf("%s: unbounded resources %v", p.Name, c.Resources.Limits)
		case p.Labels[labelComponent] != componentImageCheck || p.Labels[labelRun] != attempt.String():
			t.Errorf("%s: labels %v, want component imagecheck and the attempt as run", p.Name, p.Labels)
		}
	}
	sel := metav1.ListOptions{LabelSelector: labelRun + "=" + attempt.String()}
	left, _ := cs.CoreV1().Pods(testNamespace).List(context.Background(), sel)
	pols, _ := cs.NetworkingV1().NetworkPolicies(testNamespace).List(context.Background(), sel)
	if len(left.Items) != 0 || len(pols.Items) != 0 {
		t.Fatalf("after the check: %d pod(s) and %d polic(ies) of the attempt left, want none", len(left.Items), len(pols.Items))
	}
}

func TestCheckPrewrappedImage_PolicyIsDenyAllBothWays(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	scriptImageCheckPods(t, cs, func(string) corev1.PodStatus { return terminated(0, "") })
	var pols []*networkingv1.NetworkPolicy
	cs.PrependReactor("create", "networkpolicies", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if np, ok := action.(clienttesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy); ok && strings.HasPrefix(np.Name, "wardyn-imagecheck-") {
			pols = append(pols, np.DeepCopy())
		}
		return false, nil, nil
	})
	attempt := uuid.New()
	d.CheckPrewrappedImage(context.Background(), attempt, checkRef)

	if len(pols) != 1 {
		t.Fatalf("created %d check policies, want 1 per attempt", len(pols))
	}
	np := pols[0]
	if np.Spec.PodSelector.MatchLabels[labelRun] != attempt.String() || np.Spec.PodSelector.MatchLabels[labelComponent] != componentImageCheck {
		t.Errorf("policy selects %v, want the attempt's check pods", np.Spec.PodSelector.MatchLabels)
	}
	if len(np.Spec.PolicyTypes) != 2 || len(np.Spec.Ingress) != 0 || len(np.Spec.Egress) != 0 {
		t.Errorf("policy types %v ingress %d egress %d rules, want Ingress+Egress with no rules (deny all)",
			np.Spec.PolicyTypes, len(np.Spec.Ingress), len(np.Spec.Egress))
	}
}

func TestCheckPrewrappedImage_PinnedPlacementChecksOneArch(t *testing.T) {
	d, cs := newTestDriver(t, Config{SandboxPlacement: `{"nodeSelector":{"kubernetes.io/arch":"amd64"}}`})
	created := scriptImageCheckPods(t, cs, func(string) corev1.PodStatus { return terminated(0, "") })

	got := d.CheckPrewrappedImage(context.Background(), uuid.New(), checkRef)

	if got[0].Code != runner.ImageCheckPassed || got[1] != (runner.ImageArchVerdict{Arch: "arm64", Code: runner.ImageCheckNoNode}) {
		t.Fatalf("verdicts = %+v, want amd64 passed and arm64 no_node", got)
	}
	for _, c := range *created {
		if strings.HasSuffix(c, "-arm64") {
			t.Fatalf("created %s although the operator's placement pins amd64", c)
		}
	}
}

func TestPrewrappedCheckUnavailable(t *testing.T) {
	if got := (&Driver{netPolEnforced: true}).PrewrappedCheckUnavailable(); got != "" {
		t.Errorf("enforced direct substrate: %q, want available", got)
	}
	if got := (&Driver{}).PrewrappedCheckUnavailable(); got == "" {
		t.Error("unproven NetworkPolicy enforcement: available, want a reason")
	}
	if got := (&Driver{netPolEnforced: true, cfg: Config{RunnerID: "r1"}}).PrewrappedCheckUnavailable(); got == "" {
		t.Error("a runner's substrate: available, want a reason")
	}
}

func TestImageCheckVerdict(t *testing.T) {
	waiting := func(reason, msg string) corev1.PodStatus {
		return corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: imageCheckContainerName, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}},
		}}}
	}
	for _, c := range []struct {
		name         string
		status       corev1.PodStatus
		code, detail string
	}{
		{"passed", terminated(0, "wardyn image contract 1"), runner.ImageCheckPassed, ""},
		{"missing tool", terminated(3, "missing wardyn-rec"), runner.ImageCheckContractMissing, "wardyn-rec"},
		{"old recorder", terminated(4, "wardyn-rec -contract failed"), runner.ImageCheckContractOld, ""},
		{"exit 3 without the line", terminated(3, "something else"), runner.ImageCheckFailed, "exit 3: something else"},
		{"other exit", terminated(127, "sh: not found"), runner.ImageCheckFailed, "exit 127: sh: not found"},
		{"deadline", corev1.PodStatus{Phase: corev1.PodFailed, Reason: "DeadlineExceeded"}, runner.ImageCheckTimedOut, ""},
		{"containerd no platform", waiting("ErrImagePull", `failed to pull and unpack image "x": no match for platform in manifest: not found`), runner.ImageCheckPlatformMissing, ""},
		{"docker no platform", waiting("ImagePullBackOff", "no matching manifest for linux/arm64 in the manifest list entries"), runner.ImageCheckPlatformMissing, ""},
		{"first pull error still waits", waiting("ErrImagePull", "401 Unauthorized"), "", ""},
		{"backoff unauthorized", waiting("ImagePullBackOff", `failed to authorize: 401 Unauthorized`), runner.ImageCheckUnpullable, "unauthorized"},
		{"backoff not found", waiting("ImagePullBackOff", `ghcr.io/acme/x: not found`), runner.ImageCheckUnpullable, "not found"},
		{"invalid name", waiting("InvalidImageName", ""), runner.ImageCheckUnpullable, "invalid image name"},
		{"create error", waiting("CreateContainerError", "no sh"), runner.ImageCheckFailed, "CreateContainerError: no sh"},
		{"still creating", waiting("ContainerCreating", ""), "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var since time.Time
			code, detail := imageCheckVerdict(&corev1.Pod{Status: c.status}, &since, time.Now())
			if code != c.code || detail != c.detail {
				t.Fatalf("got (%q, %q), want (%q, %q)", code, detail, c.code, c.detail)
			}
		})
	}

	t.Run("unschedulable becomes no_node only after the grace", func(t *testing.T) {
		unsched := &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
		}}}}
		var since time.Time
		t0 := time.Now()
		if code, _ := imageCheckVerdict(unsched, &since, t0); code != "" {
			t.Fatalf("first unschedulable read = %q, want still waiting", code)
		}
		if code, _ := imageCheckVerdict(unsched, &since, t0.Add(imageCheckNoNodeAfter-time.Second)); code != "" {
			t.Fatalf("before the grace = %q, want still waiting", code)
		}
		scheduled := &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}}}
		imageCheckVerdict(scheduled, &since, t0.Add(imageCheckNoNodeAfter))
		if !since.IsZero() {
			t.Fatal("a scheduled read did not reset the unschedulable clock")
		}
		if code, _ := imageCheckVerdict(unsched, &since, t0.Add(imageCheckNoNodeAfter)); code != "" {
			t.Fatalf("unschedulable again after a reset = %q, want still waiting", code)
		}
		if code, _ := imageCheckVerdict(unsched, &since, t0.Add(2*imageCheckNoNodeAfter)); code != runner.ImageCheckNoNode {
			t.Fatalf("after the grace = %q, want no_node", code)
		}
	})
}

// TestSweepOrphanedSandboxes_ImageCheckPods: a crashed attempt's pod and policy are reclaimed by the sweep;
// a live attempt's (isOrphan false) are left.
func TestSweepOrphanedSandboxes_ImageCheckPods(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	ctx := context.Background()
	mk := func(attempt uuid.UUID) {
		t.Helper()
		labels := wardynLabels(attempt, componentImageCheck, nil)
		if _, err := cs.NetworkingV1().NetworkPolicies(testNamespace).Create(ctx, &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "wardyn-imagecheck-netpol-" + attempt.String(), Namespace: testNamespace, Labels: labels},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		name := "wardyn-imagecheck-" + attempt.String() + "-amd64"
		if _, err := cs.CoreV1().Pods(testNamespace).Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		setPodCreation(t, cs, name, time.Now().Add(-2*time.Hour))
	}
	crashed, live := uuid.New(), uuid.New()
	mk(crashed)
	mk(live)

	if _, err := d.SweepOrphanedSandboxes(ctx, time.Hour, func(id uuid.UUID) bool { return id != live }); err != nil {
		t.Fatalf("SweepOrphanedSandboxes: %v", err)
	}
	count := func(id uuid.UUID) (int, int) {
		sel := metav1.ListOptions{LabelSelector: labelRun + "=" + id.String()}
		p, _ := cs.CoreV1().Pods(testNamespace).List(ctx, sel)
		n, _ := cs.NetworkingV1().NetworkPolicies(testNamespace).List(ctx, sel)
		return len(p.Items), len(n.Items)
	}
	if p, n := count(crashed); p != 0 || n != 0 {
		t.Errorf("crashed attempt: %d pod(s), %d polic(ies) left, want none", p, n)
	}
	if p, n := count(live); p != 1 || n != 1 {
		t.Errorf("live attempt: %d pod(s), %d polic(ies) left, want both kept", p, n)
	}
}
