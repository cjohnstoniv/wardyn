// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

// scratch_test.go covers the three emptyDir scratch volumes that make
// disk_mib bound the AGENT's writes on this substrate — 0.7.4's known gap (a),
// narrowed further by #164's cache volume. The interesting assertions are not
// "the fields are set": they are the shape constraints that a fake clientset
// would otherwise let through and a real apiserver rejects (no SubPath on a
// mount Exec copies onto an ephemeral container), and the negative control
// that a budget-less run keeps the volume-less pod this substrate has always
// produced.

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// wantScratch is the mount path each scratch volume must land on. Spelled here
// rather than read from the constants it asserts about, so renaming a path in
// naming.go reds this instead of silently agreeing with itself.
var wantScratch = map[string]string{
	"wardyn-tmp":   "/tmp",
	"wardyn-work":  "/home/agent/work",
	"wardyn-cache": "/home/agent/.cache",
}

// TestEphemeralScratchVolumes_ThreeWholeVolumeEmptyDirsAtTheBudget pins the
// volumes themselves: every path the agent writes through disk_mib, each a
// WHOLE-volume mount of a disk-backed emptyDir whose SizeLimit is the run's
// own disk_mib.
//
// The medium is the subtle one. `medium: Memory` would make the emptyDir a
// tmpfs counted against the container's MEMORY limit instead of its ephemeral
// storage — the pod would OOM-kill on a big clone rather than be evicted for
// disk, and disk_mib would still bound nothing. Empty medium is the disk-backed
// one, and it is the only one this fix can be built on.
func TestEphemeralScratchVolumes_ThreeWholeVolumeEmptyDirsAtTheBudget(t *testing.T) {
	vols, mounts := ephemeralScratchVolumes(64)
	if len(vols) != len(wantScratch) || len(mounts) != len(wantScratch) {
		t.Fatalf("ephemeralScratchVolumes(64) = %d volumes / %d mounts, want %d of each (%v)",
			len(vols), len(mounts), len(wantScratch), wantScratch)
	}
	want := resource.MustParse("64Mi")
	seen := map[string]bool{}
	for _, v := range vols {
		path, ok := wantScratch[v.Name]
		if !ok {
			t.Errorf("unexpected scratch volume %q; want exactly %v", v.Name, wantScratch)
			continue
		}
		seen[v.Name] = true
		if v.EmptyDir == nil {
			t.Errorf("%s is not an emptyDir (%+v) — only an emptyDir is metered as the POD's local ephemeral storage regardless of which container writes into it", v.Name, v.VolumeSource)
			continue
		}
		if v.EmptyDir.Medium != "" {
			t.Errorf("%s uses medium %q; want the disk-backed default — `Memory` is a tmpfs charged to the memory limit, so disk_mib would bound nothing", v.Name, v.EmptyDir.Medium)
		}
		if v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.Cmp(want) != 0 {
			t.Errorf("%s sizeLimit = %v, want %s (the run's disk_mib)", v.Name, v.EmptyDir.SizeLimit, want.String())
		}
		if got := mountFor(mounts, v.Name); got == nil || got.MountPath != path {
			t.Errorf("%s mount = %+v, want MountPath %q", v.Name, got, path)
		}
	}
	if len(seen) != len(wantScratch) {
		t.Errorf("scratch volumes present = %v, want %v", seen, wantScratch)
	}
	// One Quantity per volume: a shared pointer would alias two spec fields
	// onto the same object, so a later per-volume change would silently move
	// both. Checked pairwise so a fourth volume is covered without rewriting
	// this block again.
	for i := range vols {
		for j := i + 1; j < len(vols); j++ {
			if vols[i].EmptyDir != nil && vols[j].EmptyDir != nil &&
				vols[i].EmptyDir.SizeLimit == vols[j].EmptyDir.SizeLimit {
				t.Errorf("scratch volumes %q and %q share one *Quantity — give each its own, an aliased spec field is a change nobody means to make twice", vols[i].Name, vols[j].Name)
			}
		}
	}
}

// TestEphemeralScratchVolumes_NoBudgetAddsNothing is the negative control the
// whole change rests on: a run with no disk_mib keeps the volume-less pod this
// substrate produced before scratch existed. It mirrors resourceRequirements'
// own "absent means unbounded" shape — a zero budget must not quietly become a
// zero-sized emptyDir, which would be a sandbox that cannot write at all.
func TestEphemeralScratchVolumes_NoBudgetAddsNothing(t *testing.T) {
	for _, diskMiB := range []int64{0, -1} {
		vols, mounts := ephemeralScratchVolumes(diskMiB)
		if len(vols) != 0 || len(mounts) != 0 {
			t.Errorf("ephemeralScratchVolumes(%d) = %v / %v, want nothing at all", diskMiB, vols, mounts)
		}
	}
}

// TestCreateSandbox_ScratchReachesTheAgentsOwnContainer is the end-to-end half:
// CreateSandbox puts the volumes on the pod and the mounts on the MAIN
// container, and Exec — which is where the agent actually runs — carries the
// same mounts onto the ephemeral container by copying the main container's.
// Until 0.7.5 that ephemeral container had no mounts at all, which is precisely
// why disk_mib bounded nothing an agent wrote.
func TestCreateSandbox_ScratchReachesTheAgentsOwnContainer(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.11")
	installAgentRunningReactor(t, cs)

	spec := testSandboxSpec()
	spec.Resources.DiskMiB = 64
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

	for name, path := range wantScratch {
		if volumeFor(pod.Spec.Volumes, name) == nil {
			t.Errorf("pod carries no %q volume; volumes=%+v", name, pod.Spec.Volumes)
		}
		main := containerByName(pod.Spec.Containers, mainContainerName)
		if main == nil {
			t.Fatalf("pod has no %q container", mainContainerName)
		}
		if got := mountFor(main.VolumeMounts, name); got == nil || got.MountPath != path {
			t.Errorf("main container mount for %q = %+v, want MountPath %q", name, got, path)
		}
	}
	// Scratch goes to the agent and nothing else. The proxy is its own pod on
	// this substrate, so "the main container is the only container" is what
	// keeps that true as the pod grows a second one.
	if len(pod.Spec.Containers) != 1 {
		t.Errorf("agent pod has %d containers; the scratch append targets %q by name — re-read it if a second container ever joins", len(pod.Spec.Containers), mainContainerName)
	}
	if len(pod.Spec.EphemeralContainers) != 1 {
		t.Fatalf("ephemeral containers = %d, want 1", len(pod.Spec.EphemeralContainers))
	}
	for name, path := range wantScratch {
		if got := mountFor(pod.Spec.EphemeralContainers[0].VolumeMounts, name); got == nil || got.MountPath != path {
			t.Errorf("exec container mount for %q = %+v, want MountPath %q — the agent runs HERE, so a scratch volume it does not mount bounds nothing", name, got, path)
		}
	}
}

// TestCreateSandbox_NoMountCarriesASubPath is a REGRESSION PIN, green the day it
// was written, and it is the one assertion a fake clientset can still make about
// a rule only a real apiserver enforces: "Subpath mounts are not allowed for
// ephemeral containers" (corev1.VolumeMount's own contract). Exec copies the
// main container's mounts VERBATIM, so a subPath added anywhere in this package
// — scratch, drive, or anything later — would make UpdateEphemeralContainers
// fail for EVERY autonomous k8s run while every unit test here went on passing.
// The pod walked carries a drive as well, so the pin covers both mount sources.
func TestCreateSandbox_NoMountCarriesASubPath(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.12")
	installAgentRunningReactor(t, cs)

	spec := testSandboxSpec()
	spec.Resources.DiskMiB = 64
	spec.Drive = testDriveMount()
	// The managed-file mounts are the third source that reaches this walk, and
	// they are Secret volumes rather than emptyDirs — the shape most likely to
	// be written with a subPath, since one file in an existing directory is
	// exactly what subPath is for.
	spec.ManagedFiles = []runner.ManagedFile{{Path: runner.ManagedFileDir + "/managed-settings.json", Content: []byte("{}")}}
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

	walked := 0
	check := func(where string, mounts []corev1.VolumeMount) {
		for _, m := range mounts {
			walked++
			if m.SubPath != "" || m.SubPathExpr != "" {
				t.Errorf("%s mounts %q with subPath %q/%q — the apiserver REFUSES a subPath on an ephemeral container, and exec.go copies these mounts verbatim, so this fails every autonomous run on a real cluster",
					where, m.Name, m.SubPath, m.SubPathExpr)
			}
		}
	}
	for _, c := range pod.Spec.Containers {
		check("container "+c.Name, c.VolumeMounts)
	}
	for _, c := range pod.Spec.EphemeralContainers {
		check("ephemeral container "+c.Name, c.VolumeMounts)
	}
	// Vacuity guard: a walk over zero mounts would pass forever.
	if walked < len(wantScratch)+2 {
		t.Errorf("walked only %d mounts; want at least the %d scratch mounts plus the drive and the managed file — the walk found nothing to check", walked, len(wantScratch))
	}
}

// volumeFor / mountFor / containerByName look things up BY NAME rather than by
// index: the pod's volume list now has three sources (scratch, the drive, and
// whatever comes next) and an index-based assertion would red on ordering
// instead of on meaning.
func volumeFor(vols []corev1.Volume, name string) *corev1.Volume {
	for i := range vols {
		if vols[i].Name == name {
			return &vols[i]
		}
	}
	return nil
}

func mountFor(mounts []corev1.VolumeMount, name string) *corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == name {
			return &mounts[i]
		}
	}
	return nil
}

func containerByName(cs []corev1.Container, name string) *corev1.Container {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}
