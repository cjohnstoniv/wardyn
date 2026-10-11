# 0.9 environment build record: resolved inputs, image identity and refresh (#1516)

Status: **Owner rulings recorded 2026-10-10; review r1 applied.** Reproducible rebuilds are a 0.9 promise for pinned builds. A build that could not be pinned is recorded and labelled not reproducible.
Each workspace image build records what it actually used, and a rebuild reproduces those inputs or names the one that changed.
A refresh replaces a kept image on purpose. Existing images, mirrors, offline hosts and user devcontainers keep working unchanged.

## 1. Scope

| In scope | Out of scope |
|---|---|
| The three workspace image lanes in `Server.resolveWorkspaceImage` ([`internal/api/workspace_run_image.go`](../../../internal/api/workspace_run_image.go)) | Per-run builds (`Server.resolveCreateRunImage`): `run.build` fields only |
| One record per workspace image, its read surface, its audit | A generic build interface, a distributed cache, a lockfile |
| Resolve-then-pin; rebuild from a record | Production image pinning, the UI lockfile, release signing |
| An intentional refresh, and a new `wardyn workspace build` command | The 0.8.5 wording fix, which has shipped |

### 1.1 Changes to the planner's proposal

| Brief item | This design | Why |
|---|---|---|
| New table `workspace_image_builds` with `superseded_at` | One `jsonb` column, `workspaces.image_build`, written by a compare-and-swap with `image_ref` | A record and its image cannot disagree; each build's full record and hash ride its append-only `run.build` row ([§9](#9-audit)) |
| Optional `runner.ImageDigester` | The builder returns the image identity, plus an optional capability `api.ImageIdentifier` | The builder made the image; the parity rule blesses only `internal/envbuild` |
| `POST /api/v1/workspaces/{id}/image/refresh` | `POST /api/v1/workspaces/{id}/build` with body `{"refresh": true}` | It already carries the authorization, admission gates and tracker |
| Resolve the installer channel before the build | Observe what the installer resolved, from a file the generated build writes; pin it on rebuild | No new control-plane egress; follows the build network |

## 2. Facts this design builds on

| Fact | Where |
|---|---|
| The generated recipe names `mcr.microsoft.com/devcontainers/base:ubuntu`, `ghcr.io/devcontainers/features/*:1` with `{}` options, and the claude-code `/stable` channel | `genBaseImage`, `genLangFeatures`, `genAgentToolInstalls` in [`internal/workspacescan/gen.go`](../../../internal/workspacescan/gen.go) |
| `CacheKey` is `SHA256(ProfileHash + "\|" + cacheKeySalt)`, salt `v2`; generator output is not in it | `WorkspaceProfile.CacheKey` in [`internal/workspacescan/profile.go`](../../../internal/workspacescan/profile.go) |
| Reuse needs a stored ref, a matching key and `cachedImageStillPresent`, which fails open; the generated lane audits no cache hit; its tag is `<key12>` and the repo tag is fixed | `Server.resolveWorkspaceImage` |
| `resolveWorkspaceImage` runs from `POST /build`, from run creation and from `workspaceRunImage`; only the first holds the build tracker | [`internal/api/runs_create.go`](../../../internal/api/runs_create.go), [`internal/api/workspace_build.go`](../../../internal/api/workspace_build.go) |
| `PG.SetWorkspaceBuiltImage` updates `WHERE id=$3`, unfenced | [`internal/store/store_workspaces.go`](../../../internal/store/store_workspaces.go) |
| `ImageBuilder` returns only an image ref; envbuilder pushes stage 1 to the cache registry and finalize tags it on the host daemon | `ImageBuilder` in [`internal/api/server_deps.go`](../../../internal/api/server_deps.go), [ENVBUILD.md](../../ENVBUILD.md#two-stage-build) |
| A `byoi` base is pulled only when absent and inspected once, in `assertWrapSafeBase`; the final image is never pushed | `Builder.ensureImage`, `Builder.assertWrapSafeBase` in [`internal/envbuild/builder.go`](../../../internal/envbuild/builder.go) |
| `resolveBuildView` answers `nothing_to_build` for registry and `byo` kinds before it reads the tracker | `Server.resolveBuildView` in [`internal/api/workspace_build.go`](../../../internal/api/workspace_build.go) |

## 3. The record

### 3.1 Where it lives

- Column `workspaces.image_build jsonb NULL`, added by migration `<NNNN>_workspace_image_build`; the lead assigns the number at merge.
- `PG.SetWorkspaceBuiltImage(ctx, id, observedRef, imageRef, builtHash, rec)` writes `image_ref`, `built_profile_hash` and `image_build` in one statement.
- The statement is a compare-and-swap: `WHERE id=$1 AND image_ref IS NOT DISTINCT FROM $observed`. It returns `applied`.
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
  "image": {"ref": "wardyn-workspace/<workspace-id>:<key12>-<build-id8>", "id": "sha256:<hex>"}
}
```

| Field | Meaning |
|---|---|
| `schema` | `1`; a higher number reads as unrecorded |
| `lane` | `generated`, `byoi` or `repo_devcontainer` |
| `cache_key` | The lane's cache key |
| `build_id` | The ID on the build's `run.build` row |
| `reason` | `cache_miss` (no image, or a different key), `image_missing` (key matched, image gone), `refresh` |
| `resolved` | Per lane, below. `false` is shown everywhere as "not reproducible" |
| `inputs.base` | Wardyn's base, or the operator's `byoi` ref and the digest the wrap used |
| `inputs.features[]` | Generated lane only. `options` is what the generator wrote |
| `inputs.installers[]` | Generated lane only, from the pin file ([§6.3](#63-installer-observation)) |
| `inputs.generator` | Generated lane only: the salt and the hash of each file built |
| `inputs.repo` | Repo lane only: `{url, ref}` as built |
| `image` | The build's own tag and its image ID |

| Lane | `resolved` is `true` when |
|---|---|
| Generated | The base, every feature and every installer carry a digest or a version and checksum |
| `byoi` | `inputs.base.digest` is present |
| Repo devcontainer | Never: the repository's files and their inputs are not pinned |

An empty string means "not known"; a field is never guessed. `record_sha256` is the SHA-256 of the record's `encoding/json` bytes, and is computed, not stored.

### 3.3 Current or unrecorded

A record is current only when all three hold:

1. `image.ref == workspaces.image_ref`;
2. `cache_key == workspaces.built_profile_hash`;
3. when the builder implements `ImageIdentifier`, `LocalImage(image.ref)` returns `image.id`.

| Check 3 answer | Result |
|---|---|
| Same ID | Current |
| Not found | Current, and the image is missing: the rebuild path in [§6.4](#64-rebuild-from-a-record) applies |
| A different ID: the tag moved under the record | Unrecorded |
| Any other error | Unrecorded: the record cannot be verified |

- A `NULL` column or a pre-0.9 image reads as unrecorded; invalidators that clear `image_ref` need no change.

### 3.4 Read surface

- `GET /api/v1/workspaces/{id}/build` gains `record` and `record_sha256`, present only when the record is current, and `reproducible` (`resolved`).
- The projection follows `redactWorkspaceForRead` ([`internal/api/helpers.go`](../../../internal/api/helpers.go)): below the full tier, a `byoi` record's `inputs.base.ref` and `inputs.base.digest` are blanked.
- `image` stays at every tier, as `image_ref` does.
- The console shows nothing new in 0.9 (ruling Q7).

## 4. Image identity

| Identity | Source | Recorded as |
|---|---|---|
| The final image | The `aux.ID` message of the finalize build stream, read by an extended `drainBuildResponse`; never a later inspect | `image.id` |
| A `byoi` base | The `ImageInspect` that `assertWrapSafeBase` already makes: its `ID` and the `RepoDigests` entry for the base's repository | `inputs.base.digest` |
| A generated base or feature | Registry resolution before the build ([§6.1](#61-resolution-path)) | `digest` on the input |
| The claude-code binary | The pin file the generated build writes ([§6.3](#63-installer-observation)) | `version`, `platform`, `sha256` |
| Wardyn's runner tools in the finalize stage | The running release | `wardyn_version` |

- **Build-ID tags.** Every 0.9 build tags its own image, so no later build moves the tag a record names:
  - generated: `wardyn-workspace/<id>:<key12>-<build-id8>`;
  - repo devcontainer: `wardyn-workspace/<id>:devcontainer-<build-id8>`;
  - `byoi`: `wardyn-byoi/<build-id>:latest`, unchanged.
- **`byoi` wraps by digest.** When the inspect yields a repo digest, finalize writes `FROM repo@digest`, so a concurrent pull that moves the tag cannot change what is wrapped. A local-only base wraps by ref, as today.
- **`ImageBuilder` returns `BuiltImage{Ref, ID, BaseDigest}`** instead of a bare ref. Slice 2 adds the pin file bytes.
- **Optional capability `api.ImageIdentifier`**, implemented by `envBuilderAdapter` and type-asserted the way `ImageBuildSweeper` is ([`internal/api/reconcile.go`](../../../internal/api/reconcile.go)):
  - `LocalImage(ctx, ref) (id string, err error)`, for the currency check;
  - `RemoteDigest(ctx, ref) (string, error)`, through the daemon's `DistributionInspect`, in Slice 2.
- **Provenance is asserted, not attested**: nothing signs the record ([§11](#11-residuals-and-threat-model-delta)).

## 5. Pinned versus recorded

A fresh build resolves and records. A rebuild from a record reuses the record's pins and never re-resolves them.

| Input | Fresh build (`cache_miss`, `refresh`) | Rebuild from record (`image_missing`) | Recorded |
|---|---|---|---|
| Generated base | Built as `ref@digest`; unpinned if unresolved | Recorded digest | `ref`, `digest` |
| Generated features | Built by digest if Slice 0 proves it; else unpinned (Q9) | Recorded digests, options | `ref`, `digest`, `options` |
| Feature toolchains, OS packages | Float | Float | Not recorded (Q2) |
| claude-code | Resolved in the build on a refresh; a `cache_miss` may replay a cached layer | Recorded version and checksum | `channel`, `version`, `platform`, `sha256` |
| Generated files | From profile and pins | From profile and record pins | `generator` |
| envbuilder, runner tools | Configuration and release | Not pinned | `wardyn_version` |
| `byoi` base | Pulled if absent (refresh: always); wrapped by digest | `ref@digest`, else the ref | `ref`, `digest` |
| Repo devcontainer | Built verbatim at `ref` | Same; never rewritten | `url`, `ref` |
| Final image | Output | A new `image.id` | `image` |

> [!IMPORTANT]
> A rebuild reproduces **inputs**, not bytes. Feature scripts and package managers fetch at build time, so the rebuilt `image.id` differs. The acceptance criterion is inputs: "the same inputs, or fails with a clear difference".

## 6. Resolution and rebuild (Slice 2)

### 6.1 Resolution path

- Resolution is `ImageIdentifier.RemoteDigest`: `DistributionInspect` on the host daemon, called from `internal/envbuild`.
- For the multi-architecture base it returns the index digest; kaniko selects the platform from it.
- Each lookup gets a `10s` budget. A failure, a timeout or a missing capability leaves that input unpinned ([§8](#8-offline-and-mirrors)).

<details>
<summary>Rejected: a registry client in wardynd, and resolving inside the build.</summary>

- A registry client adds a dependency and a second credential path (ruling Q9 declines it).
- Resolving inside the build cannot pin the build it runs in.
- The daemon's `registry-mirrors` setting covers `docker.io` only. For `mcr.microsoft.com` and `ghcr.io` the daemon asks the named registry, through its proxy.
- That split is safe: a digest is a content address, so a pull-through cache serves it unchanged. A path where the daemon resolves and kaniko cannot fetch fails visibly ([§11](#11-residuals-and-threat-model-delta)).

</details>

### 6.2 Generator pins

- `GenerateDevcontainer(p, pins Pins)` takes the pins; the zero `Pins` produces the Slice 1a golden fixture plus the pin-file write of [§6.3](#63-installer-observation), nothing else; `EmitEnvAsCode` output stays byte-identical (ruling Q8).
- `Pins` carries the base digest, a digest per feature ref, and a version and checksum per installer.
- A pinned base is written as `FROM ref@digest`; a pinned feature uses the key form Slice 0 proved.
- A pinned installer replaces the `/stable` fetch with a fixed `ver` and a fixed `sum`. A mismatch prints `pinned claude-code <ver>: checksum mismatch` and fails the RUN.
- `cacheKeySalt` stays `v2`. Bumping it would rebuild every kept image, which the migration forbids ([§10](#10-migration-and-compatibility)).
- `workspacescan.GeneratedInputs(p)` returns the record's unpinned `inputs` from the same `genBaseImage`, `featuresFor` and `genStandardTools`, so the record and the files cannot drift.

### 6.3 Installer observation

- The generated RUN ends by writing `<ver> <plat> <sum>` to `/usr/local/share/wardyn/inputs/claude-code`.
- Only `BuildFromDevcontainerFiles`, the generated lane's builder call, reads it back. A repo author's or `byoi` image carrying that path records no installer.
- The read creates a container from the final image with a placeholder command and network `none`, never starts it, copies the file out, and removes the container.
- The copy is capped at `4096` bytes. A missing, oversized or malformed file leaves the installer fields empty, and the build still succeeds.
- The value is what the build reported, not an attestation: a later feature script could overwrite the file.

### 6.4 Rebuild from a record

| Condition | Outcome | Where |
|---|---|---|
| Key matches, image present, record current | Reuse, exactly as today | `Server.resolveWorkspaceImage` |
| Key matches, image missing, record current | Rebuild from the record's pins; `reason` `image_missing` | Same |
| Key matches, image missing, no current record | Fresh build, as today; the new record is written | Same |
| Key differs (rescan, base change, repo ref change) | Fresh build; `reason` `cache_miss` | Same |
| A pinned digest fails its pre-check | No build. `run.build` failure carries `difference`; the run falls back as for any failed build | Same |
| An input the record left unpinned | Floats on the rebuild; never a difference; the new record stays not reproducible | Same |

- **Pre-check.** Before rebuilding, `RemoteDigest("repo@digest")` runs for each pinned base and feature. Not found is a difference.
- A pre-check that cannot reach the registry is not a difference. The build runs pinned, and kaniko's fetch decides.
- **Never a silent fallback.** A rebuild never retries with fresh resolution (ruling Q4). The operator's remedy is a refresh.
- The difference shape, in the `run.build` row and the `GET /build` failure detail:

```json
{"difference": [{"input": "feature:ghcr.io/devcontainers/features/go:1", "want": "sha256:<hex>", "got": "not_found"}]}
```

## 7. Refresh (Slice 3)

- **Route.** `POST /api/v1/workspaces/{id}/build` with `{"refresh": true}`. An empty body or `{}` behaves exactly as today. Unknown fields answer `400` through `decodeStrict`.
- **Who.** The route's existing check: owner-or-admin, then `admitRepoSources`, the provider deny and the Azure DevOps credential gate (ruling Q5).
- **The requester** is captured in the handler, before the detached goroutine starts, and carried to the audit row.
- **Both short-circuits are bypassed.** A refresh skips the `nothing_to_build` and `done` answers. Until the next build, `resolveBuildView` reads the tracker before either, so a `byoi` refresh reports `building` and `failed`.
- **Build, then swap.** The new image builds under its own build-ID tag. Success moves `image_ref` and the record by compare-and-swap against the ref observed at request time.
- **Swap applied.** The old tag goes through `Server.removeStaleImage`, which every lane now calls only after an applied swap.
- **Swap not applied** (another build moved `image_ref` first). The refresh removes its own tag, reports `failed: superseded`, and audits a failure.
- **Build failure** keeps the old image and record; `GET /build` reports `failed`.
- **Single flight** covers `POST /build` only; the compare-and-swap fences run-launch builds.

| Lane | What a refresh does |
|---|---|
| Generated | Fresh resolution, and a cache-bust line in the generated Dockerfile so kaniko replays no later layer |
| `byoi` | Force-pulls the base ref, then wraps it by digest. A pull failure fails the refresh; the kept image stays. The previous base stays on the host |
| Repo devcontainer | Rebuilds the repository at its ref, verbatim |

- The cache-bust line is `RUN : wardyn-refresh <build-id>`, placed before the installer RUN. kaniko chains layer keys, so every later layer misses.
- `ImageBuilder` methods gain an options value carrying `Refresh`, so `FinalizeBase` can force the pull.
- A run-launch build that loses the swap serves its own run and audits `cache_warn: superseded`.
- Owner ruling 2026-10-11: no shared build cache. The per-build cache tag is kept; the non-fatal kaniko cache error on every build is documented; a Limitations paragraph follows in slice B7. Nothing here enables a shared cache, and the cache-bust line and the installer-replay residual stay as written until that paragraph lands.

## 8. Offline and mirrors

> [!NOTE]
> Today's generated recipe reaches `mcr.microsoft.com`, `ghcr.io` and `downloads.claude.ai` from the build network. A private mirror works only by answering those names, as a pull-through cache or a network redirect; otherwise the operator picks a registry or `byo` base. None of that changes.

| Condition | Fresh build | Rebuild from record | Refresh |
|---|---|---|---|
| The daemon cannot resolve a base or feature | That input builds unpinned; `digest` empty; not reproducible; the run is not failed (ruling Q3) | Pre-check inconclusive; builds pinned | As a fresh build |
| A pull-through cache answers the upstream names | Pins pass through unchanged | Same | Same |
| Air-gapped, upstream unreachable, no mirror | Fails today; still fails, with no new failure mode | Fails; the builder error names the pinned ref | Fails; the kept image stays |
| The daemon resolves but the build network cannot fetch the digest | The build fails like any build | Fails | Fails; the kept image stays |
| `byoi` base pre-pulled, local only | Wraps by ref as today; `digest` empty; not reproducible | Wraps the ref as written | The forced pull fails; the kept image stays |
| `WARDYN_ENVBUILD` off, or no builder (Kubernetes) | No build, no record, as today | — | Answers the existing `none` view |

## 9. Audit

| Action | Change | Actor | Data |
|---|---|---|---|
| `run.build` | Every success gains `image_id`, `reason`, `resolved`, `record` (the canonical record) and `record_sha256`; a rebuild failure gains `difference` | `wardynd`, system | Existing fields kept |
| `run.build` | The generated lane's cache hit now writes a row, like the other two lanes | `wardynd`, system | `source`, `image`, `image_id`, `record_sha256`, `cache_hit: true` |
| `workspace.image.refresh` (new) | One row when a refresh finishes: success, failure or `superseded` | The requester, captured before the goroutine | `workspace_id`, `build_id`, `lane`, `previous_image_id`, `image_id`, `previous_record_sha256`, `record_sha256`, `changed` (input names whose digest or version moved) |

- Comparing the column's `record_sha256` with the append-only row catches a database writer who altered it. The comparison is manual in 0.9.
- With the cache-hit row, every run on a workspace image has a row naming its `image_id`.
- A `byoi` record repeats the base ref `run.build` already carries, so a run's creator learns nothing new.
- `refresh` is already in the closed verb list in [AUDIT-ACTIONS.md](../../AUDIT-ACTIONS.md); the new row and the `run.build` field changes go there in the slice that emits them.

## 10. Migration and compatibility

| Existing state | After upgrade |
|---|---|
| Cached image, key matches, image present | Reused without a rebuild; reads unrecorded |
| Cached image, key matches, image gone | Fresh build; a record is written |
| No image yet | First build writes a record |
| A workspace on a repo's own devcontainer | Files untouched; the record observes `url`, `ref` and the image; never reproducible |
| Env-as-code export | Unchanged (ruling Q8) |
| Rollback to 0.8.x | The older binary never reads the column. Its rebuilds tag `<key12>` or `:devcontainer`, which no 0.9 record names, so the record reads unrecorded after a return |

- Nothing is backfilled; a guessed record is worse than none.
- `cacheKeySalt` is not bumped, and generated output with zero pins is the Slice 1a golden fixture plus the pin-file write of [§6.3](#63-installer-observation), nothing else; `EmitEnvAsCode` output stays byte-identical (ruling Q8). Both are pinned by golden tests written before any generator edit.

## 11. Residuals and threat-model delta

For the lead's threat-model pass under residual #13; this page does not edit [THREAT-MODEL.md](../../../threatmodel/THREAT-MODEL.md).

| Residual | Substance |
|---|---|
| Inputs, not bytes | Feature scripts and package managers float; the rebuilt `image.id` differs. Toolchain versions are not pinned (ruling Q2) |
| Cache registry substitution | The cache registry is unauthenticated and multi-homed (B8). A writer on the envbuild bridge can replace the stage-1 tag or a kaniko layer, and the record still reads `resolved: true`. Pins never cover replayed layers |
| Installer layer replay | A `cache_miss` build can replay an earlier build's installer layer, so only a refresh guarantees a fresh channel resolution |
| First resolution trusts the network | Resolution is trust on first use: the first digest is whatever the daemon's path to the registry returned |
| Asserted record | Nothing signs it; tampering is detected only by comparing with the audit row |
| Self-reported installer | The pin file is written inside the build, after which feature scripts run and could overwrite it |
| Split resolution | The daemon resolves and kaniko fetches; different paths can fail a resolvable digest |
| Pinning delays fixes | A rebuild after a prune repeats old, possibly vulnerable inputs until a refresh |
| Refresh depends on the cache-bust line | Proven by a live test in Slice 3; a kaniko change that ignores it would replay stale layers |
| Crash after the swap | A crash between the swap and the audit write leaves a swapped image with no `workspace.image.refresh` row; its `run.build` row is also lost |
| Superseded bases and lost swaps | A forced `byoi` pull leaves the previous base on the host. A run-launch build that loses the swap leaves its tag behind, as a failed cache write does today |
| Local-only `byoi` base | With no repo digest, the wrap is by ref, and a concurrent re-tag can still change what is wrapped |
| Unrecorded configuration | An operator's envbuilder image override is not recorded or pinned |
| Repo devcontainer | Content moving at a fixed ref stays the existing ceiling; recorded, never pinned |
| Another host | `image.id` names an image on the building daemon; another host rebuilds from the record and gets equal inputs, not an equal ID |

## 12. Slices

Every Go change runs the full tree (`go test ./...`): the audit doc guards live in `cmd/wardynd`. Each named mutation must fail its test, then pass restored.

| Slice | Executor | Depends on | Owns |
|---|---|---|---|
| 0 | Free Z0, throwaway, commits nothing | — | The spike: checks (a) to (d) on a real daemon |
| 1a | Free Z0 | 0 passed | Migration, `types.ImageBuild`, the compare-and-swap writer, `GeneratedInputs`, golden fixtures |
| 1b | Sonnet/high | 1a | `BuiltImage`, build-ID tags, `aux.ID`, the `byoi` digest wrap, `LocalImage`, currency, records, `run.build` fields, `GET /build` |
| 2 | Sonnet/high or Opus | 1b | `RemoteDigest`, the pin file, `Pins`, rebuild from record |
| 3 | Sonnet/high | 1b | Refresh core, builder options, the cache-bust line, `workspace.image.refresh` |
| 4 | Free Z0 | 3 | New command `wardyn workspace build <id> [--refresh]`, `pkg/client` methods, extra negative tests |
| 5 | Free Z0 | 2 and 3 | [ENVBUILD.md](../../ENVBUILD.md), [build-images.md](../../operations/build-images.md), REPORT text for CHANGELOG and threat model |

### 12.0 Slice 0: spike

The standalone brief holds the exact commands. Checks:

- **(a)** `DistributionInspect` returns a digest for the base (an index digest) and for `ghcr.io/devcontainers/features/go:1`, an OCI artifact whose config media type the daemon may reject.
- **(b)** envbuilder builds `FROM ref@digest` and a `features` key pinned by digest. The proof is the negative control: a feature pinned to an all-zero digest fails the build with `MANIFEST_UNKNOWN`, which proves the digest is fetched (envbuilder 1.3.0 does not log feature fetches).
- **(c)** `ContainerCreate` plus `CopyFromContainer` works on an image with `ENTRYPOINT []` and `CMD []`, given a placeholder command and network `none`.
- **(d)** Whether kaniko replays the installer layer across builds, and that the cache-bust line stops it.

> [!IMPORTANT]
> If (a) or (b) fails for features, ruling Q9 makes every workspace with a detected language not reproducible, and the promise covers only language-less workspaces. Escalate to the owner before Slice 1a starts; do not ship that state under the 0.9 promise.

Slice 0 passed (a)–(d) on 2026-10-11 after harness corrections; both feature key forms work, and the generator writes `ref:tag@digest`.

A failure of (c) or (d) amends [§6.3](#63-installer-observation) or [§7](#7-refresh-slice-3) first.

### 12.1 Slice 1a: storage

DONE WHEN:

1. A PG test shows the writer sets `image_ref`, `built_profile_hash` and `image_build` in one statement and reads them back equal (`=== RUN` plus PASS).
2. A PG test shows a stale `observedRef` returns `applied=false` and changes nothing. Mutation: drop the `IS NOT DISTINCT FROM` predicate.
3. A PG test shows a full-row `UpdateWorkspace` leaves `image_build` unchanged.
4. The migration applies over rows that predate it; they read `ImageBuild == nil`.
5. A golden fixture of `GenerateDevcontainer` output for three profiles lands before any `gen.go` edit; later slices compare against it.
6. A golden test pins `CacheKey()` of a fixture profile. Mutation: change `cacheKeySalt`.
7. `GeneratedInputs(p)` hashes match `GenerateDevcontainer(p)` files; features match `featuresFor`.

### 12.2 Slice 1b: record on build

DONE WHEN:

1. Each lane's successful build writes one record whose `lane`, `cache_key`, `image.ref` and `image.id` match the fake builder's `BuiltImage`.
2. A `-tags docker` test shows `image.id` equals the finalize stream's `aux.ID` for a real build.
3. Each lane tags with its build ID. A tag moved under a record reads unrecorded. Mutation: drop the ID check.
4. A `byoi` build wraps `FROM repo@digest` when the inspect yields one, and records that digest.
5. `resolved` follows the per-lane table; a repo record is always `false`. Mutation: return `true` for the repo lane.
6. A cache hit in any lane writes no record. A workspace with a matching key and a `NULL` record is reused, and the builder is never called.
7. The repo lane passes `url` and `ref` unchanged, sends no files, and records no features, installers or generator.
8. A lost swap takes the `cache_warn: superseded` branch; the run proceeds on its own image; no old tag is removed.
9. Each `run.build` success carries `record` and a `record_sha256` that matches it; the generated cache hit writes its row.
10. `GET /build` shows `record` at the full tier, blanks a `byoi` base below it, and omits a non-current record. Mutation: drop the redaction.
11. The `run.build` row in [AUDIT-ACTIONS.md](../../AUDIT-ACTIONS.md) lists the new fields; the full tree, `go vet -tags docker ./...`, `make lint`, `make staticcheck` and `scripts/check-file-size.sh` pass.

### 12.3 Slice 2: resolve, pin, rebuild

DONE WHEN:

1. Zero `Pins` yields the Slice 1a golden fixture byte for byte.
2. A fresh build with a resolving fake writes `@sha256:` refs, records the same digests, and sets `resolved: true`.
3. A failing resolver builds today's refs, records empty digests and `resolved: false`, and does not fail the run.
4. The generated lane fills installer fields from the pin file; a missing or oversized file leaves them empty and the build succeeds.
5. A repo or `byoi` image carrying the pin file records no installer. Mutation: remove the lane guard.
6. A missing image with a current record builds from the record's pins, records `reason: image_missing`, and keeps the previous inputs.
7. A pre-check `not_found` builds nothing and writes `difference`. Mutation: fall back to fresh resolution.
8. A `-tags docker` integration builds a pinned image, rebuilds from its record, and shows equal inputs.

### 12.4 Slice 3: refresh

DONE WHEN:

1. `{"refresh": true}` on a `done` workspace builds, writes `reason: refresh`, moves `image_ref`, and removes the old tag through a fake `ImageRemover`.
2. A `byoi` workspace reporting `nothing_to_build` refreshes, and `GET /build` then reports `building`, then `done` or `failed`.
3. A run-launch build landing between the refresh's build and swap makes the refresh report `failed: superseded`, remove only its own tag, and audit. Mutation: drop the swap predicate.
4. A failed refresh leaves `image_ref`, the record and the old image untouched.
5. Only a refresh carries the cache-bust line; a live integration shows the refreshed build replays no layer. Mutation: omit the line, and the live test shows a replay.
6. An empty body and `{}` behave as before; an unknown field answers `400`; a member on another's workspace is refused as on build.
7. `workspace.image.refresh` names the requester and both record hashes, and its row is in [AUDIT-ACTIONS.md](../../AUDIT-ACTIONS.md).

### 12.5 Slices 4 and 5: command and docs

DONE WHEN:

1. `wardyn workspace build <id> --refresh` sends the body and prints the state, including "not reproducible"; a client test pins the request.
2. Docs state the promise as ruled: a pinned build's rebuild reproduces its recorded inputs or fails naming the input; an unpinned build is recorded and labelled not reproducible.
3. The REPORT carries the CHANGELOG lines and the [§11](#11-residuals-and-threat-model-delta) delta text for the lead.

## 13. Owner rulings (2026-10-10)

| # | Ruling | Effect on this design |
|---|---|---|
| Q1 | Reproducible rebuild is a 0.9 promise. It covers pinned builds; a build that could not be pinned is recorded and labelled not reproducible | Slice 2 is committed scope; `resolved: false` reads "not reproducible" |
| Q2 | Pin base, feature and installer identities; not toolchain versions | [§5](#5-pinned-versus-recorded); residual "Inputs, not bytes" |
| Q3 | An unresolvable digest: build unpinned and record it; no setting | [§8](#8-offline-and-mirrors) |
| Q4 | Image missing with a record: rebuild from the record's pins; fail naming the difference | [§6.4](#64-rebuild-from-a-record) |
| Q5 | Refresh by owner or admin, as Build | [§7](#7-refresh-slice-3) |
| Q6 | Refresh covers all three lanes | [§7](#7-refresh-slice-3) |
| Q7 | API and CLI only in 0.9 | [§3.4](#34-read-surface), Slice 4 |
| Q8 | Env-as-code export unchanged | [§10](#10-migration-and-compatibility) |
| Q9 | Unpinnable features recorded unpinned; no registry client dependency | [§6.1](#61-resolution-path); the Slice 0 escalation rule |
