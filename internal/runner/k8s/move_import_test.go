// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

var (
	pvcsGVR     = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	servicesGVR = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	netpolsGVR  = networkingv1.SchemeGroupVersion.WithResource("networkpolicies")
)

// helperExec is a fake exec into the move helper: it reads stdin to its end, as the helper's reader does, and answers
// err.
type helperExec struct {
	argv []string
	read []byte
	err  error
}

func (h *helperExec) factory(_, _ string, cmd []string, _, _ bool) (remotecommand.Executor, error) {
	h.argv = cmd
	return h, nil
}

func (h *helperExec) StreamWithContext(_ context.Context, opts remotecommand.StreamOptions) error {
	if opts.Stdin != nil {
		h.read, _ = io.ReadAll(opts.Stdin)
	}
	return h.err
}

func (h *helperExec) Stream(opts remotecommand.StreamOptions) error {
	return h.StreamWithContext(context.Background(), opts)
}

// importRig is a keep-on rig whose move helper pod runs at once and whose exec is helperExec.
func importRig(t *testing.T) (*keepDriver, *fake.Clientset, *dynamicfake.FakeDynamicClient, *helperExec) {
	t.Helper()
	d, cs, dyn := keepRig(t, Config{AgentSandboxKeep: true})
	he := &helperExec{}
	d.execFactory = he.factory
	cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		ga := a.(clienttesting.GetAction)
		if !strings.HasPrefix(ga.GetName(), "wardyn-move-") {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(podsGVR, ga.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		pod.Status.Phase = corev1.PodRunning
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: moveHelperContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		return true, pod, nil
	})
	return &keepDriver{Driver: d}, cs, dyn, he
}

// importSpec is a kept run as a move hands it over: no proxy config, no secret env, an agent that is revived.
func importSpec() runner.SandboxSpec {
	spec := keptSpec()
	spec.ProxyConfig = runner.ProxyConfig{}
	spec.SecretEnv = nil
	spec.Labels = map[string]string{"team": "test", labelAgent: "claude-code"}
	return spec
}

// workStream is a stream of the work folder holding one file.
func workStream(t *testing.T) (*bytes.Buffer, runner.CarriedSet) {
	t.Helper()
	set := runner.CarriedSet{Roots: []string{runner.ScratchWorkPath}}
	var buf bytes.Buffer
	w, err := movepack.NewWriter(&buf, set)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Add(movepack.Header{Path: "home/agent/work", Type: movepack.TypeDir, Mode: 0o755}, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Add(movepack.Header{Path: "home/agent/work/f", Type: movepack.TypeFile, Mode: 0o644, Size: 2}, strings.NewReader("hi")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, set
}

// namedWrites records every write either fake receives, with the object's name, in order.
func namedWrites(cs *fake.Clientset, dyn *dynamicfake.FakeDynamicClient) *[]string {
	var order []string
	record := func(a clienttesting.Action) (bool, runtime.Object, error) {
		name := ""
		switch act := a.(type) {
		case clienttesting.CreateAction: // an update has the same methods, and its object's name is read the same way
			if m, err := meta.Accessor(act.GetObject()); err == nil {
				name = m.GetName()
			}
		case clienttesting.DeleteAction:
			name = act.GetName()
		}
		switch a.GetVerb() {
		case "get", "list", "watch":
		default:
			order = append(order, a.GetVerb()+" "+a.GetResource().Resource+" "+name)
		}
		return false, nil, nil
	}
	cs.PrependReactor("*", "*", record)
	dyn.PrependReactor("*", "*", record)
	return &order
}

// importRefused runs an import expected to refuse with want before anything is created.
func importRefused(t *testing.T, k *keepDriver, cs *fake.Clientset, dyn *dynamicfake.FakeDynamicClient, spec runner.SandboxSpec, set runner.CarriedSet, want error) {
	t.Helper()
	order := namedWrites(cs, dyn)
	stream, _ := workStream(t)
	_, _, err := k.ImportSandbox(context.Background(), spec, set, 1<<20, stream)
	if !errors.Is(err, runner.ErrMoveCreate) || want != nil && !errors.Is(err, want) {
		t.Fatalf("ImportSandbox = %v, want ErrMoveCreate and %v", err, want)
	}
	if len(*order) != 0 {
		t.Fatalf("writes = %v, want none: the refusal comes before anything is created", *order)
	}
}

func TestMoveImport_RefusesExistingCarryClaim(t *testing.T) {
	k, cs, dyn, _ := importRig(t)
	spec := importSpec()
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: spec.Drives[0].ObjectName, Namespace: testNamespace}}
	if err := cs.Tracker().Create(pvcsGVR, claim, testNamespace); err != nil {
		t.Fatal(err)
	}
	_, set := workStream(t)
	importRefused(t, k, cs, dyn, spec, set, errImportClaimExists)
	_, _, err := k.ImportSandbox(context.Background(), spec, set, 1<<20, &bytes.Buffer{})
	if strings.Contains(err.Error(), spec.Drives[0].ObjectName) {
		t.Errorf("the refusal %q names the claim; it names the drive", err)
	}
}

func TestMoveImport_RefusesWithoutWorkDrive(t *testing.T) {
	k, cs, dyn, _ := importRig(t)
	spec := importSpec()
	spec.Drives = nil
	_, set := workStream(t)
	importRefused(t, k, cs, dyn, spec, set, errImportNoWorkDrive)
}

func TestMoveImport_RefusesCarriedRootWithoutCarryDrive(t *testing.T) {
	k, cs, dyn, _ := importRig(t)
	set := runner.CarriedSet{Roots: []string{runner.ScratchWorkPath, "/home/agent/elsewhere"}}
	importRefused(t, k, cs, dyn, importSpec(), set, nil)
	// A drive that carries no root would be re-attached, which a move does not do yet.
	spec := importSpec()
	other := spec.Drives[0]
	other.Target, other.ObjectName, other.DriveName = "/home/agent/drive", "wardyn-drive-other", "other"
	spec.Drives = append(spec.Drives, other)
	_, set = workStream(t)
	importRefused(t, k, cs, dyn, spec, set, errImportDriveReattach)
}

func TestMoveImport_RefusesRunAlreadyInNamespace(t *testing.T) {
	_, set := workStream(t)
	k, cs, dyn, _ := importRig(t)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: keepServiceName(asRunID), Namespace: testNamespace}}
	if err := cs.Tracker().Create(servicesGVR, svc, testNamespace); err != nil {
		t.Fatal(err)
	}
	importRefused(t, k, cs, dyn, importSpec(), set, errImportRunInNamespace)

	k, cs, dyn, _ = importRig(t)
	sb := &unstructured.Unstructured{Object: map[string]any{"apiVersion": agentSandboxAPIVersion, "kind": "Sandbox"}}
	sb.SetName(agentPodName(asRunID))
	sb.SetNamespace(testNamespace)
	if err := dyn.Tracker().Create(sandboxesGVR, sb, testNamespace); err != nil {
		t.Fatal(err)
	}
	importRefused(t, k, cs, dyn, importSpec(), set, errImportRunInNamespace)
}

// An import creates the agent's policy and the proxy's deny-all hold before the run Secret, the claim before the
// helper, the helper's policy before its pod and after it is gone, and no proxy pod at all. The helper is fed the
// stream it was given.
func TestMoveImport_HoldPolicyBeforeSecret(t *testing.T) {
	k, cs, dyn, he := importRig(t)
	order := namedWrites(cs, dyn)
	stream, set := workStream(t)
	want := stream.Bytes()
	sb, m, err := k.ImportSandbox(context.Background(), importSpec(), set, 1<<20, bytes.NewReader(slices.Clone(want)))
	if err != nil {
		t.Fatalf("ImportSandbox: %v", err)
	}
	if sb.Ref != agentPodName(asRunID) || m.Entries != 2 {
		t.Errorf("answer = %+v, %+v; want the run's ref and the stream's two entries", sb, m)
	}
	at := func(w string) int {
		i := slices.IndexFunc(*order, func(s string) bool { return strings.HasPrefix(s, w) })
		if i < 0 {
			t.Fatalf("no %q among the writes %v", w, *order)
		}
		return i
	}
	secret := at("create secrets " + secretName(asRunID))
	for _, before := range []string{
		"create networkpolicies " + agentNetPolName(asRunID),
		"create networkpolicies " + proxyHoldNetPolName(asRunID),
		"create persistentvolumeclaims",
		"create pods " + moveHelperName(asRunID),
	} {
		if at(before) > secret {
			t.Errorf("%q comes after the run Secret: %v", before, *order)
		}
	}
	if at("create persistentvolumeclaims") > at("create pods "+moveHelperName(asRunID)) {
		t.Errorf("the claim is created after the helper: %v", *order)
	}
	if slices.ContainsFunc(*order, func(s string) bool { return strings.HasPrefix(s, "create pods "+proxyPodName(asRunID)) }) {
		t.Errorf("the import created a proxy pod: %v", *order)
	}
	if !bytes.Equal(he.read, want) {
		t.Errorf("the helper read %d bytes, want the %d-byte stream re-emitted", len(he.read), len(want))
	}
	if !slices.Contains(he.argv, "-move-import") {
		t.Errorf("the helper ran %v, want -move-import", he.argv)
	}
	pols, err := cs.NetworkingV1().NetworkPolicies(testNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: labelRun + "=" + asRunID.String() + "," + labelComponent + "=" + componentProxy})
	if err != nil {
		t.Fatal(err)
	}
	if len(pols.Items) != 1 || pols.Items[0].Name != proxyHoldNetPolName(asRunID) || len(pols.Items[0].Spec.Ingress)+len(pols.Items[0].Spec.Egress) != 0 {
		t.Errorf("proxy policies after the import = %+v, want only the hold with no rules", pols.Items)
	}
}

func TestMoveHelperPod_Confined(t *testing.T) {
	k, cs, dyn, _ := importRig(t)
	var helper *corev1.Pod
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if p, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod); ok && p.Name == moveHelperName(asRunID) {
			helper = p.DeepCopy()
		}
		return false, nil, nil
	})
	order := namedWrites(cs, dyn)
	stream, set := workStream(t)
	spec := importSpec()
	if _, _, err := k.ImportSandbox(context.Background(), spec, set, 1<<20, stream); err != nil {
		t.Fatalf("ImportSandbox: %v", err)
	}
	if helper == nil {
		t.Fatal("no helper pod was created")
	}
	checkConfined(t, helper, false)
	if len(helper.Spec.Volumes) != 1 || helper.Spec.Volumes[0].PersistentVolumeClaim == nil || helper.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != spec.Drives[0].ObjectName {
		t.Errorf("helper volumes = %+v, want only the work drive's claim", helper.Spec.Volumes)
	}
	idx := func(w string) int { return slices.Index(*order, w) }
	pol, pod := "networkpolicies "+moveHelperNetPolName(asRunID), "pods "+moveHelperName(asRunID)
	if !(idx("create "+pol) >= 0 && idx("create "+pol) < idx("create "+pod) && idx("delete "+pod) < idx("delete "+pol)) {
		t.Errorf("writes = %v, want the helper's policy created before its pod and deleted after it", *order)
	}
	ro := k.moveHelperPod(moveHelper{ns: testNamespace, runID: asRunID, claims: []helperClaim{{claim: "c", target: runner.ScratchWorkPath}}, readOnly: true})
	checkConfined(t, ro, true)
}

func checkConfined(t *testing.T, pod *corev1.Pod, readOnly bool) {
	t.Helper()
	c := pod.Spec.Containers
	if len(c) != 1 || len(pod.Spec.InitContainers) != 0 {
		t.Fatalf("containers = %d, init %d; want one and none", len(c), len(pod.Spec.InitContainers))
	}
	sc := c[0].SecurityContext
	switch {
	case sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != runner.AgentUID || sc.RunAsGroup == nil || *sc.RunAsGroup != runner.AgentUID:
		t.Errorf("security context %+v, want the agent's user and group", sc)
	case sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation:
		t.Errorf("security context %+v, want a read-only root and no privilege escalation", sc)
	case sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(sc.Capabilities.Add) != 0:
		t.Errorf("capabilities %+v, want every one dropped", sc.Capabilities)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken || pod.Spec.EnableServiceLinks == nil || *pod.Spec.EnableServiceLinks {
		t.Error("the helper pod mounts a service account token or takes service links")
	}
	if len(c[0].Env)+len(c[0].EnvFrom) != 0 {
		t.Errorf("the helper has env %v %v, want none", c[0].Env, c[0].EnvFrom)
	}
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ReadOnly != readOnly {
			t.Errorf("helper volume %+v, want only claims, read-only %v", v, readOnly)
		}
	}
	for _, m := range c[0].VolumeMounts {
		if m.ReadOnly != readOnly {
			t.Errorf("helper mount %+v, want read-only %v", m, readOnly)
		}
	}
}
