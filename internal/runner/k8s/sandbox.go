// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// podDeadlineGrace is added to Config.RunMaxAge to make a run pod's
// activeDeadlineSeconds. The control plane's own max-age stop fires first and
// tears the run down; the deadline is the backstop for a run whose control
// plane is gone, so the grace only has to outlast the reaper's scan interval.
const podDeadlineGrace = 10 * time.Minute

// activeDeadline returns the pod deadline for Config.RunMaxAge, or nil when no
// max age is set. activeDeadlineSeconds fails a pod and deletes nothing: the
// reconciler finalizes the run from the failed pod and tears the rest down.
func (d *Driver) activeDeadline() *int64 {
	if d.cfg.RunMaxAge <= 0 {
		return nil
	}
	secs := int64((d.cfg.RunMaxAge + podDeadlineGrace) / time.Second)
	return &secs
}

// proxyConfigSecretKey is the Secret data key CreateSandbox writes the proxy
// config JSON under.
const proxyConfigSecretKey = "config.json"

// SECURITY: the proxy config reaches the main wardyn-proxy container as a
// FILE, never a secret-backed env var — an env var is readable via `kubectl
// exec ... env`, /proc/<pid>/environ, or a core dump. Two volumes + a nonroot
// init container close the gap: the Secret volume projects only
// proxyConfigSecretKey into the init container, which stages it into an
// in-memory emptyDir at owner-only 0400; the main container mounts only that
// staged emptyDir, read-only, via `-config <path>`, with no Env entry for it.
const (
	proxyConfigSecretVolumeName = "wardyn-proxy-config-secret"
	proxyConfigSecretMountDir   = "/var/run/wardyn-proxy-secret"
	proxyConfigStagedVolumeName = "wardyn-proxy-config-staged"
	proxyConfigStagedMountDir   = "/var/run/wardyn-proxy"
	proxyConfigFileName         = "config.json"
	stageProxyConfigInitName    = "stage-proxy-config"
)

// proxyConfigSecretFileMode: 0440 on the Secret volume projection. A
// Secret-projected file is always root:root-owned regardless of Mode, so the
// group-read bit is what lets the init container's nonroot uid (65532) read
// it (requires proxyNonrootGID below); confirmed on a real cluster that
// without it the init container fails closed with "permission denied".
var proxyConfigSecretFileMode = int32(0o440)

// proxyNonrootGID is the proxy pod's FSGroup, chowned onto the Secret-projected
// file's group so proxyConfigSecretFileMode's group-read bit works for the
// init container. Does NOT widen access to the STAGED file, which the init
// container creates itself at owner-only 0400.
var proxyNonrootGID = int64(65532)

// proxyConfigSecretPath and proxyConfigStagedPath are the config file's full
// path on each side of the staging copy.
func proxyConfigSecretPath() string { return proxyConfigSecretMountDir + "/" + proxyConfigFileName }
func proxyConfigStagedPath() string { return proxyConfigStagedMountDir + "/" + proxyConfigFileName }

// CreateSandbox provisions the run's Secret, NetworkPolicies, proxy pod, and
// agent pod, in that order, fail-closed with full rollback on any error
// (mirrors the docker driver's LIFO teardown-closure shape). Kept as one
// linear ordered sequence in one scope so the rollback compensations stay
// verifiably complete — hence the funlen nolint below.
//
//nolint:funlen // one linear ordered sequence, kept in one scope on purpose
func (d *Driver) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	ns := d.cfg.Namespace

	// (1) Preflight: fail closed BEFORE creating anything.
	if len(spec.Mounts) > 0 {
		return runner.Sandbox{}, fmt.Errorf("k8s: sandbox mounts are not supported (requested %d): %w", len(spec.Mounts), errMountsUnsupported)
	}
	if err := runner.ValidateManagedFiles(spec.ManagedFiles); err != nil {
		return runner.Sandbox{}, fmt.Errorf("k8s: %w", err)
	}
	runtimeClassName, runtimeHandler, err := d.resolveRuntimeClassName(ctx, spec.ConfinementClass)
	if err != nil {
		return runner.Sandbox{}, err
	}
	enforced := spec.ConfinementClass
	if enforced == "" {
		// Class-less floor for direct driver callers only (mirrors docker).
		enforced = types.CC1
	}
	if d.cfg.ProxyImage == "" {
		return runner.Sandbox{}, errProxyImageUnset
	}
	// The user drive comes LAST in preflight since it's the only step that
	// writes; its PVC outlives the run on purpose and carries no
	// wardyn.run-id label, so rollback can never delete the person's storage.
	if spec.Drive != nil {
		if err := ensureDrivePVC(ctx, d.clientset, ns, spec.Drive); err != nil {
			return runner.Sandbox{}, err
		}
	}

	// SECURITY: fail tears down via teardownByRunID, the same
	// wait-before-netpol-drop guard Stop/KillSandbox use — a bare LIFO delete
	// list would drop NetworkPolicies as soon as Delete is issued, and a pod
	// mid-Terminating is unselected by any policy (default-allow), reopening
	// unconfined egress on a pod still holding this run's MITM CA key and
	// RunToken. Zero grace period: nothing has been handed to a caller yet.
	fail := func(err error) (runner.Sandbox, error) {
		zero := int64(0)
		if terr := d.teardownByRunID(context.Background(), spec.RunID, &zero); terr != nil {
			slog.Error("wardynd: k8s substrate: CreateSandbox rollback failed to fully tear down a partially-created sandbox",
				slog.String("run_id", spec.RunID.String()), slog.String("create_err", err.Error()), slog.String("teardown_err", terr.Error()))
		}
		return runner.Sandbox{}, err
	}

	// (2) BOTH NetworkPolicies, before any pod AND before the Secret.
	//
	// SECURITY/RBAC: these two carry no credential, so the orphan sweep may
	// list them (the Role withholds every Secret-body read verb, which RBAC
	// can't scope by label). Created first and deleted last at teardown, so
	// any surviving Secret always has both NetworkPolicies to be found by.
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

	proxyNetPol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: proxyNetPolName(spec.RunID), Namespace: ns, Labels: wardynLabels(spec.RunID, componentProxy, spec.Labels)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: proxyLabels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: agentLabels}}},
			}},
			// SECURITY: both rules share the metadata-excluding peer — a
			// peer-less DNS rule would permit port 53 to the metadata address,
			// voiding the Except on the rule beside it.
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

	// (3) Per-run Secret holding the proxy config JSON: pod specs are
	// API-readable, secretKeyRef values aren't.
	proxyJSON, err := runner.BuildProxyConfig(spec.RunID, spec.ProxyConfig, runner.ProxyListenPort)
	if err != nil {
		return fail(fmt.Errorf("k8s: build proxy config: %w", err))
	}
	secretData := map[string][]byte{proxyConfigSecretKey: proxyJSON}
	// The agent's credential-bearing env rides this SAME Secret (see
	// runner.SandboxSpec.SecretEnv), via a secretKeyRef per variable rather
	// than an inline EnvVar.Value. One Secret, not two, is one less object
	// for rollback to get right.
	for k, v := range spec.SecretEnv {
		secretData[secretEnvDataKey(k)] = []byte(v)
	}
	// MANAGED FILES ride it too: not because the content is secret, but
	// because a Secret volume is the only projection here that lands a file
	// root-owned and read-only in a directory the agent can't replace.
	for k, v := range managedFileSecretData(spec.ManagedFiles) {
		secretData[k] = v
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName(spec.RunID),
			Namespace: ns,
			Labels:    wardynLabels(spec.RunID, componentProxy, spec.Labels),
		},
		Type: corev1.SecretTypeOpaque,
		Data: secretData,
	}
	if _, err := d.clientset.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		return fail(fmt.Errorf("k8s: create proxy config secret: %w", err))
	}

	// (4) Proxy pod, then poll until it is ready and has its CNI-assigned IP.
	proxyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: proxyPodName(spec.RunID), Namespace: ns},
		Spec: corev1.PodSpec{
			ActiveDeadlineSeconds:        d.activeDeadline(),
			AutomountServiceAccountToken: boolPtr(false),
			// FSGroup makes proxyConfigSecretFileMode's group-read bit effective
			// for the init container; without it, "permission denied".
			SecurityContext: &corev1.PodSecurityContext{FSGroup: &proxyNonrootGID},
			// SECURITY: default true injects a <SVC>_SERVICE_HOST/PORT pair for
			// every namespace Service into every container — a free
			// service-topology enumeration nothing here needs.
			EnableServiceLinks: boolPtr(false),
			// InitContainers stages the proxy config from the Secret into an
			// in-memory emptyDir as owner-only 0400 (see proxyConfigSecretVolumeName
			// doc above).
			InitContainers: []corev1.Container{{
				Name:  stageProxyConfigInitName,
				Image: d.cfg.ProxyImage,
				Args:  []string{"-stage-config-src", proxyConfigSecretPath(), "-stage-config-dst", proxyConfigStagedPath()},
				VolumeMounts: []corev1.VolumeMount{
					{Name: proxyConfigSecretVolumeName, MountPath: proxyConfigSecretMountDir, ReadOnly: true},
					{Name: proxyConfigStagedVolumeName, MountPath: proxyConfigStagedMountDir},
				},
				SecurityContext: restrictedSecurityContext(),
				Resources:       proxyResources(false),
				// The binary logs its refusal to stderr, not /dev/termination-log;
				// without this proxyStartFailure's error names no cause.
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			}},
			Containers: []corev1.Container{{
				Name:  proxyContainerName,
				Image: d.cfg.ProxyImage,
				// -config, not an env var: reaches this container only via the
				// read-only staged-file mount (see SECURITY note above).
				Args: []string{"-config", proxyConfigStagedPath()},
				Env: []corev1.EnvVar{
					{Name: "WARDYN_RUN_ID", Value: spec.RunID.String()},
					{Name: "WARDYN_CONTROL_PLANE_URL", Value: spec.ProxyConfig.ControlPlaneURL},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: proxyConfigStagedVolumeName, MountPath: proxyConfigStagedMountDir, ReadOnly: true},
				},
				SecurityContext:          restrictedSecurityContext(),
				Resources:                proxyResources(len(spec.ProxyConfig.AzureGates) > 0),
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			}},
			Volumes: []corev1.Volume{
				{
					Name: proxyConfigSecretVolumeName,
					VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						SecretName: secretName(spec.RunID),
						Items: []corev1.KeyToPath{{
							Key:  proxyConfigSecretKey,
							Path: proxyConfigFileName,
							Mode: &proxyConfigSecretFileMode,
						}},
					}},
				},
				{
					Name:         proxyConfigStagedVolumeName,
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}},
				},
			},
		},
	}
	// Operator knobs the sidecar reads from its OWN environment: a pod
	// inherits nothing from wardynd, so without this an operator setting is
	// unreachable rather than "off". Same list as the docker driver's.
	for _, kv := range runner.ProxySidecarEnvKnobs() {
		proxyPod.Spec.Containers[0].Env = append(proxyPod.Spec.Containers[0].Env,
			corev1.EnvVar{Name: kv[0], Value: kv[1]})
	}
	if d.cfg.ImagePullSecret != "" {
		proxyPod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: d.cfg.ImagePullSecret}}
	}
	d.placement.apply(proxyPod, spec.RunID, componentProxy, spec.Labels)
	createdProxy, err := d.clientset.CoreV1().Pods(ns).Create(ctx, proxyPod, metav1.CreateOptions{})
	if err != nil {
		return fail(fmt.Errorf("k8s: create proxy pod: %w", err))
	}

	clock := d.newStartClock()
	proxyIP, err := d.waitPodIP(ctx, clock, proxyPodName(spec.RunID), spec.NotifyWaiting)
	if err != nil {
		return fail(fmt.Errorf("k8s: proxy pod never became ready: %w", err))
	}

	// (5) Agent pod: idle main container, hostAliases pinning "wardyn-proxy" to
	// the resolved IP (its only route there, since the agent NetworkPolicy
	// allows no DNS at all).
	idleCmd := []string{"sh", "-c", runner.AgentIdleScript}
	if spec.Interactive {
		idleCmd = []string{"agent-run", "--idle"}
	}
	if spec.Resources.PidsLimit > 0 {
		slog.Warn("wardynd: k8s substrate: PidsLimit requested but not enforced (Kubernetes has no per-container pids ResourceName; the fork-bomb guard is a node-level kubelet setting, not a per-pod one); running WITHOUT a pids cap",
			slog.Int64("pids_limit", spec.Resources.PidsLimit), slog.String("run_id", spec.RunID.String()))
	}
	agentPod := &corev1.Pod{
		// The agent is owned by the proxy pod, so deleting the proxy by any route (Destroy, the
		// sweep, kubectl, a node drain) garbage-collects the agent with it. The reference needs the
		// proxy's UID, which exists now; the reverse (proxy owned by agent) cannot be set at create.
		// No BlockOwnerDeletion: that would need an extra permission on pods/finalizers.
		ObjectMeta: metav1.ObjectMeta{Name: agentPodName(spec.RunID), Namespace: ns, OwnerReferences: ownedByPod(createdProxy)},
		Spec: corev1.PodSpec{
			ActiveDeadlineSeconds:        d.activeDeadline(),
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolPtr(false),
			EnableServiceLinks:           boolPtr(false), // SECURITY: same service-topology-leak reason as the proxy pod above.
			HostAliases:                  []corev1.HostAlias{{IP: proxyIP, Hostnames: []string{"wardyn-proxy"}}},
			// Default ClusterFirst would point the agent at kube-dns, which its
			// NetworkPolicy denies; DNSNone with a loopback nameserver fails a
			// proxy-unaware lookup immediately instead of hanging the full
			// resolver timeout.
			DNSPolicy: corev1.DNSNone,
			DNSConfig: &corev1.PodDNSConfig{Nameservers: []string{"127.0.0.1"}},
			Containers: []corev1.Container{{
				Name:    mainContainerName,
				Image:   spec.Image,
				Command: idleCmd,
				// SECURITY: credential-bearing env is a secretKeyRef into the run
				// Secret, never inlined; Exec copies this whole slice onto the
				// ephemeral container it adds.
				Env:             append(envVars(spec.Env), secretEnvVars(spec.RunID, spec.SecretEnv)...),
				SecurityContext: agentSecurityContext(),
				Resources:       resourceRequirements(spec.Resources),
			}},
		},
	}
	// Scratch volumes, only when the run carries a disk budget: puts the
	// agent's own writes inside disk_mib.
	scratchVols, scratchMounts := ephemeralScratchVolumes(spec.Resources.DiskMiB)
	addMainContainerVolumes(agentPod, scratchVols, scratchMounts)
	// Managed files, read-only off the Secret, in the pod's filesystem before
	// any container starts, so there's no window where the agent runs without them.
	managedVols, managedMounts := managedFileVolumes(spec.RunID, spec.ManagedFiles)
	addMainContainerVolumes(agentPod, managedVols, managedMounts)
	// The drive, only when the pod has one, so a drive-less pod keeps its nil
	// pod-level SecurityContext unchanged.
	if spec.Drive != nil {
		applyDriveToPod(agentPod, spec.Drive, runtimeHandler)
	}
	if runtimeClassName != "" {
		agentPod.Spec.RuntimeClassName = &runtimeClassName
	}
	if d.cfg.ImagePullSecret != "" {
		agentPod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: d.cfg.ImagePullSecret}}
	}
	d.placement.apply(agentPod, spec.RunID, componentAgent, spec.Labels)
	if _, err := d.clientset.CoreV1().Pods(ns).Create(ctx, agentPod, metav1.CreateOptions{}); err != nil {
		return fail(fmt.Errorf("k8s: create agent pod: %w", err))
	}

	// (6) Wait for the main container to actually be Running before handing
	// the sandbox out: a k8s Pod Create is purely declarative, unlike
	// docker's ContainerStart, so a caller racing straight into
	// Attach/ExecStream would hit "container not found" against a pod still Pending.
	if err := d.waitContainerRunning(ctx, clock, agentPodName(spec.RunID), mainContainerName, spec.NotifyWaiting); err != nil {
		return fail(fmt.Errorf("k8s: agent pod's main container never started: %w", err))
	}

	if spec.ExecOutput != nil {
		d.execOutputs.Store(agentPodName(spec.RunID), spec.ExecOutput)
	}
	return runner.Sandbox{Ref: agentPodName(spec.RunID), Driver: driverName, EnforcedClass: enforced}, nil
}

// ownedByPod is the ownerReference list that makes a pod owned by owner, or nil when the API
// returned no UID (a real API server always does; a UID-less reference would be refused).
func ownedByPod(owner *corev1.Pod) []metav1.OwnerReference {
	if owner == nil || owner.UID == "" {
		return nil
	}
	return []metav1.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: owner.Name, UID: owner.UID}}
}

// addMainContainerVolumes attaches vols to pod and mounts them on the MAIN
// container only (not an omission: exec.go copies the main container's
// mounts onto the ephemeral container the agent actually runs in).
func addMainContainerVolumes(pod *corev1.Pod, vols []corev1.Volume, mounts []corev1.VolumeMount) {
	if len(vols) == 0 {
		return
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, vols...)
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == mainContainerName {
			pod.Spec.Containers[i].VolumeMounts = append(pod.Spec.Containers[i].VolumeMounts, mounts...)
		}
	}
}

// waitContainerRunning polls podName until its named container reports
// Running, bounded by clock: the sandbox's one absolute start deadline, which the
// proxy's wait has already been spending (see startClock).
//
// On timeout it reports WHY via the captured lastPod, since a re-fetch on a
// dead context returns nothing and a pod stuck Pending has no container
// status otherwise (e.g. the scheduler's "unbound immediate PersistentVolumeClaims").
//
// onWaiting (nil-safe) gets that reason while waiting, once per change — see
// runner.SandboxSpec.OnWaiting.
func (d *Driver) waitContainerRunning(ctx context.Context, clock *startClock, podName, containerName string, onWaiting func(string)) error {
	var lastPod *corev1.Pod
	// Last reason REPORTED, so the report fires on change, not on a tick.
	var lastReason string
	var pulls pullWatch
	err := clock.poll(ctx, func(pollCtx context.Context) (bool, bool, error) {
		pod, gerr := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(pollCtx, podName, metav1.GetOptions{})
		if isClientThrottled(gerr) {
			return false, false, nil
		}
		if gerr != nil {
			return false, false, gerr
		}
		lastPod = pod
		room := clock.observe(pod)
		reason := waitingReason(pod)
		if pulling := d.pullingDetail(pollCtx, pod, containerName, &pulls); pulling != "" {
			reason = pulling
		}
		if reason != lastReason {
			lastReason = reason
			if onWaiting != nil {
				onWaiting(reason)
			}
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != containerName {
				continue
			}
			if cs.State.Running != nil {
				return true, room, nil
			}
			// A container that crashes before ever reaching Running must fail
			// fast here, or it burns the full timeout polling.
			if t := cs.State.Terminated; t != nil {
				return false, room, fmt.Errorf("%s container terminated before ever reaching Running (exit code %d): %s", containerName, t.ExitCode, t.Message)
			}
			if w := cs.State.Waiting; w != nil && terminalWaitingReasons[w.Reason] {
				return false, room, fmt.Errorf("%s container stuck waiting (%s): %s", containerName, w.Reason, w.Message)
			}
			break
		}
		return false, room, nil
	})
	// Only a TIMEOUT is enriched: the poll's own errors already name their
	// cause, and a Get failure is about the apiserver, not the pod.
	if errors.Is(err, context.DeadlineExceeded) {
		if why := podStuckReason(lastPod); why != "" {
			return fmt.Errorf("%w (%s)", err, why)
		}
	}
	return err
}

// isClientThrottled reports client-go's own rate-limiter refusal, which does
// not wrap context.DeadlineExceeded; treated as "not yet" so the poll ends on
// its own deadline and enrichment still runs. String match on client-go's
// wrapper text; degrades to pass-through if upstream renames it.
func isClientThrottled(err error) bool {
	return err != nil && strings.Contains(err.Error(), "client rate limiter Wait returned an error")
}

// podStuckReason renders why a pod never started, from the last status the
// poll saw; "" when it looks fine or was never observed, so the caller falls
// back to the bare timeout rather than a fabricated cause. PodScheduled=False
// is checked first: it's where the scheduler writes storage facts like
// "unbound immediate PersistentVolumeClaims". The cross-node ReadWriteOnce
// case is NOT covered here: that pod is scheduled and stalls in
// ContainerCreating on a FailedAttachVolume event this function never reads —
// see docs/OPERATIONS.md ("User drives on Kubernetes").
func podStuckReason(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return strings.TrimSpace(fmt.Sprintf("pod is unscheduled — %s: %s", c.Reason, c.Message))
		}
	}
	if pod.Status.Phase == corev1.PodPending {
		return "pod is still Pending with no container status"
	}
	return ""
}

// resolveRuntimeClassName is CreateSandbox's fail-closed enforcement
// counterpart to Classes' advertisement: CC1 needs no RuntimeClass override;
// CC2/CC3 REQUIRE an explicit WARDYN_CONFINEMENT_MAP pin resolving to a class
// that exists and clears its floor guard. SECURITY: never silently downgrade.
//
// Returns .Handler alongside the object NAME: applyDriveToPod needs the
// runtime FAMILY (handlerRunscPrefix) to decide gVisor's directfs annotation,
// so resolving it here avoids a second RuntimeClasses Get. CC1 gets an empty
// handler.
func (d *Driver) resolveRuntimeClassName(ctx context.Context, class types.ConfinementClass) (name, handler string, err error) {
	switch class {
	case "", types.CC1:
		return "", "", nil
	case types.CC2:
		name := d.cfg.ConfinementRuntimes[types.CC2]
		if name == "" {
			return "", "", fmt.Errorf("the Wall tier (CC2) requires a RuntimeClass pinned via WARDYN_CONFINEMENT_MAP (CC2=<RuntimeClass name>); none is configured: %w", errRuntimeClassUnavailable)
		}
		handler, err := d.runtimeClassHandler(ctx, name)
		if err != nil {
			return "", "", fmt.Errorf("k8s: CC2 RuntimeClass %q: %w", name, err)
		}
		if handler == "" {
			return "", "", fmt.Errorf("the Wall tier (CC2) pins RuntimeClass %q, which does not exist on this cluster: %w", name, errRuntimeClassUnavailable)
		}
		if !strings.HasPrefix(handler, handlerRunscPrefix) {
			return "", "", fmt.Errorf("the Wall tier (CC2) pins RuntimeClass %q (handler %q), which does not deliver gVisor (%s) isolation; refusing to downgrade: %w", name, handler, handlerRunscPrefix, errRuntimeClassUnavailable)
		}
		return name, handler, nil
	case types.CC3:
		name := d.cfg.ConfinementRuntimes[types.CC3]
		if name == "" {
			return "", "", fmt.Errorf("the Vault tier (CC3) requires a RuntimeClass pinned via WARDYN_CONFINEMENT_MAP (CC3=<RuntimeClass name>); none is configured: %w", errRuntimeClassUnavailable)
		}
		handler, err := d.runtimeClassHandler(ctx, name)
		if err != nil {
			return "", "", fmt.Errorf("k8s: CC3 RuntimeClass %q: %w", name, err)
		}
		if handler == "" {
			return "", "", fmt.Errorf("the Vault tier (CC3) pins RuntimeClass %q, which does not exist on this cluster: %w", name, errRuntimeClassUnavailable)
		}
		if runner.IsKnownNonVaultRuntime(handler) {
			return "", "", fmt.Errorf("the Vault tier (CC3) pins RuntimeClass %q (handler %q), a known shared-kernel/userspace-kernel runtime that does not deliver KVM microVM isolation; refusing to downgrade: %w", name, handler, errRuntimeClassUnavailable)
		}
		return name, handler, nil
	default:
		return "", "", fmt.Errorf("k8s: unknown confinement class %q: %w", class, errRuntimeClassUnavailable)
	}
}

// waitPodIP polls the proxy pod until it is READY — its init container done
// and its main container Ready — and returns its CNI-assigned PodIP. An IP
// alone proves nothing: the CNI assigns it before any image pull, so a proxy
// stuck in ImagePullBackOff or still staging its config already has one.
// Reports why via onWaiting (nil-safe, once per change). It's the FIRST wait
// CreateSandbox blocks on, so on a cluster with nowhere to schedule it's the
// one a person sits through.
//
// One bound, clock's: absolute across the proxy and the agent, so an
// unplaceable proxy waits for room under the same deadlines the agent does
// rather than failing on a deadline of its own.
func (d *Driver) waitPodIP(ctx context.Context, clock *startClock, podName string, onWaiting func(string)) (string, error) {
	var ip string
	var lastPod *corev1.Pod
	var lastReason string
	err := clock.poll(ctx, func(pollCtx context.Context) (bool, bool, error) {
		pod, gerr := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(pollCtx, podName, metav1.GetOptions{})
		if gerr != nil {
			return false, false, gerr
		}
		lastPod = pod
		room := clock.observe(pod)
		if reason := waitingReason(pod); reason != lastReason {
			lastReason = reason
			if onWaiting != nil {
				onWaiting(reason)
			}
		}
		if err := proxyStartFailure(pod); err != nil {
			return false, room, err
		}
		if pod.Status.PodIP == "" {
			return false, room, nil
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == proxyContainerName && cs.Ready {
				ip = pod.Status.PodIP
				return true, room, nil
			}
		}
		return false, room, nil
	})
	if errors.Is(err, context.DeadlineExceeded) && lastPod != nil {
		// A container's own Waiting reason first: podStuckReason reads only the
		// pod, and would call an init-blocked pod one "with no container status".
		if why := cmp.Or(waitingDetail(lastPod), podStuckReason(lastPod)); why != "" {
			return "", fmt.Errorf("%w (%s)", err, why)
		}
	}
	return ip, err
}

// proxyStartFailure is non-nil once the proxy pod is in a state waiting cannot
// fix: a terminal Waiting reason on its init or main container, a failed init,
// or a main container that exited. The "proxy container stuck waiting (" shape
// is what the control plane's stuckStartupFromFailureHint reads back.
func proxyStartFailure(pod *corev1.Pod) error {
	for _, cs := range slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses) {
		if w := cs.State.Waiting; w != nil && terminalWaitingReasons[w.Reason] {
			return fmt.Errorf("proxy container stuck waiting (%s): %s: %s", w.Reason, cs.Name, w.Message)
		}
		t := cs.State.Terminated
		if t == nil || (cs.Name == stageProxyConfigInitName && t.ExitCode == 0) {
			continue
		}
		return fmt.Errorf("proxy container %s exited before the proxy was ready (exit code %d, %s): %s", cs.Name, t.ExitCode, t.Reason, t.Message)
	}
	return nil
}

func protoPtr(p corev1.Protocol) *corev1.Protocol          { return &p }
func intOrStrPtr(v intstr.IntOrString) *intstr.IntOrString { return &v }

// cloudMetadataAddr is the link-local instance-metadata address on every
// major cloud. SECURITY: the proxy netpol's egress carves it out so a
// compromised proxy can't reach node/instance credentials.
const cloudMetadataAddr = "169.254.169.254/32"

// notMetadataIPBlock is 0.0.0.0/0 except the cloud-metadata address, shared
// by the proxy netpol's DNS and general egress rules so they can't drift
// apart. IPv4-only.
func notMetadataIPBlock() *networkingv1.IPBlock {
	return &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{cloudMetadataAddr}}
}

// isNotFound reports whether err is a k8s "not found" API error, so
// Stop/Kill stay idempotent on an already-gone sandbox.
func isNotFound(err error) bool { return err != nil && apierrors.IsNotFound(err) }
