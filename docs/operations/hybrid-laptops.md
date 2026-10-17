> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Managed laptops: client-mode runners

- In 0.9 a laptop joins an org as a **client-mode runner**: it runs `wardyn-runnerd`, not a control plane, and the person who owns it claims it.
- A run is placed on it through one of its owner's user pools: see [Runner pools](runner-pools.md).
- Set it up with `wardyn-runnerd setup`, one command that installs the per-user service: see [Set up a runner](../runner-registration.md#set-up-a-runner).
- Design: [docs/design/0.9/PLAN.md](../design/0.9/PLAN.md). Org enrolment of a full daemon (`WARDYN_ORG_URL`) still works in 0.9, logs a deprecation warning at boot, and is removed in 1.0; see [Legacy org enrolment](#legacy-org-enrolment-deprecated).

## Where each thing lives

| Concern | Org | Laptop |
| --- | --- | --- |
| Scheduling, placement, the ceiling | Yes, the only scheduler | Nothing decides locally |
| Credentials | Held in the org; the laptop can use a `via_org` credential for a run's life | The person's own. `runner_resident` values are not yet delivered to runners in 0.9 |
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
> `runner_resident` values are not yet delivered to runners in 0.9. An admin can set the mode and a runner reports its posture, but a run that needs such a credential is still refused (`placement_credential`).

| Credential class | Modes |
| --- | --- |
| `env_secret`, `file_secret`, `ssh_key`, `git_pat_helper`, `bedrock_role_credentials` | `runner_resident` or `refuse` |
| `api_key`, `bedrock_bearer`, `aws_sso_bearer`, `azure_foundry_token`, `git_pat_broker`, `ado_minted_pat` | `via_org`, `runner_resident` or `refuse` |
| `github_token` | `via_org` or `refuse` |
| `cloud_sts` | `refuse` only |
| `oauth_subscription` | `own` only: the person's own sign-in |

- An admin can set a credential class to `runner_resident` with a `require_posture` (`mdm_managed`, `disk_encrypted`, `os_min`).
- The runner reports its posture when it connects and again when it changes. A root-owned `/etc/wardyn/posture.json` from your MDM wins over the runner's own probe.
- A runner keeps a delivered value in the OS keystore for the run (Secret Service; on macOS the Keychain, written over stdin) and erases it at run end. A runner with no keystore refuses (`runner_keystore_unavailable`).
- On Linux the Secret Service needs a login session bus (`DBUS_SESSION_BUS_ADDRESS`, or the user bus socket under `/run/user/<uid>`), so a system service with no login session has no keystore.
- Posture is self-reported, and a run with a CC3 floor never takes a resident credential.

### bedrock_sso on a laptop runner

- A `bedrock_sso` model provider works on your own runner with AWS SSO proxy-inject on (`WARDYN_AWS_SSO_PROXY_INJECT`, the default).
- The sandbox gets a placeholder `~/.aws`; the proxy injects your own AWS session at the run's `portal.sso` host, relayed to your runner as your own credential.
- Only a `credential_source: per_user` session qualifies. A shared AWS session is refused (`placement_credential`).
- With proxy-inject off the sandbox would hold your real SSO token cache. A local `bedrock_sso` run is then refused at Review, preflight and create (`placement_credential`, naming `SandboxSpec.SecretEnv[WARDYN_AWS_SSO_CONFIG_B64]`), before Launch.
- With proxy-inject on, `WARDYN_AWS_SSO_CONFIG_B64` is plain container env. `docker inspect` and a k8s pod spec show the SSO start URL, region, account id and role name, but no token or client secret.
- The short-lived role credentials the SDK mints from it are still resident in the sandbox, because SigV4 signs in-process.
- The provider's sign-in and renewal: [AWS SSO per person](../OPERATIONS.md#aws-sso-per-person).

### Private endpoints and `internal_hosts`

- The organisation's [`internal_hosts`](../OPERATIONS.md#network-upstream-proxy-and-egress-redirects) declarations reach a laptop runner's proxy as they do a cluster one. A host that resolves to a private address (PrivateLink, Private Service Connect, a zero-trust client's `100.64.0.0/10` range) is reached when it is declared there, and the decision row reads `rule_source: site-config:internal-host`.
- A declaration lifts only the private-IP guard for that suffix. The egress allowlist still decides, and loopback, link-local and metadata addresses stay closed.
- On a laptop the sandbox resolves through the person's network, so declare `cidrs` on each entry a laptop estate uses. An entry without them lifts the whole private range for that suffix.
- The organisation's upstream proxy, trusted CA, `upstream_proxy_no_proxy` and LLM upstream overrides are still not sent to a laptop's proxy. The runner's own CA bundle takes the trusted CA's place: see below.

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

- `wardyn-rec` uploads a laptop run's cast to `wardyn-proxy:3128`, as on a cluster. The proxy's in-cluster control-plane name maps to the runner's relay by design, which carries the upload to the organisation bound to the run's own runner.
- The organisation decides per run, at dispatch. With its recording store off (`WARDYN_RECORDING_STORE=off`) the run page says `org_off`.
- `wardyn-runnerd serve --recording-store off` is the runner's ceiling: it never records. Otherwise the runner records the runs its organisation asks it to; a runner older than 0.9.3 records by its own setting, and counts as not recording.
- An organisation or pool can set `recording: required` (default `preferred`). A run only a non-recording runner could take is then refused `runner_recording_off`, and with the store off every run is refused `recording_store_off`, as is saving `required`. Under `preferred` the page says `runner_declined`, until a stored cast clears it.
- An upload with no store is answered `409` `recording_off`, a reason the `brokered:recording` row and `wardyn-rec`'s log carry (migration `0180_run_recording`).

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

## Offline and revocation

| Situation | Behaviour |
| --- | --- |
| Link down at run creation | Refused `runner_offline`; no run row exists |
| Link drops mid-run | Nothing is torn down; the proxy enforces its cached policy and spools decisions |
| Link down past the lapse (about 1h5m) | The runner stops the proxy itself and keeps the agent and files |
| The runner process restarts | Its runs stop reaching the network until each is revived. This is fail-closed |
| Revoked by the owner or a security operator | The org ends that runner's unfinished runs `KILLED` and runs the kill cascade; the runner stops every governed proxy |

"Offline" and "ungoverned" are never the same thing: an offline runner is still governed and runs nothing new.

- Switching runners off, and revoking a runner, close the live session on the replica that served the request.
- A revoked runner's session on another replica closes within one 15-second ping interval. A session another replica holds after a switch-off stays open until the runner reconnects, but every sandbox create re-reads the switch, so it gets no new work.
- Revoking a runner whose session already dropped succeeds (`200`, state `revoked`).

## Behind a shared ingress

Three routes are open to callers with no credential yet. Each has its own rate-limit bucket per TCP peer: 10 attempts, then one every 10 seconds. Wardyn reads no `X-Forwarded-For`, so behind a shared ingress every caller, internet traffic included, shares the ingress's bucket.

| Route | Refusal when the bucket is empty |
| --- | --- |
| `/api/v1/runners/{id}/stream` | `429` `runner_stream_rate_limited` |
| `/api/v1/runners/register` | `429` `runner_registration_rate_limited` |
| `/api/v1/devices/enrol` | `429` `device_enrol_rate_limited` |

- A runner that proves its key, or a registration that redeems a valid token, is refunded. Anonymous traffic and failed attempts still drain the bucket, so one peer can lock every runner out.
- Allow-list your runners' source networks on these three paths at the ingress, or give runners a dedicated ingress or hostname so the bucket is not shared with the internet. Per-client limits at the ingress do not close it.
- The buckets are per replica, so an HA install's effective limit is the per-replica limit times the replica count.

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
