> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# SSH gateway in a cluster

- Nothing in [SSH.md](../SSH.md) changes when wardynd runs in Kubernetes — the gateway is the same listener, and the client commands are identical.
- What the operator owes is a route to it and an address to advertise.

## The four pieces

- Four pieces, all documented
  where they are implemented:
  - **Getting traffic in.** `ssh.enabled` adds the SSH port to wardynd's
    *existing* Service, so by default SSH inherits whatever exposure HTTP has.
  - `make kind-quickstart` publishes it as a NodePort and prints the ready-made
    `ssh -p 2222 <run-id>@127.0.0.1` line — POC-grade, single command
    ([chart README, Quickstart](../../deploy/helm/wardyn/README.md#quickstart)).
  - To expose SSH differently from the console — its own `LoadBalancer` while
    HTTP stays internal `ClusterIP` — the chart README carries a minimal
    bring-your-own Service targeting the same pods
    ([Split SSH exposure](../../deploy/helm/wardyn/README.md#split-ssh-exposure)).
  - **The address clients are told to use.** `ssh.advertiseHost`
    (`WARDYN_SSH_ADVERTISE`) is what the run-detail card and `/healthz` print.
  - It is advisory copy only — the gateway binds `WARDYN_SSH_LISTEN`, not this
    — but in a cluster the bind and the reachable address always differ.
  - So an
    unset value hands every user a `127.0.0.1` that is not theirs.
  - The chart
    never guesses it; see
    [`values.yaml`'s `ssh` block](../../deploy/helm/wardyn/values.yaml) and
    [ENV.md](../ENV.md).
  - **The host key across pod churn.** There is no host key in the chart and no
    volume for one: wardynd generates an ed25519 key on first boot and persists
    it in the secret store.
  - So the fingerprint your users pinned survives a
    rolling upgrade — *as long as the age key does*.
  - Lose the age key and the
    pod crash-loops before the gateway listens, which is a connection refused
    rather than a silently changed fingerprint.
  - See OPERATIONS,
    ["The SSH host key survives restarts"](../OPERATIONS.md#the-ssh-host-key-survives-restarts--because-the-age-key-does).
  - **Caveat, unchanged by the substrate.** `sftp` and `-L` exec binaries inside
    the *sandbox*, not in wardynd's pod, so a BYOI run still needs
    `sftp-server`/`socat` — see [Image contract](../SSH.md#image-contract-byoi).
  - A
    cluster install does not supply them on the image's behalf.
