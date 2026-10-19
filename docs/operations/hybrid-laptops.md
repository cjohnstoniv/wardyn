> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Managed laptops: client-mode runners

- In 0.9 a laptop joins an org as a **client-mode runner**: it runs `wardyn-runnerd`, not a control plane, and the person who owns it claims it.
- A run is placed on it through one of its owner's user pools: see [Runner pools](runner-pools.md).
- Set it up with `wardyn-runnerd setup`, one command that installs the per-user service: see [Set up a runner](../runner-registration.md#set-up-a-runner).
- A cluster can be an organisation's runner instead of a laptop: see [Organisation runners in a Kubernetes cluster](k8s-runners.md).
- Design: [docs/design/0.9/PLAN.md](../design/0.9/PLAN.md). Org enrolment of a full daemon (`WARDYN_ORG_URL`) still works in 0.9, logs a deprecation warning at boot, and is removed in 1.0; see [Legacy org enrolment](#legacy-org-enrolment-deprecated).

## Where each thing lives

| Concern | Org | Laptop |
| --- | --- | --- |
| Scheduling, placement, the ceiling | Yes, the only scheduler | Nothing decides locally |
| Credentials | Held in the org; the laptop can use a `via_org` credential for a run's life | The person's own, and `runner_resident` values of four classes (`env_secret`, `file_secret`, `ssh_key`, `git_pat_helper`), held in the runner's OS keystore for the run |
| A `via_org` credential's requests | The org re-checks each one and injects the credential on a TLS leg it dials itself | The proxy forwards them through the relay; it never holds the value, but sees the request in plaintext |
| Audit | The org is the only writer; every local run is an org run record | The runner carries bytes and spools rows while offline |
| Control plane, Postgres, OIDC client, `age.key` | Yes | None of them |
| Sandbox, proxy sidecar, local Docker | No | Yes, driven through `wardyn-runnerd` |
| The runner's private key | Never seen | `runner.key`, `0600`, never leaves the host |

- Azure DevOps: connected at the org; on a local run only the person's own PAT is used directly, see [Azure DevOps on a laptop runner](../AZURE-DEVOPS.md#on-a-laptop-runner-local-placement).
- The run page and the [`run.create` audit row](../AUDIT-ACTIONS.md) name the placement, pool and runner a run ran on; a run from before 0.9.1 reads "Not recorded".
- A provider sign-in is never placed on a self-hosted runner, and sandbox creation refuses one bound to a runner.

## Requirements

- `setup` and `serve` find the relay address and prove it with a probe container on Docker Desktop, Rancher Desktop (dockerd) and other VM-backed engines; Rancher Desktop's containerd mode is unsupported. See [Requirements](../runner-registration.md#requirements). There is no native Windows binary.
- The default laptop storage driver (overlayfs on Docker Desktop, Colima and Rancher Desktop) cannot cap a run's disk, so `disk_mib` is not enforced there; `setup` says so.
- On a VM-backed engine `setup` measures the VM's free disk with the proxy image (`wardyn-proxy -disk-free /`). `--total-disk` overrides it, and is needed when the probe cannot run.
- The full list, and why `serve` refuses to start: [Requirements](../runner-registration.md#requirements).

## Resident credentials and posture

> [!NOTE]
> A runner holds four classes as `runner_resident` in its OS keystore: `env_secret`, `file_secret`, `ssh_key` and `git_pat_helper`. Its proxy holds a fifth, `ado_minted_pat`, in memory only. Any other class set to `runner_resident` is still refused.
> A refusal is `runner_posture_unmet` when the run's confinement floor or the runner's posture misses the class's requirement, otherwise `placement_credential`. Whoever controls the laptop as root can read a value while the runner holds it, so erasure is best-effort and never promised.

| Credential class | Modes |
| --- | --- |
| `env_secret`, `file_secret`, `ssh_key`, `git_pat_helper`, `bedrock_role_credentials` | `runner_resident` or `refuse` |
| `api_key`, `bedrock_bearer`, `aws_sso_bearer`, `azure_foundry_token`, `git_pat_broker`, `ado_minted_pat` | `via_org`, `runner_resident` or `refuse` |
| `github_token` | `via_org` or `refuse` |
| `cloud_sts` | `refuse` only |
| `oauth_subscription` | `own` only: the person's own sign-in |

- `via_org` does not carry an MCP server's header credential: a run whose MCP server uses one the organisation shares is refused on your runner (`placement_credential`).
- An admin can set a credential class to `runner_resident` with a `require_posture` (`mdm_managed`, `disk_encrypted`, `os_min`).
- A claimed laptop runner's trust class is `personal`. A security operator can mark it `managed_device` (`wardyn runner trust <runner-id> managed_device --reason "<why>"`, held for a second person under four-eyes); the runner itself never sets it.
- The delivery policy's `by_trust` sets a class's mode for one trust class, so `runner_resident` can be limited to `managed_device` runners. `via_org` is never set per trust class.
- The runner reports its posture when it connects and again when it changes. A root-owned posture file from your MDM wins over the runner's own probe: `/etc/wardyn/posture.json` on Linux and `/Library/Application Support/Wardyn/posture.json` on macOS.
- A runner keeps a delivered value in the OS keystore for the run (Secret Service; on macOS the Keychain, written over stdin) and deletes it at run end, reporting each delete (`runner.resident.erase`). On a computer whose root user is the developer, erasure is best-effort, never promised. A runner with no keystore refuses (`runner_keystore_unavailable`).
- **When the keystore copy goes.** The cap is the earliest of the run's lifetime cap, the run's end and 24 hours after delivery.
  - **From the cap on, nothing can read the value.** The runner refuses to read it, a revive is refused, and the proxy stops answering for an SSH key or git token.
  - **Deleting the bytes is best-effort, with retry.** Normally the runner deletes the copy when the run ends, or at its next minute sweep after the cap. In four cases the delete lands later:
    1. a keychain call is busy or stuck at the cap (for example, a macOS keychain prompt waiting for a click). The copy is deleted at the first sweep after the call returns;
    2. the delete keeps failing. It is retried every minute;
    3. the runner daemon is stopped at the cap. The copy is deleted when it next starts;
    4. the runner is offline when the run ends. The copy is deleted when it reconnects, or at the cap, whichever comes first.
  - Wardyn cannot guarantee that bytes are physically gone from a computer the developer controls.
- A value already inside the sandbox or the running proxy stays there until the run ends.
- A key or token already handed to the sandbox stays valid at the forge until you rotate it.
- On macOS a value over about 3 KB is refused.
- A runner older than 0.9.3 is refused a run that needs a resident credential (`placement_credential`).
- A grant that requires approval is never `runner_resident`.
- `git_pat_helper` applies only with `WARDYN_GIT_PAT_BROKER=off`. Turning the broker on refuses new runs, dispatches and revives that would need one.
- A git token for an Azure DevOps host is never `runner_resident` (`placement_credential`).
- `ado_minted_pat` also needs the connection's opt-in and a person's own runner. The runner's proxy fetches the per-run token from the organisation at most 15 minutes apart; a refused fetch revokes it ([Azure DevOps on a laptop runner](../AZURE-DEVOPS.md#on-a-laptop-runner-local-placement)).
- A `require_posture` class is refused at launch until per-runner placement lands.
- On Linux the Secret Service needs a login session bus (`DBUS_SESSION_BUS_ADDRESS`, or the user bus socket under `/run/user/<uid>`), so a system service with no login session has no keystore.
- Posture is self-reported, and a run with a CC3 floor never takes a resident credential.
- Placement judges the posture on record, the runner's last report, however old it is. A change on the laptop counts once the runner reports it, which it does at every connect and within 15 minutes of a change.
- A runner that has not reported posture misses every `require_posture` (`runner_posture_unmet`). A class with no `require_posture` places on any runner, one that has never reported posture included; to demand a report, set at least one requirement.
- `os_min` must name every OS a runner may run: a runner on an OS it leaves out misses it.

### bedrock_sso on a laptop runner

- A `bedrock_sso` model provider works on your own runner with AWS SSO proxy-inject on (`WARDYN_AWS_SSO_PROXY_INJECT`, the default).
- The sandbox gets a placeholder `~/.aws`; the proxy injects your own AWS session at the run's `portal.sso` host, relayed to your runner as your own credential.
- Only a `credential_source: per_user` session qualifies. A shared AWS session is refused (`placement_credential`).
- With proxy-inject off the sandbox would hold your real SSO token cache. A local `bedrock_sso` run is then refused at Review, preflight and create (`placement_credential`, naming `SandboxSpec.SecretEnv[WARDYN_AWS_SSO_CONFIG_B64]`), before Launch.
- With proxy-inject on, `WARDYN_AWS_SSO_CONFIG_B64` is plain container env. `docker inspect` and a k8s pod spec show the SSO start URL, region, account id and role name, but no token or client secret.
- The short-lived role credentials the SDK mints from it are still resident in the sandbox, because SigV4 signs in-process.
- The provider's sign-in and renewal: [AWS SSO per person](../OPERATIONS.md#aws-sso-per-person).

### Private endpoints and `internal_hosts`

- The organisation's [`internal_hosts`](../OPERATIONS.md#network-upstream-proxy-and-egress-redirects) declarations reach a laptop runner's proxy as a cluster's. A host that resolves to a private address (PrivateLink, a zero-trust client's `100.64.0.0/10` range) is reached when declared there. Its decision row reads `rule_source: site-config:internal-host`; one an egress approval released reads `approval:<id>`.
- A declaration lifts only the private-IP guard for that suffix. The egress allowlist still decides, and loopback, link-local and metadata addresses stay closed.
- On a laptop the sandbox resolves through the person's network, so declare `cidrs` on each entry a laptop estate uses. An entry without them lifts the whole private range for that suffix.
- The organisation's upstream proxy, trusted CA, `upstream_proxy_no_proxy` and LLM upstream overrides are still not sent to a laptop's proxy. The runner's own CA bundle takes the trusted CA's place: see below.
- A key or `custom_endpoint` provider with a base URL (a gateway) needs those LLM upstream overrides, so any harness using one is refused on your own runner at Review, preflight and create (`placement_credential`, naming `ProxyConfig.LLMUpstreams`).

### A corporate CA on a laptop runner

- Behind a TLS-inspecting proxy, give the runner your organisation's roots. Every run's proxy and sandbox then trust them beside the system roots and the run's own interception CA.
- Certificates only, your organisation's roots, 64 KiB at most: a merged system bundle is refused.
- Only `--trusted-ca-file` or `WARDYN_TRUSTED_CA_FILE` (what setup writes) is read, never `SSL_CERT_FILE` or `SSL_CERT_DIR`.
- When an organisation's own CA is also staged and the two pass 64 KiB, agent images older than 0.9.3 trust only the organisation's roots.

The bundle is read when `serve` starts; a bad file stops it and is named.

1. Export the roots. On macOS (one `-c` per root):

   ```sh
   security find-certificate -a -p -c "Example Corp Root CA" /Library/Keychains/System.keychain \
     > "$HOME/Library/Application Support/wardyn/corp-ca.pem"
   ```

   On Linux:

   ```sh
   mkdir -p ~/.config/wardyn
   cat /usr/local/share/ca-certificates/*.crt > ~/.config/wardyn/corp-ca.pem      # Debian, Ubuntu
   cat /etc/pki/ca-trust/source/anchors/*.pem > ~/.config/wardyn/corp-ca.pem      # Fedora, RHEL
   ```

2. Run setup with the file. It checks the file before the token is spent; a rerun keeps it. If the kept file has moved, rerun with `--trusted-ca-file <new file>`.

   ```sh
   wardyn-runnerd setup --org https://wardyn.example --trusted-ca-file ~/.config/wardyn/corp-ca.pem
   ```

3. If your shell's `SSL_CERT_FILE` already names the roots, setup says so; pass it with `--trusted-ca-file "$SSL_CERT_FILE"`.

### Session recordings

**Recording defaults to `preferred`: a runner that does not record (started with `--recording-store off`, or older than 0.9.3) still takes runs, and they run unrecorded.** To refuse those runs instead, an admin sets `"recording": "required"` on the organisation's defaults (`PUT /api/v1/runner-pool-defaults`) or on a pool (`PUT /api/v1/runner-pools/{id}`). Read the organisation's defaults first: that `PUT` replaces the whole document, so a write that leaves `pool_id` or `barrier` out clears it (leaving `recording` out keeps it).

- `wardyn-rec` uploads a laptop run's cast to `wardyn-proxy:3128`, as on a cluster. The proxy's in-cluster control-plane name maps to the runner's relay by design, which carries the upload to the organisation bound to the run's own runner.
- The organisation decides per run, at dispatch. With its recording store off (`WARDYN_RECORDING_STORE=off`) the run page says `org_off`.
- `wardyn-runnerd serve --recording-store off` is the runner's ceiling: it never records a run's terminal session. Otherwise the runner records the runs its organisation asks it to; a runner older than 0.9.3 records by its own setting, and counts as not recording.
- The organisation, not the runner, records console and SSH sessions opened through it, even on a run marked not recorded, while it has a recording store.
- Under `required`, a run only a non-recording runner could take is refused `runner_recording_off`, and with the store off every run is refused `recording_store_off`, as is saving `required`. A defaults write that leaves `recording` out keeps it.
- Under `preferred` the page says `runner_declined`. A cast from a recording runner older than 0.9.3 clears it; any other runner's upload is refused `409` `recording_off`.
- An upload with no store is answered `409` `recording_off`, a reason the `brokered:recording` row and `wardyn-rec`'s log carry (migration `0180_run_recording`).

### A corporate upstream proxy on a laptop runner

- Where the only way out is your organisation's HTTP proxy, give the runner its URL. Every run's proxy then chains its egress through it.
- The organisation's own upstream proxy is not sent to a laptop; this is the laptop's own.
- Start `serve` with `--upstream-proxy http://proxy.example:3128` or `WARDYN_UPSTREAM_PROXY`. Only `http://` is supported. The URL is checked at startup, never sent to the organisation, and never printed.
- Setup does not write it. Add it to the service's environment after setup (the env file on Linux, mode `0600`), and again after a rerun, which rewrites that file.
- Put a credential in the variable, not the flag: a flag shows in the process list.
- Its own organisation connection needs `HTTPS_PROXY`: [a runner behind a corporate proxy](../runner-registration.md#a-runner-behind-a-corporate-proxy).
- On macOS do not put a credential in the LaunchAgent: the plist is mode `0644`, readable by every local account. Use a proxy URL without one, or run with `--no-service` and a `0600` environment file that holds `WARDYN_UPSTREAM_PROXY`. See [`WARDYN_UPSTREAM_PROXY`](../ENV.md#wardyn_upstream_proxy).

### Running without a service

- `wardyn-runnerd setup --no-service` registers, installs the binary and writes the environment file, then prints the command that runs the runner. It installs no systemd unit or LaunchAgent and runs no service command.
- Use it where you run the runner yourself, or device management installs the unit.
- It is refused while a systemd unit or LaunchAgent for this runner is installed: running both would be two processes with one identity, which the organisation refuses. Remove the service first (`systemctl --user disable --now wardyn-runnerd` or `launchctl bootout`, then delete its file), then run setup again.
- The environment file is `~/.config/wardyn/runnerd.env` on Linux and `runnerd.env` beside the runner's state directory on macOS, mode `0600`.

1. Run `wardyn-runnerd setup --org https://wardyn.example --token-file <file> --no-service`.
2. Run the printed command from a shell that stays open: `set -a; . '<env file>'; set +a; '<binary>' serve --org <url> --state-dir '<dir>'`. Setup prints it with the quoting your shell needs; on macOS the paths hold a space (`Application Support`), so copy the printed line rather than retyping it.
3. Claim the runner as for a service: see below.

### How audit decisions reach the organisation

- A run's proxy sends each egress decision to `wardyn-runnerd`, which alone writes it to the run's spool, `spools/<run>` in its state directory. It sends the spool to the organisation and deletes each record the organisation acknowledges.
- The proxy shares no directory with the runner, so an engine in a VM (Docker Desktop, Colima, Rancher Desktop) records decisions as a native one does.
- The proxy dials `host.docker.internal`, which its hosts map to the relay's address, at the port `wardyn-runnerd` keeps in `decision-sink.addr`. The connection is pinned to `decision-sink.key`, and each proxy has its own key: one run's proxy cannot write another run's decisions.
- A request whose decision cannot be recorded is not sent. A decision a stopped proxy never delivered is counted, and the organisation gets a `builtin:spool-lost` deny carrying the count.
- After a restart `wardyn-runnerd` listens on the same port before it stops the runs' proxies, so a proxy still running delivers what it holds first.
- The runner and organisation share one wire protocol version, and the organisation uses a newer runner feature only when the runner advertised it in its caps. An older runner keeps working with fewer features.
  - A newer runner is not promised to work against an older organisation, which is why the organisation goes first. See [Runner and organisation versions](../runner-registration.md#runner-and-organisation-versions).
  - Clear the runner's state directory before rolling a runner that ran anything under 0.9.3 back to 0.9.2.
- Upgrade the organisation before its runners. A runner newer than 0.9.3-rc.2 configures its proxy with `decision_sink`, which a proxy image from 0.9.3-rc.2 or older does not know. An unpinned runner of an older organisation takes that image (below), so its runs fail to start; a pinned runner needs an image at least as new as its own release.

### The proxy image a laptop runner uses

- An unpinned runner follows the organisation: at every connect it takes the proxy image the organisation answers for its architecture, so an upgraded organisation's image reaches the laptop without a reinstall. Runs started from then on use it; runs already going keep theirs.
- It takes any well-formed image the organisation names, including an older one. There is no version floor, and the change is logged at info level only.
- A proxy older than the organisation refuses a configuration that carries a field it does not know, because the proxy reads its configuration strictly. The run then fails to start rather than running with a rule dropped, so pin only an image from the organisation's own release.
- To fix the image, run `wardyn-runnerd setup --proxy-image <image>`. Setup marks it pinned and `serve` then never replaces it. A mirror of your own that is not the registration image, and an image `serve` is given by flag, count as pinned too.

### The Docker engine setup and serve use

- `wardyn-runnerd` reads `DOCKER_HOST` and the engine's default socket.
- Setup refuses `DOCKER_CERT_PATH`, `DOCKER_TLS_VERIFY` and `DOCKER_API_VERSION`: the service never sees them, so it would reach a different engine than the one setup checked.
- A selected docker context (`docker context use`, `DOCKER_CONTEXT`) is resolved when it is a local unix socket, and carried into the service as `DOCKER_HOST`. A TCP, SSH or TLS context is refused with the `DOCKER_HOST` to set instead.

## Claiming a runner

1. The owner mints a single-use `wdr_` registration token (`wardyn runner token`, or *My runner*). It lasts 1 hour, and needs the [`self_host_runner`](capabilities.md#self_host_runner) permission; admins are never asked.
2. An admin can mint one for a named person (`--owner`). It lasts 72 hours so MDM can deliver it, and needs no grant.
3. On the laptop, `wardyn-runnerd setup --org <url> --token-file <file>` generates an Ed25519 key pair locally, installs the service and prints the key fingerprint.
4. The org records an `unclaimed` runner. It is offered no run and relays nothing, and it expires after 24 hours.
5. The owner, signed in as themselves, claims it in *Your account → My runner*, or on the laptop with `WARDYN_TOKEN=<own API token> wardyn runner claim --pool NAME --default`. `setup --claim` does both steps.
6. The runner is now `claimed` and takes runs owned by its owner, once it is in one of the owner's pools (`placement_unavailable` otherwise).

- An admin can mint a token for a person but can never complete the claim.
- A stolen token shows up in *My runner* as a runner the owner did not register.
- Without the `self_host_runner` permission, `POST /api/v1/me/runners/tokens` answers `403` `capability_feature` (target `me.runners.tokens`). Existing runners and claims keep working.
- Commands, MDM flow and the claim refusal causes: [Set up a runner](../runner-registration.md#set-up-a-runner).

## Installing with a macOS package and MDM

- Every release from 0.9.3 carries `wardyn-runnerd.pkg`, a universal (Apple silicon and Intel) installer for `wardyn-runnerd`, and a managed-preferences profile template, `deploy/runner/macos/wardyn-runnerd.mobileconfig` (also inside the package and `wardyn-runner-units.tar.gz`).
- The package installs the binary and a LaunchAgent. It cannot register the laptop: that needs a person's single-use token ([Claiming a runner](#claiming-a-runner)).
- **Until the release is signed, the package is unsigned.** Gatekeeper refuses a double-click.
  - On macOS 15 and later, open **System Settings**, then **Privacy & Security**, and choose **Open Anyway** (right-click, **Open** no longer works there).
  - Apple's native MDM app install needs a Developer ID Installer signature, so push the package from an MDM agent or script (a Jamf or Kandji policy, or `installer -pkg` run as root).
  - Signing and notarization turn on in the release workflow when the maintainers add the Apple Developer ID secrets; nothing here changes then.

| Installed path | What it is |
|---|---|
| `/usr/local/wardyn/bin/wardyn-runnerd` | the runner (linked to `/usr/local/bin/wardyn-runnerd`) |
| `/Library/LaunchAgents/com.wardyn.runnerd.managed.plist` | starts the runner at login, like the agent `setup` writes |
| `/usr/local/wardyn/bin/runnerd-managed-start` | the agent's launcher: idle until the person is registered |
| `/usr/local/wardyn/share/wardyn-runnerd.mobileconfig` | the profile template |

1. Fill in the profile template (`OrgURL`, two fresh UUIDs, your organisation name) and push it from your MDM as a **custom settings** profile.
2. Push `wardyn-runnerd.pkg` with the same MDM, as a policy or script that runs `installer -pkg wardyn-runnerd.pkg -target /` as root (see above).
3. Deliver each person's registration token to a file only they can read ([Claiming a runner](#claiming-a-runner), step 2).
4. As that person, run `wardyn-runnerd register --org <url> --token-file <file>`, then `launchctl kickstart gui/$(id -u)/com.wardyn.runnerd.managed` (or log out and in).
5. The person claims the runner as usual.

- **Without MDM** install the package, then run `wardyn-runnerd setup --token-file <file>`, adding `--org <url>` when no profile is installed.
  - `setup` stops the managed agent, writes the agent `com.wardyn.runnerd` in the person's own `~/Library/LaunchAgents`, and the managed agent stays idle. `serve` also takes a lock in the runner state directory, so a second `serve` for one identity exits with a message.
- Logs go to `~/Library/Logs/wardyn/runnerd.log` either way.

### Managed preferences

- The profile's preference domain is `com.wardyn.runnerd`, which macOS writes to `/Library/Managed Preferences/com.wardyn.runnerd.plist`. A user-scoped profile lands in `/Library/Managed Preferences/<user>/com.wardyn.runnerd.plist` and wins over the device-wide file for that user.
- `wardyn-runnerd serve` reads it as the default of a flag. A flag wins over the profile, and the profile wins over the environment variable `setup` writes. An empty key is unset.

| Key | Same as | Notes |
|---|---|---|
| `OrgURL` | `--org` | required for `serve` unless `--org` is passed; also the default of `setup --org` |
| `ProxyImage` | `--proxy-image` | an image pin; left out, the image the organisation named at registration, else the release image matching the binary |
| `RelayListen` | `--relay-listen` | left out, found by a probe container |
| `RelayHostTarget` | `--relay-host-target` | left out, found by a probe container |
| `InternalNetwork` | `--internal-network` | default `wardyn-internal` |
| `TrustedCAFile` | `--trusted-ca-file` | a PEM bundle path; deliver the file with a separate MDM file payload ([A corporate CA](#a-corporate-ca-on-a-laptop-runner)) |
| `RecordingStore` | `--recording-store` | `pg`, `fs` or `off` |
| `DockerHost` | `DOCKER_HOST` | the engine's socket, when it is not the default |

## Offline and revocation

| Situation | Behaviour |
| --- | --- |
| Link down at run creation | Refused `runner_offline`; no run row exists |
| Link drops mid-run | Nothing is torn down; the proxy enforces its cached policy and spools decisions. A full spool holds a request up to 10 s, then answers a retryable `503` naming the runner's link |
| Link down past the lapse (about 1h5m) | The runner stops the proxy itself and keeps the agent and files |
| The runner process restarts | Its runs stop reaching the network until each is revived. This is fail-closed |
| The control plane restarts, or answers `503` while it starts | The runner reconnects once the organisation answers; live runs keep running. A teardown of a run it keeps only for containment is refused, so it cannot hold the connection; the next restart kills that agent instead. Nothing needs resetting |
| The computer is registered again under a new identity | At first start `serve` stops the old registration's run proxies, then moves `offline/`, `spools/` and `output/` into `previous/` (Docker; spooled decisions are kept, not delivered). If one cannot be stopped, it exits naming the fix and retries next start |
| Revoked by the owner or a security operator | The org ends that runner's unfinished runs `KILLED` and runs the kill cascade; the runner stops every governed proxy |

On a runner older than 0.9.3, a run kept only for containment could loop on refusals until the runner forgot it. Keeping `spools/` and `offline/` was not enough there: deleting the state was the only recovery, at the cost of undelivered audit records.

"Offline" and "ungoverned" are never the same thing: an offline runner is still governed and runs nothing new.

- Switching runners off, and revoking a runner, close the live session on the replica that served the request.
- A revoked runner's session on another replica closes within one 15-second ping interval. A session another replica holds after a switch-off stays open until the runner reconnects, but every sandbox create re-reads the switch, so it gets no new work.
- Revoking a runner whose session already dropped succeeds (`200`, state `revoked`).

## Reset a runner's local state

Do this when a runner's runs keep failing after an upgrade or after its Docker engine was reset. It ends every run in progress on the runner and keeps its identity, so it needs no new registration or claim.

1. Stop the service: `launchctl bootout gui/$(id -u)/com.wardyn.runnerd` on macOS, `systemctl --user stop wardyn-runnerd` on Linux.
2. List what the runner left on its engine. Every container and network a runner creates carries `wardyn.managed=true` and a `wardyn.runner-id` label:

   ```sh
   docker ps -a --filter label=wardyn.managed=true --filter label=wardyn.runner-id --format '{{.Names}}  {{.Label "wardyn.component"}}  {{.Status}}'
   ```

   These are the runs' `wardyn-agent-<run>` and `wardyn-proxy-<run>` containers, and any check container a stopped `serve` left. If a second runner identity uses this engine, add `--filter label=wardyn.runner-id=<id>`, with the `runner_id` from this runner's `runner.json`.
3. Remove them, and the runs' networks:

   ```sh
   docker rm -f $(docker ps -aq --filter label=wardyn.managed=true --filter label=wardyn.runner-id)
   docker network rm $(docker network ls -q --filter label=wardyn.managed=true --filter label=wardyn.runner-id)
   ```

   Leave volumes alone: a drive's volume holds its files. The shared `wardyn-internal` network carries no runner label and stays.
4. Keep the state directory as it is (`~/Library/Application Support/wardyn/runner` on macOS, `~/.config/wardyn/runner` on Linux). `runner.json` is the runner's identity, and `spools/` and `offline/` hold audit records not yet delivered and what the runner still owes for its runs. Keep the CA file you gave setup: `serve` copies it into the state directory at every start.
5. Start the service: `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.wardyn.runnerd.plist` on macOS, `systemctl --user start wardyn-runnerd` on Linux.

## Behind a shared ingress

Three routes are open to callers with no credential yet. Each has its own rate-limit bucket per client address: 10 attempts, then one every 10 seconds. The address is the TCP peer unless `WARDYN_TRUSTED_PROXIES` lists that peer, so without it every caller behind a shared ingress, internet traffic included, shares the ingress's bucket.

| Route | Refusal when the bucket is empty |
| --- | --- |
| `/api/v1/runners/{id}/stream` | `429` `runner_stream_rate_limited` |
| `/api/v1/runners/register` | `429` `runner_registration_rate_limited` |
| `/api/v1/devices/enrol` | `429` `device_enrol_rate_limited` |

- A runner that proves its key, or a registration that redeems a valid token, is refunded. Anonymous traffic and failed attempts still drain the bucket, so without trusted proxies one peer can lock every runner out.
- Reconnecting runners stay reliable only if you do one of these. Set `WARDYN_TRUSTED_PROXIES` to your ingress addresses so each client gets its own bucket. Or allow-list your runners' source networks on `/api/v1/runners/*/stream` and the other two paths. Or give runners a dedicated ingress or hostname. Per-client limits at the ingress do not close it on their own.
- The buckets are per replica, so an HA install's effective limit is the per-replica limit times the replica count.

The runner stream has two more anonymous limits and one per-runner limit, all per replica:

| Limit | Value | Spent by |
| --- | --- | --- |
| Global handshake bucket | 512, then 10 a second, shared by every address | Failed and unfinished handshakes; a runner that proves its key is refunded |
| Handshakes in progress | 256 connections between arrival and proof | A connection until it proves its key or closes; the 15-second handshake deadline bounds each |
| Per-runner reconnects | 10, then one every 10 seconds, per runner | Only a runner that has proved its key, so a caller naming a runner it cannot prove never spends that runner's budget |

- Every refusal is `429` `runner_stream_rate_limited`; the message says which limit refused.
- Memory per replica: each keyed bucket map holds at most 4096 entries of under 128 bytes, and each handshake slot under 64 KiB, so the cap bounds unproved connections under 16 MiB.

### Trusted proxies

`WARDYN_TRUSTED_PROXIES` (off by default) lists the proxies whose `X-Forwarded-For` Wardyn reads. It applies to the public listener only; the proxy-facing listener runs dial is never read through it.

1. Wardyn reads the header only when the TCP peer is inside the list.
2. It walks the chain right to left, skipping addresses inside the list; the first address outside it is the client. Entries left of it were written by that client and are never read.
3. A malformed, loopback or unspecified entry, more than 32 entries or more than 2048 bytes, or a chain made only of listed addresses, keeps the TCP peer.
4. The client becomes the row's `source_ip`, and the peer is kept as `data.proxy_peer` on every audit row the request writes (`data.device_origin.proxy_peer` on a device-forwarded row).

- List only your proxies' addresses. Any address in the list can name the client, so a range that holds callers lets them choose their own `source_ip`.
- Your proxy must append the client to `X-Forwarded-For`, or replace the header, never pass a client's value through alone.

## Legacy org enrolment (deprecated)

- A full `wardynd` in member mode (topology m′) can still enrol into an org with `WARDYN_ORG_URL` and `WARDYN_ORG_ENROLMENT_TOKEN`; see [DESKTOP.md](../DESKTOP.md#enrolling-into-an-org-control-plane).
- Moving to a client-mode runner removes the laptop's own control plane and its audit chain.

| Action | Route | Who |
| --- | --- | --- |
| Mint a single-use token (72 hours) | `POST /api/v1/admin/devices/enrolment-tokens` | Admin |
| List enrolled devices | `GET /api/v1/admin/devices` | Security admin |
| Revoke a device | `DELETE /api/v1/admin/devices/{id}` | Security admin |

- The token TTL is `deviceEnrolmentTokenTTL` in [`internal/api/devices.go`](../../internal/api/devices.go); the audit rows are `device.enrolment_token.create`, `device.enrol` and `device.revoke` ([AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md#devices-hybrid-enrolment)).
- A revoked device is answered `401`, and every run-creating path on that laptop answers `503` until it is re-enrolled.
- `deviceAuth` ([`internal/api/devices_auth.go`](../../internal/api/devices_auth.go)) resolves a `wdd_` bearer to routes under `/api/v1/devices/{id}/*` only; a stolen credential can forge audit rows about that one device and nothing else (threat-model residuals #50–#52).
- Federation lag shows as `wardyn_org_federation_lag` on the laptop ([Monitoring](monitoring.md)) and `org_federation.lag` on its `/healthz`. A refused batch writes `device.audit.ingest` failure rows and halts the forwarder until `wardynd` restarts; an unreachable org produces neither.
