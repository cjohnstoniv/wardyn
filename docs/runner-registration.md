# Register a runner

A runner holds its own Ed25519 key. Registration creates an **unclaimed** runner: it cannot receive a run or relay traffic until its owner confirms the fingerprint. The organisation must configure `WARDYN_RUNNER_ORG_URL` with its public HTTPS URL; HTTP is accepted only for loopback development.

1. Sign in under your personal account and run `wardyn runner token`. The token is printed once to stdout and expires after one hour. Save it in a private file for the runner.
2. On the runner host, run `wardyn-runnerd register --org https://wardyn.example.com --token-file ./registration-token --name my-laptop`. The runner generates its key locally and prints its fingerprint and its runs at once (see [Runs at once](#runs-at-once)).
3. Under that same owner's personal account, run `wardyn runner claim --url https://wardyn.example.com`. The CLI reads the local runner identity and confirms its fingerprint. An administrator's personal account can claim its own runner; administrator credentials cannot claim for another person.

An operator can mint for a named person with `wardyn runner token --owner <principal>`. That token expires after 72 hours and still requires the named owner's fingerprint claim. Every token is single-use, including self-minted tokens. An unclaimed runner expires after 24 hours.

The default state directory is `wardyn/runner` beneath the operating system's user configuration directory. Pass the same `--state-dir` to registration, serving and claim when overriding it. The key and identity metadata are regular files with mode `0600` in a private directory. Loading refuses an unsafe key file, a changed fingerprint or a different organisation URL.

Registration never overwrites an existing identity. If registration fails after key creation, use a new private state directory. The error carries the server's reason; obtain a fresh token only when the server refused the token or had already used it. A revoked runner must register a new key and complete a new owner claim; reusing its old key does not reactivate it. A security admin revokes a runner with `DELETE /api/v1/runners/{id}`; its owner can revoke their own with `DELETE /api/v1/me/runners/{id}`, adding `?reason=not_mine` for a runner someone else registered with a token made for them. Revocation ends every run on the runner KILLED in the same transaction, then closes its live session, within one 15-second ping interval when another server replica holds it; migration `0151_runner_revoked_by` records who revoked it and why. Delete the single-use token file after registration.

The API doors are `POST /api/v1/me/runners/tokens`, operator-only `POST /api/v1/runners/tokens`, anonymous token redemption at `POST /api/v1/runners/register`, and owner-only `POST /api/v1/me/runners/{id}/claim`. Registration responses carry a runner id, fingerprint, state and organisation binding, never a human identity. Inventory and revocation use the separate runners management surface.

Migration `0142_runner_registration` adds hashed single-use registration tokens and organisation URL bindings to the existing runners table. It does not change run placement records.

Migration `0152_runner_pools` adds the runner pool catalogue: pools, their members, the use policies of organization pools and the organization defaults. It records no run binding, and a pool never claims or transfers a runner. Migration `0158_runner_pool_limits` adds the pool's `limits`: an administrator sets them with `limits` on create and replaces the whole block with `limits` on update (an omitted field, such as `max_concurrent_runs`, is removed); a pool with none adds no bound of its own. A run is admitted by its pool's run types and barriers, and the pool's lifetime bounds its end; CPU, memory, idle stop and the concurrent-run cap are stored and audited but not yet applied to runs.

## Runner enablement

| Stored configuration | Admission |
|---|---|
| `runners` absent or `runners.enabled` false | Refused before registration, mint, or claim effects. |
| Site configuration unreadable | Refused; retry after configuration recovers. |
| `runners.enabled` true | Identity and route authorization checks still apply. |
| Generic `PUT /site-config` | Retains stored runner settings; cannot enable or disable runners. |

An administrator turns runners on at **Admin → Runners**, or with `PUT /api/v1/runners/settings {"enabled": true}`; only a super admin can, and each change is audited as `runners.enabled.set`. Turning on is refused until `WARDYN_RUNNER_ORG_URL` (and the control-plane URL) is HTTPS; `http` is accepted only for localhost. Turning off is never refused. Off stops new registrations, claims and stream connections and ends every live runner session the serving replica holds. A session another replica holds stays open until the runner reconnects, but every sandbox create on a runner re-reads the switch first, so it gets no new work. `PUT /site-config` names `runners` only to echo the stored value: a different one is refused with `site_config_runners_via_own_route`.

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

Both lists are paged and set `X-Wardyn-Truncated` when more exist. An unclaimed runner older than 24 hours is not listed, and the periodic sweep deletes it, freeing its key to register again. On a person's own list `minted_by_email` names the minter when someone else minted the token and an email is held. A runner another person revoked stays on its owner's list for seven days, with `revoked_by_email`; the owner's own revoke and older ones are hidden. Every view carries `machine_name`, the host name the runner reported at registration and again on each connect, the latest kept. It is runner-reported, never verified and never an identity; a name over 253 bytes or with a control character is refused at registration and not stored from a connect. `wardyn runner list` shows it as MACHINE. A claimed runner's view carries `capacity`, `classes` and `allowed_roots` from its last `caps` event, and omits them until it has connected to the replica that answers. The mint answers (`POST /api/v1/runners/tokens`, `POST /api/v1/me/runners/tokens`) carry `org_url`, the URL `wardyn-runnerd register --org` must match; a token list never does. The runner views report what a runner said about itself as reported, never as verified.

`wardyn runner list` (`--all` for every person's runners) and `wardyn runner tokens list [--owner]|revoke` use these routes.

## Runner limits

Every runner declares its limits: Runs at once (`runs_at_once`, 1 to 64 for a self-hosted runner), a total all its runs at once may use (`total`) and what one run may use (`per_run`), each in CPU (`cpu_millis`, at least one CPU), memory (`memory_mib`) and disk (`disk_mib`, whole GiB). No per-run amount is past its total. A runner with none declared takes no run: a launch that could only go there is refused `runner_limits_unset`. Migration `0174_runner_limits` gives a runner registered before it none, so its owner sets them.

`wardyn-runnerd register` measures the computer: the CPUs and memory Docker reports, and the free disk of Docker's data root when the engine is local (not Docker Desktop). It proposes half of each as the total, and per run the total divided by Runs at once, at least 1 CPU and 1 GB. Runs at once defaults to half the CPUs. `--max-runs`, `--total-cpu`, `--total-memory`, `--total-disk`, `--per-run-cpu`, `--per-run-memory` and `--per-run-disk` (CPUs and GB) override any of it. A total past what the computer reports is refused, and a disk it cannot measure must be given, before the token is spent. The server refuses a registration without limits (`runner_limits_required`).

| Who | Can |
|---|---|
| The owner | Change them with `wardyn runner set <runner> [--max-runs N] [--total-cpu N --total-memory GB --total-disk GB] [--per-run-cpu N --per-run-memory GB --per-run-disk GB]` or `PUT /api/v1/me/runners/{id}` with the whole `limits` set; audited as `runner.update` with the set before and after. A total past the CPUs or memory the runner last reported is refused. |
| An administrator | Read them on `/runners`. No route changes another person's runner, since the machine is theirs. |

- Every runner view carries `kind`, `limits` (`null` until set) and `usage`. `usage.in_use` counts the runs holding a sandbox on the runner and adds up what they reserve: the CPU and memory each was given and its main drive. `usage.available` is the total less that, and nothing while the runner is offline. `usage.reported` is what the runner last said it has; `usage.disk_capped` is false where its disk limits are counted, not enforced.
- Placement offers a run only to a runner whose per-run limit holds the run's size, and holds every run to the runner's Runs at once and totals. A create, a queue promotion, a queued run's start and a revive that starts an agent each count under the runner's row lock.
- A run whose runners all lack room waits in its pool's queue, with reason `runner_pool_runners_at_capacity`, whatever the pool's own cap.
- Lowering a limit ends no run. Any change lets waiting runs be looked at again at once.
- Each sandbox create carries Runs at once, and the runner refuses one past the runs it still holds itself. This backstop only catches a count the control plane got wrong.

Migration `0144_runner_minted_by` records on each runner who minted the registration token it redeemed, so the owner's list can tell a token they made from one an administrator made for them.

## Serving

Build `wardyn-runnerd` with the `docker` tag, then connect the registered identity:

```sh
wardyn-runnerd serve --org https://wardyn.example \
  --proxy-image ghcr.io/cjohnstoniv/wardyn-proxy:<matching-version> \
  --internal-network wardyn-internal
```

| Setting | Behaviour |
|---|---|
| `--state-dir` | Uses the registration directory; defaults to the user's private runner configuration directory. |
| `--proxy-image` / `WARDYN_PROXY_IMAGE` | Required; use the proxy image built with this daemon version. |
| `--internal-network` / `WARDYN_INTERNAL_NETWORK` | Existing local Docker bridge for proxy sidecars; defaults to `wardyn-internal`. |
| `--recording-store` / `WARDYN_RECORDING_STORE` | Organisation recording mode: `pg` or `fs` enables capture; `off` disables capture. Default `pg`; no local database is created. |

Identity and organisation binding are checked before connecting. Private lifecycle and output state survive reconnects. After restart, existing proxies are contained before new work is admitted. Revocation stops reconnects while local containment continues until the daemon exits.

## Run as a service

`deploy/runner/install.sh` installs `wardyn-runnerd` for one person, as that person, and keeps `wardyn-runnerd serve` running. The service is per user, not a root service: the runner identity sits in the person's own configuration directory, where `wardyn runner claim` looks for it. A root-run install for managed fleets needs a signed `.pkg`, which 0.9 does not ship.

1. Download `wardyn-runnerd-<os>-<arch>` and `wardyn-runner-units.tar.gz` from the release, and verify them against the signed `SHA256SUMS` as described in [Verify the release assets](VERIFY.md#5-verify-the-release-assets), running `sha256sum -c --ignore-missing SHA256SUMS` since you hold two files.
2. Run `tar -xzf wardyn-runner-units.tar.gz`, which unpacks to `runner/`, then pass the binary with `--binary ./wardyn-runnerd-<os>-<arch>`. It needs no exec bit.
3. Save the registration token in a private file and run `./runner/install.sh --org https://wardyn.example.com --binary ./wardyn-runnerd-<os>-<arch> --token-file ./registration-token` as yourself.
4. Run `wardyn runner claim`, as in [Register a runner](#register-a-runner).

The installer does these steps in order and stops at the first refusal:

- It refuses to run as root, and requires `docker info` to succeed for the person.
- It copies the binary to `~/.local/share/wardyn/bin` on Linux or `~/Library/Application Support/wardyn/bin` on macOS.
- It sets the proxy image to `ghcr.io/cjohnstoniv/wardyn-proxy:<version>` for a release binary. A development build refuses unless `--proxy-image` is given.
- It creates the `wardyn-internal` bridge, or the one named by `--internal-network`, when it is missing.
- It registers the runner with `--token-file` when no identity exists, and never registers an existing identity again.
- It installs and starts the service.

| System | Service | Settings | Logs |
|---|---|---|---|
| Linux | systemd user unit `wardyn-runnerd.service` | `~/.config/wardyn/runnerd.env`, mode `0600` | `journalctl --user -u wardyn-runnerd` |
| macOS | LaunchAgent `com.wardyn.runnerd`, from login | the agent's `EnvironmentVariables` | `~/Library/Logs/wardyn/runnerd.log` |

Both restart `serve` after a crash. On Linux the unit runs while you are logged in; run `loginctl enable-linger $USER` to keep it running after logout and from boot. The installer prints that command and never runs it. A `DOCKER_HOST` set when you run the installer is carried into the service.

`./install.sh --uninstall` stops the service and removes the unit or agent, the settings file and the installed binary. It keeps `runner.json` and `runner.key`. Revoke the runner itself in *My runner*.
