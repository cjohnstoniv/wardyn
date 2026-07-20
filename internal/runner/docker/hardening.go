// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Conservative platform resource defaults, applied by resourcesFromSpec when a
// runner.Resources field is zero. They exist so EVERY agent sandbox is capped
// even when policy sets nothing: without them one runaway or prompt-injected
// agent can OOM-kill the host, fork-bomb the host PID space, or fill host
// storage and take sibling runs down with it (the basic multi-tenant safety
// controls — see types.ResourceLimits). A policy value always overrides.
const (
	defaultCPUMillis int64 = 2000 // 2 vCPU
	defaultMemoryMiB int64 = 4096 // 4 GiB hard memory cap
	defaultPidsLimit int64 = 512  // max processes/threads (fork-bomb guard)
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

// dangerousKataAnnotations are the two Kata hypervisor-config annotations that
// let a workload override its OWN microVM's virtio-fs / kernel configuration —
// the knobs behind CVE-2026-44210/-47243 (a permissive virtio_fs_extra_args
// cache mode, or a permissive kernel_params, lets a compromised Kata guest
// reach host-root via virtiofsd).
//
// Wardyn ships NO pass-through for either today: runner.SandboxSpec,
// types.RunPolicySpec, and every composer proposal field were audited and none
// is annotation-shaped, so the only place either could ever reach a launched
// container is container.HostConfig.Annotations — moby's daemon copies
// EXACTLY that field (never Config.Labels/wardynLabels) into the OCI runtime
// spec's Annotations (daemon/oci_linux.go: coci.WithAnnotations(c.HostConfig.
// Annotations)) — and hardenedHostConfig below never sets it, so it is always
// the Go zero value (nil) on every launch. This denylist + the strip call in
// hardenedHostConfig is defense-in-depth against a FUTURE Annotations source:
// it is enforced at the one chokepoint every launched container's HostConfig
// passes through (driver.go's docker-exec create AND the exec-less/krun
// pending-agent path both call hardenedHostConfig).
var dangerousKataAnnotations = []string{
	"io.katacontainers.config.hypervisor.virtio_fs_extra_args",
	"io.katacontainers.config.hypervisor.kernel_params",
}

// stripDangerousKataAnnotations deletes the dangerousKataAnnotations keys from
// ann and returns it. Nil-safe: delete on a nil map is a no-op, so calling this
// unconditionally (as hardenedHostConfig does) never allocates or panics.
func stripDangerousKataAnnotations(ann map[string]string) map[string]string {
	for _, k := range dangerousKataAnnotations {
		delete(ann, k)
	}
	return ann
}

// cc3Runtimes are the runtime families that deliver the Vault tier's hardware
// (KVM) VM isolation. CC3 is defined by the GUARANTEE — a real per-sandbox VM
// boundary — not one product. Both qualify:
//   - kata*: Kata Containers (mature; ships its OWN containerd shim-v2, which must
//     track containerd's protocol and can lag a bleeding-edge daemon).
//   - krun:  crun built with libkrun — a KVM microVM delivered as a plain OCI
//     RUNTIME binary, invoked through containerd's standard runc shim exactly like
//     runsc (CC2). That plug-in shape sidesteps the shim-version coupling that
//     makes Kata brittle against a very new containerd.
//
// Probed in this order; the first installed one wins. NOTE: bare "crun" (no
// libkrun) is a shared-kernel runtime and is deliberately NOT accepted for CC3.
var cc3Runtimes = []string{runtimeKata, runtimeKrun}

// knownNonVaultRuntimes are runtime families we POSITIVELY know do not boot a
// per-sandbox KVM VM: runc/crun/sysbox share the host kernel, and runsc (gVisor)
// is a userspace-kernel sandbox (the Wall/CC2 tier). An operator CC3 pin naming
// one of these is a silent downgrade and is refused even under an explicit pin —
// unlike an unrecognized runtime name, which we let the operator vouch for.
var knownNonVaultRuntimes = []string{"runc", "crun", runtimeSysbox, runtimeRunsc}

// runtimeSupportsExec reports whether the OCI runtime can enter a running
// container via `docker exec`. Every runtime we use can EXCEPT krun/libkrun: a
// libkrun microVM has no in-guest exec agent (crun's krun handler returns "the
// handler does not support exec"), so its agent workload must run as the
// container's MAIN process instead of being exec'd into a keep-alive container.
// Kata (its own in-VM agent), runsc, and runc all support exec.
func runtimeSupportsExec(runtimeName string) bool {
	return !strings.HasPrefix(runtimeName, runtimeKrun)
}

// isKnownNonVaultRuntime reports whether name is a runtime we can positively
// classify as delivering less than a VM boundary. Prefix match (like pickRuntime)
// so "runc"/"crun-foo"/"sysbox-runc"/"runsc-*" are all caught. Note "krun" is NOT
// matched by the "crun" family (different leading byte), so libkrun stays eligible.
func isKnownNonVaultRuntime(name string) bool {
	for _, fam := range knownNonVaultRuntimes {
		if strings.HasPrefix(name, fam) {
			return true
		}
	}
	return false
}

// proxyListenPort is the port wardyn-proxy listens on inside the per-run
// internal network. The agent's ONLY reachable address.
const proxyListenPort = 3128

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

// verifyCapsEnforced fails closed when the Docker daemon reports it DISCARDED a
// requested CPU / memory / pids limit — the AUTHORITATIVE post-create signal,
// read from the ContainerCreate response. On a cgroup-v1 host under rootless
// Docker (or any host where the cpu/memory/pids controllers aren't delegated to
// the runtime user), the daemon silently drops the limit and appends a
// "…Limitation discarded" warning to the create response, leaving an untrusted
// sandbox effectively uncapped (a fork bomb or memory hog could take out the host).
//
// This replaces a pre-flight `docker info` capability check: `docker info`'s
// MemoryLimit/PidsLimit/CPUCfsQuota booleans are NOT reliable on Podman's
// Docker-compat API (it under-reports CpuCfsQuota=false even when the quota
// binds), which would false-positive fail-closed. The daemon's own create-time
// discard warning is authoritative on BOTH engines: Moby emits it when it can't
// enforce a limit; a daemon that actually applied the caps (Podman, or a healthy
// Moby) returns no such warning (verified). Mirrors classToRuntime's fail-closed
// contract (invariant 5): refuse rather than run a workload without its guardrails.
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
	// Wrapped as adjacent string literals purely to keep the SOURCE line under the
	// lll cap — the concatenated message is byte-identical to the reader.
	return fmt.Errorf("the Docker daemon discarded a requested resource limit — an untrusted sandbox would run without it: %s. "+
		"On cgroup v2, delegate the controllers to the runtime user (systemd unit: Delegate=yes; rootless Docker: enable cgroup v2 delegation per the rootless docs). "+
		"Set WARDYN_ALLOW_UNENFORCEABLE_CAPS=1 to override on a TRUSTED host: %w",
		strings.Join(discarded, "; "), errCapsUnenforceable)
}

// resolveRuntime is classToRuntime with operator overrides applied: it is the
// substrate-selection seam for CC3 (and CC2). An override pins the EXACT runtime
// family a class must use (still probed against `docker info` and FAIL CLOSED
// when absent — never downgrade); no override reproduces classToRuntime's
// built-in default mapping byte-for-byte. overrides may be nil.
func resolveRuntime(class types.ConfinementClass, info system.Info, overrides map[types.ConfinementClass]string) (runtimeName string, needsRuntime bool, err error) {
	want, pinned := overrides[class]
	if !pinned || want == "" {
		return classToRuntime(class, info) // default path, unchanged
	}
	// Isolation floor: a pin must not silently downgrade a class below the
	// isolation family the control plane advertises/gates it as. CC2 must run
	// gVisor (runsc*), CC3 must run Kata (kata*); e.g. WARDYN_CONFINEMENT_MAP=
	// "CC2=runc" would run a shared-kernel runtime while the run is gated as
	// CC2 (Wall) — a silent downgrade. Reject a weaker pin fail-closed, same
	// error class as an absent runtime, so "the tier you asked for is the tier
	// you got" holds. CC1 is the weakest tier: any pin (including a STRONGER one
	// like sysbox) is legitimate, so it is left unrestricted.
	switch class {
	case types.CC2:
		if !strings.HasPrefix(want, runtimeRunsc) {
			return "", false, fmt.Errorf("the Wall tier (CC2) pins runtime %q, which does not deliver gVisor (%s) isolation; refusing to downgrade: %w", want, runtimeRunsc, errRuntimeUnavailable)
		}
	case types.CC3:
		// The built-in auto-mapping (classToRuntime, no pin) stays limited to the
		// known-VM allowlist (cc3Runtimes: kata, krun) — Wardyn only auto-advertises
		// CC3 for runtimes it KNOWS boot a VM (invariant 5). An EXPLICIT operator pin
		// may instead name ANY registered runtime the operator vouches delivers a VM
		// boundary (bring-your-own microVM: firecracker, cloud-hypervisor, a custom
		// shim...), so the allowlist is not required here — the pluggable seam. We
		// still refuse runtimes we POSITIVELY know deliver less than a VM (shared-kernel
		// runc/crun/sysbox, or gVisor/runsc = the CC2 tier): pinning one of those at
		// Vault is a silent downgrade, not a bring-your-own choice.
		if isKnownNonVaultRuntime(want) {
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
	names := make([]string, 0, len(info.Runtimes))
	for name := range info.Runtimes {
		names = append(names, name)
	}
	sort.Strings(names)
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
func capabilitiesForWith(info system.Info, overrides map[types.ConfinementClass]string) runner.Capabilities {
	classes := []types.ConfinementClass{}
	resolved := map[types.ConfinementClass]string{}
	// CC1 is the floor, but an operator CC1 override (e.g. a stronger sysbox pin)
	// can still be unhonorable when that runtime is absent. Treat its error EXACTLY
	// like CC2/CC3: never advertise a class whose (possibly pinned) runtime can't
	// be enforced, so /healthz reflects the substrate the host can actually deliver
	// (invariant 5 — fail closed, never overclaim). With no override this is
	// byte-for-byte the old CC1 path (runtimeOrRunc labels the daemon default).
	if rt, _, err := resolveRuntime(types.CC1, info, overrides); err == nil {
		classes = append(classes, types.CC1)
		resolved[types.CC1] = "oci/" + runtimeOrRunc(rt)
	}
	if rt, _, err := resolveRuntime(types.CC2, info, overrides); err == nil {
		classes = append(classes, types.CC2)
		resolved[types.CC2] = "oci/" + rt
	}
	if rt, _, err := resolveRuntime(types.CC3, info, overrides); err == nil {
		classes = append(classes, types.CC3)
		resolved[types.CC3] = "oci/" + rt
	}
	return runner.Capabilities{
		Driver:             driverName,
		ConfinementClasses: classes, // strongest last
		Resolved:           resolved,
		// L0 is structural here: NetworkMode "none" + internal-only per-run
		// network means the agent has no default route and one egress path.
		StructuralEgress: true,
		// L1 nftables default-deny is v0.5 — be honest, do not claim it.
		NetworkPolicy: false,
		WarmPools:     false,
		// wardyn-rec sidecar is supported by Exec.
		SessionRecording: true,
	}
}

// hardenedHostConfig builds the agent container's HostConfig with Wardyn's
// non-negotiable hardening. networkMode is set by the caller (always "none"
// at create time for L0). runtimeName is "" for the daemon default (CC1) or
// the resolved runtime for CC2/CC3. info is the probed `docker info`, used to
// gate the AppArmor pin on actual host support.
//
// Constraints encoded here:
//   - CapDrop ALL: the agent needs no Linux capabilities.
//   - no-new-privileges: blocks setuid escalation.
//   - seccomp: we NEVER pass "seccomp=unconfined", so Docker's RuntimeDefault
//     seccomp profile stays in force. RuntimeDefault is the claimed and
//     enforced baseline; we do not ship a custom profile.
//   - AppArmor: explicitly pinned to docker-default on the CC1 (runc) path when
//     the host supports AppArmor. CC2 (gVisor/runsc) and CC3 (Kata) mediate
//     syscalls around the host LSMs, so forcing apparmor there is at best a
//     no-op and can error — it is omitted for non-runc runtimes (invariant 5).
//   - tmpfs /tmp: writable scratch without a writable rootfs requirement.
//   - Resources: hard caps from the spec, with conservative platform defaults
//     applied for any zero field so EVERY sandbox is capped (CPU, memory with
//     MemorySwap pinned so the cap is not silently doubled via swap, and a
//     PidsLimit fork-bomb guard set unconditionally).
//   - StorageOpt: a per-container writable-disk quota when the spec requests one
//     (DiskMiB>0) AND the daemon storage driver can enforce it; otherwise a
//     clear warning is logged and the run proceeds uncapped (never hard-broken).
//   - userns: left to daemon config (daemon-wide userns-remap); documented,
//     not forced per-container, because per-container userns conflicts with
//     some runtimes and the daemon setting is the supported knob.
func hardenedHostConfig(networkMode string, runtimeName string, res runner.Resources, info system.Info) *container.HostConfig {
	secOpt := []string{"no-new-privileges"}
	// Pin AppArmor explicitly for CC1 (default runc runtime) when the host
	// advertises AppArmor support. Gating on support avoids Docker rejecting
	// the option on hosts without AppArmor (many non-Ubuntu kernels, WSL2).
	if runtimeName == "" && hostSupportsAppArmor(info) {
		secOpt = append(secOpt, "apparmor=docker-default")
	}
	// gVisor (runsc) has no SELinux integration: when the daemon applies SELinux
	// labels (selinux-enabled), runsc refuses to start ("SELinux is not
	// supported: ...container_t..."). Disable labeling for the runsc path ONLY —
	// gVisor's own sandbox is the isolation boundary there, so dropping the host
	// SELinux label is acceptable. NEVER do this on the runc (CC1) path, where
	// SELinux labeling is a real defense we keep; Kata (CC3) is unaffected. We
	// key off the daemon's advertised SELinux (not the local host) so it is
	// correct even when wardynd talks to a remote/VM dockerd.
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

// applyDiskQuota sets the per-container writable-storage cap (StorageOpt "size")
// when the spec requests one (DiskMiB>0) AND the daemon storage driver can
// actually enforce a per-container quota. Detection is best-effort from
// `docker info` (storageDriverSupportsQuota). When a cap is requested but the
// backend cannot enforce it we do NOT hard-break the run: we log a clear,
// visible warning and proceed uncapped — mirroring the codebase's "visible
// blindness" posture (never silently claim a control we cannot enforce, but do
// not punish a run for an operator's storage-driver choice). A DiskMiB of 0 is
// the common case and never warns.
func applyDiskQuota(hc *container.HostConfig, res runner.Resources, info system.Info) {
	if res.DiskMiB <= 0 {
		return
	}
	if !storageDriverSupportsQuota(info) {
		slog.Warn("wardyn/docker: disk cap requested but the storage driver does not support a per-container size quota (need overlay2 with project quota, or btrfs/zfs); running WITHOUT a disk cap",
			slog.Int64("disk_mib", res.DiskMiB),
			slog.String("storage_driver", info.Driver),
		)
		return
	}
	if hc.StorageOpt == nil {
		hc.StorageOpt = map[string]string{}
	}
	hc.StorageOpt["size"] = fmt.Sprintf("%dm", res.DiskMiB)
}

// storageDriverSupportsQuota reports, best-effort from `docker info`, whether
// the daemon's storage driver can enforce a per-container `size` quota.
//   - overlay2 honors the size storage-opt ONLY on a project-quota-capable
//     backing filesystem (xfs mounted with pquota, or ext4 with the project
//     feature). `docker info` cannot fully confirm the pquota mount option, so
//     we approve overlay2 on an xfs/ext4 backing fs and rely on the daemon to
//     reject a genuinely-unquota'd backend at create time (the run then fails
//     closed at ContainerCreate rather than running silently uncapped).
//   - btrfs and zfs support the size opt natively (subvolume/dataset quotas).
//   - every other driver (vfs, devicemapper on loopback, fuse-overlayfs, ...)
//     cannot, so we report false and the caller warns + runs uncapped.
func storageDriverSupportsQuota(info system.Info) bool {
	switch info.Driver {
	case "overlay2":
		backing := strings.ToLower(driverStatusValue(info, "Backing Filesystem"))
		return backing == "xfs" || backing == "extfs" || backing == "ext4"
	case "btrfs", "zfs":
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

// DiskMiB is handled separately via StorageOpt (applyDiskQuota) because it is a
// HostConfig field, not a cgroup Resources field, and is backend-gated.
func resourcesFromSpec(res runner.Resources) container.Resources {
	cpuMillis := res.CPUMillis
	if cpuMillis <= 0 {
		cpuMillis = defaultCPUMillis
	}
	memMiB := res.MemoryMiB
	if memMiB <= 0 {
		memMiB = defaultMemoryMiB
	}
	pids := res.PidsLimit
	if pids <= 0 {
		pids = defaultPidsLimit
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
