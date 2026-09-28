// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ProbeDrive implements runner.DriveProber (#165): a claim Get plus a phase
// read — the honest ceiling of what this substrate can say. There is no
// pod-less way to ask the apiserver whether the agent's own uid can read a
// volume's root once mounted, so this never returns DriveProbeReadable or
// DriveProbeUnreadable — every outcome, including a Bound claim, answers
// DriveProbeUnknown rather than guess.
func (d *Driver) ProbeDrive(ctx context.Context, drive types.DriveMount) (runner.DriveProbe, error) {
	claim, err := d.clientset.CoreV1().PersistentVolumeClaims(d.cfg.Namespace).
		Get(ctx, drive.ObjectName, metav1.GetOptions{})
	if err != nil {
		return runner.DriveProbe{Result: runner.DriveProbeUnknown,
			Detail: fmt.Sprintf("get claim %q: %v", drive.ObjectName, err)}, nil
	}
	if claim.Status.Phase == corev1.ClaimBound {
		return runner.DriveProbe{Result: runner.DriveProbeUnknown, Detail: "claim Bound"}, nil
	}
	return runner.DriveProbe{Result: runner.DriveProbeUnknown,
		Detail: fmt.Sprintf("claim %q is %s, not Bound", drive.ObjectName, claim.Status.Phase)}, nil
}
