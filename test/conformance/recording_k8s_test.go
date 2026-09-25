// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

// recording_k8s_test.go is TestRecordingDocker's k8s counterpart (#704): it
// builds the REAL k8s substrate (the real wardyn-proxy image, an agent image
// carrying wardyn-rec — k8s's SessionRecording is unconditionally true, see
// internal/runner/k8s/exec.go's recordCmd doc, no docker-style opt-out) and
// proves a recorded command's cast leaves the sandbox through the proxy's
// brokered recording route, over the TLS hop, to a stand-in control plane
// (test/conformance/hopfake) that logs what it received — the same proof
// TestRecordingDocker makes for docker.
//
// Unlike docker there is no shared network with a DNS alias to lean on:
// hopfake runs as its own pod in the SAME namespace as the run
// (WARDYN_K8S_NAMESPACE), fronted by a same-named ClusterIP Service so the
// PROXY sidecar — the only leg of this substrate that ever dials a control
// plane; the agent's own NetworkPolicy allows egress to nothing but the
// proxy — can resolve and reach it over cluster DNS (the proxy pod's
// NetworkPolicy explicitly allows DNS plus everything except the
// cloud-metadata address; see internal/runner/k8s/sandbox.go's proxyNetPol).
// The hopfake binary has no image of its own, so there is nothing to `kind
// load`: it is built by this test and streamed into a plain busybox pod over
// the exec subresource (the k8s analogue of docker's CopyToContainer —
// kubectl cp's own mechanism), which execs it once the copy and its TLS
// credentials land.
import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/cjohnstoniv/wardyn/internal/hoptls"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/k8s"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

// hopfakeSelectorLabel is the label the hopfake pod carries and its fronting
// Service selects on.
const hopfakeSelectorLabel = "wardyn-conformance-hopfake"

func TestRecordingK8s(t *testing.T) {
	if os.Getenv("WARDYN_TEST_K8S") != "1" {
		t.Skip("WARDYN_TEST_K8S=1 not set; skipping the k8s session-recording case")
	}
	proxyImage := os.Getenv("WARDYN_PROXY_IMAGE")
	if proxyImage == "" {
		t.Fatal("WARDYN_PROXY_IMAGE must name the REAL wardyn-proxy build (see conformance_k8s_test.go's doc comment)")
	}
	agentImage := os.Getenv("WARDYN_TEST_K8S_AGENT_IMAGE")
	if agentImage == "" {
		t.Fatal("WARDYN_TEST_K8S_AGENT_IMAGE must name an image built from deploy/kind/Dockerfile.conformance-agent (carries wardyn-rec — a recorder-less image fails every Exec closed under this substrate's unconditional SessionRecording; see conformance_k8s_test.go's doc comment)")
	}
	ns := os.Getenv("WARDYN_K8S_NAMESPACE")
	if ns == "" {
		ns = "default" // k8s.Config.withDefaults' own fallback
	}

	restCfg, cs := k8sTestClient(t)

	sub, err := k8s.New(k8s.Config{Namespace: ns, ProxyImage: proxyImage})
	if err != nil {
		t.Fatalf("k8s.New: %v (this cluster's CNI must enforce NetworkPolicy — see ci.yml's Calico step)", err)
	}
	r := orchestrator.New(sub)

	// Up front, so a substrate that stopped declaring recording fails here
	// instead of CheckRecordingCapability taking its honest-empty branch.
	caps, err := r.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.SessionRecording {
		t.Fatalf("k8s substrate declares SessionRecording=false (exec.go's recordCmd has no opt-out — this should never happen)")
	}

	caBlob, err := hoptls.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := hoptls.ParseCA(caBlob)
	if err != nil {
		t.Fatal(err)
	}
	host := "wardyn-hopfake-" + uuid.NewString()[:8]
	token := "recording-run-token-k8s"
	startHopFakeK8s(t, restCfg, cs, ns, ca, host, uuid.New(), token)

	var runID uuid.UUID
	conformance.CheckRecordingCapability(t, r, conformance.RecordingOptions{
		Options: conformance.Options{
			SandboxImage: agentImage,
			Timeout:      3 * time.Minute,
			ExitArgv:     func(int) []string { return []string{"sh", "-c", "echo wardyn-rec-canary"} },
		},
		MutateSpec: func(spec *runner.SandboxSpec) {
			spec.ProxyConfig = runner.ProxyConfig{
				RunToken:          token,
				ControlPlaneURL:   "https://" + host + ":8443",
				ControlPlaneCAPEM: string(ca.CertPEM),
				Policy:            types.RunPolicySpec{MinConfinementClass: types.CC1},
			}
			runID = spec.RunID
		},
		RecordingProbe: func(t *testing.T, _ string) {
			want := "hopfake: recording run=" + runID.String()
			logs := waitForHopfakeLog(t, cs, ns, host, want, 90*time.Second)
			for line := range strings.SplitSeq(logs, "\n") {
				if !strings.HasPrefix(line, want) {
					continue
				}
				if !strings.Contains(line, "canary=true") || !strings.Contains(line, "tls=true") {
					t.Fatalf("the cast arrived without the canary or off TLS: %q\n%s", line, logs)
				}
				if !strings.Contains(line, "auth=true") {
					t.Fatalf("the proxy did not inject this run's token on the upload: %q\n%s", line, logs)
				}
				t.Logf("control plane saw: %s", line)
				return
			}
			t.Fatalf("no line starts with %q:\n%s", want, logs)
		},
	})
}

// k8sTestClient builds a rest.Config/Clientset pair the same way the
// substrate's own config does (in-cluster first, then the default kubeconfig
// loading rules) — mirrors ephemeralStorageProbe in conformance_k8s_test.go,
// which needs the same pair for its own read-back.
func k8sTestClient(t *testing.T) (*rest.Config, *kubernetes.Clientset) {
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
	return restCfg, cs
}

// startHopFakeK8s builds test/conformance/hopfake, runs it in a plain busybox
// pod in ns fronted by a same-named ClusterIP Service (so the proxy sidecar
// can resolve host over cluster DNS), with a serving cert for host signed by
// ca. Unlike docker's startHopFake (CopyToContainer before the container's
// main process starts), a k8s pod has no such "not started yet" window to
// copy into: the pod starts with an idle loop, waits for the binary and its
// TLS credentials to land via the exec subresource (kubectl cp's own
// mechanism), then execs hopfake in the loop's place.
func startHopFakeK8s(t *testing.T, restCfg *rest.Config, cs *kubernetes.Clientset, ns string, ca *hoptls.CA, host string, grant uuid.UUID, token string) {
	t.Helper()
	bin := buildHopfakeBinary(t)
	cert, err := ca.ServingCert(host, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	labels := map[string]string{hopfakeSelectorLabel: host}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: host, Namespace: ns, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolPtr(false),
			Containers: []corev1.Container{{
				Name:  "hopfake",
				Image: "busybox:1.36",
				// Idles until the copy below lands /tmp/hopfake, sources the
				// credentials it wrote alongside it, then execs in place —
				// so the pod's own stdout (what waitForHopfakeLog reads) is
				// hopfake's log, not a wrapper shell's.
				Command: []string{"sh", "-c", "while [ ! -x /tmp/hopfake ]; do sleep 0.2; done; . /tmp/hopfake.env; exec /tmp/hopfake"},
				SecurityContext: &corev1.SecurityContext{
					RunAsNonRoot:             boolPtr(true),
					RunAsUser:                int64Ptr(65534), // busybox has no numeric non-root USER; pin one explicitly (PSS-restricted admission)
					AllowPrivilegeEscalation: boolPtr(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
			}},
		},
	}
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create hopfake pod: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Pods(ns).Delete(context.Background(), host, metav1.DeleteOptions{})
	})

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: host, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Port: 8443, TargetPort: intstr.FromInt32(8443), Protocol: corev1.ProtocolTCP}},
		},
	}
	if _, err := cs.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create hopfake service: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Services(ns).Delete(context.Background(), host, metav1.DeleteOptions{})
	})

	if err := waitForPodRunning(ctx, cs, ns, host); err != nil {
		t.Fatalf("hopfake pod never reached Running: %v", err)
	}

	binBytes, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	// A sourced env file, not exec env vars on the Pod spec: the cert/key PEM
	// and token are only known AFTER the pod (and its name, which the cert's
	// SAN is bound to) already exist, and pod env is immutable post-create.
	envFile := fmt.Sprintf("export HOPFAKE_CERT_PEM=%s\nexport HOPFAKE_KEY_PEM=%s\nexport HOPFAKE_GRANT=%s\nexport HOPFAKE_TOKEN=%s\n",
		shQuote(string(certPEM)), shQuote(string(keyPEM)), shQuote(grant.String()), shQuote(token))

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	writeTarFile(t, tw, "hopfake", 0o755, binBytes)
	writeTarFile(t, tw, "hopfake.env", 0o644, []byte(envFile))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if out, err := execInPod(ctx, restCfg, cs, ns, host, "hopfake", []string{"sh", "-c", "cd /tmp && tar -xf -"}, &tarBuf); err != nil {
		t.Fatalf("copy hopfake into pod: %v\n%s", err, out)
	}
}

// shQuote wraps s in single quotes for embedding in a generated shell
// script. The only inputs are PEM blocks, a UUID and a fixed token literal —
// none of which ever carry a single quote — so plain wrapping is enough; this
// is test-fixture plumbing, not a general-purpose shell escaper.
func shQuote(s string) string { return "'" + s + "'" }

func writeTarFile(t *testing.T, tw *tar.Writer, name string, mode int64, data []byte) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
}

// waitForPodRunning polls until name's phase is Running with at least one
// container actually started, or ctx's deadline passes.
func waitForPodRunning(ctx context.Context, cs *kubernetes.Clientset, ns, name string) error {
	for {
		pod, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && pod.Status.Phase == corev1.PodRunning {
			for _, st := range pod.Status.ContainerStatuses {
				if st.State.Running != nil {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("get pod: %w", err)
			}
			return fmt.Errorf("still %q: %w", pod.Status.Phase, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// execInPod runs cmd in pod/container over the exec subresource, feeding it
// stdin and returning its combined stdout+stderr. This is the k8s analogue
// of docker's CopyToContainer — the same mechanism `kubectl cp` and `kubectl
// exec` use (a POST to the exec subresource, SPDY-streamed).
func execInPod(ctx context.Context, restCfg *rest.Config, cs *kubernetes.Clientset, ns, pod, container string, cmd []string, stdin *bytes.Buffer) (string, error) {
	req := cs.CoreV1().RESTClient().Post().
		Namespace(ns).
		Resource("pods").
		Name(pod).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(restCfg, http.MethodPost, req.URL())
	if err != nil {
		return "", fmt.Errorf("build spdy executor: %w", err)
	}
	var out bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &out,
		Stderr: &out,
	})
	return out.String(), err
}

// buildHopfakeBinary builds test/conformance/hopfake for the cluster node's
// platform (assumed to match the local runtime's GOARCH, true for a kind
// node on this same machine — mirrors the docker test's identical
// assumption) as a static linux binary, so it needs nothing from the
// busybox image it lands in.
func buildHopfakeBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hopfake")
	build := exec.Command("go", "build", "-o", bin, "./hopfake")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hopfake: %v\n%s", err, out)
	}
	return bin
}

// waitForHopfakeLog polls the hopfake pod's own container log (hopfake execs
// in place of the pod's idle shell, so the pod log IS hopfake's log) until it
// contains want or within elapses.
func waitForHopfakeLog(t *testing.T, cs *kubernetes.Clientset, ns, pod, want string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		logs := hopfakePodLogs(cs, ns, pod)
		if strings.Contains(logs, want) {
			return logs
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q never appeared in %s's logs:\n%s", want, pod, logs)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func hopfakePodLogs(cs *kubernetes.Clientset, ns, pod string) string {
	b, err := cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: "hopfake"}).DoRaw(context.Background())
	if err != nil {
		return "logs: " + err.Error()
	}
	return string(b)
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }
