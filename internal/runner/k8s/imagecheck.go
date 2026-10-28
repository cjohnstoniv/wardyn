// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A prewrapped image's registration check (design final §3): per candidate architecture, one pod from the
// pinned image runs runner.ImageCheckScript behind the attempt's own deny-all NetworkPolicy. The cluster pulls
// the image with the deployment's existing pull secret; the control plane reads no registry and no Secret.

var _ runner.PrewrappedImageChecker = (*Driver)(nil)

// Vars, not consts, only so tests can shorten them.
var (
	// imageCheckDeadlineSeconds is the pod's activeDeadlineSeconds: the cluster ends a check that runs on,
	// even when the control plane that started it is gone.
	imageCheckDeadlineSeconds int64 = 600
	// imageCheckWait bounds this side's wait a little past the cluster's deadline, so the kubelet's own
	// DeadlineExceeded is normally what ends a stuck check.
	imageCheckWait = 11 * time.Minute
	// imageCheckNoNodeAfter is how long a pod may stay unschedulable before its architecture counts as having
	// no node to check on (an autoscaler may still be adding one).
	imageCheckNoNodeAfter = 2 * time.Minute
)

const imageCheckContainerName = "check"

// imageCheckDetailMax bounds what a verdict carries from the kubelet or the pod's output.
const imageCheckDetailMax = 200

// PrewrappedCheckUnavailable implements runner.PrewrappedImageChecker: the check runs only on the control
// plane's own substrate, and only where the boot canary proved NetworkPolicy enforced, since the check pod
// runs the image's own shell with nothing but a deny-all policy around it.
func (d *Driver) PrewrappedCheckUnavailable() string {
	switch {
	case d.runnerMode():
		return "a runner's Kubernetes substrate does not check images"
	case !d.netPolEnforced:
		return "this cluster's NetworkPolicy enforcement is not proven (the boot canary did not show it enforced)"
	}
	return ""
}

// CheckPrewrappedImage implements runner.PrewrappedImageChecker.
func (d *Driver) CheckPrewrappedImage(ctx context.Context, attempt uuid.UUID, ref string) []runner.ImageArchVerdict {
	out := make([]runner.ImageArchVerdict, len(runner.PrewrappedCandidateArches))
	ns := d.cfg.Namespace
	labels := wardynLabels(attempt, componentImageCheck, nil)
	// The attempt's policy comes first and is removed last (teardownByRunID waits for its pods to be gone).
	netpol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "wardyn-imagecheck-netpol-" + attempt.String(), Namespace: ns, Labels: labels},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			// No rules: deny all in and out.
		},
	}
	if _, err := d.clientset.NetworkingV1().NetworkPolicies(ns).Create(ctx, netpol, metav1.CreateOptions{}); err != nil {
		for i, arch := range runner.PrewrappedCandidateArches {
			out[i] = runner.ImageArchVerdict{Arch: arch, Code: runner.ImageCheckFailed, Detail: bounded("create the check's NetworkPolicy: " + err.Error())}
		}
		return out
	}
	var wg sync.WaitGroup
	for i, arch := range runner.PrewrappedCandidateArches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = d.checkImageOnArch(ctx, attempt, ref, arch)
		}()
	}
	wg.Wait()
	zero := int64(0)
	if err := d.teardownByRunID(context.WithoutCancel(ctx), attempt, &zero); err != nil {
		slog.WarnContext(ctx, "wardynd: k8s substrate: image check teardown failed; the orphan sweep reclaims it",
			slog.String("attempt", attempt.String()), slog.Any("err", err))
	}
	return out
}

// checkImageOnArch runs one architecture's check pod and maps what it saw to a verdict. An operator
// placement that already pins another architecture gets no pod: there is no node of this one to check on.
func (d *Driver) checkImageOnArch(ctx context.Context, attempt uuid.UUID, ref, arch string) runner.ImageArchVerdict {
	v := runner.ImageArchVerdict{Arch: arch}
	placement, err := d.placement.withArch(arch)
	if err != nil {
		v.Code = runner.ImageCheckNoNode
		return v
	}
	ns := d.cfg.Namespace
	podName := "wardyn-imagecheck-" + attempt.String() + "-" + arch
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:        &imageCheckDeadlineSeconds,
			AutomountServiceAccountToken: boolPtr(false),
			EnableServiceLinks:           boolPtr(false),
			Containers: []corev1.Container{{
				Name:    imageCheckContainerName,
				Image:   ref,
				Command: []string{"sh", "-c", runner.ImageCheckScript},
				// agent-base runs as `USER agent`, a name the kubelet cannot verify as non-root: the agent's
				// own context supplies the numeric UID with the same hardening.
				SecurityContext: agentSecurityContext(),
				Resources:       imageCheckResources(),
				// The script prints what it found; on a failure that line becomes the termination message.
				TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			}},
		},
	}
	if d.cfg.ImagePullSecret != "" {
		pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: d.cfg.ImagePullSecret}}
	}
	placement.apply(pod, attempt, componentImageCheck, nil)
	if _, err := d.clientset.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		v.Code, v.Detail = runner.ImageCheckFailed, bounded("create the check pod: "+err.Error())
		return v
	}
	v.Code, v.Detail = d.waitImageCheck(ctx, podName)
	// Early, per architecture; the attempt's teardown is what waits for every pod to be gone.
	zero := int64(0)
	if err := d.clientset.CoreV1().Pods(ns).Delete(context.WithoutCancel(ctx), podName, metav1.DeleteOptions{GracePeriodSeconds: &zero}); err != nil && !isNotFound(err) {
		slog.DebugContext(ctx, "wardynd: k8s substrate: image check pod delete failed; the attempt's teardown retries",
			slog.String("pod", podName), slog.Any("err", err))
	}
	return v
}

// waitImageCheck polls the check pod to a verdict.
func (d *Driver) waitImageCheck(ctx context.Context, podName string) (code, detail string) {
	var unschedulableSince time.Time
	pollErr := wait.PollUntilContextTimeout(ctx, k8sPollInterval, imageCheckWait, true, func(pollCtx context.Context) (bool, error) {
		pod, err := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(pollCtx, podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		code, detail = imageCheckVerdict(pod, &unschedulableSince, time.Now())
		return code != "", nil
	})
	if pollErr != nil {
		if errors.Is(pollErr, context.DeadlineExceeded) {
			return runner.ImageCheckTimedOut, ""
		}
		return runner.ImageCheckFailed, bounded("read the check pod: " + pollErr.Error())
	}
	return code, detail
}

// imageCheckVerdict maps one read of the check pod to a verdict, or "" while it is still running. Only an
// exit 0 is a pass; every outcome it does not recognise is ImageCheckFailed, never a pass.
func imageCheckVerdict(pod *corev1.Pod, unschedulableSince *time.Time, now time.Time) (code, detail string) {
	if pod.Status.Phase == corev1.PodFailed && pod.Status.Reason == "DeadlineExceeded" {
		return runner.ImageCheckTimedOut, ""
	}
	for _, c := range pod.Status.Conditions {
		if c.Type != corev1.PodScheduled {
			continue
		}
		if c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			if unschedulableSince.IsZero() {
				*unschedulableSince = now
			}
			if now.Sub(*unschedulableSince) >= imageCheckNoNodeAfter {
				return runner.ImageCheckNoNode, ""
			}
		} else {
			*unschedulableSince = time.Time{}
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != imageCheckContainerName {
			continue
		}
		if t := cs.State.Terminated; t != nil {
			msg := strings.TrimSpace(t.Message)
			switch t.ExitCode {
			case 0:
				return runner.ImageCheckPassed, ""
			case 3:
				if tool, ok := strings.CutPrefix(lastLine(msg), "missing "); ok {
					return runner.ImageCheckContractMissing, bounded(tool)
				}
			case 4:
				return runner.ImageCheckContractOld, ""
			}
			return runner.ImageCheckFailed, bounded(fmt.Sprintf("exit %d: %s", t.ExitCode, lastLine(msg)))
		}
		if w := cs.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff":
				if noPlatform(w.Message) {
					return runner.ImageCheckPlatformMissing, ""
				}
				if w.Reason == "ImagePullBackOff" { // failed at least once and backing off
					return runner.ImageCheckUnpullable, pullFailure(w.Message)
				}
			case "InvalidImageName":
				return runner.ImageCheckUnpullable, "invalid image name"
			default:
				if runner.TerminalWaitingReasons[w.Reason] {
					return runner.ImageCheckFailed, bounded(w.Reason + ": " + w.Message)
				}
			}
		}
	}
	return "", ""
}

// noPlatform reports whether a pull failure says the image has no build for the node's platform:
// containerd's "no match for platform in manifest" and Docker's "no matching manifest for linux/arm64".
func noPlatform(msg string) bool {
	return strings.Contains(msg, "no match for platform") || strings.Contains(msg, "no matching manifest for")
}

// pullFailure is a pull failure's short reason: what an admin acts on, never the registry's whole answer.
func pullFailure(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "401") || strings.Contains(m, "unauthorized") || strings.Contains(m, "403") || strings.Contains(m, "forbidden") || strings.Contains(m, "denied"):
		return "unauthorized"
	case strings.Contains(m, "not found") || strings.Contains(m, "404") || strings.Contains(m, "manifest unknown"):
		return "not found"
	}
	return "pull failed"
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func bounded(s string) string {
	if len(s) > imageCheckDetailMax {
		return s[:imageCheckDetailMax]
	}
	return s
}

// imageCheckResources: a shell, four tests and one small Go binary.
func imageCheckResources() corev1.ResourceRequirements {
	list := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(100, resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(64*1024*1024, resource.BinarySI),
	}
	return corev1.ResourceRequirements{Requests: list, Limits: list}
}
