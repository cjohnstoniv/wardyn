# 0.9 environment build record: resolved inputs, image identity and refresh (#1516)

Status: **Draft for owner approval.** Lane F-1516a writes the design; F-1516b to F-1516e build it, and only after [Owner decisions](#13-owner-decisions) are answered.
Each workspace image build records what it actually used, and a rebuild can reproduce those inputs or name the one that changed.
A refresh replaces a kept image on purpose. Existing images, mirrors, offline hosts and user devcontainers keep working unchanged.

## 1. Scope

| In scope | Out of scope |
|---|---|
| The three workspace image lanes in `Server.resolveWorkspaceImage` ([`internal/api/workspace_run_image.go`](../../../internal/api/workspace_run_image.go)): generated, `byoi`, repo devcontainer | Per-run builds from `--image` and `--devcontainer-repo` (`Server.resolveCreateRunImage`): they keep a `run.build` row only ([§11](#11-residuals-and-threat-model-delta)) |
| One record per workspace image, its read surface, and the `run.build` audit fields | A generic build interface, a distributed cache, or a lockfile format |
| Resolve-then-pin for the generated lane; rebuild from a record | Production image pinning, the UI lockfile and release signing (the issue's own exclusions) |
| An intentional refresh, its CLI and SDK mirror | The 0.8.5 wording fix, which has shipped |

### 1.1 Changes to the planner's proposal

| Brief item | This design | Why |
|---|---|---|
| New table `workspace_image_builds` with `superseded_at` | One `jsonb` column, `workspaces.image_build`, written in the same `UPDATE` as `image_ref` | A record and the image it describes can never disagree; history lives in the append-only audit trail, not in rows the application updates |
| Optional `runner.ImageDigester` | An optional capability on the image builder, `api.ImageIdentifier` | The builder made the image and already holds the Docker client; the parity rule blesses `internal/envbuild` and no other control-plane package |
| `POST /api/v1/workspaces/{id}/image/refresh` | `POST /api/v1/workspaces/{id}/build` with body `{"refresh": true}` | The build route already carries the authorization, the repo admission gates and the single-flight claim a refresh needs |
| Resolve the installer channel before the build | Observe what the installer resolved, from a file the build writes; pin it on rebuild | No new control-plane egress, and the observation follows the build network, mirrors included |

## 2. Facts this design builds on

| Fact | Where |
|---|---|
| The generated recipe names `mcr.microsoft.com/devcontainers/base:ubuntu`, `ghcr.io/devcontainers/features/*:1` with `{}` options, and the claude-code `/stable` channel | `genBaseImage`, `genLangFeatures`, `genAgentToolInstalls` in [`internal/workspacescan/gen.go`](../../../internal/workspacescan/gen.go) |
| `GenerateDevcontainer` is pure: the same profile gives byte-identical files | [`internal/workspacescan/gen.go`](../../../internal/workspacescan/gen.go) |
| `CacheKey` is `SHA256(ProfileHash + "\|" + cacheKeySalt)`, salt `v2`; generator output is not in it | `WorkspaceProfile.CacheKey` in [`internal/workspacescan/profile.go`](../../../internal/workspacescan/profile.go) |
| Reuse needs a stored ref, a matching key and `cachedImageStillPresent`, which fails open | `Server.cachedImageStillPresent` in [`internal/api/workspace_run_image.go`](../../../internal/api/workspace_run_image.go) |
| The generated lane writes no `run.build` row on a cache hit; the other two lanes do | `Server.resolveWorkspaceImage` |
| Every build lane writes through one scoped `UPDATE` of `image_ref` and `built_profile_hash` | `PG.SetWorkspaceBuiltImage` in [`internal/store/store_workspaces.go`](../../../internal/store/store_workspaces.go) |
| `ImageBuilder` returns only an image ref; envbuilder pushes stage 1 to `WARDYN_ENVBUILD_CACHE_REPO` and finalize tags it on the host daemon | `ImageBuilder` in [`internal/api/server_deps.go`](../../../internal/api/server_deps.go), [ENVBUILD.md](../../ENVBUILD.md#two-stage-build) |
| The final image is never pushed, so it has an image ID and no registry manifest digest | `Builder.finalizeImage` in [`internal/envbuild/builder.go`](../../../internal/envbuild/builder.go) |
| A `byoi` base is pulled only when absent, so a kept tag never refreshes | `Builder.ensureImage` in [`internal/envbuild/builder.go`](../../../internal/envbuild/builder.go) |
| Wardyn has no registry-mirror setting for the build stage; kaniko inside envbuilder fetches the names the recipe uses | [ENVBUILD.md](../../ENVBUILD.md#envbuilder-environment-variables-used) |
| The image builder exists only with `-tags docker`; Kubernetes installs have none | `newEnvBuilder` in [`cmd/wardynd/envbuild_docker.go`](../../../cmd/wardynd/envbuild_docker.go) |
| `POST /build` is owner-or-admin and short-circuits on `done` | `Server.handleBuildWorkspace` in [`internal/api/workspace_build.go`](../../../internal/api/workspace_build.go) |

## 3. The record

### 3.1 Where it lives

- Column `workspaces.image_build jsonb NULL`, added by migration `<NNNN>_workspace_image_build`; the lead assigns the number at merge.
- `PG.SetWorkspaceBuiltImage` gains a record argument and writes `image_ref`, `built_profile_hash` and `image_build` in its one statement.
- The full-row `UpdateWorkspace` never names the column, so a stale snapshot cannot overwrite a record.
- `types.Workspace` carries it as `ImageBuild *ImageBuild` with `json:"-"`. The only read surface is [§3.4](#34-read-surface).

### 3.2 Shape

```json
{
  "schema": 1,
  "lane": "generated",
  "cache_key": "<sha256 hex>",
  "build_id": "<uuid>",
  "reason": "cache_miss",
  "built_at": "2026-10-10T12:00:00Z",
  "wardyn_version": "0.9.0",
  "resolved": true,
  "inputs": {
    "base": {"ref": "mcr.microsoft.com/devcontainers/base:ubuntu", "digest": "sha256:<hex>"},
    "features": [
      {"ref": "ghcr.io/devcontainers/features/go:1", "digest": "sha256:<hex>", "options": {}}
    ],
    "installers": [
      {"tool": "claude-code", "channel": "stable", "version": "<ver>", "platform": "linux-x64", "sha256": "<hex>"}
    ],
    "generator": {"salt": "v2", "files_sha256": {".devcontainer/devcontainer.json": "<hex>", ".devcontainer/Dockerfile": "<hex>"}}
  },
  "image": {"ref": "wardyn-workspace/<workspace-id>:<key12>", "id": "sha256:<hex>"}
}
```

| Field | Meaning |
|---|---|
| `schema` | `1`. A reader that meets a higher number treats the record as unrecorded |
| `lane` | `generated`, `byoi` or `repo_devcontainer` |
| `cache_key` | The key the build was keyed on: `CacheKey`, `byoiCacheKey` or `repoDevcontainerCacheKey` |
| `build_id` | The run ID or build ID the build's `run.build` row carries |
| `reason` | `cache_miss` (no image, or a different key), `image_missing` (key matched, image gone), `refresh` |
| `resolved` | `true` only when every pinnable input of the lane carries a digest or version |
| `inputs.base` | Generated: Wardyn's base. `byoi`: the operator's ref as written, plus the digest observed after the wrap |
| `inputs.features[]` | Generated lane only. `options` is what the generator wrote |
| `inputs.installers[]` | Generated lane only. `version`, `platform` and `sha256` are what the build observed ([§6.3](#63-installer-observation)) |
| `inputs.generator` | Generated lane only. `files_sha256` hashes the exact files handed to the builder |
| `inputs.repo` | Repo-devcontainer lane only: `{url, ref}` as passed to `BuildDevcontainer` |
| `image.ref` / `image.id` | The tag runs use, and the host daemon's image ID for it ([§4](#4-image-identity)) |

An empty string means "not known"; a field is never guessed.

### 3.3 Current or unrecorded

- A record is current when `image.ref == workspaces.image_ref` and `cache_key == workspaces.built_profile_hash`.
- Anything else reads as unrecorded: a `NULL` column, a pre-0.9 image, a row an older binary rebuilt, a higher `schema`.
- Invalidators that clear `image_ref` therefore need no change: the record stops being current on its own.

### 3.4 Read surface

- `GET /api/v1/workspaces/{id}/build` gains `record`, present only when the record is current.
- The projection follows `redactWorkspaceForRead` ([`internal/api/helpers.go`](../../../internal/api/helpers.go)): below the full tier, a `byoi` record's `inputs.base.ref` and `inputs.base.digest` are blanked.
- `image` stays at every tier, for the reason `image_ref` stays: it is the image the reader's own run executes.
- The console shows nothing new until [OD-7](#od-7-console-surface) is answered.

## 4. Image identity

| Identity | Source | Recorded as |
|---|---|---|
| The final image | `ImageInspect` of the output tag on the host daemon | `image.id`, the config digest that names the image on that daemon |
| A `byoi` base | `ImageInspect` of the base ref right after the wrap; the first `RepoDigests` entry for that repository | `inputs.base.digest`; empty for a local-only image with no repo digest |
| A generated base or feature | Registry resolution before the build ([§6.1](#61-resolution-path)) | `digest` on the input |
| The claude-code binary | The pin file the build writes ([§6.3](#63-installer-observation)) | `version`, `platform`, `sha256` |
| Wardyn's runner tools in the finalize stage | The running release | `wardyn_version` |

- **New optional capability** `api.ImageIdentifier`, implemented by `envBuilderAdapter` and type-asserted the way `ImageBuildSweeper` is ([`internal/api/reconcile.go`](../../../internal/api/reconcile.go)):
  - `LocalImage(ctx, ref) (id string, repoDigests []string, err error)`, Session 1;
  - `RemoteDigest(ctx, ref) (string, error)`, through the daemon's `DistributionInspect`, Session 2;
  - `ReadImageFile(ctx, ref, path string, max int) ([]byte, error)`: create a container, never start it, copy one file out, remove it. Session 2.
- A builder without the capability still builds. The record then carries empty identities and `resolved: false`.
- **Provenance is asserted, not attested.** The control plane writes the record from what its own daemon reported. Nothing signs it ([§11](#11-residuals-and-threat-model-delta)).
- The stage-1 manifest digest in the cache registry is an intermediate and is not recorded.

## 5. Pinned versus recorded

A fresh build resolves and records. A rebuild from a record reuses the record's pins and never re-resolves them.

| Input | Fresh build (`cache_miss`, `refresh`) | Rebuild from record (`image_missing`) | Recorded |
|---|---|---|---|
| Generated base | Resolved to a digest and built as `ref@digest`; unpinned when resolution fails | The recorded digest | `ref`, `digest` |
| Generated features | Resolved and built as `ref@digest`, subject to the Session 2 spike ([OD-9](#od-9-if-features-cannot-be-pinned)) | The recorded digests and options | `ref`, `digest`, `options` |
| Toolchain a feature installs (Go, Node, …) | Floats: `{}` options install the feature's default version | Floats | Not recorded ([OD-2](#od-2-depth-of-reproducibility)) |
| OS packages in the base and in feature scripts | Float | Float | Not recorded |
| claude-code | The channel resolves inside the build; the build writes what it got | The recorded version and checksum, written into the RUN | `channel`, `version`, `platform`, `sha256` |
| Generated files | Pure function of profile and pins | Pure function of profile and the record's pins | `generator.salt`, `generator.files_sha256` |
| envbuilder image, runner tools | Operator configuration and the running release | Same: not pinned | `wardyn_version` only |
| `byoi` base | The operator's ref as written; pulled only when absent (refresh: always pulled) | `ref@digest` when a digest was recorded, else the ref as written | `ref`, `digest` |
| Repo devcontainer | The repository at `ref`, built verbatim | The same: the files are never rewritten | `url`, `ref` |
| Final image | Output | Output: a new `image.id` is expected | `image.ref`, `image.id` |

> [!IMPORTANT]
> A rebuild reproduces **inputs**, not bytes. Feature scripts and package managers fetch at build time, so the rebuilt `image.id` differs. The acceptance criterion is inputs: "the same inputs, or fails with a clear difference".

## 6. Resolution and rebuild (Session 2)

### 6.1 Resolution path

- Resolution is `ImageIdentifier.RemoteDigest`: `DistributionInspect` on the host daemon, called from `internal/envbuild`.
- No new dependency; it uses the daemon's proxy settings and stays inside the parity rule's one exception.
- Each lookup gets a `10s` budget. A failure, a timeout or a missing capability leaves that input unpinned ([§8](#8-offline-and-mirrors)).

<details>
<summary>Rejected: a registry client in wardynd, and resolving inside the build.</summary>

- A registry client in wardynd adds a dependency and a second credential path beside the daemon's.
- Resolving inside the build container cannot happen before the recipe is written, so it cannot pin the build it runs in.
- The daemon's `registry-mirrors` setting covers `docker.io` only. For `mcr.microsoft.com` and `ghcr.io` the daemon asks the named registry, through its proxy.
- That split is safe: a digest is a content address, so a pull-through cache serves it unchanged. A path where the daemon resolves and kaniko cannot fetch fails visibly ([§11](#11-residuals-and-threat-model-delta)).

</details>

### 6.2 Generator pins

- `GenerateDevcontainer(p, pins Pins)` takes the pins; the zero `Pins` produces today's files byte for byte.
- `Pins` carries the base digest, a digest per feature ref, and a version and checksum per installer.
- A pinned base is written as `FROM ref@digest`; a pinned feature as the `features` key `ref@digest`.
- A pinned installer replaces the `/stable` fetch with a fixed `ver` and a fixed `sum`. A mismatch prints `pinned claude-code <ver>: checksum mismatch` and fails the RUN.
- `cacheKeySalt` stays `v2`. Bumping it would rebuild every kept image, which the migration forbids ([§10](#10-migration-and-compatibility)).
- `workspacescan.GeneratedInputs(p)` returns the record's unpinned `inputs` from the same `genBaseImage`, `featuresFor` and `genStandardTools`, so the record and the files cannot drift.

### 6.3 Installer observation

- The generated RUN ends by writing `<ver> <plat> <sum>` to `/usr/local/share/wardyn/inputs/claude-code`.
- After the build, `ReadImageFile` copies that file out of the final image without starting it, capped at `4096` bytes.
- A missing, oversized or malformed file leaves the installer fields empty. The build still succeeds.
- The value is what the build reported, not an attestation: a later feature script could overwrite the file.

### 6.4 Rebuild from a record

| Condition | Outcome | Where |
|---|---|---|
| Key matches, image present | Reuse, exactly as today | `Server.resolveWorkspaceImage` |
| Key matches, image missing, record current | Rebuild from the record's pins; `reason` `image_missing` | Same |
| Key matches, image missing, no current record | Fresh build, as today; the new record is written | Same |
| Key differs (rescan, base change, repo ref change) | Fresh build; `reason` `cache_miss` | Same |
| A pinned digest fails its pre-check | No build. `run.build` failure carries `difference`; the run falls back as for any failed build | Same |
| An input the record left unpinned | Floats on the rebuild; never a difference | Same |

- **Pre-check.** Before rebuilding, `RemoteDigest("repo@digest")` runs for each pinned base and feature. Not found is a difference.
- A pre-check that cannot reach the registry is not a difference. The build runs pinned, and kaniko's fetch decides.
- **Never a silent fallback.** A rebuild never retries with fresh resolution. The operator's remedy is a refresh.
- The difference shape, in the `run.build` row and the `GET /build` failure detail:

```json
{"difference": [{"input": "feature:ghcr.io/devcontainers/features/go:1", "want": "sha256:<hex>", "got": "not_found"}]}
```

## 7. Refresh (Session 3)

- **Route.** `POST /api/v1/workspaces/{id}/build` with `{"refresh": true}`. An empty body or `{}` behaves exactly as today. Unknown fields answer `400` through `decodeStrict`.
- **Who.** The route's existing check: owner-or-admin, then `admitRepoSources`, the provider deny and the Azure DevOps credential gate ([OD-5](#od-5-who-may-refresh)).
- **Never automatic.** Nothing else sets the flag.
- **Build, then swap.** The new image builds under a new tag; only success moves `image_ref` and the record. Then the old tag goes through `Server.removeStaleImage`.
- **Failure keeps the old image.** The kept image and its record stay current, and `GET /build` reports `failed` with the builder's error.
- **Single flight.** A refresh takes the same tracker slot as a build; a second click answers `202 building`.
- **In-flight runs** keep their image; removal is best-effort, as today.

| Lane | What a refresh does |
|---|---|
| Generated | Fresh resolution, a cache-bust line in the generated Dockerfile so kaniko replays no layer, a tag `<key12>-<build-id8>` |
| `byoi` | Force-pulls the base ref, then wraps it. A pull failure fails the refresh; the kept image stays |
| Repo devcontainer | Rebuilds the repository at its ref, verbatim. The fixed tag moves, and the superseded image is removed by ID |

- The cache-bust line is `RUN : wardyn-refresh <build-id>`, placed before the installer RUN. kaniko chains layer keys, so every later layer misses.
- `ImageBuilder` methods gain an options value carrying `Refresh`, so `FinalizeBase` can force the pull. Every test fake of `ImageBuilder` takes the extra argument.

## 8. Offline and mirrors

> [!NOTE]
> Today's generated recipe reaches `mcr.microsoft.com`, `ghcr.io` and `downloads.claude.ai` from the build network. A private mirror works only by answering those names, as a pull-through cache or a network redirect; otherwise the operator picks a registry or `byo` base. None of that changes.

| Condition | Fresh build | Rebuild from record | Refresh |
|---|---|---|---|
| The daemon cannot resolve a base or feature | That input builds unpinned; `digest` empty; `resolved: false`; the run is not failed | Pre-check inconclusive; builds pinned | As a fresh build |
| A pull-through cache answers the upstream names | Pins pass through unchanged | Same | Same |
| Air-gapped, upstream unreachable, no mirror | Fails today; still fails, with no new failure mode | Fails; the builder error names the pinned ref | Fails; the kept image stays |
| The daemon resolves but the build network cannot fetch the digest | The build fails ([OD-3](#od-3-resolution-failure-posture)) | Fails | Fails; the kept image stays |
| `byoi` base pre-pulled, local only | Wraps as today; `digest` empty, `image.id` recorded | Wraps the ref as written | The forced pull fails; the kept image stays |
| `WARDYN_ENVBUILD` off, or no builder (Kubernetes) | No build, no record, as today | — | Answers the existing `none` view |

## 9. Audit

| Action | Change | Actor | Data |
|---|---|---|---|
| `run.build` | Every success gains `image_id`, `reason` and `resolved`; a rebuild failure gains `difference` | `wardynd`, system | Existing fields kept |
| `run.build` | The generated lane's cache hit now writes a row, like the other two lanes | `wardynd`, system | `source`, `image`, `image_id`, `cache_hit: true` |
| `workspace.image.refresh` (new) | One row when a refresh finishes, success or failure | The requesting principal | `workspace_id`, `build_id`, `lane`, `previous_image_id`, `image_id`, `changed` (the input names whose digest or version moved) |

- With the cache-hit row, every run on a workspace image has a row naming its `image_id`, across later refreshes.
- `refresh` is already in the closed verb list in [AUDIT-ACTIONS.md](../../AUDIT-ACTIONS.md); the new row and the `run.build` field changes go there in the slice that emits them.
- A daemon restart mid-refresh writes no `workspace.image.refresh` row and changes nothing: build-then-swap never committed.

## 10. Migration and compatibility

| Existing state | After upgrade |
|---|---|
| Cached image, key matches, image present | Reused without a rebuild; reads unrecorded |
| Cached image, key matches, image gone | Fresh build, as today; a record is written |
| No image yet | First build writes a record |
| A workspace on a repo's own devcontainer | Files untouched; the record observes `url`, `ref` and the image |
| Env-as-code export | Unchanged ([OD-8](#od-8-env-as-code-export)) |
| Rollback to 0.8.x | The older binary never reads the column; its rebuilds leave the record non-current, which reads unrecorded after a return |

- Nothing is backfilled; a guessed record is worse than none.
- `cacheKeySalt` is not bumped, and generated output with zero pins is byte-identical; both are pinned by golden tests.

## 11. Residuals and threat-model delta

For the lead's threat-model pass under residual #13; this page does not edit [THREAT-MODEL.md](../../../threatmodel/THREAT-MODEL.md).

| Residual | Substance |
|---|---|
| Inputs, not bytes | Feature scripts and package managers float; the rebuilt `image.id` differs. Toolchain versions are not pinned ([OD-2](#od-2-depth-of-reproducibility)) |
| Asserted record | The control plane writes it from its daemon's answers. A database writer can alter it; nothing signs or attests it |
| Self-reported installer | The pin file is written inside the build, after which feature scripts run and could overwrite it |
| Split resolution | The daemon resolves; kaniko fetches. Different network paths can turn a resolvable digest into a failed build |
| Pinning delays fixes | A rebuild after a prune repeats old inputs, vulnerable ones included, until someone refreshes |
| Refresh depends on the cache-bust line | Proven by a live test in Session 3; a kaniko change that ignores it would replay stale layers |
| Unrecorded configuration | An operator's envbuilder image override is not recorded or pinned |
| Repo devcontainer | Content moving at a fixed ref stays the existing, named ceiling; the lane is recorded, never pinned |
| Per-run builds | `--image` and `--devcontainer-repo` builds get the `run.build` fields only |
| Another host | `image.id` names an image on the building daemon; another host rebuilds from the record and gets equal inputs, not an equal ID |

## 12. Slices

Executors follow the release's lane rules. Every Go change runs the full tree (`go test ./...`) because the audit doc guards live in `cmd/wardynd`. Every test that pins a fix carries a mutation proof.

| Slice | Lane | Executor | Depends on | Owns |
|---|---|---|---|---|
| 1a | F-1516b | Free Z0 | OD-1 | Migration, `types.ImageBuild`, `PG.SetWorkspaceBuiltImage`, `workspacescan.GeneratedInputs`, golden tests |
| 1b | F-1516b | Free Z0 | 1a | `ImageIdentifier.LocalImage`, the record on each build, `run.build` fields, `GET /build` `record` |
| 2 | F-1516c | Sonnet/high or Opus | 1b, OD-1 = A, OD-2, OD-3, OD-4, OD-9 | Spike, `RemoteDigest`, `ReadImageFile`, `Pins`, rebuild from record |
| 3 | F-1516d | Sonnet/high | 1b, OD-5, OD-6 | Refresh core, builder options, cache-bust line, `workspace.image.refresh` |
| 4 | F-1516e | Free Z0 | 3 | `wardyn workspace build <id> [--refresh]`, `pkg/client` methods, extra negative tests |
| 5 | F-1516e | Free Z0 | 2 or 3 as shipped | [ENVBUILD.md](../../ENVBUILD.md), [build-images.md](../../operations/build-images.md), REPORT text for CHANGELOG and threat model |

### 12.1 Slice 1a: storage

DONE WHEN:

1. A PG test shows `SetWorkspaceBuiltImage` writes `image_ref`, `built_profile_hash` and `image_build` in one statement and reads them back equal (`=== RUN` plus PASS).
2. A PG test shows a full-row `UpdateWorkspace` leaves `image_build` unchanged.
3. The migration applies over rows that predate it; they read `ImageBuild == nil`.
4. `GeneratedInputs(p)` hashes match `GenerateDevcontainer(p)` files; features match `featuresFor`.
5. A golden test pins `CacheKey()` of a fixture profile, and fails when `cacheKeySalt` changes.
6. Each test's mutation (drop the column from the `UPDATE`; change the salt) fails, then passes restored.

### 12.2 Slice 1b: record on build

DONE WHEN:

1. Each lane's successful build writes one record whose `lane`, `cache_key` and `image.ref` match; `image.id` comes from a fake `ImageIdentifier`.
2. A builder without the capability still builds; its record has empty identities and `resolved: false`.
3. A cache hit in any lane writes no record. A workspace with a matching key and a `NULL` record is reused, and the builder is never called.
4. The repo lane passes `url` and `ref` unchanged, sends no files, and records no features, installers or generator.
5. A record write failure takes the existing `cache_warn` branch; the run proceeds.
6. Each `run.build` success carries `image_id`; the generated cache hit writes its row.
7. `GET /build` shows `record` at the full tier, blanks a `byoi` base below it, and omits a non-current record.
8. The `run.build` row in [AUDIT-ACTIONS.md](../../AUDIT-ACTIONS.md) lists the new fields; the full tree, `go vet -tags docker ./...`, `make lint`, `make staticcheck` and `scripts/check-file-size.sh` pass.

### 12.3 Slice 2: resolve, pin, rebuild

DONE WHEN:

1. The REPORT carries spike evidence: `DistributionInspect` returns a digest for the base and for `ghcr.io/devcontainers/features/go:1`, and envbuilder builds with both pinned.
2. Zero `Pins` yields today's files byte for byte, against a fixture captured before the change.
3. A fresh build with a resolving fake writes `@sha256:` refs, records the same digests, and sets `resolved: true`.
4. A failing resolver builds today's refs, records empty digests and `resolved: false`, and does not fail the run.
5. The pin file fills the installer fields; a missing or oversized file leaves them empty and the build succeeds.
6. A missing image with a current record builds from the record's pins, records `reason: image_missing`, and keeps the previous inputs.
7. A pre-check `not_found` builds nothing and writes `difference`. Its mutation, a fallback to fresh resolution, fails the test.
8. A `-tags docker` integration builds a pinned image, rebuilds from its record, and shows equal inputs.

### 12.4 Slice 3: refresh

DONE WHEN:

1. `{"refresh": true}` on a `done` workspace builds, writes `reason: refresh`, moves `image_ref`, and removes the old tag through a fake `ImageRemover`.
2. A failed refresh leaves `image_ref`, the record and the old image untouched; `GET /build` reports `failed`.
3. Only a refresh carries the cache-bust line. A live integration shows the refreshed build replays no layer.
4. A `byoi` refresh reaches the builder with `Refresh` set; a repo refresh sends the repo's files untouched.
5. An empty body and `{}` behave as before; an unknown field answers `400`; a member on another's workspace is refused as on build.
6. `workspace.image.refresh` is written with the requesting principal, and its row is in [AUDIT-ACTIONS.md](../../AUDIT-ACTIONS.md).

### 12.5 Slices 4 and 5: mirrors and docs

DONE WHEN:

1. `wardyn workspace build <id> --refresh` sends the body and prints the state; a client test pins the request.
2. Docs state exactly what shipped: "recorded and refreshable", and "reproducible inputs" only if Slice 2 merged.
3. The REPORT carries the CHANGELOG lines and the [§11](#11-residuals-and-threat-model-delta) delta text for the lead.

## 13. Owner decisions

The design above assumes the recommended option in each row. A different answer changes the slices named.

### OD-1: Is reproducible rebuild a 0.9 product promise?

| Option | Ships | Docs promise |
|---|---|---|
| A | Slices 1 to 5 | "Rebuilds reproduce recorded inputs, or fail naming the input" |
| B | Slices 1, 3, 4, 5 | "Each build is recorded; a kept image is refreshed on purpose. Not locked" |
| C | Slices 1, 5 | "Each build is recorded" |
| D | Nothing in 0.9 | The 0.8.5 wording stands |

- Recommendation: **B committed, A as stretch.** The lane is in cut tier 3, and Slice 2 depends on an unproven spike. The promise upgrades to A only if Slice 2 merges with its proofs.

### OD-2: Depth of reproducibility

| Option | Pins |
|---|---|
| A | Base, feature and installer identities; feature toolchains and packages float |
| B | A, plus each feature's installed toolchain version, observed by a confined probe and pinned as the feature's `version` option |

- Recommendation: **A.** B needs a per-feature probe that runs the built image. Name it as a residual and revisit after 0.9.

### OD-3: Resolution failure posture

| Option | When the daemon cannot resolve, or the build cannot fetch the pinned digest |
|---|---|
| A | Build unpinned and record `resolved: false`; a pinned fetch failure fails like any build |
| B | A, but retry once unpinned after a pinned fetch failure |
| C | Fail closed: no unpinned builds |
| D | A, with an operator setting to choose C |

- Recommendation: **A, no setting.** It keeps every mirror and offline build working. B doubles the time of a failing build and hides the split; C breaks air-gapped hosts. Nobody has asked for D.

### OD-4: A missing image with a record

| Option | Behaviour |
|---|---|
| A | Rebuild from the record's pins; fail with a difference |
| B | Rebuild from the record; on a difference, build fresh and audit a warning |
| C | Always build fresh; rebuild from a record only on request |

- Recommendation: **A.** It is the acceptance criterion. Refresh remains the remedy, and B would be the silent fallback the issue rules out.

### OD-5: Who may refresh

| Option | Who |
|---|---|
| A | The build route's rule: owner-or-admin, with the repo admission gates |
| B | Operators only |

- Recommendation: **A.** A refresh runs the same Wardyn-generated recipe a build runs, and the owner already holds Build.

### OD-6: Lanes refresh covers

| Option | Lanes |
|---|---|
| A | All three: generated (fresh resolution), `byoi` (forced pull), repo (rebuild at ref) |
| B | Generated only |

- Recommendation: **A.** A kept `byoi` tag never refreshes today, which is the staleness the issue names.

### OD-7: Console surface

| Option | 0.9 surface |
|---|---|
| A | API and CLI only; a console Refresh control and record view go through a mock round in 0.10 |
| B | A Refresh control and an "unrecorded" or build-age line on the workspace Build step, after a 0.9 mock round |
| C | B, plus a background check that warns when an upstream tag has moved |

- Recommendation: **A.** [CONSOLE-RULES.md](../CONSOLE-RULES.md) requires a mock round, and C adds polling of upstream registries.

### OD-8: Env-as-code export

| Option | Export |
|---|---|
| A | Unchanged: tags, as today |
| B | Pinned refs from the current record |
| C | Unchanged files, plus the record as `.devcontainer/wardyn-build-record.json` |

- Recommendation: **A in 0.9.** B and C put Wardyn-owned files into a repository a person commits, so they deserve their own decision.

### OD-9: If features cannot be pinned

| Option | If the spike shows envbuilder or `DistributionInspect` cannot handle feature digests |
|---|---|
| A | Record feature refs and options unpinned; pin base and installer only |
| B | Add a registry client dependency to resolve feature artifacts |
| C | Hold Slice 2 |

- Recommendation: **A.** `resolved` stays `false` for generated records with features, and the docs say so.
