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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
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
	if !errors.Is(err, runner.ErrMoveCreate) || !errors.Is(err, want) {
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
	importRefused(t, k, cs, dyn, importSpec(), set, errImportRootNoDrive)
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

func TestMoveImport_RefusesSecretEnv(t *testing.T) {
	k, cs, dyn, _ := importRig(t)
	spec := importSpec()
	spec.SecretEnv = map[string]string{"TOKEN": "x"}
	_, set := workStream(t)
	importRefused(t, k, cs, dyn, spec, set, errImportSecretEnv)
}

func TestMoveImport_RefusesUnkeptRun(t *testing.T) {
	for name, edit := range map[string]func(*runner.SandboxSpec){
		"not interactive":      func(s *runner.SandboxSpec) { s.Interactive = false },
		"agent is not revived": func(s *runner.SandboxSpec) { s.Labels[labelAgent] = "aider" },
	} {
		t.Run(name, func(t *testing.T) {
			k, cs, dyn, _ := importRig(t)
			spec := importSpec()
			edit(&spec)
			_, set := workStream(t)
			importRefused(t, k, cs, dyn, spec, set, errImportNotKept)
		})
	}
}

// A failure after the first object tears the run down: its policies (the hold too), Secret, Service, address and pods
// go, and the carry claim stays for the move's drive rows.
func TestMoveImport_FailureRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		rig  func(*helperExec, *fake.Clientset, *dynamicfake.FakeDynamicClient)
		want error
	}{
		{"the helper fails", func(he *helperExec, _ *fake.Clientset, _ *dynamicfake.FakeDynamicClient) {
			he.err = errors.New("test: the helper failed")
		}, runner.ErrMoveFiles},
		{"the Sandbox create fails", func(_ *helperExec, _ *fake.Clientset, dyn *dynamicfake.FakeDynamicClient) {
			dyn.PrependReactor("create", "sandboxes", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("test: Sandbox create refused")
			})
		}, runner.ErrMoveCreate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, cs, dyn, he := importRig(t)
			tc.rig(he, cs, dyn)
			order := namedWrites(cs, dyn)
			stream, set := workStream(t)
			spec := importSpec()
			if _, _, err := k.ImportSandbox(context.Background(), spec, set, 1<<20, stream); !errors.Is(err, tc.want) {
				t.Fatalf("ImportSandbox = %v, want %v", err, tc.want)
			}
			if !slices.ContainsFunc(*order, func(s string) bool { return strings.HasPrefix(s, "create networkpolicies") }) {
				t.Fatalf("writes = %v, want the import to have created its policies first", *order)
			}
			sel := metav1.ListOptions{LabelSelector: labelRun + "=" + asRunID.String()}
			ctx := context.Background()
			pols, err := cs.NetworkingV1().NetworkPolicies(testNamespace).List(ctx, sel)
			if err != nil {
				t.Fatal(err)
			}
			pods, err := cs.CoreV1().Pods(testNamespace).List(ctx, sel)
			if err != nil {
				t.Fatal(err)
			}
			if len(pols.Items)+len(pods.Items) != 0 {
				t.Errorf("after the rollback: %d policies, %d pods of the run, want none", len(pols.Items), len(pods.Items))
			}
			if _, err := cs.Tracker().Get(secretsGVR, testNamespace, secretName(asRunID)); !isNotFound(err) {
				t.Errorf("the run Secret is still there (%v)", err)
			}
			if _, err := cs.Tracker().Get(servicesGVR, testNamespace, keepServiceName(asRunID)); !isNotFound(err) {
				t.Errorf("the run Service is still there (%v)", err)
			}
			if _, err := cs.Tracker().Get(pvcsGVR, testNamespace, spec.Drives[0].ObjectName); err != nil {
				t.Errorf("the carry claim is gone (%v), want it kept", err)
			}
		})
	}
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
	if !(idx("create "+pol) >= 0 && idx("create "+pol) < idx("create "+pod) && idx("delete "+pod) >= 0 && idx("delete "+pod) < idx("delete "+pol)) {
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

// helperWatch records the helper pod's deletes. After each one it answers the deleted pod to the next Get, so a
// cleanup that does not wait for the pod to go reaches its policy delete with the pod still there. policyAfter holds,
// for each helper policy delete that follows a pod delete, how many pod reads came between them; polUIDs holds the
// UID precondition of every helper policy delete.
type helperWatch struct {
	deletes     []clienttesting.DeleteAction
	gone        *corev1.Pod
	deleted     bool
	reads       int
	policyAfter []int
	polUIDs     []k8stypes.UID
}

func watchHelper(cs *fake.Clientset) *helperWatch {
	w := &helperWatch{}
	name, polName := moveHelperName(asRunID), moveHelperNetPolName(asRunID)
	cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.(clienttesting.GetAction).GetName() != name || !w.deleted {
			return false, nil, nil
		}
		w.reads++
		if w.gone != nil {
			pod := w.gone
			w.gone = nil
			return true, pod, nil
		}
		return false, nil, nil
	})
	cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		da := a.(clienttesting.DeleteAction)
		if da.GetName() != name {
			return false, nil, nil
		}
		w.deletes = append(w.deletes, da)
		w.deleted, w.reads, w.gone = true, 0, nil
		if obj, err := cs.Tracker().Get(podsGVR, da.GetNamespace(), name); err == nil {
			w.gone = obj.(*corev1.Pod).DeepCopy()
		}
		return false, nil, nil
	})
	cs.PrependReactor("delete", "networkpolicies", func(a clienttesting.Action) (bool, runtime.Object, error) {
		da := a.(clienttesting.DeleteAction)
		if da.GetName() != polName {
			return false, nil, nil
		}
		w.polUIDs = append(w.polUIDs, deleteUID(da))
		if w.deleted {
			w.policyAfter = append(w.policyAfter, w.reads)
			w.deleted = false
		}
		return false, nil, nil
	})
	return w
}

// stampHelperUID gives the helper pod podUID and its policy polUID, as the API server would on their creates.
func stampHelperUID(cs *fake.Clientset, podUID, polUID k8stypes.UID) {
	cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if p, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod); ok && p.Name == moveHelperName(asRunID) {
			p.UID = podUID
		}
		return false, nil, nil
	})
	cs.PrependReactor("create", "networkpolicies", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if p, ok := a.(clienttesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy); ok && p.Name == moveHelperNetPolName(asRunID) {
			p.UID = polUID
		}
		return false, nil, nil
	})
}

// honourPolicyUID makes a NetworkPolicy delete answer Conflict when its UID precondition names another object, as the
// API server does.
func honourPolicyUID(cs *fake.Clientset) {
	cs.PrependReactor("delete", "networkpolicies", func(a clienttesting.Action) (bool, runtime.Object, error) {
		da := a.(clienttesting.DeleteAction)
		obj, err := cs.Tracker().Get(netpolsGVR, da.GetNamespace(), da.GetName())
		if err != nil {
			return false, nil, nil
		}
		if p := da.GetDeleteOptions().Preconditions; p != nil && p.UID != nil && *p.UID != obj.(*networkingv1.NetworkPolicy).UID {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, da.GetName(), errors.New("test: UID precondition failed"))
		}
		return false, nil, nil
	})
}

func deleteUID(a clienttesting.DeleteAction) k8stypes.UID {
	if p := a.GetDeleteOptions().Preconditions; p != nil && p.UID != nil {
		return *p.UID
	}
	return ""
}

// The helper pod is deleted under its UID, and its policy only after a read finds the pod gone.
func TestMoveHelper_CleanupDeletesPodUnderItsUIDAndWaits(t *testing.T) {
	k, cs, _, _ := importRig(t)
	stampHelperUID(cs, "helper-uid", "helper-pol-uid")
	honourPolicyUID(cs)
	w := watchHelper(cs)
	stream, set := workStream(t)
	if _, _, err := k.ImportSandbox(context.Background(), importSpec(), set, 1<<20, stream); err != nil {
		t.Fatalf("ImportSandbox: %v", err)
	}
	if len(w.deletes) != 1 || deleteUID(w.deletes[0]) != "helper-uid" {
		t.Fatalf("helper pod deletes = %d (first UID %q), want one under helper-uid", len(w.deletes), firstUID(w.deletes))
	}
	if !slices.Equal(w.policyAfter, []int{2}) {
		t.Errorf("pod reads before the helper policy delete = %v, want [2]: the pod still there, then gone", w.policyAfter)
	}
	if !slices.Equal(w.polUIDs, []k8stypes.UID{"helper-pol-uid"}) {
		t.Errorf("helper policy delete UIDs = %v, want [helper-pol-uid]", w.polUIDs)
	}
}

func firstUID(ds []clienttesting.DeleteAction) k8stypes.UID {
	if len(ds) == 0 {
		return ""
	}
	return deleteUID(ds[0])
}

// A pod create that fails may still have made the pod: the cleanup reads it by name, adopts and deletes it under its
// UID, and deletes the policy only once the pod is known to be gone. A read that fails leaves the policy in place.
func TestMoveHelper_PodCreateErrorAdoptsPod(t *testing.T) {
	timeout := apierrors.NewServerTimeout(schema.GroupResource{Resource: "pods"}, "create", 1)
	for _, tc := range []struct {
		name       string
		made       bool
		readFails  bool
		wantDelete k8stypes.UID
		wantPolicy bool
	}{
		{name: "made, then refused", made: true, wantDelete: "adopted-uid"},
		{name: "never made"},
		{name: "unknown", readFails: true, wantPolicy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, cs, _, _ := importRig(t)
			stampHelperUID(cs, "", "pol-uid")
			w := watchHelper(cs)
			tried := false
			cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
				if tc.readFails && tried && a.(clienttesting.GetAction).GetName() == moveHelperName(asRunID) {
					return true, nil, apierrors.NewServerTimeout(schema.GroupResource{Resource: "pods"}, "get", 1)
				}
				return false, nil, nil
			})
			cs.PrependReactor("create", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
				p, ok := a.(clienttesting.CreateAction).GetObject().(*corev1.Pod)
				if !ok || p.Name != moveHelperName(asRunID) {
					return false, nil, nil
				}
				tried = true
				if tc.made {
					made := p.DeepCopy()
					made.UID = "adopted-uid"
					if err := cs.Tracker().Create(podsGVR, made, testNamespace); err != nil {
						t.Fatal(err)
					}
				}
				return true, nil, timeout
			})
			h := moveHelper{ns: testNamespace, runID: asRunID, claims: []helperClaim{{claim: "c", target: runner.ScratchWorkPath}}}
			err := k.runMoveHelper(context.Background(), h, []string{proxyBinary}, nil, io.Discard)
			if !errors.Is(err, timeout) {
				t.Fatalf("runMoveHelper = %v, want the create's error", err)
			}
			if firstUID(w.deletes) != tc.wantDelete || len(w.deletes) > 1 {
				t.Errorf("helper pod deletes = %d (first UID %q), want UID %q", len(w.deletes), firstUID(w.deletes), tc.wantDelete)
			}
			if tc.made && !slices.Equal(w.policyAfter, []int{2}) {
				t.Errorf("pod reads before the helper policy delete = %v, want [2]", w.policyAfter)
			}
			if _, err := cs.Tracker().Get(podsGVR, testNamespace, moveHelperName(asRunID)); !isNotFound(err) {
				t.Errorf("the helper pod is still there (%v)", err)
			}
			_, perr := cs.Tracker().Get(netpolsGVR, testNamespace, moveHelperNetPolName(asRunID))
			if kept := perr == nil; kept != tc.wantPolicy {
				t.Errorf("helper policy kept = %v, want %v", kept, tc.wantPolicy)
			}
			if wantUIDs := []k8stypes.UID{"pol-uid"}; tc.wantPolicy {
				if len(w.polUIDs) != 0 {
					t.Errorf("helper policy delete UIDs = %v, want none", w.polUIDs)
				}
			} else if !slices.Equal(w.polUIDs, wantUIDs) {
				t.Errorf("helper policy delete UIDs = %v, want %v", w.polUIDs, wantUIDs)
			}
		})
	}
}

// A helper an earlier move of this run left behind is deleted under its UID and waited for, then its policy, before
// this move creates its own; the move then goes on.
func TestMoveImport_ReclaimsStaleHelper(t *testing.T) {
	k, cs, dyn, he := importRig(t)
	h := moveHelper{ns: testNamespace, runID: asRunID, claims: []helperClaim{{claim: "c", target: runner.ScratchWorkPath}}}
	stale := k.moveHelperPod(h)
	stale.UID = "stale-uid"
	if err := cs.Tracker().Create(podsGVR, stale, testNamespace); err != nil {
		t.Fatal(err)
	}
	stalePol := k.moveHelperNetPol(h)
	stalePol.UID = "stale-pol-uid"
	if err := cs.Tracker().Create(netpolsGVR, stalePol, testNamespace); err != nil {
		t.Fatal(err)
	}
	stampHelperUID(cs, "new-uid", "new-pol-uid")
	honourPolicyUID(cs)
	w := watchHelper(cs)
	order := namedWrites(cs, dyn)
	stream, set := workStream(t)
	if _, _, err := k.ImportSandbox(context.Background(), importSpec(), set, 1<<20, stream); err != nil {
		t.Fatalf("ImportSandbox with a stale helper = %v, want nil", err)
	}
	if len(w.deletes) != 2 || deleteUID(w.deletes[0]) != "stale-uid" || deleteUID(w.deletes[1]) != "new-uid" {
		t.Fatalf("helper pod deletes = %d, want the stale one under stale-uid, then this move's under new-uid", len(w.deletes))
	}
	if !slices.Equal(w.policyAfter, []int{2, 2}) {
		t.Errorf("pod reads before each helper policy delete = %v, want [2 2]", w.policyAfter)
	}
	if !slices.Equal(w.polUIDs, []k8stypes.UID{"stale-pol-uid", "new-pol-uid"}) {
		t.Errorf("helper policy delete UIDs = %v, want [stale-pol-uid new-pol-uid]", w.polUIDs)
	}
	pol, pod := "networkpolicies "+moveHelperNetPolName(asRunID), "pods "+moveHelperName(asRunID)
	idx := func(w string) int { return slices.Index(*order, w) }
	if !(idx("delete "+pod) >= 0 && idx("delete "+pod) < idx("delete "+pol) && idx("delete "+pol) < idx("create "+pol) && idx("create "+pol) < idx("create "+pod)) {
		t.Errorf("writes = %v, want the stale pod, then its policy, deleted before the new policy and pod", *order)
	}
	if !slices.Contains(he.argv, "-move-import") {
		t.Errorf("the helper ran %v, want the import to go on", he.argv)
	}
	if strings.Contains(strings.Join(*order, "\n"), "create pods "+proxyPodName(asRunID)) {
		t.Errorf("the import created a proxy pod: %v", *order)
	}
}

// Two helpers of one run overlap: another helper's policy has replaced this one's by the time this cleanup runs. The
// delete under this policy's UID answers Conflict, so the other helper's policy stays and the cleanup returns the error.
func TestMoveHelper_PolicyDeleteKeepsAnotherHelpersPolicy(t *testing.T) {
	k, cs, _, _ := importRig(t)
	stampHelperUID(cs, "a-uid", "a-pol-uid")
	honourPolicyUID(cs)
	w := watchHelper(cs)
	swapped := false
	cs.PrependReactor("delete", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if swapped || a.(clienttesting.DeleteAction).GetName() != moveHelperName(asRunID) {
			return false, nil, nil
		}
		swapped = true
		other := k.moveHelperNetPol(moveHelper{ns: testNamespace, runID: asRunID})
		other.UID = "b-pol-uid"
		if err := cs.Tracker().Update(netpolsGVR, other, testNamespace); err != nil {
			t.Error(err)
		}
		return false, nil, nil
	})
	h := moveHelper{ns: testNamespace, runID: asRunID, claims: []helperClaim{{claim: "c", target: runner.ScratchWorkPath}}}
	err := k.runMoveHelper(context.Background(), h, []string{proxyBinary}, nil, io.Discard)
	if !apierrors.IsConflict(err) {
		t.Fatalf("runMoveHelper = %v, want the policy delete's Conflict", err)
	}
	if !slices.Equal(w.polUIDs, []k8stypes.UID{"a-pol-uid"}) {
		t.Errorf("helper policy delete UIDs = %v, want [a-pol-uid]", w.polUIDs)
	}
	obj, perr := cs.Tracker().Get(netpolsGVR, testNamespace, moveHelperNetPolName(asRunID))
	if perr != nil || obj.(*networkingv1.NetworkPolicy).UID != "b-pol-uid" {
		t.Errorf("the other helper's policy = %v (%v), want it kept", obj, perr)
	}
}
