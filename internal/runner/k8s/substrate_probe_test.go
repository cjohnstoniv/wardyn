// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The probe is one list of pods in the runs namespace, and each failure class
// comes from the apiserver's own answer. (The fake clientset does not expose
// the list's limit, so the limit of 1 is pinned by reading the call.)
func TestProbeSubstrate_OnePodListClassified(t *testing.T) {
	pods := schema.GroupResource{Resource: "pods"}
	cases := []struct {
		name string
		err  error
		want runner.SubstrateState
	}{
		{"answers", nil, runner.SubstrateOK},
		{"token refused", apierrors.NewUnauthorized("token expired"), runner.SubstrateUnauthorized},
		{"role binding gone", apierrors.NewForbidden(pods, "", errors.New("no RBAC policy")), runner.SubstrateForbidden},
		{"apiserver down", errors.New("dial tcp: connection refused"), runner.SubstrateUnreachable},
		{"server error", apierrors.NewInternalError(errors.New("etcd")), runner.SubstrateUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, cs := newTestDriver(t, Config{})
			var lists []ktesting.ListAction
			cs.PrependReactor("list", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
				lists = append(lists, a.(ktesting.ListAction))
				if tc.err != nil {
					return true, nil, tc.err
				}
				return false, nil, nil
			})
			if got := d.ProbeSubstrate(context.Background()); got != tc.want {
				t.Fatalf("ProbeSubstrate = %q, want %q", got, tc.want)
			}
			if len(lists) != 1 {
				t.Fatalf("the probe made %d pod lists, want exactly 1", len(lists))
			}
			if a := lists[0]; a.GetNamespace() != testNamespace {
				t.Fatalf("the list was for namespace %q, want the runs namespace %q", a.GetNamespace(), testNamespace)
			}
		})
	}
}

// An apiserver that never answers is unreachable at the caller's deadline.
func TestProbeSubstrate_DeadlineIsUnreachable(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	cs.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		time.Sleep(200 * time.Millisecond)
		return true, nil, errors.New("late")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if got := d.ProbeSubstrate(ctx); got != runner.SubstrateUnreachable {
		t.Fatalf("ProbeSubstrate = %q, want unreachable", got)
	}
}
