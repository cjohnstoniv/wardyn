// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// byoiImagePrefix marks a BYOI (bring-your-own-image) run — mirrors
// internal/api/runs_dispatch.go's own `strings.HasPrefix(image, "wardyn-byoi/")`
// check. Re-derived here (not imported: internal/api is outside this
// package's build tag) so Exec can refuse it BEFORE creating any ephemeral
// container, entirely at the substrate seam.
const byoiImagePrefix = "wardyn-byoi/"

const (
	execWaitPollInterval = 1 * time.Second
	// execWaitMaxProbeErrors mirrors docker's waitMaxProbeErrors BUDGET
	// (~1 minute), not its raw number: docker polls at 200ms so 300 errors is
	// ~1 minute there; this package polls at 1s, so the same budget is 60.
	// A transient error is NOT "the agent exited" and must not surface as one.
	execWaitMaxProbeErrors = 60
)

// notFoundExitCode is what Wait reports when the pod vanishes without ever
// showing a terminated exec status ("pod NotFound is authoritative
// completion"). Non-zero (routes to FAILED, not a false COMPLETED): most of
// the time this is our own KillSandbox having already deleted the pod, so the
// run is already KILLED and this value is never written — but on the rarer
// path where the pod disappeared for some other reason (node drain, an
// external delete) while still RUNNING, reporting failure is the honest
// choice: Wardyn must never claim a vanished sandbox as a false success.
const notFoundExitCode = -1

// Exec launches the agent process as an ephemeral container named
// "wardyn-agent" (execContainerName) on the sandbox ref. THREE hard API facts
// drive this shape:
//   - the ephemeral container's OWN securityContext must carry the full
//     restricted set — Pod Security Standards admission checks ephemeral
//     containers, not just the pod;
//   - it must NOT set Resources — the apiserver rejects a resource request on
//     an ephemeral container (pod-level bounding only);
//   - ephemeral containers are ADD-ONLY, so this is one-shot per ref: a
//     second Exec and a BYOI image (refused before the first) both fail
//     closed with a clear error rather than a silent no-op or a stale id.
//
// Env is copied from the pod's existing main container spec (ephemeral
// containers inherit nothing), read from the live pod object, so this is
// restart-safe (no in-memory state).
func (d *Driver) Exec(ctx context.Context, ref string, argv []string) (string, error) {
	if len(argv) == 0 {
		return "", errors.New("k8s: exec: empty argv")
	}
	ns := d.cfg.Namespace
	pod, err := d.clientset.CoreV1().Pods(ns).Get(ctx, ref, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("k8s: exec: get pod %q: %w", ref, err)
	}

	main, ok := findContainer(pod.Spec.Containers, mainContainerName)
	if !ok {
		return "", fmt.Errorf("k8s: exec: sandbox %q has no %q container", ref, mainContainerName)
	}
	// BYOI refusal BEFORE any ephemeral container is created — see
	// errBYOIUnsupported's doc for why this must happen on the FIRST Exec
	// call, not merely the second.
	if strings.HasPrefix(main.Image, byoiImagePrefix) {
		return "", errBYOIUnsupported
	}
	for _, ec := range pod.Spec.EphemeralContainers {
		if ec.Name == execContainerName {
			return "", errSecondExec
		}
	}

	cmd := argv
	if d.cfg.Record {
		runID := uuid.Nil
		if id, perr := uuid.Parse(pod.Labels[labelRun]); perr == nil {
			runID = id
		}
		cmd = recordCmd(runID, argv)
	}
	podCopy := pod.DeepCopy()
	podCopy.Spec.EphemeralContainers = append(podCopy.Spec.EphemeralContainers, corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:    execContainerName,
			Image:   main.Image,
			Command: cmd,
			Env:     main.Env, // copied verbatim: ephemeral containers inherit nothing
			// The user drive AND the disk budget's scratch volumes, read the
			// same way as Env — from the live pod. Ephemeral containers
			// inherit no mounts either, and the agent's real work happens
			// HERE, not in the idle main container: without this the
			// member's drive would be mounted into a container the agent
			// never touches, and ephemeralScratchVolumes would bound a
			// filesystem it doesn't write. Copying rather than rebuilding
			// keeps the two in step by construction (the apiserver refuses a
			// mount naming a volume the pod lacks). These two sources are the
			// only mounts here — spec.Mounts is refused outright by
			// CreateSandbox — and none may carry a subPath, which the
			// apiserver forbids on an ephemeral container:
			// TestCreateSandbox_NoMountCarriesASubPath pins it.
			VolumeMounts:    main.VolumeMounts,
			SecurityContext: agentSecurityContext(),
			// Resources deliberately left zero-value: the apiserver rejects a
			// resource request on an ephemeral container.
		},
	})
	if _, err := d.clientset.CoreV1().Pods(ns).UpdateEphemeralContainers(ctx, ref, podCopy, metav1.UpdateOptions{}); err != nil {
		return "", fmt.Errorf("k8s: exec: add ephemeral container: %w", err)
	}
	if w, ok := d.execOutputs.Load(ref); ok {
		go d.followExecOutput(ref, w.(io.Writer))
	}
	return execContainerName, nil
}

// followExecOutput streams the agent exec container's log into w (the run's
// output tail) once the kubelet has started it, until it exits or the pod is
// deleted. Best-effort: a container that never starts, a pod that vanishes or
// a refused log read leaves the tail as far as it got. The log read needs
// `get` on pods/log in the runs namespace.
func (d *Driver) followExecOutput(ref string, w io.Writer) {
	ctx := context.Background()
	pods := d.clientset.CoreV1().Pods(d.cfg.Namespace)
	for errs := 0; ; time.Sleep(execWaitPollInterval) {
		pod, err := pods.Get(ctx, ref, metav1.GetOptions{})
		if err != nil {
			if isNotFound(err) {
				return
			}
			if errs++; errs >= execWaitMaxProbeErrors {
				return
			}
			continue
		}
		errs = 0
		if !execContainerStarted(pod) {
			continue
		}
		rc, err := pods.GetLogs(ref, &corev1.PodLogOptions{Container: execContainerName, Follow: true}).Stream(ctx)
		if err != nil {
			slog.Warn("wardynd: exec output tail: could not follow the agent's log", slog.String("ref", ref), slog.Any("err", err))
			return
		}
		defer rc.Close()
		_, _ = io.Copy(w, rc)
		return
	}
}

// execContainerStarted reports whether the agent exec container has a log to
// read: it is running or has terminated. A Waiting reason that never resolves
// (execNeverStartedReasons) keeps it false; followExecOutput then polls until
// teardown deletes the pod.
func execContainerStarted(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.EphemeralContainerStatuses {
		if cs.Name == execContainerName {
			return cs.State.Running != nil || cs.State.Terminated != nil
		}
	}
	return false
}

// findContainer returns the container named name, if present.
func findContainer(containers []corev1.Container, name string) (corev1.Container, bool) {
	for _, c := range containers {
		if c.Name == name {
			return c, true
		}
	}
	return corev1.Container{}, false
}

// recordCmd wraps argv with the recorder — mirrors docker's recordCmd, but
// simpler: k8s has no RecordingMount config (mounts are impossible on this
// substrate), so delivery is ALWAYS the masked brokered upload, never a
// shared-mount fallback. hostAliases + NO_PROXY + the agent NetworkPolicy
// already permit exactly this route (the agent's only egress peer is the
// proxy on 3128). castDir is a plain "/tmp", but WHAT that path resolves to
// depends on the run's disk budget: unlike docker (one container, one
// filesystem), an ephemeral container inherits no filesystem from the main
// container, so with no budget it writes to its OWN rootfs /tmp (the main
// container's idle-script /tmp/wardyn is not visible here). With disk_mib > 0
// the VolumeMounts copied from the main container carry
// ephemeralScratchVolumes' /tmp emptyDir, making /tmp ONE filesystem shared
// by every container of the SAME run only (an emptyDir lives and dies with
// its pod).
//
// runID mirrors docker's own tolerance for an unresolvable label: recording
// is best-effort, so a missing/corrupt wardyn.run-id label degrades to a
// local-only (never-uploaded) cast rather than failing Exec outright.
//
// The Command IS the bare recorder argv — no shell, no "is wardyn-rec on
// PATH" fallback. Exec calls this only when Config.Record is on; when off,
// Exec runs argv unwrapped and this function is never reached. While Record
// is on, an image that can't honour it must fail closed, not silently
// downgrade to unrecorded, the same fail-closed posture invariant 5
// (runner/substrate's package doc) requires everywhere else. An agent image
// on this substrate MUST ship wardyn-rec (image contract §3); one that
// doesn't fails Exec closed via the normal container-start error path
// (parity with docker). The conformance suite's own agent image
// (deploy/kind/Dockerfile.conformance-agent) exercises this path for real.
func recordCmd(runID uuid.UUID, argv []string) []string {
	uploadURL := ""
	if runID != uuid.Nil {
		uploadURL = fmt.Sprintf("http://wardyn-proxy:%d/wardyn/v1/recordings/%s", runner.ProxyListenPort, runID)
	}
	return runner.RecorderArgv("/tmp", "", uploadURL, runID, argv)
}

// Wait blocks until the ephemeral exec Exec added terminates and returns its
// exit code. Restart-safe by construction: every poll reads live from the
// apiserver, carrying no in-memory exec-id map to lose across a wardynd
// restart (unlike docker's agentExecs map). Errors if Exec was never called for ref.
func (d *Driver) Wait(ctx context.Context, ref string) (int, error) {
	// ponytail: a 1s Get-poll of the pod, not a Watch. A watch.Interface
	// would save apiserver chatter, but polling is a five-line loop against
	// the same kubernetes.Interface every clientset satisfies. Upgrade if
	// poll volume ever shows up as real apiserver load.
	errs := 0
	for {
		pod, err := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(ctx, ref, metav1.GetOptions{})
		switch {
		case err == nil:
			errs = 0
			if !slices.ContainsFunc(pod.Spec.EphemeralContainers, func(ec corev1.EphemeralContainer) bool { return ec.Name == execContainerName }) {
				return 0, fmt.Errorf("k8s: exec wait: no agent exec tracked for ref %q (Exec not called?)", ref)
			}
			if st, done := terminalExecStatus(pod, execContainerName); done {
				if st.ExitCode != nil {
					return *st.ExitCode, nil
				}
				return notFoundExitCode, nil
			}
			for _, cs := range pod.Status.EphemeralContainerStatuses {
				if cs.Name != execContainerName {
					continue
				}
				// Fail closed on a Waiting status the platform will never
				// resolve on its own (terminalWaitingReasons) — the shape a
				// platform admission/policy engine or a missing
				// ephemeral-container feature produces: the container never
				// starts, so polling for Terminated would run out the ctx
				// deadline instead of ever returning. Without this, run.exec
				// still read success and the caller had no way to tell
				// "still starting" from "will never start".
				if w := cs.State.Waiting; w != nil && execNeverStartedReasons[w.Reason] {
					return 0, fmt.Errorf("k8s: exec wait: agent container never started (%s: %s): %w",
						w.Reason, w.Message, runner.ErrExecNeverStarted)
				}
				break
			}
			// Added but not yet terminated (Waiting/Running/no status yet): keep polling.
		case isNotFound(err):
			return notFoundExitCode, nil
		default:
			if errs++; errs >= execWaitMaxProbeErrors {
				return 0, fmt.Errorf("k8s: exec wait: get pod (%d consecutive errors): %w", errs, err)
			}
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("k8s: exec wait: %w", ctx.Err())
		case <-time.After(execWaitPollInterval):
		}
	}
}

// AgentStatus reports the agent's liveness restart-safely: it reads the
// ephemeral container's status straight from the apiserver on every call, so
// there is no in-memory state to lose across a wardynd restart. When
// agentExecID is "" (Exec never ran, or an exec-less path — none exists on
// this substrate today) it falls back to Status, where the pod is the agent.
func (d *Driver) AgentStatus(ctx context.Context, ref, agentExecID string) (runner.Status, error) {
	if agentExecID == "" {
		return d.Status(ctx, ref)
	}
	pod, err := d.clientset.CoreV1().Pods(d.cfg.Namespace).Get(ctx, ref, metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return runner.Status{State: types.RunStopped, Message: "agent exec not found"}, nil
		}
		return runner.Status{}, fmt.Errorf("k8s: agent status: get pod: %w", err)
	}
	if st, done := terminalExecStatus(pod, agentExecID); done {
		return st, nil
	}
	for _, cs := range pod.Status.EphemeralContainerStatuses {
		if cs.Name != agentExecID {
			continue
		}
		if cs.State.Running != nil {
			return runner.Status{State: types.RunRunning}, nil
		}
		if w := cs.State.Waiting; w != nil {
			// Named, not just RunStarting: the one detail an operator has no
			// other way to see — whether the container is still legitimately
			// starting or stuck on a Reason that will never resolve.
			return runner.Status{State: types.RunStarting, Message: waitingMessage(w)}, nil
		}
		return runner.Status{State: types.RunStarting}, nil // no state populated yet
	}
	// Present in Spec but absent from Status is the pre-kubelet window: the
	// apiserver accepted the exec and the kubelet hasn't published its first
	// status yet. Reporting THAT terminal (nil error, so nothing retries)
	// would make sweepRunWatchers/reconcileWatch finalize a healthy
	// just-started run FAILED and tear its sandbox down — avoided here with
	// strictly better evidence available, since the pod Get succeeded. An
	// exec id absent from Spec was never exec'd against this pod; terminal
	// is the right answer there and stays.
	for _, ec := range pod.Spec.EphemeralContainers {
		if ec.Name == agentExecID {
			return runner.Status{State: types.RunStarting, Message: "agent exec accepted; kubelet has not started it yet"}, nil
		}
	}
	return runner.Status{State: types.RunStopped, Message: "agent exec not found"}, nil
}

func terminalExecStatus(pod *corev1.Pod, execID string) (runner.Status, bool) {
	for _, cs := range pod.Status.EphemeralContainerStatuses {
		if cs.Name == execID && cs.State.Terminated != nil {
			code := int(cs.State.Terminated.ExitCode)
			return runner.Status{State: types.RunStopped, ExitCode: &code}, true
		}
	}
	// Eviction or node loss can terminate the pod without an updated exec
	// status. The idle main container's success doesn't prove the task finished.
	switch pod.Status.Phase {
	case corev1.PodFailed:
		return runner.Status{State: types.RunFailed, Message: failureDetail(pod)}, true
	case corev1.PodSucceeded:
		return runner.Status{State: types.RunFailed, Message: "sandbox stopped without an agent exit status"}, true
	default:
		return runner.Status{}, false
	}
}

// execNeverStartedReasons are the Waiting reasons under which the ephemeral
// exec container will never run its process: terminalWaitingReasons minus
// CrashLoopBackOff, because a crash-looping container DID start — its
// process ran and died, a task failure, not ErrExecNeverStarted's "the task
// never ran" (ephemeral containers aren't restarted, so excluded for correctness).
var execNeverStartedReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"CreateContainerError":       true,
	"CreateContainerConfigError": true,
	"InvalidImageName":           true,
	"RunContainerError":          true,
}

// waitingMessage words a Waiting container state for AgentStatus: the reason,
// plus the kubelet's message only when it carries one.
func waitingMessage(w *corev1.ContainerStateWaiting) string {
	if w.Message == "" {
		return "waiting: " + w.Reason
	}
	return "waiting: " + w.Reason + ": " + w.Message
}
