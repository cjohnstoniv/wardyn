// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"path"
	"strconv"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
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
// WHY A DIRECTORY MOUNT AND NOT subPath. A subPath would drop one file into an
// existing directory and leave its siblings alone, which reads like the
// tidier answer. It is unusable here: the apiserver forbids subPath on an
// EPHEMERAL container, and Exec copies the main container's VolumeMounts
// verbatim onto the ephemeral container the agent actually runs in — so a
// subPath here would make UpdateEphemeralContainers fail for every run that
// carries a managed file, while a fake-clientset test went on passing. That is
// the same constraint ephemeralScratchVolumes records, and
// TestCreateSandbox_NoMountCarriesASubPath is the shared pin.
//
// WHAT MAKES THE FILE IMMUTABLE. A Secret volume is a kubelet-managed tmpfs:
// its files are root-owned at the mode the item names, and ReadOnly on the
// mount makes the whole tree refuse a write with EROFS whatever the mode says.
// The DIRECTORY is a mount point, so the agent cannot rename it away and put
// its own there instead — which is the half a root-owned file alone does not
// give you. runner.ValidateManagedFiles' two-deep path rule is what keeps that
// mount point off a top-level directory the image needs.
func managedFileVolumes(runID uuid.UUID, files []runner.ManagedFile) ([]corev1.Volume, []corev1.VolumeMount) {
	if len(files) == 0 {
		return nil, nil
	}
	items := map[string][]corev1.KeyToPath{}
	for i, f := range files {
		mode := int32(f.FileMode().Perm())
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
