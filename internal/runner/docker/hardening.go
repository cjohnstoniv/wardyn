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

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Proxy sidecar caps. The wardyn-proxy does little but relay HTTP, so a tight
// envelope leaves ample headroom while still bounding a compromised proxy: it
// gets its own PID cap (fork-bomb guard) and a modest memory cap, independent
// of the agent's (larger) spec-driven caps.
const (
	proxyPidsLimit int64 = 128
	proxyMemoryMiB int64 = 256
)

// Docker runtime names probed from `docker info`. Mapping to Confinement
// Classes is conservative: we only claim a class when its enforcing runtime
// is actually installed (invariant 5: never overclaim).
const (
	runtimeRunsc  = "runsc"  // gVisor       -> CC2 (userspace-kernel sandbox)
	runtimeKata   = "kata"   // Kata         -> CC3 (KVM microVM via its own containerd shim-v2)
	runtimeKrun   = "krun"   // crun+libkrun -> CC3 (KVM microVM as a crun-based OCI runtime binary)
	runtimeSysbox = "sysbox" // sysbox-runc: stronger CC1, still shared-kernel
)

// dangerousKataAnnotations are the two Kata hypervisor-config annotations
// that let a workload override its OWN microVM's virtio-fs / kernel
// configuration — the knobs behind CVE-2026-44210/-47243 (a compromised
// Kata guest reaching host-root via virtiofsd). Nothing today ever sets
// hc.Annotations, so this is defense-in-depth against a FUTURE source,
// enforced at the one chokepoint every launched container's HostConfig
// passes through (hardenedHostConfig).
var dangerousKataAnnotations = []string{
	"io.katacontainers.config.hypervisor.virtio_fs_extra_args",
	"io.katacontainers.config.hypervisor.kernel_params",
}

// stripDangerousKataAnnotations deletes the dangerousKataAnnotations keys
// from ann and returns it. Nil-safe: delete on a nil map is a no-op.
func stripDangerousKataAnnotations(ann map[string]string) map[string]string {
	for _, k := range dangerousKataAnnotations {
		delete(ann, k)
	}
	return ann
}

// cc3Runtimes are the runtime families that deliver the Vault tier's
// hardware (KVM) VM isolation. CC3 is defined by the GUARANTEE — a real
// per-sandbox VM boundary — not one product: kata* (its own containerd
// shim-v2) and krun (libkrun, a KVM microVM via the standard runc shim).
// Probed in this order; first installed wins. Bare "crun" (no libkrun) is
// shared-kernel and deliberately NOT accepted for CC3.
var cc3Runtimes = []string{runtimeKata, runtimeKrun}

// runtimeSupportsExec reports whether the OCI runtime can enter a running
// container via `docker exec`. Every runtime we use can EXCEPT krun/libkrun:
// a libkrun microVM has no in-guest exec agent, so its workload must run as
// the container's MAIN process instead of being exec'd in.
func runtimeSupportsExec(runtimeName string) bool {
	return !strings.HasPrefix(runtimeName, runtimeKrun)
}

const (
	// ociFeaturesStatusKey is the `docker info` runtime-status key carrying
	// that runtime's OCI features struct as JSON (API v1.44+) — the SAME
	// evidence the daemon itself decides on, rather than guessing from a name.
	ociFeaturesStatusKey = "org.opencontainers.runtime-spec.features"
	// ociMountOptionRRO is the OCI mount option for a RECURSIVELY read-only
	// bind, the one BindOptions.ReadOnlyForceRecursive asks the runtime for.
	ociMountOptionRRO = "rro"
)

// runtimeSupportsRecursiveReadOnly reports whether runtimeName — as the
// DAEMON describes it in `docker info` — declares the OCI `rro` mount
// option.
//
// IT IS A PRE-FLIGHT FOR A CREATE THE DAEMON WOULD OTHERWISE REFUSE, not an
// optimisation: moby's supportsRecursivelyReadOnly errors for a runtime that
// doesn't list `rro`, and container_routes then fails the create. gVisor is
// exactly that runtime (`runsc features` lists `ro`/`rbind`, no `rro`), so
// this avoids refusing every read-only share drive at ContainerCreate under
// CC2, this product's default confinement floor.
//
// UNKNOWN READS AS UNSUPPORTED, matching the direction the DAEMON itself
// takes: a daemon with no features for this runtime would refuse the
// create anyway, so a request it can't honour is a run that doesn't start,
// not a stronger guarantee.
func runtimeSupportsRecursiveReadOnly(info system.Info, runtimeName string) bool {
	name := runtimeName
	if name == "" {
		// "" is the daemon's DEFAULT runtime (CC1) — the same hop the daemon
		// makes before consulting the features struct.
		name = info.DefaultRuntime
	}
	rt, ok := info.Runtimes[name]
	if !ok {
		return false
	}
	// Only the one field is decoded: a single yes/no question, not the whole
	// (large, growing) OCI features struct.
	var feats struct {
		MountOptions []string `json:"mountOptions"`
	}
	if err := json.Unmarshal([]byte(rt.Status[ociFeaturesStatusKey]), &feats); err != nil {
		return false
	}
	return slices.Contains(feats.MountOptions, ociMountOptionRRO)
}

// classToRuntime returns the Docker runtime name required to enforce class,
// and whether a non-default runtime is needed at all. CC1 uses the default
// (runc) runtime; CC2 requires runsc; CC3 requires a kata runtime.
func classToRuntime(class types.ConfinementClass, info system.Info) (runtimeName string, needsRuntime bool, err error) {
	switch class {
	case types.CC1, "":
		return "", false, nil
	case types.CC2:
		name := pickRuntime(info, runtimeRunsc)
		if name == "" {
			// Fail closed: policy demanded CC2 but gVisor is absent. Never
			// silently downgrade to runc (invariant 5).
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

// verifyCapsEnforced fails closed when the Docker daemon reports it
// DISCARDED a requested CPU / memory / pids limit — the AUTHORITATIVE
// post-create signal, read from the ContainerCreate response. On a
// cgroup-v1 host under rootless Docker, the daemon silently drops the limit
// and appends a "…Limitation discarded" warning, leaving an untrusted
// sandbox effectively uncapped.
//
// This replaces a pre-flight `docker info` capability check: those booleans
// are NOT reliable on Podman's Docker-compat API (it under-reports
// CpuCfsQuota=false even when the quota binds). The daemon's own
// create-time discard warning is authoritative on both engines (verified).
// Mirrors classToRuntime's fail-closed contract (invariant 5).
func verifyCapsEnforced(createWarnings []string) error {
	var discarded []string
	for _, w := range createWarnings {
		if strings.Contains(strings.ToLower(w), "discard") {
			discarded = append(discarded, strings.TrimSpace(w))
		}
	}
	if len(discarded) == 0 {
		return nil
	}
	// Adjacent string literals only to keep the SOURCE line under the lll cap.
	return fmt.Errorf("the Docker daemon discarded a requested resource limit — an untrusted sandbox would run without it: %s. "+
		"On cgroup v2, delegate the controllers to the runtime user (systemd unit: Delegate=yes; rootless Docker: enable cgroup v2 delegation per the rootless docs). "+
		"Set WARDYN_ALLOW_UNENFORCEABLE_CAPS=1 to override on a TRUSTED host: %w",
		strings.Join(discarded, "; "), errCapsUnenforceable)
}

// resolveRuntime is classToRuntime with operator overrides applied: the
// substrate-selection seam for CC3 (and CC2). An override pins the EXACT
// runtime family a class must use (still probed against `docker info` and
// FAIL CLOSED when absent); no override reproduces the built-in default
// mapping. overrides may be nil.
func resolveRuntime(class types.ConfinementClass, info system.Info, overrides map[types.ConfinementClass]string) (runtimeName string, needsRuntime bool, err error) {
	want, pinned := overrides[class]
	if !pinned || want == "" {
		return classToRuntime(class, info) // default path, unchanged
	}
	// Isolation floor: a pin must not silently downgrade a class below the
	// isolation family it's gated as (e.g. WARDYN_CONFINEMENT_MAP="CC2=runc"
	// would run shared-kernel under a CC2 gate). Rejected fail-closed, same
	// error class as an absent runtime. CC1 is the weakest tier, so any pin
	// (including a stronger one like sysbox) is left unrestricted.
	switch class {
	case types.CC2:
		if !strings.HasPrefix(want, runtimeRunsc) {
			return "", false, fmt.Errorf("the Wall tier (CC2) pins runtime %q, which does not deliver gVisor (%s) isolation; refusing to downgrade: %w", want, runtimeRunsc, errRuntimeUnavailable)
		}
	case types.CC3:
		// The built-in auto-mapping stays limited to the known-VM allowlist
		// (kata, krun), but an EXPLICIT pin may name ANY registered runtime
		// the operator vouches delivers a VM boundary (bring-your-own
		// microVM). Still refused: runtimes POSITIVELY known to deliver less
		// than a VM (shared-kernel runc/crun/sysbox, or gVisor/runsc).
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

// pickRuntime returns the first registered runtime whose name equals or is
// prefixed by want (Kata ships as "kata", "kata-runtime", "kata-qemu", ...).
// Returns "" when no matching runtime is registered.
func pickRuntime(info system.Info, want string) string {
	// Exact match first (deterministic), then prefix match (sorted for
	// stable selection across daemons).
	if _, ok := info.Runtimes[want]; ok {
		return want
	}
	names := slices.Sorted(maps.Keys(info.Runtimes))
	for _, name := range names {
		if strings.HasPrefix(name, want) {
			return name
		}
	}
	return "" // Every caller fails closed on empty.
}

// capabilitiesForWith maps a probed `docker info` to the driver Capabilities the
// control plane uses for Confinement-Class gating, honoring operator runtime
// overrides (pass nil for the built-in default runtime mapping). A
// class is advertised ONLY when its (possibly pinned) enforcing runtime is
// actually registered — overrides never overclaim (invariant 5). Resolved
// carries the per-class substrate label ("oci/<runtime>") for /healthz.
func capabilitiesForWith(info system.Info, overrides map[types.ConfinementClass]string, record bool) runner.Capabilities {
	classes := []types.ConfinementClass{}
	resolved := map[types.ConfinementClass]string{}
	// effective is the runtime NAME each class is actually scheduled on, not
	// the display label (CC1's "" is the daemon's default runtime).
	effective := map[types.ConfinementClass]string{}
	// CC1 is the floor, but an operator CC1 override (e.g. a sysbox pin) can
	// still be unhonorable when absent. Treated exactly like CC2/CC3: never
	// advertise a class whose runtime can't be enforced (invariant 5).
	if rt, _, err := resolveRuntime(types.CC1, info, overrides); err == nil {
		classes = append(classes, types.CC1)
		resolved[types.CC1] = "oci/" + runtimeOrRunc(rt)
		effective[types.CC1] = cmp.Or(rt, info.DefaultRuntime) // the same hop runtimeSupportsRecursiveReadOnly makes
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
	// Gated on the SAME probe applyDiskQuota consults. `none` covers both a
	// driver that takes no size option (runs uncapped) and overlay2 over
	// non-xfs (fails closed instead) — a reader of `none` must not assume
	// "uncapped" (§6.2 covers both).
	disk := types.StorageEnforcementNone
	if storageDriverSupportsQuota(info) {
		disk = types.StorageEnforcementFilesystem
	}
	// Freeze (RL-6): ContainerPause/Unpause is verified only on runc. runsc
	// and Kata pause are UNVERIFIED (RL-0 spike), so every other effective
	// runtime reports false rather than assume a control nobody proved.
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
		// L0 is structural here: NetworkMode "none" + internal-only per-run
		// network means the agent has no default route and one egress path.
		StructuralEgress: true,
		// L1 nftables default-deny is v0.5 — be honest, do not claim it.
		NetworkPolicy: false,
		// Exec wraps the agent with wardyn-rec only when Config.Record is on
		// (recordCmd); advertising recording a Record=false substrate never
		// performs would make the site-config probe warn about a cast that was
		// never going to be uploaded.
		SessionRecording: record,
	}
}

// hardenedHostConfig builds the agent container's HostConfig with Wardyn's
// non-negotiable hardening. networkMode is set by the caller (always "none"
// at create time for L0). runtimeName is "" for the daemon default (CC1) or
// the resolved runtime for CC2/CC3.
//
// Constraints encoded here:
//   - CapDrop ALL, no-new-privileges: no Linux capabilities, no setuid escalation.
//   - seccomp: we NEVER pass "seccomp=unconfined", so Docker's RuntimeDefault
//     profile stays in force; no custom profile shipped.
//   - AppArmor: pinned to docker-default on CC1 (runc) when the host
//     supports it; omitted for non-runc runtimes, where it's at best a
//     no-op and can error (invariant 5).
//   - tmpfs /tmp: writable scratch without a writable rootfs.
//   - Resources: hard caps from the spec, with conservative defaults for
//     any zero field so EVERY sandbox is capped (MemorySwap pinned so the
//     cap isn't silently doubled; PidsLimit set unconditionally).
//   - StorageOpt: a per-container quota when requested AND enforceable; a
//     driver that can't take the opt logs a warning and runs uncapped,
//     while overlay2 on non-xfs fails the create closed (applyDiskQuota).
//   - userns: left to daemon-wide config, not forced per-container.
func hardenedHostConfig(networkMode string, runtimeName string, res runner.Resources, info system.Info) *container.HostConfig {
	secOpt := []string{"no-new-privileges"}
	// Pin AppArmor explicitly for CC1 (default runc runtime) when the host
	// advertises AppArmor support. Gating on support avoids Docker rejecting
	// the option on hosts without AppArmor (many non-Ubuntu kernels, WSL2).
	if runtimeName == "" && hostSupportsAppArmor(info) {
		secOpt = append(secOpt, "apparmor=docker-default")
	}
	// gVisor (runsc) has no SELinux integration and refuses to start under
	// selinux-enabled. Disable labeling for the runsc path ONLY — gVisor's
	// own sandbox is the isolation boundary there. NEVER on runc (CC1),
	// where SELinux is a real defense we keep; Kata is unaffected.
	if strings.HasPrefix(runtimeName, runtimeRunsc) && hostSupportsSELinux(info) {
		secOpt = append(secOpt, "label=disable")
	}

	hc := &container.HostConfig{
		NetworkMode:    container.NetworkMode(networkMode),
		CapDrop:        []string{"ALL"},
		SecurityOpt:    secOpt,
		ReadonlyRootfs: false, // workspace tooling writes; /tmp is tmpfs below.
		Tmpfs: map[string]string{
			"/tmp": "rw,nosuid,nodev,noexec,size=256m",
		},
		Runtime: runtimeName, // "" => daemon default runtime (runc).
		// Never auto-remove: teardown is explicit so Status can observe exit.
		AutoRemove: false,
		Resources:  resourcesFromSpec(res),
	}
	// krun/libkrun opens /dev/kvm from INSIDE the container's namespaces (the VMM is
	// the container's own process), so the KVM device must be handed into the
	// container — the docker-run equivalent is `--device /dev/kvm`; without it the
	// microVM cannot start. This is krun-SPECIFIC: Kata's containerd shim opens
	// /dev/kvm host-side and boots the VM outside the container, so a Kata container
	// must NOT receive /dev/kvm (that would expose host KVM to the guest, i.e. nested
	// virt in the Kata guest). CC1 (runc)/CC2 (runsc) never get it either.
	if strings.HasPrefix(runtimeName, runtimeKrun) {
		hc.Devices = append(hc.Devices, container.DeviceMapping{
			PathOnHost:        "/dev/kvm",
			PathInContainer:   "/dev/kvm",
			CgroupPermissions: "rwm",
		})
		// /dev/kvm is mode 0660 root:kvm and the agent runs as a NON-root user, so
		// once CapDrop ALL removes CAP_DAC_OVERRIDE the open fails EACCES ("Error
		// creating the Kvm object"). Add the device's owning group as a supplementary
		// group so the agent can open it via group perms — no capability regained,
		// hardening intact. Skip silently if the gid can't be resolved: the run then
		// fails closed at VM boot rather than the harder-to-diagnose EACCES.
		if gid := kvmDeviceGID(); gid != "" {
			hc.GroupAdd = append(hc.GroupAdd, gid)
		}
	}
	applyDiskQuota(hc, res, info)
	// Belt-and-suspenders (see dangerousKataAnnotations): strip the two dangerous
	// Kata hypervisor-override annotations no matter what, even though nothing
	// above ever populates hc.Annotations today.
	hc.Annotations = stripDangerousKataAnnotations(hc.Annotations)
	return hc
}

// kvmDeviceGID returns the numeric GID owning /dev/kvm on the host (as a string
// for HostConfig.GroupAdd), preferring the device node's actual group over the
// name "kvm" so it is correct even on hosts where the group is named differently.
// Returns "" when neither resolves. Assumes wardynd shares the host with the KVM
// device (true for the CC3/local case; a remote-daemon CC3 is out of scope).
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

// orgDefaultDiskNotEnforceable is what a host says when the size it was handed
// is the ORG's default rather than a policy's own request and this host cannot
// keep it: the run goes UNCAPPED instead of failing closed. See
// runner.Resources.DiskMiBFilled for why that asymmetry is the whole point of
// the bit.
//
// DRAFT (M2 canon pending)
const orgDefaultDiskNotEnforceable = "wardyn/docker: org default not enforceable on this host — " +
	"storage.ephemeral.default_disk_mib was filled in for this run and overlay2 over non-xfs cannot carry the quota, " +
	"so the run proceeds WITHOUT a disk cap rather than failing closed (a policy's own disk_mib still fails closed here); " +
	"enforcement: none, matching /setup/status"

// applyDiskQuota sets the per-container writable-storage cap (StorageOpt "size")
// when the spec requests one (DiskMiB>0). A DiskMiB of 0 is the common case and
// never warns. Above 0 there are exactly four outcomes, and which one a host
// gets is decided here rather than left to a surprise at ContainerCreate:
//
//   - The driver can ENFORCE the cap (storageDriverSupportsQuota): the size opt
//     goes on the create and the cap is real.
//   - The driver TAKES a size opt but this host's backing filesystem cannot
//     carry the project quota it needs (overlay2 on anything but xfs): the opt
//     goes on the create ANYWAY and the daemon refuses it, so the run fails
//     closed rather than running with a cap the policy promised and the host
//     never applied. Only the daemon can settle this (see
//     storageDriverSupportsQuota), so we warn FIRST, naming xfs — otherwise the
//     operator's only evidence is the daemon's own bare create error.
//   - SAME HOST, but the size was FILLED IN from the org's
//     storage.ephemeral.default_disk_mib (res.DiskMiBFilled): the run goes
//     uncapped with a warning instead. Nobody asked this run for a cap; an
//     org-wide default reaches laptops by MDM and the desktop tier IS overlay2
//     over ext4, so fail-closed here would brick every request-less desktop run
//     the day an admin typed a number for the cluster half of the estate.
//   - The driver cannot take a size opt at all (vfs, fuse-overlayfs, ...): we do
//     NOT hard-break the run. We log a clear, visible warning and proceed
//     uncapped — the codebase's "visible blindness" posture (never silently
//     claim a control we cannot enforce, but do not punish a run for an
//     operator's storage-driver choice). docs/POLICIES.md's disk_mib row states
//     this outcome, and the k8s substrate behaves the same way.
//
// ponytail: every outcome above is disclosed by slog alone, because a driver has
// no run-warnings channel to write to (the same absence runs_dispatch_mounts.go
// names at its own member-facing seam). The word the ADMIN reads is carried
// separately, on Capabilities.EphemeralDiskEnforcement.
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
		// Reachable for overlay2 ONLY: btrfs and zfs take the opt AND always
		// enforce it, so they are already answered by the case above. The message
		// names overlay2 because nothing else can arrive here.
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

// storageDriverSupportsQuota reports, best-effort from `docker info`, whether
// the daemon's storage driver can ENFORCE a per-container `size` quota.
//
//   - overlay2 honors the size storage-opt on xfs ONLY. That is the driver's
//     contract, not a Wardyn policy: Docker's CLI reference states the size
//     option "is only available if the backing filesystem is xfs and mounted
//     with the pquota mount option", and moby's overlay2 driver sets its
//     projectQuotaSupported flag only when the backing filesystem is xfs. ext4
//     is NOT supported by that driver's size option — significant because
//     overlay2 over ext4 is the default on Docker Desktop / WSL2 and on stock
//     Ubuntu/Debian. (moby is not vendored here — the client is — so this
//     states the driver's contract rather than citing a local line.)
//   - btrfs and zfs support the size opt natively (subvolume/dataset quotas).
//   - every other driver (vfs, devicemapper on loopback, fuse-overlayfs, ...)
//     cannot, so we report false and the caller warns + runs uncapped.
//
// `docker info` reports the backing filesystem but never the pquota MOUNT
// OPTION, so an xfs answer here is necessary and not sufficient: an xfs data-root
// mounted without pquota still fails the create, which is the deliberate
// fail-closed posture applyDiskQuota describes. This predicate is the "can this
// host enforce it" question alone; storageDriverTakesSizeOpt is the separate
// "will the daemon even look at a size opt" question, and applyDiskQuota uses
// both.
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

// storageDriverTakesSizeOpt reports whether the daemon's storage driver accepts
// a `size` storage-opt AT ALL — a strictly wider set than the drivers that can
// enforce one on a given host. The gap between the two is exactly overlay2 on a
// non-xfs backing filesystem, and it is the branch that fails the run closed at
// create instead of degrading it to uncapped: the driver takes the option, so
// the daemon (which alone knows whether the backing filesystem carries project
// quotas) is the authority on whether the cap can be honoured.
func storageDriverTakesSizeOpt(driver string) bool {
	switch driver {
	case "overlay2", "btrfs", "zfs":
		return true
	default:
		return false
	}
}

// driverStatusValue returns the value for key in `docker info`'s DriverStatus
// (a list of [key, value] pairs), or "" when absent. Key match is
// case-insensitive (daemons report "Backing Filesystem").
func driverStatusValue(info system.Info, key string) string {
	for _, kv := range info.DriverStatus {
		if strings.EqualFold(kv[0], key) {
			return kv[1]
		}
	}
	return ""
}

// hostSupportsSELinux reports whether the Docker daemon advertises SELinux in
// its `docker info` SecurityOptions (entry "name=selinux"), i.e. the daemon
// applies SELinux labels to containers. Used to gate the runsc `label=disable`
// opt so gVisor containers start on enforcing hosts — mirrors
// hostSupportsAppArmor's daemon-advertised gating.
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

// hostSupportsAppArmor reports whether the Docker daemon advertises AppArmor in
// its `docker info` SecurityOptions (entries are comma-separated key=val lists,
// e.g. "name=apparmor" or "name=seccomp,profile=builtin"). Used to gate the CC1
// apparmor=docker-default pin so Wardyn never passes an option the host rejects.
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

// proxyResources is the wardyn-proxy sidecar's cgroup envelope: a tight PID cap
// (fork-bomb guard) and a modest memory cap (MemorySwap pinned so swap cannot
// silently double it). The proxy only relays HTTP, so this leaves ample
// headroom while bounding a compromised proxy. CPU is left unconstrained — the
// proxy is on the latency path and is already PID/memory bounded.
func proxyResources() container.Resources {
	memBytes := proxyMemoryMiB * 1024 * 1024
	pids := proxyPidsLimit
	return container.Resources{
		Memory:     memBytes,
		MemorySwap: memBytes,
		PidsLimit:  &pids,
	}
}

// resourcesFromSpec converts Wardyn resource caps to Docker cgroup limits,
// applying conservative platform defaults for any zero field so the returned
// Resources ALWAYS carries a CPU, memory and PID cap — every sandbox is capped
// even when policy sets nothing. A non-zero spec value always overrides its
// default.
//
//   - CPUMillis -> NanoCPUs (1000 millis == 1 CPU == 1e9 nanos).
//   - MemoryMiB -> Memory (bytes), AND MemorySwap pinned EQUAL to Memory so
//     Docker does not silently allow ~2x the cap via swap (an unset MemorySwap
//     defaults to twice Memory). The agent gets the memory cap it was given,
//     not double it.
//   - PidsLimit set UNCONDITIONALLY (the fork-bomb guard): a non-nil *int64 so
//     a fork bomb cannot exhaust the host PID space.
//
// DiskMiB is handled separately via StorageOpt (applyDiskQuota) because it is a
// HostConfig field, not a cgroup Resources field, and is backend-gated.
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
		NanoCPUs: cpuMillis * 1_000_000, // millis -> nanos
		Memory:   memBytes,
		// Pin swap to the memory cap: without this Docker defaults MemorySwap to
		// 2*Memory, silently doubling the effective cap via swap.
		MemorySwap: memBytes,
		PidsLimit:  &pidsLimit,
	}
}
