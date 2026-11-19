// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"github.com/google/uuid"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// proxyPodInputs is everything the proxy pod is built from. CreateSandbox fills it from the run's spec; a proxy
// replaced on a kept run fills it from the stored proxy config, since no SandboxSpec exists by then.
type proxyPodInputs struct {
	RunID           uuid.UUID
	Namespace       string
	SecretName      string // the Secret whose proxyConfigSecretKey the init container stages
	Deadline        *int64 // the pod's ActiveDeadlineSeconds
	ControlPlaneURL string
	Azure           bool              // the run has Azure gates, which size the proxy larger
	Labels          map[string]string // the run's extra labels (SandboxSpec.Labels)
	CPPin           *corev1.HostAlias // a No network run's one address on a server; nil otherwise
}

// buildProxyPod is the run's proxy pod, not yet created.
func (d *Driver) buildProxyPod(in proxyPodInputs) (*corev1.Pod, error) {
	proxyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: proxyPodName(in.RunID), Namespace: in.Namespace},
		Spec: corev1.PodSpec{
			ActiveDeadlineSeconds:        in.Deadline,
			AutomountServiceAccountToken: boolPtr(false),
			// FSGroup makes proxyConfigSecretFileMode's group-read bit effective
			// for the init container; without it, "permission denied".
			SecurityContext: &corev1.PodSecurityContext{FSGroup: &proxyNonrootGID, Sysctls: proxyDNSSysctls()},
			// SECURITY: default true injects a <SVC>_SERVICE_HOST/PORT pair for
			// every namespace Service into every container — a free
			// service-topology enumeration nothing here needs.
			EnableServiceLinks: boolPtr(false),
			// InitContainers stages the proxy config from the Secret into an
			// in-memory emptyDir as owner-only 0400 (see proxyConfigSecretVolumeName's
			// doc in sandbox.go).
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
				// read-only staged-file mount (see the SECURITY note on proxyConfigSecretVolumeName in sandbox.go).
				Args: []string{"-config", proxyConfigStagedPath()},
				Env: []corev1.EnvVar{
					{Name: "WARDYN_RUN_ID", Value: in.RunID.String()},
					{Name: "WARDYN_CONTROL_PLANE_URL", Value: in.ControlPlaneURL},
					{Name: proxy.DNSListenEnv, Value: ":53"},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: proxyConfigStagedVolumeName, MountPath: proxyConfigStagedMountDir, ReadOnly: true},
				},
				SecurityContext:          restrictedSecurityContext(),
				Resources:                proxyResources(in.Azure),
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
				// The proxy binds its listener only once it serves: after its startup resolve has
				// succeeded and, on a runner (self_lapse), after its first renew and its decision sink's
				// first grant. A connect to it is that proof: without a probe the container is Ready as
				// soon as it runs, and waitPodIP can return before a failing resolve exits it, or start an
				// agent whose requests the proxy still refuses, a short run's recording upload among them.
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(runner.ProxyListenPort)}},
					PeriodSeconds: 1,
				},
			}},
			Volumes: []corev1.Volume{
				{
					Name: proxyConfigSecretVolumeName,
					VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						SecretName: in.SecretName,
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
	if !d.runnerMode() {
		withLiveDenyDir(proxyPod)
	}
	if d.cfg.ImagePullSecret != "" {
		proxyPod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: d.cfg.ImagePullSecret}}
	}
	d.placement.apply(proxyPod, in.RunID, componentProxy, in.Labels)
	if err := d.runnerPodSpec(proxyPod, componentProxy, in.ControlPlaneURL); err != nil {
		return nil, err
	}
	if in.CPPin != nil { // a No network run on a server: Wardyn's one address, with no DNS rule to find it
		proxyPod.Spec.HostAliases = append(proxyPod.Spec.HostAliases, *in.CPPin)
	}
	return proxyPod, nil
}
