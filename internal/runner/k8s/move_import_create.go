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
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The import's refusals, before anything is created. A move writes only into storage it creates, and re-attaches
// none of a person's drives yet, so a spec that would need either is refused with its reason.
var (
	errImportNotKept        = errors.New("k8s: a moved run lands only as a kept run: keep on, interactive, of an agent that is revived")
	errImportNoWorkDrive    = errors.New("k8s: the moved run's spec names no Kubernetes work drive at " + runner.ScratchWorkPath + " for the import to create")
	errImportSecretEnv      = errors.New("k8s: a moved run carries no secret env")
	errImportClaimExists    = errors.New("k8s: storage for a carried drive already exists at the destination; a move writes only into storage it creates")
	errImportRootNoDrive    = errors.New("k8s: a carried root is on no drive the import creates")
	errImportDriveReattach  = errors.New("k8s: re-attaching a person's drive to a moved run is not supported yet")
	errImportRunInNamespace = errors.New("k8s: the run already has a sandbox in the destination namespace")
)

// importPlan is what the import's checks settled: where the run goes, the drives it creates and writes, and the
// runtime its agent pod takes.
type importPlan struct {
	ns                               string
	carry                            []types.DriveMount
	runtimeClassName, runtimeHandler string
	enforced                         types.ConfinementClass
}

// importPlan checks spec and set before anything is created. Every carried root that the set does not leave out whole
// must be at or under a drive of the spec, each such drive (a carry drive) is a managed claim this import creates and
// whose claim does not exist yet, and the work folder is one of them. A drive that carries no root would be re-attached,
// which a move does not do yet. The run's Service and Sandbox must not exist in the namespace: they are named by the
// run, and the run's teardown would take both sandboxes.
func (k *keepDriver) importPlan(ctx context.Context, spec runner.SandboxSpec, set runner.CarriedSet) (importPlan, error) {
	ns, err := k.createNamespace(spec)
	if err != nil {
		return importPlan{}, err
	}
	if err := k.netPolRefusal(ns); err != nil {
		return importPlan{}, err
	}
	if !k.keepRun(spec) || keepHarnessRefusal(spec.Labels[labelAgent]) != "" {
		if !slices.ContainsFunc(spec.Drives, func(m types.DriveMount) bool { return m.Target == scratchWorkPath }) {
			return importPlan{}, errImportNoWorkDrive
		}
		return importPlan{}, errImportNotKept
	}
	if len(spec.SecretEnv) > 0 {
		return importPlan{}, errImportSecretEnv
	}
	carry, err := carryDrives(spec.Drives, set)
	if err != nil {
		return importPlan{}, err
	}
	for _, d := range carry {
		_, err := k.clientset.CoreV1().PersistentVolumeClaims(ns).Get(ctx, d.ObjectName, metav1.GetOptions{})
		switch {
		case err == nil:
			return importPlan{}, fmt.Errorf("%w: drive %q", errImportClaimExists, d.DriveName)
		case !isNotFound(err):
			return importPlan{}, fmt.Errorf("k8s: look up the storage of drive %q: %w", d.DriveName, err)
		}
	}
	if _, err := k.clientset.CoreV1().Services(ns).Get(ctx, keepServiceName(spec.RunID), metav1.GetOptions{}); err == nil {
		return importPlan{}, errImportRunInNamespace
	} else if !isNotFound(err) {
		return importPlan{}, fmt.Errorf("k8s: look up the run's Service: %w", err)
	}
	if _, err := k.dynamic.Resource(sandboxesGVR).Namespace(ns).Get(ctx, agentPodName(spec.RunID), metav1.GetOptions{}); err == nil {
		return importPlan{}, errImportRunInNamespace
	} else if !isNotFound(err) {
		return importPlan{}, fmt.Errorf("k8s: look up the run's Sandbox: %w", err)
	}
	rcName, handler, enforced, err := k.createPreflight(ctx, spec)
	if err != nil {
		return importPlan{}, err
	}
	return importPlan{ns: ns, carry: carry, runtimeClassName: rcName, runtimeHandler: handler, enforced: enforced}, nil
}

// carryDrives is the drives set's roots are written into: for each root the set does not leave out whole, the drive
// whose target is the deepest at or above it. The work drive must be one. Each is a managed, writable claim. Any
// other drive of the spec refuses: a move does not re-attach a person's drive yet. That is where re-attaching would
// begin: such a drive would go through ensureDrivePVC, as a create's does, and never be mounted in the helper.
func carryDrives(drives []types.DriveMount, set runner.CarriedSet) ([]types.DriveMount, error) {
	carry := map[int]bool{}
	for _, root := range set.Roots {
		if movepack.Excluded(strings.TrimPrefix(root, "/"), set.Exclude) {
			continue
		}
		best := -1
		for i, d := range drives {
			if atOrUnderAny(root, []string{d.Target}) && (best < 0 || len(d.Target) > len(drives[best].Target)) {
				best = i
			}
		}
		if best < 0 {
			return nil, fmt.Errorf("%w: %s", errImportRootNoDrive, root)
		}
		carry[best] = true
	}
	var out []types.DriveMount
	work := false
	for i, d := range drives {
		if !carry[i] {
			return nil, fmt.Errorf("%w: drive %q", errImportDriveReattach, d.DriveName)
		}
		if d.Backend != types.DriveBackendK8sPVC || d.ReadOnly || d.Team {
			return nil, fmt.Errorf("k8s: drive %q is not a writable managed drive, so the import cannot create it", d.DriveName)
		}
		if err := validateDriveMount(&d); err != nil {
			return nil, err
		}
		work = work || d.Target == scratchWorkPath
		out = append(out, d)
	}
	if !work {
		return nil, errImportNoWorkDrive
	}
	return out, nil
}

// importInto creates the run held and never started, as importPlan settled it: both policies (the proxy's is the
// deny-all hold), the carry claims, the files through the helper, then the Secret, Service, empty EndpointSlice and
// the gated Sandbox, whose UID must be recorded and read back. A failure after the first object tears the run down;
// the claims stay, since no run label is on them and a move's own drive rows own them.
func (k *keepDriver) importInto(ctx context.Context, spec runner.SandboxSpec, set runner.CarriedSet, maxBytes int64, plan importPlan, rd *movepack.Reader) (runner.Sandbox, runner.CarriedManifest, error) {
	none := runner.CarriedManifest{}
	ns, runID := plan.ns, spec.RunID
	k.noteNamespace(runID, ns)
	fail := func(kind, err error) (runner.Sandbox, runner.CarriedManifest, error) {
		zero := int64(0)
		tctx, cancel := context.WithTimeout(context.Background(), moveHelperCleanup)
		defer cancel()
		if terr := k.teardownByRunID(tctx, runID, &zero); terr != nil {
			slog.Error("wardynd: k8s substrate: a failed move import could not tear down what it created",
				slog.String("run_id", runID.String()), slog.String("import_err", err.Error()), slog.String("teardown_err", terr.Error()))
		}
		return runner.Sandbox{}, none, fmt.Errorf("%w: %w", kind, err)
	}
	objLabels := func(component string) map[string]string {
		return k.stampRunner(wardynLabels(runID, component, spec.Labels))
	}
	if err := k.createImportPolicies(ctx, ns, runID, objLabels); err != nil {
		return fail(runner.ErrMoveCreate, err)
	}
	claims := make([]helperClaim, 0, len(plan.carry))
	for i := range plan.carry {
		if err := k.createCarryClaim(ctx, ns, &plan.carry[i]); err != nil {
			return fail(runner.ErrMoveCreate, err)
		}
		claims = append(claims, helperClaim{claim: plan.carry[i].ObjectName, target: plan.carry[i].Target})
	}
	m, err := k.importFiles(ctx, moveHelper{ns: ns, runID: runID, claims: claims}, set, maxBytes, rd)
	if err != nil {
		return fail(runner.ErrMoveFiles, err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName(runID), Namespace: ns, Labels: objLabels(componentProxy)},
		Type:       corev1.SecretTypeOpaque,
		Data:       managedFileSecretData(spec.ManagedFiles),
	}
	if _, err := k.clientset.CoreV1().Secrets(ns).Create(ctx, sec, metav1.CreateOptions{}); err != nil {
		return fail(runner.ErrMoveCreate, fmt.Errorf("k8s: create the run Secret: %w", err))
	}
	svc, err := k.createKeepService(ctx, ns, runID, objLabels(componentProxy))
	if err != nil {
		return fail(runner.ErrMoveCreate, err)
	}
	if err := k.writeKeepEndpoints(ctx, ns, runID, svc, ""); err != nil {
		return fail(runner.ErrMoveCreate, err)
	}
	if err := k.createImportedSandbox(ctx, spec, plan, svc); err != nil {
		return fail(runner.ErrMoveCreate, err)
	}
	return podSandbox(spec, plan.enforced, plan.runtimeHandler, "", svc.Spec.ClusterIP), m, nil
}

// createImportPolicies creates the agent's policy as a create does, and the deny-all hold on the run's proxy pods, with
// the labels the real proxy policy would carry. Both exist before the run Secret.
func (k *keepDriver) createImportPolicies(ctx context.Context, ns string, runID uuid.UUID, objLabels func(string) map[string]string) error {
	agentLabels := wardynLabels(runID, componentAgent, nil)
	proxyLabels := wardynLabels(runID, componentProxy, nil)
	both := []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}
	agent := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: agentNetPolName(runID), Namespace: ns, Labels: objLabels(componentAgent)},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: agentLabels},
			PolicyTypes: both,
			Ingress:     []networkingv1.NetworkPolicyIngressRule{},
			Egress: []networkingv1.NetworkPolicyEgressRule{{
				Ports: agentToProxyPorts(),
				To:    []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: proxyLabels}}},
			}},
		},
	}
	hold := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: proxyHoldNetPolName(runID), Namespace: ns, Labels: objLabels(componentProxy)},
		Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: proxyLabels}, PolicyTypes: both},
	}
	for _, pol := range []*networkingv1.NetworkPolicy{agent, hold} {
		if _, err := k.clientset.NetworkingV1().NetworkPolicies(ns).Create(ctx, pol, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("k8s: create NetworkPolicy %s: %w", pol.Name, err)
		}
	}
	return nil
}

// createCarryClaim creates drive's claim as a create provisions a managed one, and never reuses one: a claim that
// appeared since the import's check refuses.
func (k *keepDriver) createCarryClaim(ctx context.Context, ns string, drive *types.DriveMount) error {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      drive.ObjectName,
			Namespace: ns,
			Labels: map[string]string{
				labelManaged:      "true",
				labelDrive:        drive.DriveID.String(),
				labelDriveHome:    drive.HomeName,
				labelDriveSubject: drive.SubjectHash,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(driveRequestBytes(drive), resource.BinarySI)},
			},
		},
	}
	if drive.StorageClass != "" {
		pvc.Spec.StorageClassName = &drive.StorageClass
	}
	_, err := k.clientset.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
	switch {
	case apierrors.IsAlreadyExists(err):
		return fmt.Errorf("%w: drive %q", errImportClaimExists, drive.DriveName)
	case err != nil:
		return fmt.Errorf("k8s: create the storage of drive %q: %w", drive.DriveName, err)
	}
	return nil
}

// importFiles runs the helper's import with rd's entries re-emitted on its stdin, so nothing reaches the helper that
// rd has not passed, and answers the manifest rd read.
func (k *keepDriver) importFiles(ctx context.Context, h moveHelper, set runner.CarriedSet, maxBytes int64, rd *movepack.Reader) (runner.CarriedManifest, error) {
	setJSON, err := json.Marshal(set)
	if err != nil {
		return runner.CarriedManifest{}, err
	}
	targets := make([]string, 0, len(h.claims))
	for _, c := range h.claims {
		targets = append(targets, c.target)
	}
	mountsJSON, err := json.Marshal(targets)
	if err != nil {
		return runner.CarriedManifest{}, err
	}
	argv := []string{proxyBinary, "-move-import", "-move-set", string(setJSON), "-move-max-bytes", strconv.FormatInt(maxBytes, 10), "-move-mounts", string(mountsJSON)}
	type copied struct {
		m   movepack.Manifest
		err error
	}
	pr, pw := io.Pipe()
	done := make(chan copied, 1)
	go func() {
		var c copied
		var w *movepack.Writer
		if w, c.err = movepack.NewWriter(pw, set); c.err == nil {
			c.m, c.err = movepack.Copy(w, rd)
		}
		pw.CloseWithError(c.err)
		done <- c
	}()
	err = k.runMoveHelper(ctx, h, argv, pr, io.Discard)
	// Ends the copy if the helper stopped reading first.
	_ = pr.CloseWithError(errors.New("k8s: the move helper ended"))
	c := <-done
	// Both are kept: the helper's, and the stream's refusal, which may be why the helper stopped.
	if err = errors.Join(err, c.err); err != nil {
		return runner.CarriedManifest{}, err
	}
	return c.m, nil
}

// createImportedSandbox builds the agent pod as a create does, with the run's Service address pinned, and creates its
// Sandbox gated. It is never admitted: the revive that starts the run removes the gate. The Sandbox's UID is recorded
// and read back, since a kept run whose UID is not recorded can never be revived.
func (k *keepDriver) createImportedSandbox(ctx context.Context, spec runner.SandboxSpec, plan importPlan, svc *corev1.Service) error {
	pod, err := k.importedAgentPod(spec, plan, svc)
	if err != nil {
		return err
	}
	pod, err = k.templatedAgentPod(ctx, pod)
	if err != nil {
		return err
	}
	sb, err := agentSandboxObject(pod)
	if err != nil {
		return err
	}
	created, err := k.dynamic.Resource(sandboxesGVR).Namespace(plan.ns).Create(ctx, sb, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("k8s: create the agent's Sandbox: %w", err)
	}
	k.recordKeptSandbox(ctx, pod, created.GetUID())
	if err := k.checkKeptSandbox(ctx, plan.ns, spec.RunID, created.GetUID()); err != nil {
		return fmt.Errorf("k8s: the moved run's Sandbox was not recorded, so it could not be revived: %w", err)
	}
	return nil
}

// importedAgentPod is CreateSandbox's agent pod for a moved run, owned by the run's Service and pinned to its address.
// It is built as a create builds it; a create's warm node and claim re-check do not apply to a pod that is never
// started here.
func (k *keepDriver) importedAgentPod(spec runner.SandboxSpec, plan importPlan, svc *corev1.Service) (*corev1.Pod, error) {
	proxyAddr := svc.Spec.ClusterIP
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: agentPodName(spec.RunID), Namespace: plan.ns},
		Spec: corev1.PodSpec{
			ActiveDeadlineSeconds:        k.podDeadline(spec.Resources.LifetimeCap),
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolPtr(false),
			EnableServiceLinks:           boolPtr(false),
			HostAliases:                  []corev1.HostAlias{{IP: proxyAddr, Hostnames: runner.ProxyHostnames(spec.Env)}},
			DNSPolicy:                    corev1.DNSNone,
			DNSConfig:                    &corev1.PodDNSConfig{Nameservers: []string{proxyAddr}},
			Containers: []corev1.Container{{
				Name:            mainContainerName,
				Image:           spec.Image,
				Command:         k.interactiveIdleCmd(spec),
				Env:             envVars(k.withRunnerTrustedCA(spec.Env)),
				SecurityContext: agentSecurityContext(),
				Resources:       resourceRequirements(spec.Resources),
			}},
		},
	}
	scratchVols, scratchMounts := ephemeralScratchVolumes(spec.Resources.DiskMiB, true)
	addMainContainerVolumes(pod, scratchVols, scratchMounts)
	managedVols, managedMounts := managedFileVolumes(spec.RunID, spec.ManagedFiles)
	addMainContainerVolumes(pod, managedVols, managedMounts)
	secretVols, stage, secretMount, wipe := agentSecretVolumes(spec.RunID, spec.ManagedFiles, spec.Image, resourceRequirements(spec.Resources))
	if stage != nil {
		pod.Spec.Volumes = append(pod.Spec.Volumes, secretVols...)
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, *stage)
		main := &pod.Spec.Containers[0]
		main.VolumeMounts = append(main.VolumeMounts, secretMount)
		main.Command = wipeAfterReadCommand(main.Command, wipe)
	}
	applyAgentSecretFSGroup(pod, spec.ManagedFiles)
	for i := range spec.Drives {
		applyDriveToPod(pod, &spec.Drives[i], i, plan.runtimeHandler)
	}
	if plan.runtimeClassName != "" {
		pod.Spec.RuntimeClassName = &plan.runtimeClassName
	}
	if err := k.placeAgentPod(pod, spec); err != nil {
		return nil, err
	}
	pod.Labels[labelRecord] = strconv.FormatBool(spec.RecordWithin(k.cfg.Record))
	if err := k.runnerPodSpec(pod, componentAgent, ""); err != nil {
		return nil, err
	}
	keepAgent(pod, svc)
	return pod, nil
}
