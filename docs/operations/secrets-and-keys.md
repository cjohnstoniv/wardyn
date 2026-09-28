> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Secrets from files, age-key rotation and external key stores

## Secrets from files (Vault Agent / CSI)

Every secret-carrying `wardynd` boot setting has a `<VAR>_FILE` twin that holds
a **path** instead of the value: `WARDYN_PG_DSN_FILE`,
`WARDYN_PG_MIGRATE_DSN_FILE`, `WARDYN_ADMIN_TOKEN_FILE`, `WARDYN_AGE_KEY_FILE`,
`WARDYN_OIDC_CLIENT_SECRET_FILE`, `WARDYN_DIRECTORY_CLIENT_SECRET_FILE`,
`WARDYN_AUDIT_SINKS_FILE` and `WARDYN_ORG_ENROLMENT_TOKEN_FILE`
([ENV.md](../ENV.md)). Use them when a control requires
secrets delivered at runtime (Vault Agent injector, Secrets Store CSI driver,
projected volumes), or when a posture scanner flags secret env vars.

How `wardynd` reads them (`resolveSecretFiles`, `cmd/wardynd/secret_file.go`):

- **Once, at boot.** A rotated file takes effect on the next restart, the same
  as a changed env var.
- **Both forms set refuses boot**, naming both variables. There is no
  precedence.
- **An unreadable, empty, group- or world-writable file refuses boot**, naming
  the variable and the path. The content is never logged or echoed. The mode
  is checked on the opened file, so the file checked is the file read.
- **One trailing newline is trimmed** (`\n` or `\r\n`). Anything else in the
  file is part of the value.
- **Other-readable is refused only on a file wardynd's own non-root uid
  owns.** That is the hand-made host file, and `chmod 640` fixes it (under
  Vault Agent, set `agent-inject-perms-<name>`). Other-readable files that a
  supported mechanism produces are allowed: a kubelet Secret volume
  (root-owned `0440` under `fsGroup`), a Secrets Store CSI file (root-owned
  `0644`, readable by a non-root process only through the other-read bit) and
  Vault Agent's default (`0644`, owned by the agent's uid). This is why the
  rule differs from `WARDYN_DAEMON_PROXY_SECRET`'s 0600 rule. Scope the mount
  to the wardynd container.

On Kubernetes, `secretFiles.enabled=true` delivers the chart's own Secrets this
way with no other change (chart README, "Boot secrets as files"). Both examples
below replace the Kubernetes Secret entirely. They use generic names: Vault at
`https://vault.example:8200`, a Vault role `wardyn` bound to the chart's
ServiceAccount, and a KV v2 secret at `secret/data/wardyn/boot` with keys
`pg_dsn`, `admin_token` and `age_key`. Both leave `postgres.dsn.secretRef.name`
empty (it has a non-empty default), so the DSN comes only from the file.

**Vault Agent injector.** The injector writes each secret to `/vault/secrets/`.
`agent-pre-populate-only` runs the agent as an init container only, which is
enough because wardynd reads once at boot.

```yaml
podAnnotations:
  vault.hashicorp.com/agent-inject: "true"
  vault.hashicorp.com/agent-pre-populate-only: "true"
  vault.hashicorp.com/service: "https://vault.example:8200"
  vault.hashicorp.com/role: "wardyn"
  vault.hashicorp.com/agent-inject-secret-pg-dsn: "secret/data/wardyn/boot"
  vault.hashicorp.com/agent-inject-template-pg-dsn: |
    {{- with secret "secret/data/wardyn/boot" }}{{ .Data.data.pg_dsn }}{{ end }}
  vault.hashicorp.com/agent-inject-perms-pg-dsn: "0440"
  vault.hashicorp.com/agent-inject-secret-admin-token: "secret/data/wardyn/boot"
  vault.hashicorp.com/agent-inject-template-admin-token: |
    {{- with secret "secret/data/wardyn/boot" }}{{ .Data.data.admin_token }}{{ end }}
  vault.hashicorp.com/agent-inject-perms-admin-token: "0440"
  vault.hashicorp.com/agent-inject-secret-age-key: "secret/data/wardyn/boot"
  vault.hashicorp.com/agent-inject-template-age-key: |
    {{- with secret "secret/data/wardyn/boot" }}{{ .Data.data.age_key }}{{ end }}
  vault.hashicorp.com/agent-inject-perms-age-key: "0440"
postgres:
  dsn:
    secretRef:
      name: ""
env:
  WARDYN_PG_DSN_FILE: /vault/secrets/pg-dsn
  WARDYN_ADMIN_TOKEN_FILE: /vault/secrets/admin-token
  WARDYN_AGE_KEY_FILE: /vault/secrets/age-key
networkPolicy:
  egress:
    extra:
      # The agent runs inside the wardynd pod, so the pod's egress policy
      # applies to it. Narrow this to your Vault address.
      - ports: [{port: 8200, protocol: TCP}]
```

The injector's Kubernetes auth needs a ServiceAccount token in the pod, and
the chart turns automount off. Either set `serviceAccount.automount=true`, or
mount a projected token through `extraVolumes` and name that volume in
`vault.hashicorp.com/agent-service-account-token-volume-name`.

**Secrets Store CSI driver** (Vault provider shown; other providers take the same
chart values). The driver mounts the files from the node, so the pod's
NetworkPolicy and ServiceAccount automount do not apply.

```yaml
apiVersion: secrets-store.csi.x-k8s.io/v1
kind: SecretProviderClass
metadata:
  name: wardyn-boot
spec:
  provider: vault
  parameters:
    vaultAddress: "https://vault.example:8200"
    roleName: "wardyn"
    objects: |
      - objectName: "pg-dsn"
        secretPath: "secret/data/wardyn/boot"
        secretKey: "pg_dsn"
      - objectName: "admin-token"
        secretPath: "secret/data/wardyn/boot"
        secretKey: "admin_token"
      - objectName: "age-key"
        secretPath: "secret/data/wardyn/boot"
        secretKey: "age_key"
```

```yaml
extraVolumes:
  - name: boot-secrets-csi
    csi:
      driver: secrets-store.csi.k8s.io
      readOnly: true
      volumeAttributes:
        secretProviderClass: wardyn-boot
extraVolumeMounts:
  - name: boot-secrets-csi
    mountPath: /mnt/secrets-store
    readOnly: true
postgres:
  dsn:
    secretRef:
      name: ""
env:
  WARDYN_PG_DSN_FILE: /mnt/secrets-store/pg-dsn
  WARDYN_ADMIN_TOKEN_FILE: /mnt/secrets-store/admin-token
  WARDYN_AGE_KEY_FILE: /mnt/secrets-store/age-key
```

The chart counts an `env`/`extraEnv` `_FILE` entry as that secret being wired.
The auth and age-key render checks pass. Naming it beside the chart's own
source (`auth.adminToken.*`, `postgres.dsn.*`, `secrets.ageKey*`) is refused
at render, as it would be at boot, and so is naming both `WARDYN_X` and
`WARDYN_X_FILE`. The file path itself is not a secret, which
is why it can sit in `env`.

**Compose.** `deploy/compose/docker-compose.yaml` still passes the plain
variables, and `WARDYN_ADMIN_TOKEN` falls back to the demo token when it is
empty. A `_FILE` beside a non-empty plain variable refuses boot. Before you use a
`_FILE` twin, set the plain variable to `""` under `environment:` in a compose
override file. Blanking it in `.env` does not work, because the `:-` default
fills an empty value.


## Rotating the age key

Each stored secret is an envelope (`internal/secretstore/pg`, since 0.7.12): the
value is sealed with AES-256-GCM under its own data key, bound to the row's owner
and name, and that data key is wrapped by a `local` key-encryption key — derived
from `WARDYN_AGE_KEY` with HKDF-SHA256, one per purpose, and recorded on each row as
`kek_id` (`local/cred:<fingerprint of the public recipient>` for credentials,
`local/platform:<fingerprint>` for wardynd's own boot keys; rows written before
0.8 say `local:<fingerprint>`, and `wardynd -rewrap` moves them). So simply changing
`WARDYN_AGE_KEY` migrates nothing — every row still names the old key, and a read
refuses a row whose `kek_id` is not the configured one. Startup decrypts the persisted signing
key through `loadOrCreateSigningKey` / `loadOrCreateSecret` and fails closed on
a mismatch, before serving requests. A healthy start checks that boot key, not
every application secret; verify a secret-dependent run after recovery too.

The at-rest cipher is AES-256-GCM from the Go standard library
(`cipher.NewGCMWithRandomNonce`, which draws each 96-bit nonce inside Go's
cryptographic module) with HKDF-SHA256 key derivation, so Go's FIPS 140-3 mode
(`GODEBUG=fips140=on`) applies to it — and with the Go version in `go.mod` the
envelope also runs under `GODEBUG=fips140=only`. That is a statement about this
path only: the build does not pin a frozen module snapshot (`GOFIPS140`), and age
(used once, to convert pre-envelope rows) is outside it. The `local` key's id is
taken over the age key's public recipient, which is X25519, and
`GODEBUG=fips140=only` forbids X25519: under it wardynd refuses to start with a
`WARDYN_AGE_KEY` (or an ephemeral one) and names store mode. Store mode
(`WARDYN_SECRET_STORE=vaultkv`, below) needs no age key and boots under
`GODEBUG=fips140=only`.

`wardynd -rotate-age-key <key-file>` is the supported rotation, a **maintenance
mode, not a server start**: it mints a new identity, rewraps every row's data key
from the current key-encryption key to the new one in ONE transaction — the sealed
values are never decrypted and not rewritten — replaces the key file, writes a
`secret.rekey` audit event, and exits. It never opens a
listener and never dispatches a run. Three properties:

- **The daemon must be stopped.** A serving wardynd holds the OLD identity in
  memory for the life of the process; after a rotation it decrypts nothing and
  would write any newly-stored secret under the retired key. A Postgres advisory
  lock (`db.SecretRekeyLockKey`) refuses a second concurrent *rotation*, but it
  cannot see a serving daemon, so stopping it is **your** step, not one the tool
  enforces.
- **All-or-nothing.** The whole rewrap runs in ONE transaction. A row whose data
  key the current key cannot unwrap aborts everything with an error naming that
  row and how far it got (`rekey ABORTED after 3 of 9 rows …`), and nothing is
  committed — every secret is still readable with the old key. There is no
  half-rotated state to diagnose. A pre-envelope row aborts it too: boot the
  upgraded daemon once, which converts them (see [Upgrades](../OPERATIONS.md#upgrades)), before rotating.
- **The CLI never sees the key.** `wardyn` has no rotation surface at all; this
  is a `wardynd` flag, run by whoever has shell access to the key file.

The key file is a **bare `AGE-SECRET-KEY-…` line** (`#` comment lines allowed, so
`age-keygen` output works as-is) — *not* an env file. It must already hold the
identity `WARDYN_AGE_KEY` names, or the rotation is refused: this file is
replaced, and pointing the flag at `deploy/compose/.env` would overwrite it.

```sh
# 0. Take the Postgres dump above FIRST. It is the only rollback for the data
#    half; the .bak below is only the rollback for the key half.

# 1. Stop the daemon. Nothing may be writing secrets during the rotation.
docker compose -f deploy/compose/docker-compose.yaml stop wardynd

# 2. Put the CURRENT key in a key file, if it is not already in one.
#    (Compose keeps it as a WARDYN_AGE_KEY= line in deploy/compose/.env.)
mkdir -p ~/.wardyn
umask 077
grep -E '^WARDYN_AGE_KEY=' deploy/compose/.env | cut -d= -f2- > ~/.wardyn/age.key

# 3. Rotate. The old key still comes in via WARDYN_AGE_KEY; the new one is
#    generated here and lands in the key file.
WARDYN_PG_DSN='postgres://…' WARDYN_AGE_KEY="$(cat ~/.wardyn/age.key)" \
  ./bin/wardynd -rotate-age-key ~/.wardyn/age.key
# INFO wardynd: age key rotated; … secrets=7 key_file=/home/you/.wardyn/age.key
#      public_recipient=age1… rollback_copy=/home/you/.wardyn/age.key.bak

# 4. Put the NEW key back where the deployment reads it from, then restart.
#    Compose: rewrite the .env line. Helm: update the Secret's age-key entry.
docker compose -f deploy/compose/docker-compose.yaml up -d wardynd
```

**With `WARDYN_AGE_KEY_FILE` on Kubernetes**, the key file is a read-only
Secret, CSI or Vault Agent mount, and the rotation cannot replace it in place.
Rotate against a writable copy instead. Scale wardynd to 0, then in a one-off
pod (or on a host with the DSN) copy the mounted key to a scratch file you own
(`umask 077; cp /etc/wardyn/secrets/age-key /tmp/age.key`). Run
`WARDYN_AGE_KEY_FILE=/tmp/age.key wardynd -rotate-age-key /tmp/age.key`. Then
write the new key into the Secret, or into the Vault/CSI source it comes from,
before you scale back up. Until the source holds the new key, every restart
reads the retired one and fails closed.

Verify the same way the restore runbook does — a row count proves nothing about
decryptability, so launch a run against a workspace that depends on a stored
secret and confirm it starts. The audit trail records the rotation itself:

```sh
docker exec -i wardyn-postgres psql -U wardyn -d wardyn \
  -c "SELECT time, data FROM audit_events WHERE action='secret.rekey' ORDER BY time DESC LIMIT 1;"
```

**Rollback.** The previous key file is kept as `<key-file>.bak`, `0600`, until
*you* delete it. Restoring it is only half an undo: the database is already
re-encrypted, so `.bak` is usable **only** together with the Postgres dump from
step 0. Once the rotated deployment is confirmed working, delete `.bak` — leaving
it leaves a second copy of a retired master key on disk.

If a step after the commit fails (the key file could not be replaced), the error
says so and names `<key-file>.new`, which holds the new identity and at that point
is the **only** key that reads the store. Save it before doing anything else.

Whatever you do, **back the key up off-host.** Rotation re-encrypts what is there;
it cannot recover a key you have already lost.

With `WARDYN_PLATFORM_KEY_FILE` set (below), the rotation moves only the rows
under the age key; the boot keys under the platform key stay as they are. Rows
sealed under Vault Transit (below) hold nothing under the age key either, and a
rotation leaves them alone.


## Separating the platform keys

By default one age key protects everything the `secrets` table holds: people's
credentials, and wardynd's own signing, session, UI-session and SSH host keys. A
leak of `WARDYN_AGE_KEY` together with the database then lets someone forge run
identities, console sessions and the SSH host, not only read credentials
(`threatmodel/THREAT-MODEL.md` residual #49). `/setup/status` says so as the amber
`platform_shared` row.

`WARDYN_PLATFORM_KEY_FILE` names a file holding a **second** age identity. The
boot keys are then wrapped under a key derived from it alone, and no key
`WARDYN_AGE_KEY` derives opens one: a boot key row wrapped under the age key is
refused, naming `wardynd -rewrap`, and boot stops rather than mint over it. The
file stays optional; nothing requires it.

```sh
# 1. Mint the second key where only wardynd can read it (0600, off-host backup).
umask 077
./bin/wardynd -gen-age-key > ~/.wardyn/platform.key
# 2. Move the boot keys onto it with -rewrap (see "Moving data keys" below).
WARDYN_PG_DSN='postgres://…' WARDYN_AGE_KEY="$(cat ~/.wardyn/age.key)" \
  WARDYN_PLATFORM_KEY_FILE=~/.wardyn/platform.key ./bin/wardynd -rewrap
# INFO wardynd: stored secrets rewrapped … secrets=4 platform_key_separate=true
# 3. Restart every replica with WARDYN_PLATFORM_KEY_FILE set.
```

The one moment the age key still vouches for the boot keys is this move: run it
from a host you trust, not after a suspected leak of the age key (then replace
the boot keys instead: delete their rows and restart, which mints new ones —
console sessions end and SSH clients see a new host key).

Keep the platform key as carefully as the age key, and apart from it: both are
needed to read everything, and losing the platform key loses the boot keys
(a restart then refuses; delete their rows to mint new ones). In store mode
there is no local key at all and the file is refused; there the boot keys live
under `platform/` in the organisation's store (two Vault roles, below). With
`WARDYN_KEK=transit` (next section) the boot keys are wrapped by Transit like
every other row, and the file only reads the rows still under it.


## Key service: Vault Transit

With `WARDYN_KEK=transit`, each stored credential is still sealed in Postgres
(AES-256-GCM under its own data key, as above), but the data key is wrapped by
your Vault's Transit engine instead of a key derived from `WARDYN_AGE_KEY`. The
key-encryption key never leaves Vault, and every unwrap is one Transit `decrypt`
in your Vault audit device. The database alone decrypts nothing; neither does
the database plus anything on the Wardyn host, once no row is sealed under the
age key and `WARDYN_AGE_KEY` is unset. Wardyn's boot keys are wrapped the same
way, under the same Transit key, so **do not restart wardynd during a Vault
outage**: it will wait for Vault rather than boot. (If your organisation wants
Vault to hold the values themselves, not a key, that is store mode, below.)

Transit uses the same client as the Vault store: `WARDYN_VAULT_ADDR`,
`WARDYN_VAULT_NAMESPACE`, Kubernetes or token-file authentication as
`WARDYN_VAULT_ROLE`, and the TLS settings described under "Store mode:
credentials in Vault". The key:

```sh
vault secrets enable transit
vault write -f transit/keys/wardyn type=aes256-gcm96
```

Keep the key at `exportable=false`, `allow_plaintext_backup=false` and
`deletion_allowed=false` (Vault's defaults; `vault read transit/keys/wardyn`
shows them). The first two cannot be turned back off once set, and either lets
the key leave Vault; the third keeps one command from destroying every stored
credential. One Transit key and one Vault role wrap Wardyn's boot keys and the
credentials alike; a separate key and role for the boot keys is a 0.8.x
follow-up (`threatmodel/THREAT-MODEL.md` residual #49).

**Policy.** Two paths, `update` only. Wardyn never calls `rewrap/` (Vault does
not document `associated_data` on it, so `wardynd -rewrap` rewraps client-side)
and never reads `keys/`:

```hcl
path "transit/encrypt/wardyn" { capabilities = ["update"] }
path "transit/decrypt/wardyn" { capabilities = ["update"] }
```

**Binding.** Every wrap carries the row's owner and name as Transit
`associated_data`, so a wrapped data key moved to another row does not unwrap.
Only key types that bind it can do that; at boot wardynd wraps a probe data key,
unwraps it, and tries to unwrap it under another row's `associated_data`. If
that works, or the probe does not round-trip, **wardynd refuses to start**.

**Moving an install to Transit, and back.** Reads follow each row's `kek_id`,
so a daemon configured with both keys reads every row while `-rewrap` moves
them:

```sh
# 0. Boot this version once with your WARDYN_AGE_KEY (it converts any
#    pre-envelope rows), and take the Postgres dump (see Backup).
# 1. Set WARDYN_KEK=transit and WARDYN_VAULT_TRANSIT_KEY=wardyn with the
#    WARDYN_VAULT_* client settings, keep WARDYN_AGE_KEY, and restart: new
#    rows are wrapped by Transit, and old ones still read.
# 2. Rewrap the rest, with the same settings:
wardynd -rewrap
#    every sealed secret is wrapped under transit:transit/wardyn version 1; …
# 3. Unset WARDYN_AGE_KEY and restart. wardynd refuses to start until you do
#    (and, the other way, refuses without it, naming -rewrap, while any row is
#    still sealed under it).
```

Back: set `WARDYN_KEK=local` and `WARDYN_AGE_KEY`, keep
`WARDYN_VAULT_TRANSIT_KEY` so Transit still reads its rows, restart, and run
`wardynd -rewrap` again.

**Rotating the Transit key.** Rotate in Vault, rewrap, then retire the old
versions:

```sh
vault write -f transit/keys/wardyn/rotate
wardynd -rewrap
#    every sealed secret is wrapped under transit:transit/wardyn version 2;
#    raising the Transit key's min_decryption_version to 2 now retires the older versions
vault write transit/keys/wardyn/config min_decryption_version=2
```

A row still wrapped under a retired version is refused, naming the row, until
`min_decryption_version` is lowered again; `-rewrap` first, then raise it.

**When Vault is unavailable.** As for the Vault store: a sealed, throttled or
unreachable Vault is *transient* (the sink answers 503, "Wardyn couldn't reach
the service that holds this run's credential"), while a 403, a wrap that does
not unwrap for its row, or a retired version is *definitive*.


## Moving data keys: `wardynd -rewrap`

`wardynd -rewrap` is the one command that moves stored secrets' data keys from
one key-encryption key to another. It is a **maintenance mode**: it rewraps,
writes one `secret.rewrap` audit row, and exits. Each row's data key moves onto
the key a write uses under the settings it runs with:

- **The local key of the row's purpose** (`local/cred:` or `local/platform:`):
  rows a pre-0.8 wardynd wrote (`local:`), and, once `WARDYN_PLATFORM_KEY_FILE`
  is set, the boot keys still under the age key ("Separating the platform
  keys" above).
- **The key service**, with `WARDYN_KEK=transit`, at the Transit key's latest
  version: every local row, and every Transit row wrapped under an older
  version ("Key service: Vault Transit" above). It then prints the
  `min_decryption_version` that retires the older versions.
- **Back to the local key**, with `WARDYN_KEK=local` and
  `WARDYN_VAULT_TRANSIT_KEY` still set: every row under Transit.

Run it with the same `WARDYN_AGE_KEY`, `WARDYN_PLATFORM_KEY_FILE`, `WARDYN_KEK`
and `WARDYN_VAULT_*` settings the daemon uses (`WARDYN_AGE_KEY` may be unset
only with `WARDYN_KEK=transit`, once no row is under it). Properties:

- **Data keys only.** Each data key is unwrapped under its row's own key and
  wrapped again under the target, both bound to the row. Only the wrapped data
  key and `kek_id` change: no sealed value is decrypted or rewritten.
  Transit's server-side `rewrap` is never called.
- **All-or-nothing.** The whole run is ONE transaction. A row it cannot move —
  under a key these settings do not reach, a pre-envelope row, a Transit
  outage — aborts everything with an error naming that row and how far it got
  (`rewrap ABORTED after 3 of 9 rows …`), and nothing is committed. Re-running
  it is safe: a row already under its target is left alone.
- **Safe beside a serving daemon with the same settings**, which reads a row
  under both its old and its new key. While it runs, a write to an existing
  secret waits for its commit. It takes the same Postgres advisory lock as
  `-rotate-age-key`, so the two never run at once.
- **Pointer rows** (store mode) hold no data key and are never touched.

Restart every replica with the settings it ran with afterwards. After a move
onto Transit, the last step is: **unset `WARDYN_AGE_KEY`; wardynd refuses to
start until you do.** With `WARDYN_KEK=transit` and no stored row left under the
age key, the key could only let whoever also holds the database forge a row
wardynd still reads under it, its own boot keys among them.


## Store mode: credentials in Vault

With `WARDYN_SECRET_STORE=vaultkv`, every stored credential's value lives in
your organisation's Vault KV v2 engine (OpenBao is a supported, API-compatible
endpoint), and Wardyn keeps only a pointer row in Postgres: owner, name, when,
and where in Vault (`enc_version` 2, `kek_id` `vaultkv:<mount>/<path>`, no
ciphertext). Wardyn does no at-rest cryptography for such a row, and holds no
key: once every row is in Vault, `WARDYN_AGE_KEY` is unset. Every read is one
Vault read, so it appears in your Vault audit device (with the path and the
token's entity; values HMAC'd) as well as in Wardyn's audit log. Wardyn's own
boot keys (signing, session, UI-session, SSH host, internal CA) live there too.

**Paths.** Under the mount (`WARDYN_VAULT_KV_MOUNT`, default `wardyn`) and the
install's prefix (`WARDYN_VAULT_KV_PREFIX`; the chart sets the release
namespace):

```
<prefix>/platform/<name>               Wardyn's boot keys
<prefix>/operator/<name>               operator-namespace credentials
<prefix>/people/<owner>/<name>         a person's credentials; <owner> is the
                                       principal in base32hex, lowercase, unpadded
```

Each value is `{"value": "<base64>"}` with `custom_metadata`
`wardyn-owner`, `wardyn-name`, `wardyn-kind` and `wardyn-format`, and
`max_versions` `WARDYN_VAULT_KV_MAX_VERSIONS` (default 1, so a replaced value
does not linger). A read derives the path from the row's owner and name and
refuses a row that points anywhere else, then refuses a value whose metadata
names another row: a pointer moved by a database writer reads nothing.
Removing a credential is `DELETE metadata/<path>`, every version at once.
Paths carry the owner and name, so they reach your Vault audit log.

**Policy.** Least privilege, templated so another install in another namespace
cannot read this one's paths. There is no `destroy/` or `undelete/` stanza and
no `delete` on `data/`: Wardyn never calls any of them. `read` on
`wardyn/config` lets wardynd check at boot that a KV v2 engine is mounted at
`WARDYN_VAULT_KV_MOUNT`: a mistyped mount, or an engine not yet enabled, fails
boot instead of the first write.

```hcl
# <accessor> is the Kubernetes auth mount's accessor (vault auth list)
path "wardyn/config" {
  capabilities = ["read"]
}
path "wardyn/data/{{identity.entity.aliases.<accessor>.metadata.service_account_namespace}}/*" {
  capabilities = ["create", "update", "read"]
}
path "wardyn/metadata/{{identity.entity.aliases.<accessor>.metadata.service_account_namespace}}/*" {
  capabilities = ["create", "update", "read", "delete", "list"]
}
```

With token-file authentication (compose, VMs) there is no Kubernetes alias to
template on: write the install's `WARDYN_VAULT_KV_PREFIX` literally, as
`wardyn/data/<prefix>/*` and `wardyn/metadata/<prefix>/*`, and give each
install its own policy.

**Two Vault roles (recommended).** With one role, the policy above covers
`platform/` and `people/` alike: the split is for your audit and filtering, and a
leak of wardynd's Vault token reaches its signing and session keys too. Two
Kubernetes-auth roles bound to the same service account separate the privilege:
`wardyn-platform` with a policy over `<ns>/platform/*` only, and
`wardyn-credentials` with a policy over `<ns>/operator/*` and `<ns>/people/*`
(the `wardyn/config` stanza, and the two path stanzas above, each with that
path in place of `*`, plus `list` on `metadata/<ns>/` for `-reconcile`; the
boot check of the mount runs as this role, so the platform role needs no
`wardyn/config`). Set `WARDYN_VAULT_ROLE=wardyn-credentials`
and `WARDYN_VAULT_ROLE_PLATFORM=wardyn-platform` (chart:
`secretStore.vault.role` and `secretStore.vault.rolePlatform`): wardynd logs in
as both at boot, refuses to start if either login fails, and makes every
`platform/` call as the platform role only. Revoking or rotating one role leaves
the other untouched, and the platform policy can sit with fewer people. The
second role needs Kubernetes auth.

**Authentication.** There is no Vault token in an environment variable, by
design.

- *Kubernetes* (`WARDYN_VAULT_AUTH=kubernetes`, the default). The chart
  projects a dedicated service-account token with audience `vault` at
  `/var/run/secrets/wardyn-vault/token` (`secretStore.vault.*` in
  `values.yaml`). Configure the role to match:

  ```sh
  vault write auth/kubernetes/role/wardyn \
      bound_service_account_names=<the chart's service account> \
      bound_service_account_namespaces=<the release namespace> \
      audience=vault policies=wardyn-kv token_ttl=1h
  ```
- *Token file* (`WARDYN_VAULT_AUTH=token-file`), for compose and VMs: a
  Vault Agent sink or a CSI file at `WARDYN_VAULT_TOKEN_FILE`, re-read
  when Vault answers 403. `deploy/compose/docker-compose.vault.yaml` is
  the compose overlay.

wardynd logs in at boot and **refuses to start if it cannot**, renews its token
at two thirds of its TTL, and logs in again if a renewal fails. TLS uses
`WARDYN_VAULT_CACERT_FILE`, else `WARDYN_TRUSTED_CA_FILE`, else the system
roots, in a TLS config of its own; `http://` is refused except to a loopback
host.

**When Vault is unavailable.** A sealed, throttled or unreachable Vault (a 429, a
5xx, a timeout; each call retried three times first) is *transient*: the
credential sink answers the proxy 503, "Wardyn couldn't reach the service that
holds this run's credential", distinct from a missing credential's 424. A run
already using the credential keeps injecting the last value it read for up to
15 minutes past that value's expiry (a stored key's is ten minutes after it was
read), asking again every 30 s.
A 401 or 403, a value that is gone, or a binding that does not match is
*definitive*: revoking Wardyn's Vault role bites at once. (A 401 or 403 makes
wardynd log in again, or re-read its token file, at most once every 30 s.)
**Do not restart wardynd during a Vault outage**: its boot keys are in Vault,
so it will wait for Vault rather than boot.

**Moving an install to Vault, and back.** Online, one row per transaction, safe
while a daemon serves; idempotent and resumable.

```sh
# 0. Boot this version once with your WARDYN_AGE_KEY (it converts any
#    pre-envelope rows), and take the Postgres dump (see Backup).
# 1. Configure WARDYN_VAULT_* and WARDYN_SECRET_STORE=vaultkv, keep
#    WARDYN_AGE_KEY set, and restart: new writes go to Vault, old rows still read.
# 2. Move the rest:
wardynd -migrate-secrets -to=vaultkv
#    INFO wardynd: stored secrets migrated to=vaultkv moved=7 soft_deleted=0
# 3. Unset WARDYN_AGE_KEY and restart. Boot refuses, naming the command above,
#    while any local row remains.
```

`-to=local` moves every row back (it needs `WARDYN_AGE_KEY`); each value is
removed from Vault once its row holds it locally. Each run writes one
`secret.migrate` audit row and one `secret.read` per value it moved. A
migration never overwrites a value already at the target path: if one is there
(a write landing at the same moment, or a leftover), it stops and names the row.

**Checking both sides.** `wardynd -reconcile` lists the pointer rows and the
Vault paths side by side and reports pointers whose value is gone and values no
row points to. It reads metadata only, deletes nothing, and exits non-zero when
it finds either. A crash between the two writes of a Put or a Delete is what
produces one; neither leaks a value to anyone.

**Erasure horizon.** A removed credential is gone from Vault at once (all
versions); what survives is your Vault storage's own snapshots and backups,
under your retention. **Backup** in store mode is the Postgres dump plus your
Vault's own backup: the dump alone holds pointers, not values.


## Store mode: credentials in Azure Key Vault

With `WARDYN_SECRET_STORE=azurekv`, every stored credential's value lives in
your organisation's Azure Key Vault as a secret, and Wardyn keeps only a
pointer row in Postgres (`enc_version` 2, `kek_id`
`azurekv:<vault-host>/<secret name>#<n>`, no ciphertext). Everything the Vault
section above says about pointer rows, the boot keys, `-migrate-secrets` (here
`-to=azurekv|local`), `-reconcile`, and transient versus definitive failures
applies unchanged. No Azure SDK is involved: the Entra token exchange and the
Key Vault calls are plain HTTPS. One external store is configured at a time;
to move from Vault to Key Vault, migrate to local first.

**Use a vault dedicated to Wardyn** (Microsoft's "a vault per application"
advice), and give wardynd's identity **Key Vault Secrets Officer** on it and
nothing else. Every read is a `SecretGet` in the vault's `AuditEvent` log, with
wardynd's identity and the secret's URI. Secrets Officer also grants backup,
restore and recover, which Wardyn never calls; the least-privilege alternative
is a custom role with exactly these `dataActions`:

```
Microsoft.KeyVault/vaults/secrets/getSecret/action
Microsoft.KeyVault/vaults/secrets/setSecret/action
Microsoft.KeyVault/vaults/secrets/readMetadata/action
Microsoft.KeyVault/vaults/secrets/update/action
Microsoft.KeyVault/vaults/secrets/delete
Microsoft.KeyVault/vaults/secrets/purge/action
```

Leave out `purge/action` to withhold purge (see "Removing a credential" below).

**Names, versions and the owner check.** Key Vault names cannot hold `/`, `@`,
`.` or `_`, so a secret's name is derived from its row, one per owner and name:

```
<prefix>-<platform|operator|people>-<32 hex of SHA-256(owner, name)>-g<generation>
```

`<prefix>` is `WARDYN_AZURE_KV_PREFIX` (the chart sets the release namespace).
Each value is the base64 of the bytes (content type
`application/octet-stream;base64`, at most 18 KiB) with tags `wardyn-owner`,
`wardyn-name`, `wardyn-kind` and `wardyn-format`. The kind (`platform` for
wardynd's boot keys) is in the name and the tag, so your Key Vault logs can
tell the two apart; Key Vault has no per-name policy, so unlike two Vault roles
it does not separate the privilege. A read derives the name from
the row's owner and name and refuses a row that points to any other name or
vault, then refuses a value whose tags name another row. A replace is a new
**version** of the same name, and every earlier version is **disabled**: Key
Vault cannot delete old versions. A write lists the versions before it writes
and disables only those, so it never disables a newer one, and writes to one
credential wait for each other across replicas. Once the name holds
`WARDYN_AZURE_KV_MAX_VERSIONS` versions (default 100, well under the 500 at
which Key Vault's backup of a secret fails; the vault's own count decides, not
the pointer row), the next write starts a new generation, a fresh name, and
the old generation is deleted. A read that loaded the row just before such a
write committed can find the old name already deleted: it is refused once, with
no grace, and the next read follows the row to the new name. Each write is one transaction in the vault's
secret-create limit (300 per 10 seconds, shared with key and certificate
imports), plus a version listing and one update per version it disables.

**Authentication.** There is no client secret, by design.

- *Workload identity* on AKS (`WARDYN_AZURE_AUTH=workload-identity`, the
  default). With `secretStore.azure.*` set, the chart labels the pod
  `azure.workload.identity/use: "true"` and annotates the service account with
  `azure.workload.identity/client-id`; the webhook projects a token and sets
  `AZURE_FEDERATED_TOKEN_FILE`, which wardynd re-reads at every exchange.
  Add a federated credential for the chart's service account:

  ```sh
  az identity federated-credential create --name wardyn \
      --identity-name <identity> --resource-group <group> \
      --issuer "$(az aks show -n <cluster> -g <group> --query oidcIssuerProfile.issuerUrl -o tsv)" \
      --subject system:serviceaccount:<namespace>:<the chart's service account> \
      --audience api://AzureADTokenExchange
  az role assignment create --role "Key Vault Secrets Officer" \
      --assignee <the identity's client id> --scope <the vault's resource id>
  ```
- *Managed identity* on a VM (`WARDYN_AZURE_AUTH=managed-identity`): the
  instance metadata service, never through a proxy. `WARDYN_AZURE_CLIENT_ID`
  selects a user-assigned identity.

wardynd gets a token at boot and **refuses to start if it cannot**, then keeps
it until five minutes before it expires. TLS uses `WARDYN_TRUSTED_CA_FILE`,
else the system roots, in a TLS config of its own; `http://` is refused except
to a loopback host. The default NetworkPolicy denies wardynd's egress: allow
the vault and `login.microsoftonline.com` in `networkPolicy.egress.extra`.

**When Key Vault is unavailable.** A 429, a 5xx, a timeout or any token
endpoint failure is transient (each call retried three times first, honouring
`Retry-After`). A token endpoint refusal is transient too: Entra answers
`invalid_client` for passing conditions (`AADSTS700024`, a projected token
outside its valid time while the kubelet refreshes it), so it rides the
15-minute grace. A 401 fetches a new token at most once every 30 s; a 403, a
secret that is gone or disabled, or a binding that does not match is
definitive. Revoking wardynd's role at the vault bites at once; removing its
federated credential bites when its cached token expires (within the hour) and
the vault then refuses the call. A write, the wait for the credential's lock
included, gives up after six times `WARDYN_SECRET_STORE_TIMEOUT` (30 s by
default), so an outage never holds a database connection longer.

**Removing a credential, and the erasure horizon.** Removal is a soft delete
of the secret (every version), then, with `WARDYN_AZURE_KV_PURGE=auto` (the
default), a purge. Withholding purge is your choice: purge protection on the
vault, or a custom role without the purge permission. Then the purge is
refused, the secret stays soft-deleted, and the `secret.delete` audit row says
`purged: false` with `recoverable_days`, the vault's retention (7 to 90 days,
fixed when the vault was created). `WARDYN_AZURE_KV_PURGE=never` never purges.
Until then your organisation can recover the value; ask the vault's operators
to purge it sooner. `wardynd -reconcile` lists each soft-deleted value no
row points to, with the days until the vault purges it (a listing, not a
failure), and `-migrate-secrets -to=local` counts the old copies it left
soft-deleted (`soft_deleted` in its log line and `secret.migrate` row). Wardyn
never recovers a deleted secret: a credential removed and added again within
the retention reuses the name after a purge, or takes a new generation. After the purge, what survives is your vault's own
backups. **Backup** in store mode is the Postgres dump plus the vault's.


