// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

// agent_standin_k8s_test.go: a pod in a run's agent position, for the node-restart test. The agent pod has
// RestartPolicy Never, so a node reboot leaves it dead and nothing would be asked from the agent's side.
// The stand-in is a copy of the agent pod that the kubelet restarts: the same labels, so the run's two
// NetworkPolicies select it as they select the agent; the same pinned proxy address and nameserver; the
// same image and command.

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// standInContainer is the k8s driver's name for the agent pod's main container.
const standInContainer = "agent"

// pinnedProxy is the name the agent pod's hostAlias pins to the proxy's address at launch.
const pinnedProxy = "wardyn-proxy"

// newAgentStandIn copies run runID's agent pod into a pod with RestartPolicy Always and no active deadline,
// waits for it to run, and answers its name. The run's teardown deletes it with the agent (its labels carry
// the run id), and so does the test's cleanup.
func newAgentStandIn(t *testing.T, cs kubernetes.Interface, ns string, runID uuid.UUID) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	agent, err := cs.CoreV1().Pods(ns).Get(ctx, "wardyn-agent-"+runID.String(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the agent pod to copy: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "wardyn-conformance-standin-" + runID.String()[:8],
			Namespace:       ns,
			Labels:          maps.Clone(agent.Labels),
			OwnerReferences: agent.OwnerReferences,
		},
		Spec: *agent.Spec.DeepCopy(),
	}
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	pod.Spec.ActiveDeadlineSeconds = nil
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the agent stand-in: %v", err)
	}
	t.Cleanup(func() { _ = cs.CoreV1().Pods(ns).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}) })
	waitFor(t, 2*time.Minute, "the agent stand-in to run", func() bool {
		p, err := cs.CoreV1().Pods(ns).Get(context.Background(), pod.Name, metav1.GetOptions{})
		return err == nil && p.Status.Phase == corev1.PodRunning
	})
	return pod.Name
}

// standInClients loads the kubeconfig on each call, so a client made after the node moved follows the copy
// pointKubeconfigAt wrote.
func standInClients(t *testing.T) (*rest.Config, *kubernetes.Clientset) {
	t.Helper()
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	return restCfg, cs
}

// standInAnswer is proxyAnswer asked from the stand-in over the exec subresource, of the proxy at proxyAddr
// (pinnedProxy, or an address): what it gets back for an https CONNECT to host, lower-cased, followed by
// probeDone.
func standInAnswer(t *testing.T, ns, pod, proxyAddr, host string) string {
	t.Helper()
	restCfg, cs := standInClients(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	script := fmt.Sprintf(`printf 'CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n' | nc -w 5 %s 3128; echo %s`, host, host, proxyAddr, probeDone)
	out, err := execInPod(ctx, restCfg, cs, ns, pod, standInContainer, []string{"sh", "-c", script}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("exec in the agent stand-in (ask the proxy at %s for %s): %v\n%s", proxyAddr, host, err, out)
	}
	return strings.ToLower(out)
}

// assertStandInNarrowed is assertNarrowed asked from the stand-in, of the proxy at proxyAddr. The narrowed
// host's refusal is the proxy's own reply, so it also proves the stand-in reaches that proxy.
func assertStandInNarrowed(t *testing.T, ns, pod, proxyAddr string) {
	t.Helper()
	if got := standInAnswer(t, ns, pod, proxyAddr, narrowedHost); !strings.Contains(got, deniedBy) {
		t.Errorf("the stand-in's answer from %s for %s lacks %q:\n%s", proxyAddr, narrowedHost, deniedBy, got)
	}
	if got := standInAnswer(t, ns, pod, proxyAddr, otherHost); strings.Contains(got, deniedBy) {
		t.Errorf("the proxy at %s refused %s to the stand-in by policy, which the narrowing did not name:\n%s", proxyAddr, otherHost, got)
	}
}
