# Filesystem recording read confinement

- How the `fs` recording store keeps a replay read inside the recording directory, and what it does not cover.
- Only the Docker shared-mount fallback is affected. The default `pg` store does not use this path.

## Behaviour

- With the shared-mount fallback, a sandbox can write the recording directory, so it can place a symlink there.
- Replay opens a cast through a rooted handle (`os.OpenRoot`, then `Root.Open`; [`internal/recording/store.go`](../internal/recording/store.go)). A link that leaves the recording directory is refused. A link that stays inside it works.
- Replay refuses a file that is not a regular file.

## Not covered

- Another sandbox that shares the directory can still alter other recordings in it.
- Recordings on the fallback are not masked.
- An administrator who replaces the configured root, or installs a mount inside it, is outside this check.
- Hard links and file permissions stay OS boundaries.

Prefer the default `pg` store ([`WARDYN_RECORDING_STORE`](ENV.md#wardyn_recording_store)).
