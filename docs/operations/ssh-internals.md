> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# SSH gateway internals

- Registered keys live in `ssh_public_keys` (migration `0033`), keyed by the SHA256 fingerprint (computed server-side from the parsed key — a caller cannot choose or forge one).

## Where the code is

- The gateway and the `/me/ssh-keys` REST surface
  are both in `internal/api` ([`sshgateway.go`](../../internal/api/sshgateway.go), [`sshgateway_channels.go`](../../internal/api/sshgateway_channels.go),
  [`sshkeys.go`](../../internal/api/sshkeys.go)), next to [`attach.go`](../../internal/api/attach.go).
- The SSH shell path bridges through the
  exact same `Runner.Attach` call and masked recorder the browser terminal
  uses, so there is only one interactive-shell code path to keep correct, not
  two that could drift.
