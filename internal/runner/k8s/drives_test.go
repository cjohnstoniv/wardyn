// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// dns1123Subdomain is the shape every Kubernetes object name must take. The
// claim's name is the resolver's ObjectName verbatim, so this is the assertion
// that the control plane's home-name rule and this substrate's object naming
// actually meet.
var dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// testDriveID is a fixed drive row id so the label assertions below read as
// literals rather than as whatever uuid.New() happened to produce.
var testDriveID = uuid.MustParse("6f1d4b6e-3c2a-4d5f-9a71-8b0c1d2e3f40")

// testDriveMount is the shape the control plane's resolver hands the driver for
// a MANAGED (k8s_pvc) drive: an object name it derived, the reserved target as
// the symbol, and the size/enforcement pair the honesty vocabulary is built on.
func testDriveMount() *types.DriveMount {
	return &types.DriveMount{
		DriveID:      testDriveID,
		Backend:      types.DriveBackendK8sPVC,
		ObjectName:   "wardyn-drive-research-d-9f3a1c",
		StorageClass: "fast-block",
		HomeName:     "d-9f3a1c",
		Target:       runner.DriveTarget,
		SizeMiB:      10240,
		Enforcement:  types.StorageEnforcementRequest,
	}
}

// createdPVCs returns every claim the fake actually stored.
func createdPVCs(t *testing.T, cs *fake.Clientset) []corev1.PersistentVolumeClaim {
	t.Helper()
	list, err := cs.CoreV1().PersistentVolumeClaims(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	return list.Items
}

// countPVCVerbs counts the actions issued against the persistentvolumeclaims
// resource, by verb — the direct proof of the RBAC the chart grants (get and
// create, and nothing else).
func countPVCVerbs(cs *fake.Clientset) map[string]int {
	verbs := map[string]int{}
	for _, a := range cs.Actions() {
		if a.GetResource().Resource == "persistentvolumeclaims" {
			verbs[a.GetVerb()]++
		}
	}
	return verbs
}

// TestEnsureDrivePVC_ManagedCreatesTheClaim pins every field of the claim a
// managed drive provisions — including the one that is ABSENT: no wardyn.run-id
// label, so teardownByRunID's DeleteCollection selector cannot match a person's
// storage (see TestCreateSandbox_DriveSurvivesTeardown for the other half).
func TestEnsureDrivePVC_ManagedCreatesTheClaim(t *testing.T) {
	cs := fake.NewClientset()
	drive := testDriveMount()

	if err := ensureDrivePVC(context.Background(), cs, testNamespace, drive); err != nil {
		t.Fatalf("ensureDrivePVC: %v", err)
	}

	// Counted BEFORE createdPVCs, whose own List would otherwise show up here.
	if verbs := countPVCVerbs(cs); verbs["get"] != 1 || verbs["create"] != 1 || len(verbs) != 2 {
		t.Errorf("claim verbs = %v, want exactly one get and one create (the two the chart's Role grants)", verbs)
	}
	pvcs := createdPVCs(t, cs)
	if len(pvcs) != 1 {
		t.Fatalf("claims created = %d, want exactly 1", len(pvcs))
	}
	pvc := pvcs[0]
	if pvc.Name != drive.ObjectName {
		t.Errorf("claim name = %q, want the resolver's object name %q", pvc.Name, drive.ObjectName)
	}
	// The name is used RAW: the API refuses a home that is not a DNS-1123
	// subdomain before a run reaches a driver, so wardyn-drive-<slug>-<home> is
	// always a legal object name and nothing here re-shapes it. This asserts
	// that contract rather than trusting it.
	if !dns1123Subdomain.MatchString(pvc.Name) || len(pvc.Name) > 253 {
		t.Errorf("claim name %q is not a DNS-1123 subdomain — the apiserver would refuse the create outright", pvc.Name)
	}
	if got := pvc.Spec.AccessModes; len(got) != 1 || got[0] != corev1.ReadWriteOnce {
		t.Errorf("accessModes = %v, want [ReadWriteOnce]", got)
	}
	// Compared by VALUE, not by spelling: a Quantity loses its cached string on
	// any round trip, so 10240Mi reads back as its canonical 10Gi — the same
	// bytes, which is what the storage class is asked for.
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Value() != 10240*1024*1024 {
		t.Errorf("requests.storage = %s (%d bytes), want the allocation 10240Mi", got.String(), got.Value())
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast-block" {
		t.Errorf("storageClassName = %v, want the drive's own class", pvc.Spec.StorageClassName)
	}
	if got := pvc.Labels[labelDrive]; got != testDriveID.String() {
		t.Errorf("%s = %q, want the drive row's id", labelDrive, got)
	}
	if got := pvc.Labels[labelDriveHome]; got != drive.HomeName {
		t.Errorf("%s = %q, want %q", labelDriveHome, got, drive.HomeName)
	}
	if got := pvc.Labels[labelManaged]; got != "true" {
		t.Errorf("%s = %q, want %q", labelManaged, got, "true")
	}
	// The load-bearing absence: a drive outlives every run that mounts it.
	if _, ok := pvc.Labels[labelRun]; ok {
		t.Errorf("claim carries a %s label (%v) — teardown's DeleteCollection selects on exactly that and would delete the person's storage", labelRun, pvc.Labels)
	}
	if _, ok := pvc.Labels[labelComponent]; ok {
		t.Errorf("claim carries a %s label (%v); it is not a per-run component", labelComponent, pvc.Labels)
	}
}

// TestEnsureDrivePVC_ManagedOmitsAnEmptyStorageClass proves "" means the cluster
// default rather than a class literally named "": a claim pinned to the empty
// string binds nothing at all on a cluster with a default class.
func TestEnsureDrivePVC_ManagedOmitsAnEmptyStorageClass(t *testing.T) {
	cs := fake.NewClientset()
	drive := testDriveMount()
	drive.StorageClass = ""

	if err := ensureDrivePVC(context.Background(), cs, testNamespace, drive); err != nil {
		t.Fatalf("ensureDrivePVC: %v", err)
	}
	pvcs := createdPVCs(t, cs)
	if len(pvcs) != 1 {
		t.Fatalf("claims created = %d, want 1", len(pvcs))
	}
	if sc := pvcs[0].Spec.StorageClassName; sc != nil {
		t.Errorf("storageClassName = %q, want unset so the cluster default applies", *sc)
	}
}

// TestEnsureDrivePVC_ManagedReusesAnExistingClaim is the idempotence a member's
// SECOND run depends on: the claim is found by name and no second one is created
// (a create-always driver would 409 every run after the first, or worse, race
// two claims for one person).
func TestEnsureDrivePVC_ManagedReusesAnExistingClaim(t *testing.T) {
	drive := testDriveMount()
	cs := fake.NewClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: drive.ObjectName, Namespace: testNamespace},
	})

	if err := ensureDrivePVC(context.Background(), cs, testNamespace, drive); err != nil {
		t.Fatalf("ensureDrivePVC: %v", err)
	}
	if verbs := countPVCVerbs(cs); verbs["create"] != 0 {
		t.Errorf("claim verbs = %v, want no create against an existing claim", verbs)
	}
}

// TestEnsureDrivePVC_StaticShareIsNeverCreated pins the SHARE half: an
// admin-provisioned claim that is missing is a refusal, never an empty volume
// invented under the same name where a corporate home was meant to be.
func TestEnsureDrivePVC_StaticShareIsNeverCreated(t *testing.T) {
	cs := fake.NewClientset()
	drive := testDriveMount()
	drive.Backend = types.DriveBackendK8sPVCStatic
	drive.Enforcement = types.StorageEnforcementExternal

	err := ensureDrivePVC(context.Background(), cs, testNamespace, drive)
	if !errors.Is(err, errDriveClaimNotProvisioned) {
		t.Fatalf("err = %v, want errors.Is(err, errDriveClaimNotProvisioned)", err)
	}
	if verbs := countPVCVerbs(cs); verbs["create"] != 0 {
		t.Errorf("claim verbs = %v, want no create for a static share", verbs)
	}
	// The message is the run's failure hint verbatim, so it has to name the
	// remedy in words the person reading it can act on.
	if got := err.Error(); !strings.Contains(got, "not provisioned on this cluster") {
		t.Errorf("err = %q, want it to say the volume is not provisioned", got)
	}
}

// TestEnsureDrivePVC_ForbiddenNamesTheSwitch covers the RBAC arm: a 403 on
// Create is the one failure an operator can fix in one move, so the error names
// the verbs and the chart switch instead of surfacing the apiserver's own
// message alone.
func TestEnsureDrivePVC_ForbiddenNamesTheSwitch(t *testing.T) {
	cs := fake.NewClientset()
	drive := testDriveMount()
	cs.PrependReactor("create", "persistentvolumeclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "persistentvolumeclaims"}, drive.ObjectName,
			errors.New(`persistentvolumeclaims is forbidden: User "system:serviceaccount:wardyn:wardyn" cannot create resource`))
	})

	err := ensureDrivePVC(context.Background(), cs, testNamespace, drive)
	if !errors.Is(err, errDrivePVCForbidden) {
		t.Fatalf("err = %v, want errors.Is(err, errDrivePVCForbidden)", err)
	}
	for _, want := range []string{"persistentvolumeclaims", "userDrives.enabled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err.Error(), want)
		}
	}
}

// TestEnsureDrivePVC_ToleratesAnAlreadyExistsRace covers the Get→Create window two
// concurrent runs of the SAME person share: the loser of the race gets
// AlreadyExists, and the claim existing is all this function ever promised.
func TestEnsureDrivePVC_ToleratesAnAlreadyExistsRace(t *testing.T) {
	cs := fake.NewClientset()
	drive := testDriveMount()
	cs.PrependReactor("create", "persistentvolumeclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "persistentvolumeclaims"}, drive.ObjectName)
	})

	if err := ensureDrivePVC(context.Background(), cs, testNamespace, drive); err != nil {
		t.Fatalf("ensureDrivePVC: %v, want a lost create race to be tolerated", err)
	}
}

// TestEnsureDrivePVC_RefusesANonKubernetesBackend proves the wrong-substrate
// arm errors rather than skipping: a run that quietly came up with no drive is
// how a member's work disappears with the container.
func TestEnsureDrivePVC_RefusesANonKubernetesBackend(t *testing.T) {
	for _, backend := range []types.DriveBackend{types.DriveBackendDockerVolume, types.DriveBackendHostPath} {
		cs := fake.NewClientset()
		drive := testDriveMount()
		drive.Backend = backend

		err := ensureDrivePVC(context.Background(), cs, testNamespace, drive)
		if !errors.Is(err, errDriveBackendUnsupported) {
			t.Errorf("%s: err = %v, want errors.Is(err, errDriveBackendUnsupported)", backend, err)
		}
		if n := len(cs.Actions()); n != 0 {
			t.Errorf("%s: issued %d API calls, want none — the backend is wrong before anything is asked of the cluster", backend, n)
		}
	}
}

// TestEnsureDrivePVC_SurfacesANonNotFoundGetError pins the fail-closed arm of the
// lookup: a Get that fails for any reason OTHER than NotFound (a timeout, a
// broken apiserver) must not be read as "no claim yet" and provisioned over.
func TestEnsureDrivePVC_SurfacesANonNotFoundGetError(t *testing.T) {
	cs := fake.NewClientset()
	drive := testDriveMount()
	cs.PrependReactor("get", "persistentvolumeclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("apiserver is having a day"))
	})

	if err := ensureDrivePVC(context.Background(), cs, testNamespace, drive); err == nil {
		t.Fatal("ensureDrivePVC: want an error when the claim lookup itself fails")
	}
	if verbs := countPVCVerbs(cs); verbs["create"] != 0 {
		t.Errorf("claim verbs = %v, want no create after an unreadable lookup", verbs)
	}
}

// TestExec_EphemeralContainerMountsTheDrive is the half of the feature a pod
// spec alone does not deliver: the agent process runs in the EPHEMERAL exec
// container, so a drive mounted only into the idle main container would be
// invisible to the agent that is supposed to use it.
func TestExec_EphemeralContainerMountsTheDrive(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.9")
	installAgentRunningReactor(t, cs)

	spec := testSandboxSpec()
	spec.Drive = testDriveMount()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if _, err := d.Exec(context.Background(), sb.Ref, []string{"agent-run"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	if len(pod.Spec.EphemeralContainers) != 1 {
		t.Fatalf("ephemeral containers = %d, want 1", len(pod.Spec.EphemeralContainers))
	}
	mounts := pod.Spec.EphemeralContainers[0].VolumeMounts
	if len(mounts) != 1 {
		t.Fatalf("exec container mounts = %v, want exactly the drive", mounts)
	}
	want := corev1.VolumeMount{Name: driveVolumeName, MountPath: runner.DriveTarget, ReadOnly: false}
	if mounts[0] != want {
		t.Errorf("exec container mount = %+v, want %+v", mounts[0], want)
	}

	// Negative control: a drive-less run's exec container mounts nothing, so
	// this change is invisible to every run that does not ask for a drive.
	spec2 := testSandboxSpec()
	sb2, err := d.CreateSandbox(context.Background(), spec2)
	if err != nil {
		t.Fatalf("CreateSandbox (drive-less): %v", err)
	}
	if _, err := d.Exec(context.Background(), sb2.Ref, []string{"agent-run"}); err != nil {
		t.Fatalf("Exec (drive-less): %v", err)
	}
	pod2, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb2.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod (drive-less): %v", err)
	}
	if got := pod2.Spec.EphemeralContainers[0].VolumeMounts; len(got) != 0 {
		t.Errorf("drive-less exec container mounts = %v, want none", got)
	}
}
