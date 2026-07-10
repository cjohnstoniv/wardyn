// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"errors"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/system"
	dockerseccomp "github.com/docker/docker/profiles/seccomp"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func infoWithRuntimes(names ...string) system.Info {
	rt := map[string]system.RuntimeWithStatus{
		"runc": {}, // always present
	}
	for _, n := range names {
		rt[n] = system.RuntimeWithStatus{}
	}
	return system.Info{Runtimes: rt}
}

func TestCapabilitiesFor_ClassMapping(t *testing.T) {
	tests := []struct {
		name     string
		runtimes []string
		want     []types.ConfinementClass
	}{
		{"runc only -> CC1", nil, []types.ConfinementClass{types.CC1}},
		{"runsc -> CC1,CC2", []string{"runsc"}, []types.ConfinementClass{types.CC1, types.CC2}},
		{"kata -> CC1,CC3", []string{"kata-runtime"}, []types.ConfinementClass{types.CC1, types.CC3}},
		{"krun -> CC1,CC3", []string{"krun"}, []types.ConfinementClass{types.CC1, types.CC3}},
		{"both -> CC1,CC2,CC3", []string{"runsc", "kata-qemu"}, []types.ConfinementClass{types.CC1, types.CC2, types.CC3}},
		{"runsc+krun -> CC1,CC2,CC3", []string{"runsc", "krun"}, []types.ConfinementClass{types.CC1, types.CC2, types.CC3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caps := capabilitiesForWith(infoWithRuntimes(tt.runtimes...), nil)
			if caps.Driver != driverName {
				t.Errorf("Driver = %q, want %q", caps.Driver, driverName)
			}
			if !caps.StructuralEgress {
				t.Error("StructuralEgress must be true (L0 is structural here)")
			}
			if caps.NetworkPolicy {
				t.Error("NetworkPolicy must be false (L1 nftables is v0.5 — never overclaim)")
			}
			if !caps.SessionRecording {
				t.Error("SessionRecording must be true")
			}
			if !equalClasses(caps.ConfinementClasses, tt.want) {
				t.Errorf("ConfinementClasses = %v, want %v", caps.ConfinementClasses, tt.want)
			}
			// strongest last invariant
			if len(caps.ConfinementClasses) > 0 {
				if caps.ConfinementClasses[0] != types.CC1 {
					t.Errorf("first class must be CC1, got %v", caps.ConfinementClasses[0])
				}
			}
		})
	}
}

// A CC1 operator override that pins a runtime the host does not have must NOT
// leave CC1 advertised (nor a CC1 substrate in Resolved): a class whose pinned
// runtime cannot be honored is unenforceable, so /healthz must not claim it
// (invariant 5). This mirrors the CC2/CC3 handling — an unhonorable pin drops
// the class entirely rather than silently advertising the daemon default.
func TestCapabilitiesForWith_CC1UnhonorablePinNotAdvertised(t *testing.T) {
	info := infoWithRuntimes("runsc") // runc + runsc present; sysbox is NOT
	overrides := map[types.ConfinementClass]string{types.CC1: "sysbox"}

	caps := capabilitiesForWith(info, overrides)

	for _, c := range caps.ConfinementClasses {
		if c == types.CC1 {
			t.Errorf("CC1 pinned to absent runtime %q must not be advertised; classes=%v", "sysbox", caps.ConfinementClasses)
		}
	}
	if _, ok := caps.Resolved[types.CC1]; ok {
		t.Errorf("Resolved must not carry a CC1 substrate when its pin is unhonorable; got %v", caps.Resolved)
	}
	// A valid (unpinned) class is still advertised — the drop is surgical.
	if !containsClass(caps.ConfinementClasses, types.CC2) {
		t.Errorf("CC2 (runsc present, no pin) must still be advertised; classes=%v", caps.ConfinementClasses)
	}

	// Control: with a HONORABLE CC1 pin (sysbox present) CC1 is advertised again.
	caps2 := capabilitiesForWith(infoWithRuntimes("sysbox"), overrides)
	if !containsClass(caps2.ConfinementClasses, types.CC1) || caps2.Resolved[types.CC1] != "oci/sysbox" {
		t.Errorf("honorable CC1=sysbox pin must advertise CC1 as oci/sysbox; classes=%v resolved=%v", caps2.ConfinementClasses, caps2.Resolved)
	}
}

func containsClass(cs []types.ConfinementClass, want types.ConfinementClass) bool {
	for _, c := range cs {
		if c == want {
			return true
		}
	}
	return false
}

func TestClassToRuntime_FailsClosed(t *testing.T) {
	// CC2 without runsc must fail closed (never downgrade to runc).
	if _, _, err := classToRuntime(types.CC2, infoWithRuntimes()); err == nil {
		t.Fatal("CC2 without runsc must error, got nil")
	} else if !errors.Is(err, errRuntimeUnavailable) {
		t.Errorf("error must wrap errRuntimeUnavailable, got %v", err)
	}

	// CC3 without kata must fail closed.
	if _, _, err := classToRuntime(types.CC3, infoWithRuntimes("runsc")); err == nil {
		t.Fatal("CC3 without kata must error, got nil")
	} else if !errors.Is(err, errRuntimeUnavailable) {
		t.Errorf("error must wrap errRuntimeUnavailable, got %v", err)
	}

	// Unknown class fails closed too.
	if _, _, err := classToRuntime(types.ConfinementClass("CC9"), infoWithRuntimes()); err == nil {
		t.Fatal("unknown class must error, got nil")
	}
}

func TestClassToRuntime_Resolves(t *testing.T) {
	// CC1 / empty => default runtime, no explicit runtime name.
	for _, c := range []types.ConfinementClass{types.CC1, ""} {
		name, needs, err := classToRuntime(c, infoWithRuntimes())
		if err != nil {
			t.Fatalf("class %q: unexpected error %v", c, err)
		}
		if needs || name != "" {
			t.Errorf("class %q: want default runtime (\"\", false), got (%q, %v)", c, name, needs)
		}
	}
	// CC2 with runsc resolves to "runsc".
	name, needs, err := classToRuntime(types.CC2, infoWithRuntimes("runsc"))
	if err != nil || !needs || name != "runsc" {
		t.Errorf("CC2: got (%q, %v, %v), want (runsc, true, nil)", name, needs, err)
	}
	// CC3 prefix match (kata-qemu) resolves.
	name, needs, err = classToRuntime(types.CC3, infoWithRuntimes("kata-qemu"))
	if err != nil || !needs || name != "kata-qemu" {
		t.Errorf("CC3: got (%q, %v, %v), want (kata-qemu, true, nil)", name, needs, err)
	}
	// CC3 is substrate-agnostic: krun (crun+libkrun) also delivers the Vault
	// VM boundary and resolves when kata is absent.
	name, needs, err = classToRuntime(types.CC3, infoWithRuntimes("krun"))
	if err != nil || !needs || name != "krun" {
		t.Errorf("CC3 (krun): got (%q, %v, %v), want (krun, true, nil)", name, needs, err)
	}
}

// A pinned runtime must not silently downgrade a class below its advertised
// isolation family (the isolation floor). CC2 must run gVisor (runsc*), CC3
// must run Kata (kata*); CC1 is the weakest tier and may pin anything stronger.
// A weaker/mismatched pin fails closed with errRuntimeUnavailable, and the
// existing "pinned runtime not installed" check is preserved.
func TestResolveRuntime_PinIsolationFloor(t *testing.T) {
	info := infoWithRuntimes("runsc", "kata-qemu", "sysbox", "krun", "firecracker")
	tests := []struct {
		name     string
		class    types.ConfinementClass
		pin      string
		wantName string // "" when an error is expected
		wantErr  bool
	}{
		{"CC2=runc rejected (shared-kernel downgrade)", types.CC2, "runc", "", true},
		{"CC2=runsc accepted", types.CC2, "runsc", "runsc", false},
		{"CC3=kata-qemu accepted", types.CC3, "kata-qemu", "kata-qemu", false},
		{"CC3=krun accepted (KVM microVM, OCI-runtime form)", types.CC3, "krun", "krun", false},
		{"CC3=runsc rejected (userspace kernel, not a VM)", types.CC3, "runsc", "", true},
		{"CC3=crun rejected (shared-kernel, no libkrun)", types.CC3, "crun", "", true},
		// Pluggability: an operator may pin ANY registered runtime they vouch is a
		// VM (bring-your-own microVM), even one outside the kata/krun allowlist...
		{"CC3=firecracker accepted (operator-vouched BYO microVM)", types.CC3, "firecracker", "firecracker", false},
		// ...but a KNOWN shared-kernel runtime is still refused even under a pin,
		// and an unregistered name still fails closed.
		{"CC3=sysbox rejected (known shared-kernel, vouch not allowed)", types.CC3, "sysbox", "", true},
		{"CC3=myvm rejected (vouched but not installed)", types.CC3, "myvm-absent", "", true},
		{"CC1=sysbox accepted (stronger pin allowed)", types.CC1, "sysbox", "sysbox", false},
		{"CC2=runsc-missing rejected (not installed)", types.CC2, "runsc-missing", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overrides := map[types.ConfinementClass]string{tt.class: tt.pin}
			name, needs, err := resolveRuntime(tt.class, info, overrides)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveRuntime(%s=%s) = (%q, %v, nil), want error", tt.class, tt.pin, name, needs)
				}
				if !errors.Is(err, errRuntimeUnavailable) {
					t.Errorf("error must wrap errRuntimeUnavailable, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRuntime(%s=%s) unexpected error %v", tt.class, tt.pin, err)
			}
			if name != tt.wantName || !needs {
				t.Errorf("resolveRuntime(%s=%s) = (%q, %v), want (%q, true)", tt.class, tt.pin, name, needs, tt.wantName)
			}
		})
	}
}

func TestHardenedHostConfig_Invariants(t *testing.T) {
	hc := hardenedHostConfig("none", "", runner.Resources{CPUMillis: 1500, MemoryMiB: 512}, system.Info{})

	if got := string(hc.NetworkMode); got != "none" {
		t.Errorf("NetworkMode = %q, want none (L0: no default route)", got)
	}
	if len(hc.CapDrop) != 1 || hc.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", hc.CapDrop)
	}
	if !containsStr(hc.SecurityOpt, "no-new-privileges") {
		t.Errorf("SecurityOpt = %v, must contain no-new-privileges", hc.SecurityOpt)
	}
	if _, ok := hc.Tmpfs["/tmp"]; !ok {
		t.Errorf("Tmpfs must mount /tmp, got %v", hc.Tmpfs)
	}
	if hc.AutoRemove {
		t.Error("AutoRemove must be false so Status can observe exit")
	}
	if hc.Runtime != "" {
		t.Errorf("Runtime = %q, want empty (daemon default) for CC1", hc.Runtime)
	}
	// 1500 millis => 1.5 CPU => 1.5e9 nanos.
	if hc.NanoCPUs != 1_500_000_000 {
		t.Errorf("NanoCPUs = %d, want 1500000000", hc.NanoCPUs)
	}
	if hc.Memory != 512*1024*1024 {
		t.Errorf("Memory = %d, want %d", hc.Memory, 512*1024*1024)
	}
	// MemorySwap must be pinned to Memory so the cap is not silently doubled.
	if hc.MemorySwap != hc.Memory {
		t.Errorf("MemorySwap = %d, want == Memory %d (no silent 2x via swap)", hc.MemorySwap, hc.Memory)
	}
	// PidsLimit (fork-bomb guard) must always be set, even when the spec did
	// not request one.
	if hc.PidsLimit == nil {
		t.Fatal("PidsLimit must be non-nil (fork-bomb guard set unconditionally)")
	}
	if *hc.PidsLimit != defaultPidsLimit {
		t.Errorf("PidsLimit = %d, want default %d", *hc.PidsLimit, defaultPidsLimit)
	}
}

func TestHardenedHostConfig_RuntimePassthrough(t *testing.T) {
	hc := hardenedHostConfig("none", "runsc", runner.Resources{}, system.Info{})
	if hc.Runtime != "runsc" {
		t.Errorf("Runtime = %q, want runsc (CC2)", hc.Runtime)
	}
}

// A bring-your-own microVM runtime the operator vouches for (outside the kata/krun
// allowlist) must NOT auto-advertise CC3 with no pin (safe default: only known VMs
// auto-advertise), but MUST advertise CC3 once the operator explicitly pins it —
// the pluggable, non-opinionated seam, still honoring never-overclaim.
func TestCapabilitiesFor_ByoVaultRuntime(t *testing.T) {
	info := infoWithRuntimes("firecracker") // registered, but not on the auto allowlist

	// No pin: firecracker is unrecognized, so CC3 is NOT auto-advertised.
	caps := capabilitiesForWith(info, nil)
	if containsClass(caps.ConfinementClasses, types.CC3) {
		t.Fatalf("CC3 must NOT auto-advertise for an unrecognized runtime; got %v", caps.ConfinementClasses)
	}

	// Explicit operator pin: the operator vouches firecracker is a VM -> CC3 advertised.
	overrides := map[types.ConfinementClass]string{types.CC3: "firecracker"}
	caps2 := capabilitiesForWith(info, overrides)
	if !containsClass(caps2.ConfinementClasses, types.CC3) {
		t.Fatalf("CC3 must advertise for an operator-pinned BYO runtime; got %v", caps2.ConfinementClasses)
	}
	if got := caps2.Resolved[types.CC3]; got != "oci/firecracker" {
		t.Errorf("Resolved[CC3] = %q, want oci/firecracker", got)
	}
}

// Only krun/libkrun gets /dev/kvm mapped into the agent container (its VMM opens
// KVM from inside the container's namespaces). Kata must NOT — its containerd shim
// opens /dev/kvm host-side and boots the VM outside the container, so handing the
// device in would expose host KVM to the Kata guest. CC1 (runc)/CC2 (runsc) never
// get it either.
func TestHardenedHostConfig_KvmDeviceForVault(t *testing.T) {
	hasKvm := func(hc *container.HostConfig) bool {
		for _, d := range hc.Devices {
			if d.PathOnHost == "/dev/kvm" && d.PathInContainer == "/dev/kvm" {
				return true
			}
		}
		return false
	}
	tests := []struct {
		runtime string
		wantKvm bool
	}{
		{"krun", true},
		{"kata-qemu", false}, // Kata boots the VM host-side; no in-container /dev/kvm
		{"runsc", false},
		{"", false}, // CC1 (daemon default runc)
	}
	for _, tt := range tests {
		t.Run("runtime="+tt.runtime, func(t *testing.T) {
			hc := hardenedHostConfig("none", tt.runtime, runner.Resources{}, system.Info{})
			if got := hasKvm(hc); got != tt.wantKvm {
				t.Errorf("runtime %q: /dev/kvm present = %v, want %v (Devices=%v)", tt.runtime, got, tt.wantKvm, hc.Devices)
			}
			// A vault runtime must also join the kvm-owning group (so the non-root
			// agent can open the 0660 device once caps are dropped); a non-vault one
			// must not. GroupAdd content is host-dependent (needs /dev/kvm to resolve
			// the gid), so only assert presence/absence when the gid is resolvable.
			gid := kvmDeviceGID()
			if tt.wantKvm && gid != "" && !containsStr(hc.GroupAdd, gid) {
				t.Errorf("runtime %q: GroupAdd = %v, want it to contain kvm gid %q", tt.runtime, hc.GroupAdd, gid)
			}
			if !tt.wantKvm && len(hc.GroupAdd) != 0 {
				t.Errorf("runtime %q: GroupAdd = %v, want empty for non-vault", tt.runtime, hc.GroupAdd)
			}
		})
	}
}

// A zero-Resources spec MUST still produce a fully-capped HostConfig: the
// conservative platform defaults are applied so no sandbox ever runs uncapped.
func TestHardenedHostConfig_ResourceDefaults(t *testing.T) {
	hc := hardenedHostConfig("none", "", runner.Resources{}, system.Info{})

	if hc.PidsLimit == nil {
		t.Fatal("PidsLimit must be non-nil for a zero-Resources spec (fork-bomb guard)")
	}
	if *hc.PidsLimit != defaultPidsLimit {
		t.Errorf("PidsLimit = %d, want default %d", *hc.PidsLimit, defaultPidsLimit)
	}
	if hc.Memory <= 0 {
		t.Errorf("Memory = %d, want > 0 (default applied)", hc.Memory)
	}
	if hc.Memory != defaultMemoryMiB*1024*1024 {
		t.Errorf("Memory = %d, want default %d", hc.Memory, defaultMemoryMiB*1024*1024)
	}
	if hc.MemorySwap != hc.Memory {
		t.Errorf("MemorySwap = %d, want == Memory %d (no silent 2x via swap)", hc.MemorySwap, hc.Memory)
	}
	if hc.NanoCPUs <= 0 {
		t.Errorf("NanoCPUs = %d, want > 0 (default applied)", hc.NanoCPUs)
	}
	if hc.NanoCPUs != defaultCPUMillis*1_000_000 {
		t.Errorf("NanoCPUs = %d, want default %d", hc.NanoCPUs, defaultCPUMillis*1_000_000)
	}
	// No disk cap requested => no StorageOpt and no fatal behaviour.
	if hc.StorageOpt != nil {
		t.Errorf("StorageOpt = %v, want nil when DiskMiB == 0", hc.StorageOpt)
	}
}

// Explicit spec values must override every default.
func TestHardenedHostConfig_ResourceOverrides(t *testing.T) {
	hc := hardenedHostConfig("none", "", runner.Resources{
		CPUMillis: 3000,
		MemoryMiB: 1024,
		PidsLimit: 64,
	}, system.Info{})

	if hc.NanoCPUs != 3_000_000_000 {
		t.Errorf("NanoCPUs = %d, want 3000000000 (override)", hc.NanoCPUs)
	}
	if hc.Memory != 1024*1024*1024 {
		t.Errorf("Memory = %d, want %d (override)", hc.Memory, 1024*1024*1024)
	}
	if hc.MemorySwap != hc.Memory {
		t.Errorf("MemorySwap = %d, want == Memory %d", hc.MemorySwap, hc.Memory)
	}
	if hc.PidsLimit == nil || *hc.PidsLimit != 64 {
		t.Errorf("PidsLimit = %v, want 64 (override)", hc.PidsLimit)
	}
}

// A disk cap is applied as StorageOpt["size"] only when DiskMiB>0 AND the
// daemon storage driver can enforce a per-container quota; otherwise the run
// proceeds uncapped (no StorageOpt, never a hard failure).
func TestHardenedHostConfig_DiskQuota(t *testing.T) {
	// overlay2 on an xfs backing fs supports project quotas => StorageOpt set.
	supported := system.Info{
		Driver:       "overlay2",
		DriverStatus: [][2]string{{"Backing Filesystem", "xfs"}},
	}
	hc := hardenedHostConfig("none", "", runner.Resources{DiskMiB: 2048}, supported)
	if got := hc.StorageOpt["size"]; got != "2048m" {
		t.Errorf("StorageOpt[size] = %q, want 2048m", got)
	}

	// Unsupported driver (vfs) => no StorageOpt, but the run is NOT broken.
	unsupported := system.Info{Driver: "vfs"}
	hc = hardenedHostConfig("none", "", runner.Resources{DiskMiB: 2048}, unsupported)
	if _, ok := hc.StorageOpt["size"]; ok {
		t.Errorf("StorageOpt must be unset on a driver without quota support, got %v", hc.StorageOpt)
	}

	// DiskMiB == 0 => never a StorageOpt regardless of driver.
	hc = hardenedHostConfig("none", "", runner.Resources{}, supported)
	if hc.StorageOpt != nil {
		t.Errorf("StorageOpt = %v, want nil when DiskMiB == 0", hc.StorageOpt)
	}
}

// storageDriverSupportsQuota's best-effort detection across daemons.
func TestStorageDriverSupportsQuota(t *testing.T) {
	tests := []struct {
		name    string
		driver  string
		backing string
		want    bool
	}{
		{"overlay2 + xfs", "overlay2", "xfs", true},
		{"overlay2 + extfs", "overlay2", "extfs", true},
		{"overlay2 + ext4", "overlay2", "ext4", true},
		{"overlay2 + zfs backing (unsupported)", "overlay2", "zfs", false},
		{"overlay2 + no backing", "overlay2", "", false},
		{"btrfs", "btrfs", "", true},
		{"zfs", "zfs", "", true},
		{"vfs", "vfs", "", false},
		{"fuse-overlayfs", "fuse-overlayfs", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := system.Info{Driver: tt.driver}
			if tt.backing != "" {
				info.DriverStatus = [][2]string{{"Backing Filesystem", tt.backing}}
			}
			if got := storageDriverSupportsQuota(info); got != tt.want {
				t.Errorf("storageDriverSupportsQuota(%q/%q) = %v, want %v", tt.driver, tt.backing, got, tt.want)
			}
		})
	}
}

// proxyResources must always carry a PID cap and a swap-pinned memory cap.
func TestProxyResources(t *testing.T) {
	r := proxyResources()
	if r.PidsLimit == nil || *r.PidsLimit != proxyPidsLimit {
		t.Errorf("proxy PidsLimit = %v, want %d", r.PidsLimit, proxyPidsLimit)
	}
	if r.Memory != proxyMemoryMiB*1024*1024 {
		t.Errorf("proxy Memory = %d, want %d", r.Memory, proxyMemoryMiB*1024*1024)
	}
	if r.MemorySwap != r.Memory {
		t.Errorf("proxy MemorySwap = %d, want == Memory %d", r.MemorySwap, r.Memory)
	}
}

func equalClasses(a, b []types.ConfinementClass) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func infoWithSecurity(secOpts ...string) system.Info {
	return system.Info{
		Runtimes:        map[string]system.RuntimeWithStatus{"runc": {}, "runsc": {}},
		SecurityOptions: secOpts,
	}
}

func TestHostSupportsAppArmor(t *testing.T) {
	tests := []struct {
		name string
		opts []string
		want bool
	}{
		{"apparmor present", []string{"name=apparmor", "name=seccomp,profile=builtin"}, true},
		{"apparmor in csv entry", []string{"name=seccomp,profile=builtin", "name=apparmor"}, true},
		{"no apparmor", []string{"name=seccomp,profile=builtin", "name=rootless"}, false},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostSupportsAppArmor(infoWithSecurity(tt.opts...)); got != tt.want {
				t.Errorf("hostSupportsAppArmor = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHostSupportsSELinux(t *testing.T) {
	tests := []struct {
		name string
		opts []string
		want bool
	}{
		{"selinux present", []string{"name=selinux", "name=seccomp,profile=builtin"}, true},
		{"selinux after seccomp", []string{"name=seccomp,profile=builtin", "name=selinux"}, true},
		{"no selinux (apparmor host)", []string{"name=apparmor", "name=seccomp,profile=builtin"}, false},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostSupportsSELinux(infoWithSecurity(tt.opts...)); got != tt.want {
				t.Errorf("hostSupportsSELinux = %v, want %v", got, tt.want)
			}
		})
	}
}

// gVisor has no SELinux integration, so on an SELinux-enabled daemon runsc
// containers need label=disable to start. This must apply to the runsc path
// ONLY — never runc (CC1), where SELinux labeling is a real defense we keep.
func TestHardenedHostConfig_SELinuxLabelDisable(t *testing.T) {
	selinux := infoWithSecurity("name=selinux", "name=seccomp,profile=builtin")
	noSelinux := infoWithSecurity("name=apparmor", "name=seccomp,profile=builtin")

	tests := []struct {
		name        string
		runtimeName string // "" => CC1/runc
		info        system.Info
		wantDisable bool
	}{
		{"CC1 runc + selinux host -> NOT disabled (keep labeling)", "", selinux, false},
		{"CC2 runsc + selinux host -> disabled", "runsc", selinux, true},
		{"CC2 runsc-prefixed + selinux host -> disabled", "runsc-custom", selinux, true},
		{"CC2 runsc + no-selinux host -> not set", "runsc", noSelinux, false},
		{"CC3 kata + selinux host -> not set", "kata-qemu", selinux, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc := hardenedHostConfig("none", tt.runtimeName, runner.Resources{}, tt.info)
			if !containsStr(hc.SecurityOpt, "no-new-privileges") {
				t.Error("no-new-privileges must always be set")
			}
			gotDisable := containsStr(hc.SecurityOpt, "label=disable")
			if gotDisable != tt.wantDisable {
				t.Errorf("label=disable present = %v, want %v (SecurityOpt=%v)", gotDisable, tt.wantDisable, hc.SecurityOpt)
			}
		})
	}
}

func TestHardenedHostConfig_SeccompApparmor(t *testing.T) {
	apparmorInfo := infoWithSecurity("name=apparmor", "name=seccomp,profile=builtin")
	noApparmorInfo := infoWithSecurity("name=seccomp,profile=builtin")

	tests := []struct {
		name         string
		runtimeName  string // "" => CC1/runc
		info         system.Info
		wantApparmor bool
	}{
		{"CC1 + apparmor host -> pinned", "", apparmorInfo, true},
		{"CC1 + no-apparmor host -> omitted", "", noApparmorInfo, false},
		{"CC2 runsc + apparmor host -> omitted (runtime mediates)", "runsc", apparmorInfo, false},
		{"CC3 kata + apparmor host -> omitted", "kata-qemu", apparmorInfo, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc := hardenedHostConfig("none", tt.runtimeName, runner.Resources{}, tt.info)
			// Baseline invariants on every class.
			if !containsStr(hc.SecurityOpt, "no-new-privileges") {
				t.Error("no-new-privileges must always be set")
			}
			if !containsStr(hc.CapDrop, "ALL") {
				t.Error("CapDrop ALL must always be set")
			}
			// We must NEVER weaken seccomp to unconfined (RuntimeDefault stays).
			if containsStr(hc.SecurityOpt, "seccomp=unconfined") {
				t.Error("seccomp=unconfined must never be set")
			}
			gotApparmor := containsStr(hc.SecurityOpt, "apparmor=docker-default")
			if gotApparmor != tt.wantApparmor {
				t.Errorf("apparmor=docker-default present = %v, want %v (SecurityOpt=%v)", gotApparmor, tt.wantApparmor, hc.SecurityOpt)
			}
		})
	}
}

// TestDockerDefaultSeccompProfile_BlocksIoUring locks in the CC1 (runc)
// io_uring dependency: hardenedHostConfig NEVER ships a custom seccomp profile
// (asserted above — no "seccomp=" SecurityOpt entry is ever set, so
// RuntimeDefault always applies), so CC1's actual io_uring exposure is
// whatever the shipped Docker version's DEFAULT profile allows. io_uring
// syscalls let a sandboxed process perform file/network I/O through a path
// that bypasses much of the traditional syscall-level seccomp mediation
// (CVE-class widely discussed for io_uring), so io_uring_setup/_enter/
// _register must fall through to the profile's defaultAction (ERRNO/deny),
// not an explicit or accidental ALLOW.
//
// This calls docker/docker's OWN profiles/seccomp.DefaultProfile() (the exact
// code the daemon serves as "runtime/default") rather than re-deriving the
// syscall list, so a future `go get -u github.com/docker/docker` that changes
// the default profile re-runs this check for free.
func TestDockerDefaultSeccompProfile_BlocksIoUring(t *testing.T) {
	profile := dockerseccomp.DefaultProfile()
	if profile.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Fatalf("Docker default seccomp profile defaultAction = %v, want ERRNO (default-deny) — the io_uring omission below only blocks it under default-deny", profile.DefaultAction)
	}
	blocked := map[string]bool{"io_uring_setup": true, "io_uring_enter": true, "io_uring_register": true}
	for _, sc := range profile.Syscalls {
		if sc.Action != "SCMP_ACT_ALLOW" {
			continue
		}
		for _, name := range sc.Names {
			if blocked[name] {
				t.Errorf("Docker default seccomp profile explicitly ALLOWs %q; CC1 (runc, no custom profile) would no longer block io_uring", name)
			}
		}
	}
}

// TestStripDangerousKataAnnotations locks in the denylist behavior: the two
// CVE-2026-44210/-47243 hypervisor-override keys are always removed, an
// unrelated annotation survives, and a nil map is returned unchanged (the
// nil-safety hardenedHostConfig's unconditional call relies on).
func TestStripDangerousKataAnnotations(t *testing.T) {
	ann := map[string]string{
		"io.katacontainers.config.hypervisor.virtio_fs_extra_args": "cache=none,no_open,no_readdirplus",
		"io.katacontainers.config.hypervisor.kernel_params":        "init=/bin/sh",
		"safe.annotation": "keep-me",
	}
	got := stripDangerousKataAnnotations(ann)
	for _, k := range dangerousKataAnnotations {
		if _, ok := got[k]; ok {
			t.Errorf("dangerous annotation %q must be stripped, got %v", k, got)
		}
	}
	if got["safe.annotation"] != "keep-me" {
		t.Errorf("an unrelated annotation must survive the strip, got %v", got)
	}
	if out := stripDangerousKataAnnotations(nil); out != nil {
		t.Errorf("stripDangerousKataAnnotations(nil) = %v, want nil", out)
	}
}

// TestHardenedHostConfig_NeverCarriesKataAnnotations proves the audited claim
// in dangerousKataAnnotations' doc: NOTHING in Wardyn populates
// HostConfig.Annotations today, on ANY runtime/class path (including CC3/Kata
// itself), so a run can never carry a virtio_fs_extra_args/kernel_params
// override through Wardyn regardless of what a policy, grant, or composer
// proposal contains. If a future change threads an Annotations source
// through, this test (and the strip call in hardenedHostConfig) are what stop
// it from silently reaching a launched Kata guest.
func TestHardenedHostConfig_NeverCarriesKataAnnotations(t *testing.T) {
	for _, rt := range []string{"", "runsc", "kata-qemu", "kata-clh", "krun"} {
		hc := hardenedHostConfig("test-net", rt, runner.Resources{}, system.Info{})
		if len(hc.Annotations) != 0 {
			t.Errorf("runtime %q: HostConfig.Annotations = %v, want empty (Wardyn has no annotation pass-through)", rt, hc.Annotations)
		}
	}
}
