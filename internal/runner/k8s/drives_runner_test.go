// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keptDriveClaimName is a claim name Wardyn derives (types.DriveClaimNamedByWardyn).
const keptDriveClaimName = "wardyn-drive-research-d-9f3a1c"

// keptDriveRunner is a runner harness over two runs namespaces, whose drives switch is drives, with a
// runner id the driver can parse as the one a drive is kept on.
func keptDriveRunner(t *testing.T, drives bool) (*runnerHarness, uuid.UUID) {
	t.Helper()
	h := newRunnerHarness(t, testNamespace, otherNamespace)
	self := uuid.New()
	h.d.cfg.RunnerID, h.d.cfg.Drives = self.String(), drives
	return h, self
}

// keptDrive is a drive an administrator keeps on runner self, in namespace ns: a static claim, read-only.
func keptDrive(self uuid.UUID, ns string) types.DriveMount {
	d := staticShareDriveMount()
	d.ObjectName, d.ReadOnly, d.RunnerID, d.Namespace = keptDriveClaimName, true, &self, ns
	return *d
}

// addKeptClaim creates the administrator's claim, which carries no Wardyn label.
func addKeptClaim(t *testing.T, h *runnerHarness, ns, name string) {
	t.Helper()
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: "uid-kept-claim"}}
	if _, err := h.cs.CoreV1().PersistentVolumeClaims(ns).Create(context.Background(), claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// assertKeptRefusalCreatedNothing fails when the fake recorded any create, or any call on a claim.
func assertKeptRefusalCreatedNothing(t *testing.T, h *runnerHarness) {
	t.Helper()
	for _, a := range h.cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource != "selfsubjectaccessreviews" {
			t.Errorf("a refused drive still created %s", a.GetResource().Resource)
		}
		if a.GetResource().Resource == "persistentvolumeclaims" {
			t.Errorf("a refused drive still issued %s on a claim", a.GetVerb())
		}
	}
}

func TestCreateSandbox_RunnerMountsAKeptClaim(t *testing.T) {
	px := httptest.NewServer(http.NotFoundHandler())
	defer px.Close()
	h, self := keptDriveRunner(t, true)
	h.d.cfg.ProxyImage = "registry.example/wardyn-proxy:pinned"
	h.d.cfg.Sink = &fakeSink{h: h}
	h.d.cfg.ControlPlaneRelay = func(*url.URL) (string, error) { return "10.96.0.40", nil }
	installProbedProxyReactor(h, px.Listener.Addr().String(), nil)
	installAgentRunningReactor(t, h.cs)
	addKeptClaim(t, h, testNamespace, keptDriveClaimName)
	h.cs.ClearActions()

	spec := testSandboxSpec()
	spec.RunID = h.runID
	spec.Drives = []types.DriveMount{keptDrive(self, testNamespace)}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sb, err := h.d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	pod, err := h.cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	vol := volumeFor(pod.Spec.Volumes, driveVolumeName)
	if vol == nil || vol.PersistentVolumeClaim == nil || vol.PersistentVolumeClaim.ClaimName != keptDriveClaimName || !vol.PersistentVolumeClaim.ReadOnly {
		t.Fatalf("agent pod drive volume = %+v, want the kept claim, read-only", vol)
	}
	if m := mountFor(pod.Spec.Containers[0].VolumeMounts, driveVolumeName); m == nil || !m.ReadOnly {
		t.Errorf("agent container drive mount = %+v, want read-only", m)
	}
	if sc := pod.Spec.SecurityContext; sc != nil && sc.FSGroup != nil {
		t.Errorf("pod fsGroup = %d, want none beside a kept claim", *sc.FSGroup)
	}
	verbs := countPVCVerbs(h.cs)
	if verbs["get"] == 0 {
		t.Errorf("claim verbs = %v, want the claim read", verbs)
	}
	for v := range verbs {
		if v != "get" {
			t.Errorf("issued %q against persistentvolumeclaims; a runner holds get only", v)
		}
	}
}

func TestCreateSandbox_RunnerDrivesOffRefusesEveryDrive(t *testing.T) {
	h, self := keptDriveRunner(t, false)
	addKeptClaim(t, h, testNamespace, keptDriveClaimName)
	h.cs.ClearActions()

	spec := testSandboxSpec()
	spec.Drives = []types.DriveMount{keptDrive(self, testNamespace)}
	if _, err := h.d.CreateSandbox(context.Background(), spec); !errors.Is(err, errRunnerDrivesOff) {
		t.Fatalf("err = %v, want errRunnerDrivesOff", err)
	}
	assertKeptRefusalCreatedNothing(t, h)
}

func TestCreateSandbox_RunnerRefusesAManagedDrive(t *testing.T) {
	h, self := keptDriveRunner(t, true)
	h.cs.ClearActions()

	spec := testSandboxSpec()
	d := keptDrive(self, testNamespace)
	d.Backend = types.DriveBackendK8sPVC
	spec.Drives = []types.DriveMount{d}
	if _, err := h.d.CreateSandbox(context.Background(), spec); !errors.Is(err, errRunnerDriveNotKept) {
		t.Fatalf("err = %v, want errRunnerDriveNotKept", err)
	}
	assertKeptRefusalCreatedNothing(t, h)
}

func TestCreateSandbox_RunnerRefusesADriveNotKeptHere(t *testing.T) {
	other := uuid.New()
	for _, tc := range []struct {
		name   string
		change func(d *types.DriveMount)
	}{
		{"another served namespace", func(d *types.DriveMount) { d.Namespace = otherNamespace }},
		{"another runner", func(d *types.DriveMount) { d.RunnerID = &other }},
		{"no runner", func(d *types.DriveMount) { d.RunnerID = nil }},
		{"no namespace", func(d *types.DriveMount) { d.Namespace = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, self := keptDriveRunner(t, true)
			addKeptClaim(t, h, testNamespace, keptDriveClaimName)
			h.cs.ClearActions()

			spec := testSandboxSpec()
			d := keptDrive(self, testNamespace)
			tc.change(&d)
			spec.Drives = []types.DriveMount{d}
			if _, err := h.d.CreateSandbox(context.Background(), spec); !errors.Is(err, errRunnerDriveNotKept) {
				t.Fatalf("err = %v, want errRunnerDriveNotKept", err)
			}
			assertKeptRefusalCreatedNothing(t, h)
		})
	}
}

func TestCreateSandbox_ServerRefusesADriveKeptOnARunner(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.7")
	installAgentRunningReactor(t, cs)
	cs.ClearActions()

	self := uuid.New()
	spec := testSandboxSpec()
	spec.Drives = []types.DriveMount{keptDrive(self, testNamespace)}
	if _, err := d.CreateSandbox(context.Background(), spec); !errors.Is(err, errDriveKeptOnRunner) {
		t.Fatalf("err = %v, want errDriveKeptOnRunner", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			t.Errorf("a refused drive still created %s", a.GetResource().Resource)
		}
	}

	// A drive with no namespace is not kept on a runner: it mounts as before.
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: keptDriveClaimName, Namespace: testNamespace, UID: "uid-share"}}
	if _, err := cs.CoreV1().PersistentVolumeClaims(testNamespace).Create(context.Background(), claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	share := staticShareDriveMount()
	share.ObjectName, share.ReadOnly = keptDriveClaimName, true
	spec = testSandboxSpec()
	spec.Drives = []types.DriveMount{*share}
	if _, err := d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatalf("a drive with no namespace refused on the server's cluster: %v", err)
	}
}

func TestCreateSandbox_KeptClaimForeignLabelsStillRefused(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"a managed claim":      {labelManaged: "true"},
		"another subject":      {labelDriveSubject: "ffffffffffffffffffff"},
		"another drive's home": {labelDriveHome: "d-0000"},
	} {
		t.Run(name, func(t *testing.T) {
			h, self := keptDriveRunner(t, true)
			claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: keptDriveClaimName, Namespace: testNamespace, UID: "uid-kept-claim", Labels: labels}}
			if _, err := h.cs.CoreV1().PersistentVolumeClaims(testNamespace).Create(context.Background(), claim, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			spec := testSandboxSpec()
			spec.Drives = []types.DriveMount{keptDrive(self, testNamespace)}
			if _, err := h.d.CreateSandbox(context.Background(), spec); !errors.Is(err, errDriveClaimForeign) {
				t.Fatalf("err = %v, want errDriveClaimForeign", err)
			}
		})
	}
}

func TestCreateSandbox_RunnerRefusesAClaimNotNamedByWardyn(t *testing.T) {
	h, self := keptDriveRunner(t, true)
	addKeptClaim(t, h, testNamespace, "data")
	h.cs.ClearActions()

	spec := testSandboxSpec()
	d := keptDrive(self, testNamespace)
	d.ObjectName = "data"
	spec.Drives = []types.DriveMount{d}
	if _, err := h.d.CreateSandbox(context.Background(), spec); !errors.Is(err, errRunnerDriveNotKept) {
		t.Fatalf("err = %v, want errRunnerDriveNotKept", err)
	}
	assertKeptRefusalCreatedNothing(t, h)
	for _, a := range h.cs.Actions() {
		if a.GetResource().Resource == "pods" {
			t.Errorf("a refused drive still issued %s on pods", a.GetVerb())
		}
	}
}
