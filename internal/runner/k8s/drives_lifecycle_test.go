// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"testing"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// ssarDriver is a driver whose fake apiserver answers each SelfSubjectAccessReview
// from allow (verb -> allowed), counts the reviews, and fails them with failErr when set.
func ssarDriver(t *testing.T, allow map[string]bool, failErr error) (*Driver, *int) {
	t.Helper()
	cs := fake.NewClientset()
	reviews := 0
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a ktesting.Action) (bool, runtime.Object, error) {
		reviews++
		if failErr != nil {
			return true, nil, failErr
		}
		in := a.(ktesting.CreateAction).GetObject().(*authzv1.SelfSubjectAccessReview)
		ra := in.Spec.ResourceAttributes
		if ra.Namespace != testNamespace || ra.Resource != "persistentvolumeclaims" {
			t.Errorf("review for %+v, want persistentvolumeclaims in %s", ra, testNamespace)
		}
		out := in.DeepCopy()
		out.Status.Allowed = allow[ra.Verb]
		return true, out, nil
	})
	return &Driver{clientset: cs, cfg: Config{Namespace: testNamespace}}, &reviews
}

// fakeLifecycleClock replaces the verdict clock for one test.
func fakeLifecycleClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	prev := driveLifecycleNow
	driveLifecycleNow = func() time.Time { return now }
	t.Cleanup(func() { driveLifecycleNow = prev })
	return &now
}

// TestK8sCanManageDrivesSSAR: all three verbs allowed -> nil; a denial of ANY
// one (create alone is not enough) or an apiserver error -> the unavailable
// sentinel; a denial is not remembered past 10 s, a yes is remembered for 60 s.
func TestK8sCanManageDrivesSSAR(t *testing.T) {
	ctx := context.Background()
	all := map[string]bool{"get": true, "create": true, "delete": true}

	t.Run("all allowed", func(t *testing.T) {
		d, n := ssarDriver(t, all, nil)
		if err := d.CanManageDrives(ctx); err != nil {
			t.Fatalf("CanManageDrives = %v, want nil", err)
		}
		if *n != 3 {
			t.Errorf("reviews = %d, want one each for get, create, delete", *n)
		}
	})

	for _, missing := range []string{"get", "create", "delete"} {
		t.Run("denied "+missing, func(t *testing.T) {
			allow := map[string]bool{"get": true, "create": true, "delete": true}
			allow[missing] = false
			d, _ := ssarDriver(t, allow, nil)
			if err := d.CanManageDrives(ctx); !errors.Is(err, runner.ErrDriveLifecycleUnavailable) {
				t.Fatalf("CanManageDrives = %v, want ErrDriveLifecycleUnavailable", err)
			}
		})
	}

	t.Run("apiserver error fails closed and is not cached", func(t *testing.T) {
		d, n := ssarDriver(t, all, errors.New("ssar forbidden"))
		for i := 0; i < 2; i++ {
			if err := d.CanManageDrives(ctx); !errors.Is(err, runner.ErrDriveLifecycleUnavailable) {
				t.Fatalf("CanManageDrives = %v, want ErrDriveLifecycleUnavailable", err)
			}
		}
		if *n != 2 {
			t.Errorf("reviews = %d, want 2 (an error is not a cached verdict)", *n)
		}
	})

	t.Run("a denial is cached 10 s, a yes 60 s", func(t *testing.T) {
		now := fakeLifecycleClock(t)
		allow := map[string]bool{"get": true, "create": true}
		d, n := ssarDriver(t, allow, nil)
		if err := d.CanManageDrives(ctx); err == nil {
			t.Fatal("delete denied but CanManageDrives = nil")
		}
		allow["delete"] = true // the admin grants the verb
		*now = now.Add(9 * time.Second)
		if err := d.CanManageDrives(ctx); err == nil {
			t.Fatal("inside the 10 s window the cached denial must stand")
		}
		*now = now.Add(2 * time.Second) // 11 s after the denial
		if err := d.CanManageDrives(ctx); err != nil {
			t.Fatalf("past 10 s the denial must be re-asked, got %v", err)
		}
		asked := *n
		*now = now.Add(59 * time.Second)
		if err := d.CanManageDrives(ctx); err != nil || *n != asked {
			t.Fatalf("inside 60 s the yes must be cached: err=%v reviews %d -> %d", err, asked, *n)
		}
		*now = now.Add(2 * time.Second)
		allow["delete"] = false // the admin revokes it
		if err := d.CanManageDrives(ctx); err == nil {
			t.Fatal("past 60 s the yes must be re-asked and the revoked verb refused")
		}
	})
}
