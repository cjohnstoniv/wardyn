// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

// TestCreateSandbox_ProxyConfigIsAnOwnerOnlyFile is T-28's (issue #688) own
// pinning test for the pod-spec plumbing that makes the proxy config reach
// the sidecar as a FILE rather than a secret-backed environment variable: a
// nonroot init container (the SAME wardyn-proxy image) stages the config out
// of a Secret volume that projects only the config key at mode 0440, into a
// shared in-memory emptyDir the main container alone reads via -config.
//
// netpol_invariant_probe_test.go's TestProxySecretsLiveOnlyInTheSecret covers
// the same wiring from the opposite direction (no leak path exists anywhere
// on either pod); this file is CreateSandbox's own contract test, run
// standalone by the issue's CHECK command (`-run 'ProxyConfig|ProxySecrets'`)
// and is what a mutation to the staging wiring must fail first.
package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCreateSandbox_ProxyConfigIsAnOwnerOnlyFile(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.9")
	installAgentRunningReactor(t, cs)

	spec := testSandboxSpec()
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	proxyPod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), proxyPodName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get proxy pod: %v", err)
	}

	mainC := containerByName(proxyPod.Spec.Containers, proxyContainerName)
	if mainC == nil {
		t.Fatalf("proxy pod has no %q container", proxyContainerName)
	}
	initC := containerByName(proxyPod.Spec.InitContainers, stageProxyConfigInitName)
	if initC == nil {
		t.Fatalf("proxy pod has no %q init container", stageProxyConfigInitName)
	}

	// The init container's declared DESTINATION and the main container's
	// declared SOURCE (-config) must be the identical path — that is the
	// entire contract between the two containers, and a drift here would
	// have the main container fail closed at boot (file not found) with no
	// signal at pod-create time.
	initDst := ""
	for i, a := range initC.Args {
		if a == "-stage-config-dst" && i+1 < len(initC.Args) {
			initDst = initC.Args[i+1]
		}
	}
	mainSrc := ""
	for i, a := range mainC.Args {
		if a == "-config" && i+1 < len(mainC.Args) {
			mainSrc = mainC.Args[i+1]
		}
	}
	if initDst == "" || mainSrc == "" {
		t.Fatalf("init -stage-config-dst=%q, main -config=%q — both must be set", initDst, mainSrc)
	}
	if initDst != mainSrc {
		t.Errorf("init container stages to %q but main container reads %q; they must be the same path", initDst, mainSrc)
	}

	// The init container's SOURCE Secret volume projects EXACTLY one key —
	// the proxy config — at mode 0440, never the whole per-run Secret (which
	// also carries the agent's SecretEnv values and managed files).
	secretVol, ok := findVolume(proxyPod.Spec.Volumes, proxyConfigSecretVolumeName)
	if !ok || secretVol.Secret == nil {
		t.Fatalf("no Secret volume %q", proxyConfigSecretVolumeName)
	}
	if got := len(secretVol.Secret.Items); got != 1 {
		t.Fatalf("Secret volume projects %d keys, want exactly 1: %+v", got, secretVol.Secret.Items)
	}
	item := secretVol.Secret.Items[0]
	if item.Key != proxyConfigSecretKey {
		t.Errorf("projected key = %q, want %q", item.Key, proxyConfigSecretKey)
	}
	if item.Mode == nil || *item.Mode != 0o440 {
		t.Errorf("projected item mode = %v, want 0440", item.Mode)
	}

	// FSGroup is what makes that 0440 group-read bit effective: a Secret
	// volume's projected file is always root:root-owned regardless of Mode,
	// so without a pod-level FSGroup (any value: the kubelet adds it as a
	// supplemental group; 65532 is what the product sets), a fake clientset
	// never notices, but a REAL kubelet leaves the
	// file's group at root and the init container fails closed on
	// "permission denied" reading its own Secret volume — confirmed
	// empirically against a live cluster (see the T-28/#688 PR body).
	if proxyPod.Spec.SecurityContext == nil || proxyPod.Spec.SecurityContext.FSGroup == nil || *proxyPod.Spec.SecurityContext.FSGroup != 65532 {
		var got any
		if proxyPod.Spec.SecurityContext != nil {
			got = proxyPod.Spec.SecurityContext.FSGroup
		}
		t.Errorf("proxy pod FSGroup = %v, want 65532 (the proxy image's nonroot GID) — without it the init container cannot read the Secret-projected config", got)
	}

	// NO container anywhere on the proxy pod resolves the config via
	// SecretKeyRef any more — the file is the only path.
	for _, c := range append(append([]corev1.Container{}, proxyPod.Spec.Containers...), proxyPod.Spec.InitContainers...) {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Key == proxyConfigSecretKey {
				t.Errorf("container %s env %s still resolves the config via secretKeyRef; it must be a file", c.Name, e.Name)
			}
		}
	}

	// The init container is nonroot (same restricted security posture as the
	// main proxy container — both run the proxy image's default uid 65532).
	if initC.SecurityContext == nil || initC.SecurityContext.RunAsNonRoot == nil || !*initC.SecurityContext.RunAsNonRoot {
		t.Error("init container SecurityContext does not require RunAsNonRoot")
	}
	if initC.Image != mainC.Image {
		t.Errorf("init container image %q != main container image %q; the plan requires the SAME proxy image", initC.Image, mainC.Image)
	}
}
