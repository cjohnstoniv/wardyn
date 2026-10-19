# Filesystem recording read confinement

- How the filesystem recording store keeps a replay read inside the recording directory, and what it does not cover.
- Only the shared-mount fallback is affected; the default PostgreSQL store does not use this path.

## Scope and preconditions

- `FSStore.OpenCast` and `StatAndTail` validated the key lexically, then used `os.Open`, which could follow a symlink outside the recording directory.
- The supported Docker recording-mount fallback shares writable storage with the sandbox (`agentMounts`, `RecordingMount`).
- In that configuration, a caller able to place a cast symlink and replay that run can cause the daemon to read an unrelated daemon-readable file.
- The route's owner/operator authorization does not confine the underlying filesystem open.
- The default PostgreSQL store does not use this path; configuring filesystem storage without an untrusted writer does not by itself supply the attacker precondition.
- This exceeds the fallback's already-disclosed lack of cross-run isolation and unmasked-recording residual: the target need not be a recording at all.

## Minimal correction

- Both read sites open the file through a rooted handle (`os.OpenRoot`, then `Root.Open`) with the existing validated basename.
- There is no new dependency, interface, storage layout, mount, policy or default.
- Regular saves/replacements and missing-recording error mapping are unchanged.
- Relative links contained inside the recording directory remain supported; absolute links and links escaping it are refused.
- Wardyn's own saved casts are ordinary files, not absolute symlinks.
- The standard-library API supplies traversal-resistant opening rather than a separate symlink check followed by an unsafe open.
- See the [Go API contract](https://pkg.go.dev/os#Root) and [Go's rooted-filesystem discussion](https://go.dev/blog/osroot). Wardyn already uses rooted filesystem operations for workspace writes.

## Remaining limits

- This is a read-path containment fix, not a redesign of the reduced-isolation mount.
- It does not prevent another sandbox from modifying other recordings in that same shared directory, make fallback data masked, reject every special file, or protect against an administrator replacing the configured root or installing a mount inside it.
- Hard-link and filesystem permissions remain OS boundaries; no universal filesystem-isolation claim is made.
- Prefer the default brokered PostgreSQL path over the shared-mount fallback.
