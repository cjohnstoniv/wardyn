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
// So this NEVER claims DriveProbeReadable, on any evidence: a Bound claim is
// proof the volume is provisioned and attachable, not proof the AGENT'S OWN
// uid can read it once mounted (DriveProbeReadable's own contract,
// runner.go), and this driver has no other evidence to reach for. It never
// claims DriveProbeUnreadable either — a Pending or unreachable claim is not
// proof the agent uid cannot read it, it is only proof this call could not
// tell. Every arm answers DriveProbeUnknown: a probe that cannot see the
// storage must say so, never guess either way (DriveProbeUnknown's own doc).
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
		// Bound says the volume is provisioned and attachable — nothing about
		// whether the AGENT'S OWN uid can read it once mounted (DriveProbeReadable's
		// own contract, runner.go). Only a caller running inside a mounted pod
		// could answer that, and none exists yet at create/preflight/`/me` — so
		// this stays Unknown even on the positive case, never a guessed pass.
		return runner.DriveProbe{Result: runner.DriveProbeUnknown, Detail: "claim Bound"}, nil
	}
	// Found but not (yet) Bound: PENDING/LOST is a fact about provisioning,
	// not about read permission, so it is no more a proof of unreadability
	// than a Bound phase is proof of readability by the agent's OWN uid —
	// unknown either way.
	return runner.DriveProbe{Result: runner.DriveProbeUnknown,
		Detail: fmt.Sprintf("claim %q is %s, not Bound", drive.ObjectName, claim.Status.Phase)}, nil
}
