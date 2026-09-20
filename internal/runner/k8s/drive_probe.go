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
// read — the honest ceiling of what this substrate can say. Unlike the Docker
// driver, there is no filesystem here for wardynd to stat at all (the problem
// #165 names: "on Kubernetes wardynd cannot stat a claim at all"), and no
// pod-less way to ask the apiserver "can uid 1000 read this volume's root" —
// only a caller running INSIDE a mounted pod could answer that, which is not
// available before a sandbox exists (create, preflight, a /me poll).
//
// So this never claims DriveProbeReadable on evidence weaker than "the claim
// exists and is Bound", and it never claims DriveProbeUnreadable at all — a
// Pending or unreachable claim is not proof the agent uid cannot read it, it
// is only proof this call could not tell. See DriveProbeUnknown's doc: a
// probe that cannot see the storage must answer unknown, never a guess either
// way.
func (d *Driver) ProbeDrive(ctx context.Context, drive types.DriveMount) (runner.DriveProbe, error) {
	claim, err := d.clientset.CoreV1().PersistentVolumeClaims(d.cfg.Namespace).
		Get(ctx, drive.ObjectName, metav1.GetOptions{})
	if err != nil {
		// Not found, forbidden, or the apiserver simply did not answer: every
		// one of these is "this driver cannot read the claim", and an
		// unreadable claim reports unknown, never a pass — see the package
		// doc above.
		return runner.DriveProbe{Result: runner.DriveProbeUnknown,
			Detail: fmt.Sprintf("get claim %q: %v", drive.ObjectName, err)}, nil
	}
	if claim.Status.Phase == corev1.ClaimBound {
		return runner.DriveProbe{Result: runner.DriveProbeReadable}, nil
	}
	// Found but not (yet) Bound: PENDING/LOST is a fact about provisioning,
	// not about read permission, so it is no more a proof of unreadability
	// than a Bound phase is proof of readability by the agent's OWN uid —
	// unknown either way.
	return runner.DriveProbe{Result: runner.DriveProbeUnknown,
		Detail: fmt.Sprintf("claim %q is %s, not Bound", drive.ObjectName, claim.Status.Phase)}, nil
}
