// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package conformance_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

// k8sDriveFixture is the UserDrives case's kubernetes half. This substrate's
// ceiling is not a list of host roots: it binds ONLY a claim in the run's
// namespace, and never a host path. So:
//
//   - the allowed drive is a MANAGED k8s_pvc the driver provisions itself on
//     the cluster's default StorageClass — the real PVC, bound and mounted;
//   - the refused drive is a host_path share, the docker shape. Its object
//     name is a legal claim name and it carries a size on purpose, so the
//     backend check is the ONE thing refusing it: without that check the
//     driver would provision a claim under the name and mount it, which turns
//     the sub-case red.
//
// Lock cannot use root: the run namespace is PSS-restricted. The locked
// directory is made by uid 2000 instead, which for the agent's uid 1000 is the
// same question (a 0700 directory it does not own).
func k8sDriveFixture(t *testing.T, cs kubernetes.Interface, ns, image string) *conformance.UserDriveFixture {
	t.Helper()
	suffix := uuid.NewString()[:8]
	claim := "wardyn-conformance-drive-" + suffix
	hostPathName := "wardyn-conformance-hostpath-" + suffix
	// Both names: the refused one only exists if the refusal regressed. The
	// driver never deletes a drive's claim (it outlives every run), so the
	// fixture does; registered before any sandbox, it runs after their stops.
	t.Cleanup(func() {
		for _, n := range []string{claim, hostPathName} {
			_ = cs.CoreV1().PersistentVolumeClaims(ns).Delete(context.Background(), n, metav1.DeleteOptions{})
		}
	})
	home := "conf-" + suffix
	return &conformance.UserDriveFixture{
		Mount: types.DriveMount{
			DriveID:    uuid.New(),
			Backend:    types.DriveBackendK8sPVC,
			ObjectName: claim,
			DriveName:  "conformance-managed",
			HomeName:   home,
			Target:     runner.DriveTarget,
			SizeMiB:    64,
		},
		Lock: func(t *testing.T) { lockDriveAs2000(t, cs, ns, image, claim) },
		Refused: []conformance.RefusedDrive{{
			Name: "HostPathShare",
			Mount: types.DriveMount{
				DriveID:    uuid.New(),
				Backend:    types.DriveBackendHostPath,
				ObjectName: hostPathName,
				HostRoot:   "/srv/share",
				DriveName:  "conformance-share",
				HomeName:   home,
				Target:     runner.DriveTarget,
				SizeMiB:    64,
			},
			WantErr: "which this substrate cannot mount",
		}},
	}
}

// lockDriveAs2000 runs a one-shot uid-2000 pod on the drive's claim that makes
// the locked directory, and waits for it to succeed.
func lockDriveAs2000(t *testing.T, cs kubernetes.Interface, ns, image, claim string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	name := "wardyn-conformance-drivelock-" + uuid.NewString()[:8]
	d := "/d/" + conformance.UserDriveLockedDir
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: ptr(false),
			Containers: []corev1.Container{{
				Name:            "lock",
				Image:           image,
				Command:         []string{"sh", "-c", "mkdir " + d + " && echo s > " + d + "/" + conformance.UserDriveLockedFile + " && chmod 0700 " + d},
				VolumeMounts:    []corev1.VolumeMount{{Name: "drive", MountPath: "/d"}},
				SecurityContext: restrictedContainer(2000),
			}},
			Volumes: []corev1.Volume{{
				Name:         "drive",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}},
			}},
		},
	}
	if _, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the drive-lock pod: %v", err)
	}
	defer func() {
		_ = cs.CoreV1().Pods(ns).Delete(context.Background(), name, metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))})
	}()
	for {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get the drive-lock pod: %v", err)
		}
		switch p.Status.Phase {
		case corev1.PodSucceeded:
			return
		case corev1.PodFailed:
			b, _ := cs.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{}).DoRaw(ctx)
			t.Fatalf("the drive-lock pod failed:\n%s", b)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("the drive-lock pod never finished (phase %s): %v", p.Status.Phase, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
