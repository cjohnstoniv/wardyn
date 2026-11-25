// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A move's export and import on a real cluster with the agent-sandbox controller: a fake clientset runs no helper pod,
// and no extraction. The proxy image must be built from this tree, since the helper runs its -move- modes.
//
//	WARDYN_TEST_K8S=1 WARDYN_TEST_K8S_AGENT_SANDBOX=1 WARDYN_K8S_NAMESPACE=<ns> \
//	WARDYN_PROXY_IMAGE=<proxy built at this head> WARDYN_TEST_K8S_AGENT_IMAGE=<a claude-code agent image>

// moveKindTimeout bounds each wait of these cases.
const moveKindTimeout = 3 * time.Minute

// liveMoveDriver is the keep driver on the current kubeconfig, with wardynd's own pods named as a No network proxy's
// peer.
func liveMoveDriver(t *testing.T) (*keepDriver, string) {
	t.Helper()
	ns, proxyImage, agentImage := os.Getenv("WARDYN_K8S_NAMESPACE"), os.Getenv("WARDYN_PROXY_IMAGE"), os.Getenv("WARDYN_TEST_K8S_AGENT_IMAGE")
	if os.Getenv("WARDYN_TEST_K8S") != "1" || os.Getenv("WARDYN_TEST_K8S_AGENT_SANDBOX") != "1" || ns == "" || proxyImage == "" || agentImage == "" {
		t.Skip("WARDYN_TEST_K8S=1, WARDYN_TEST_K8S_AGENT_SANDBOX=1, WARDYN_K8S_NAMESPACE, WARDYN_PROXY_IMAGE and WARDYN_TEST_K8S_AGENT_IMAGE not all set")
	}
	d, err := New(Config{
		Namespaces: []string{ns}, ProxyImage: proxyImage, AgentSandbox: true, AgentSandboxKeep: true,
		ControlPlanePeer: &ControlPlanePeer{Namespace: ns, PodLabels: map[string]string{"app": "wardyn-move-kind"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &keepDriver{Driver: d}, agentImage
}

// moveKindSpec is a run that can be kept, with a work drive on a claim of its own that cleanup deletes.
func moveKindSpec(t *testing.T, k *keepDriver, runID uuid.UUID, image string) runner.SandboxSpec {
	t.Helper()
	suffix := uuid.NewString()[:8]
	claim := "wardyn-move-kind-" + suffix
	t.Cleanup(func() {
		_ = k.clientset.CoreV1().PersistentVolumeClaims(k.defaultNamespace()).Delete(context.Background(), claim, metav1.DeleteOptions{})
	})
	return runner.SandboxSpec{
		RunID: runID, Image: image, Interactive: true,
		Labels:      map[string]string{labelAgent: "claude-code"},
		ProxyConfig: runner.ProxyConfig{ControlPlaneURL: "http://127.0.0.1:9", RunToken: "move-kind"},
		Resources:   runner.Resources{CPUMillis: 250, MemoryMiB: 128},
		Drives: []types.DriveMount{{
			DriveID: uuid.New(), Backend: types.DriveBackendK8sPVC, ObjectName: claim,
			DriveName: "move-kind", HomeName: "mk-" + suffix, Target: runner.ScratchWorkPath, SizeMiB: 64,
		}},
	}
}

var moveKindSet = runner.CarriedSet{Roots: []string{runner.ScratchWorkPath}}

// moveKindSource is a kept run whose work folder holds a file, a directory and a link to a file outside it, ended so
// it is kept and stopped.
func moveKindSource(t *testing.T, k *keepDriver, image string) (runner.SandboxSpec, string) {
	t.Helper()
	ctx := context.Background()
	spec := moveKindSpec(t, k, uuid.New(), image)
	sb, err := k.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = k.KillSandbox(context.Background(), sb.Ref) })
	moveKindExec(ctx, t, k, sb.Ref, `mkdir -p ~/work/a && printf hello > ~/work/a/f && ln -s /etc/passwd ~/work/l`)
	if err := k.EndSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("EndSandbox: %v", err)
	}
	return spec, sb.Ref
}

func moveKindExec(ctx context.Context, t *testing.T, k *keepDriver, ref, script string) {
	t.Helper()
	sess, err := k.ExecStream(ctx, ref, runner.ExecSpec{Argv: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}
	defer func() { _ = sess.Close() }()
	if sess.Stdin != nil {
		_ = sess.Stdin.Close()
	}
	go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	_, _ = io.Copy(io.Discard, sess.Stdout)
	if code, err := sess.Wait(); err != nil || code != 0 {
		t.Fatalf("exec %q: code %d, %v", script, code, err)
	}
}

// moveKindExport exports ref and reads the stream through movepack's reader, answering the stream, its entries and
// its manifest.
func moveKindExport(t *testing.T, k *keepDriver, ref string) ([]byte, []movepack.Header, movepack.Manifest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), moveKindTimeout)
	defer cancel()
	ex, err := k.ExportState(ctx, ref, moveKindSet)
	if err != nil {
		t.Fatalf("ExportState: %v", err)
	}
	defer ex.Stream.Close()
	stream, err := io.ReadAll(ex.Stream)
	if err != nil {
		t.Fatalf("read the export: %v", err)
	}
	rd, err := movepack.NewReader(bytes.NewReader(stream), movepack.Options{Set: moveKindSet})
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	var hs []movepack.Header
	for {
		h, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return stream, hs, rd.Manifest()
		}
		if err != nil {
			t.Fatalf("the export's stream: %v", err)
		}
		hs = append(hs, h)
	}
}

// moveKindImport tears the source down and imports stream as the same run on a claim of its own.
func moveKindImport(t *testing.T, k *keepDriver, src runner.SandboxSpec, srcRef string, stream []byte) (runner.Sandbox, movepack.Manifest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), moveKindTimeout)
	defer cancel()
	if err := k.KillSandbox(ctx, srcRef); err != nil {
		t.Fatalf("KillSandbox of the source: %v", err)
	}
	spec := moveKindSpec(t, k, src.RunID, src.Image)
	spec.ProxyConfig = runner.ProxyConfig{}
	sb, m, err := k.ImportSandbox(ctx, spec, moveKindSet, 1<<30, bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("ImportSandbox: %v", err)
	}
	t.Cleanup(func() { _ = k.KillSandbox(context.Background(), sb.Ref) })
	return sb, m
}

// readsStopped asserts the run's Status reads as the runner's observer judges absent: RunStopped with no exit code.
func readsStopped(t *testing.T, k *keepDriver, ref, when string) runner.Status {
	t.Helper()
	st, err := k.Status(context.Background(), ref)
	if err != nil {
		t.Fatalf("Status %s: %v", when, err)
	}
	if st.State != types.RunStopped || st.ExitCode != nil {
		t.Fatalf("Status %s = %+v, want RunStopped with no exit code", when, st)
	}
	return st
}

func TestMoveState_K8sExportImport(t *testing.T) {
	k, image := liveMoveDriver(t)
	for _, sub := range []any{k, k.Driver} {
		if _, ok := sub.(runner.MoveImportPresenter); ok {
			t.Fatalf("%T is a runner.MoveImportPresenter; its imported pod reads absent", sub)
		}
	}
	src, srcRef := moveKindSource(t, k, image)
	stream, _, exported := moveKindExport(t, k, srcRef)
	sb, imported := moveKindImport(t, k, src, srcRef, stream)
	if imported.Digest != exported.Digest {
		t.Fatalf("the import read digest %s, the export wrote %s", imported.Digest, exported.Digest)
	}
	readsStopped(t, k, sb.Ref, "at once after the import")
	ns := k.defaultNamespace()
	var pod *corev1.Pod
	waitFor(t, "the controller's held pod", func() bool {
		p, err := k.clientset.CoreV1().Pods(ns).Get(context.Background(), sb.Ref, metav1.GetOptions{})
		pod = p
		return err == nil
	})
	if !agentPodGated(pod) {
		t.Fatal("the imported pod is not held")
	}
	if st := readsStopped(t, k, sb.Ref, "once the pod is made"); !strings.HasPrefix(st.Message, "kept") {
		t.Errorf("Status message = %q, want the kept reading", st.Message)
	}
	if _, kept, err := k.keptPodStatus(context.Background(), ns, pod); err != nil || !kept {
		t.Errorf("keptPodStatus = kept %v, %v; want the imported pod read as kept", kept, err)
	}
	if _, _, again := moveKindExport(t, k, sb.Ref); again.Digest != exported.Digest {
		t.Errorf("an export of the imported run has digest %s, want %s", again.Digest, exported.Digest)
	}
}

func TestMoveImport_WritesOnlyBeneathRoots(t *testing.T) {
	k, image := liveMoveDriver(t)
	src, srcRef := moveKindSource(t, k, image)
	stream, _, _ := moveKindExport(t, k, srcRef)
	ns := k.defaultNamespace()
	var (
		mu     sync.Mutex
		helper *corev1.Pod
	)
	stop := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
			if p, err := k.clientset.CoreV1().Pods(ns).Get(context.Background(), moveHelperName(src.RunID), metav1.GetOptions{}); err == nil {
				mu.Lock()
				helper = p
				mu.Unlock()
			}
		}
	}()
	sb, _ := moveKindImport(t, k, src, srcRef, stream)
	close(stop)
	<-watched
	mu.Lock()
	defer mu.Unlock()
	if helper == nil {
		t.Fatal("the import's helper pod was never seen")
	}
	sc := helper.Spec.Containers[0].SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Errorf("the helper's root is not read-only: %+v", sc)
	}
	for _, v := range helper.Spec.Volumes {
		if v.PersistentVolumeClaim == nil || !strings.HasPrefix(v.PersistentVolumeClaim.ClaimName, "wardyn-move-kind-") {
			t.Errorf("the helper mounts %+v, want only the import's own claim", v)
		}
	}
	if _, err := k.clientset.CoreV1().Pods(ns).Get(context.Background(), moveHelperName(src.RunID), metav1.GetOptions{}); err == nil {
		t.Error("the helper pod outlived the import")
	}
	back, hs, _ := moveKindExport(t, k, sb.Ref)
	link := false
	for _, h := range hs {
		if h.Path != "home/agent/work" && !strings.HasPrefix(h.Path, "home/agent/work/") {
			t.Errorf("entry %q is outside the work folder", h.Path)
		}
		link = link || h.Path == "home/agent/work/l" && h.Type == movepack.TypeSymlink && h.Link == "/etc/passwd"
	}
	if !link {
		t.Error("the link to /etc/passwd did not come back as a link")
	}
	if bytes.Contains(back, []byte("root:x:0:0")) {
		t.Error("the link's target content is in the stream")
	}
}

func TestMoveImport_ProxyPolicyDenyAllUntilRevive(t *testing.T) {
	k, image := liveMoveDriver(t)
	src, srcRef := moveKindSource(t, k, image)
	stream, _, _ := moveKindExport(t, k, srcRef)
	sb, _ := moveKindImport(t, k, src, srcRef, stream)
	ctx := context.Background()
	ns := k.defaultNamespace()
	sel := metav1.ListOptions{LabelSelector: labelRun + "=" + src.RunID.String() + "," + labelComponent + "=" + componentProxy}
	pols, err := k.clientset.NetworkingV1().NetworkPolicies(ns).List(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(pols.Items) != 1 || pols.Items[0].Name != proxyHoldNetPolName(src.RunID) || len(pols.Items[0].Spec.Ingress)+len(pols.Items[0].Spec.Egress) != 0 {
		t.Fatalf("proxy policies after the import = %+v, want only the hold with no rules", pols.Items)
	}
	cfg, err := runner.BuildProxyConfig(src.RunID, runner.ProxyConfig{ControlPlaneURL: "http://10.0.0.5:9", RunToken: "move-kind", NoNetwork: true}, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(ctx, moveKindTimeout)
	defer cancel()
	if _, err := k.ReplaceProxyHashedAt(rctx, sb.Ref, cfg, 0); err != nil {
		t.Fatalf("ReplaceProxyHashedAt: %v", err)
	}
	pols, err = k.clientset.NetworkingV1().NetworkPolicies(ns).List(ctx, sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(pols.Items) != 1 || pols.Items[0].Name != proxyNetPolName(src.RunID) {
		t.Fatalf("proxy policies after the revive = %+v, want the real one alone", pols.Items)
	}
	eg := pols.Items[0].Spec.Egress
	if len(eg) != 1 || len(eg[0].To) != 1 || eg[0].To[0].NamespaceSelector == nil || eg[0].To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != ns {
		t.Fatalf("the No network proxy's egress = %+v, want the control-plane peer alone", eg)
	}
	if err := k.StartSandbox(rctx, sb.Ref); err != nil {
		t.Fatalf("StartSandbox: %v", err)
	}
	pod, err := k.clientset.CoreV1().Pods(ns).Get(ctx, sb.Ref, metav1.GetOptions{})
	if err != nil || !mainContainerRunning(pod) {
		t.Fatalf("the agent after StartSandbox: %v; want its main container running", err)
	}
}
