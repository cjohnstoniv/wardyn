// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

// agentsandbox_k8s_test.go holds the experimental agent-sandbox backend's own cases. They run only under
// WARDYN_TEST_K8S=1 and WARDYN_TEST_K8S_AGENT_SANDBOX=1 (ci.yml's conformance-k8s-agent-sandbox job), on a
// cluster with agent-sandbox installed and the chart's two admission policies applied. The same switch
// makes every k8s suite in this package build its substrate with the backend (k8sSuiteConfig), so
// TestConformanceK8s runs its unchanged cases against pods a Sandbox made.
//
// Identities. The suite must authenticate as the ServiceAccount the chart's ungate policy names as
// wardynd's; the job renders the policies for wardyn/wardynd and hands the suite that account's token.
// The other requesters are reached by impersonation (the suite's account is cluster-admin): the
// controller's ServiceAccount, and a ServiceAccount these cases create with pod get, create, update and
// patch in the runs namespace, so its refusals come from the policy, not from RBAC.
//
// Every policy decision is matched by the words of the rule that made it, so a refusal from anything
// else (RBAC, Pod Security admission, the API server's own validation) fails a case that expects the
// policy. Two decisions belong to the API server itself, before any admission policy, and are asserted
// as such: a pod created with both a node and a scheduling gate, and an ephemeral container added by a
// plain pod update.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	authenticationv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

const (
	asGate       = "wardyn.dev/verify"
	asAPIVersion = "agents.x-k8s.io/v1beta1"
	// asControllerDefault is the upstream v1.0.9 manifest's ServiceAccount, the chart's default;
	// WARDYN_TEST_K8S_AGENT_SANDBOX_CONTROLLER names another.
	asControllerDefault = "system:serviceaccount:agent-sandbox-system:agent-sandbox-controller"

	// The rules' own messages (deploy/helm/wardyn/templates/agent-sandbox-policy.yaml).
	asDenyNoGate      = "must carry the wardyn.dev/verify scheduling gate"
	asDenyNoOwner     = "must be owned by a Sandbox"
	asDenyWho         = "only wardynd may remove the wardyn.dev/verify scheduling gate"
	asDenyNotSandbox  = "may be removed only from a pod owned by a Sandbox"
	asDenyAlsoChanged = "must not also change the pod's labels"
	// The API server's own refusals, which come before any admission policy.
	asInvalidNodeWithGate = "cannot be set until all schedulingGates have been cleared"
	asInvalidPodUpdate    = "pod updates may not change fields other than"

	// asReplacedHint opens the sentence the run's person reads, the run's failure hint, when its pod
	// was replaced outside Wardyn.
	asReplacedHint = "This run ended: its pod was replaced outside Wardyn"
)

var asSandboxes = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1beta1", Resource: "sandboxes"}

// agentSandboxOn is WARDYN_TEST_K8S_AGENT_SANDBOX=1.
func agentSandboxOn() bool { return os.Getenv("WARDYN_TEST_K8S_AGENT_SANDBOX") == "1" }

// k8sSuiteConfig is cfg with the suite-wide backend switch applied; every k8s.New in this package goes
// through it, so the switch changes the driver and nothing else.
func k8sSuiteConfig(cfg k8s.Config) k8s.Config {
	cfg.AgentSandbox = agentSandboxOn()
	return cfg
}

func requireAgentSandbox(t *testing.T) {
	t.Helper()
	if os.Getenv("WARDYN_TEST_K8S") != "1" || !agentSandboxOn() {
		t.Skip("WARDYN_TEST_K8S=1 and WARDYN_TEST_K8S_AGENT_SANDBOX=1 are not both set; skipping the agent-sandbox cases")
	}
}

// asEnv is the clients and settings the cases share.
type asEnv struct {
	cfg   *rest.Config
	cs    kubernetes.Interface
	dyn   dynamic.Interface
	ns    string
	image string
}

func newASEnv(t *testing.T) *asEnv {
	t.Helper()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	e := &asEnv{cfg: cfg, ns: os.Getenv("WARDYN_K8S_NAMESPACE"), image: os.Getenv("WARDYN_TEST_K8S_AGENT_IMAGE")}
	if e.ns == "" || e.image == "" {
		t.Fatal("WARDYN_K8S_NAMESPACE (the namespace the policies are bound to) and WARDYN_TEST_K8S_AGENT_IMAGE must be set")
	}
	if e.cs, err = kubernetes.NewForConfig(cfg); err != nil {
		t.Fatalf("clientset: %v", err)
	}
	if e.dyn, err = dynamic.NewForConfig(cfg); err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	return e
}

// as is a clientset that impersonates user.
func (e *asEnv) as(t *testing.T, user string) kubernetes.Interface {
	t.Helper()
	cfg := rest.CopyConfig(e.cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset as %s: %v", user, err)
	}
	return cs
}

// asHeldPod is what the hook between read-back and gate removal saw for one agent pod.
type asHeldPod struct {
	netPolErr error
	pod       *corev1.Pod
	podErr    error
}

var (
	asRunnerOnce sync.Once
	asRunner     runner.Runner
	asRunnerErr  error
	asHeld       sync.Map // agent pod name -> asHeldPod
)

// agentSandboxRunner is one substrate for these cases, built once: the canary at boot is not repeated
// per case. Its AgentSandboxBeforeUngate hook records, for every run, the agent NetworkPolicy and the
// pod as they stand after the read-back passed and before the gate is removed.
func agentSandboxRunner(t *testing.T, e *asEnv) runner.Runner {
	t.Helper()
	asRunnerOnce.Do(func() {
		proxy := os.Getenv("WARDYN_PROXY_IMAGE")
		if proxy == "" {
			asRunnerErr = errors.New("WARDYN_PROXY_IMAGE must name the real wardyn-proxy build")
			return
		}
		cfg := k8sSuiteConfig(k8s.Config{Namespaces: []string{e.ns}, ProxyImage: proxy, Record: true})
		cs := e.cs
		cfg.AgentSandboxBeforeUngate = func(ctx context.Context, ns, pod string) error {
			var h asHeldPod
			runID := strings.TrimPrefix(pod, "wardyn-agent-")
			_, h.netPolErr = cs.NetworkingV1().NetworkPolicies(ns).Get(ctx, "wardyn-agent-netpol-"+runID, metav1.GetOptions{})
			h.pod, h.podErr = cs.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
			asHeld.Store(pod, h)
			return nil
		}
		sub, err := k8s.New(cfg)
		if err != nil {
			asRunnerErr = err
			return
		}
		asRunner = orchestrator.New(sub)
	})
	if asRunnerErr != nil {
		t.Fatalf("k8s.New with the agent-sandbox backend: %v", asRunnerErr)
	}
	return asRunner
}

func asPodName(runID uuid.UUID) string { return "wardyn-agent-" + runID.String() }

// startRun creates a run with the conformance agent image, changed by each mod; a created run is
// killed at cleanup.
func (e *asEnv) startRun(ctx context.Context, t *testing.T, r runner.Runner, runID uuid.UUID, mods ...func(*runner.SandboxSpec)) (runner.Sandbox, error) {
	t.Helper()
	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Fatal("the substrate declares no ConfinementClasses; nothing can be created")
	}
	spec := runner.SandboxSpec{
		RunID:            runID,
		Image:            e.image,
		ConfinementClass: caps.ConfinementClasses[len(caps.ConfinementClasses)-1],
		Labels:           map[string]string{"wardyn.conformance": "true"},
		ProxyConfig:      conformance.LoadableProxyConfig(),
		Resources:        runner.Resources{CPUMillis: 250, MemoryMiB: 128},
	}
	for _, mod := range mods {
		mod(&spec)
	}
	sb, err := r.CreateSandbox(ctx, spec)
	if err == nil {
		t.Cleanup(func() {
			stopCtx, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			_ = r.KillSandbox(stopCtx, sb.Ref)
		})
	}
	return sb, err
}

// asNotHeld names the first sign that pod is not held by Wardyn's gate; "" while it is.
func asNotHeld(pod *corev1.Pod) string {
	gated := false
	for _, g := range pod.Spec.SchedulingGates {
		gated = gated || g.Name == asGate
	}
	switch {
	case !gated:
		return "no " + asGate + " scheduling gate"
	case pod.Spec.NodeName != "":
		return "bound to node " + pod.Spec.NodeName
	case pod.Status.Phase != corev1.PodPending:
		return "phase " + string(pod.Status.Phase)
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionTrue {
			return "PodScheduled is True"
		}
	}
	return ""
}

func asContainerSecurity() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// asPodSpec is a Pod Security "restricted" spec that sleeps, held by Wardyn's gate when gated.
func asPodSpec(image string, gated bool) corev1.PodSpec {
	s := corev1.PodSpec{
		AutomountServiceAccountToken: ptr(false),
		RestartPolicy:                corev1.RestartPolicyNever,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   ptr(true),
			RunAsUser:      ptr(int64(65534)),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{{
			Name: "agent", Image: image, ImagePullPolicy: corev1.PullIfNotPresent,
			Command: []string{"sleep", "3600"}, SecurityContext: asContainerSecurity(),
		}},
	}
	if gated {
		s.SchedulingGates = []corev1.PodSchedulingGate{{Name: asGate}}
	}
	return s
}

func asPod(ns, name, image string, gated bool, owner *metav1.OwnerReference) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"wardyn.conformance": "true"}},
		Spec:       asPodSpec(image, gated),
	}
	if owner != nil {
		p.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return p
}

// gatedSandboxPod creates a bare Sandbox whose template carries the gate, and returns the pod its
// controller made (a create the gate policy judged) with the Sandbox's UID. The Sandbox is deleted at
// cleanup, and its pod with it.
func (e *asEnv) gatedSandboxPod(ctx context.Context, t *testing.T, name string) (*corev1.Pod, k8stypes.UID) {
	t.Helper()
	spec := asPodSpec(e.image, true)
	specMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	if err != nil {
		t.Fatalf("convert the pod spec: %v", err)
	}
	sb := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": asAPIVersion,
		"kind":       "Sandbox",
		"metadata":   map[string]any{"name": name, "namespace": e.ns},
		"spec": map[string]any{
			"service":         false,
			"failedPodPolicy": "Retain",
			"podTemplate": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"wardyn.conformance": "true"}},
				"spec":     specMap,
			},
		},
	}}
	created, err := e.dyn.Resource(asSandboxes).Namespace(e.ns).Create(ctx, sb, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create Sandbox %s: %v", name, err)
	}
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = e.dyn.Resource(asSandboxes).Namespace(e.ns).Delete(delCtx, name, metav1.DeleteOptions{})
	})
	deadline := time.Now().Add(time.Minute)
	for {
		pod, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return pod, created.GetUID()
		}
		if !apierrors.IsNotFound(err) || time.Now().After(deadline) {
			t.Fatalf("the controller made no pod for Sandbox %s within a minute (last read: %v)", name, err)
		}
		time.Sleep(time.Second)
	}
}

// otherServiceAccount creates a ServiceAccount with pod get, create, update and patch in the runs
// namespace, waits until the authorizer grants its patch, and returns its username.
func (e *asEnv) otherServiceAccount(ctx context.Context, t *testing.T, name string) string {
	t.Helper()
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns}}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "create", "update", "patch"}}},
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: e.ns}},
	}
	if _, err := e.cs.CoreV1().ServiceAccounts(e.ns).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create ServiceAccount: %v", err)
	}
	if _, err := e.cs.RbacV1().Roles(e.ns).Create(ctx, role, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create Role: %v", err)
	}
	if _, err := e.cs.RbacV1().RoleBindings(e.ns).Create(ctx, binding, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create RoleBinding: %v", err)
	}
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = e.cs.RbacV1().RoleBindings(e.ns).Delete(delCtx, name, metav1.DeleteOptions{})
		_ = e.cs.RbacV1().Roles(e.ns).Delete(delCtx, name, metav1.DeleteOptions{})
		_ = e.cs.CoreV1().ServiceAccounts(e.ns).Delete(delCtx, name, metav1.DeleteOptions{})
	})
	user := "system:serviceaccount:" + e.ns + ":" + name
	other := e.as(t, user)
	review := &authzv1.SelfSubjectAccessReview{Spec: authzv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &authzv1.ResourceAttributes{Namespace: e.ns, Resource: "pods", Verb: "patch"},
	}}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		got, err := other.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err == nil && got.Status.Allowed {
			return user
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never granted pods patch (last review: %v, %v)", user, got, err)
		}
	}
}

// asDecision asserts one admission decision: want "" is admitted; otherwise refused with want in the
// error, the words of the rule that must have refused it.
func asDecision(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Errorf("want admitted, got refused: %v", err)
	case want == "":
		t.Log("admitted")
	case err == nil:
		t.Errorf("want refused (%q), got admitted", want)
	case !strings.Contains(err.Error(), want):
		t.Errorf("refused, but not by the expected rule (%q): %v", want, err)
	default:
		t.Logf("refused: %v", err)
	}
}

// TestAgentSandboxBackendIsReallyOn: the run's agent pod is controlled by a Sandbox of the same name,
// so a typo in the switch cannot pass the suite on plain pods.
func TestAgentSandboxBackendIsReallyOn(t *testing.T) {
	requireAgentSandbox(t)
	e := newASEnv(t)
	r := agentSandboxRunner(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	runID := uuid.New()
	if _, err := e.startRun(ctx, t, r, runID); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	name := asPodName(runID)
	pod, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the run's agent pod: %v", err)
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "Sandbox" || owner.APIVersion != asAPIVersion || owner.Name != name {
		t.Fatalf("the run's pod is not controlled by its Sandbox: controller %+v", owner)
	}
	sb, err := e.dyn.Resource(asSandboxes).Namespace(e.ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the run's Sandbox: %v", err)
	}
	if sb.GetUID() != owner.UID {
		t.Fatalf("the pod's controller UID %s is not the run's Sandbox (%s)", owner.UID, sb.GetUID())
	}
	if len(pod.Spec.SchedulingGates) != 0 {
		t.Fatalf("the run started but its pod still carries %v", pod.Spec.SchedulingGates)
	}
}

// TestAgentSandboxStagedSecretStarts: a run with a delivered secret file has the stage-secrets init
// container, which sets terminationMessagePolicy itself; the real API server keeps that value, so the
// read-back passes and the run starts.
func TestAgentSandboxStagedSecretStarts(t *testing.T) {
	requireAgentSandbox(t)
	e := newASEnv(t)
	r := agentSandboxRunner(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	runID := uuid.New()
	secret := func(s *runner.SandboxSpec) {
		s.ManagedFiles = []runner.ManagedFile{{Path: runner.ComponentSecretDir + "/probe", Mode: runner.ComponentSecretFileMode,
			AgentOwned: true, Content: []byte("probe-secret")}}
	}
	if _, err := e.startRun(ctx, t, r, runID, secret); err != nil {
		t.Fatalf("CreateSandbox with a delivered secret file: %v", err)
	}
	pod, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, asPodName(runID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the run's agent pod: %v", err)
	}
	if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].Name != "stage-secrets" {
		t.Fatalf("the run's pod has init containers %+v, want stage-secrets alone", pod.Spec.InitContainers)
	}
	if got := pod.Spec.InitContainers[0].TerminationMessagePolicy; got != corev1.TerminationMessageFallbackToLogsOnError {
		t.Fatalf("stage-secrets terminationMessagePolicy = %q, want %q", got, corev1.TerminationMessageFallbackToLogsOnError)
	}
	if len(pod.Spec.SchedulingGates) != 0 {
		t.Fatalf("the run started but its pod still carries %v", pod.Spec.SchedulingGates)
	}
}

// TestAgentSandboxNetworkPolicyFirst: when the gate is about to come off, the agent's NetworkPolicy
// already exists and the pod is still held and unscheduled. Proves API order, not a CNI's programming.
func TestAgentSandboxNetworkPolicyFirst(t *testing.T) {
	requireAgentSandbox(t)
	e := newASEnv(t)
	r := agentSandboxRunner(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	runID := uuid.New()
	if _, err := e.startRun(ctx, t, r, runID); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	v, ok := asHeld.Load(asPodName(runID))
	if !ok {
		t.Fatal("the hook between read-back and gate removal never ran for this run")
	}
	h := v.(asHeldPod)
	if h.netPolErr != nil {
		t.Fatalf("the agent NetworkPolicy was not there while the pod was held: %v", h.netPolErr)
	}
	if h.podErr != nil {
		t.Fatalf("read the held pod: %v", h.podErr)
	}
	if why := asNotHeld(h.pod); why != "" {
		t.Fatalf("the pod was not held when its NetworkPolicy was read: %s", why)
	}
}

// TestAgentSandboxSquatterPodIsRefused: a pod with the run's name that the run did not make (here
// carrying Wardyn's gate and no owner) is refused, never released, and left alone; the run's Sandbox
// is deleted.
func TestAgentSandboxSquatterPodIsRefused(t *testing.T) {
	requireAgentSandbox(t)
	e := newASEnv(t)
	r := agentSandboxRunner(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	runID := uuid.New()
	name := asPodName(runID)
	squatter := asPod(e.ns, name, e.image, true, nil)
	squatter.Labels["wardyn.conformance"] = "squatter"
	created, err := e.cs.CoreV1().Pods(e.ns).Create(ctx, squatter, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create the squatter pod: %v", err)
	}
	t.Cleanup(func() {
		delCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = e.cs.CoreV1().Pods(e.ns).Delete(delCtx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
	})

	_, err = e.startRun(ctx, t, r, runID)
	if err == nil {
		t.Fatal("a run started over a pod it did not make")
	}
	if !strings.Contains(err.Error(), "this run did not start") {
		t.Errorf("refused, but not by the read-back: %v", err)
	}
	t.Logf("refused: %v", err)
	got, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err != nil:
		t.Errorf("the squatter pod is gone after the refusal (%v): Wardyn deleted a pod it did not make", err)
	case got.UID != created.UID:
		t.Errorf("the squatter pod was replaced (uid %s, was %s)", got.UID, created.UID)
	default:
		if why := asNotHeld(got); why != "" {
			t.Errorf("the squatter pod was released: %s", why)
		}
	}
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		_, err := e.dyn.Resource(asSandboxes).Namespace(e.ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refused run's Sandbox is still there a minute later (last read: %v)", err)
		}
	}
}

// TestAgentSandboxGoneAfterTeardown: once Kill or Stop returns, the run's Sandbox is gone, and for 60 s
// no pod with the run's name or labels comes back (the controller recreates a deleted pod while its
// Sandbox exists). ClosedOffAfterTeardown in TestConformanceK8s covers the pods and connections.
func TestAgentSandboxGoneAfterTeardown(t *testing.T) {
	requireAgentSandbox(t)
	e := newASEnv(t)
	r := agentSandboxRunner(t, e)
	for _, tc := range []struct {
		name     string
		teardown func(context.Context, string) error
	}{
		{"Kill", r.KillSandbox},
		{"Stop", r.StopSandbox},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			runID := uuid.New()
			sb, err := e.startRun(ctx, t, r, runID)
			if err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			if err := tc.teardown(ctx, sb.Ref); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			name := asPodName(runID)
			if _, err := e.dyn.Resource(asSandboxes).Namespace(e.ns).Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatalf("%s returned but the run's Sandbox is still there (read: %v)", tc.name, err)
			}
			byName := metav1.ListOptions{FieldSelector: "metadata.name=" + name}
			byRun := metav1.ListOptions{LabelSelector: "wardyn.run-id=" + runID.String()}
			start := time.Now()
			for time.Since(start) < 60*time.Second {
				for _, opts := range []metav1.ListOptions{byName, byRun} {
					pods, err := e.cs.CoreV1().Pods(e.ns).List(ctx, opts)
					if err != nil {
						t.Fatalf("list the run's pods: %v", err)
					}
					if len(pods.Items) > 0 {
						t.Fatalf("pod %s (%s) is there %s after %s returned", pods.Items[0].Name, pods.Items[0].Status.Phase,
							time.Since(start).Round(time.Second), tc.name)
					}
				}
				time.Sleep(2 * time.Second)
			}
		})
	}
}

// TestAgentSandboxPodReplacedOutsideWardynEndsTheRun: a run's pod deleted behind Wardyn's back comes
// back from the controller held by the gate; Status reads the run failed with the person-facing failure
// hint, and the replacement is never released.
func TestAgentSandboxPodReplacedOutsideWardynEndsTheRun(t *testing.T) {
	requireAgentSandbox(t)
	e := newASEnv(t)
	r := agentSandboxRunner(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	runID := uuid.New()
	sb, err := e.startRun(ctx, t, r, runID)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	name := asPodName(runID)
	before, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the run's pod: %v", err)
	}
	if err := e.cs.CoreV1().Pods(e.ns).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))}); err != nil {
		t.Fatalf("delete the run's pod: %v", err)
	}
	var last runner.Status
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(2 * time.Second) {
		st, err := r.Status(ctx, sb.Ref)
		if err == nil {
			last = st
			if st.State == types.RunFailed && strings.HasPrefix(st.FailureHint, asReplacedHint) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("Status never read the replaced pod as failed with the replaced-pod hint; last status %+v (last error %v)", last, err)
		}
	}
	got, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the replacement pod: %v", err)
	}
	if got.UID == before.UID {
		t.Fatalf("Status read the run failed, but the pod is the original (%s)", got.UID)
	}
	if why := asNotHeld(got); why != "" {
		t.Fatalf("the replacement pod was released: %s", why)
	}
}

// TestAgentSandboxAdmission drives every decision of the chart's two ValidatingAdmissionPolicies on
// this API server: pod CREATE by the controller (the gate, a Sandbox owner, no node), and the UPDATE that
// removes the gate (only wardynd's identity, only from a Sandbox's pod, nothing else changed with it).
// Each decision but the last two is a server-side dry run, so the live pod stays held between cases.
func TestAgentSandboxAdmission(t *testing.T) {
	requireAgentSandbox(t)
	e := newASEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	self, err := e.cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("SelfSubjectReview: %v", err)
	}
	if me := self.Status.UserInfo.Username; !strings.HasPrefix(me, "system:serviceaccount:") {
		t.Fatalf("the suite runs as %q; it must run as the ServiceAccount the ungate policy names as wardynd's (ci.yml renders it for wardyn/wardynd)", me)
	}
	controller := os.Getenv("WARDYN_TEST_K8S_AGENT_SANDBOX_CONTROLLER")
	if controller == "" {
		controller = asControllerDefault
	}
	suffix := uuid.New().String()[:8]
	asController := e.as(t, controller)
	asOther := e.as(t, e.otherServiceAccount(ctx, t, "wardyn-conformance-other-"+suffix))
	nodes, err := e.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(nodes.Items) == 0 {
		t.Fatal("the cluster lists no nodes")
	}
	node := nodes.Items[0].Name

	// The controller made this pod, so the gate policy has already admitted one create by it.
	held, sbUID := e.gatedSandboxPod(ctx, t, "wardyn-conformance-gate-"+suffix)
	if why := asNotHeld(held); why != "" {
		t.Fatalf("the controller's pod for a gated Sandbox is not held: %s", why)
	}
	dryRun := []string{metav1.DryRunAll}

	t.Run("Create", func(t *testing.T) {
		owner := metav1.OwnerReference{APIVersion: asAPIVersion, Kind: "Sandbox", Name: held.Name, UID: sbUID, Controller: ptr(true)}
		for _, tc := range []struct {
			name   string
			client kubernetes.Interface
			mutate func(*corev1.Pod)
			want   string
		}{
			{"ByTheControllerWithTheGateASandboxOwnerAndNoNodeIsAdmitted", asController, func(*corev1.Pod) {}, ""},
			{"ByTheControllerWithoutTheGateIsDenied", asController, func(p *corev1.Pod) { p.Spec.SchedulingGates = nil }, asDenyNoGate},
			{"ByTheControllerWithoutASandboxOwnerIsDenied", asController, func(p *corev1.Pod) { p.OwnerReferences = nil }, asDenyNoOwner},
			{"ByTheControllerWithANodeAndTheGateIsInvalid", asController, func(p *corev1.Pod) { p.Spec.NodeName = node }, asInvalidNodeWithGate},
			{"ByTheControllerWithANodeAndNoGateIsDenied", asController, func(p *corev1.Pod) {
				p.Spec.NodeName = node
				p.Spec.SchedulingGates = nil
			}, asDenyNoGate},
			{"ByAnotherIdentityIsNotSelected", e.cs, func(p *corev1.Pod) {
				p.Spec.SchedulingGates = nil
				p.OwnerReferences = nil
			}, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				p := asPod(e.ns, "wardyn-conformance-create-"+suffix, e.image, true, &owner)
				tc.mutate(p)
				_, err := tc.client.CoreV1().Pods(e.ns).Create(ctx, p, metav1.CreateOptions{DryRun: dryRun})
				asDecision(t, err, tc.want)
			})
		}
	})

	ungate := func(p *corev1.Pod) { p.Spec.SchedulingGates = nil }
	also := func(change func(*corev1.Pod)) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			ungate(p)
			change(p)
		}
	}
	// update applies mutate to the live pod and sends it as client, as a dry run; a conflict (the
	// kubelet or the controller wrote the pod in between) re-reads and tries again.
	update := func(client kubernetes.Interface, name string, mutate func(*corev1.Pod)) error {
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			p, err := e.cs.CoreV1().Pods(e.ns).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			mutate(p)
			_, err = client.CoreV1().Pods(e.ns).Update(ctx, p, metav1.UpdateOptions{DryRun: dryRun})
			return err
		})
	}

	t.Run("Ungate", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			client kubernetes.Interface
			mutate func(*corev1.Pod)
			want   string
		}{
			{"ByWardyndIsAdmitted", e.cs, ungate, ""},
			{"ByTheControllerIsDenied", asController, ungate, asDenyWho},
			{"ByAnotherServiceAccountIsDenied", asOther, ungate, asDenyWho},
			{"WithALabelIsDenied", e.cs, also(func(p *corev1.Pod) {
				if p.Labels == nil {
					p.Labels = map[string]string{}
				}
				p.Labels["conformance.wardyn.dev/extra"] = "x"
			}), asDenyAlsoChanged},
			{"WithAnAnnotationIsDenied", e.cs, also(func(p *corev1.Pod) {
				if p.Annotations == nil {
					p.Annotations = map[string]string{}
				}
				p.Annotations["conformance.wardyn.dev/extra"] = "x"
			}), asDenyAlsoChanged},
			{"WithAnImageIsDenied", e.cs, also(func(p *corev1.Pod) { p.Spec.Containers[0].Image += "-other" }), asDenyAlsoChanged},
			{"WithANodeSelectorIsDenied", e.cs, also(func(p *corev1.Pod) {
				if p.Spec.NodeSelector == nil {
					p.Spec.NodeSelector = map[string]string{}
				}
				p.Spec.NodeSelector["kubernetes.io/os"] = "linux"
			}), asDenyAlsoChanged},
			{"WithAnAffinityIsDenied", e.cs, also(func(p *corev1.Pod) {
				p.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}}},
					}}},
				}}
			}), asDenyAlsoChanged},
			{"WithATolerationIsDenied", e.cs, also(func(p *corev1.Pod) {
				p.Spec.Tolerations = append(p.Spec.Tolerations, corev1.Toleration{Key: "conformance", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule})
			}), asDenyAlsoChanged},
			{"WithActiveDeadlineSecondsIsDenied", e.cs, also(func(p *corev1.Pod) {
				if p.Spec.ActiveDeadlineSeconds == nil {
					p.Spec.ActiveDeadlineSeconds = ptr(int64(3600))
				} else {
					*p.Spec.ActiveDeadlineSeconds--
				}
			}), asDenyAlsoChanged},
			{"WithAnOwnerReferenceIsDenied", e.cs, also(func(p *corev1.Pod) {
				p.OwnerReferences = append(p.OwnerReferences, metav1.OwnerReference{
					APIVersion: "v1", Kind: "ConfigMap", Name: "conformance", UID: k8stypes.UID(uuid.New().String()),
				})
			}), asDenyAlsoChanged},
			// A plain update cannot add an ephemeral container: the API server refuses it before admission.
			{"WithAnEphemeralContainerInTheSameUpdateIsInvalid", e.cs, also(func(p *corev1.Pod) {
				p.Spec.EphemeralContainers = append(p.Spec.EphemeralContainers, asDebugContainer(e.image))
			}), asInvalidPodUpdate},
		} {
			t.Run(tc.name, func(t *testing.T) { asDecision(t, update(tc.client, held.Name, tc.mutate), tc.want) })
		}
	})

	// The ephemeralcontainers subresource is how one is really added (wardynd's Role holds it). Once a
	// held pod has one, its gate can no longer be removed.
	t.Run("UngateAfterAnEphemeralContainerIsDenied", func(t *testing.T) {
		p, _ := e.gatedSandboxPod(ctx, t, "wardyn-conformance-eph-"+suffix)
		p.Spec.EphemeralContainers = append(p.Spec.EphemeralContainers, asDebugContainer(e.image))
		if _, err := e.cs.CoreV1().Pods(e.ns).UpdateEphemeralContainers(ctx, p.Name, p, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("add an ephemeral container to the held pod: %v", err)
		}
		asDecision(t, update(e.cs, p.Name, ungate), asDenyAlsoChanged)
	})

	t.Run("UngateOfAPodNoSandboxOwnsIsDenied", func(t *testing.T) {
		name := "wardyn-conformance-plain-" + suffix
		if _, err := e.cs.CoreV1().Pods(e.ns).Create(ctx, asPod(e.ns, name, e.image, true, nil), metav1.CreateOptions{}); err != nil {
			t.Fatalf("create a gated pod no Sandbox owns: %v", err)
		}
		t.Cleanup(func() {
			delCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
			defer stop()
			_ = e.cs.CoreV1().Pods(e.ns).Delete(delCtx, name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
		})
		asDecision(t, update(e.cs, name, ungate), asDenyNotSandbox)
	})

	// Real requests from here: the gate comes off the held pod as the driver removes it (the pod's uid
	// and the exact gate list tested in the same patch), then an ordinary update of the now gate-less pod
	// by another identity, which the ungate policy would refuse if it selected the pod.
	t.Run("RealUngateByWardyndIsAdmitted", func(t *testing.T) {
		patch, err := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": string(held.UID)},
			{"op": "test", "path": "/spec/schedulingGates", "value": []map[string]string{{"name": asGate}}},
			{"op": "remove", "path": "/spec/schedulingGates"},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.cs.CoreV1().Pods(e.ns).Patch(ctx, held.Name, k8stypes.JSONPatchType, patch, metav1.PatchOptions{})
		asDecision(t, err, "")
	})
	t.Run("AGateLessPodIsNotSelected", func(t *testing.T) {
		patch := []byte(`{"metadata":{"labels":{"conformance.wardyn.dev/after":"x"}}}`)
		_, err := asOther.CoreV1().Pods(e.ns).Patch(ctx, held.Name, k8stypes.MergePatchType, patch, metav1.PatchOptions{})
		asDecision(t, err, "")
	})
}

func asDebugContainer(image string) corev1.EphemeralContainer {
	return corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
		Name: "debug", Image: image, ImagePullPolicy: corev1.PullIfNotPresent,
		Command: []string{"sleep", "60"}, SecurityContext: asContainerSecurity(),
	}}
}
