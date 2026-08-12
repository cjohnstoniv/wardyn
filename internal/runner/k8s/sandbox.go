// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// proxyConfigSecretKey is the Secret data key CreateSandbox writes the proxy
// config JSON under, and secretKeyRef reads it back from.
const proxyConfigSecretKey = "config.json"

// CreateSandbox provisions the run's Secret, NetworkPolicies, proxy pod, and
// agent pod, in that order, fail-closed with full rollback on any error —
// mirrors the docker driver's rollback shape (a LIFO list of teardown
// closures, run on any later failure). One linear, ordered assembly sequence
// (preflight -> secret -> netpols -> proxy pod -> agent pod) whose
// rollback-on-failure compensations must stay in one scope to be verifiably
// complete — that's the funlen nolint below.
//
//nolint:funlen // see the doc comment above: one linear ordered sequence, kept in one scope on purpose
func (d *Driver) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	ns := d.cfg.Namespace

	// (1) Preflight: fail closed BEFORE creating anything.
	if len(spec.Mounts) > 0 {
		return runner.Sandbox{}, fmt.Errorf("k8s: sandbox mounts are not supported (requested %d): %w", len(spec.Mounts), errMountsUnsupported)
	}
	runtimeClassName, err := d.resolveRuntimeClassName(ctx, spec.ConfinementClass)
	if err != nil {
		return runner.Sandbox{}, err
	}
	enforced := spec.ConfinementClass
	if enforced == "" {
		// Class-less floor for direct driver callers only, mirrors docker: every
		// wardynd dispatch path resolves a concrete class before reaching here.
		enforced = types.CC1
	}
	if d.cfg.ProxyImage == "" {
		return runner.Sandbox{}, errProxyImageUnset
	}

	var rollback []func()
	fail := func(err error) (runner.Sandbox, error) {
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
		return runner.Sandbox{}, err
	}

	// (2) Per-run Secret holding the proxy config JSON: pod specs are
	// API-readable, secretKeyRef values are not.
	proxyJSON, err := runner.BuildProxyConfig(spec.RunID, spec.ProxyConfig, runner.ProxyListenPort)
	if err != nil {
		return runner.Sandbox{}, fmt.Errorf("k8s: build proxy config: %w", err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName(spec.RunID),
			Namespace: ns,
			Labels:    wardynLabels(spec.RunID, componentProxy, spec.Labels),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{proxyConfigSecretKey: proxyJSON},
	}
	if _, err := d.clientset.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		return runner.Sandbox{}, fmt.Errorf("k8s: create proxy config secret: %w", err)
	}
	rollback = append(rollback, func() {
		_ = d.clientset.CoreV1().Secrets(ns).Delete(context.Background(), secretName(spec.RunID), metav1.DeleteOptions{})
	})

	// (3) BOTH NetworkPolicies BEFORE any pod exists.
	agentLabels := wardynLabels(spec.RunID, componentAgent, nil)
	proxyLabels := wardynLabels(spec.RunID, componentProxy, nil)

	agentNetPol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: agentNetPolName(spec.RunID), Namespace: ns, Labels: wardynLabels(spec.RunID, componentAgent, spec.Labels)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: agentLabels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{}, // empty => deny all ingress
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: protoPtr(corev1.ProtocolTCP), Port: intOrStrPtr(intstr.FromInt32(runner.ProxyListenPort))}},
				To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: proxyLabels}}},
			}},
		},
	}
	if _, err := d.clientset.NetworkingV1().NetworkPolicies(ns).Create(ctx, agentNetPol, metav1.CreateOptions{}); err != nil {
		return fail(fmt.Errorf("k8s: create agent NetworkPolicy: %w", err))
	}
	rollback = append(rollback, func() {
		_ = d.clientset.NetworkingV1().NetworkPolicies(ns).Delete(context.Background(), agentNetPolName(spec.RunID), metav1.DeleteOptions{})
	})

	proxyNetPol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: proxyNetPolName(spec.RunID), Namespace: ns, Labels: wardynLabels(spec.RunID, componentProxy, spec.Labels)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: proxyLabels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: agentLabels}}},
			}},
			// M4: BOTH rules carry the same metadata-excluding peer. A rule
			// with Ports but no To matches ALL destinations on those ports —
			// a peer-less "DNS" rule would permit port 53 to the metadata
			// address too, voiding the Except on the general rule right
			// next to it.
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{ // DNS
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: protoPtr(corev1.ProtocolTCP), Port: intOrStrPtr(intstr.FromInt32(53))},
						{Protocol: protoPtr(corev1.ProtocolUDP), Port: intOrStrPtr(intstr.FromInt32(53))},
					},
					To: []networkingv1.NetworkPolicyPeer{{IPBlock: notMetadataIPBlock()}},
				},
				{ // everything except the cloud-metadata address
					To: []networkingv1.NetworkPolicyPeer{{IPBlock: notMetadataIPBlock()}},
				},
			},
		},
	}
	if _, err := d.clientset.NetworkingV1().NetworkPolicies(ns).Create(ctx, proxyNetPol, metav1.CreateOptions{}); err != nil {
		return fail(fmt.Errorf("k8s: create proxy NetworkPolicy: %w", err))
	}
	rollback = append(rollback, func() {
		_ = d.clientset.NetworkingV1().NetworkPolicies(ns).Delete(context.Background(), proxyNetPolName(spec.RunID), metav1.DeleteOptions{})
	})

	// (4) Proxy pod, then poll for its CNI-assigned IP.
	proxyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: proxyPodName(spec.RunID), Namespace: ns, Labels: wardynLabels(spec.RunID, componentProxy, spec.Labels)},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: boolPtr(false),
			Containers: []corev1.Container{{
				Name:  proxyContainerName,
				Image: d.cfg.ProxyImage,
				Env: []corev1.EnvVar{
					{Name: "WARDYN_PROXY_CONFIG_JSON", ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: secretName(spec.RunID)},
							Key:                  proxyConfigSecretKey,
						},
					}},
					{Name: "WARDYN_RUN_ID", Value: spec.RunID.String()},
					{Name: "WARDYN_CONTROL_PLANE_URL", Value: spec.ProxyConfig.ControlPlaneURL},
				},
				SecurityContext: restrictedSecurityContext(),
				Resources:       proxyResources(),
			}},
		},
	}
	if d.cfg.ImagePullSecret != "" {
		proxyPod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: d.cfg.ImagePullSecret}}
	}
	if _, err := d.clientset.CoreV1().Pods(ns).Create(ctx, proxyPod, metav1.CreateOptions{}); err != nil {
		return fail(fmt.Errorf("k8s: create proxy pod: %w", err))
	}
	rollback = append(rollback, func() {
		_ = d.clientset.CoreV1().Pods(ns).Delete(context.Background(), proxyPodName(spec.RunID), metav1.DeleteOptions{})
	})

	proxyIP, err := d.waitPodIP(ctx, proxyPodName(spec.RunID))
	if err != nil {
		return fail(fmt.Errorf("k8s: proxy pod never got an IP: %w", err))
	}

	// (5) Agent pod: idle main container, hostAliases pinning "wardyn-proxy"
	// to the resolved IP (the agent's ONLY route to it — mirrors docker's
	// static /etc/hosts entry, needed because the agent NetworkPolicy allows
	// no DNS at all, see the agent netpol above).
	idleCmd := []string{"sh", "-c", runner.AgentIdleScript}
	if spec.Interactive {
		idleCmd = []string{"agent-run", "--idle"}
	}
	if spec.Resources.DiskMiB > 0 {
		slog.Warn("wardynd: k8s substrate: DiskMiB requested but not enforced (no per-container writable-storage quota is wired up on this substrate); running WITHOUT a disk cap",
			slog.Int64("disk_mib", spec.Resources.DiskMiB), slog.String("run_id", spec.RunID.String()))
	}
	if spec.Resources.PidsLimit > 0 {
		slog.Warn("wardynd: k8s substrate: PidsLimit requested but not enforced (Kubernetes has no per-container pids ResourceName; the fork-bomb guard is a node-level kubelet setting, not a per-pod one); running WITHOUT a pids cap",
			slog.Int64("pids_limit", spec.Resources.PidsLimit), slog.String("run_id", spec.RunID.String()))
	}
	agentPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: agentPodName(spec.RunID), Namespace: ns, Labels: wardynLabels(spec.RunID, componentAgent, spec.Labels)},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolPtr(false),
			HostAliases:                  []corev1.HostAlias{{IP: proxyIP, Hostnames: []string{"wardyn-proxy"}}},
			// L3: the default ClusterFirst dnsPolicy points the agent at
			// kube-dns/CoreDNS — which its own NetworkPolicy denies (no DNS
			// egress at all; see the agent netpol above). A proxy-UNAWARE
			// lookup (a tool that ignores HTTP_PROXY) would then hang for
			// the full resolver timeout instead of failing fast. DNSNone
			// with a loopback nameserver (nothing listens there) makes such
			// a lookup fail IMMEDIATELY with connection-refused.
			DNSPolicy: corev1.DNSNone,
			DNSConfig: &corev1.PodDNSConfig{Nameservers: []string{"127.0.0.1"}},
			Containers: []corev1.Container{{
				Name:            mainContainerName,
				Image:           spec.Image,
				Command:         idleCmd,
				Env:             envVars(spec.Env),
				SecurityContext: agentSecurityContext(),
				Resources:       resourceRequirements(spec.Resources),
			}},
		},
	}
	if runtimeClassName != "" {
		agentPod.Spec.RuntimeClassName = &runtimeClassName
	}
	if d.cfg.ImagePullSecret != "" {
		agentPod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: d.cfg.ImagePullSecret}}
	}
	if _, err := d.clientset.CoreV1().Pods(ns).Create(ctx, agentPod, metav1.CreateOptions{}); err != nil {
		return fail(fmt.Errorf("k8s: create agent pod: %w", err))
	}
	rollback = append(rollback, func() {
		_ = d.clientset.CoreV1().Pods(ns).Delete(context.Background(), agentPodName(spec.RunID), metav1.DeleteOptions{})
	})

	// (6) Wait for the main container to actually be Running before handing
	// the sandbox out. A k8s Pod Create is purely declarative (accepted, not
	// yet scheduled/pulled/started) — unlike docker's ContainerStart, which
	// blocks until the container's init process is actually running. Without
	// this wait, a caller racing straight into Attach/ExecStream immediately
	// after CreateSandbox returns (exactly what the conformance suite does)
	// hits "container not found" against a pod still Pending — a REAL gap a
	// live-cluster conformance run surfaced (a fake-clientset unit test
	// can't: nothing simulates the kubelet).
	if err := d.waitContainerRunning(ctx, agentPodName(spec.RunID), mainContainerName); err != nil {
		return fail(fmt.Errorf("k8s: agent pod's main container never started: %w", err))
	}

	return runner.Sandbox{Ref: agentPodName(spec.RunID), Driver: driverName, EnforcedClass: enforced}, nil
}

// waitContainerRunning polls podName until its named container reports
// Running. canaryWaitTimeout (not the tighter podIPWaitTimeout): the agent
// image is whatever the run specifies, not the proxy image the canary (or a
// prior run) has likely already pulled onto this node — a first pull of an
// arbitrary, possibly large agent image needs the same generous budget the
// canary itself gets.
func (d *Driver) waitContainerRunning(ctx context.Context, podName, containerName string) error {
	return wait.PollUntilContextTimeout(ctx, k8sPollInterval, canaryWaitTimeout, true, func(pollCtx context.Context) (bool, error) {
		pod, gerr := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(pollCtx, podName, metav1.GetOptions{})
		if gerr != nil {
			return false, gerr
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != containerName {
				continue
			}
			if cs.State.Running != nil {
				return true, nil
			}
			// A container that crashes before ever reaching Running (a bad
			// image whose entrypoint exits immediately, CrashLoopBackOff's
			// first cycle, ...) must fail fast here — without this check
			// review round 2 (M5) found it falls through to "keep polling"
			// and burns the full canaryWaitTimeout on a container that will
			// never run.
			if t := cs.State.Terminated; t != nil {
				return false, fmt.Errorf("%s container terminated before ever reaching Running (exit code %d): %s", containerName, t.ExitCode, t.Message)
			}
			if w := cs.State.Waiting; w != nil && terminalWaitingReasons[w.Reason] {
				return false, fmt.Errorf("%s container stuck waiting (%s): %s", containerName, w.Reason, w.Message)
			}
			break
		}
		return false, nil
	})
}

// resolveRuntimeClassName is CreateSandbox's fail-closed enforcement
// counterpart to Classes' advertisement: CC1 needs no RuntimeClass override;
// CC2/CC3 REQUIRE an explicit WARDYN_CONFINEMENT_MAP pin (see Config.
// ConfinementRuntimes's doc — a k8s RuntimeClass object name carries no
// platform convention Wardyn could safely guess) resolving to a RuntimeClass
// that exists and clears the class's floor guard. Never silently downgrade.
func (d *Driver) resolveRuntimeClassName(ctx context.Context, class types.ConfinementClass) (string, error) {
	switch class {
	case "", types.CC1:
		return "", nil
	case types.CC2:
		name := d.cfg.ConfinementRuntimes[types.CC2]
		if name == "" {
			return "", fmt.Errorf("the Wall tier (CC2) requires a RuntimeClass pinned via WARDYN_CONFINEMENT_MAP (CC2=<RuntimeClass name>); none is configured: %w", errRuntimeClassUnavailable)
		}
		handler, err := d.runtimeClassHandler(ctx, name)
		if err != nil {
			return "", fmt.Errorf("k8s: CC2 RuntimeClass %q: %w", name, err)
		}
		if handler == "" {
			return "", fmt.Errorf("the Wall tier (CC2) pins RuntimeClass %q, which does not exist on this cluster: %w", name, errRuntimeClassUnavailable)
		}
		if !strings.HasPrefix(handler, handlerRunscPrefix) {
			return "", fmt.Errorf("the Wall tier (CC2) pins RuntimeClass %q (handler %q), which does not deliver gVisor (%s) isolation; refusing to downgrade: %w", name, handler, handlerRunscPrefix, errRuntimeClassUnavailable)
		}
		return name, nil
	case types.CC3:
		name := d.cfg.ConfinementRuntimes[types.CC3]
		if name == "" {
			return "", fmt.Errorf("the Vault tier (CC3) requires a RuntimeClass pinned via WARDYN_CONFINEMENT_MAP (CC3=<RuntimeClass name>); none is configured: %w", errRuntimeClassUnavailable)
		}
		handler, err := d.runtimeClassHandler(ctx, name)
		if err != nil {
			return "", fmt.Errorf("k8s: CC3 RuntimeClass %q: %w", name, err)
		}
		if handler == "" {
			return "", fmt.Errorf("the Vault tier (CC3) pins RuntimeClass %q, which does not exist on this cluster: %w", name, errRuntimeClassUnavailable)
		}
		if runner.IsKnownNonVaultRuntime(handler) {
			return "", fmt.Errorf("the Vault tier (CC3) pins RuntimeClass %q (handler %q), a known shared-kernel/userspace-kernel runtime that does not deliver KVM microVM isolation; refusing to downgrade: %w", name, handler, errRuntimeClassUnavailable)
		}
		return name, nil
	default:
		return "", fmt.Errorf("k8s: unknown confinement class %q: %w", class, errRuntimeClassUnavailable)
	}
}

// waitPodIP polls podName until its CNI-assigned Status.PodIP is set.
func (d *Driver) waitPodIP(ctx context.Context, podName string) (string, error) {
	var ip string
	err := wait.PollUntilContextTimeout(ctx, k8sPollInterval, podIPWaitTimeout, true, func(pollCtx context.Context) (bool, error) {
		pod, gerr := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(pollCtx, podName, metav1.GetOptions{})
		if gerr != nil {
			return false, gerr
		}
		if pod.Status.PodIP != "" {
			ip = pod.Status.PodIP
			return true, nil
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && terminalWaitingReasons[w.Reason] {
				return false, fmt.Errorf("proxy container stuck waiting (%s): %s", w.Reason, w.Message)
			}
		}
		return false, nil
	})
	return ip, err
}

func protoPtr(p corev1.Protocol) *corev1.Protocol          { return &p }
func intOrStrPtr(v intstr.IntOrString) *intstr.IntOrString { return &v }

// cloudMetadataAddr is the link-local address every major cloud provider
// serves its instance-metadata API on (AWS/GCP/Azure all use it) — the
// proxy netpol's egress carves it out so a compromised proxy cannot reach
// node/instance credentials.
const cloudMetadataAddr = "169.254.169.254/32"

// notMetadataIPBlock is 0.0.0.0/0 except the cloud-metadata address, shared
// by the proxy netpol's DNS and general egress rules so they can never
// drift apart (M4 finding: a rule missing this peer permits its ports to
// the metadata address too, voiding the other rule's Except). IPv4-only —
// this substrate does not yet reason about IPv6 pod networks.
func notMetadataIPBlock() *networkingv1.IPBlock {
	return &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{cloudMetadataAddr}}
}

// isNotFound reports whether err is a k8s "not found" API error — used
// throughout teardown so Stop/Kill stay idempotent on an already-gone
// sandbox, mirroring docker's isNotFound.
func isNotFound(err error) bool { return err != nil && apierrors.IsNotFound(err) }
