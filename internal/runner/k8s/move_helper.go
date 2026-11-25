// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const (
	// proxyBinary is wardyn-proxy in the proxy image, which also holds the move helper's modes.
	proxyBinary = "/usr/local/bin/wardyn-proxy"
	// moveHelperContainer is the helper pod's one container.
	moveHelperContainer = "move"
	// moveHelperDeadline is the helper pod's backstop (EST): the exec ends it long before, and its cleanup deletes it.
	moveHelperDeadline int64 = 6 * 60 * 60
	// moveHelperCleanup bounds the helper's cleanup, which runs apart from the caller's context.
	moveHelperCleanup = 3 * time.Minute
	// moveHelperStderrBytes caps the stderr kept from a helper exec: enough for its one error line.
	moveHelperStderrBytes = 4 << 10
	// moveHelperUnknownFlag is what Go's flag package prints on a proxy image whose binary has no move modes.
	moveHelperUnknownFlag = "flag provided but not defined: -move-"
)

var errMoveHelperImageOld = errors.New("k8s: the proxy image predates moves: its wardyn-proxy has no -move- modes")

func moveHelperName(runID uuid.UUID) string       { return "wardyn-move-" + runID.String() }
func moveHelperNetPolName(runID uuid.UUID) string { return "wardyn-move-netpol-" + runID.String() }

// helperClaim is one claim the helper mounts, and where: the path the sandbox sees it at.
type helperClaim struct{ claim, target string }

// moveHelper is one helper pod: the run it serves, the claims it mounts, read-only for an export.
type moveHelper struct {
	ns       string
	runID    uuid.UUID
	claims   []helperClaim
	readOnly bool
}

// moveHelperPod is the helper's pod: the proxy image, idle until wardynd execs a move mode into it, run as the agent's
// user and group with no capabilities and a read-only root, no service account token, no service links, no env and no
// Secret, and only h's claims mounted. An import's pod group-owns the fresh claims as a create does.
func (d *Driver) moveHelperPod(h moveHelper) *corev1.Pod {
	sc := agentSecurityContext()
	sc.RunAsGroup = int64Ptr(runner.AgentUID)
	sc.ReadOnlyRootFilesystem = boolPtr(true)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: moveHelperName(h.runID), Namespace: h.ns},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:         int64Ptr(moveHelperDeadline),
			TerminationGracePeriodSeconds: int64Ptr(5),
			AutomountServiceAccountToken:  boolPtr(false),
			EnableServiceLinks:            boolPtr(false),
			Containers: []corev1.Container{{
				Name:                     moveHelperContainer,
				Image:                    d.cfg.ProxyImage,
				Command:                  []string{proxyBinary, "-move-idle"},
				SecurityContext:          sc,
				Resources:                proxyResources(false),
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			}},
		},
	}
	if d.cfg.ImagePullSecret != "" {
		pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: d.cfg.ImagePullSecret}}
	}
	d.placement.apply(pod, h.runID, componentMoveHelper, nil)
	pod.Labels = d.stampRunner(pod.Labels)
	for i, c := range h.claims {
		name := driveVolumeNameAt(i)
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: c.claim, ReadOnly: h.readOnly,
			}},
		})
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: name, MountPath: c.target, ReadOnly: h.readOnly})
	}
	pod.Spec.SecurityContext = &corev1.PodSecurityContext{SupplementalGroups: []int64{driveFSGroup}}
	if !h.readOnly {
		policy := corev1.FSGroupChangeOnRootMismatch
		pod.Spec.SecurityContext = &corev1.PodSecurityContext{FSGroup: int64Ptr(driveFSGroup), FSGroupChangePolicy: &policy}
	}
	return pod
}

// moveHelperNetPol denies the helper pod every connection, both ways.
func (d *Driver) moveHelperNetPol(h moveHelper) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: moveHelperNetPolName(h.runID), Namespace: h.ns, Labels: d.stampRunner(wardynLabels(h.runID, componentMoveHelper, nil))},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: wardynLabels(h.runID, componentMoveHelper, nil)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
}

// runMoveHelper reclaims a helper an earlier move of this run left behind, creates the helper's policy, then its pod,
// waits for it to run, and execs argv in it with stdin (nil for none) and stdout. Its cleanup always runs, on a bounded
// context of its own: the pod is deleted under its UID and waited for before its policy goes, so the pod is never
// without its policy.
func (d *Driver) runMoveHelper(ctx context.Context, h moveHelper, argv []string, stdin io.Reader, stdout io.Writer) (err error) {
	if err := d.cleanupMoveHelper(ctx, h.ns, nil, moveHelperName(h.runID), moveHelperNetPolName(h.runID)); err != nil {
		return fmt.Errorf("k8s: move helper: reclaim one left by an earlier move: %w", err)
	}
	pols := d.clientset.NetworkingV1().NetworkPolicies(h.ns)
	pol := d.moveHelperNetPol(h)
	if _, err := pols.Create(ctx, pol, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("k8s: move helper: create its NetworkPolicy: %w", err)
	}
	var created *corev1.Pod
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), moveHelperCleanup)
		defer cancel()
		if cerr := d.cleanupMoveHelper(cctx, h.ns, created, moveHelperName(h.runID), pol.Name); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	created, err = d.clientset.CoreV1().Pods(h.ns).Create(ctx, d.moveHelperPod(h), metav1.CreateOptions{})
	if err != nil {
		// A failed create may still have made the pod; the cleanup looks it up by name.
		created = nil
		return fmt.Errorf("k8s: move helper: create its pod: %w", err)
	}
	if err := d.waitContainerRunning(ctx, d.newStartClock(), h.ns, created.Name, moveHelperContainer, nil); err != nil {
		if d.helperImageOld(ctx, h.ns, created.Name) {
			return errMoveHelperImageOld
		}
		return fmt.Errorf("k8s: move helper: its pod never started: %w", err)
	}
	ex, err := d.newExecutor(h.ns, created.Name, moveHelperContainer, argv, stdin != nil, false)
	if err != nil {
		return fmt.Errorf("k8s: move helper: %w", err)
	}
	stderr := &cappedBuffer{max: moveHelperStderrBytes}
	if err := ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr}); err != nil {
		msg := strings.TrimSpace(stderr.buf.String())
		if strings.Contains(msg, moveHelperUnknownFlag) {
			return errMoveHelperImageOld
		}
		return fmt.Errorf("k8s: move helper: %w: %s", err, msg)
	}
	return nil
}

// helperImageOld reads a helper pod that never ran: its container ended on Go's unknown-flag error.
func (d *Driver) helperImageOld(ctx context.Context, ns, name string) bool {
	pod, err := d.clientset.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false
	}
	for _, cs := range pod.Status.ContainerStatuses {
		for _, t := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
			if t != nil && strings.Contains(t.Message, moveHelperUnknownFlag) {
				return true
			}
		}
	}
	return false
}

// cleanupMoveHelper deletes the helper pod under its UID, waits until it is gone, then deletes its policy. A nil pod
// is looked up by name and adopted if it exists; the policy is deleted only once the pod is known to be gone.
func (d *Driver) cleanupMoveHelper(ctx context.Context, ns string, pod *corev1.Pod, podName, polName string) error {
	if pod == nil {
		got, err := d.clientset.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		switch {
		case isNotFound(err):
		case err != nil:
			return fmt.Errorf("k8s: move helper: read its pod, so its NetworkPolicy stays: %w", err)
		default:
			pod = got
		}
	}
	if pod != nil {
		uid := pod.UID
		err := d.clientset.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("k8s: move helper: delete its pod: %w", err)
		}
		if err := d.waitPodGone(ctx, ns, pod, moveHelperCleanup); err != nil {
			return fmt.Errorf("k8s: move helper: wait for its pod to go: %w", err)
		}
	}
	if err := d.clientset.NetworkingV1().NetworkPolicies(ns).Delete(ctx, polName, metav1.DeleteOptions{}); err != nil && !isNotFound(err) {
		return fmt.Errorf("k8s: move helper: delete its NetworkPolicy: %w", err)
	}
	return nil
}
