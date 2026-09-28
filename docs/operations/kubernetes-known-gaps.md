> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Kubernetes: known gaps

The `k8s` runner substrate (`deploy/helm/wardyn`, `k8s.enabled=true`,
`internal/runner/k8s`) is a **separate, independent confinement substrate** from
the Docker Compose path (L1/NetworkPolicy-backed vs. Compose's L0 structural one)
— most of this document applies to both, but the list below is what the k8s
substrate does NOT do yet. Each item is a real limitation checked against the
driver, not a guess:

- ⛔ **No BYOI or devcontainer builds.** A `wardyn-byoi/`-prefixed image ref is
  refused before any pod is created — ephemeral containers cannot honor the
  selftest-then-task double-exec BYOI needs (`internal/runner/k8s/errors.go`'s
  `errBYOIUnsupported`, `internal/runner/k8s/exec.go`). `WARDYN_ENVBUILD`
  devcontainer builds are Docker-only for the same reason, unaffected by
  `k8s.enabled`.
- ⛔ **No `local_dir` / host-path workspace mounts.** A policy with any
  `WorkspaceMounts` entry fails the run closed with a clear error
  (`internal/runner/k8s/sandbox.go`'s `errMountsUnsupported`) — a k8s pod has no
  path back to an arbitrary directory on wardynd's own host. Git-clone workspaces
  (`WorkspaceRepos`) are unaffected; only a *local directory* source is refused.
- ⛔ **No `~/.aws` / `~/.claude` host staging.** The same `errMountsUnsupported`
  refusal covers the RESIDENT-COPY credential path — there is no host filesystem
  to stage from. Use proxy-side injection instead: managed-subscription OAuth
  injection and the Bedrock AWS SSO exchange are substrate-agnostic (they happen
  at `wardyn-proxy`), so they work unchanged on k8s.
- 🟡 **No in-sandbox DNS.** Every sandbox pod is `DNSPolicy: DNSNone` with a
  single nameserver, `127.0.0.1` — nothing listens there, so a DNS query fails
  FAST (connection refused) rather than hanging out a real timeout
  (`internal/runner/k8s/sandbox.go`). Only the pinned `wardyn-proxy` sidecar
  resolves hostnames, matching the Compose substrate's proxy-only egress posture —
  parity, not a new gap, but the *mechanism* (a present-but-unreachable loopback
  resolver vs. Compose's no-resolver-at-all) is k8s-specific.
- 🟡 **No per-pod PIDs limit.** Kubernetes has no per-container "pids" resource
  the way Docker's `--pids-limit` does — a run's `ResourceLimits.PidsLimit` is
  accepted but not enforced, and wardynd logs a warning naming the run id each
  time (`internal/runner/k8s/sandbox.go`). **Recommendation**: set the node-level
  kubelet `podPidsLimit` (or your distribution's
  `SystemReserved`/`KubeReserved` PID accounting) as a cluster-wide fork-bomb
  backstop — coarser but real, and the only lever this substrate has today.
- 🟡 **`DiskMiB` is enforced by EVICTION, and since 0.7.5 (further narrowed by #164 in 0.8) it
  reaches an AUTONOMOUS run's `/tmp`, workdir and toolchain-cache writes — a narrowing, not a
  close.** All of this describes AUTONOMOUS (task-mode)
  runs. An interactive run's agent runs in the pod's main container, whose whole writable layer —
  `$HOME` and the toolchain caches included — the kubelet has counted against `disk_mib` since
  0.7.2; there nothing is outside the cap, so size an interactive run's budget for its caches too.
  A run's `disk_mib` is the agent container's
  `resources.limits[ephemeral-storage]` and the `sizeLimit` of the three `emptyDir` volumes mounted
  on it (`internal/runner/k8s/naming.go`'s `ephemeralScratchVolumes`): `wardyn-tmp` at `/tmp`,
  `wardyn-work` at `/home/agent/work`, and `wardyn-cache` at `/home/agent/.cache`. The ephemeral
  container `Exec` attaches for the agent
  process (`internal/runner/k8s/exec.go`) copies the main container's mounts verbatim, so writes
  to those paths land in volumes the kubelet meters as the pod's local ephemeral storage.
  Before 0.7.5 they landed on the ephemeral container's own writable layer, which the kubelet
  meters not at all: the limit evicted writes by the pod's idle main container only, and the
  conformance case `EphemeralDiskLimit/OverTheLimitTheRunIsEvicted` was red from 0.7.2 for exactly
  that reason (0.7.4 disclosed it; it now runs for all fill targets and passes). The three
  `sizeLimit`s are one budget, not three: `emptyDir` usage counts toward the pod's
  `ephemeral-storage` total as well, so filling every volume partway still evicts. **Upgrade
  note:** an operator's `default_disk_mib` or policy `disk_mib` did not bind an autonomous k8s run
  before 0.7.5 and does now — size it for the clone, installs and toolchain caches before
  upgrading, or a run that used to finish will be evicted with its in-flight work lost. **What is
  still OUTSIDE the cap:**
  everything the agent writes beyond those three paths — the rest of `$HOME` (`/home/agent/go` —
  GOPATH itself is unmoved, so the installed tool binaries under its `bin/` stay reachable; only
  `GOMODCACHE` moved under the cache volume — and `~/.cache/pip`) and the
  dotfiles (`~/.wardyn`, `~/.ssh`, `~/.claude`); `/opt/rust`; and any authored `workspace_repos`
  or ephemeral-source target outside `/home/agent/work`, since an authored target may legally sit
  at `/work`, `/workspace` or elsewhere under `/home/agent` (`internal/runner/mount.go`'s allowed
  target prefixes). Nothing is mounted at `/home/agent` itself, because a volume there would
  shadow each image's baked `.bashrc`, swallow the reserved drive target `/home/agent/drive`, and
  hide the read-only `~/.claude` bind the subscription path mounts. **The cache volume starts
  cold:** an `emptyDir` at `/home/agent/.cache` shadows the full image's pre-created
  `/home/agent/.cache/go-build` (`deploy/images/full/Dockerfile`), so the Go build cache is
  rebuilt from empty. The mount is writable without `FSGroup`: the kubelet creates an `emptyDir`
  root-owned but `0777`, the same mode `/tmp` and `/home/agent/work` have been written through by
  the uid-1000 agent since 0.7.5; the conformance "Cache" fill target writes it against a real
  cluster. **What the proof
  does not cover:** the kind conformance evidence is from the busybox conformance-agent image on
  runc (CC1), and `emptyDir` metering of ephemeral-container writes is unmeasured under gVisor and
  Kata. The live kind SSO walk separately exercises a real `agent-run` boot — the aws-sso sign-in
  sandbox and a real claude-code run — on the `emptyDir`-backed `/tmp` and `/home/agent/work`, on
  runc; that walk is manual and self-skipping, not CI (see `docs/TEST-GAPS.md`). What remains a gap about the
  mechanism, precisely: (i) enforcement is by **eviction, not a
  quota** — the kubelet kills the POD once it exceeds the limit, in-flight work
  is lost, and the agent process never sees `ENOSPC`; it gets no chance to
  flush or fail gracefully. The completion watcher and restart reconciler recognize a terminal
  pod even if Kubernetes never publishes the agent container's exit status. An unknown agent
  exit is a failed run, not a successful task; a recorded agent exit keeps its actual result.
  Normal run finalization then revokes credentials and attempts to reclaim the run's siblings —
  the proxy pod still running with
  its resolved upstream credentials, and the per-run Secret holding the run
  token, the MITM CA key and any injected git token. Failed cleanup can be retried by the
  control plane's
  orphan sweep (`internal/api/reconcile.go`, implemented on this substrate by
  `internal/runner/k8s/lifecycle.go`'s `SweepOrphanedSandboxes`), on the next
  boot and on its cadence after. Sweep candidates must be past `undispatchedGrace`;
  cleanup is best-effort and can take longer when the Kubernetes API is unavailable.
  A user drive's claim is never touched by it. Two narrowings of that window since 0.7.3: an
  ordinary
  stop/kill of a run whose agent pod is ALREADY gone now reclaims the siblings
  itself (the sandbox ref is the agent pod name, so the run id needs no live pod
  to read it from), and the sweep lists the per-run Secret and both
  NetworkPolicies as well as the pods — so a run whose agent AND proxy pods are
  both gone (a deleted node takes them together) is still reachable rather than
  stranded forever; (ii) the kubelet measures periodically (~10s
  housekeeping), so a fast enough burst can overshoot the limit before the
  next tick catches it; (iii) with neither a policy-authored `disk_mib` nor a
  `default_disk_mib` on the deployment's storage provider, a run's `disk_mib`
  is simply absent — BOTH `ephemeral-storage` keys stay off the pod — and
  node-level eviction — a cluster-wide, not per-run, bound — is the only thing
  holding the line, same shape as the PIDs gap above. Separately, whenever a
  limit IS set, the agent container also carries an explicit `ephemeral-storage`
  request — 256Mi, or the limit itself when the limit is smaller — so the
  scheduler never inherits the org's whole ceiling as a request (Kubernetes
  copies an unset request from the limit); on a node whose allocatable
  ephemeral storage is already short of that request, a run that sets a cap
  can now sit `Pending` on "Insufficient ephemeral-storage" where it could have
  scheduled before; that joins this gap list too.

  User drives introduced the same `enforcement` vocabulary —
  `types.StorageEnforcement`, one of `filesystem` (a quota binds it),
  `eviction` (the kubelet kills the pod), `request` (a volume request; the
  storage class decides), `external` (somebody else's quota binds it) or
  `none` (nothing binds it) — and one frozen sentence the console and the docs
  both render verbatim:

  > Wardyn never enforces a drive's size itself. On Kubernetes the size is the volume request and the storage class decides whether it binds — block disks do, network-share provisioners do not. On Docker a managed drive has no byte cap, the same gap disk_mib has. A share is bounded by its own quota. The size you see is the allocation, not a guarantee.

  A drive is `request` on a managed claim and `external` on a share;
  `disk_mib` is `filesystem` on a Docker host whose storage driver can enforce
  a per-container quota, `none` on a Docker host whose driver cannot, and
  `eviction` here on Kubernetes. The two vocabularies **have now converged on
  the one**, rather than staying two ways of saying "accepted, not enforced".
- 🟡 **No k8s ground-truth correlator.** The Tetragon host-sensor → ground-truth
  pipeline (`cmd/wardynd/gt_rotator.go`, `wardyn-tetragon-ingest`, the
  `groundtruth` Compose profile) has no k8s-substrate equivalent — it is not
  referenced anywhere under `internal/runner/k8s`. A k8s deployment gets the
  NetworkPolicy-enforced boundary (proven live by the boot-time egress canary)
  but not the independent kernel-level corroboration Compose + Tetragon provides.
  **Where to read the canary's verdict, 0.7.3 on:** the admin setup page's
  Environment step (`GET /setup/status`, the `k8s_egress_containment` check
  below) — not the console's global header, which carried a permanent
  `NetworkPolicy: enforcing`/`Fence` chip through 0.7.2 and no longer does
  (removed as deployment-wide, boot-fixed facts that conveyed nothing after one
  read; see CHANGELOG.md). The Environment step is also where the Fence/Wall/
  Vault tier matrix for every driver lives.
- ⛔ **A pre-existing default-deny NetworkPolicy in `k8s.runsNamespace` refuses
  boot outright, with no override — unless the canary pod actually ran and could
  not connect.** The boot-time egress canary's phase A applies no NetworkPolicy of
  its own; it only proves the cluster is reachable before phase B proves Wardyn's
  deny-all rule takes effect. If the namespace already carries a default-deny
  policy from something else, phase A's pod is blocked too and wardynd refuses to
  boot with an INDETERMINATE verdict indistinguishable from a genuinely broken
  cluster (`internal/runner/k8s/canary.go`). **Fix**: give `k8s.runsNamespace` a
  namespace with no ambient default-deny, or exempt Wardyn's pods from *that
  policy's own* `podSelector` (a `matchExpressions` entry with `key:
  wardyn.managed`, `operator: NotIn`, `values: ["true"]`). **Do not instead add a
  separate allow policy for `wardyn.managed=true`**: NetworkPolicy allows are
  additive and both the agent and proxy pods carry that label, so such a policy
  widens every sandbox pod's egress past Wardyn's per-run deny+proxy-only policy
  (`internal/runner/k8s/sandbox.go`) and flips the canary's phase B to "CNI does
  not enforce" — which in turn invites `WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=1` and
  fully unconfined runs. If the ambient default-deny is expected — a managed,
  multi-tenant cluster where a platform team applies the baseline — and the canary
  pod DID reach Running with its own connect exiting exactly 1 (not "never reached
  Running", not any other exit code), `WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY=1`
  acknowledges that shape and boots anyway. It is never proof of enforcement
  (phase B is skipped); the setup checklist's `k8s_egress_containment` row grades
  this `warn` ("acknowledged, not proven"), never `ok`. The exempt-the-podSelector
  fix is still the way to get REAL proof.
- 🟡 **`replicas` stays 1 on k8s exactly as everywhere else** — see
  [One replica, by construction](../OPERATIONS.md#one-replica-by-construction); the masking
  registry is still in-process, per-pod.

None of these are silent: the mount and BYOI gaps fail the run closed with a named
error, the resource-cap gaps log a warning naming exactly what is unenforced.
Closing any of them is unstarted work, not a documented-but-planned near-term item
— see ROADMAP.md for what is actually queued.
