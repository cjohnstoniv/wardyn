// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// How long a lifecycle verdict is reused. A yes is long (RBAC rarely loosens
// mid-flight); a no is short, so an admin who grants the verb is believed
// within seconds. An apiserver error is never cached: it is not a verdict.
const (
	driveLifecycleYesTTL = 60 * time.Second
	driveLifecycleNoTTL  = 10 * time.Second
)

// driveLifecycleVerbs are what a member drive's claim needs over its life:
// get (mount), create (first mount) and delete (ReclaimDrive).
var driveLifecycleVerbs = []string{"get", "create", "delete"}

type driveLifecycleVerdict struct {
	denied string // the first denied verb; "" means all three are allowed
	until  time.Time
}

// driveLifecycleCache holds each Driver's last verdict. It is package state
// because the verdict is the only per-Driver memory this check needs.
var driveLifecycleCache = struct {
	sync.Mutex
	m map[*Driver]driveLifecycleVerdict
}{m: map[*Driver]driveLifecycleVerdict{}}

// driveLifecycleNow is the test seam for the verdict clock.
var driveLifecycleNow = time.Now

// CanManageDrives implements runner.DriveLifecycleChecker: one
// SelfSubjectAccessReview each for get, create and delete on
// persistentvolumeclaims in the runs namespace. Any denial or error answers
// runner.ErrDriveLifecycleUnavailable, so a member drive is never created that
// ReclaimDrive could not later delete. In practice this is the chart's
// drives.enabled plus drives.reclaim.enabled. A cluster that withholds
// SelfSubjectAccessReview from wardynd fails closed the same way.
func (d *Driver) CanManageDrives(ctx context.Context) error {
	now := driveLifecycleNow()
	driveLifecycleCache.Lock()
	v, ok := driveLifecycleCache.m[d]
	driveLifecycleCache.Unlock()
	if ok && now.Before(v.until) {
		return d.driveLifecycleAnswer(v.denied)
	}
	denied, err := d.reviewDriveLifecycle(ctx)
	if err != nil {
		slog.Warn("wardynd: k8s substrate: could not review the drive-claim permissions",
			slog.String("namespace", d.cfg.Namespace), slog.String("error", err.Error()))
		return fmt.Errorf("k8s: drives: review claim permissions in %q: %w", d.cfg.Namespace, runner.ErrDriveLifecycleUnavailable)
	}
	ttl := driveLifecycleYesTTL
	if denied != "" {
		ttl = driveLifecycleNoTTL
	}
	driveLifecycleCache.Lock()
	driveLifecycleCache.m[d] = driveLifecycleVerdict{denied: denied, until: now.Add(ttl)}
	driveLifecycleCache.Unlock()
	return d.driveLifecycleAnswer(denied)
}

func (d *Driver) driveLifecycleAnswer(denied string) error {
	if denied == "" {
		return nil
	}
	return fmt.Errorf("k8s: drives: the service account may not %s claims in %q: %w", denied, d.cfg.Namespace, runner.ErrDriveLifecycleUnavailable)
}

// reviewDriveLifecycle asks the apiserver about each verb and returns the
// first denied one, or "" when all are allowed.
func (d *Driver) reviewDriveLifecycle(ctx context.Context) (string, error) {
	for _, verb := range driveLifecycleVerbs {
		review := &authzv1.SelfSubjectAccessReview{Spec: authzv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authzv1.ResourceAttributes{Namespace: d.cfg.Namespace, Verb: verb, Resource: "persistentvolumeclaims"},
		}}
		got, err := d.clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			return "", err
		}
		if !got.Status.Allowed {
			return verb, nil
		}
	}
	return "", nil
}
