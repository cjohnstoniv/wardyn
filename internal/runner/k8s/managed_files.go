// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"errors"
	"path"
	"slices"
	"strconv"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// managedFileDataKey names managed file i's content inside the per-run Secret,
// beside the proxy config and the `env.` keys — one object, swept by the same
// run-id label, for the reason stated where SecretEnv joins it.
func managedFileDataKey(i int) string { return "managed." + strconv.Itoa(i) }

// managedFileVolumeName names the Secret volume that carries managed-file
// directory i. DNS-1123 by construction (the index is the only variable part).
func managedFileVolumeName(i int) string { return "wardyn-managed-" + strconv.Itoa(i) }

// managedFileSecretData returns the Secret entries for files, keyed by
// managedFileDataKey and indexed the same way managedFileVolumes indexes them —
// the two are built from the same slice in the same order, which is what keeps
// a volume item pointing at the content it names.
func managedFileSecretData(files []runner.ManagedFile) map[string][]byte {
	if len(files) == 0 {
		return nil
	}
	data := make(map[string][]byte, len(files))
	for i, f := range files {
		data[managedFileDataKey(i)] = f.Content
	}
	return data
}

// managedFileVolumes builds one READ-ONLY Secret volume per distinct parent
// directory, each projecting exactly the keys that belong in it via explicit
// items — never the whole Secret, which also holds the proxy config and every
// credential-bearing env value.
//
// A directory mount, not subPath: the apiserver forbids subPath on an
// EPHEMERAL container, and Exec copies the main container's VolumeMounts
// verbatim onto it, so subPath here would fail UpdateEphemeralContainers for
// every run with a managed file (TestCreateSandbox_NoMountCarriesASubPath
// pins this, same constraint as ephemeralScratchVolumes).
//
// Immutability: the Secret volume is a kubelet-managed tmpfs (root-owned,
// ReadOnly makes writes EROFS regardless of mode), and mounting the
// DIRECTORY (not just the file) stops the agent renaming it away and
// substituting its own. runner.ValidateManagedFiles confines the mount
// points to runner.ManagedFileDir and runner.ComponentSecretDir, so one
// never hides a directory the image needs.
//
// An AgentOwned file (a delivered secret) is projected at agentSecretItemMode
// and read through the pod's fsGroup (applyAgentSecretFSGroup).
func managedFileVolumes(runID uuid.UUID, files []runner.ManagedFile) ([]corev1.Volume, []corev1.VolumeMount) {
	if len(files) == 0 {
		return nil, nil
	}
	items := map[string][]corev1.KeyToPath{}
	for i, f := range files {
		mode := int32(f.FileMode().Perm())
		if f.AgentOwned {
			mode = agentSecretItemMode
		}
		dir := path.Dir(f.Path)
		items[dir] = append(items[dir], corev1.KeyToPath{
			Key:  managedFileDataKey(i),
			Path: path.Base(f.Path),
			Mode: &mode,
		})
	}
	dirs := runner.ManagedFileDirs(files) // sorted: a deterministic pod spec
	vols := make([]corev1.Volume, 0, len(dirs))
	mounts := make([]corev1.VolumeMount, 0, len(dirs))
	for i, dir := range dirs {
		name := managedFileVolumeName(i)
		vols = append(vols, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: secretName(runID),
				Items:      items[dir],
			}},
		})
		// No SubPath — see the doc comment. ReadOnly is the belt to the
		// tmpfs's own braces.
		mounts = append(mounts, corev1.VolumeMount{Name: name, MountPath: dir, ReadOnly: true})
	}
	return vols, mounts
}

// agentFSGroup is the agent's gid by the image contract — every Wardyn agent
// image runs as 1000, the value driveFSGroup also carries, so a pod with both
// a managed drive and a delivered secret asks for one fsGroup, not two.
const agentFSGroup int64 = 1000

// agentSecretItemMode is what an AgentOwned file is projected at. A Secret
// volume cannot set a file's OWNER (always root), so runner's 0400 would leave
// the agent unable to read its own secret; 0440 with the pod's fsGroup as the
// file's group is the nearest this substrate comes to "the agent's user only".
//
// Stated plainly, since the mode reads narrower than it is: fsGroup is a
// supplementary group of EVERY container in the pod, so the reach is every
// process in the agent pod. Those all run as uid 1000 (agentSecurityContext on
// the main container and on each ephemeral exec container, RunAsNonRoot, no
// capabilities, no privilege escalation), so no second uid is there to read
// it. The proxy is a separate pod and never mounts this volume.
const agentSecretItemMode = int32(0o440)

// errAgentSecretBesideShare refuses a delivered secret on a pod that mounts a
// SHARE drive: a secret needs the pod's fsGroup, and applyDriveToPod explains
// why a share must never get one — it would re-own other people's files.
var errAgentSecretBesideShare = errors.New("a secret delivered as a file needs the pod's fsGroup, which is never set beside a shared drive (it would re-own other people's files on it); run without the shared drive, or deliver the secret another way")

// agentSecretsAllowDrive is CreateSandbox's preflight half of
// applyAgentSecretFSGroup: nil unless files carry a delivered secret AND the
// run mounts a drive that is not a managed one.
func agentSecretsAllowDrive(files []runner.ManagedFile, drive *types.DriveMount) error {
	if drive == nil || drive.Backend.Kind() == types.DriveKindManaged || !hasAgentSecret(files) {
		return nil
	}
	return errAgentSecretBesideShare
}

// applyAgentSecretFSGroup sets the agent pod's fsGroup to agentFSGroup when
// files carry a delivered secret, so the kubelet group-owns the projected
// file and the agent can read it. A pod with none keeps its SecurityContext
// exactly as it was. Set, never assigned over: applyDriveToPod writes the same
// value for a managed drive.
func applyAgentSecretFSGroup(pod *corev1.Pod, files []runner.ManagedFile) {
	if !hasAgentSecret(files) {
		return
	}
	if pod.Spec.SecurityContext == nil {
		pod.Spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	pod.Spec.SecurityContext.FSGroup = int64Ptr(agentFSGroup)
}

func hasAgentSecret(files []runner.ManagedFile) bool {
	return slices.ContainsFunc(files, func(f runner.ManagedFile) bool { return f.AgentOwned })
}
