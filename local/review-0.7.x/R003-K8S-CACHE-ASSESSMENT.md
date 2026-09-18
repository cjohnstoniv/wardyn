# R076-003: Kubernetes cache accounting assessment

Read-only review, 2026-09-18. Reviewed product tip `cd179c617acee6dd14ba6bc273db44569310355b`; no cache paths, images, pod specifications, policy, cluster resources or budgets changed. No new live cache-accounting experiment was performed.

## Conclusion

The autonomous Kubernetes cache-accounting gap is real in the supported code path and remains open. It is suitable for a separately scoped 0.7.x investigation, not an untested one-line change in the current batch. The other 0.7.6 owner explicitly deferred this item to 0.7.7. There is no basis here to call either prospective implementation compatibility-neutral or to claim comprehensive disk containment.

`ephemeralScratchVolumes` creates disk-backed emptyDirs only at `/tmp` and `/home/agent/work` when `DiskMiB > 0`. `resourceRequirements` sets the main container's ephemeral-storage limit and a small explicit request; `Exec` copies the main container's environment and whole-volume mounts into the autonomous ephemeral container. The repository's existing live evidence and Operations residual describe its unmetered ephemeral-container writable layer. Go temporary/build/module caches and npm's default home cache are outside the two mounts. This is not a claim that an npm install writes *nothing* metered: project `node_modules` under the default workdir is metered, while the home cache is not.

Interactive runs use the main container and are a different accounting case: their writable layer is already within the existing kubelet accounting described by Wardyn. Disk limits remain periodic eviction, not synchronous write quotas. Main-container writable layers/logs and other local ephemeral volume usage also count; saying the budget covers literally nothing except the two paths would overstate the gap.

## Code and supported configuration

| Surface | Evidence at the reviewed tip | Consequence |
| --- | --- | --- |
| Scratch and budget | `internal/runner/k8s/naming.go`, `ephemeralScratchVolumes` / `resourceRequirements` | Two separate whole-volume mounts; `DiskMiB <= 0` adds neither volumes nor a disk limit. Each size limit is not an extra independent pod budget. |
| Pod assembly | `internal/runner/k8s/sandbox.go`, `CreateSandbox`; `internal/runner/k8s/exec.go`, `Exec` | Mounts append to the main container and copy verbatim to the task ephemeral container. Kubernetes `SubPath` is not a usable split-cache design here. |
| Toolchain environment | `internal/api/runs_dispatch_mounts.go`, `buildBaseSandboxEnv`; `internal/api/toolchain_env_test.go` | Go needs (or unknown needs) set `GOTMPDIR=/home/agent/.gotmp` and `GOCACHE=/home/agent/.cache/go-build`. Known non-Go workspaces do not receive these variables. This helper is shared with Docker. |
| Image defaults | `deploy/images/full/Dockerfile`, toolchain installation and `/etc/profile.d/toolchains.sh` | `GOPATH=/home/agent/go`; `/home/agent/go/bin` is on PATH. The login profile unconditionally re-exports GOPATH, GOCACHE and GOTMPDIR, undoing a naive environment-only relocation in login-shell task mode. |
| Entry-point initialization | `deploy/images/common/agent-run-lib.sh`, `make_toolchain_dirs` | Creates the selected GOTMPDIR; does not itself relocate caches or establish storage containment. |
| Runtime configuration | `internal/runner/k8s/driver.go`, `Config`; `CreateSandbox`; `runner.SandboxSpec` | No supported cache-path/cache-volume knob. K8s rejects arbitrary `spec.Mounts`. The internal Env/ExtraEnv maps are not a public operator cache API; daemon extra environment is not automatically sandbox environment. |

Stock defaults imply the Go module cache under `/home/agent/go/pkg/mod`, Go build cache under `/home/agent/.cache/go-build`, temporary Go output under `/home/agent/.gotmp`, and npm cache under `~/.npm`. Go/npm coverage would still not cover pnpm/Yarn/pip/Maven/Rust caches, credential/config dotfiles, or arbitrary authored targets outside the metered mounts. `/opt/rust` also contains installed toolchains, not just disposable downloads.

## Compatibility choices to resolve before implementation

1. **Separate emptyDirs on default cache leaf directories.** Keeps existing paths and old image login profiles working, and could stay within the K8s driver. Candidate leaves are `.gotmp`, `.cache/go-build`, `go/pkg/mod`, and `.npm`; not all of HOME, GOPATH, `.cache`, or `/opt/rust`. It hides any image-baked content at each leaf, including prewarmed offline module caches, and misses customized cache paths. Newly counted bytes can evict previously successful runs. A single third volume mounted at several existing paths aliases its root contents; distinct directories cannot be obtained using forbidden ephemeral-container SubPath mounts.
2. **K8s-only cache environment defaults under metered scratch.** Can avoid masking image directories, but must account for the full image's unconditional login-profile exports, requirements-driven Go injection, explicit/custom image settings, and creation/permissions. Use `GOMODCACHE` rather than casually changing GOPATH and losing installed tool binaries. Relocating into the workdir also exposes cache contents to workspace scans. A profile fix requires image build/rollout compatibility proof. Never change the shared Docker Go-temp defaults blindly: those avoid Docker's noexec `/tmp` behavior.

Neither choice stops an agent selecting a different cache path or writing elsewhere on its writable layer. No new read-only-root-filesystem mandate, agent-container architecture change, API knob, or budget-policy migration is proposed in this review.

## Required acceptance before claiming a fix

- Unit checks for zero/positive disk budgets, default and custom environment, autonomous versus interactive pod shape, whole-volume mounts and exact main-to-ephemeral copy behavior.
- Full-image plain and login shell checks of `go env` / npm cache configuration. Compile and execute Go tests, preserve installed tools, and test offline/prewarmed module compatibility; do not substitute a generic `dd` for these checks.
- Dedicated live Kubernetes evidence that real Go and npm cache bytes land on the intended mounts, and cache-only plus cumulative volume writes trigger the advertised eviction. Keep budget sizing and lost in-flight work explicit.
- Preserve terminal-pod completion/finalization semantics from `46dbcf6d` (already adopted by the other 0.7.6 branch): unknown exit is not success, and eviction must not leave normal finalization waiting forever.
- Separate runc evidence from gVisor/Kata claims; no such runtime parity measurement was added here. Kubernetes filesystem/accounting configuration matters, so do not claim live proof from a fake clientset.

Kubernetes primary references confirm that [ephemeral containers do not support resource requests/limits](https://kubernetes.io/docs/concepts/workloads/pods/ephemeral-containers/) and that [local ephemeral accounting includes writable layers, logs and disk-backed emptyDir volumes, with eviction-based enforcement](https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/). These general documents are not new live proof of Wardyn's specific ephemeral-container writable-layer gap; that attribution remains the repository's disclosed evidence.

## Other-owner coordination

Read-only comparison used `feature/0.7.6` at `77fb99613ac595e77f4cc7e993a06976c4812e4a` (a moving branch). At that captured tip `naming.go`, `runs_dispatch_mounts.go`, the full-image Dockerfile and common entry-point helper were unchanged from `dfa89f60`. However, `sandbox.go`, `exec.go`, `runner.go` and lifecycle code changed for starting diagnostics, reauth and terminal-pod handling. A later cache patch still needs semantic integration review even if a naming-only hunk applies cleanly.

The owner's `/home/cjohn/wt-v076/local/v076/evidence/patch-review/SELECTION.md` explicitly marks R076-003 **DEFER-0.7.7**; `D.md` contains the corresponding assessment. The plan's O-1 decision also defaults to deferral. Existing product comments/docs that still promise a 0.7.6 follow-up are stale relative to that selection; the owner has a docs addendum in progress. Do not edit their branch or duplicate that release-note work. Review campaign implementation should be coordinated as a separate selected 0.7.x patch after the compatibility choice and live proof budget are agreed.
