// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"errors"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// extraEnvAllowed is the nine variables an extra gets, spelled here rather than read from
// extraContainerEnvKeys, so a key added there reds this test instead of agreeing with itself.
var extraEnvAllowed = map[string]string{
	"WARDYN_RUN_ID":      "run-1",
	"WARDYN_PROXY_URL":   "http://wardyn-proxy:3128",
	"HTTP_PROXY":         "http://wardyn-proxy:3128",
	"HTTPS_PROXY":        "http://wardyn-proxy:3128",
	"http_proxy":         "http://wardyn-proxy:3128",
	"https_proxy":        "http://wardyn-proxy:3128",
	"NO_PROXY":           "localhost",
	"no_proxy":           "localhost",
	"WARDYN_MITM_CA_PEM": "-----BEGIN CERTIFICATE-----\nAA==\n-----END CERTIFICATE-----\n",
}

func twoExtras() []runner.ExtraContainer {
	return []runner.ExtraContainer{
		{Name: "kernel", Image: "registry.example/kernel@sha256:" + strings.Repeat("a", 64), Command: []string{"sleep", "3600"}, Ports: []int{8888}, CPUMillis: 200, MemoryMiB: 256},
		{Name: "mcp", Image: "registry.example/mcp@sha256:" + strings.Repeat("b", 64), CPUMillis: 100, MemoryMiB: 128},
	}
}

// installPodContainersReactor reports every container of the agent pod Running, except those in
// stuck, held Waiting with the given reason. It overlays the stored pod, as installAgentRunningReactor does.
func installPodContainersReactor(t *testing.T, cs *fake.Clientset, stuck map[string]string) {
	t.Helper()
	cs.PrependReactor("get", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(clienttesting.GetAction)
		if !ok || !strings.HasPrefix(ga.GetName(), agentPodNamePrefix) {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(podsGVR, action.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		pod.Status.ContainerStatuses = statusesAllRunning(pod, stuck)
		return true, pod, nil
	})
}

func statusesAllRunning(pod *corev1.Pod, stuck map[string]string) []corev1.ContainerStatus {
	var out []corev1.ContainerStatus
	for _, c := range pod.Spec.Containers {
		st := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
		if reason, ok := stuck[c.Name]; ok {
			st = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "pull failed"}}
		}
		out = append(out, corev1.ContainerStatus{Name: c.Name, State: st})
	}
	return out
}

func createExtrasPod(t *testing.T, d *Driver, cs *fake.Clientset, spec runner.SandboxSpec) *corev1.Pod {
	t.Helper()
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	pod, err := cs.CoreV1().Pods(testNamespace).Get(context.Background(), sb.Ref, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get agent pod: %v", err)
	}
	return pod
}

func containerNames(cs []corev1.Container) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func assertLimits(t *testing.T, c *corev1.Container, cpuMillis, memMiB int64) {
	t.Helper()
	cpu, mem := c.Resources.Limits[corev1.ResourceCPU], c.Resources.Limits[corev1.ResourceMemory]
	if cpu.MilliValue() != cpuMillis || mem.Value() != memMiB*1024*1024 {
		t.Errorf("%s limits = %s cpu / %s memory, want %dm / %dMi", c.Name, cpu.String(), mem.String(), cpuMillis, memMiB)
	}
}

// assertOnlySharedMounts fails when an extra mounts anything but the work folder and the run's drives.
func assertOnlySharedMounts(t *testing.T, pod *corev1.Pod, drives int) {
	t.Helper()
	allowed := map[string]bool{"wardyn-work": true}
	for i := range drives {
		allowed[driveVolumeNameAt(i)] = true
	}
	for _, c := range pod.Spec.Containers[1:] {
		for _, m := range c.VolumeMounts {
			if !allowed[m.Name] {
				t.Errorf("extra %s mounts %q; only the work folder and the drives are shared", c.Name, m.Name)
			}
		}
	}
}

// The agent pod carries each extra after the agent, built from the agent's security context, the proxy's
// variables only, the work folder and the drives, with the agent keeping the run's size less the extras'.
// Classes declares the path, and the two are pinned together here.
func TestCreateSandbox_ExtraContainersShape(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.21")
	installPodContainersReactor(t, cs, nil)

	support, err := d.Classes(context.Background())
	if err != nil {
		t.Fatalf("Classes: %v", err)
	}
	if !support.ExtraContainers {
		t.Fatal("Classes reports ExtraContainers=false while this driver builds extra containers")
	}

	spec := testSandboxSpec()
	spec.Resources = runner.Resources{CPUMillis: 1000, MemoryMiB: 1024}
	drive := testDriveMount()
	drive.ReadOnly = true
	spec.Drives = []types.DriveMount{*drive}
	spec.Env = map[string]string{"GIT_AUTHOR_NAME": "dev", "SSL_CERT_FILE": "/etc/ssl/certs/wardyn.pem"}
	for k, v := range extraEnvAllowed {
		spec.Env[k] = v
	}
	spec.SecretEnv = map[string]string{"TOKEN": "x"}
	spec.Containers = twoExtras()
	pod := createExtrasPod(t, d, cs, spec)

	if got := containerNames(pod.Spec.Containers); !slices.Equal(got, []string{mainContainerName, "kernel", "mcp"}) {
		t.Fatalf("containers = %v, want [agent kernel mcp]", got)
	}
	assertLimits(t, &pod.Spec.Containers[0], 700, 640)
	assertLimits(t, &pod.Spec.Containers[1], 200, 256)
	assertLimits(t, &pod.Spec.Containers[2], 100, 128)

	work := volumeFor(pod.Spec.Volumes, "wardyn-work")
	if work == nil || work.EmptyDir == nil || work.EmptyDir.SizeLimit != nil {
		t.Errorf("wardyn-work volume = %+v, want an emptyDir with no size limit", work)
	}
	if m := mountFor(pod.Spec.Containers[0].VolumeMounts, "wardyn-work"); m == nil || m.MountPath != "/home/agent/work" {
		t.Errorf("agent mount for wardyn-work = %+v, want /home/agent/work", m)
	}

	wantMounts := []corev1.VolumeMount{
		{Name: driveVolumeNameAt(0), MountPath: runner.DriveTarget, ReadOnly: true},
		{Name: "wardyn-work", MountPath: "/home/agent/work"},
	}
	for _, c := range pod.Spec.Containers[1:] {
		if !reflect.DeepEqual(c.SecurityContext, agentSecurityContext()) {
			t.Errorf("%s SecurityContext = %+v, want agentSecurityContext()", c.Name, c.SecurityContext)
		}
		got := map[string]string{}
		for _, e := range c.Env {
			if e.ValueFrom != nil {
				t.Errorf("%s env %s has a ValueFrom", c.Name, e.Name)
			}
			got[e.Name] = e.Value
		}
		if !reflect.DeepEqual(got, extraEnvAllowed) {
			t.Errorf("%s env = %v, want exactly the nine proxy variables", c.Name, got)
		}
		if len(c.EnvFrom) != 0 || len(c.Ports) != 0 {
			t.Errorf("%s EnvFrom = %v, Ports = %v; want neither", c.Name, c.EnvFrom, c.Ports)
		}
		mounts := slices.Clone(c.VolumeMounts)
		slices.SortFunc(mounts, func(a, b corev1.VolumeMount) int { return strings.Compare(a.Name, b.Name) })
		if !reflect.DeepEqual(mounts, wantMounts) {
			t.Errorf("%s mounts = %+v, want %+v", c.Name, mounts, wantMounts)
		}
	}
	if k := pod.Spec.Containers[1]; !slices.Equal(k.Command, []string{"sleep", "3600"}) || k.Args != nil {
		t.Errorf("kernel Command = %v, Args = %v; want [sleep 3600] and nil", k.Command, k.Args)
	}
	if m := pod.Spec.Containers[2]; m.Command != nil {
		t.Errorf("mcp Command = %v, want nil (the image's entrypoint)", m.Command)
	}
}

// A managed file never reaches an extra: neither the root-owned ceiling nor the staged agent-owned
// secret. The stage init container keeps the run's whole size.
func TestCreateSandbox_ExtraContainersGetNoSecretMounts(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.22")
	installPodContainersReactor(t, cs, nil)

	spec := secretSandboxSpec()
	spec.Drives = []types.DriveMount{*testDriveMount()}
	spec.Containers = []runner.ExtraContainer{{Name: "mcp", Image: "registry.example/mcp@sha256:" + strings.Repeat("b", 64), CPUMillis: 100, MemoryMiB: 128}}
	pod := createExtrasPod(t, d, cs, spec)

	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("containers = %v, want agent and mcp", containerNames(pod.Spec.Containers))
	}
	assertOnlySharedMounts(t, pod, len(spec.Drives))
	if len(pod.Spec.Containers[1].VolumeMounts) != 2 {
		t.Errorf("mcp mounts = %+v, want the work folder and the drive", pod.Spec.Containers[1].VolumeMounts)
	}
	i := slices.IndexFunc(pod.Spec.InitContainers, func(c corev1.Container) bool { return c.Name == stageAgentSecretsName })
	if i < 0 {
		t.Fatalf("init containers = %v, want %s", containerNames(pod.Spec.InitContainers), stageAgentSecretsName)
	}
	assertLimits(t, &pod.Spec.InitContainers[i], spec.Resources.CPUMillis, spec.Resources.MemoryMiB)
	assertLimits(t, &pod.Spec.Containers[0], spec.Resources.CPUMillis-100, spec.Resources.MemoryMiB-128)
}

// With disk_mib the run already has its work volume: no second one, and each extra carries the same
// ephemeral-storage limit and request floor as the agent container.
func TestCreateSandbox_ExtraContainersWithDiskBudget(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.23")
	installPodContainersReactor(t, cs, nil)

	spec := testSandboxSpec()
	spec.Resources = runner.Resources{CPUMillis: 1000, MemoryMiB: 1024, DiskMiB: 2048}
	spec.Containers = twoExtras()
	pod := createExtrasPod(t, d, cs, spec)

	n := 0
	for _, v := range pod.Spec.Volumes {
		if v.Name == "wardyn-work" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("wardyn-work volumes = %d, want the one disk_mib scratch volume", n)
	}
	wantLimit, wantRequest := resource.MustParse("2048Mi"), resource.MustParse("256Mi")
	for _, c := range pod.Spec.Containers[1:] {
		lim, req := c.Resources.Limits[corev1.ResourceEphemeralStorage], c.Resources.Requests[corev1.ResourceEphemeralStorage]
		if lim.Cmp(wantLimit) != 0 || req.Cmp(wantRequest) != 0 {
			t.Errorf("%s ephemeral-storage = limit %s / request %s, want 2048Mi / 256Mi", c.Name, lim.String(), req.String())
		}
	}
	assertOnlySharedMounts(t, pod, 0)
}

func TestAgentResources(t *testing.T) {
	extra := func(cpu, mem int64) runner.ExtraContainer {
		return runner.ExtraContainer{Name: "x", Image: "img", CPUMillis: cpu, MemoryMiB: mem}
	}
	run := runner.Resources{CPUMillis: 1000, MemoryMiB: 1024, DiskMiB: 64}

	got, err := agentResources(runner.SandboxSpec{Resources: run})
	if err != nil || got != run {
		t.Fatalf("no extras: %+v, %v; want spec.Resources unchanged", got, err)
	}
	got, err = agentResources(runner.SandboxSpec{Resources: run, Containers: []runner.ExtraContainer{extra(750, 768)}})
	if err != nil || got.CPUMillis != 250 || got.MemoryMiB != 256 || got.DiskMiB != 64 {
		t.Fatalf("750/768: %+v, %v; want 250 m / 256 MiB left", got, err)
	}
	for name, extras := range map[string][]runner.ExtraContainer{
		"cpu under the floor":    {extra(751, 768)},
		"memory under the floor": {extra(750, 769)},
		"no cpu":                 {extra(0, 128)},
		"no memory":              {extra(100, 0)},
		"four 2^62 MiB extras":   {extra(100, 1<<62), extra(100, 1<<62), extra(100, 1<<62), extra(100, 1<<62)},
		"two MaxInt64 extras":    {extra(math.MaxInt64, math.MaxInt64), extra(math.MaxInt64, math.MaxInt64)},
	} {
		if _, err := agentResources(runner.SandboxSpec{Resources: run, Containers: extras}); !errors.Is(err, errExtraContainersBudget) {
			t.Errorf("%s: err = %v, want errExtraContainersBudget", name, err)
		}
	}

	t.Cleanup(func() { _ = runner.SetRequestRatio(0) })
	if err := runner.SetRequestRatio(0.5); err != nil {
		t.Fatalf("SetRequestRatio: %v", err)
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: mainContainerName}}}}
	addExtraContainers(pod, runner.SandboxSpec{Resources: run, Containers: []runner.ExtraContainer{extra(200, 256)}}, nil)
	req := pod.Spec.Containers[1].Resources.Requests
	cpu, mem := req[corev1.ResourceCPU], req[corev1.ResourceMemory]
	if cpu.MilliValue() != 100 || mem.Value() != 128*1024*1024 {
		t.Errorf("extra requests at ratio 0.5 = %s cpu / %s memory, want 100m / 128Mi", cpu.String(), mem.String())
	}
}

// A Linux VM has no pod to put an extra in: the run is refused with ErrExtraContainersUnsupported before
// anything exists, never started without them.
func TestCreatePreflight_RefusesExtraContainersOnALinuxVM(t *testing.T) {
	d, cs := vmDriver(t, Config{})
	before := len(cs.Actions())
	spec := testSandboxSpec()
	spec.MachineKind = types.MachineLinuxVM
	spec.Containers = twoExtras()
	if _, err := d.CreateSandbox(context.Background(), spec); !errors.Is(err, runner.ErrExtraContainersUnsupported) {
		t.Fatalf("err = %v, want ErrExtraContainersUnsupported", err)
	}
	assertNoCreates(t, cs, before)
}

func TestCreatePreflight_RefusesAnOverBudgetSplit(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	before := len(cs.Actions())
	spec := testSandboxSpec()
	spec.Drives = []types.DriveMount{*testDriveMount()} // its claim is the first write: it must not happen either
	spec.Resources = runner.Resources{CPUMillis: 1000, MemoryMiB: 1024}
	spec.Containers = []runner.ExtraContainer{{Name: "big", Image: "img", CPUMillis: 900, MemoryMiB: 128}}
	if _, err := d.CreateSandbox(context.Background(), spec); !errors.Is(err, errExtraContainersBudget) {
		t.Fatalf("err = %v, want errExtraContainersBudget", err)
	}
	assertNoCreates(t, cs, before)
}

// An extra that never starts fails the create naming it, and the run's objects are rolled back.
func TestCreateSandbox_FailsNamingAnExtraThatNeverStarts(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.24")
	installPodContainersReactor(t, cs, map[string]string{"mcp": "ErrImagePull"})

	spec := testSandboxSpec()
	spec.Resources = runner.Resources{CPUMillis: 1000, MemoryMiB: 1024}
	spec.Containers = twoExtras()
	_, err := d.CreateSandbox(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), `"mcp"`) || !strings.Contains(err.Error(), "ErrImagePull") {
		t.Fatalf("err = %v, want a start failure naming mcp", err)
	}
	assertRunObjectsGone(t, cs, spec.RunID)
}

// On the agent-sandbox backend the Sandbox's pod template carries the extras, and the admitted pod, with
// the API server's defaults on every container, passes the read-back: the gate comes off once.
func TestAgentSandbox_ExtraContainersPassReadBack(t *testing.T) {
	d, cs, dyn := newTemplateRig(t, allowedTemplate(), controllerMaking(t, nil))
	cs.PrependReactor("get", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		ga, ok := a.(clienttesting.GetAction)
		if !ok || !strings.HasPrefix(ga.GetName(), agentPodNamePrefix) {
			return false, nil, nil
		}
		obj, err := cs.Tracker().Get(podsGVR, ga.GetNamespace(), ga.GetName())
		if err != nil {
			return true, nil, err
		}
		pod := obj.(*corev1.Pod).DeepCopy()
		if len(pod.Spec.SchedulingGates) == 0 && pod.Status.Phase == corev1.PodPending {
			pod.Spec.NodeName, pod.Status.Phase = "node-1", corev1.PodRunning
			pod.Status.ContainerStatuses = statusesAllRunning(pod, nil)
		}
		return true, pod, nil
	})

	spec := asSpec()
	spec.Containers = []runner.ExtraContainer{
		{Name: "kernel", Image: "registry.example/kernel@sha256:" + strings.Repeat("a", 64), Command: []string{"sleep", "3600"}, Ports: []int{8888}, CPUMillis: 100, MemoryMiB: 64},
		{Name: "mcp", Image: "registry.example/mcp@sha256:" + strings.Repeat("b", 64), CPUMillis: 100, MemoryMiB: 64},
	}
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	obj, err := dyn.Tracker().Get(sandboxesGVR, testNamespace, sb.Ref)
	if err != nil {
		t.Fatalf("get Sandbox: %v", err)
	}
	pod := podTemplateOf(t, obj.(*unstructured.Unstructured))
	if got := containerNames(pod.Spec.Containers); !slices.Equal(got, []string{mainContainerName, "kernel", "mcp"}) {
		t.Errorf("Sandbox pod template containers = %v, want [agent kernel mcp]", got)
	}
	if p := podPatches(cs); len(p) != 1 {
		t.Errorf("pod patches = %d, want the one gate removal", len(p))
	}
}

// A kept work folder is the drive at the work path: no wardyn-work volume beside it, and the extra
// shares the drive there.
func TestCreateSandbox_ExtraContainersShareAKeptWorkFolder(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	installProxyIPReactor(t, cs, "10.244.0.25")
	installPodContainersReactor(t, cs, nil)

	spec := testSandboxSpec()
	spec.Resources = runner.Resources{CPUMillis: 1000, MemoryMiB: 1024}
	kept := testDriveMount()
	kept.Target = "/home/agent/work"
	spec.Drives = []types.DriveMount{*kept}
	spec.Containers = []runner.ExtraContainer{{Name: "mcp", Image: "registry.example/mcp@sha256:" + strings.Repeat("b", 64), CPUMillis: 100, MemoryMiB: 128}}
	pod := createExtrasPod(t, d, cs, spec)

	if volumeFor(pod.Spec.Volumes, "wardyn-work") != nil {
		t.Errorf("pod carries a wardyn-work volume beside the kept work folder; volumes=%+v", pod.Spec.Volumes)
	}
	for _, c := range pod.Spec.Containers {
		var atWork []string
		for _, m := range c.VolumeMounts {
			if m.MountPath == "/home/agent/work" {
				atWork = append(atWork, m.Name)
			}
		}
		if !slices.Equal(atWork, []string{driveVolumeNameAt(0)}) {
			t.Errorf("%s mounts at /home/agent/work = %v, want only the drive %q", c.Name, atWork, driveVolumeNameAt(0))
		}
	}
}

func TestExtraContainerNames(t *testing.T) {
	named := func(names ...string) []runner.ExtraContainer {
		out := make([]runner.ExtraContainer, 0, len(names))
		for _, n := range names {
			out = append(out, runner.ExtraContainer{Name: n})
		}
		return out
	}
	for _, names := range [][]string{
		{"agent"}, {"wardyn-agent"}, {"wardyn-proxy"}, {"wardyn-x"}, {"stage-secrets"}, {"stage-proxy-config"},
		{"mcp", "mcp"}, {"kernel", "mcp", "kernel"},
	} {
		if err := extraContainerNames(named(names...)); !errors.Is(err, errExtraContainerName) {
			t.Errorf("%v: err = %v, want errExtraContainerName", names, err)
		}
	}
	for _, names := range [][]string{nil, {"kernel", "mcp"}, {"agents"}, {"my-wardyn-"}, {"staged"}} {
		if err := extraContainerNames(named(names...)); err != nil {
			t.Errorf("%v: err = %v, want nil", names, err)
		}
	}
}

func TestCreatePreflight_RefusesAReservedExtraName(t *testing.T) {
	d, cs := newTestDriver(t, Config{})
	before := len(cs.Actions())
	spec := testSandboxSpec()
	spec.Drives = []types.DriveMount{*testDriveMount()}
	spec.Resources = runner.Resources{CPUMillis: 1000, MemoryMiB: 1024}
	spec.Containers = []runner.ExtraContainer{{Name: "wardyn-agent", Image: "img", CPUMillis: 100, MemoryMiB: 128}}
	if _, err := d.CreateSandbox(context.Background(), spec); !errors.Is(err, errExtraContainerName) {
		t.Fatalf("err = %v, want errExtraContainerName", err)
	}
	assertNoCreates(t, cs, before)
}

// A start timeout on an extra still waiting names the extra and its reason, not only the pod's phase.
func TestCreateSandbox_TimeoutNamesAWaitingExtra(t *testing.T) {
	d, cs := newTestDriver(t, Config{StartTimeout: 300 * time.Millisecond})
	installProxyIPReactor(t, cs, "10.244.0.26")
	installPodContainersReactor(t, cs, map[string]string{"mcp": "ContainerCreating"})

	spec := testSandboxSpec()
	spec.Resources = runner.Resources{CPUMillis: 1000, MemoryMiB: 1024}
	spec.Containers = twoExtras()
	_, err := d.CreateSandbox(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "mcp: ContainerCreating") {
		t.Fatalf("err = %v, want the timeout to name mcp's waiting reason", err)
	}
	assertRunObjectsGone(t, cs, spec.RunID)
}

func agentPodWith(phase corev1.PodPhase, extras int, agent corev1.ContainerState) *corev1.Pod {
	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: phase}}
	pod.Spec.Containers = []corev1.Container{{Name: mainContainerName}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: mainContainerName, State: agent}}
	for i := range extras {
		name := "extra-" + strconv.Itoa(i)
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: name})
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses,
			corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
	}
	return pod
}

func exited(code int32) corev1.ContainerState {
	return corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}
}

// With an extra still running the pod stays Running after the agent exits; the run reads ended with
// the agent's own exit code.
func TestStatusFromPod_AgentExitedBesideARunningExtra(t *testing.T) {
	for _, tc := range []struct {
		code int32
		want types.RunState
	}{{2, types.RunFailed}, {143, types.RunFailed}, {0, types.RunStopped}} {
		st := statusFromPod(agentPodWith(corev1.PodRunning, 1, exited(tc.code)))
		if st.State != tc.want || st.ExitCode == nil || *st.ExitCode != int(tc.code) || st.Message == "" {
			t.Errorf("agent exited %d beside a running extra: %+v, want %s with exit code %d", tc.code, st, tc.want, tc.code)
		}
	}
	if st := statusFromPod(agentPodWith(corev1.PodRunning, 2, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})); st.State != types.RunRunning || st.ExitCode != nil {
		t.Errorf("agent running beside extras: %+v, want RUNNING with no exit code", st)
	}
}

// A run without extras reads from the phase alone, exactly as before extras existed: a terminated agent
// status under a Running phase does not end it.
func TestStatusFromPod_OneContainerPodReadsThePhase(t *testing.T) {
	zero, two := 0, 2
	for _, tc := range []struct {
		name  string
		pod   *corev1.Pod
		state types.RunState
		code  *int
	}{
		{"pending", agentPodWith(corev1.PodPending, 0, corev1.ContainerState{}), types.RunStarting, nil},
		{"running", agentPodWith(corev1.PodRunning, 0, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}), types.RunRunning, nil},
		{"running, agent status terminated", agentPodWith(corev1.PodRunning, 0, exited(2)), types.RunRunning, nil},
		{"succeeded", agentPodWith(corev1.PodSucceeded, 0, exited(0)), types.RunStopped, &zero},
		{"failed", agentPodWith(corev1.PodFailed, 0, exited(2)), types.RunFailed, &two},
	} {
		st := statusFromPod(tc.pod)
		if st.State != tc.state || !reflect.DeepEqual(st.ExitCode, tc.code) {
			t.Errorf("%s: %+v, want %s with exit code %v", tc.name, st, tc.state, tc.code)
		}
		if tc.state == types.RunRunning && st.Message != "" {
			t.Errorf("%s: message %q, want none", tc.name, st.Message)
		}
	}
}
