// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// Only the keep driver moves a run: a kept run is the only one whose files outlive its pod, and the only one a revive
// can start again. Neither driver is a runner.MoveImportPresenter: an imported pod is held and reads stopped.
var (
	_ runner.StateExporter = (*keepDriver)(nil)
	_ runner.StateImporter = (*keepDriver)(nil)
)

// componentMoveHelper is the wardyn.component of a move's helper pod and its policy.
const componentMoveHelper = "move-helper"

// proxyHoldNetPolName is the deny-all policy an import puts on the run's proxy pods until the revive that starts the run
// creates the real one from its own config.
func proxyHoldNetPolName(runID uuid.UUID) string { return "wardyn-proxy-hold-" + runID.String() }

// proxyPolicies is the run's proxy policies a replace found: the real one, and the import's hold.
type proxyPolicies struct {
	canonical, hold *networkingv1.NetworkPolicy
}

// settleProxyPolicy gives a moved run's proxy its policy, before any Secret or pod of the replace. While only the
// import's hold exists, it creates the real policy with the hold's labels and egress, the egress the revive's own config
// asks for, and refuses when a proxy pod is already there. A create that fails leaves the hold, so a retry starts
// clean. Then the hold goes, under its UID; a delete that fails is logged only, as a deny-all policy beside the real one
// narrows nothing, and the run's teardown deletes it with the rest of its policies.
func (k *keepDriver) settleProxyPolicy(ctx context.Context, ns string, runID uuid.UUID, egress []networkingv1.NetworkPolicyEgressRule, pols proxyPolicies, old *corev1.Pod) error {
	if pols.hold == nil {
		return nil
	}
	if pols.canonical == nil {
		if old != nil {
			return fmt.Errorf("%w: the run's proxy policy is still the move's hold, yet a proxy pod is there", runner.ErrProxyReplaceFailed)
		}
		pol := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: proxyNetPolName(runID), Namespace: ns, Labels: maps.Clone(pols.hold.Labels)},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: wardynLabels(runID, componentProxy, nil)},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: wardynLabels(runID, componentAgent, nil)}}},
				}},
				Egress: egress,
			},
		}
		if _, err := k.clientset.NetworkingV1().NetworkPolicies(ns).Create(ctx, pol, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("k8s: keep: create the moved run's proxy NetworkPolicy: %w", err)
		}
	}
	uid := pols.hold.UID
	err := k.clientset.NetworkingV1().NetworkPolicies(ns).Delete(ctx, pols.hold.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
	if err != nil && !isNotFound(err) {
		slog.Warn("wardynd: k8s substrate: could not delete a moved run's proxy hold policy; the run's teardown removes it",
			slog.String("namespace", ns), slog.String("run_id", runID.String()), slog.String("error", err.Error()))
	}
	return nil
}

// ExportState reads set's files out of ref, a kept run, through a helper pod that mounts the run's kept claims under
// the carried roots read-only. The stream is the helper's output; it ends cleanly only after the helper exited 0 and
// the run's Sandbox reads kept and the same as at the start, so a short stream is always an error. The driver keeps no
// spool, so the export answers SpoolDrained.
func (k *keepDriver) ExportState(ctx context.Context, ref string, set runner.CarriedSet) (runner.StateExport, error) {
	runID, ns, err := k.refRun(ctx, ref)
	if err != nil {
		return runner.StateExport{}, err
	}
	uid, tmpl, err := k.exportable(ctx, ns, runID)
	if err != nil {
		return runner.StateExport{}, err
	}
	claims := carriedClaims(tmpl, set.Roots)
	if len(claims) == 0 {
		return runner.StateExport{}, fmt.Errorf("k8s: export %s: no kept claim is mounted at or under a carried root", ref)
	}
	if root, ok := rootWithoutClaim(claims, set.Roots); ok {
		return runner.StateExport{}, fmt.Errorf("k8s: export %s: no kept claim is mounted at or under the carried root %s", ref, root)
	}
	setJSON, err := json.Marshal(set)
	if err != nil {
		return runner.StateExport{}, err
	}
	h := moveHelper{ns: ns, runID: runID, claims: claims, readOnly: true}
	argv := []string{proxyBinary, "-move-export", "-move-set", string(setJSON)}
	pr, pw := io.Pipe()
	go func() {
		err := k.runMoveHelper(ctx, h, argv, nil, pw)
		if err == nil {
			err = k.stillExportable(ctx, ns, runID, uid)
		}
		pw.CloseWithError(err)
	}()
	return runner.StateExport{Stream: pr, SpoolDrained: true}, nil
}

// exportable answers the Sandbox UID and the pod template of a run whose files a move carries: kept, its agent pod held
// or gone, and no proxy pod.
func (k *keepDriver) exportable(ctx context.Context, ns string, runID uuid.UUID) (k8stypes.UID, *corev1.Pod, error) {
	sb, err := k.keptSandbox(ctx, ns, runID)
	if err != nil {
		return "", nil, fmt.Errorf("k8s: export: the run is not kept: %w", err)
	}
	pod, err := k.clientset.CoreV1().Pods(ns).Get(ctx, agentPodName(runID), metav1.GetOptions{})
	switch {
	case err != nil && !isNotFound(err):
		return "", nil, fmt.Errorf("k8s: export: read the agent pod: %w", err)
	case err == nil && !agentPodGated(pod):
		return "", nil, errors.New("k8s: export: the agent pod is not held, so the sandbox is not stopped")
	}
	if _, err := k.clientset.CoreV1().Pods(ns).Get(ctx, proxyPodName(runID), metav1.GetOptions{}); err == nil {
		return "", nil, errors.New("k8s: export: the run's proxy pod is still there, so the sandbox is not stopped")
	} else if !isNotFound(err) {
		return "", nil, fmt.Errorf("k8s: export: read the proxy pod: %w", err)
	}
	tmpl, err := podFromSandboxTemplate(sb)
	if err != nil {
		return "", nil, err
	}
	return sb.GetUID(), tmpl, nil
}

// stillExportable fails an export whose run changed under it: no longer kept or stopped, or a Sandbox made again.
func (k *keepDriver) stillExportable(ctx context.Context, ns string, runID uuid.UUID, uid k8stypes.UID) error {
	now, _, err := k.exportable(ctx, ns, runID)
	switch {
	case err != nil:
		return fmt.Errorf("k8s: export: the sandbox changed during the export: %w", err)
	case now != uid:
		return errors.New("k8s: export: the run's Sandbox was replaced during the export")
	}
	return nil
}

// carriedClaims is the claims of tmpl's main container mounted at or under one of roots, with where they mount.
func carriedClaims(tmpl *corev1.Pod, roots []string) []helperClaim {
	main, ok := findContainer(tmpl.Spec.Containers, mainContainerName)
	if !ok {
		return nil
	}
	var out []helperClaim
	for _, v := range tmpl.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		for _, m := range main.VolumeMounts {
			if m.Name == v.Name && atOrUnderAny(m.MountPath, roots) {
				out = append(out, helperClaim{claim: v.PersistentVolumeClaim.ClaimName, target: m.MountPath})
			}
		}
	}
	return out
}

// rootWithoutClaim answers the first root with no claim mounted at or under it: its files live only in the agent
// pod's own filesystem, which the helper cannot read.
func rootWithoutClaim(claims []helperClaim, roots []string) (string, bool) {
	for _, r := range roots {
		if !slices.ContainsFunc(claims, func(c helperClaim) bool { return atOrUnderAny(c.target, []string{r}) }) {
			return r, true
		}
	}
	return "", false
}

// atOrUnderAny reports whether p, an absolute path, is one of roots or beneath one.
func atOrUnderAny(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, strings.TrimSuffix(r, "/")+"/") {
			return true
		}
	}
	return false
}

// ImportSandbox creates spec's run as a kept run that is held and has never started: its claims, policies, Secret,
// Service and Sandbox, and no proxy pod. The carried files are written by a helper pod into claims this import
// creates, and only then are the Secret, Service and Sandbox made, so a refused stream leaves no agent object. The
// stream is read here before any of it reaches the helper, which reads it again. Before anything is created the import
// refuses a spec it would have to write a claim that already exists for, or re-attach a drive for, and a run that
// already has a sandbox in the namespace. No booted marker is written: the carried work folder holds the run's own.
func (k *keepDriver) ImportSandbox(ctx context.Context, spec runner.SandboxSpec, set runner.CarriedSet, maxBytes int64, carried io.Reader) (runner.Sandbox, runner.CarriedManifest, error) {
	none := runner.CarriedManifest{}
	refuse := func(err error) (runner.Sandbox, runner.CarriedManifest, error) {
		return runner.Sandbox{}, none, fmt.Errorf("%w: %w", runner.ErrMoveCreate, err)
	}
	if err := runner.CheckImportSpec(spec); err != nil {
		return refuse(err)
	}
	if spec.MachineKind.Normalize() != "" {
		return refuse(fmt.Errorf("k8s: a moved run's sandbox is a container, never a %s", spec.MachineKind))
	}
	if err := runner.CheckCarriedLimit(maxBytes); err != nil {
		return refuse(err)
	}
	rd, err := movepack.NewReader(carried, movepack.Options{Set: set, Limits: movepack.Limits{MaxBytes: maxBytes}})
	if err != nil {
		return refuse(err)
	}
	defer rd.Close()
	plan, err := k.importPlan(ctx, spec, set)
	if err != nil {
		return refuse(err)
	}
	return k.importInto(ctx, spec, set, maxBytes, plan, rd)
}
