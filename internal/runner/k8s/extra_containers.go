// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

// Extra containers (runner.SandboxSpec.Containers) run in the agent pod beside the agent: the pod's
// NetworkPolicy, DNS, hostAliases, RuntimeClass and token settings already hold every container in it.
// What is per container is set here, and nothing else: the agent's restricted security context, the
// proxy's variables, the work folder and the run's drives, and a share of the run's size.

import (
	"errors"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const (
	agentFloorCPUMillis int64 = 250
	agentFloorMemoryMiB int64 = 256
)

var errExtraContainersBudget = errors.New("the run's extra containers do not fit its size")

// extraContainerEnvKeys are the only variables of the run's env an extra container gets. Never a
// secretKeyRef, and never the CA bundle paths: they name files only agent-run writes, in the agent's
// own filesystem.
var extraContainerEnvKeys = []string{
	"WARDYN_RUN_ID", "WARDYN_PROXY_URL", "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy", mitmCAPEMEnv,
}

// agentResources is the agent container's size: the run's effective size less every extra's. Without
// extras it is spec.Resources unchanged. Each extra is taken out of what is left, and one that sets no
// cpu or memory, or asks for more than is left, is refused; so no sum is formed and none can wrap. A
// remainder under the floor is refused last.
func agentResources(spec runner.SandboxSpec) (runner.Resources, error) {
	if len(spec.Containers) == 0 {
		return spec.Resources, nil
	}
	size := runner.EffectiveResources(spec.Resources)
	cpu, mem := size.AgentCPULimitMillis, size.AgentMemoryLimitMiB // what is left for the agent
	for _, c := range spec.Containers {
		if c.CPUMillis <= 0 || c.MemoryMiB <= 0 {
			return runner.Resources{}, fmt.Errorf("k8s: extra container %q: %w: it sets no cpu or memory", c.Name, errExtraContainersBudget)
		}
		if c.CPUMillis > cpu || c.MemoryMiB > mem {
			return runner.Resources{}, fmt.Errorf("k8s: extra container %q: %w: it asks for %dm and %d MiB with %dm and %d MiB left",
				c.Name, errExtraContainersBudget, c.CPUMillis, c.MemoryMiB, cpu, mem)
		}
		cpu, mem = cpu-c.CPUMillis, mem-c.MemoryMiB
	}
	if cpu < agentFloorCPUMillis || mem < agentFloorMemoryMiB {
		return runner.Resources{}, fmt.Errorf("k8s: %w: the agent would keep %dm and %d MiB, under %dm and %d MiB",
			errExtraContainersBudget, cpu, mem, agentFloorCPUMillis, agentFloorMemoryMiB)
	}
	agent := spec.Resources
	agent.CPUMillis, agent.MemoryMiB = cpu, mem
	return agent, nil
}

// shareWorkFolder gives a pod with extras a work volume the extras can mount: every disk_mib run has one
// (ephemeralScratchVolumes); a run without one, and without a kept work folder, gets an unbounded emptyDir there.
func shareWorkFolder(pod *corev1.Pod, spec runner.SandboxSpec, keptWork bool) {
	if len(spec.Containers) == 0 || keptWork ||
		slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == scratchWorkVolumeName }) {
		return
	}
	addMainContainerVolumes(pod,
		[]corev1.Volume{{Name: scratchWorkVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		[]corev1.VolumeMount{{Name: scratchWorkVolumeName, MountPath: scratchWorkPath}})
}

// addExtraContainers appends each extra after the agent container. Call it after the drives are applied:
// it copies the agent container's work-folder and drive mounts, and no other.
func addExtraContainers(pod *corev1.Pod, spec runner.SandboxSpec, env map[string]string) {
	if len(spec.Containers) == 0 {
		return
	}
	shared := map[string]bool{scratchWorkVolumeName: true}
	for i := range spec.Drives {
		shared[driveVolumeNameAt(i)] = true
	}
	var mounts []corev1.VolumeMount
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if shared[m.Name] {
			mounts = append(mounts, m)
		}
	}
	keep := map[string]string{}
	for _, k := range extraContainerEnvKeys {
		if v, ok := env[k]; ok {
			keep[k] = v
		}
	}
	for _, c := range spec.Containers {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{
			Name:            c.Name,
			Image:           c.Image,
			Command:         slices.Clone(c.Command),
			Env:             envVars(keep),
			SecurityContext: agentSecurityContext(),
			Resources:       resourceRequirements(runner.Resources{CPUMillis: c.CPUMillis, MemoryMiB: c.MemoryMiB, DiskMiB: spec.Resources.DiskMiB}),
			VolumeMounts:    slices.Clone(mounts),
		})
	}
}
