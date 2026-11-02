// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"slices"

	corev1 "k8s.io/api/core/v1"
)

// The status half of the arms for a pod whose main containers never started (design K1 §3.1 (a)):
// pure reads of one pod status, with no client and no build tag, so they test in the light build.
// runEvidence.neverInitializedArm binds what they accept to the pod's node.

// reasonDeletionByPodGC is the DisruptionTarget reason the pod garbage collector sets when it marks
// a pod Failed without any kubelet, before deleting the pod of a Node that is gone.
const reasonDeletionByPodGC = "DeletionByPodGC"

// Arms of a pod whose main containers never started.
const (
	armNeverInit = "never_initialized"
	// armMainNeverStarted: deleted after its init containers completed and before any main
	// container started (#3537).
	armMainNeverStarted = "main_never_started"
)

func podGCMarked(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.DisruptionTarget && c.Reason == reasonDeletionByPodGC {
			return true
		}
	}
	return false
}

// containerStatuses is every init and main container status; ephemeral containers are never
// evidence inputs (Fable r10 L2).
func containerStatuses(pod *corev1.Pod) []corev1.ContainerStatus {
	return slices.Concat(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses)
}

// specContainerNames is every init and main container the spec declares.
func specContainerNames(pod *corev1.Pod) (inits, mains []string) {
	for _, c := range pod.Spec.InitContainers {
		inits = append(inits, c.Name)
	}
	for _, c := range pod.Spec.Containers {
		mains = append(mains, c.Name)
	}
	return inits, mains
}

func statusByName(pod *corev1.Pod) map[string]corev1.ContainerStatus {
	m := map[string]corev1.ContainerStatus{}
	for _, cs := range containerStatuses(pod) {
		m[cs.Name] = cs
	}
	return m
}

// unstartedArm names the arm whose status conditions pod meets, armNeverInit or
// armMainNeverStarted ("" for neither), with the init containers that terminated. clean reports a
// container that no read of the pod ever showed with a runtime ID (no baseline, no latch).
func unstartedArm(pod *corev1.Pod, clean func(container string) bool) (arm string, terminatedInits []string) {
	if inits, ok := neverInitialized(pod, clean); ok {
		return armNeverInit, inits
	}
	if inits, ok := mainNeverStarted(pod, clean); ok {
		return armMainNeverStarted, inits
	}
	return "", nil
}

// neverInitialized reports the never-initialized arm's status conditions alone (Fable r10 H1, r11
// L3): Failed, deleting, not by the pod GC, Initialized=False present, a status for every spec
// container, every main container waiting with no baseline or latch, and every init container
// waiting with none or terminated. The node binding is checked separately.
func neverInitialized(pod *corev1.Pod, clean func(string) bool) (terminatedInits []string, ok bool) {
	if pod.Status.Phase != corev1.PodFailed || pod.DeletionTimestamp == nil || podGCMarked(pod) {
		return nil, false
	}
	initialized := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodInitialized {
			if c.Status != corev1.ConditionFalse {
				return nil, false
			}
			initialized = true
		}
	}
	if !initialized {
		return nil, false
	}
	inits, mains := specContainerNames(pod)
	statuses := statusByName(pod)
	for _, name := range mains {
		cs, ok := statuses[name]
		if !ok || cs.State.Waiting == nil || !clean(name) {
			return nil, false
		}
	}
	for _, name := range inits {
		cs, ok := statuses[name]
		switch {
		case !ok:
			return nil, false
		case cs.State.Waiting != nil && clean(name):
		case cs.State.Terminated != nil:
			terminatedInits = append(terminatedInits, name)
		default:
			return nil, false
		}
	}
	return terminatedInits, true
}

// mainNeverStarted reports the main-never-started arm's status conditions alone: Failed, deleting,
// not by the pod GC, Initialized=True present, at least one init container, a status for every
// spec container, every init container terminated with exit code 0, and every main container
// waiting with no container ID, no start, no restart, no last termination, and no baseline or
// latch. The kubelet writes this status for a pod deleted between its last init container's exit
// and its first main container's start: TerminatePod takes the Initialized condition and the init
// container's completion from its own cached status, but asks hasPodInitialized of an older copy
// of the pod that shows no completion yet, so it leaves the main containers waiting instead of
// synthesising their terminated states. The kubelet reports Failed only once no container can be
// running, from a runtime read after the kill, so a main container the runtime ever created would
// show its container ID here. The node binding is checked separately, as for neverInitialized.
func mainNeverStarted(pod *corev1.Pod, clean func(string) bool) (inits []string, ok bool) {
	if pod.Status.Phase != corev1.PodFailed || pod.DeletionTimestamp == nil || podGCMarked(pod) {
		return nil, false
	}
	initialized := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodInitialized {
			if c.Status != corev1.ConditionTrue {
				return nil, false
			}
			initialized = true
		}
	}
	inits, mains := specContainerNames(pod)
	if !initialized || len(inits) == 0 {
		return nil, false
	}
	statuses := statusByName(pod)
	for _, name := range mains {
		cs, ok := statuses[name]
		if !ok || cs.State.Waiting == nil || cs.ContainerID != "" || (cs.Started != nil && *cs.Started) ||
			cs.RestartCount != 0 || cs.LastTerminationState != (corev1.ContainerState{}) || !clean(name) {
			return nil, false
		}
	}
	for _, name := range inits {
		cs, ok := statuses[name]
		if !ok || cs.State.Terminated == nil || cs.State.Terminated.ExitCode != 0 {
			return nil, false
		}
	}
	return inits, true
}
