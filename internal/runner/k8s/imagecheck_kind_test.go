// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A prewrapped image's check on a real cluster (design final §3): a fake clientset cannot run an image or
// lose the control plane mid-check. nightly.yml's kind-survival-walk runs these on the quickstart cluster
// (amd64 nodes only, Calico), in a namespace of their own so that cluster's wardynd never sweeps them.
//
//	WARDYN_TEST_K8S=1 WARDYN_K8S_NAMESPACE=<ns> WARDYN_PROXY_IMAGE=<proxy> \
//	WARDYN_TEST_PREWRAP_IMAGE=<agent-base> WARDYN_TEST_PREWRAP_NO_REC_IMAGE=<agent-base without wardyn-rec>

// liveImageCheckDriver is the production driver on the current kubeconfig: New's boot canary has to prove
// NetworkPolicy enforced, or the check is unavailable.
func liveImageCheckDriver(t *testing.T) (d *Driver, base, noRec string) {
	t.Helper()
	base, noRec = os.Getenv("WARDYN_TEST_PREWRAP_IMAGE"), os.Getenv("WARDYN_TEST_PREWRAP_NO_REC_IMAGE")
	if os.Getenv("WARDYN_TEST_K8S") != "1" || base == "" || noRec == "" {
		t.Skip("WARDYN_TEST_K8S=1, WARDYN_TEST_PREWRAP_IMAGE and WARDYN_TEST_PREWRAP_NO_REC_IMAGE not set")
	}
	d, err := New(Config{Namespaces: []string{os.Getenv("WARDYN_K8S_NAMESPACE")}, ProxyImage: os.Getenv("WARDYN_PROXY_IMAGE")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if why := d.PrewrappedCheckUnavailable(); why != "" {
		t.Fatalf("check unavailable: %s", why)
	}
	return d, base, noRec
}

func TestKindPrewrappedCheck(t *testing.T) {
	d, base, noRec := liveImageCheckDriver(t)
	defer func(v time.Duration) { imageCheckNoNodeAfter = v }(imageCheckNoNodeAfter)
	imageCheckNoNodeAfter = 15 * time.Second // the arm64 pod never schedules here
	for _, tc := range []struct {
		name, image, status, reason string
		accepted                    []string
	}{
		{"agent-base passes on amd64", base, types.BaseImageCheckReady, "no_node:arm64", []string{"amd64"}},
		{"without wardyn-rec it is refused", noRec, types.BaseImageCheckRefused, "contract_missing:amd64:wardyn-rec", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt := uuid.New()
			vs := d.CheckPrewrappedImage(context.Background(), attempt, tc.image)
			status, reason, accepted := runner.AggregateImageCheck(vs)
			if status != tc.status || reason != tc.reason || !slices.Equal(accepted, tc.accepted) {
				t.Fatalf("verdict = %s %q %v, want %s %q %v (per arch: %+v)", status, reason, accepted, tc.status, tc.reason, tc.accepted, vs)
			}
			if n := checkObjects(t, d, attempt); n != 0 {
				t.Fatalf("%d of the attempt's objects outlived the check", n)
			}
		})
	}
}

// The control plane dies mid-check: a child process starts a check and is killed while its arm64 pod waits
// for a node. Nothing tears the attempt down, and the orphan sweep reclaims its pod and policy.
func TestKindPrewrappedCheckControlPlaneKilled(t *testing.T) {
	d, base, _ := liveImageCheckDriver(t)
	if id := os.Getenv("WARDYN_TEST_PREWRAP_CHILD"); id != "" {
		d.CheckPrewrappedImage(context.Background(), uuid.MustParse(id), base)
		return // killed long before the arm64 pod's two minutes are up
	}
	attempt := uuid.New()
	child := exec.Command(os.Args[0], "-test.run=^TestKindPrewrappedCheckControlPlaneKilled$")
	child.Env = append(os.Environ(), "WARDYN_TEST_PREWRAP_CHILD="+attempt.String())
	if err := child.Start(); err != nil {
		t.Fatalf("start the child: %v", err)
	}
	ns := d.defaultNamespace()
	waitFor(t, "the arm64 check pod", func() bool {
		_, err := d.clientset.CoreV1().Pods(ns).Get(context.Background(), "wardyn-imagecheck-"+attempt.String()+"-arm64", metav1.GetOptions{})
		return err == nil
	})
	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill the child: %v", err)
	}
	_ = child.Wait()
	if n := checkObjects(t, d, attempt); n < 2 {
		t.Fatalf("the killed check left %d objects; want its pod and policy", n)
	}
	if _, err := d.SweepOrphanedSandboxes(context.Background(), 0, func(id uuid.UUID) bool { return id == attempt }); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	waitFor(t, "the sweep to reclaim the attempt", func() bool { return checkObjects(t, d, attempt) == 0 })
}

// checkObjects counts the attempt's pods and NetworkPolicies.
func checkObjects(t *testing.T, d *Driver, attempt uuid.UUID) int {
	t.Helper()
	sel := metav1.ListOptions{LabelSelector: labelRun + "=" + attempt.String()}
	pods, err := d.clientset.CoreV1().Pods(d.defaultNamespace()).List(context.Background(), sel)
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	pols, err := d.clientset.NetworkingV1().NetworkPolicies(d.defaultNamespace()).List(context.Background(), sel)
	if err != nil {
		t.Fatalf("list policies: %v", err)
	}
	return len(pods.Items) + len(pols.Items)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Minute); time.Now().Before(end); time.Sleep(time.Second) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}
