# Filesystem recording read confinement

Commit: `18fb91fdb6a3e656eed2a922ea73ddd22998ef3f`, branch
`review/0.7x-recording-root-read`, independently based on `dfa89f60`, signed off.
This follow-up is NOT in the frozen 15-patch batch at `0d63ab2e`.

## Scope and preconditions

`FSStore.OpenCast` and `StatAndTail` validated the key lexically, then used
`os.Open`, which could follow a symlink outside the recording directory. The
supported Docker recording-mount fallback shares writable storage with the
sandbox (`agentMounts`, `RecordingMount`). In that configuration, a caller able
to place a cast symlink and replay that run can cause the daemon to read an
unrelated daemon-readable file. The route's owner/operator authorization does
not confine the underlying filesystem open. The default PostgreSQL store does
not use this path; configuring filesystem storage without an untrusted writer
does not by itself supply the attacker precondition.

This exceeds the fallback's already-disclosed lack of cross-run isolation and
unmasked-recording residual: the target need not be a recording at all. No live
deployment, real credential, or non-test target was accessed during verification.

## Minimal correction

Both read sites use `os.OpenInRoot` with the existing validated basename. There
is no new dependency, interface, storage layout, mount, policy or default.
Regular saves/replacements and missing-recording error mapping are unchanged.
Relative links contained inside the recording directory remain supported;
absolute links and links escaping it are refused. Wardyn's own saved casts are
ordinary files, not absolute symlinks.

The standard-library API supplies traversal-resistant opening rather than a
separate symlink check followed by an unsafe open. See the [Go API contract](https://pkg.go.dev/os#OpenInRoot)
and [Go's rooted-filesystem discussion](https://go.dev/blog/osroot). Wardyn already
requires Go 1.26 and uses rooted filesystem operations for workspace writes.

## Executed checks

- Baseline regression: before production edits, four absolute/relative and
  bare/named-key cases failed (exit 1). `OpenCast` accepted each link,
  `StatAndTail` returned the synthetic fixture, and the recording handler
  returned HTTP 200 with the fixture. All paths were under `t.TempDir()`.
- Fixed regression plus compatibility cases: exit 0, 0.008 seconds.
- Filesystem tests including shared store conformance under race detection:
  exit 0, 1.027 seconds.
- Complete recording package under race detection: exit 0, 1.044 seconds.
  The first sandboxed attempt could not bind the existing loopback HTTP test
  server; the permission-approved rerun passed. No assertion failure was hidden.
- `go vet ./internal/recording`, file-size guard and diff whitespace check passed.

Independent lifecycle-lane peer review found no blocking issue and confirmed the
compatibility/residual boundaries below. The security lane independently checked
the writable-mount and replay preconditions. The router fixture deliberately
uses an allow-all authorizer to isolate storage behavior; it does not constitute
full authentication or cross-user end-to-end acceptance. Full-tree checks are
complete: `go test ./...` passed (exit 0) on `18fb91fd`, including the daemon's
guard tests, with a clean worktree before/after. Log:
`evidence/recording-root-full-tree.log`, SHA-256
`d5510e188433757a0678f79372048dba065065ecfc93af1cca5ef6556bdb3b17`.
The frozen batch's earlier make-ci/browser results do not certify this later
patch; run the complete merge gate on its eventual release combination.

## Remaining limits

This is a read-path containment fix, not a redesign of the reduced-isolation
mount. It does not prevent another sandbox from modifying other recordings in
that same shared directory, make fallback data masked, reject every special
file, or protect against an administrator replacing the configured root or
installing a mount inside it. Hard-link and filesystem permissions remain OS
boundaries; no universal filesystem-isolation claim is made. Prefer the default
brokered PostgreSQL path over the shared-mount fallback.
