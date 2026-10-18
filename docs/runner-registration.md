# Register a runner

A runner holds its own Ed25519 key. Registration creates an **unclaimed** runner: it cannot receive a run or relay traffic until its owner confirms the fingerprint. The organisation must configure `WARDYN_RUNNER_ORG_URL` with its public HTTPS URL; HTTP is accepted only for loopback development.

From 0.9 the control plane places runs on a claimed runner through its owner's user pools ([Runner pools](operations/runner-pools.md)). The quickest path is [Set up a runner](#set-up-a-runner); the steps below are the same flow by hand.

## Requirements

| Requirement | Detail |
|---|---|
| Docker | A reachable engine for the person. `serve` exits at startup, with the reason, when it is not: a runner never shows Online and then refuses runs. |
| Native Linux Docker | Works with the defaults: the relay listens on the default bridge's gateway, and a run's proxy reaches it through Docker's host-gateway. |
| Docker Desktop (Windows through WSL2) | Works, and needs no relay flag: `setup` finds `127.0.0.1` and checks it. Verified. |
| Docker Desktop (macOS) | Needs no relay flag. Expected, not yet verified. |
| Rancher Desktop, dockerd (moby) engine, on macOS and on Windows through WSL2 | Expected, not yet verified. On macOS the proxy reaches a loopback relay at `192.168.5.2`; on WSL2 the relay listens on Rancher Desktop's `veth-rd-wsl` address (`192.168.143.2`). The startup check proves it on each computer. |
| Other VM-backed engines (Colima, Podman machine, OrbStack, rootless Docker) | Work when the startup reachability check passes; not verified by us. `--relay-host-target` is the knob when it does not. |
| Podman on Linux (docker-compat API) | Unsupported and not verified in 0.9.1: the startup check is expected to refuse it. |
| Rancher Desktop, containerd (nerdctl) mode | Unsupported: it has no Docker API. Switch Rancher Desktop to the dockerd (moby) engine. |
| `DOCKER_HOST` | `runnerd` follows `DOCKER_HOST`, else a selected docker context that is a local unix socket; a tcp, ssh or TLS context is refused. Point `DOCKER_HOST` at the engine's socket, for example `unix://$HOME/.rd/docker.sock` for Rancher Desktop (expected) or `unix://$HOME/.colima/default/docker.sock` for Colima (expected). |
| An explicit relay flag | `--relay-listen` accepts loopback on any engine, the native bridge gateway, or the `veth-rd-wsl` address. It never accepts `0.0.0.0` or another interface's address. The startup check still has to pass. |
| The relay's port | The port of the org's `WARDYN_CONTROL_PLANE_URL` (`8443` by default), not the org URL's. No control plane may listen on that loopback port on the same computer. |
| A privileged port | For a runner installed as a user service, the control-plane URL's port must be 1024 or higher. A user service cannot bind a port such as `:443`, so it cannot be relayed; this fails at the first run, not at the startup check. |
| Internal network | `serve` creates the `--internal-network` bridge (default `wardyn-internal`) when it is missing. |
| Architecture | The runner reports its engine's (`amd64` or `arm64`). Images are multi-arch indexes, or the organisation names an `@arm64` entry in `WARDYN_AGENT_IMAGES` and `WARDYN_PROXY_IMAGE_ARM64`; see [Multi-arch images](adoption/corp-image-authoring.md#multi-arch-images). |
| Windows | There is no native binary: run the runner inside WSL2 with Docker Desktop's WSL integration. |
| Permission | `POST /api/v1/me/runners/tokens` needs the [`self_host_runner`](operations/capabilities.md#self_host_runner) permission (`403` `capability_feature`, target `me.runners.tokens`: "hosting your own runner isn't available to you. Ask your admin for the self_host_runner permission."). Admins are never asked. |

- A Docker engine, or a relay address no proxy can reach, that fails the startup check is an error; systemd `Restart=on-failure` and launchd `KeepAlive` retry it.
- On macOS verify a download with `shasum -a 256 <file>`; on Linux, `sha256sum <file>`.
- On Docker Desktop, or any VM-backed Docker, pass `--total-disk <GB>` to `register` (free disk measures 0 there).

### The startup reachability check

`serve` and `setup` prove the relay address before they use it: on this engine, a throwaway container from the proxy image, on the `--internal-network` bridge, must connect to the relay's address (up to three tries per address pair, 0, 2 and 4 s apart). `setup` runs it before it spends the token when it already knows an image the engine has; otherwise (a development build without `--proxy-image`, or a release image this engine cannot pull) it runs after registering, with the image the organisation names. `serve` runs it before it shows Online.

| If the check | Then |
|---|---|
| Passes | `setup` prints the listen address and the target a proxy reaches it at, and pins both in the service settings (`WARDYN_RELAY_LISTEN` and `WARDYN_RELAY_HOST_TARGET`, even when the target is `host-gateway`) on a VM-backed engine. `serve` checks that one pair. |
| Fails | It exits with the engine's name, each address pair it tried and why it failed. Before registering, `setup` adds that no token was spent; after, that no new token is needed. The service is not installed. |

- `--relay-listen` / `WARDYN_RELAY_LISTEN` fixes where the relay listens, and `--relay-host-target` / `WARDYN_RELAY_HOST_TARGET` fixes the address a proxy dials: `host-gateway` or an IP address. A value you give is checked as given and never replaced by a guess.
- On a native engine with a firewall that drops container-to-host traffic (`firewalld`, `ufw`), the check fails; allow the bridge to reach the relay port.
- When `DOCKER_HOST` names an engine other than this computer's own dockerd (Rancher Desktop on WSL2 beside a distro-local dockerd), pass both flags: `--relay-listen 192.168.143.2 --relay-host-target 192.168.143.2`.
- The check pulls the proxy image first. A registry that needs a login: run `docker pull` on the image as yourself, and the runner uses the local copy.

## Register and claim by hand

1. Sign in under your personal account and run `wardyn runner token`. The token is printed once to stdout and expires after one hour. Save it in a private file for the runner.
2. On the runner host, run `wardyn-runnerd register --org https://wardyn.example.com --token-file ./registration-token --name my-laptop`. The runner generates its key locally and prints its fingerprint, what it measured and the limits it sent (see [Runner limits](#runner-limits)).
3. Under that same owner's personal account, run `wardyn runner claim --url https://wardyn.example.com --pool NAME --default`. The CLI reads the local runner identity and confirms its fingerprint. An administrator's personal account can claim its own runner; administrator credentials cannot claim for another person.

The claim runs with the person's own API token in `WARDYN_TOKEN`, or in *Your account → My runner* by pasting the fingerprint the runner printed. `--pool` puts the runner in your own pool of that name, made if you have none, and `--default` makes it your default; the console claim dialog has the same pool field and "Make it my default pool" box. A claimed runner in none of your pools runs nothing (`placement_unavailable`).

A refused claim is `403` `runner_claim_mismatch`. Once the owner matched, its message names the cause:

| Cause | Do |
|---|---|
| A different organisation URL than the control plane's now | Register again |
| The runner was revoked | Register again |
| Already claimed | Nothing; it is yours |
| The registration expired before the claim | Mint a new token and register again |
| The fingerprint does not match | Compare it with what the runner printed |

Another person's runner and an unknown id get one generic message, so a refusal never says whose runner it is.

An operator can mint for a named person with `wardyn runner token --owner <principal>`, with no `self_host_runner` grant needed. An email resolves to the person's principal; an unknown or ambiguous owner is refused `422` `runner_owner_invalid`. That token expires after 72 hours and still requires the named owner's fingerprint claim. Every token is single-use, including self-minted tokens. An unclaimed runner expires after 24 hours.

The default state directory is `wardyn/runner` beneath the operating system's user configuration directory. Pass the same `--state-dir` to registration, serving and claim when overriding it. The key and identity metadata are regular files with mode `0600` in a private directory. Loading refuses an unsafe key file, a changed fingerprint or a different organisation URL.

Registration never overwrites an existing identity. If registration fails after key creation, use a new private state directory. The error carries the server's reason; obtain a fresh token only when the server refused the token or had already used it. A revoked runner must register a new key and complete a new owner claim; reusing its old key does not reactivate it. A security admin revokes a runner with `DELETE /api/v1/runners/{id}`; its owner can revoke their own with `DELETE /api/v1/me/runners/{id}`, adding `?reason=not_mine` for a runner someone else registered with a token made for them. Revocation ends every run on the runner KILLED in the same transaction, then closes its live session on the replica that served the request, and within one 15-second ping interval on another replica; revoking a runner whose session already dropped succeeds (`200`, state `revoked`); migration `0151_runner_revoked_by` records who revoked it and why. Delete the single-use token file after registration.

The API doors are `POST /api/v1/me/runners/tokens`, operator-only `POST /api/v1/runners/tokens`, anonymous token redemption at `POST /api/v1/runners/register`, and owner-only `POST /api/v1/me/runners/{id}/claim`. Registration responses carry a runner id, fingerprint, state and organisation binding, never a human identity. Inventory and revocation use the separate runners management surface.

Migration `0142_runner_registration` adds hashed single-use registration tokens and organisation URL bindings to the existing runners table. It does not change run placement records.

Migration `0152_runner_pools` adds the runner pool catalogue: pools, their members, the use policies of organization pools and the organization defaults. It records no run binding, and a pool never claims or transfers a runner. Migration `0158_runner_pool_limits` adds the pool's `limits`: `limits` on create sets them and `limits` on update replaces the whole block (an omitted field, such as `max_concurrent_runs`, is removed and is then set by the pool's runners). [Runner pools](operations/runner-pools.md) says what a pool is and how its limits, barriers and queue apply to a run.

## Runner enablement

| Stored configuration | Admission |
|---|---|
| `runners` absent or `runners.enabled` false | Refused before registration, mint, or claim effects. |
| Site configuration unreadable | Refused; retry after configuration recovers. |
| `runners.enabled` true | Identity and route authorization checks still apply. |
| Generic `PUT /site-config` | Retains stored runner settings; cannot enable or disable runners. |

An administrator turns runners on at **Admin → Runners**, or with `PUT /api/v1/runners/settings {"enabled": true}`; only a super admin can, and each change is audited as `runners.enabled.set`. Turning on is refused until `WARDYN_RUNNER_ORG_URL` is HTTPS (`http` is accepted only for localhost) and `WARDYN_CONTROL_PLANE_URL` is HTTPS with no localhost exception, because the relay serves only on the internal TLS listener. Turning off is never refused. Off stops new registrations, claims and stream connections and ends every live runner session the replica that served the request holds. A session another replica holds stays open until the runner reconnects, but every sandbox create on a runner re-reads the switch first, so it gets no new work. `PUT /site-config` names `runners` only to echo the stored value: a different one is refused with `site_config_runners_via_own_route`.

## Runner inventory and unused tokens

| Route | Who | Answers |
|---|---|---|
| `GET /api/v1/runners?state=active\|revoked\|all&limit=&offset=` | admin or `security_admin` | Every person's runners, owner named; default `active` (unclaimed and claimed). `online` is the live session, and `usage` is what its runs hold now. |
| `GET /api/v1/runners/{id}` | admin or `security_admin` | One runner, revoked included. |
| `GET /api/v1/me/runners`, `GET /api/v1/me/runners/{id}` | the owner | The caller's own runners only; another person's runner and an absent id answer alike. An unclaimed runner's fingerprint is abbreviated here, so it cannot be copied from the console into the claim. |
| `GET /api/v1/runners/tokens?owner=&limit=&offset=` | admin or `security_admin` | Unused, unexpired registration tokens, optionally one person's. No token value is ever returned. |
| `DELETE /api/v1/runners/tokens/{id}` | admin or `security_admin` | The token stops being redeemable at once; audited as `runner.token.revoke`. Minting stays admin only. |
| `GET /api/v1/runners/settings` | admin or `security_admin` | `{"enabled": bool}`, readable while the runner routes refuse. |
| `PUT /api/v1/me/runners/{id}` | the owner | `{"limits": {...}}` declares the runner's limits; see [Runner limits](#runner-limits). |
| `GET /api/v1/me/runner-pools` | any signed-in person | The pools you may use (for an admin, without the oversight rows); accepts `run_type`. |

Both lists are paged and set `X-Wardyn-Truncated` when more exist. An unclaimed runner older than 24 hours is not listed, and the periodic sweep deletes it, freeing its key to register again. On a person's own list `minted_by_email` names the minter when someone else minted the token and an email is held. A runner another person revoked stays on its owner's list for seven days, with `revoked_by_email`; the owner's own revoke and older ones are hidden. Every view carries `machine_name`, the host name the runner reported at registration and again on each connect, the latest kept. It is runner-reported, never verified and never an identity; a name over 253 bytes or with a control character is refused at registration and not stored from a connect. `wardyn runner list` shows it as MACHINE. A view also carries `arch` (`amd64` or `arm64`), the CPU architecture of the runner's Docker engine, reported the same way and omitted until the runner reports one or when its engine could not be asked; `wardyn runner list` shows it as ARCH, and a value other than those two is refused at registration. A claimed runner's view carries `capacity`, `classes` and `allowed_roots` from its last `caps` event, and omits them until it has connected to the replica that answers. The mint answers (`POST /api/v1/runners/tokens`, `POST /api/v1/me/runners/tokens`) carry `org_url`, the URL `wardyn-runnerd register --org` must match, and `proxy_image`, the control plane's `WARDYN_PROXY_IMAGE` (omitted when unset), which a runner set up from that token should use; a token list never carries either. The runner views report what a runner said about itself as reported, never as verified.

`wardyn runner list` (shows ID, MACHINE, ARCH, LAST SEEN and FINGERPRINT beside name, kind, state and runs; `--all` for every person's runners; `--no-limits` for those with none declared), `wardyn runner revoke <runner>` (your own runner, by id or name) and `wardyn runner tokens list [--owner]|revoke` use these routes.

For an organization's runner a security operator runs `wardyn runner org-token mint --name <label>`, `wardyn runner claim-org <id> --fingerprint <fp>` and `wardyn runner trust <id> <class> --reason <why>`.

## Runner limits

Every runner declares its limits: Runs at once (`runs_at_once`, 1 to 64 for a self-hosted runner), a total all its runs at once may use (`total`) and what one run may use (`per_run`), each in CPU (`cpu_millis`, at least one CPU), memory (`memory_mib`) and disk (`disk_mib`, whole GiB). No per-run amount is past its total. A runner with none declared takes no run: a launch that could only go there is refused `runner_limits_unset`. Migration `0174_runner_limits` gives a runner registered before it none, so its owner sets them.

`wardyn-runnerd register` measures the computer: the CPUs and memory Docker reports, and the free disk of Docker's data root when the engine is local (not Docker Desktop). It proposes half of each as the total, and per run the total divided by Runs at once, at least 1 CPU and 1 GB. Runs at once defaults to half the CPUs. `--max-runs`, `--total-cpu`, `--total-memory`, `--total-disk`, `--per-run-cpu`, `--per-run-memory` and `--per-run-disk` (CPUs and GB) override any of it. A total past what the computer reports is refused, and a disk it cannot measure must be given, before the token is spent. At a terminal, `register` asks for each limit not given as a flag and offers the proposal, which Enter accepts; when its input or its output is not a terminal it asks nothing and registers with the proposal. The server refuses a registration without limits (`runner_limits_required`).

| Who | Can |
|---|---|
| The owner | Change them with `wardyn runner set <runner> [--max-runs N] [--total-cpu N --total-memory GB --total-disk GB] [--per-run-cpu N --per-run-memory GB --per-run-disk GB]` or `PUT /api/v1/me/runners/{id}` with the whole `limits` set; audited as `runner.update` with the set before and after. A total past the CPUs or memory the runner last reported is refused. |
| An administrator | Read them on `/runners`. No route changes another person's runner, since the machine is theirs. A security operator sets an organization's runner's limits with `PUT /api/v1/runners/{id}`, also audited as `runner.update`. |

- Every runner view carries `kind`, `limits` (`null` until set) and `usage`. `usage.in_use` counts the runs holding a sandbox on the runner and adds up what they reserve: the CPU and memory each was given and its main drive. `usage.available` is the total less that, and nothing while the runner is offline. `usage.reported` is what the runner last said it has; `usage.disk_capped` is false where its disk limits are counted, not enforced.
- Placement offers a run only to a runner whose per-run limit holds the run's size, and holds every run to the runner's Runs at once and totals. A create, a queue promotion, a queued run's start and a revive that starts an agent each count under the runner's row lock.
- A run whose runners all lack room waits in its pool's queue, with reason `runner_pool_runners_at_capacity`, whatever the pool's own cap.
- Lowering a limit ends no run. Any change lets waiting runs be looked at again at once.
- Each sandbox create carries Runs at once, and the runner refuses one past the runs it still holds itself. This backstop only catches a count the control plane got wrong.

Migration `0144_runner_minted_by` records on each runner who minted the registration token it redeemed, so the owner's list can tell a token they made from one an administrator made for them.

## Runner and organisation versions

- The runner and the organisation speak one wire protocol version, and a session is refused outright when they differ. No release so far has changed it, so a release does not by itself end a session.
- A newer feature is negotiated: the runner states what it serves in its `caps` event, and the organisation sends it only what the runner said. An older runner therefore keeps working with fewer features; for example it keeps every run's output, because `caps.retire` is absent and the organisation queues no `retire_run` for it.
- A runner also says whether its substrate keeps an ended sandbox (`caps.ends_unkept`, from 0.9.4): a pool with a Kubernetes runner in it offers no `main_drive.keep`. An older runner says nothing and is read as one that keeps.
- The organisation ignores a field in `caps` it does not know, but it refuses an event kind it does not know. A runner newer than its organisation can therefore be refused when it sends a kind the organisation lacks, and a new runner's proxy settings may not suit an older proxy image. Upgrade the organisation first, then its runners.
- After a runner is downgraded to a release without `caps.retire`, a `retire_run` that was already deferred for it stays idle until the runner is upgraded again. The run's output and spool are kept meanwhile.

## Serving

`wardyn-runnerd setup` starts `serve` for you. To run it by hand, build `wardyn-runnerd` with the `docker` tag, then connect the registered identity:

```sh
wardyn-runnerd serve --org https://wardyn.example \
  --proxy-image ghcr.io/cjohnstoniv/wardyn-proxy:<matching-version> \
  --internal-network wardyn-internal
```

The console prints the same command from the token mint answer, with its `proxy_image` unless the organisation keeps one proxy image per architecture, and a note on `--total-disk` for VM-backed Docker. The relay flags are needed only when the [startup check](#the-startup-reachability-check) refuses.

| Setting | Behaviour |
|---|---|
| `--state-dir` | Uses the registration directory; defaults to the user's private runner configuration directory. |
| `--proxy-image` / `WARDYN_PROXY_IMAGE` | The flag, then the variable, then the image the register answer named for this runner's architecture (kept in `runner.json`), then, on a release binary, `ghcr.io/cjohnstoniv/wardyn-proxy:<version>`. A development build with none of these must pass it. |
| `--relay-listen` / `WARDYN_RELAY_LISTEN` | Where the run relay listens, instead of the engine's default; any engine, loopback included. See [the startup check](#the-startup-reachability-check). |
| `--relay-host-target` / `WARDYN_RELAY_HOST_TARGET` | The address a run's proxy dials to reach the relay: `host-gateway` or an IP address. `setup` pins the one it proved on a VM-backed engine. |
| `--trusted-ca-file` / `WARDYN_TRUSTED_CA_FILE` | Extra roots for every run's proxy and sandbox, 64 KiB at most; `SSL_CERT_FILE` and `SSL_CERT_DIR` are not read. On a Kubernetes runner it and the organisation's bundle share one 128 KiB environment variable, and a run whose pair is bigger is refused at create. See [A corporate CA on a laptop runner](operations/hybrid-laptops.md#a-corporate-ca-on-a-laptop-runner). |
| `--internal-network` / `WARDYN_INTERNAL_NETWORK` | Local Docker bridge for proxy sidecars; defaults to `wardyn-internal` and is created when missing. |
| `--recording-store` / `WARDYN_RECORDING_STORE` | Organisation recording mode: `pg` or `fs` enables capture; `off` disables capture. Default `pg`; no local database is created. |

Identity and organisation binding are checked before connecting. Private lifecycle and output state survive reconnects. After restart, existing proxies are contained before new work is admitted. After a runner restarts, its runs stop reaching the network until each is revived. The first revive of such a run queues its recovery on the runner and asks you to revive it again once the runner has reconnected ([offline runner recovery](runner-offline.md)). Revocation stops reconnects while local containment continues until the daemon exits.

## Set up a runner

`wardyn-runnerd setup` is the one command: it registers this computer, creates the proxy bridge, installs the binary and a per-user service, and starts it. It is idempotent, so running it again is safe: an existing identity is never registered again, and the service files are rewritten and restarted.

The service is per user, not a root service: the runner identity sits in the person's own configuration directory, where `wardyn runner claim` looks for it. A root-run install for managed fleets needs a signed `.pkg`, which 0.9 does not ship.

| Who | Run |
|---|---|
| The person | `WARDYN_TOKEN=<your personal API token> wardyn-runnerd setup --org https://wardyn.example.com --token-file ./registration-token --claim --pool Laptop --default` |
| Device management (MDM) | Push the binary and run `wardyn-runnerd setup --org https://wardyn.example.com --token-file ./registration-token` without `--claim`. The person claims later in *Your account → My runner*, or with `wardyn runner claim --pool Laptop --default`. |

1. Download `wardyn-runnerd-<os>-<arch>` from the release, and verify it against the signed `SHA256SUMS` as described in [Verify the release assets](VERIFY.md#5-verify-the-release-assets), running `sha256sum -c --ignore-missing SHA256SUMS` since you hold one file.
2. Save the registration token in a private file. Mint it with `wardyn runner token`, or ask an administrator.
3. Run `chmod +x` on the download (a release download has no exec bit), then the command above as yourself, not as root. `install.sh --binary <path>` does both.
4. Without `--claim`, `setup` prints the key fingerprint and how to claim. With `--claim` and no `WARDYN_TOKEN`, it claims nothing and says so; the admin token cannot claim.

- `--claim` uses your own personal API token from `WARDYN_TOKEN`. `--pool NAME` needs `--claim` and `--default` needs `--pool`; both are refused before the token is spent.
- Setup checks the inputs, Docker and the user service manager, and proves with a probe container that a proxy on this engine reaches the relay ([the startup check](#the-startup-reachability-check)). With no image the engine has yet, that proof waits for the organisation's answer, after the token is spent; a refusal then needs no new token.
- It accepts `--name`, `--proxy-image`, `--relay-listen`, `--relay-host-target`, `--internal-network`, `--trusted-ca-file` and the limit flags of `register`. At a terminal it asks for each limit not given; with output sent to a file it asks nothing.
- A release binary uses `ghcr.io/cjohnstoniv/wardyn-proxy:<version>` without pinning it, so replacing the binary moves the proxy too. A named image is pinned and kept by a rerun. Installs from before 0.9.3 pinned the release image: run setup once after replacing the binary. A development build without `--proxy-image` uses the organisation's answer and refuses when there is none.
- Rolling a runner back to 0.9.2 after it ran anything under 0.9.3 needs its state directory cleared (0.9.3 records a captured-output flag for an ordinary run, which 0.9.2 refuses to read), so it registers and is claimed again; rolling back from a later release to 0.9.3 keeps its identity and run state, but let the runner reconnect until its audit spool is empty first: a spooled decision written by a later release may fail 0.9.3's integrity check.
- A claim that fails after the service started says "the runner is installed and running but not claimed into place" and names `wardyn runner claim --pool <name>` to finish.
- `deploy/runner/install.sh` is a thin wrapper: it finds a binary (`--binary`, beside the script, or on `PATH`), runs a private executable copy and passes every other argument to `setup`. It ships in `wardyn-runner-units.tar.gz`.

| System | Service | Settings | Logs |
|---|---|---|---|
| Linux | systemd user unit `wardyn-runnerd.service` | `~/.config/wardyn/runnerd.env`, mode `0600` | `journalctl --user -u wardyn-runnerd` |
| macOS | LaunchAgent `com.wardyn.runnerd`, from login | the agent's `EnvironmentVariables` | `~/Library/Logs/wardyn/runnerd.log` |

Both restart `serve` after a crash. On Linux the unit runs while you are logged in; run `loginctl enable-linger $USER` to keep it running after logout and from boot. Setup prints that command and never runs it. A `DOCKER_HOST` set when you run setup is carried into the service.

`./install.sh --uninstall` stops the service and removes the unit or agent, the settings file and the installed binary. It keeps `runner.json` and `runner.key`. Revoke the runner itself in *My runner*.
