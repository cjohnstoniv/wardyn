// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

// The k8s substrate's CONFINEMENT invariant, pinned end to end against a fake
// clientset: what a run's NetworkPolicies actually admit, and what its Secret
// actually holds. Three properties, each of which fails open in a way no other
// test in this package would notice.
//
//  1. The agent's egress peer set is EXACTLY this run's proxy pod on the proxy
//     port — no DNS peer, no 0.0.0.0/0, no namespaceSelector — and the policy's
//     selector still selects the agent even when spec.Labels tries to override
//     the reserved keys wardynLabels stamps.
//  2. The proxy admits ingress from exactly this run's agent selector, and every
//     egress peer is an ipBlock that excludes the cloud-metadata address.
//  3. The run token and the MITM CA private key reach the per-run Secret and
//     nothing else: the proxy consumes the config JSON as a FILE, staged by a
//     nonroot init container off a Secret volume that projects only the
//     config key, and NO EnvVar — inline or secretKeyRef — on any container
//     of either pod carries the material (T-28, issue #688: a secret-backed
//     env var still lands in the container's own process environment, which
//     an env-var credential scanner or `kubectl exec ... env` can see, even
//     though the pod SPEC itself is API-unreadable). Same for the credential
//     half of the agent's own environment (SandboxSpec.SecretEnv), which
//     secretEnvVars must deliver by reference — a pod spec is readable by any
//     principal holding pods/get in the runs namespace.
//
// The boot-time canary's three verdicts (refuse when the CNI does not enforce,
// never advertise NetworkPolicy under the opt-out, refuse an indeterminate
// verdict even with it) are NOT here: canary_test.go owns them one verdict per
// test, and this file's fourth property was a fourth run of those same
// reactors — the exception that made the sentence above untrue.
//
// Property 3's control-plane premise — that dispatch reports its credential
// keys so splitSecretEnv can move them off SandboxSpec.Env — is pinned on the
// other side of the seam by internal/api's TestDispatchEnvSplit_CredentialsLeaveEnv
// and TestDispatchEnvSplit_BedrockCredentialsLeaveEnv. Together the three cover
// the whole leak path; alone, each would pass over a break in the other half.
//
// Built on this package's own helpers (newTestDriver, probeCreate,
// installProxyIPReactor, installAgentRunningReactor, testSandboxSpec), so it
// needs no cluster and no Postgres.
package k8s

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/testfloor"
)

// probeCreate runs a full CreateSandbox on a fresh enforcing fake and returns
// the driver, clientset, spec and sandbox. spec is mutated by mutate before
// creation so a probe can inject adversarial labels/env/secrets.
func probeCreate(t *testing.T, mutate func(*runner.SandboxSpec)) (*Driver, *fake.Clientset, runner.SandboxSpec, runner.Sandbox) {
	t.Helper()
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)
	spec := testSandboxSpec()
	if mutate != nil {
		mutate(&spec)
	}
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	return d, cs, spec, sb
}

func probeGetNetPol(t *testing.T, cs *fake.Clientset, name string) *networkingv1.NetworkPolicy {
	t.Helper()
	np, err := cs.NetworkingV1().NetworkPolicies(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get netpol %q: %v", name, err)
	}
	return np
}

func probeGetPod(t *testing.T, cs *fake.Clientset, name string) *corev1.Pod {
	t.Helper()
	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod %q: %v", name, err)
	}
	return pod
}

func probeSelectorMatches(t *testing.T, what string, sel *metav1.LabelSelector, podLabels map[string]string) bool {
	t.Helper()
	s, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		t.Fatalf("%s: LabelSelectorAsSelector: %v", what, err)
	}
	return s.Matches(labels.Set(podLabels))
}

func probeHasPolicyType(np *networkingv1.NetworkPolicy, pt networkingv1.PolicyType) bool {
	for _, p := range np.Spec.PolicyTypes {
		if p == pt {
			return true
		}
	}
	return false
}

// TestAgentNetPolEgressPeerIsExactlyTheProxy is the structural
// invariant: the agent netpol's egress peer set is ONLY this run's proxy pod
// selector on TCP/3128 — no DNS peer, no 0.0.0.0/0, no namespaceSelector, no
// second rule — and the selector really selects the agent pod even when
// spec.Labels tries to override the reserved keys.
func TestAgentNetPolEgressPeerIsExactlyTheProxy(t *testing.T) {
	// ticket: F9
	testfloor.Mark(t, "k8s")
	_, cs, spec, sb := probeCreate(t, func(s *runner.SandboxSpec) {
		// Adversarial extras: attempt to un-select the agent from its own policy.
		s.Labels[labelComponent] = componentProxy
		s.Labels[labelRun] = "spoofed-run-id"
		s.Labels[labelManaged] = "false"
		s.Labels["team"] = "probe"
	})
	wantProxy := wardynLabels(spec.RunID, componentProxy, nil)
	wantAgent := wardynLabels(spec.RunID, componentAgent, nil)

	np := probeGetNetPol(t, cs, agentNetPolName(spec.RunID))

	if !probeHasPolicyType(np, networkingv1.PolicyTypeIngress) || !probeHasPolicyType(np, networkingv1.PolicyTypeEgress) {
		t.Errorf("agent netpol PolicyTypes = %v, want both Ingress and Egress", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("agent netpol Ingress = %+v, want empty (deny all ingress)", np.Spec.Ingress)
	}
	if !maps.Equal(np.Spec.PodSelector.MatchLabels, wantAgent) || len(np.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("agent netpol PodSelector = %+v, want exactly matchLabels %v", np.Spec.PodSelector, wantAgent)
	}
	if len(np.Spec.Egress) != 1 {
		t.Fatalf("agent netpol Egress has %d rules, want exactly 1: %+v", len(np.Spec.Egress), np.Spec.Egress)
	}
	eg := np.Spec.Egress[0]
	if len(eg.To) != 1 {
		t.Fatalf("agent netpol egress rule has %d peers, want exactly 1: %+v", len(eg.To), eg.To)
	}
	peer := eg.To[0]
	if peer.IPBlock != nil {
		t.Errorf("agent netpol egress peer carries an ipBlock %+v, want NONE (a CIDR peer is not the proxy)", peer.IPBlock)
	}
	if peer.NamespaceSelector != nil {
		t.Errorf("agent netpol egress peer carries a namespaceSelector %+v, want NONE (cross-namespace peers must never be permitted)", peer.NamespaceSelector)
	}
	if peer.PodSelector == nil {
		t.Fatalf("agent netpol egress peer has no podSelector: %+v", peer)
	}
	if !maps.Equal(peer.PodSelector.MatchLabels, wantProxy) || len(peer.PodSelector.MatchExpressions) != 0 {
		t.Errorf("agent netpol egress peer podSelector = %+v, want exactly matchLabels %v (all three reserved keys)", peer.PodSelector, wantProxy)
	}
	if len(eg.Ports) != 1 {
		t.Fatalf("agent netpol egress rule has %d ports, want exactly 1: %+v", len(eg.Ports), eg.Ports)
	}
	p := eg.Ports[0]
	if p.Protocol == nil || *p.Protocol != corev1.ProtocolTCP || p.Port == nil || p.Port.IntVal != runner.ProxyListenPort || p.EndPort != nil {
		t.Errorf("agent netpol egress port = %+v, want exactly TCP/%d with no endPort", p, runner.ProxyListenPort)
	}
	for _, r := range np.Spec.Egress {
		for _, pp := range r.Ports {
			if pp.Port != nil && pp.Port.IntVal == 53 {
				t.Errorf("agent netpol permits port 53 (DNS) — the agent must have NO DNS egress: %+v", r)
			}
		}
		if len(r.To) == 0 {
			t.Errorf("agent netpol egress rule with no To peer permits ALL destinations on its ports: %+v", r)
		}
	}

	// The selector must select the agent pod that was actually created (even
	// with the spoofed extras) and must NOT select the proxy pod; the egress
	// peer selector must select the proxy pod and NOT the agent pod.
	agentPod := probeGetPod(t, cs, sb.Ref)
	proxyPod := probeGetPod(t, cs, proxyPodName(spec.RunID))
	if !probeSelectorMatches(t, "agent netpol podSelector vs agent pod", &np.Spec.PodSelector, agentPod.Labels) {
		t.Errorf("agent netpol podSelector %v does NOT select the agent pod labels %v — the agent would be unconfined", np.Spec.PodSelector.MatchLabels, agentPod.Labels)
	}
	if probeSelectorMatches(t, "agent netpol podSelector vs proxy pod", &np.Spec.PodSelector, proxyPod.Labels) {
		t.Errorf("agent netpol podSelector %v also selects the PROXY pod labels %v", np.Spec.PodSelector.MatchLabels, proxyPod.Labels)
	}
	if !probeSelectorMatches(t, "agent egress peer vs proxy pod", peer.PodSelector, proxyPod.Labels) {
		t.Errorf("agent netpol egress peer %v does NOT select the proxy pod labels %v — the agent would have no egress at all", peer.PodSelector.MatchLabels, proxyPod.Labels)
	}
	if probeSelectorMatches(t, "agent egress peer vs agent pod", peer.PodSelector, agentPod.Labels) {
		t.Errorf("agent netpol egress peer %v selects the AGENT pod labels %v (spoofed extra labels reached the selector)", peer.PodSelector.MatchLabels, agentPod.Labels)
	}
	if agentPod.Labels[labelComponent] != componentAgent || agentPod.Labels[labelRun] != spec.RunID.String() || agentPod.Labels[labelManaged] != "true" {
		t.Errorf("agent pod reserved labels were overridden by spec.Labels: %v", agentPod.Labels)
	}
}

// TestProxyNetPolIngressIsAgentOnlyAndEgressExcludesMetadata pins
// the proxy side: ingress from exactly this run's agent selector (nothing
// else can use the credential-injecting proxy at the packet layer), and every
// egress peer is an ipBlock that excludes the cloud-metadata address.
func TestProxyNetPolIngressIsAgentOnlyAndEgressExcludesMetadata(t *testing.T) {
	// ticket: F9
	testfloor.Mark(t, "k8s")
	_, cs, spec, sb := probeCreate(t, nil)
	wantAgent := wardynLabels(spec.RunID, componentAgent, nil)
	wantProxy := wardynLabels(spec.RunID, componentProxy, nil)

	np := probeGetNetPol(t, cs, proxyNetPolName(spec.RunID))
	if !probeHasPolicyType(np, networkingv1.PolicyTypeIngress) || !probeHasPolicyType(np, networkingv1.PolicyTypeEgress) {
		t.Errorf("proxy netpol PolicyTypes = %v, want both Ingress and Egress", np.Spec.PolicyTypes)
	}
	if !maps.Equal(np.Spec.PodSelector.MatchLabels, wantProxy) || len(np.Spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("proxy netpol PodSelector = %+v, want exactly matchLabels %v", np.Spec.PodSelector, wantProxy)
	}
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("proxy netpol Ingress has %d rules, want exactly 1: %+v", len(np.Spec.Ingress), np.Spec.Ingress)
	}
	in := np.Spec.Ingress[0]
	if len(in.From) != 1 {
		t.Fatalf("proxy netpol ingress rule has %d peers, want exactly 1: %+v", len(in.From), in.From)
	}
	from := in.From[0]
	if from.IPBlock != nil || from.NamespaceSelector != nil || from.PodSelector == nil {
		t.Fatalf("proxy netpol ingress peer must be a bare podSelector (no ipBlock, no namespaceSelector): %+v", from)
	}
	if !maps.Equal(from.PodSelector.MatchLabels, wantAgent) || len(from.PodSelector.MatchExpressions) != 0 {
		t.Errorf("proxy netpol ingress peer podSelector = %+v, want exactly matchLabels %v", from.PodSelector, wantAgent)
	}
	agentPod := probeGetPod(t, cs, sb.Ref)
	if !probeSelectorMatches(t, "proxy ingress peer vs agent pod", from.PodSelector, agentPod.Labels) {
		t.Errorf("proxy netpol ingress peer %v does NOT select the agent pod labels %v", from.PodSelector.MatchLabels, agentPod.Labels)
	}

	if len(np.Spec.Egress) == 0 {
		t.Fatal("proxy netpol has no egress rules (would deny all proxy egress — fail-closed but broken)")
	}
	for i, r := range np.Spec.Egress {
		if len(r.To) == 0 {
			t.Errorf("proxy netpol egress rule %d has no To peer: permits its ports to ALL destinations incl. %s: %+v", i, cloudMetadataAddr, r)
			continue
		}
		for _, peer := range r.To {
			if peer.IPBlock == nil {
				t.Errorf("proxy netpol egress rule %d peer is not an ipBlock (podSelector/namespaceSelector peers carry no metadata carve-out): %+v", i, peer)
				continue
			}
			if peer.IPBlock.CIDR != "0.0.0.0/0" {
				t.Errorf("proxy netpol egress rule %d ipBlock CIDR = %q, want 0.0.0.0/0", i, peer.IPBlock.CIDR)
			}
			found := false
			for _, ex := range peer.IPBlock.Except {
				if ex == cloudMetadataAddr {
					found = true
				}
			}
			if !found {
				t.Errorf("proxy netpol egress rule %d ipBlock %v lacks except %s", i, peer.IPBlock, cloudMetadataAddr)
			}
		}
	}
}

// TestProxySecretsLiveOnlyInTheSecret pins the secret boundary: the run token
// and the MITM CA private key reach the per-run Secret, and NEITHER pod's
// process environment ever carries the config JSON — not as an inline Value,
// and not even by reference via secretKeyRef (T-28, issue #688: a
// secret-backed env var still lands in the container's own process
// environment, where an env-var credential scanner, `kubectl exec ... env`,
// or /proc/<pid>/environ all see it — the pod SPEC being API-unreadable does
// not help once the process is running). The main wardyn-proxy container
// instead reads the config from a FILE, staged there by a nonroot init
// container off a Secret volume that projects only the config key.
func TestProxySecretsLiveOnlyInTheSecret(t *testing.T) {
	// ticket: F9
	testfloor.Mark(t, "k8s")
	const token = "probe-run-token-7f3a9c"
	const caKey = "-----BEGIN EC PRIVATE KEY-----\nPROBE-MITM-KEY-0xdeadbeef\n-----END EC PRIVATE KEY-----"
	_, cs, spec, sb := probeCreate(t, func(s *runner.SandboxSpec) {
		s.ProxyConfig.RunToken = token
		s.ProxyConfig.MITMCAKeyPEM = caKey
		s.ProxyConfig.MITMCACertPEM = "-----BEGIN CERTIFICATE-----\nPROBE-PUBLIC\n-----END CERTIFICATE-----"
	})

	sec, err := cs.CoreV1().Secrets(testNamespace).Get(context.Background(), secretName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get proxy config secret: %v", err)
	}
	data := string(sec.Data[proxyConfigSecretKey])
	if !strings.Contains(data, token) || !strings.Contains(data, "PROBE-MITM-KEY-0xdeadbeef") {
		t.Errorf("proxy config Secret does not carry the run token / MITM key (proxy would fail closed): %q", data)
	}
	if sec.Type != corev1.SecretTypeOpaque {
		t.Errorf("Secret type = %q, want Opaque", sec.Type)
	}
	if sec.Labels[labelRun] != spec.RunID.String() {
		t.Errorf("Secret lacks the run-id label teardown sweeps by: %v", sec.Labels)
	}

	proxyPod := probeGetPod(t, cs, proxyPodName(spec.RunID))
	agentPod := probeGetPod(t, cs, sb.Ref)

	// NO container on the proxy pod — main OR init — carries a
	// WARDYN_PROXY_CONFIG_JSON env at all any more, inline or by reference:
	// the whole point of T-28 is that this variable no longer exists.
	for _, c := range append(append([]corev1.Container{}, proxyPod.Spec.Containers...), proxyPod.Spec.InitContainers...) {
		for _, e := range c.Env {
			if e.Name == "WARDYN_PROXY_CONFIG_JSON" {
				t.Errorf("container %s still carries a WARDYN_PROXY_CONFIG_JSON env (Value=%q ValueFrom=%+v); the config must reach the main container as a file, never an env var", c.Name, e.Value, e.ValueFrom)
			}
		}
	}
	for _, pod := range []*corev1.Pod{proxyPod, agentPod} {
		for _, c := range append(append([]corev1.Container{}, pod.Spec.Containers...), pod.Spec.InitContainers...) {
			for _, e := range c.Env {
				if strings.Contains(e.Value, token) || strings.Contains(e.Value, "PROBE-MITM-KEY") {
					t.Errorf("pod %s container %s env %s carries a run secret inline: %q", pod.Name, c.Name, e.Name, e.Value)
				}
			}
		}
	}

	// The main container reads the config from a FILE (-config <path>),
	// mounted read-only off the shared staged emptyDir — never the raw
	// Secret volume, which the init container alone mounts.
	mainC, ok := findContainer(proxyPod.Spec.Containers, proxyContainerName)
	if !ok {
		t.Fatalf("proxy pod has no %q container", proxyContainerName)
	}
	if got, want := mainC.Args, []string{"-config", proxyConfigStagedPath()}; !slices.Equal(got, want) {
		t.Errorf("main proxy container Args = %v, want %v", got, want)
	}
	mainMount := mountFor(mainC.VolumeMounts, proxyConfigStagedVolumeName)
	if mainMount == nil {
		t.Fatalf("main proxy container has no VolumeMount for %q (the staged config emptyDir)", proxyConfigStagedVolumeName)
	}
	if !mainMount.ReadOnly {
		t.Error("main proxy container's staged-config mount is not ReadOnly")
	}
	for _, vm := range mainC.VolumeMounts {
		if vm.Name == proxyConfigSecretVolumeName {
			t.Errorf("main proxy container mounts %q directly; only the init container may see the raw Secret volume", proxyConfigSecretVolumeName)
		}
	}

	// The init container stages the config: same image, mounts the
	// Secret-projected source read-only and the destination emptyDir
	// writable, and its Args name the exact src/dst StageProxyConfig uses.
	initC, ok := findContainer(proxyPod.Spec.InitContainers, stageProxyConfigInitName)
	if !ok {
		t.Fatalf("proxy pod has no %q init container", stageProxyConfigInitName)
	}
	if initC.Image != mainC.Image {
		t.Errorf("init container image = %q, want the same proxy image as the main container %q", initC.Image, mainC.Image)
	}
	if got, want := initC.Args, []string{"-stage-config-src", proxyConfigSecretPath(), "-stage-config-dst", proxyConfigStagedPath()}; !slices.Equal(got, want) {
		t.Errorf("init container Args = %v, want %v", got, want)
	}
	srcMount := mountFor(initC.VolumeMounts, proxyConfigSecretVolumeName)
	if srcMount == nil {
		t.Fatalf("init container has no VolumeMount for %q", proxyConfigSecretVolumeName)
	}
	if !srcMount.ReadOnly {
		t.Error("init container's Secret-projected source mount is not ReadOnly")
	}
	dstMount := mountFor(initC.VolumeMounts, proxyConfigStagedVolumeName)
	if dstMount == nil {
		t.Fatalf("init container has no VolumeMount for %q", proxyConfigStagedVolumeName)
	}
	if dstMount.ReadOnly {
		t.Error("init container's staged-config destination mount is ReadOnly; it must WRITE the staged file there")
	}

	// The Secret volume projects ONLY the config key, at a non-agent-facing
	// mode — never the agent's SecretEnv or managed-file entries that share
	// the same underlying Secret object.
	secretVol, ok := findVolume(proxyPod.Spec.Volumes, proxyConfigSecretVolumeName)
	if !ok || secretVol.Secret == nil {
		t.Fatalf("proxy pod has no Secret volume %q", proxyConfigSecretVolumeName)
	}
	if secretVol.Secret.SecretName != secretName(spec.RunID) {
		t.Errorf("Secret volume SecretName = %q, want %q", secretVol.Secret.SecretName, secretName(spec.RunID))
	}
	if n := len(secretVol.Secret.Items); n != 1 {
		t.Fatalf("Secret volume projects %d items, want exactly 1 (the config key alone): %+v", n, secretVol.Secret.Items)
	}
	if item := secretVol.Secret.Items[0]; item.Key != proxyConfigSecretKey {
		t.Errorf("Secret volume projects key %q, want %q", item.Key, proxyConfigSecretKey)
	} else if item.Mode == nil || *item.Mode != 0o440 {
		t.Errorf("Secret volume item mode = %v, want 0440", item.Mode)
	}

	// The staged volume is an in-memory emptyDir — no node-disk copy of the
	// config, however briefly.
	stagedVol, ok := findVolume(proxyPod.Spec.Volumes, proxyConfigStagedVolumeName)
	if !ok || stagedVol.EmptyDir == nil {
		t.Fatalf("proxy pod has no emptyDir volume %q", proxyConfigStagedVolumeName)
	}
	if stagedVol.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Errorf("staged config emptyDir Medium = %q, want Memory", stagedVol.EmptyDir.Medium)
	}
	if proxyPod.Spec.AutomountServiceAccountToken == nil || *proxyPod.Spec.AutomountServiceAccountToken {
		t.Error("proxy pod automounts a ServiceAccount token")
	}
	if agentPod.Spec.AutomountServiceAccountToken == nil || *agentPod.Spec.AutomountServiceAccountToken {
		t.Error("agent pod automounts a ServiceAccount token")
	}
}

// findVolume returns the Volume named name, if present.
func findVolume(vols []corev1.Volume, name string) (corev1.Volume, bool) {
	for _, v := range vols {
		if v.Name == name {
			return v, true
		}
	}
	return corev1.Volume{}, false
}

// TestAgentEnvSecretsReachTheAgentOnlyByReference is property 3's agent half: a
// value dispatch resolved for an env_secret grant (resolveEnvSecretGrants, with
// a REAL stored secret) must not land as an inline, API-readable Value on the
// agent pod spec.
//
// The FIXTURE writes SandboxSpec.SecretEnv rather than Env because that is the
// only shape dispatch can now produce — splitSecretEnv moves the credential half
// there before a spec reaches any driver — while the assertion below is
// byte-for-byte the one that caught the original leak.
//
// It is STRICTER than a pure absence check: the variable must also still REACH
// the agent, by reference. A driver that silently dropped it would satisfy the
// no-inline-value loop while breaking every env_secret grant on this substrate.
func TestAgentEnvSecretsReachTheAgentOnlyByReference(t *testing.T) {
	// ticket: F9 H1
	testfloor.Mark(t, "k8s")
	const secretVal = "ghp_probe_env_secret_value_1234567890"
	_, cs, spec, sb := probeCreate(t, func(s *runner.SandboxSpec) {
		// shape of resolveEnvSecretGrants' sandboxEnv[name] = string(val), after
		// splitSecretEnv moves the credential half onto SandboxSpec.SecretEnv.
		s.SecretEnv = map[string]string{"MY_TOKEN": secretVal}
	})
	agentPod := probeGetPod(t, cs, sb.Ref)
	for _, c := range agentPod.Spec.Containers {
		for _, e := range c.Env {
			if e.Value == secretVal {
				t.Errorf("H1 CONFIRMED: agent pod container %s env %s carries the secret value INLINE in the pod spec (API-readable via pods/get); want a secretKeyRef or no inline carrier", c.Name, e.Name)
			}
		}
	}
	// ... and the variable must still REACH the agent, by reference: a driver
	// that silently dropped it would satisfy the loop above while breaking every
	// env_secret grant on this substrate.
	var got *corev1.EnvVar
	for _, e := range agentPod.Spec.Containers[0].Env {
		if e.Name == "MY_TOKEN" {
			got = &e
		}
	}
	if got == nil {
		t.Fatalf("agent pod has no MY_TOKEN env at all: the credential never reached the sandbox: %+v", agentPod.Spec.Containers[0].Env)
	}
	if got.ValueFrom == nil || got.ValueFrom.SecretKeyRef == nil ||
		got.ValueFrom.SecretKeyRef.Name != secretName(spec.RunID) || got.ValueFrom.SecretKeyRef.Key != secretEnvDataKey("MY_TOKEN") {
		t.Errorf("agent pod MY_TOKEN ValueFrom = %+v, want secretKeyRef{%s/%s}", got.ValueFrom, secretName(spec.RunID), secretEnvDataKey("MY_TOKEN"))
	}
	sec, err := cs.CoreV1().Secrets(testNamespace).Get(context.Background(), secretName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get run secret: %v", err)
	}
	if string(sec.Data[secretEnvDataKey("MY_TOKEN")]) != secretVal {
		t.Errorf("run Secret %s = %q, want the credential value", secretEnvDataKey("MY_TOKEN"), sec.Data[secretEnvDataKey("MY_TOKEN")])
	}
	if string(sec.Data[proxyConfigSecretKey]) == "" {
		t.Error("run Secret lost its proxy config: the agent env entries must not displace it")
	}
}
