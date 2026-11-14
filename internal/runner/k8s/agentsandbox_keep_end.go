// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// interactiveIdleCmd is an interactive run's main command; a run kept at its end gets the script that restores the
// image's booted marker on a second boot.
func (d *Driver) interactiveIdleCmd(spec runner.SandboxSpec) []string {
	if d.keepRun(spec) && keepHarnessRefusal(spec.Labels[labelAgent]) == "" {
		return keepIdleScript(spec.RunID.String())
	}
	return []string{"agent-run", "--idle"}
}

// annotationKeptSandbox, on a kept run's EndpointSlice, is the UID of the Sandbox Wardyn created for the run. A Sandbox
// deleted and made again under the same name is at generation 1 again, but has a new UID.
const annotationKeptSandbox = "wardyn.dev/kept-sandbox-uid"

var errKeptSandboxNotOurs = errors.New("k8s: the run's EndpointSlice does not record the agent's Sandbox as the one Wardyn created for this run")

// recordKeptSandbox writes uid, from Wardyn's own Sandbox create, on a kept run's EndpointSlice. A failure is logged: the
// run is then not kept at its end, since nothing vouches for its Sandbox.
func (d *Driver) recordKeptSandbox(ctx context.Context, pod *corev1.Pod, uid k8stypes.UID) {
	if pod.Labels[labelKeep] != "true" {
		return
	}
	runID, err := runIDFromAgentPodName(pod.Name)
	if err == nil {
		eps := d.clientset.DiscoveryV1().EndpointSlices(pod.Namespace)
		var es *discoveryv1.EndpointSlice
		if es, err = eps.Get(ctx, keepServiceName(runID), metav1.GetOptions{}); err == nil {
			metav1.SetMetaDataAnnotation(&es.ObjectMeta, annotationKeptSandbox, string(uid))
			_, err = eps.Update(ctx, es, metav1.UpdateOptions{})
		}
	}
	if err != nil {
		slog.Warn("wardynd: k8s substrate: could not record a kept run's Sandbox; the run will not be kept at its end",
			slog.String("namespace", pod.Namespace), slog.String("pod", pod.Name), slog.String("error", err.Error()))
	}
}

// checkKeptSandbox answers errKeptSandboxNotOurs unless uid is the one recorded on the run's EndpointSlice.
func (d *Driver) checkKeptSandbox(ctx context.Context, ns string, runID uuid.UUID, uid k8stypes.UID) error {
	es, err := d.clientset.DiscoveryV1().EndpointSlices(ns).Get(ctx, keepServiceName(runID), metav1.GetOptions{})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: keep: read the run's EndpointSlice: %w", err)
	}
	if err != nil || uid == "" || es.Annotations[annotationKeptSandbox] != string(uid) {
		return errKeptSandboxNotOurs
	}
	return nil
}

// keptSandbox decides from the Sandbox, which the controller never remakes, whether the run is kept: never from the pod,
// which may be gone for a moment. It returns the Sandbox of a kept run.
func (k *keepDriver) keptSandbox(ctx context.Context, ns string, runID uuid.UUID) (*unstructured.Unstructured, error) {
	sb, err := k.dynamic.Resource(sandboxesGVR).Namespace(ns).Get(ctx, agentPodName(runID), metav1.GetOptions{})
	if isNotFound(err) {
		return nil, runner.ErrEndUnsupported
	}
	if err != nil {
		return nil, fmt.Errorf("k8s: keep: read the agent's Sandbox: %w", err)
	}
	labels, _, _ := unstructured.NestedStringMap(sb.Object, "spec", "podTemplate", "metadata", "labels")
	if sb.GetDeletionTimestamp() != nil || labels[labelKeep] != "true" || sandboxSpecChanged(sb) {
		return nil, runner.ErrEndUnsupported
	}
	if why := keepHarnessRefusal(labels[labelAgent]); why != "" {
		return nil, fmt.Errorf("%w: %s", runner.ErrEndUnsupported, why)
	}
	err = k.checkKeptSandbox(ctx, ns, runID, sb.GetUID())
	if errors.Is(err, errKeptSandboxNotOurs) {
		return nil, fmt.Errorf("%w: %w", runner.ErrEndUnsupported, err)
	}
	if err != nil {
		return nil, err
	}
	return sb, nil
}

// refRun is the run id and the namespace of a sandbox ref.
func (d *Driver) refRun(ctx context.Context, ref string) (uuid.UUID, string, error) {
	runID, err := runIDFromAgentPodName(ref)
	if err != nil {
		return uuid.Nil, "", err
	}
	ns, err := d.runNamespace(ctx, runID)
	if err != nil {
		return uuid.Nil, "", err
	}
	return runID, ns, nil
}

// EndSandbox keeps the run: the proxy stops and the agent pod goes; the controller makes it again held behind
// Wardyn's gate, with no node and no compute, until StartSandbox reads it back. The work drive and the run Secret stay.
// Every step can run again: the lease pass repeats the end until the run is revived or torn down, and a run whose
// pod the controller has not yet made again is still kept.
func (k *keepDriver) EndSandbox(ctx context.Context, ref string) error {
	runID, ns, err := k.refRun(ctx, ref)
	if err != nil {
		return err
	}
	if _, err := k.keptSandbox(ctx, ns, runID); err != nil {
		return err
	}
	svc, err := k.clientset.CoreV1().Services(ns).Get(ctx, keepServiceName(runID), metav1.GetOptions{})
	if isNotFound(err) {
		return runner.ErrEndUnsupported
	}
	if err != nil {
		return fmt.Errorf("k8s: keep: read the run's Service: %w", err)
	}
	if err := k.writeKeepEndpoints(ctx, ns, runID, svc, ""); err != nil {
		return err
	}
	if err := k.stopKeptProxy(ctx, ns, runID); err != nil {
		return err
	}
	return k.deleteKeptAgentPod(ctx, ns, runID)
}

// stopKeptProxy deletes the run's proxy pod, waits until it is gone, and removes the replacement config Secrets.
func (d *Driver) stopKeptProxy(ctx context.Context, ns string, runID uuid.UUID) error {
	if err := d.clientset.CoreV1().Pods(ns).Delete(ctx, proxyPodName(runID), metav1.DeleteOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: keep: delete the proxy pod: %w", err)
	}
	proxyOnly := metav1.ListOptions{LabelSelector: labelRun + "=" + runID.String() + "," + labelComponent + "=" + componentProxy}
	if err := d.waitPodsGone(ctx, ns, proxyOnly, 2*time.Minute); err != nil {
		return fmt.Errorf("k8s: keep: wait for the proxy pod to go: %w", err)
	}
	replacements := metav1.ListOptions{LabelSelector: labelRun + "=" + runID.String() + "," + labelProxyCfg}
	if err := d.clientset.CoreV1().Secrets(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, replacements); err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: keep: delete the replacement config Secrets: %w", err)
	}
	return nil
}

// deleteKeptAgentPod deletes the agent pod so the controller makes it again held. A pod already gone, or already held,
// is left as it is.
func (d *Driver) deleteKeptAgentPod(ctx context.Context, ns string, runID uuid.UUID) error {
	pod, err := d.clientset.CoreV1().Pods(ns).Get(ctx, agentPodName(runID), metav1.GetOptions{})
	if isNotFound(err) || err == nil && agentPodGated(pod) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("k8s: keep: read the agent pod: %w", err)
	}
	uid := pod.UID
	if err := d.clientset.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: keep: delete the agent pod: %w", err)
	}
	if err := d.waitPodGone(ctx, ns, pod, 2*time.Minute); err != nil {
		return fmt.Errorf("k8s: keep: wait for the agent pod to go: %w", err)
	}
	return nil
}

// StartSandbox starts a kept run's agent: it reads the held pod back against the Sandbox's own template, removes the
// gate, and waits for the main container. A pod the controller has not yet made is waited for by the read-back.
func (k *keepDriver) StartSandbox(ctx context.Context, ref string) error {
	runID, ns, err := k.refRun(ctx, ref)
	if err != nil {
		return err
	}
	pod, err := k.clientset.CoreV1().Pods(ns).Get(ctx, agentPodName(runID), metav1.GetOptions{})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: keep: read the agent pod: %w", err)
	}
	if err == nil && pod.DeletionTimestamp != nil {
		return errors.New("k8s: keep: the agent pod is still being deleted by the run's end; revive again once it is gone")
	}
	if err == nil && !agentPodGated(pod) && mainContainerRunning(pod) {
		return nil
	}
	want, uid, err := k.keptWant(ctx, ns, runID)
	if err != nil {
		return err
	}
	clock := k.newStartClock()
	if err := k.admitAgentSandboxPod(ctx, clock, want, uid); err != nil {
		return err
	}
	return k.waitContainerRunning(ctx, clock, ns, agentPodName(runID), mainContainerName, nil)
}

func mainContainerRunning(pod *corev1.Pod) bool {
	return slices.ContainsFunc(pod.Status.ContainerStatuses, func(s corev1.ContainerStatus) bool {
		return s.Name == mainContainerName && s.State.Running != nil
	})
}

// keptWant is the pod a revive expects to find held: the Sandbox's own template, only if its Sandbox was never edited,
// is the one Wardyn created, and is of an agent that is revived.
func (k *keepDriver) keptWant(ctx context.Context, ns string, runID uuid.UUID) (*corev1.Pod, k8stypes.UID, error) {
	sb, err := k.dynamic.Resource(sandboxesGVR).Namespace(ns).Get(ctx, agentPodName(runID), metav1.GetOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("k8s: keep: read the agent's Sandbox: %w", err)
	}
	if sandboxSpecChanged(sb) {
		return nil, "", errors.New("k8s: the agent's Sandbox was changed after the run started; revive refused")
	}
	if err := k.checkKeptSandbox(ctx, ns, runID, sb.GetUID()); err != nil {
		return nil, "", fmt.Errorf("%w; revive refused", err)
	}
	tmplLabels, _, _ := unstructured.NestedStringMap(sb.Object, "spec", "podTemplate", "metadata", "labels")
	if why := keepHarnessRefusal(tmplLabels[labelAgent]); why != "" {
		return nil, "", fmt.Errorf("%s; revive refused", why)
	}
	want, err := podFromSandboxTemplate(sb)
	if err != nil {
		return nil, "", err
	}
	want, err = k.runtimeClassAdmitted(ctx, want)
	if err != nil {
		return nil, "", err
	}
	return want, sb.GetUID(), nil
}

// podFromSandboxTemplate is the pod the Sandbox's template asks for. Generation 1 means this Sandbox's spec was never edited; it does not prove the Sandbox is the one Wardyn created.
// checkKeptSandbox proves that, from the UID recorded when Wardyn created it.
func podFromSandboxTemplate(sb *unstructured.Unstructured) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	pod.Name, pod.Namespace, pod.OwnerReferences = sb.GetName(), sb.GetNamespace(), sb.GetOwnerReferences()
	pod.Labels, _, _ = unstructured.NestedStringMap(sb.Object, "spec", "podTemplate", "metadata", "labels")
	pod.Annotations, _, _ = unstructured.NestedStringMap(sb.Object, "spec", "podTemplate", "metadata", "annotations")
	spec, _, err := unstructured.NestedMap(sb.Object, "spec", "podTemplate", "spec")
	if err != nil {
		return nil, fmt.Errorf("k8s: keep: read the Sandbox's pod template: %w", err)
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &pod.Spec); err != nil {
		return nil, fmt.Errorf("k8s: keep: convert the Sandbox's pod template: %w", err)
	}
	pod.Spec.SchedulingGates = slices.DeleteFunc(pod.Spec.SchedulingGates, func(g corev1.PodSchedulingGate) bool { return g.Name == agentSandboxGate })
	if len(pod.Spec.SchedulingGates) == 0 {
		pod.Spec.SchedulingGates = nil
	}
	return pod, nil
}

// keptPodStatus reads a kept run's held pod as stopped. The pod must be gated, keep-labelled, of an agent that is revived,
// and carry this run's labels; the run's proxy must be gone; and the pod's controlling Sandbox must be the one Wardyn
// created. Anything else is left to replacedPodStatus.
func (d *Driver) keptPodStatus(ctx context.Context, ns string, pod *corev1.Pod) (runner.Status, bool, error) {
	if !d.keepOn() || pod.Labels[labelKeep] != "true" || !agentPodGated(pod) || keepHarnessRefusal(pod.Labels[labelAgent]) != "" {
		return runner.Status{}, false, nil
	}
	runID, err := runIDFromAgentPodName(pod.Name)
	if err != nil || !hasAgentLabels(pod.Labels, runID) {
		return runner.Status{}, false, nil
	}
	if _, err := d.clientset.CoreV1().Pods(ns).Get(ctx, proxyPodName(runID), metav1.GetOptions{}); err == nil {
		return runner.Status{}, false, nil
	} else if !isNotFound(err) {
		return runner.Status{}, false, fmt.Errorf("read proxy pod: %w", err)
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != "Sandbox" {
		return runner.Status{}, false, nil
	}
	err = d.checkKeptSandbox(ctx, ns, runID, owner.UID)
	if errors.Is(err, errKeptSandboxNotOurs) {
		return runner.Status{}, false, nil
	}
	if err != nil {
		return runner.Status{}, false, err
	}
	return runner.Status{State: types.RunStopped, Message: "kept: revive to start it again"}, true, nil
}
