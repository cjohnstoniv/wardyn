// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

// TestConformanceK8s_ProxyConfigIsAReadableOwnerOnlyFile proves, against a
// REAL apiserver and kubelet, the ONE thing no fake-clientset test in
// internal/runner/k8s can see: that the Secret-volume-projection ->
// nonroot-init-container-stage -> in-memory-emptyDir pipeline actually
// produces a file the intended reader can open and a DIFFERENT uid cannot
// (T-28, issue #688). It builds the pod directly (not through the runner
// abstraction, which only Execs into the AGENT pod) using the exact same
// wardyn-proxy image and CLI contract (-stage-config-src/-dst) production's
// internal/runner/k8s.CreateSandbox wires onto the real proxy pod, so a
// regression in either the image's flag handling or the volume/mode
// plumbing shows up here even though this test never calls CreateSandbox.
//
// Does NOT `kubectl exec` into the distroless wardyn-proxy image (no shell) —
// the reads happen in two dedicated probe containers running the conformance
// agent image (busybox), one at the staging uid (65532, expected to read
// fine) and one at a different uid (1000, the agent's own convention —
// agentSecurityContext — expected to be denied). See this file's doc above.
import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	proxyConfigProbeSecretVolume = "secret"
	proxyConfigProbeStagedVolume = "staged"
	proxyConfigProbeSecretDir    = "/secret"
	proxyConfigProbeStagedDir    = "/staged"
	proxyConfigProbeFileName     = "config.json"
	proxyConfigProbeToken        = "conformance-owner-only-probe-token"
)

// ownerReadScript runs as the staging uid (65532): the staged file must
// exist, be readable, carry the exact bytes staged, and be mode 0400.
const ownerReadScript = `set -eu
mode=$(stat -c '%a' ` + proxyConfigProbeStagedDir + `/` + proxyConfigProbeFileName + `) || exit 90
[ "$mode" = "400" ] || { echo "mode=$mode want 400" >&2; exit 91; }
grep -q ` + proxyConfigProbeToken + ` ` + proxyConfigProbeStagedDir + `/` + proxyConfigProbeFileName + ` || exit 92
exit 0`

// otherUIDReadScript runs as uid 1000 (agentSecurityContext's own
// convention — a stand-in for "any principal that is not the proxy's own
// staging uid"): reading the 0400 file must be DENIED. Exit 93 (not the
// shell's own nonzero from a failed cat) is used ONLY when the read
// unexpectedly SUCCEEDS, so the security breach this proves and an
// unrelated script problem are never confused with each other.
const otherUIDReadScript = `if cat ` + proxyConfigProbeStagedDir + `/` + proxyConfigProbeFileName + ` 2>/dev/null; then exit 93; fi
exit 0`

func k8sConformanceClientset(t *testing.T) (*kubernetes.Clientset, string) {
	t.Helper()
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		restCfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			t.Fatalf("kubeconfig: %v", err)
		}
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	ns := os.Getenv("WARDYN_K8S_NAMESPACE")
	if ns == "" {
		ns = "default"
	}
	return cs, ns
}

func boolPtrP(b bool) *bool    { return &b }
func int64PtrP(i int64) *int64 { return &i }
func int32PtrP(i int32) *int32 { return &i }

// probeSecurityContext is the Restricted-PSS-compliant shape every container
// in this test uses, mirroring internal/runner/k8s's baseSecurityContext:
// RunAsNonRoot, no privilege escalation, all capabilities dropped, the
// runtime-default seccomp profile. uid is nil for the init container (it
// inherits the wardyn-proxy image's own nonroot default, exactly like
// production's restrictedSecurityContext), and explicit for the two probe
// readers.
func probeSecurityContext(uid *int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             boolPtrP(true),
		RunAsUser:                uid,
		AllowPrivilegeEscalation: boolPtrP(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func TestConformanceK8s_ProxyConfigIsAReadableOwnerOnlyFile(t *testing.T) {
	if os.Getenv("WARDYN_TEST_K8S") != "1" {
		t.Skip("WARDYN_TEST_K8S=1 not set; skipping k8s conformance")
	}
	proxyImage := os.Getenv("WARDYN_PROXY_IMAGE")
	if proxyImage == "" {
		t.Fatal("WARDYN_PROXY_IMAGE must name the real wardyn-proxy build (this test invokes its real -stage-config-src/-dst flags)")
	}
	agentImage := os.Getenv("WARDYN_TEST_K8S_AGENT_IMAGE")
	if agentImage == "" {
		t.Fatal("WARDYN_TEST_K8S_AGENT_IMAGE must name the busybox-based conformance agent image (the two probe containers need a shell, stat, grep and cat — the distroless proxy image has none)")
	}

	cs, ns := k8sConformanceClientset(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	runID := uuid.New()
	suffix := runID.String()[:8]
	secretName := "wardyn-conformance-proxyconfig-" + suffix
	podName := "wardyn-conformance-proxyconfig-" + suffix

	cfg := struct {
		RunID           string `json:"run_id"`
		ControlPlaneURL string `json:"control_plane_url"`
		RunToken        string `json:"run_token"`
	}{RunID: runID.String(), ControlPlaneURL: "http://127.0.0.1:9", RunToken: proxyConfigProbeToken}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal probe config: %v", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{proxyConfigProbeFileName: cfgJSON},
	}
	if _, err := cs.CoreV1().Secrets(ns).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create probe Secret: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Secrets(ns).Delete(context.Background(), secretName, metav1.DeleteOptions{})
	})

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ns, Labels: map[string]string{"wardyn.conformance": "true"}},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolPtrP(false),
			// FSGroup mirrors internal/runner/k8s's proxyNonrootGID wiring:
			// a Secret-projected file is always root:root-owned regardless of
			// its Mode, so the init container's uid-65532 read of the
			// group-readable (0440) source needs the kubelet to also chown
			// that file's GROUP to 65532 — proven necessary empirically
			// against a live cluster (T-28/#688's own real-cluster find).
			SecurityContext: &corev1.PodSecurityContext{FSGroup: int64PtrP(65532)},
			InitContainers: []corev1.Container{{
				Name:  "stage-proxy-config",
				Image: proxyImage,
				Args: []string{
					"-stage-config-src", proxyConfigProbeSecretDir + "/" + proxyConfigProbeFileName,
					"-stage-config-dst", proxyConfigProbeStagedDir + "/" + proxyConfigProbeFileName,
				},
				SecurityContext: probeSecurityContext(nil), // inherits the image's own nonroot uid, like production
				VolumeMounts: []corev1.VolumeMount{
					{Name: proxyConfigProbeSecretVolume, MountPath: proxyConfigProbeSecretDir, ReadOnly: true},
					{Name: proxyConfigProbeStagedVolume, MountPath: proxyConfigProbeStagedDir},
				},
			}},
			Containers: []corev1.Container{
				{
					Name:            "owner-read",
					Image:           agentImage,
					Command:         []string{"sh", "-c", ownerReadScript},
					SecurityContext: probeSecurityContext(int64PtrP(65532)), // the staging uid: must succeed
					VolumeMounts:    []corev1.VolumeMount{{Name: proxyConfigProbeStagedVolume, MountPath: proxyConfigProbeStagedDir, ReadOnly: true}},
				},
				{
					Name:            "other-uid-read",
					Image:           agentImage,
					Command:         []string{"sh", "-c", otherUIDReadScript},
					SecurityContext: probeSecurityContext(int64PtrP(1000)), // NOT the staging uid: must be denied
					VolumeMounts:    []corev1.VolumeMount{{Name: proxyConfigProbeStagedVolume, MountPath: proxyConfigProbeStagedDir, ReadOnly: true}},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: proxyConfigProbeSecretVolume,
					VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						SecretName: secretName,
						Items: []corev1.KeyToPath{{
							Key:  proxyConfigProbeFileName,
							Path: proxyConfigProbeFileName,
							Mode: int32PtrP(0o440),
						}},
					}},
				},
				{
					Name:         proxyConfigProbeStagedVolume,
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}},
				},
			},
		},
	}
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create probe pod: %v", err)
	}
	t.Cleanup(func() {
		zero := int64(0)
		_ = cs.CoreV1().Pods(ns).Delete(context.Background(), podName, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	})

	final, err := waitPodTerminal(ctx, cs, ns, podName)
	if err != nil {
		t.Fatalf("wait for probe pod: %v", err)
	}

	ownerCode, ownerOK := containerExitCode(final, "owner-read")
	if !ownerOK {
		t.Fatalf("owner-read container never reported a terminated status: %+v", final.Status.ContainerStatuses)
	}
	switch ownerCode {
	case 0:
		// staged file exists, mode 0400, exact content — proven.
	case 90:
		t.Error("owner-read: stat failed — the staged file does not exist at all")
	case 91:
		t.Error("owner-read: staged file mode != 400")
	case 92:
		t.Error("owner-read: staged file content does not carry the staged token")
	default:
		t.Errorf("owner-read exited %d, an unexpected code", ownerCode)
	}

	otherCode, otherOK := containerExitCode(final, "other-uid-read")
	if !otherOK {
		t.Fatalf("other-uid-read container never reported a terminated status: %+v", final.Status.ContainerStatuses)
	}
	switch otherCode {
	case 0:
		// permission denied, as required — the negative control holds.
	case 93:
		t.Error("SECURITY: uid 1000 (not the staging uid) successfully read the owner-only staged proxy config file")
	default:
		t.Errorf("other-uid-read exited %d, an unexpected code", otherCode)
	}
}

// waitPodTerminal polls until every container in the pod has a terminated
// status (Succeeded or Failed overall — RestartPolicyNever means a
// container's exit is final), or the context expires.
func waitPodTerminal(ctx context.Context, cs *kubernetes.Clientset, ns, name string) (*corev1.Pod, error) {
	for {
		pod, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			return pod, nil
		}
		select {
		case <-ctx.Done():
			return pod, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func containerExitCode(pod *corev1.Pod, name string) (int32, bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == name && cs.State.Terminated != nil {
			return cs.State.Terminated.ExitCode, true
		}
	}
	return 0, false
}
