// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"strconv"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

const (
	agentSecretsSourceVolume = "wardyn-secrets-src"
	agentSecretsVolume       = "wardyn-secrets"
	agentSecretsSourceDir    = "/run/wardyn/secrets-src"
	stageAgentSecretsName    = "stage-secrets"
)

// stageAgentSecretsScript copies each named file (its arguments) from the
// Secret volume onto the tmpfs the agent mounts, owner-only.
const stageAgentSecretsScript = `umask 077
for f; do cp "` + agentSecretsSourceDir + `/$f" "` + runner.ComponentSecretDir + `/$f" && chmod 0400 "` + runner.ComponentSecretDir + `/$f" || exit 1; done`

// wipeAfterReadScript deletes each named file once it has been read, then
// execs the container's own command. Arguments: the directory, the count of
// names, the names, the command. A read is an access time newer than the
// file's write: tmpfs is relatime, so the first read after the copy moves it.
// ponytail: a 1 s poll inside the sandbox, which the agent's own uid could
// stop; it shortens how long a file sits there, it is not a boundary.
const wipeAfterReadScript = `d=$1; n=$2; shift 2; w=""
while [ "$n" -gt 0 ]; do w="$w $1"; shift; n=$((n - 1)); done
(while :; do
  left=""
  for f in $w; do
    p="$d/$f"; [ -e "$p" ] || continue
    t=$(stat -c '%x|%y' "$p" 2>/dev/null) || { left=1; continue; }
    if expr "${t%%|*}" \> "${t#*|}" >/dev/null; then rm -f "$p"; else left=1; fi
  done
  [ -n "$left" ] || exit 0
  sleep 1
done) &
exec "$@"`

// agentSecretVolumes stages the AgentOwned files onto an in-memory emptyDir at
// runner.ComponentSecretDir, owned by the agent's uid at 0400, so no other
// member of the pod's fsGroup can read them. The Secret volume they come from
// is mounted by the init container alone. wipe names the files to delete after
// their first read (ManagedFile.WipeAfterRead). No AgentOwned file: all zero.
func agentSecretVolumes(runID uuid.UUID, files []runner.ManagedFile, image string, res corev1.ResourceRequirements) (vols []corev1.Volume, stage *corev1.Container, mount corev1.VolumeMount, wipe []string) {
	var items []corev1.KeyToPath
	var names []string
	mode := agentSecretItemMode
	for i, f := range files {
		if !f.AgentOwned {
			continue
		}
		name := f.Path[len(runner.ComponentSecretDir)+1:]
		items = append(items, corev1.KeyToPath{Key: managedFileDataKey(i), Path: name, Mode: &mode})
		names = append(names, name)
		if f.WipeAfterRead {
			wipe = append(wipe, name)
		}
	}
	if len(items) == 0 {
		return nil, nil, corev1.VolumeMount{}, nil
	}
	vols = []corev1.Volume{
		{Name: agentSecretsSourceVolume, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secretName(runID), Items: items}}},
		{Name: agentSecretsVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
	}
	stage = &corev1.Container{
		Name:    stageAgentSecretsName,
		Image:   image,
		Command: append([]string{"sh", "-c", stageAgentSecretsScript, stageAgentSecretsName}, names...),
		VolumeMounts: []corev1.VolumeMount{
			{Name: agentSecretsSourceVolume, MountPath: agentSecretsSourceDir, ReadOnly: true},
			{Name: agentSecretsVolume, MountPath: runner.ComponentSecretDir},
		},
		SecurityContext:          agentSecurityContext(),
		Resources:                res,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
	// Writable only when a file is to be deleted from it: the agent owns the
	// files either way.
	mount = corev1.VolumeMount{Name: agentSecretsVolume, MountPath: runner.ComponentSecretDir, ReadOnly: len(wipe) == 0}
	return vols, stage, mount, wipe
}

// wipeAfterReadCommand wraps cmd so the named files under
// runner.ComponentSecretDir are deleted after their first read.
func wipeAfterReadCommand(cmd, wipe []string) []string {
	if len(wipe) == 0 {
		return cmd
	}
	out := append([]string{"sh", "-c", wipeAfterReadScript, "wardyn-agent", runner.ComponentSecretDir, strconv.Itoa(len(wipe))}, wipe...)
	return append(out, cmd...)
}
