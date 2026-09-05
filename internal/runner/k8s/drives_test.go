// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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

// testDriveSubject is the fixture's subject digest, and it is NON-EMPTY on
// purpose. The fixture used to leave SubjectHash at "" — which made the third
// identity label unfalsifiable everywhere it was read: `existingDriveClaim` is
// stamped FROM the drive, so both sides of every reuse comparison were "", and
// driveClaimIdentity's own check is presence-guarded (`got != "" && …`), so ""
// on both sides is the arm that always passes. Deleting `labelDriveSubject:
// drive.SubjectHash` from ensureDrivePVC's create left the whole file green, and
// every claim created thereafter became ADOPTABLE by any principal sharing this
// drive's id and home — precisely the collision the label exists to break.
const testDriveSubject = "0f1e2d3c4b5a69788796"

// testDriveMount is the shape the control plane's resolver hands the driver for
// a MANAGED (k8s_pvc) drive: an object name it derived, the reserved target as
// the symbol, the subject digest that discriminates two principals folded onto
// one home, and the size/enforcement pair the honesty vocabulary is built on.
func testDriveMount() *types.DriveMount {
	return &types.DriveMount{
		DriveID:      testDriveID,
		Backend:      types.DriveBackendK8sPVC,
		ObjectName:   "wardyn-drive-research-d-9f3a1c",
		StorageClass: "fast-block",
		HomeName:     "d-9f3a1c",
		SubjectHash:  testDriveSubject,
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

// assertOnlyChartGrantedVerbs is the RBAC ceiling as a per-case assertion: the
// chart's Role lists get and create and nothing else (the Makefile pins that
// list, and pins that userDrives.enabled is what gates the rule existing at
// all), so a driver that reached for list, watch, patch or delete would fail on
// a real cluster with a 403 an operator cannot fix by flipping a switch.
//
// It is asserted per case rather than once, because the verbs a case issues
// depend on which arm it took: a refusal that already knows the answer must not
// go on to ask, and a delete-and-retry is exactly the "repair" a driver with no
// delete verb must never be written to attempt.
func assertOnlyChartGrantedVerbs(t *testing.T, cs *fake.Clientset) map[string]int {
	t.Helper()
	verbs := countPVCVerbs(cs)
	for v := range verbs {
		if v != "get" && v != "create" {
			t.Errorf("issued %q against persistentvolumeclaims; the chart's Role grants only get and create", v)
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
	// THE THIRD IDENTITY LABEL, on the claim this code path CREATED. Every other
	// assertion on wardyn.subject in this file runs against existingDriveClaim,
	// a fixture stamped by the same rule the code uses — so the create path was
	// the one place the label could go missing unobserved, and
	// driveClaimIdentity's presence guard (`got != "" && …`) means a claim
	// created WITHOUT it is adoptable by any principal who shares this drive's
	// id and home. That is the collision the label exists to break, so the stamp
	// is asserted where it is written.
	if got := pvc.Labels[labelDriveSubject]; got != drive.SubjectHash {
		t.Errorf("%s = %q, want the resolver's subject digest %q — a created claim without it is adoptable by anyone sharing this drive id and home",
			labelDriveSubject, got, drive.SubjectHash)
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
	cs := fake.NewClientset(existingDriveClaim(drive))

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
	for _, want := range []string{drive.ObjectName, "persistentvolumeclaims", "userDrives.enabled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to name %q", err.Error(), want)
		}
	}
	assertRefusalKeepsClusterNamesToItself(t, err)
}

// TestEnsureDrivePVC_ToleratesAnAlreadyExistsRace covers the Get→Create window,
// and the thing the loser of that race may NOT conclude from an AlreadyExists:
// that the claim now under its object name is its own member's.
//
// Two DIFFERENT first runs reach the Create over one name, because the name
// folds two variable-width fields with a separator both admit — drive "eng" with
// home "us-bob" and drive "eng-us" with home "bob" both resolve to
// wardyn-drive-eng-us-bob. Both Get→NotFound inside the window, both Create, one
// wins; a loser that read AlreadyExists as success would mount the winner's
// storage inside its own member's agent, through the one door that skipped
// reuseDriveClaim's identity check. So the loser re-reads and goes through
// exactly that check — which is what the FOREIGN row here proves, and what the
// same-identity row proves it did not break.
//
// The rows are the SAME shapes the Get-hit arm's tables walk —
// TestEnsureDrivePVC_RefusesAClaimThatIsNotThisMembers,
// TestEnsureDrivePVC_RefusesAClaimStampedForAnotherPerson,
// TestEnsureDrivePVC_RefusesATerminatingClaim — because the two doors have to
// ask the same question and only one of them is ever exercised in practice.
// This one is reached only by a first run that lost a race, so a door that
// diverged would diverge exactly where nobody looks, and each shape's wrong
// answer here is the same wrong answer it would be there.
func TestEnsureDrivePVC_ToleratesAnAlreadyExistsRace(t *testing.T) {
	for _, tc := range []struct {
		name string
		// mutate adjusts the drive this run resolved for, where a shape needs a
		// field the fixture leaves at its zero value.
		mutate func(*types.DriveMount)
		// winner is the claim the race is lost TO, or nil when the winner is
		// gone by the time the loser looks (an operator's reclaim landing
		// between the Create and the re-read).
		winner func(*types.DriveMount) *corev1.PersistentVolumeClaim
		wantIs error
	}{
		{
			// The tolerated case, and the reason this arm is not simply a refusal:
			// one person's two concurrent first runs.
			name:   "the winner is this same person's other run",
			winner: func(d *types.DriveMount) *corev1.PersistentVolumeClaim { return existingDriveClaim(d) },
		},
		{
			// The ambiguous pair, colliding inside the window rather than across
			// runs: this run is drive "eng" / home "us-bob", the winner was
			// created for drive "eng-us" / home "bob".
			name: "the winner is the ambiguous pair's other drive",
			winner: func(d *types.DriveMount) *corev1.PersistentVolumeClaim {
				other := testDriveMount()
				other.DriveID = uuid.MustParse("11111111-2222-3333-4444-555555555555")
				other.HomeName = "bob"
				claim := existingDriveClaim(other)
				claim.Name = d.ObjectName
				return claim
			},
			wantIs: errDriveClaimForeign,
		},
		{
			// Present for the Create, gone for the read. Never re-created: that
			// would be the recreate-under-a-reclaim errDriveClaimTerminating
			// refuses, one moment later.
			name:   "the winner is gone again by the time the loser reads it",
			winner: func(*types.DriveMount) *corev1.PersistentVolumeClaim { return nil },
			wantIs: errDriveClaimVanished,
		},
		{
			// Same drive row, another person's home — the rename / changed
			// home-template collision, which the drive-id label alone cannot
			// catch because the id matches.
			name: "the winner is another person's home on this same drive",
			winner: func(d *types.DriveMount) *corev1.PersistentVolumeClaim {
				other := testDriveMount()
				other.HomeName = "d-0000aa"
				claim := existingDriveClaim(other)
				claim.Name = d.ObjectName
				return claim
			},
			wantIs: errDriveClaimForeign,
		},
		{
			// One home template folding two principals onto ONE home
			// (email_local across two domains): drive id and home both match,
			// and only the subject digest tells the claims apart.
			name:   "the winner is another PRINCIPAL under the same home",
			mutate: func(d *types.DriveMount) { d.SubjectHash = "1111111111111111aaaa" },
			winner: func(d *types.DriveMount) *corev1.PersistentVolumeClaim {
				claim := existingDriveClaim(d)
				claim.Labels[labelDriveSubject] = "2222222222222222bbbb"
				return claim
			},
			wantIs: errDriveClaimForeign,
		},
		{
			// An object carrying none of the three labels was never created by
			// this driver, so it is an admin's own claim that happens to collide
			// — not an older Wardyn claim to adopt.
			name: "the winner is an unlabelled object under this claim name",
			winner: func(d *types.DriveMount) *corev1.PersistentVolumeClaim {
				claim := existingDriveClaim(d)
				claim.Labels = nil
				return claim
			},
			wantIs: errDriveClaimForeign,
		},
		{
			// The reclaim that landed inside the window: this run's own claim,
			// already being deleted. Mounting it hangs the pod until the
			// dispatch timeout with no readable cause, and re-creating it under
			// the same name would undo the operator's reclaim mid-flight.
			name: "the winner is this member's own claim, already terminating",
			winner: func(d *types.DriveMount) *corev1.PersistentVolumeClaim {
				claim := existingDriveClaim(d)
				deleted := metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
				claim.DeletionTimestamp = &deleted
				claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
				return claim
			},
			wantIs: errDriveClaimTerminating,
		},
		{
			// The presence-guarded arm, through this door too: a claim stamped
			// before wardyn.subject existed is this member's, and an upgrade
			// that turned it foreign would orphan every drive on the cluster.
			name:   "the winner predates the subject label and is still this member's",
			mutate: func(d *types.DriveMount) { d.SubjectHash = "1111111111111111aaaa" },
			winner: func(d *types.DriveMount) *corev1.PersistentVolumeClaim {
				claim := existingDriveClaim(d)
				delete(claim.Labels, labelDriveSubject)
				return claim
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drive := testDriveMount()
			if tc.mutate != nil {
				tc.mutate(drive)
			}
			var seed []runtime.Object
			if winner := tc.winner(drive); winner != nil {
				seed = append(seed, winner)
			}
			cs := fake.NewClientset(seed...)
			// The window itself: the FIRST lookup misses even though the winner
			// is already in the tracker, so the create is attempted; every later
			// lookup falls through to the tracker and sees what actually won.
			gets := 0
			cs.PrependReactor("get", "persistentvolumeclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
				if gets++; gets == 1 {
					return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "persistentvolumeclaims"}, drive.ObjectName)
				}
				return false, nil, nil
			})
			cs.PrependReactor("create", "persistentvolumeclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "persistentvolumeclaims"}, drive.ObjectName)
			})

			err := ensureDrivePVC(context.Background(), cs, testNamespace, drive)
			switch {
			case tc.wantIs == nil && err != nil:
				t.Fatalf("ensureDrivePVC: %v, want a race lost to this member's OWN claim to be tolerated", err)
			case tc.wantIs != nil && !errors.Is(err, tc.wantIs):
				t.Fatalf("err = %v, want errors.Is(err, %v) — the claim that won the race is not this run's to mount", err, tc.wantIs)
			}
			// The proof that the fix is the RE-READ and not a re-worded return:
			// two gets, one per side of the window. And nothing else — a driver
			// that "repaired" a foreign winner would reach for a verb the chart
			// does not grant, which is a 403 on every real cluster.
			if verbs := assertOnlyChartGrantedVerbs(t, cs); verbs["get"] != 2 || verbs["create"] != 1 {
				t.Errorf("claim verbs = %v, want the lost create race to re-read the claim it lost to (get=2, create=1)", verbs)
			}
		})
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
//
// The MODE is walked in both directions across all THREE places it has to
// arrive, because read-only here is not one flag but three independent ones —
// the pod Volume's PVC reference, the main container's VolumeMount, and the
// exec container's copy of it. Kubernetes ANDs them, so any one left false is a
// writable drive: a read-only allocation the agent can write to is the failure
// nobody sees until a member's files change under a share an admin published as
// read-only, and the exec container's is the copy most easily dropped, since it
// is assembled by Exec rather than by applyDriveToPod.
func TestExec_EphemeralContainerMountsTheDrive(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.9")
	installAgentRunningReactor(t, cs)

	for _, readOnly := range []bool{false, true} {
		spec := testSandboxSpec()
		spec.Drive = testDriveMount()
		spec.Drive.ReadOnly = readOnly
		sb, err := d.CreateSandbox(context.Background(), spec)
		if err != nil {
			t.Fatalf("CreateSandbox (ro=%v): %v", readOnly, err)
		}
		if _, err := d.Exec(context.Background(), sb.Ref, []string{"agent-run"}); err != nil {
			t.Fatalf("Exec (ro=%v): %v", readOnly, err)
		}

		pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get agent pod (ro=%v): %v", readOnly, err)
		}

		// (1) The pod VOLUME. A PVC reference carries its own ReadOnly, and it
		// is the one the kubelet honours for the volume as a whole.
		var vol *corev1.Volume
		for i := range pod.Spec.Volumes {
			if pod.Spec.Volumes[i].Name == driveVolumeName {
				vol = &pod.Spec.Volumes[i]
			}
		}
		if vol == nil || vol.PersistentVolumeClaim == nil {
			t.Fatalf("ro=%v: no %s volume backed by a claim; volumes=%+v", readOnly, driveVolumeName, pod.Spec.Volumes)
		}
		if vol.PersistentVolumeClaim.ClaimName != spec.Drive.ObjectName || vol.PersistentVolumeClaim.ReadOnly != readOnly {
			t.Errorf("ro=%v: pod volume = %+v, want claim %q readOnly=%v", readOnly, *vol.PersistentVolumeClaim, spec.Drive.ObjectName, readOnly)
		}

		// (2) The MAIN container's mount, by name — a sidecar is not where the
		// agent's drive goes.
		var mainMount *corev1.VolumeMount
		for _, c := range pod.Spec.Containers {
			if c.Name != mainContainerName {
				continue
			}
			for i := range c.VolumeMounts {
				if c.VolumeMounts[i].Name == driveVolumeName {
					mainMount = &c.VolumeMounts[i]
				}
			}
		}
		wantMain := corev1.VolumeMount{Name: driveVolumeName, MountPath: runner.DriveTarget, ReadOnly: readOnly}
		if mainMount == nil || *mainMount != wantMain {
			t.Errorf("ro=%v: main container mount = %+v, want %+v", readOnly, mainMount, wantMain)
		}

		// (3) The EPHEMERAL exec container's copy — the one the agent actually
		// runs in.
		if len(pod.Spec.EphemeralContainers) != 1 {
			t.Fatalf("ro=%v: ephemeral containers = %d, want 1", readOnly, len(pod.Spec.EphemeralContainers))
		}
		mounts := pod.Spec.EphemeralContainers[0].VolumeMounts
		if len(mounts) != 1 {
			t.Fatalf("ro=%v: exec container mounts = %v, want exactly the drive", readOnly, mounts)
		}
		if mounts[0] != wantMain {
			t.Errorf("ro=%v: exec container mount = %+v, want %+v", readOnly, mounts[0], wantMain)
		}

		// The claim the whole run resolved against is still a drive, not a
		// per-run object: teardownByRunID selects on the run label, and a drive
		// that acquired one would be deleted with the pod that mounted it.
		claim, err := cs.CoreV1().PersistentVolumeClaims(testNamespace).Get(context.Background(), spec.Drive.ObjectName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("ro=%v: get claim: %v", readOnly, err)
		}
		if _, has := claim.Labels[labelRun]; has {
			t.Errorf("ro=%v: the claim carries %s — teardownByRunID would delete the member's storage", readOnly, labelRun)
		}
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

// existingDriveClaim is the claim ensureDrivePVC would itself have created for
// drive — the shape a REUSE must find in the namespace with nothing to say about
// it. Built from the drive rather than written out, so a drift assertion that
// mutates one field is unambiguously about that field.
func existingDriveClaim(drive *types.DriveMount) *corev1.PersistentVolumeClaim {
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      drive.ObjectName,
			Namespace: testNamespace,
			Labels: map[string]string{
				labelManaged:      "true",
				labelDrive:        drive.DriveID.String(),
				labelDriveHome:    drive.HomeName,
				labelDriveSubject: drive.SubjectHash,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(driveRequestBytes(drive), resource.BinarySI)},
			},
		},
	}
	if drive.StorageClass != "" {
		class := drive.StorageClass
		claim.Spec.StorageClassName = &class
	}
	return claim
}

// TestEnsureDrivePVC_RefusesAMountTheApiserverWould covers the driver's OWN
// validation of the mount it is handed. The control plane refuses a non-DNS-1123
// home on a Kubernetes backend, and this driver does not trust it to: the name
// crosses a process boundary, and a driver that trusts its input has no
// fail-closed path left — only a 422 from the apiserver, mid-dispatch, as
// somebody's run failure hint.
//
// The underscore case is the motivating one and is not hypothetical: an Entra
// `sub` is base64url and routinely carries `_`, which is legal in a Docker
// volume name and illegal in a claim's.
func TestEnsureDrivePVC_RefusesAMountTheApiserverWould(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(*types.DriveMount)
		want  error
	}{
		{"underscore, the Entra sub case", func(d *types.DriveMount) { d.ObjectName = "wardyn-drive-research-d_9f3a1c" }, errDriveNameInvalid},
		{"trailing dash", func(d *types.DriveMount) { d.ObjectName = "wardyn-drive-research-" }, errDriveNameInvalid},
		{"uppercase", func(d *types.DriveMount) { d.ObjectName = "Wardyn-Drive-Research" }, errDriveNameInvalid},
		{"empty", func(d *types.DriveMount) { d.ObjectName = "" }, errDriveNameInvalid},
		{"consecutive dots", func(d *types.DriveMount) { d.ObjectName = "wardyn-drive-research..d" }, errDriveNameInvalid},
		// A legal object name whose HOME is not a legal LABEL VALUE. The two
		// alphabets differ, so the home gets its own check rather than riding
		// the object name's.
		{"home too long for a label value", func(d *types.DriveMount) { d.HomeName = strings.Repeat("a", 64) }, errDriveNameInvalid},
		{"home cannot open a label value", func(d *types.DriveMount) { d.HomeName = "-bsmith" }, errDriveNameInvalid},
		// The API refuses a zero allocation on a managed drive; so does this,
		// because a zero request is not a claim any storage class will bind.
		// Its OWN sentinel: errDriveNameInvalid's member-facing words name the
		// directory-name field, and no value in that field produces this.
		{"zero allocation on a managed drive", func(d *types.DriveMount) { d.SizeMiB = 0 }, errDriveAllocationInvalid},
		{"negative allocation", func(d *types.DriveMount) { d.SizeMiB = -1 }, errDriveAllocationInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset()
			drive := testDriveMount()
			tc.shape(drive)

			err := ensureDrivePVC(context.Background(), cs, testNamespace, drive)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want errors.Is(err, %v)", err, tc.want)
			}
			if n := len(cs.Actions()); n != 0 {
				t.Errorf("issued %d API calls, want none — an illegal name is this driver's refusal to make, not the apiserver's", n)
			}
		})
	}
}

// TestEnsureDrivePVC_RefusesAMountAwayFromTheDriveTarget covers the field on
// this struct that nobody authors and that therefore nobody was checking: the
// mount's Target, which becomes the agent pod's VolumeMount.MountPath.
//
// Every other rule in validateDriveMount refuses a mount that would FAIL. This
// one refuses a mount that would SUCCEED at the wrong place: a drive is
// persistent and survives the run, so landing it on /home/agent/.claude puts a
// writable, member-owned, cross-run volume over the injected credential
// directory, and landing it on / puts one over the image. The control plane
// copies runner.DriveTarget rather than deriving it, so the only way either
// value arrives is a change nobody meant to make — which is the case a refusal
// is for.
func TestEnsureDrivePVC_RefusesAMountAwayFromTheDriveTarget(t *testing.T) {
	for _, target := range []string{
		"/home/agent/.claude", // over the injected credentials
		"/",                   // over the image
		"/home/agent/drive/",  // the reserved path, differently spelled: not equal is not equal
		"",
	} {
		t.Run(target, func(t *testing.T) {
			cs := fake.NewClientset()
			drive := testDriveMount()
			drive.Target = target

			err := ensureDrivePVC(context.Background(), cs, testNamespace, drive)
			if !errors.Is(err, errDriveTargetInvalid) {
				t.Fatalf("err = %v, want errors.Is(err, errDriveTargetInvalid)", err)
			}
			if !strings.Contains(err.Error(), runner.DriveTarget) {
				t.Errorf("err = %q, want it to name the one path a drive may bind at", err.Error())
			}
			if n := len(cs.Actions()); n != 0 {
				t.Errorf("issued %d API calls, want none — the target is wrong before anything is asked of the cluster", n)
			}
		})
	}
}

// TestEnsureDrivePVC_ShareNeedsNoAllocation is the negative control for the size
// guard above: a static share's claim is never created, its size is a display
// value (StorageEnforcementExternal), and a zero there must not refuse the run.
func TestEnsureDrivePVC_ShareNeedsNoAllocation(t *testing.T) {
	drive := testDriveMount()
	drive.Backend = types.DriveBackendK8sPVCStatic
	drive.Enforcement = types.StorageEnforcementExternal
	drive.SizeMiB = 0
	cs := fake.NewClientset(&corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: drive.ObjectName, Namespace: testNamespace},
	})

	if err := ensureDrivePVC(context.Background(), cs, testNamespace, drive); err != nil {
		t.Fatalf("ensureDrivePVC: %v, want a share with no allocation to mount", err)
	}
}

// assertRefusalKeepsClusterNamesToItself is the assertion every 403 arm shares.
// A CreateSandbox error becomes the run's failure hint verbatim and is read by
// the MEMBER whose run failed, so the apiserver's own refusal — which spells
// `system:serviceaccount:<namespace>:<name>` — may not survive into it. The
// operator loses nothing: refuseForbiddenDriveClaim slogs the raw text with the
// claim name that is in both halves.
func assertRefusalKeepsClusterNamesToItself(t *testing.T, err error) {
	t.Helper()
	for _, leaked := range []string{"system:serviceaccount", testNamespace, "cannot get resource", "cannot create resource"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("err = %q, want it NOT to carry %q — the run's failure hint is member-visible", err.Error(), leaked)
		}
	}
}

// TestEnsureDrivePVC_ForbiddenLookupNamesTheSwitch is the DEFAULT deployment's
// failure and the one the review found unmapped: userDrives.enabled is off out
// of the box, so the Role has no persistentvolumeclaims rule at all and the
// LOOKUP is refused — before any Create the old code was the only mapper of.
// Unmapped it surfaced the apiserver's own "cannot get resource" text, which
// names no switch an operator could flip.
func TestEnsureDrivePVC_ForbiddenLookupNamesTheSwitch(t *testing.T) {
	for _, backend := range []types.DriveBackend{types.DriveBackendK8sPVC, types.DriveBackendK8sPVCStatic} {
		t.Run(string(backend), func(t *testing.T) {
			cs := fake.NewClientset()
			drive := testDriveMount()
			drive.Backend = backend
			cs.PrependReactor("get", "persistentvolumeclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(
					schema.GroupResource{Resource: "persistentvolumeclaims"}, drive.ObjectName,
					errors.New(`persistentvolumeclaims is forbidden: User "system:serviceaccount:wardyn:wardyn" cannot get resource`))
			})

			err := ensureDrivePVC(context.Background(), cs, testNamespace, drive)
			if !errors.Is(err, errDrivePVCForbidden) {
				t.Fatalf("err = %v, want errors.Is(err, errDrivePVCForbidden)", err)
			}
			for _, want := range []string{drive.ObjectName, "persistentvolumeclaims", "userDrives.enabled"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to name %q", err.Error(), want)
				}
			}
			assertRefusalKeepsClusterNamesToItself(t, err)
			if strings.Contains(err.Error(), "ResourceQuota") {
				t.Errorf("err = %q, want ONE remedy: an RBAC 403 must not also hand the reader the quota one", err.Error())
			}
			if verbs := countPVCVerbs(cs); verbs["create"] != 0 {
				t.Errorf("claim verbs = %v, want no create after a refused lookup", verbs)
			}
		})
	}
}

// TestEnsureDrivePVC_ForbiddenCarriesTheApiserverQuotaText pins the OTHER cause
// of a 403, which no status code distinguishes from the RBAC one and which takes
// the opposite remedy: a namespace ResourceQuota.
//
// The apiserver's message used to be interpolated ahead of a sentinel that
// carried BOTH remedies, leaving the reader to pick — which meant the raw 403
// had to be shown, and a raw 403 spells the runs namespace and the runner's
// ServiceAccount to whichever member's run failed. The driver picks instead, on
// the same substring the reader would have used, and this is the test that the
// picking still works with the evidence no longer on display.
func TestEnsureDrivePVC_ForbiddenCarriesTheApiserverQuotaText(t *testing.T) {
	cs := fake.NewClientset()
	drive := testDriveMount()
	cs.PrependReactor("create", "persistentvolumeclaims", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "persistentvolumeclaims"}, drive.ObjectName,
			errors.New("exceeded quota: storage-quota, requested: requests.storage=10Gi, used: requests.storage=95Gi, limited: requests.storage=100Gi"))
	})

	err := ensureDrivePVC(context.Background(), cs, testNamespace, drive)
	if !errors.Is(err, errDrivePVCForbidden) {
		t.Fatalf("err = %v, want errors.Is(err, errDrivePVCForbidden)", err)
	}
	got := err.Error()
	if !strings.Contains(got, "ResourceQuota") || !strings.Contains(got, drive.ObjectName) {
		t.Errorf("err = %q, want the claim name and the sentence that tells the reader it is a quota, not RBAC", got)
	}
	// ONE remedy, not two: the whole point of choosing is that the reader is not
	// handed the RBAC instruction for a problem RBAC will not fix.
	if strings.Contains(got, "userDrives.enabled") {
		t.Errorf("err = %q, want the quota remedy ALONE — the chart switch is not this reader's move", got)
	}
	assertRefusalKeepsClusterNamesToItself(t, err)
}

// TestEnsureDrivePVC_RefusesATerminatingClaim is the one existing-claim state
// that cannot be a warning. A finalizer-pinned claim admits no new pod, so
// reusing it hangs the run until the dispatch timeout with no readable cause;
// and re-creating it under the same name would undo the reclaim an operator is
// in the middle of, handing the member an empty volume where their files were.
func TestEnsureDrivePVC_RefusesATerminatingClaim(t *testing.T) {
	drive := testDriveMount()
	claim := existingDriveClaim(drive)
	deleted := metav1.NewTime(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	claim.DeletionTimestamp = &deleted
	claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
	cs := fake.NewClientset(claim)

	err := ensureDrivePVC(context.Background(), cs, testNamespace, drive)
	if !errors.Is(err, errDriveClaimTerminating) {
		t.Fatalf("err = %v, want errors.Is(err, errDriveClaimTerminating)", err)
	}
	if verbs := countPVCVerbs(cs); verbs["create"] != 0 {
		t.Errorf("claim verbs = %v, want no create over a claim somebody is deliberately deleting", verbs)
	}
	if got := err.Error(); !strings.Contains(got, drive.ObjectName) {
		t.Errorf("err = %q, want it to name the claim", got)
	}
}

// TestEnsureDrivePVC_RefusesAClaimThatIsNotThisMembers is the fail-closed half
// of reuse, and the one whose wrong answer is a data breach rather than a
// confusing log line: a claim found under a member's object name is mounted
// ONLY when its own labels say it is that member's storage.
//
// The name cannot decide it. `wardyn-drive-<slug>-<home>` joins two
// variable-width fields with the separator both of them admit, so the ambiguous
// pair below — drive "eng" with home "us-bob", and drive "eng-us" with home
// "bob" — resolves to ONE claim name; the home-only case is the same collision
// arriving through a rename or a changed home template. Wardyn holds no delete
// verb and cannot repair either, so a refused run is the whole remedy the
// driver has.
func TestEnsureDrivePVC_RefusesAClaimThatIsNotThisMembers(t *testing.T) {
	// The ambiguous pair, written out once: two DIFFERENT drive rows whose
	// (slug, home) pairs fold to the same object name.
	const collidingName = "wardyn-drive-eng-us-bob"
	otherDriveID := uuid.MustParse("11111111-2222-3333-4444-555555555555")

	for _, tc := range []struct {
		name  string
		drive func() *types.DriveMount
		claim func(*types.DriveMount) *corev1.PersistentVolumeClaim
		want  string
	}{
		{
			// This run belongs to drive "eng" / home "us-bob"; the claim already in
			// the namespace was provisioned for drive "eng-us" / home "bob".
			name: "the ambiguous pair: another drive's claim under one name",
			drive: func() *types.DriveMount {
				d := testDriveMount()
				d.ObjectName, d.HomeName = collidingName, "us-bob"
				return d
			},
			claim: func(d *types.DriveMount) *corev1.PersistentVolumeClaim {
				other := testDriveMount()
				other.DriveID, other.ObjectName, other.HomeName = otherDriveID, collidingName, "bob"
				return existingDriveClaim(other)
			},
			want: labelDrive,
		},
		{
			// Same drive row, different person: the rename/home-template case,
			// which the drive-id label alone cannot catch.
			name:  "the home-only mismatch: this drive, another person's home",
			drive: func() *types.DriveMount { return testDriveMount() },
			claim: func(d *types.DriveMount) *corev1.PersistentVolumeClaim {
				other := testDriveMount()
				other.HomeName = "d-0000aa"
				claim := existingDriveClaim(other)
				claim.Name = d.ObjectName
				return claim
			},
			want: labelDriveHome,
		},
		{
			// A claim carrying none of the three labels was never created by this
			// driver — every claim it has ever written stamps all three.
			name:  "an unlabelled object under a member's claim name",
			drive: func() *types.DriveMount { return testDriveMount() },
			claim: func(d *types.DriveMount) *corev1.PersistentVolumeClaim {
				claim := existingDriveClaim(d)
				claim.Labels = nil
				return claim
			},
			want: labelDrive,
		},
		{
			// A SHARE must never resolve to a managed claim: an admin pointing a
			// share at that name would hand every member of it one person's drive.
			name: "a share whose claim is somebody's managed drive",
			drive: func() *types.DriveMount {
				d := testDriveMount()
				d.Backend = types.DriveBackendK8sPVCStatic
				d.Enforcement = types.StorageEnforcementExternal
				return d
			},
			claim: func(d *types.DriveMount) *corev1.PersistentVolumeClaim {
				managed := testDriveMount()
				return existingDriveClaim(managed) // carries wardyn.managed=true
			},
			want: labelManaged,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drive := tc.drive()
			cs := fake.NewClientset(tc.claim(drive))

			err := ensureDrivePVC(context.Background(), cs, testNamespace, drive)
			if !errors.Is(err, errDriveClaimForeign) {
				t.Fatalf("err = %v, want errors.Is(err, errDriveClaimForeign)", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to name the label that decided it (%s)", err.Error(), tc.want)
			}
			// Never a create: the claim under that name is somebody's data and
			// the driver holds no verb that could move it aside — asserted as
			// the whole verb set, since "move it aside" is precisely the repair
			// a driver without a delete verb must never be written to attempt.
			if verbs := assertOnlyChartGrantedVerbs(t, cs); verbs["create"] != 0 {
				t.Errorf("claim verbs = %v, want no create over a claim this drive does not own", verbs)
			}
		})
	}

	// Negative control, and the reason the share arm is scoped to wardyn.managed
	// rather than to "has any label": an admin's own claim carries labels of
	// their own, and a share must still mount over them.
	share := testDriveMount()
	share.Backend = types.DriveBackendK8sPVCStatic
	share.Enforcement = types.StorageEnforcementExternal
	admins := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: share.ObjectName, Namespace: testNamespace,
		Labels: map[string]string{"app.kubernetes.io/managed-by": "corp-storage", "team": "eng"},
	}}
	if err := ensureDrivePVC(context.Background(), fake.NewClientset(admins), testNamespace, share); err != nil {
		t.Errorf("ensureDrivePVC over an administrator's own labelled share: %v, want it mounted", err)
	}
}

// TestDriveClaimDrift covers the SILENT-REUSE finding: an existing claim whose
// SHAPE disagrees with the drive it was resolved from is mounted anyway (the
// claim is the member's data, and a PVC request cannot be shrunk), but the
// disagreement is named rather than swallowed.
//
// Every case here therefore also asserts the run still succeeds. The identity
// labels are NOT a shape and are not here: they refuse, and
// TestEnsureDrivePVC_RefusesAClaimThatIsNotThisMembers owns them.
func TestDriveClaimDrift(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(*corev1.PersistentVolumeClaim)
		want  string
	}{
		{"a foreign storage class", func(c *corev1.PersistentVolumeClaim) {
			other := "slow-nfs"
			c.Spec.StorageClassName = &other
		}, "storage class"},
		{"no storage class at all", func(c *corev1.PersistentVolumeClaim) {
			c.Spec.StorageClassName = nil
		}, "storage class"},
		{"a smaller request", func(c *corev1.PersistentVolumeClaim) {
			c.Spec.Resources.Requests[corev1.ResourceStorage] = *resource.NewQuantity(1024*1024*1024, resource.BinarySI)
		}, "request is"},
		{"an access mode a managed drive never provisions", func(c *corev1.PersistentVolumeClaim) {
			c.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
		}, "access modes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			drive := testDriveMount()
			claim := existingDriveClaim(drive)
			tc.shape(claim)

			drift := driveClaimDrift(claim, drive)
			if len(drift) != 1 || !strings.Contains(drift[0], tc.want) {
				t.Fatalf("drift = %v, want exactly one entry naming %q", drift, tc.want)
			}
			// Drift is a warning, never a refusal: the run still gets its data.
			cs := fake.NewClientset(claim)
			if err := ensureDrivePVC(context.Background(), cs, testNamespace, drive); err != nil {
				t.Errorf("ensureDrivePVC: %v, want a drifted claim mounted with a warning, not refused", err)
			}
		})
	}
}

// TestDriveClaimDrift_CleanClaimAndShare pins the two silences: the claim this
// driver would itself have created says nothing, and a static share's SHAPE is
// never compared at all — its class and size are facts about an admin's
// storage, not drift from anything Wardyn asserted.
func TestDriveClaimDrift_CleanClaimAndShare(t *testing.T) {
	drive := testDriveMount()
	if drift := driveClaimDrift(existingDriveClaim(drive), drive); len(drift) != 0 {
		t.Errorf("drift = %v on the claim ensureDrivePVC would itself create, want none", drift)
	}
	// The cluster-default case: the drive names no class, so the class the
	// default resolved to is not drift.
	defaulted := testDriveMount()
	defaulted.StorageClass = ""
	claim := existingDriveClaim(defaulted)
	resolved := "whatever-the-cluster-default-is"
	claim.Spec.StorageClassName = &resolved
	if drift := driveClaimDrift(claim, defaulted); len(drift) != 0 {
		t.Errorf("drift = %v, want none: an empty class means the cluster default, not a class named \"\"", drift)
	}

	share := testDriveMount()
	share.Backend = types.DriveBackendK8sPVCStatic
	share.Enforcement = types.StorageEnforcementExternal
	foreign := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: share.ObjectName, Namespace: testNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		},
	}
	if drift := driveClaimDrift(foreign, share); drift != nil {
		t.Errorf("drift = %v on a static share, want none — the claim is the admin's object", drift)
	}
}

// TestApplyDriveToPod_AppendsRatherThanAssigns is the coexistence pin. Nothing
// else puts a Volume, a VolumeMount or a pod-level SecurityContext on the agent
// pod today, so an assignment is correct right now and silently wrong the first
// time anything does — and an assignment does not fail a test on that day, it
// drops the other half. This test builds the pod that day produces.
func TestApplyDriveToPod_AppendsRatherThanAssigns(t *testing.T) {
	drive := testDriveMount()
	existingMount := corev1.VolumeMount{Name: "member-work", MountPath: "/work"}
	nonRoot := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"checksum/policy": "abc123"}},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name:         "member-work",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &nonRoot},
			Containers: []corev1.Container{
				{Name: mainContainerName, VolumeMounts: []corev1.VolumeMount{existingMount}},
				{Name: "sidecar"},
			},
		},
	}

	applyDriveToPod(pod, drive, "runsc")
	spec := &pod.Spec

	// The gVisor annotation is SET, not assigned over: a pod already carrying an
	// annotation keeps it (the fourth field of the append-never-assign rule).
	if got := pod.Annotations[driveDirectfsAnnotation]; got != "off" {
		t.Errorf("%s = %q, want %q on a runsc pod", driveDirectfsAnnotation, got, "off")
	}
	if got := pod.Annotations["checksum/policy"]; got != "abc123" {
		t.Errorf("pre-existing annotation = %q, want it untouched — the drive SET one key, it must not replace the map", got)
	}
	if len(spec.Volumes) != 2 || spec.Volumes[0].Name != "member-work" || spec.Volumes[1].Name != driveVolumeName {
		t.Fatalf("volumes = %+v, want the pre-existing one AND the drive", spec.Volumes)
	}
	mounts := spec.Containers[0].VolumeMounts
	wantDrive := corev1.VolumeMount{Name: driveVolumeName, MountPath: runner.DriveTarget, ReadOnly: false}
	if len(mounts) != 2 || mounts[0] != existingMount || mounts[1] != wantDrive {
		t.Fatalf("main container mounts = %+v, want [%+v %+v]", mounts, existingMount, wantDrive)
	}
	// By NAME, not by index: a sidecar is not where the agent's drive goes.
	if got := spec.Containers[1].VolumeMounts; len(got) != 0 {
		t.Errorf("sidecar mounts = %+v, want none", got)
	}
	sc := spec.SecurityContext
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("pod securityContext lost RunAsNonRoot — the drive SET two fields, it must not replace the struct")
	}
	if sc.FSGroup == nil || *sc.FSGroup != driveFSGroup {
		t.Errorf("fsGroup = %v, want %d", sc.FSGroup, driveFSGroup)
	}
	if sc.FSGroupChangePolicy == nil || *sc.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
		t.Errorf("fsGroupChangePolicy = %v, want OnRootMismatch", sc.FSGroupChangePolicy)
	}

	// A pod that is NOT a gVisor pod gets no annotation map invented for it: the
	// annotation is a runsc-only request, and a nil map is the shape every
	// drive pod had before it existed.
	plain := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: mainContainerName}}}}
	applyDriveToPod(plain, drive, "")
	if plain.Annotations != nil {
		t.Errorf("annotations = %v on a pod with no RuntimeClass, want nil", plain.Annotations)
	}
}

// TestEnsureDrivePVC_RefusesAClaimStampedForAnotherPerson is the third identity
// label: when a home template folds two principals onto ONE home (email_local
// on two domains), drive id and home both match and only the subject digest
// the resolver carried can tell the claims apart. A claim stamped before the
// label existed carries none and still mounts — the guard is on presence, so
// an upgrade never turns every member's drive foreign.
func TestEnsureDrivePVC_RefusesAClaimStampedForAnotherPerson(t *testing.T) {
	t.Run("another person's digest is refused before any create", func(t *testing.T) {
		drive := testDriveMount()
		drive.SubjectHash = "1111111111111111aaaa"
		claim := existingDriveClaim(drive)
		claim.Labels[labelDriveSubject] = "2222222222222222bbbb"
		cs := fake.NewClientset(claim)

		err := ensureDrivePVC(context.Background(), cs, testNamespace, drive)
		if !errors.Is(err, errDriveClaimForeign) {
			t.Fatalf("err = %v, want errors.Is(err, errDriveClaimForeign)", err)
		}
		if verbs := countPVCVerbs(cs); verbs["create"] != 0 {
			t.Errorf("claim verbs = %v, want no create over somebody else's claim", verbs)
		}
		if got := err.Error(); !strings.Contains(got, labelDriveSubject) {
			t.Errorf("err = %q, want it to name the deciding label", got)
		}
	})
	t.Run("a claim stamped before the label existed still mounts", func(t *testing.T) {
		drive := testDriveMount()
		drive.SubjectHash = "1111111111111111aaaa"
		claim := existingDriveClaim(drive)
		delete(claim.Labels, labelDriveSubject)
		cs := fake.NewClientset(claim)

		if err := ensureDrivePVC(context.Background(), cs, testNamespace, drive); err != nil {
			t.Fatalf("ensureDrivePVC: %v, want a pre-label claim to be reused", err)
		}
		if verbs := countPVCVerbs(cs); verbs["create"] != 0 {
			t.Errorf("claim verbs = %v, want reuse, not create", verbs)
		}
	})
}

// TestClassesDeclaresDrivesAndBindsThem pins the DECLARATION against the
// BEHAVIOUR: this substrate says it can bind a member's drive, and it does.
//
// The two halves are one fact, and drift between them is member-visible in
// whichever direction it drifts. The control plane admits or refuses a
// drive-carrying run by reading Classes().UserDrives, at create and again at
// preflight (api.driveIsMountableHere). A driver that declares FALSE while it
// can bind refuses every allocation on a deployment that would have worked; one
// that declares TRUE while it cannot is the run that previews green, answers
// 201 and then fails at dispatch.
//
// This is the D4 half of that pair. Before D4 the same assertion ran the other
// way, over the errDriveUnsupported stub this driver no longer has: it asserted
// that CreateSandbox refused a drive AND that Classes declared false. D4 landed
// the claim (ensureDrivePVC, applyDriveToPod) and the declaration moved with it.
// What is pinned never changed — the two must agree.
//
// The CLAIM's own behaviour — naming, labels, reuse, the RBAC verbs, every
// fail-closed arm — is this file's other tests; this asserts only that a bind
// happens at all, because that is what the declaration claims.
func TestClassesDeclaresDrivesAndBindsThem(t *testing.T) {
	ctx := context.Background()
	d, cs := newTestDriver(t, Config{})

	support, err := d.Classes(ctx)
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	if !support.UserDrives {
		t.Fatal("Classes reports UserDrives=false while this driver binds drives — " +
			"the control plane would refuse every allocation on this deployment")
	}

	// …and the claim is true. A declaration nothing exercises is the half of the
	// pair that rots.
	cs.ClearActions()
	if err := ensureDrivePVC(ctx, cs, testNamespace, testDriveMount()); err != nil {
		t.Fatalf("Classes declares UserDrives, but ensureDrivePVC refused: %v", err)
	}
	if pvcs := createdPVCs(t, cs); len(pvcs) != 1 {
		t.Errorf("claims created = %d, want exactly 1 — the declaration promises a bind", len(pvcs))
	}
}

// docCommentAbove returns the contiguous "//" block immediately above the first
// line of file starting with decl — the comment a maintainer reads before
// touching that declaration.
func docCommentAbove(t *testing.T, file, decl string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	lines := strings.Split(string(src), "\n")
	at := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), decl) {
			at = i
			break
		}
	}
	if at < 0 {
		t.Fatalf("%s: no line starting with %q", file, decl)
	}
	start := at
	for start > 0 && strings.HasPrefix(strings.TrimSpace(lines[start-1]), "//") {
		start--
	}
	if start == at {
		t.Fatalf("%s: %q has no doc comment", file, decl)
	}
	return strings.Join(lines[start:at], "\n")
}

// res2-01: the annotation wardynd stamps here is INERT — runsc drops a mount
// hint that carries no share/source/type, and binds a hint to a mount by SOURCE
// PATH, not by the <NAME> in the key. docs/OPERATIONS.md ("User drives on
// Kubernetes") says so with the upstream reference; these comments described it
// as a working request whose only caveat was containerd forwarding, which sends
// the next reader to configure a node setting that changes nothing. The
// stamping itself is deliberate and unchanged — TestApplyDriveToPod pins it.
func TestDirectfsCommentsSayTheAnnotationIsInert(t *testing.T) {
	for _, tc := range []struct {
		decl string
		// falseClaim must NOT survive: the sentence that was wrong.
		falseClaim string
	}{
		{"driveDirectfsAnnotation = ", "keys the option by the mount's name"},
		{"func applyDriveToPod(", "It is a REQUEST, not an enforcement"},
	} {
		doc := docCommentAbove(t, "drives.go", tc.decl)
		if strings.Contains(doc, tc.falseClaim) {
			t.Errorf("drives.go %q still claims %q — runsc discards this hint (missing share/source/type) and matches hints by mount source, so it is not a request that a node config would grant:\n%s", tc.decl, tc.falseClaim, doc)
		}
		if !strings.Contains(doc, "inert") && !strings.Contains(doc, "discard") {
			t.Errorf("drives.go %q does not say the annotation is inert/discarded, so the next reader will still treat it as the remedy:\n%s", tc.decl, doc)
		}
		if !strings.Contains(doc, "--directfs=false") && !strings.Contains(doc, "driveDirectfsAnnotation") {
			t.Errorf("drives.go %q names neither the working remedy (--directfs=false) nor where it is explained:\n%s", tc.decl, doc)
		}
	}
}
