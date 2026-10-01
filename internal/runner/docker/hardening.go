// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"

	"github.com/cjohnstoniv/wardyn/internal/dockerutil"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Proxy sidecar caps: the proxy only relays HTTP, so a tight envelope bounds a compromised proxy —
// its own PID (fork-bomb guard) and memory cap, independent of the agent's larger spec-driven caps.
const (
	proxyPidsLimit int64 = 128
	proxyMemoryMiB int64 = 256
)

// Docker runtime names probed from `docker info`. SECURITY (invariant 5): a class is claimed only
// when its enforcing runtime is actually installed.
const (
	runtimeRunsc  = "runsc"  // gVisor       -> CC2 (userspace-kernel sandbox)
	runtimeKata   = "kata"   // Kata         -> CC3 (KVM microVM via its own containerd shim-v2)
	runtimeKrun   = "krun"   // crun+libkrun -> CC3 (KVM microVM as a crun-based OCI runtime binary)
	runtimeSysbox = "sysbox" // sysbox-runc: stronger CC1, still shared-kernel
)

// dangerousKataAnnotations are the two Kata hypervisor-config annotations letting a workload
// override its own microVM's virtio-fs/kernel config — the knobs behind CVE-2026-44210/-47243 (a
// compromised Kata guest reaching host-root via virtiofsd). Nothing sets hc.Annotations today;
// this is defense-in-depth at hardenedHostConfig, the one chokepoint every HostConfig passes through.
var dangerousKataAnnotations = []string{
	"io.katacontainers.config.hypervisor.virtio_fs_extra_args",
	"io.katacontainers.config.hypervisor.kernel_params",
}

// stripDangerousKataAnnotations deletes the dangerousKataAnnotations keys from ann and returns it.
// Nil-safe: delete on a nil map is a no-op.
func stripDangerousKataAnnotations(ann map[string]string) map[string]string {
	for _, k := range dangerousKataAnnotations {
		delete(ann, k)
	}
	return ann
}

// cc3Runtimes deliver the Vault tier's hardware (KVM) VM isolation: kata* and krun (libkrun),
// probed in order, first installed wins. Bare "crun" (no libkrun) is shared-kernel and deliberately
// NOT accepted.
var cc3Runtimes = []string{runtimeKata, runtimeKrun}

// runtimeSupportsExec reports whether the OCI runtime can enter a running container via `docker
// exec`. Every runtime we use can EXCEPT krun/libkrun: a libkrun microVM has no in-guest exec
// agent, so its workload must run as the container's main process instead.
func runtimeSupportsExec(runtimeName string) bool {
	return !strings.HasPrefix(runtimeName, runtimeKrun)
}

const (
	// ociFeaturesStatusKey: the `docker info` runtime-status key carrying that runtime's OCI features JSON.
	ociFeaturesStatusKey = "org.opencontainers.runtime-spec.features"
	// ociMountOptionRRO is the OCI mount option for a RECURSIVELY read-only bind (BindOptions.ReadOnlyForceRecursive).
	ociMountOptionRRO = "rro"
)

// runtimeSupportsRecursiveReadOnly reports whether runtimeName, as `docker info` describes it,
// declares the OCI `rro` mount option. This is a PRE-FLIGHT for a create the daemon would otherwise
// refuse, not an optimisation: moby's supportsRecursivelyReadOnly errors for a runtime lacking
// `rro`. gVisor is exactly that runtime (lists `ro`/`rbind`, no `rro`), so this avoids refusing
// every read-only share drive under CC2, the default confinement floor. Unknown reads as
// unsupported, matching the daemon: a request it can't honour is a run that doesn't start.
func runtimeSupportsRecursiveReadOnly(info system.Info, runtimeName string) bool {
	name := runtimeName
	if name == "" {
		name = info.DefaultRuntime // "" is the daemon's DEFAULT runtime (CC1)
	}
	rt, ok := info.Runtimes[name]
	if !ok {
		return false
	}
	var feats struct { // only the one field: a yes/no question, not the whole OCI features struct
		MountOptions []string `json:"mountOptions"`
	}
	if err := json.Unmarshal([]byte(rt.Status[ociFeaturesStatusKey]), &feats); err != nil {
		return false
	}
	return slices.Contains(feats.MountOptions, ociMountOptionRRO)
}

// classToRuntime returns the Docker runtime name required to enforce class, and whether a
// non-default runtime is needed at all.
func classToRuntime(class types.ConfinementClass, info system.Info) (runtimeName string, needsRuntime bool, err error) {
	switch class {
	case types.CC1, "":
		return "", false, nil
	case types.CC2:
		name := pickRuntime(info, runtimeRunsc)
		if name == "" {
			// SECURITY: fail closed rather than silently downgrade to runc (invariant 5).
			return "", false, fmt.Errorf("the Wall tier (CC2) requires the gVisor (runsc) runtime, which is not installed on this Docker host: %w", errRuntimeUnavailable)
		}
		return name, true, nil
	case types.CC3:
		for _, family := range cc3Runtimes {
			if name := pickRuntime(info, family); name != "" {
				return name, true, nil
			}
		}
		return "", false, fmt.Errorf("the Vault tier (CC3) requires a KVM microVM runtime (Kata or krun/libkrun), none of which is installed on this Docker host: %w", errRuntimeUnavailable)
	default:
		return "", false, fmt.Errorf("unknown confinement class %q: %w", class, errRuntimeUnavailable)
	}
}

// verifyCapsEnforced fails closed when the daemon reports it DISCARDED a requested CPU/memory/pids
// limit — the AUTHORITATIVE post-create signal from the ContainerCreate response. On cgroup-v1
// under rootless Docker the daemon silently drops the limit, leaving an untrusted sandbox uncapped.
//
// SECURITY: replaces a pre-flight `docker info` capability check (unreliable on Podman's
// Docker-compat API); the create-time discard warning is authoritative on both engines (verified).
// Mirrors classToRuntime's fail-closed contract (invariant 5).
func verifyCapsEnforced(createWarnings []string) error {
	discarded := dockerutil.DiscardedLimits(createWarnings)
	if len(discarded) == 0 {
		return nil
	}
	// Adjacent literals only to keep the source line under the lll cap.
	return fmt.Errorf("the Docker daemon discarded a requested resource limit — an untrusted sandbox would run without it: %s. "+
		"On cgroup v2, delegate the controllers to the runtime user (systemd unit: Delegate=yes; rootless Docker: enable cgroup v2 delegation per the rootless docs). "+
		"Set WARDYN_ALLOW_UNENFORCEABLE_CAPS=1 to override on a TRUSTED host: %w",
		strings.Join(discarded, "; "), errCapsUnenforceable)
}

// resolveRuntime is classToRuntime with operator overrides applied: an override pins the exact
// runtime family a class must use (still probed against `docker info`, fail closed when absent);
// no override reproduces the built-in default. overrides may be nil.
func resolveRuntime(class types.ConfinementClass, info system.Info, overrides map[types.ConfinementClass]string) (runtimeName string, needsRuntime bool, err error) {
	want, pinned := overrides[class]
	if !pinned || want == "" {
		return classToRuntime(class, info) // default path, unchanged
	}
	// SECURITY (isolation floor): a pin must not silently downgrade a class below its gated
	// isolation family (e.g. CC2=runc), rejected fail-closed. CC1 is the weakest tier, unrestricted.
	switch class {
	case types.CC2:
		if !strings.HasPrefix(want, runtimeRunsc) {
			return "", false, fmt.Errorf("the Wall tier (CC2) pins runtime %q, which does not deliver gVisor (%s) isolation; refusing to downgrade: %w", want, runtimeRunsc, errRuntimeUnavailable)
		}
	case types.CC3:
		// The auto-mapping stays limited to the known-VM allowlist (kata, krun), but an explicit pin
		// may name any registered runtime the operator vouches delivers a VM boundary (BYO microVM).
		if runner.IsKnownNonVaultRuntime(want) {
			return "", false, fmt.Errorf("the Vault tier (CC3) pins runtime %q, a known shared-kernel/userspace-kernel runtime that does not deliver KVM microVM isolation; refusing to downgrade: %w", want, errRuntimeUnavailable)
		}
	}
	name := pickRuntime(info, want)
	if name == "" {
		return "", false, fmt.Errorf("confinement class %s pins runtime %q, which is not installed on this Docker host: %w", class, want, errRuntimeUnavailable)
	}
	return name, true, nil
}

// runtimeOrRunc labels the daemon-default runtime as "runc" for advertisement.
func runtimeOrRunc(rt string) string {
	if rt == "" {
		return "runc"
	}
	return rt
}

// pickRuntime returns the first registered runtime matching or prefixed by want (Kata ships as
// "kata", "kata-runtime", "kata-qemu", ...), or "".
func pickRuntime(info system.Info, want string) string {
	if _, ok := info.Runtimes[want]; ok { // exact match first, then prefix (sorted for stable selection)
		return want
	}
	names := slices.Sorted(maps.Keys(info.Runtimes))
	for _, name := range names {
		if strings.HasPrefix(name, want) {
			return name
		}
	}
	return "" // every caller fails closed on empty
}

// capabilitiesForWith maps a probed `docker info` to the driver Capabilities the control plane uses
// for Confinement-Class gating, honoring operator runtime overrides (nil for the built-in default).
// SECURITY: a class is advertised ONLY when its (possibly pinned) enforcing runtime is actually
// registered (invariant 5). Resolved carries the per-class substrate label ("oci/<runtime>") for /healthz.
func capabilitiesForWith(info system.Info, overrides map[types.ConfinementClass]string, record bool) runner.Capabilities {
	classes := []types.ConfinementClass{}
	resolved := map[types.ConfinementClass]string{}
	effective := map[types.ConfinementClass]string{} // runtime NAME each class actually runs on, not the display label
	// CC1 is the floor, but an operator CC1 override (e.g. sysbox) can still be unhonorable when
	// absent; treated like CC2/CC3 (invariant 5).
	if rt, _, err := resolveRuntime(types.CC1, info, overrides); err == nil {
		classes = append(classes, types.CC1)
		resolved[types.CC1] = "oci/" + runtimeOrRunc(rt)
		effective[types.CC1] = cmp.Or(rt, info.DefaultRuntime) // same hop runtimeSupportsRecursiveReadOnly makes
	}
	if rt, _, err := resolveRuntime(types.CC2, info, overrides); err == nil {
		classes = append(classes, types.CC2)
		resolved[types.CC2] = "oci/" + rt
		effective[types.CC2] = rt
	}
	if rt, _, err := resolveRuntime(types.CC3, info, overrides); err == nil {
		classes = append(classes, types.CC3)
		resolved[types.CC3] = "oci/" + rt
		effective[types.CC3] = rt
	}
	// Gated on the SAME probe applyDiskQuota consults; `none` covers both "no size option" (uncapped)
	// and overlay2-over-non-xfs (fails closed) — a reader must not assume "uncapped" means the former.
	disk := types.StorageEnforcementNone
	if storageDriverSupportsQuota(info) {
		disk = types.StorageEnforcementFilesystem
	}
	// ContainerPause/Unpause is verified only on runc; runsc/Kata pause are UNVERIFIED, so every
	// other runtime reports false rather than assume an unproven control.
	freeze := map[types.ConfinementClass]bool{}
	for class, rt := range effective {
		freeze[class] = rt == "runc"
	}
	return runner.Capabilities{
		Driver:                   driverName,
		ConfinementClasses:       classes, // strongest last
		Resolved:                 resolved,
		EphemeralDiskEnforcement: disk,
		Freeze:                   freeze,
		// L0 is structural: NetworkMode "none" + internal-only per-run network means no default
		// route and one egress path.
		StructuralEgress: true,
		NetworkPolicy:    false, // L1 nftables default-deny is v0.5 — don't claim it yet
		// Exec wraps the agent with wardyn-rec only when Config.Record is on; advertising it on a
		// Record=false substrate would make the site-config probe warn about an upload never coming.
		SessionRecording: record,
	}
}

// hardenedHostConfig builds the agent container's HostConfig with Wardyn's non-negotiable
// hardening. networkMode is set by the caller (always "none" at create time for L0). runtimeName is
// "" for the daemon default (CC1) or the resolved runtime for CC2/CC3.
//
// SECURITY constraints encoded here:
//   - CapDrop ALL, no-new-privileges: no Linux capabilities, no setuid escalation.
//   - seccomp: NEVER "unconfined", so Docker's RuntimeDefault profile stays in force.
//   - AppArmor: pinned to docker-default on CC1 (runc) when the host supports it; omitted for
//     non-runc runtimes, where it's at best a no-op and can error (invariant 5).
//   - tmpfs /tmp: writable scratch without a writable rootfs.
//   - Resources: hard caps from the spec, conservative defaults for any zero field so EVERY sandbox
//     is capped (MemorySwap pinned; PidsLimit always set).
//   - StorageOpt: a per-container quota when requested AND enforceable; overlay2 on non-xfs fails
//     the create closed (applyDiskQuota).
//   - userns: left to daemon-wide config, not forced per-container.
func hardenedHostConfig(networkMode string, runtimeName string, res runner.Resources, info system.Info) *container.HostConfig {
	secOpt := []string{"no-new-privileges"}
	// Pin AppArmor for CC1 (runc) when the host advertises support, to avoid Docker rejecting the
	// option on hosts without it (many non-Ubuntu kernels, WSL2).
	if runtimeName == "" && hostSupportsAppArmor(info) {
		secOpt = append(secOpt, "apparmor=docker-default")
	}
	// gVisor (runsc) has no SELinux integration and refuses to start under selinux-enabled, so
	// disable labeling for the runsc path ONLY — gVisor's own sandbox is the boundary there. NEVER
	// on runc (CC1), a real defense we keep.
	if strings.HasPrefix(runtimeName, runtimeRunsc) && hostSupportsSELinux(info) {
		secOpt = append(secOpt, "label=disable")
	}

	hc := &container.HostConfig{
		NetworkMode:    container.NetworkMode(networkMode),
		CapDrop:        []string{"ALL"},
		SecurityOpt:    secOpt,
		ReadonlyRootfs: false, // workspace tooling writes; /tmp is tmpfs below
		Tmpfs: map[string]string{
			"/tmp": "rw,nosuid,nodev,noexec,size=256m",
		},
		Runtime:    runtimeName, // "" => daemon default runtime (runc)
		AutoRemove: false,       // teardown is explicit so Status can observe exit
		Resources:  resourcesFromSpec(res),
	}
	// krun/libkrun opens /dev/kvm from INSIDE the container's namespaces (the VMM is the container's
	// own process), so the device must be handed in or the microVM can't start. krun-SPECIFIC: Kata's
	// shim opens /dev/kvm host-side, so a Kata container must NOT get it (host KVM exposed to guest).
	if strings.HasPrefix(runtimeName, runtimeKrun) {
		hc.Devices = append(hc.Devices, container.DeviceMapping{
			PathOnHost:        "/dev/kvm",
			PathInContainer:   "/dev/kvm",
			CgroupPermissions: "rwm",
		})
		// /dev/kvm is mode 0660 root:kvm and the agent is non-root, so CapDrop ALL (no
		// CAP_DAC_OVERRIDE) means EACCES without this: add the device's group as a supplementary
		// group instead (no capability regained). Skip silently if the gid can't resolve — fails
		// closed at VM boot as a harder-to-diagnose EACCES.
		if gid := kvmDeviceGID(); gid != "" {
			hc.GroupAdd = append(hc.GroupAdd, gid)
		}
	}
	applyDiskQuota(hc, res, info)
	hc.Annotations = stripDangerousKataAnnotations(hc.Annotations) // belt-and-suspenders; see dangerousKataAnnotations
	return hc
}

// kvmDeviceGID returns the numeric GID owning /dev/kvm (for HostConfig.GroupAdd), preferring the
// device node's actual group over the name "kvm". "" if neither resolves. Assumes wardynd shares
// the host with the KVM device (remote-daemon CC3 is out of scope).
func kvmDeviceGID() string {
	if fi, err := os.Stat("/dev/kvm"); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			return strconv.FormatUint(uint64(st.Gid), 10)
		}
	}
	if g, err := user.LookupGroup("kvm"); err == nil {
		return g.Gid
	}
	return ""
}

// orgDefaultDiskNotEnforceable: the host was handed the ORG's default disk size, not a policy's own
// request, and cannot keep it — the run goes UNCAPPED instead of failing closed (see
// runner.Resources.DiskMiBFilled for why that asymmetry is the point).
//
// DRAFT (M2 canon pending)
const orgDefaultDiskNotEnforceable = "wardyn/docker: org default not enforceable on this host — " +
	"storage.ephemeral.default_disk_mib was filled in for this run and overlay2 over non-xfs cannot carry the quota, " +
	"so the run proceeds WITHOUT a disk cap rather than failing closed (a policy's own disk_mib still fails closed here); " +
	"enforcement: none, matching /setup/status"

// applyDiskQuota sets the per-container writable-storage cap (StorageOpt "size") when DiskMiB>0 (0
// never warns). Above 0, four outcomes:
//
//   - Driver can ENFORCE the cap: the size opt goes on the create, real cap.
//   - Driver TAKES a size opt but the backing filesystem can't carry the quota (overlay2 on
//     non-xfs): the opt goes on anyway and the daemon refuses it, fails closed rather than running
//     with a cap the host never applied — warned FIRST, naming xfs.
//   - SAME HOST, but the size was FILLED IN from the org's default_disk_mib (res.DiskMiBFilled):
//     runs uncapped with a warning instead — nobody asked this run for a cap, and fail-closed here
//     would brick every request-less desktop run (overlay2/ext4) the day an admin sets a default.
//   - Driver can't take a size opt at all (vfs, fuse-overlayfs, ...): warn and proceed uncapped
//     rather than hard-break — never silently claim an unenforceable control, but don't punish a run
//     for the operator's storage-driver choice. docs/POLICIES.md's disk_mib row states this; k8s behaves the same.
//
// ponytail: every outcome is disclosed by slog alone (a driver has no run-warnings channel); the
// word the ADMIN reads is carried separately, on Capabilities.EphemeralDiskEnforcement.
func applyDiskQuota(hc *container.HostConfig, res runner.Resources, info system.Info) {
	if res.DiskMiB <= 0 {
		return
	}
	switch {
	case storageDriverSupportsQuota(info):
		// Enforced.
	case storageDriverTakesSizeOpt(info.Driver) && res.DiskMiBFilled:
		slog.Warn(orgDefaultDiskNotEnforceable,
			slog.Int64("disk_mib", res.DiskMiB),
			slog.String("storage_driver", info.Driver),
			slog.String("backing_filesystem", strings.ToLower(driverStatusValue(info, "Backing Filesystem"))),
			slog.String("enforcement", string(types.StorageEnforcementNone)),
		)
		return
	case storageDriverTakesSizeOpt(info.Driver):
		// Reachable for overlay2 ONLY: btrfs/zfs take the opt and always enforce it, already answered above.
		slog.Warn("wardyn/docker: disk cap on overlay2 over non-xfs; needs xfs mounted with the pquota option, so the daemon will REFUSE this create and fail closed (move data-root to xfs+pquota, or drop disk_mib); enforcement: none, matching /setup/status",
			slog.Int64("disk_mib", res.DiskMiB),
			slog.String("storage_driver", info.Driver),
			slog.String("backing_filesystem", strings.ToLower(driverStatusValue(info, "Backing Filesystem"))),
			slog.String("enforcement", string(types.StorageEnforcementNone)),
		)
	default:
		slog.Warn("wardyn/docker: disk cap requested but the storage driver does not support a per-container size quota (need overlay2 on xfs mounted with pquota, or btrfs/zfs); running WITHOUT a disk cap; reported enforcement is none, same as the daemon reports on /setup/status",
			slog.Int64("disk_mib", res.DiskMiB),
			slog.String("storage_driver", info.Driver),
			slog.String("enforcement", string(types.StorageEnforcementNone)),
		)
		return
	}
	if hc.StorageOpt == nil {
		hc.StorageOpt = map[string]string{}
	}
	hc.StorageOpt["size"] = fmt.Sprintf("%dm", res.DiskMiB)
}

// storageDriverSupportsQuota reports, best-effort from `docker info`, whether the storage driver
// can ENFORCE a per-container `size` quota: overlay2 honors it on xfs ONLY (the driver's own
// contract; ext4 is NOT supported, significant since overlay2/ext4 is the default on Docker
// Desktop/WSL2 and stock Ubuntu/Debian); btrfs and zfs support it natively; every other driver
// (vfs, devicemapper on loopback, fuse-overlayfs, ...) cannot.
//
// `docker info` reports the backing filesystem but never the pquota MOUNT OPTION, so xfs here is
// necessary but not sufficient: an xfs data-root without pquota still fails the create
// (applyDiskQuota's fail-closed posture). This is "can this host enforce it" alone;
// storageDriverTakesSizeOpt is "will the daemon even look at a size opt"; applyDiskQuota uses both.
func storageDriverSupportsQuota(info system.Info) bool {
	switch info.Driver {
	case "overlay2":
		return strings.ToLower(driverStatusValue(info, "Backing Filesystem")) == "xfs"
	case "btrfs", "zfs":
		return true
	default:
		return false
	}
}

// storageDriverTakesSizeOpt reports whether the storage driver accepts a `size` storage-opt AT ALL
// — wider than the drivers that can enforce one. The gap is exactly overlay2 on non-xfs, the branch
// that fails the run closed at create rather than degrading to uncapped.
func storageDriverTakesSizeOpt(driver string) bool {
	switch driver {
	case "overlay2", "btrfs", "zfs":
		return true
	default:
		return false
	}
}

// driverStatusValue returns the value for key in `docker info`'s DriverStatus, or "" when absent (case-insensitive).
func driverStatusValue(info system.Info, key string) string {
	for _, kv := range info.DriverStatus {
		if strings.EqualFold(kv[0], key) {
			return kv[1]
		}
	}
	return ""
}

// hostSupportsSELinux reports whether the daemon advertises SELinux in `docker info`
// SecurityOptions ("name=selinux"). Gates the runsc `label=disable` opt so gVisor containers start
// on enforcing hosts — mirrors hostSupportsAppArmor.
func hostSupportsSELinux(info system.Info) bool {
	for _, opt := range info.SecurityOptions {
		for _, field := range strings.Split(opt, ",") {
			if strings.TrimSpace(field) == "name=selinux" {
				return true
			}
		}
	}
	return false
}

// hostSupportsAppArmor reports whether the daemon advertises AppArmor in `docker info`
// SecurityOptions. Gates the CC1 apparmor=docker-default pin so Wardyn never passes an option the host rejects.
func hostSupportsAppArmor(info system.Info) bool {
	for _, opt := range info.SecurityOptions {
		for _, field := range strings.Split(opt, ",") {
			if strings.TrimSpace(field) == "name=apparmor" {
				return true
			}
		}
	}
	return false
}

// proxyResources is the proxy sidecar's cgroup envelope: a tight PID cap (fork-bomb guard) and
// memory cap (MemorySwap pinned so swap can't double it). CPU is unconstrained — latency-path and
// already PID/memory bounded.
func proxyResources() container.Resources {
	memBytes := proxyMemoryMiB * 1024 * 1024
	pids := proxyPidsLimit
	return container.Resources{
		Memory:     memBytes,
		MemorySwap: memBytes,
		PidsLimit:  &pids,
	}
}

// resourcesFromSpec converts Wardyn resource caps to Docker cgroup limits, applying conservative
// defaults for any zero field so every sandbox is capped even when policy sets nothing (a non-zero
// spec value always overrides its default):
//   - CPUMillis -> NanoCPUs (1000 millis == 1 CPU == 1e9 nanos).
//   - MemoryMiB -> Memory (bytes), AND MemorySwap pinned EQUAL to Memory so Docker doesn't silently
//     allow ~2x the cap via swap (unset MemorySwap defaults to double Memory).
//   - PidsLimit set UNCONDITIONALLY (fork-bomb guard): a non-nil *int64 so a fork bomb can't exhaust
//     the host PID space.
//
// DiskMiB is handled separately via StorageOpt (applyDiskQuota): a HostConfig field, not a cgroup
// Resources field, and backend-gated.
func resourcesFromSpec(res runner.Resources) container.Resources {
	cpuMillis := res.CPUMillis
	if cpuMillis <= 0 {
		cpuMillis = runner.DefaultCPUMillis
	}
	memMiB := res.MemoryMiB
	if memMiB <= 0 {
		memMiB = runner.DefaultMemoryMiB
	}
	pids := res.PidsLimit
	if pids <= 0 {
		pids = runner.DefaultPidsLimit
	}
	memBytes := memMiB * 1024 * 1024
	pidsLimit := pids // addressable for the *int64 field
	return container.Resources{
		NanoCPUs:   cpuMillis * 1_000_000, // millis -> nanos
		Memory:     memBytes,
		MemorySwap: memBytes, // pin swap to the memory cap, or Docker defaults MemorySwap to 2*Memory
		PidsLimit:  &pidsLimit,
	}
}
