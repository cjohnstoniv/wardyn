// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

// managed_files_test.go covers the root-owned managed-file delivery on this
// substrate. The interesting assertions are the ones a fake clientset would
// otherwise let through: the content leaves the pod spec (it rides the Secret),
// the mount is read-only and carries no SubPath (the apiserver refuses one on
// the ephemeral container Exec copies these onto), the items are EXPLICIT (a
// whole-Secret projection would drop the proxy config and every credential
// into a directory the agent reads), and the mount reaches the ephemeral
// container the agent actually runs in.

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	testManagedDir      = runner.ManagedFileDir
	testManagedSettings = testManagedDir + "/managed-settings.json"
	testManagedLocked   = testManagedDir + "/locked"
)

func managedSandboxSpec() runner.SandboxSpec {
	spec := testSandboxSpec()
	spec.ManagedFiles = []runner.ManagedFile{
		{Path: testManagedSettings, Mode: 0o644, Content: []byte(`{"managed":true}`)},
		{Path: testManagedLocked, Mode: 0o444, Content: []byte("locked")},
	}
	return spec
}

func TestCreateSandbox_DeliversManagedFiles(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.12")
	installAgentRunningReactor(t, cs)

	spec := managedSandboxSpec()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}

	// the content rides the per-run Secret, not the pod spec
	sec, err := cs.CoreV1().Secrets(testNamespace).Get(context.Background(), secretName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get run secret: %v", err)
	}
	if got := string(sec.Data[managedFileDataKey(0)]); got != `{"managed":true}` {
		t.Errorf("secret key %q = %q, want the first managed file's content", managedFileDataKey(0), got)
	}
	if got := string(sec.Data[managedFileDataKey(1)]); got != "locked" {
		t.Errorf("secret key %q = %q, want the second managed file's content", managedFileDataKey(1), got)
	}
	if _, ok := sec.Data[proxyConfigSecretKey]; !ok {
		t.Error("the managed files displaced the proxy config; they must join the per-run Secret, not replace its data")
	}

	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}

	// one read-only Secret volume, mounted at the managed directory
	main, ok := findContainer(pod.Spec.Containers, mainContainerName)
	if !ok {
		t.Fatal("no main container")
	}
	var mount *corev1.VolumeMount
	for i := range main.VolumeMounts {
		if main.VolumeMounts[i].MountPath == testManagedDir {
			mount = &main.VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatalf("no volume mounted at %s (mounts: %+v)", testManagedDir, main.VolumeMounts)
	}
	if !mount.ReadOnly {
		t.Error("the managed-file mount is writable; read-only is what makes the write fail with EROFS whatever the mode says")
	}
	if mount.SubPath != "" || mount.SubPathExpr != "" {
		t.Errorf("the managed-file mount carries a subPath (%q/%q); the apiserver refuses one on the ephemeral container Exec copies this onto", mount.SubPath, mount.SubPathExpr)
	}

	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == mount.Name {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.Secret == nil {
		t.Fatalf("volume %q is not a Secret volume: %+v", mount.Name, vol)
	}
	if vol.Secret.SecretName != secretName(spec.RunID) {
		t.Errorf("volume projects secret %q, want the per-run Secret %q", vol.Secret.SecretName, secretName(spec.RunID))
	}

	// EXPLICIT items, and only the managed keys: a whole-Secret projection
	// would publish the proxy config and every credential-bearing env value
	// into a directory the agent reads.
	if len(vol.Secret.Items) != 2 {
		t.Fatalf("volume projects %d items, want exactly the 2 managed files: %+v", len(vol.Secret.Items), vol.Secret.Items)
	}
	wantModes := map[string]int32{"managed-settings.json": 0o644, "locked": 0o444}
	for _, it := range vol.Secret.Items {
		want, known := wantModes[it.Path]
		if !known {
			t.Errorf("volume projects an unexpected item %q", it.Path)
			continue
		}
		if !strings.HasPrefix(it.Key, "managed.") {
			t.Errorf("item %q projects key %q, which is not a managed-file key — a non-managed key here publishes the run's credentials into the sandbox", it.Path, it.Key)
		}
		if it.Mode == nil || *it.Mode != want {
			t.Errorf("item %q mode = %v, want %04o", it.Path, it.Mode, want)
		}
		delete(wantModes, it.Path)
	}
	if len(wantModes) != 0 {
		t.Errorf("the volume never projects %v", wantModes)
	}

	// and the mount reaches the container the agent runs in
	if _, err := d.Exec(context.Background(), sb.Ref, []string{"agent-run"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	pod, err = cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-get agent pod: %v", err)
	}
	if len(pod.Spec.EphemeralContainers) == 0 {
		t.Fatal("Exec added no ephemeral container")
	}
	found := false
	for _, m := range pod.Spec.EphemeralContainers[0].VolumeMounts {
		if m.MountPath == testManagedDir {
			found = true
			if !m.ReadOnly {
				t.Error("the ephemeral container's managed-file mount is writable")
			}
		}
	}
	if !found {
		t.Errorf("the ephemeral container the agent runs in has no mount at %s; the ceiling would be mounted into a container the agent never touches", testManagedDir)
	}
}

// A budget-less, managed-file-less run must produce the pod this substrate has
// always produced: the field is additive.
func TestCreateSandbox_NoManagedFilesNoVolume(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.12")
	installAgentRunningReactor(t, cs)

	spec := testSandboxSpec()
	spec.Resources.DiskMiB = 0
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	if len(pod.Spec.Volumes) != 0 {
		t.Errorf("a run with no managed files and no disk budget got %d volumes: %+v", len(pod.Spec.Volumes), pod.Spec.Volumes)
	}
	sec, err := cs.CoreV1().Secrets(testNamespace).Get(context.Background(), secretName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get run secret: %v", err)
	}
	for k := range sec.Data {
		if strings.HasPrefix(k, "managed.") {
			t.Errorf("the run secret carries %q for a spec with no managed files", k)
		}
	}
}

// Two files in two directories are two mount points, each projecting only its
// own file: a single volume mounted twice would publish both files in both
// places.
func TestManagedFileVolumesGroupByDirectory(t *testing.T) {
	spec := testSandboxSpec()
	files := []runner.ManagedFile{
		{Path: "/etc/wardyn/agent/settings.json", Content: []byte("a")},
		{Path: "/opt/wardyn/policy/rules", Content: []byte("b")},
		{Path: "/etc/wardyn/agent/second", Content: []byte("c")},
	}
	vols, mounts := managedFileVolumes(spec.RunID, files)
	if len(vols) != 2 || len(mounts) != 2 {
		t.Fatalf("got %d volumes / %d mounts, want 2 each (one per distinct directory)", len(vols), len(mounts))
	}
	byPath := map[string]corev1.VolumeMount{}
	for _, m := range mounts {
		byPath[m.MountPath] = m
	}
	etc, ok := byPath["/etc/wardyn/agent"]
	if !ok {
		t.Fatalf("no mount at /etc/wardyn/agent: %+v", mounts)
	}
	opt, ok := byPath["/opt/wardyn/policy"]
	if !ok {
		t.Fatalf("no mount at /opt/wardyn/policy: %+v", mounts)
	}
	if etc.Name == opt.Name {
		t.Fatal("both directories share one volume; each must project only its own files")
	}
	byName := map[string]corev1.Volume{}
	for _, v := range vols {
		byName[v.Name] = v
	}
	if n := len(byName[etc.Name].Secret.Items); n != 2 {
		t.Errorf("/etc/wardyn/agent projects %d items, want 2", n)
	}
	if n := len(byName[opt.Name].Secret.Items); n != 1 {
		t.Errorf("/opt/wardyn/policy projects %d items, want 1", n)
	}
}

// An unusable spec is refused before the namespace has anything in it.
func TestCreateSandbox_RefusesAnInvalidManagedFile(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	cs.ClearActions()

	spec := testSandboxSpec()
	spec.ManagedFiles = []runner.ManagedFile{{Path: testManagedSettings, Mode: 0o666, Content: []byte("{}")}}
	_, err := d.CreateSandbox(context.Background(), spec)
	if err == nil {
		t.Fatal("CreateSandbox accepted a world-writable managed file")
	}
	if !strings.Contains(err.Error(), "writable") {
		t.Errorf("refusal = %v, want it to name the writable mode", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			t.Errorf("the refusal created a %s; preflight must fail before anything exists", a.GetResource().Resource)
		}
	}
}

// secretSandboxSpec is testSandboxSpec plus a managed-settings ceiling and one
// delivered secret, as dispatch composes a run carrying both.
func secretSandboxSpec() runner.SandboxSpec {
	spec := testSandboxSpec()
	spec.ManagedFiles = []runner.ManagedFile{
		{Path: testManagedSettings, Mode: 0o644, Content: []byte(`{"managed":true}`)},
		{Path: runner.ComponentSecretDir + "/api-token", Mode: runner.ComponentSecretFileMode, AgentOwned: true, Content: []byte("file-secret-value")},
	}
	return spec
}

// What this substrate gives a delivered secret, pinned: the value rides the
// per-run Secret (never the pod spec), in its OWN read-only volume at the fixed
// directory projecting only that key, at 0440 with the pod's fsGroup 1000 as
// the file's group, mounted on the agent pod's main container and copied onto
// the ephemeral container the agent runs in — and nowhere on the proxy pod.
// The ceiling beside it keeps its own volume, items and modes.
func TestCreateSandbox_DeliversASecretFileToTheAgentPodOnly(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.12")
	installAgentRunningReactor(t, cs)

	spec := secretSandboxSpec()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	sec, err := cs.CoreV1().Secrets(testNamespace).Get(context.Background(), secretName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get run secret: %v", err)
	}
	if got := string(sec.Data[managedFileDataKey(1)]); got != "file-secret-value" {
		t.Errorf("secret key %q = %q, want the delivered secret", managedFileDataKey(1), got)
	}
	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	if strings.Contains(podSpecString(pod), "file-secret-value") {
		t.Fatal("the secret's value is in the agent pod spec; it must ride the run Secret only")
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.FSGroup == nil || *pod.Spec.SecurityContext.FSGroup != agentFSGroup {
		t.Fatalf("agent pod SecurityContext = %+v, want fsGroup %d so the agent can read a root-owned projected file", pod.Spec.SecurityContext, agentFSGroup)
	}
	main, _ := findContainer(pod.Spec.Containers, mainContainerName)
	var mount *corev1.VolumeMount
	for i := range main.VolumeMounts {
		if main.VolumeMounts[i].MountPath == runner.ComponentSecretDir {
			mount = &main.VolumeMounts[i]
		}
	}
	if mount == nil || !mount.ReadOnly || mount.SubPath != "" {
		t.Fatalf("main container mount at %s = %+v, want one read-only, subPath-free mount", runner.ComponentSecretDir, mount)
	}
	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == mount.Name {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.Secret == nil || vol.Secret.SecretName != secretName(spec.RunID) {
		t.Fatalf("volume %q = %+v, want a projection of the run Secret", mount.Name, vol)
	}
	if len(vol.Secret.Items) != 1 {
		t.Fatalf("the secret volume projects %d items, want only the one secret: %+v", len(vol.Secret.Items), vol.Secret.Items)
	}
	it := vol.Secret.Items[0]
	if it.Key != managedFileDataKey(1) || it.Path != "api-token" || it.Mode == nil || *it.Mode != 0o440 {
		t.Errorf("secret item = key %q path %q mode %v, want %q api-token 0440", it.Key, it.Path, it.Mode, managedFileDataKey(1))
	}
	// The ceiling beside it is untouched: its own volume, its own modes.
	for _, v := range pod.Spec.Volumes {
		if v.Secret == nil || v.Name == mount.Name {
			continue
		}
		for _, it := range v.Secret.Items {
			if it.Key == managedFileDataKey(1) {
				t.Errorf("volume %q also projects the secret", v.Name)
			}
			if it.Path == "managed-settings.json" && (it.Mode == nil || *it.Mode != 0o644) {
				t.Errorf("the ceiling's mode moved to %v", it.Mode)
			}
		}
	}

	// The proxy is its own pod; nothing of the agent's secret volume reaches it.
	proxy, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), proxyPodName(spec.RunID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get proxy pod: %v", err)
	}
	for _, v := range proxy.Spec.Volumes {
		if v.Secret == nil {
			continue
		}
		for _, it := range v.Secret.Items {
			if strings.HasPrefix(it.Key, "managed.") {
				t.Errorf("the proxy pod projects managed key %q", it.Key)
			}
		}
	}
	for _, c := range append(proxy.Spec.InitContainers, proxy.Spec.Containers...) {
		for _, m := range c.VolumeMounts {
			if m.MountPath == runner.ComponentSecretDir {
				t.Errorf("proxy container %s mounts %s", c.Name, runner.ComponentSecretDir)
			}
		}
	}

	// The agent runs in the ephemeral container Exec adds: it gets the mount,
	// as uid 1000 like every container in this pod.
	if _, err := d.Exec(context.Background(), sb.Ref, []string{"agent-run"}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	pod, err = cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-get agent pod: %v", err)
	}
	if len(pod.Spec.EphemeralContainers) == 0 {
		t.Fatal("Exec added no ephemeral container")
	}
	eph := pod.Spec.EphemeralContainers[0]
	found := false
	for _, m := range eph.VolumeMounts {
		found = found || (m.MountPath == runner.ComponentSecretDir && m.ReadOnly)
	}
	if !found {
		t.Errorf("the ephemeral container has no read-only mount at %s", runner.ComponentSecretDir)
	}
	for _, c := range append(pod.Spec.Containers, corev1.Container{Name: eph.Name, SecurityContext: eph.SecurityContext}) {
		if sc := c.SecurityContext; sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 1000 {
			t.Errorf("container %s does not run as uid 1000 (%+v); a second uid in the pod could read the 0440 fsGroup file", c.Name, sc)
		}
	}
}

// podSpecString is the agent pod's spec as the apiserver would serve it to
// anyone with pods/get.
func podSpecString(pod *corev1.Pod) string { return pod.Spec.String() }

// A pod with no delivered secret keeps the SecurityContext it always had: the
// fsGroup is the secret's, not the ceiling's.
func TestCreateSandbox_NoSecretNoFSGroup(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.12")
	installAgentRunningReactor(t, cs)
	sb, err := d.CreateSandbox(context.Background(), managedSandboxSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	if pod.Spec.SecurityContext != nil {
		t.Errorf("a pod with no delivered secret got a pod SecurityContext %+v", pod.Spec.SecurityContext)
	}
}

// A delivered secret needs the pod's fsGroup and a share must never get one,
// so the pair is refused before the namespace holds anything; a MANAGED drive
// asks for the same fsGroup and is fine.
func TestCreateSandbox_ASecretFileIsRefusedBesideAShareDrive(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	cs.ClearActions()
	spec := secretSandboxSpec()
	spec.Drive = testDriveMount()
	spec.Drive.Backend = types.DriveBackendK8sPVCStatic
	if _, err := d.CreateSandbox(context.Background(), spec); !errors.Is(err, errAgentSecretBesideShare) {
		t.Fatalf("err = %v, want errAgentSecretBesideShare", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			t.Errorf("the refusal created a %s", a.GetResource().Resource)
		}
	}

	if err := agentSecretsAllowDrive(spec.ManagedFiles, testDriveMount()); err != nil {
		t.Errorf("a managed drive beside a secret = %v, want allowed", err)
	}
	if err := agentSecretsAllowDrive(managedSandboxSpec().ManagedFiles, spec.Drive); err != nil {
		t.Errorf("a share drive beside a ceiling alone = %v, want allowed (today's behaviour)", err)
	}
	pod := &corev1.Pod{}
	applyAgentSecretFSGroup(pod, spec.ManagedFiles)
	applyDriveToPod(pod, testDriveMount(), "")
	if agentFSGroup != driveFSGroup {
		t.Fatalf("agentFSGroup %d != driveFSGroup %d: a pod with a secret and a managed drive would ask for two fsGroups", agentFSGroup, driveFSGroup)
	}
	if got := *pod.Spec.SecurityContext.FSGroup; got != agentFSGroup {
		t.Errorf("fsGroup = %d after a secret and a managed drive, want %d", got, agentFSGroup)
	}
}
