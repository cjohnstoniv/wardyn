> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# User drives, and reclaiming a departed person's storage

Per-person persistent storage on Docker: the two backends, the labels that identify an
object, the share recipe, and the ordered reclaim procedure for someone who has left.

## Reclaiming a departed person's storage

- Deleting a drive removes its row and deleting an allocation stops the mount; neither deletes a byte.
- The storage object one person's allocation resolved to — a Docker named volume, a PersistentVolumeClaim, or a directory on a share — outlives both.

> [!WARNING]
> **Reclaiming it destroys data and nothing undoes it.**

- On Kubernetes, Wardyn deletes the claim; what happens to the bytes then follows the StorageClass's `reclaimPolicy`: `Delete` (the usual default) destroys them, `Retain` leaves them on the released PersistentVolume until an operator removes it.

There are two supported ways, and both stay supported: the substrate command by hand (the recipes in "[User drives on Docker](#user-drives-on-docker)" and "[User drives on Kubernetes](../OPERATIONS.md#user-drives-on-kubernetes)" below), or the product's own verb.

**The verb.** `POST /api/v1/drives/{id}/reclaim`, body `{"subject_type":"user","subject":"<the person's sign-in subject>"}`, or from the CLI:

```sh
wardyn drive reclaim <drive-id> --subject <sign-in subject> --yes
```

- It answers `deleted` (this call destroyed the storage) or `already_absent` (nothing answered to the name).
- **There is no console button**: a destructive confirmation is a screen, and this one has no approved mock, so the API and the CLI are the whole surface in 0.8.

**Do it in this order.**

- Reclaim the storage **first**, then delete the allocation.
- A home directory an admin pinned (`home_override`) lives on the allocation,
  - so once that row is gone the pinned name cannot be recovered from the database and the object name this verb derives is the drive template's instead — a different directory.
- Check the name it reports against `POST /drives/preview`, which prints the object name for a principal.

**What refuses it, and why each one is there:**

| Refusal | What it means |
| --- | --- |
| `403` | Not a super-admin. Same tier as the rest of `/drives`, for a sharper reason: this one is irreversible |
| `400` | The `subject_type` is `group` or `all`. Those give **every** person they match their own object, so they name no single thing to destroy — reclaim the people one at a time |
| `422` | The drive is a share (`host_path`, `k8s_pvc_static`). Wardyn did not create that object and never deletes it; reclaiming it is a change on the share itself. There is no recursive delete in this product, at any privilege, for any backend |
| `409` | A run still holds the object, a reclaim is already in flight, or the object is not this drive's; see [below](#409-refusal) |
| `501` | This deployment's runner cannot reclaim at all — use the substrate command |

**On Kubernetes the daemon does not even hold the verb by default.**

- The chart's Role carries `persistentvolumeclaims: [get, create]` and adds `delete` only under `drives.reclaim.enabled` (default `false`, see [the chart's values](../../deploy/helm/wardyn/values.yaml)).
- Leave it off and every attempt ends in the apiserver's own `403`, recorded as a failed `drive.reclaim` row; turn it on only when your offboarding runbook calls the API instead of running `kubectl delete pvc` by hand.
- Nothing else changes either way: no run path, no teardown and no sweep can reach a claim on either setting.

**Every attempt that reaches the substrate is audited**, `409` refusals and failures included, as `drive.reclaim` — naming the drive, the person, the backend, the object and what became of it ([AUDIT-ACTIONS.md](../AUDIT-ACTIONS.md)).

- That row is deliberately the only durable record: the drive row and the allocation can both be gone by the time anyone reads the trail.
- The `400`, `422` and `501` answers above, and a daemon that cannot read the allocation, are refused before any object is addressed and write no `drive.reclaim` row.
### 409 refusal

- A run still holds the object, a reclaim is already in flight (a claim already `Terminating`), or the object answering to that name is **not this drive's**
- The driver re-checks the `wardyn.drive` / `wardyn.home` / `wardyn.subject` labels before issuing any delete,
- and on Kubernetes binds the delete to the claim it checked: a claim deleted and re-created under the same name in between is refused, never deleted
- Docker's volume remove takes no such precondition.
## User drives on Docker

- A **user drive** is persistent storage an admin registers once and allocates to people or groups; a member mounts theirs per run at `/home/agent/drive`.
- On a Docker deployment there are two backends, and the difference is who owns the bytes.
- With `storage.user_drive.disabled` set, all three surfaces answer that one switch identically:
  - a run is refused 422,
  - `POST /drives/preview` answers the same 422,
  - and `GET /me` reports no allocation at all.
- So nothing in the console ever offers a mount the create path refuses (see "Turning drives OFF deployment-wide").

- **`docker_volume` — Wardyn allocates.** A per-person named volume (`wardyn-drive-<drive-slug>-<home>`), created on first use with the `local` driver and mounted at the reserved target.
  - Nothing to configure.
  - It carries four labels:
    - `wardyn.managed=true`;
    - `wardyn.drive` = the **drive row's id** (the name folds the drive's SLUG, which a rename changes, and the id never does);
      - So the label is the only key that still finds a drive's volumes across one, which is what the reclaim recipes below select on;
    - `wardyn.home` = that person's directory name; and `wardyn.subject` = a **digest** of the person themselves (never their claim — see the restore note below).
  - Reclaim is a command, never a button — either `wardyn drive reclaim` ("[Reclaiming a departed person's storage](#reclaiming-a-departed-persons-storage)" above) or, by hand:
    - one person: `docker volume rm wardyn-drive-<drive-slug>-<home>` — `POST /drives/preview` prints the object name for a principal —
      - paste the sign-in subject FIRST: on a `hash` drive the name keys on the first claim, and the API's `home_subject` says which claim it used (the console does not yet show it);
    - one drive, everybody: `docker volume ls --filter label=wardyn.drive=<drive id>` lists every volume that drive allocated.

- **Restoring one by hand: re-create it with its labels, and with no `--opt`.**
- Wardyn reuses a volume that already answers to the name, but only when it has Wardyn's own shape —
  - the `local` driver and **no driver options** —
- and refuses to mount anything else rather than adopt it.
- That refusal is deliberate: a volume an operator precreated with `--opt type=cifs --opt o=…,password=…` would otherwise become somebody's drive, on a share credential Wardyn never chose.
- So a restore is

```
docker volume create \
  --label wardyn.managed=true \
  --label wardyn.drive=<drive id> \
  --label wardyn.home=<home> \
  wardyn-drive-<drive-slug>-<home>
```

- then copy the data in.
- `wardyn.drive` carries the **drive row's id** (the `id` on `GET /api/v1/drives`, and the `Target` of that drive's `drive.write` audit row), not the volume's name — the id is what groups every person's object under the drive that allocated them.
- Get it wrong and Wardyn **refuses** the volume rather than adopting it: a label naming a *different* drive is how two drives whose home names collided would otherwise hand one member the other's storage.
- A volume restored with **no** `wardyn.drive` label at all still mounts (that is the fall-back this path is for, and every volume created before the label carried an id has none).
- It just no longer answers `docker volume ls --filter label=wardyn.drive=<drive id>`.

- Wardyn also stamps **`wardyn.subject`**, a digest of the person the volume was allocated to — never their sign-in claim, because `docker volume inspect` echoes labels to anyone who can reach the daemon.
- It is the discriminator `wardyn.drive` cannot be: a volume name carries the drive and the *home* and no person at all (`DriveObjectName` mints `wardyn-drive-<drive-slug>-<home>`).
- So one drive whose home template folded two people onto one directory would produce one volume that *both* their allocations agree belongs to this drive.
- Wardyn refuses to mount a volume stamped for a different person.

- A managed drive can no longer be *authored* into that state.
- The rule is `ManagedBackendRejectsTemplate` in [`internal/types/user_drive.go`](../../internal/types/user_drive.go).
- And as of 0.7 it refuses **every** non-`hash` template on a `docker_volume` or `k8s_pvc` backend — `sub` as well as `email_local` — at **both** enforcement points: the write boundary, and the run-time resolver that derives the home.
- It used to name `email_local` alone, and the resolver keyed on `email_local` alone, so a `sub` row written by an older binary (or by hand) was refused on write and still mounted.
- The folded homes `wardyn.subject` discriminates are therefore rows from before that widening, or hand-made ones — which is exactly why the label is still checked rather than assumed away.

- You need not compute the digest for a restore (it is a truncated sha256 of the sign-in subject): **leave `wardyn.subject` off** the `docker volume create` above and the volume mounts, exactly as a label-less `wardyn.drive` does.

- **`host_path` — you already mount the share.** Wardyn binds **one person's subdirectory** of a tree the *operator* mounted host-side.
- Wardyn never performs the share mount, never holds a share credential, and never creates a volume with `--opt type=cifs`: those options are stored with the volume and echoed by `docker volume inspect` to anyone who can reach the daemon.
- The recipe:

1. **Mount the share on the host**, in `fstab` or a systemd mount unit:

   ```
   # SMB — the credential is a root-owned 0600 file, never a mount option in a table
   //nas.corp/wardyn-drives /srv/wardyn-drives cifs credentials=/etc/wardyn/smb.cred,uid=1000,gid=1000,file_mode=0600,dir_mode=0700,vers=3.1.1 0 0
   # or Kerberos instead of a service account: replace credentials= with sec=krb5
   # NFS — export it Wardyn-dedicated and squashed to the sandbox uid
   nas.corp:/export/wardyn-drives /srv/wardyn-drives nfs4 rw,hard,_netdev 0 0
   ```

   The matching NFS export line, on the NAS:
   `/export/wardyn-drives 10.0.0.0/8(rw,all_squash,anonuid=1000,anongid=1000)`.

2. **Make one `0700` subdirectory per person** under the mount point, named the way the drive's home template resolves.

   - There are three templates:
     - `hash` (a digest of the drive id and the subject),
     - `sub` (the sign-in subject claim verbatim) and `email_local` (the part of the email claim before the `@`, the usual shape of a corporate home)
   - and a **share** drive may only use `sub` or `email_local`: a hash would name a directory nobody created.

   - Per person, a grant's *home override* pins any other name.
   - Wardyn does **not** `mkdir` on a share.
   - A missing home is a `422` at run create ("directory `<home>` does not exist on the share — ask an admin to create it"), not a directory Wardyn invents inside somebody's NAS.

   - Two people whose email addresses share the part before the `@` resolve to the **same** home under `email_local` — the segment is validated, not proven unique.
   - On a share that is a tree you own and can inspect: use `sub`, or a per-person home override, where it can happen.

   - **A managed drive takes `hash` and nothing else.**
   - As of 0.7 the refusal covers **every** non-`hash` template on a `docker_volume` or `k8s_pvc` backend — `sub` as well as `email_local`.
   - Registering either answers a `400` beginning `invalid drive: home_template "<template>" is not allowed on a managed backend`, and the message names `hash` as the single remedy.
   - Two reasons, and `sub` fails the second one:
     - Wardyn names a managed object after the drive and the home and nothing about the person (`wardyn-drive-<drive-slug>-<home>`),
       - so within one drive `email_local` allocates two colliding people one volume with write access to each other's files whenever the drive is writable
     - and an object *name* is what `docker volume ls` and `kubectl get pvc` print with no inspect or describe,
       - so a verbatim `sub` publishes the sign-in subject to anyone who can list the daemon or the namespace, in the more exposed of the two places the label vocabulary already refuses to put it.
   - `hash` is unique and reveals nothing, and it is the default; a **share** backend keeps every template, because its tree is one you own and navigate by hand.
   - A row written before this rule is refused at *run* time too (`drive: this deployment cannot mount your drive (…)`).
   - And every managed volume carries a `wardyn.subject` label — a digest of the principal, never the claim — that the driver refuses to mount for anybody else.

3. **Set the ceiling**: `WARDYN_USER_DRIVE_HOST_ROOTS=/srv/wardyn-drives` ([ENV.md](../ENV.md)).

   - Unset means **no `host_path` drive may be registered at all** — the same fail-closed posture `WARDYN_USER_WORKSPACE_ROOTS` takes, one level up:
     - a drive's `host_root` is authored in the database by an admin and its subdirectories are bound into *other people's* sandboxes,
     - so the allowlist over it lives where a console compromise cannot reach it.
   - The driver re-checks the **symlink-resolved real path** against these roots as the last thing before the container is created,
     - so a home directory replaced by a symlink out of the share after the drive was registered is refused at run time too.
     - And it adds two checks the ceiling cannot make, because every other drive's tree and every sibling home are inside it as well.
   - The resolved directory must be **inside this drive's own `host_root`**, which catches a home replaced by a link into *another* `host_path` drive's root
     - (a ceiling naming both roots allows either tree, so it cannot tell one drive's from the other's);
   - and it must still be **named after the person it resolved for**, which catches a home replaced by a link to the home *next to it*.
   - A home symlinked deeper inside its own drive's root — homes filed under a year or a department — still works, as long as the directory keeps its name.
   - A home symlinked onto a *second export* no longer does, even when that export is also a configured root.
   - Give the drive the root its homes actually live under, or register a second drive for the second export.

   - **Two `host_path` drives may not nest.**
   - Registering a drive whose `host_root` is inside — or contains — another `host_path` drive's `host_root` answers `422`, naming the other drive.
   - Two `host_path` drives may share one `host_root` — a read-write and a read-only view of `/srv/homes` is a supported shape, and only a NESTED root is refused — and so are sibling trees.
   - What is refused is one drive rooted inside a tree whose directories another drive's members can rewrite from inside a run.
   - They must agree on `home_template`, and a second one that disagrees is refused `409`:
     - a share's storage object is `<host_root>/<home>` with no drive component,
     - so two different derivation rules over one tree hand two different members the same directory (a member whose `sub` is `alice` and a member whose address is `alice@corp.example` both derive `alice`).

   - The question is asked on the stored strings **and again on the symlink-resolved paths**, and either answer refuses.
   - A root that is a link into the other drive's tree nests exactly as surely as a literal path does.
   - So the refusal **names where each root resolves** whenever that differs from what was typed: *host_root "/mnt/teamshare" (resolves to "/srv/shares/alice/team") is inside drive "Corp NAS"'s host_root "/srv/shares" — …*.
   - Without it an admin reads a refusal about two paths that plainly do not nest.
   - And the one fact that explains it — the hop the link makes — is the one thing the console form cannot show them.
   - A root that no longer resolves on this host falls back to the lexical answer rather than to a refusal, so one dead row cannot block every new drive (`driveHostRootNesting`, `internal/api`).
   - Two admins creating nested drives at the same instant can still both be stored — the gate is a read followed by an unconditional write, and the database-level form is 0.7.1.

   - **And the drive ceiling must not overlap `WARDYN_USER_WORKSPACE_ROOTS` — the member ceiling defeats per-person isolation where they meet.**
   - Per-person isolation is the **bind of the subdirectory**: Wardyn hands a run one home out of the share and refuses a source that resolved to the root.
   - A member workspace is a different surface with a different rule — a member names a directory under `WARDYN_USER_WORKSPACE_ROOTS` and binds it **whole**, writable where `WARDYN_USER_WRITABLE_ROOTS` allows it, and that path consults no drive allocation at all.
   - Point the two ceilings at one tree and a member onboards the share as a workspace and mounts **every** person's home.
   - Each list is valid on its own, so wardynd compares the pair at boot and **WARNs**.
   - It does not refuse, because an operator may have opened a tree to both deliberately and a boot refusal would take a running deployment down on upgrade (`MountCeilingOverlapWarnings`, [`internal/runner/user_drive_mount.go`](../../internal/runner/user_drive_mount.go)).
   - Three shapes earn the line, each behind the prefix `wardynd: mount ceilings overlap — `:

   | Shape | The line says |
   |---|---|
   | The two lists name the same tree | The line is quoted under [The two lists name the same tree](#the-two-lists-name-the-same-tree) below. |
   | A member root CONTAINS a drive root | The line is quoted under [A member root CONTAINS a drive root](#a-member-root-contains-a-drive-root) below. |
   | A member root is INSIDE a drive root | ``WARDYN_USER_WORKSPACE_ROOTS contains "<m>", which is INSIDE the WARDYN_USER_DRIVE_HOST_ROOTS entry "<d>": member workspaces would be authored inside a share whose directories Wardyn hands out one person at a time. Point the member ceiling outside the share`` |

   - Every member ceiling is compared, the shared list **and** each `WARDYN_USER_WORKSPACE_ROOTS_MAP` per-principal override — an override *replaces* the shared list, so it is a ceiling in its own right.
   - The comparison is **lexical**, on the values as configured: boot is not the place to touch a share that may not be mounted yet.

   - **What the operator gets when a bind is refused.**
   - The member-facing hint from a driver-side share refusal carries the drive and the directory and never a path.
   - The paths go to the log, on the same run, as `wardyn: user drive: this share mount was refused at bind time` with `source`, `real_path` and `host_root` attributes (`RefuseUserDriveBind`, [`internal/runner/user_drive_mount.go`](../../internal/runner/user_drive_mount.go)).
   - That split is the rule everywhere on this path — see the member's own refusal vocabulary in [docs/design/user-drives-prompt.md](../design/user-drives-prompt.md) §7.7 and §7.9.

- **On the Compose stack, wardynd must be able to SEE the root — set two variables.**
- The bind's source is resolved by the host daemon (wardynd's sandboxes are sibling containers), but the ceiling check resolves symlinks and fails closed on a path it cannot stat.
- So a `host_path` drive registered from a containerised wardynd is refused unless the share is visible inside it too.
- `docker-compose.yaml` carries both halves already — nothing to hand-edit:

```
WARDYN_USER_DRIVE_HOST_ROOTS=/srv/wardyn-drives   # the ceiling wardynd enforces
WARDYN_USER_DRIVE_HOST_ROOT=/srv/wardyn-drives    # compose binds this one, RO, same path
```

- in `deploy/compose/.env` (or the environment `docker compose` is run with).
- Unset, both default to nothing exposed — the same opt-in posture `WARDYN_WORKSPACES_ROOT` and `WARDYN_USER_WORKSPACE_ROOTS` take.

- **One root on Compose.**
- The ceiling is a CSV and may name several roots; the bind is singular, because compose cannot expand a CSV into volume lines.
- A deployment whose ceiling names more than one root adds one more volume line per extra root in [`deploy/compose/docker-compose.yaml`](../../deploy/compose/docker-compose.yaml), copied from the `WARDYN_USER_DRIVE_HOST_ROOT` line
  - or runs wardynd on the host, or on Kubernetes, where no bind is involved and the ceiling is the only thing to set.

- Read-only is enough for wardynd: it stats the tree and never writes to it.
- It does need **search (`x`) permission down to the person's directory**, though, because the bind-time ceiling check resolves symlinks in *wardynd's own process*.
- So a CIFS mount table line like the `dir_mode=0700,uid=1000` one above works when wardynd runs as root or as uid 1000, and otherwise needs `dir_mode=0750,gid=<wardynd's gid>` (or the equivalent NFS export mode).
- A share wardynd cannot traverse fails every drive on it closed at run create:
  - with `drive: this deployment cannot mount your drive (drive "<name>" is on a share this deployment does not allow — ask an admin)` when the **root itself** is what wardynd cannot resolve,
  - and with `drive: directory <home> does not exist on the share — ask an admin to create it` when the root resolves but the person's directory does not stat
    - (a home that was never created, and a home behind a directory whose permissions hide it, are the same sentence).
- **Neither 422 names a path**, deliberately: both are read by the MEMBER,
  - so the diagnosis — the drive's `host_root`, the whole `WARDYN_USER_DRIVE_HOST_ROOTS` list, and the check's own sentence — goes to wardynd's log instead, as `wardynd: user drive: a stored share drive's host_root is no longer allowed by this deployment`,
  - which is where the admin who can act on it is looking (`driveShareIsBindable`, [`internal/api/user_drives_run.go`](../../internal/api/user_drives_run.go)).
- The sandbox's own mode comes from the allocation, not from this line.
- A `docker_volume` drive needs none of this — there is no host path to see.

- **Why every sandbox is uid 1000, and what that buys.**
- Every agent image is `USER agent` (uid 1000), and every agent image pre-creates `/home/agent/drive` owned by agent
  - the ones built on a public base do it themselves, the ones built on a sibling image inherit it
- So a fresh managed volume inherits that ownership by Docker's copy-up.
- Isolation between people is the **bind of the subdirectory**, never the uid: a run sees its own home and has no path to the root or to anyone else's.
- NFS `AUTH_SYS` trusts the client's uid, which is why the export above is Wardyn-dedicated and squashed rather than a corporate home tree.
- Existing corporate home directories owned by per-user uids are supported read-only where uid 1000 can read them.
- Where it cannot, Wardyn does **not** refuse — the directory only has to EXIST for wardynd's own uid (`driveShareIsBindable`), so the mount succeeds and the agent sees permission denied at first access.

- A **BYOI** image is your own to get right on this one point: a custom base that never creates `/home/agent/drive` gets a root-owned one from the daemon at mount time.
- So a drive you allocated writable is unwritable by uid 1000 on its first run.
- [`deploy/images/README.md`](../../deploy/images/README.md)'s image contract states the one line that fixes it; wardynd will not chown volume state to compensate.

- **gVisor (CC2): if a share bind misbehaves under `runsc`, turn `directfs` off.**
- Wardyn does not claim this is required — `runsc`'s own filesystem guidance ([gvisor.dev](https://gvisor.dev/docs/user_guide/filesystem/)) is the reference, and whether a given network-backed mount needs direct host-FD access disabled depends on the share.
- If a `host_path` drive reads or writes wrongly under CC2 and works under CC1, this is the first thing to try.
- It is a **daemon** setting, not a Wardyn one — add it to the runtime in `/etc/docker/daemon.json` and restart the daemon:

```json
{ "runtimes": { "runsc": { "path": "/usr/local/bin/runsc", "runtimeArgs": ["--directfs=false"] } } }
```

- CC1 (`runc`) and CC3 (Kata) need nothing.
- Wardyn's own runsc tweaks are unchanged: this is an operator recipe, and the product does not rewrite your daemon config.

- **A READ-ONLY share loses its RECURSIVE guarantee under `runsc`, and says so in the log.**
- A read-only bind's `ro` reaches SUBMOUNTS only when the runtime declares the OCI `rro` mount option.
- And gVisor does not (`runsc features` lists `ro` and `rbind` and no `rro`), while the daemon **refuses the create outright** for a runtime that does not.
- So Wardyn asks for it only where it is declared (`runtimeSupportsRecursiveReadOnly`, [`internal/runner/docker/hardening.go`](../../internal/runner/docker/hardening.go); `driveBindOptions`, [`internal/runner/docker/driver_mounts.go`](../../internal/runner/docker/driver_mounts.go)):
  - the bind still goes in read-only, and a submount **under** the person's home — an autofs home, a second export mounted below the first — can be writable inside the sandbox.
- wardynd WARNs on the run it affects, with the drive and the home:

```
wardyn: user drive: this runtime does not support recursively read-only binds,
so a submount under the share's home could be writable inside the sandbox
```

- Asking unconditionally is not the alternative: it made every CC2 run with a read-only drive fail at `ContainerCreate` with the daemon's `rro is not supported by runtime "runsc"` as the member's failure hint.
- There is one lever — run the drives that need the recursive guarantee at **CC1**, where the daemon's default `runc` declares `rro`.
- Nothing in the sandbox is affected when the share carries no submounts.

- **What a drive's SIZE means here.** Quoted verbatim, and the same sentence the console renders:

> Wardyn never enforces a drive's size itself. On Kubernetes the size is the
> volume request and the storage class decides whether it binds — block disks
> do, network-share provisioners do not. On Docker a managed drive has no byte
> cap, the same gap disk_mib has. A share is bounded by its own quota. The
> size you see is the allocation, not a guarantee.

- Concretely on Docker: a `docker_volume` drive reports `enforcement: none` — `--storage-opt size` caps only a container's writable layer, never a volume, and an XFS project quota needs `CAP_SYS_ADMIN` the control plane must not hold.
- A `host_path` drive reports `enforcement: external`: the NAS's own quota binds it, and Wardyn displays the allocation.

- **A real byte cap on Docker: an XFS project quota, run by the operator, on the host, never inside the control plane.**
- `CAP_SYS_ADMIN` is what WARDYN must not hold, not a statement that nothing can enforce a `docker_volume` drive's size.
- The recipe below is exactly the case `types.StorageEnforcementFilesystem` was named and reserved for ([`internal/types/user_drive.go`](../../internal/types/user_drive.go): "NOTHING in v1 reports this — it is the value the documented operator recipe earns").
- Wardyn still reports `enforcement: none` on the wire; this is an operator ceiling underneath it, invisible to the product and unaffected by a `wardynd` restart.

1. **The Docker data root must be XFS, mounted with project quotas.**

   - Find it with `docker info -f '{{.DockerRootDir}}'`, then confirm with `xfs_info <that path>` — the output must list `pquota` or `prjquota`.
   - A filesystem created without it needs a remount (`mount -o remount,prjquota <mountpoint>`, persisted in `/etc/fstab`) — a host operation, unrelated to Wardyn, that does not require restarting the daemon.

2. **Assign a project to the volume's own directory, one per drive per person.**

   - Resolve the real path rather than guessing the data root,
   - and resolve the XFS mount point rather than assuming it is the data root itself (a bind-mounted or LVM-backed data root is not always its own filesystem root):

   ```
   VOL=wardyn-drive-<drive-slug>-<home>                    # from the reclaim recipe above
   DIR=$(docker volume inspect -f '{{.Mountpoint}}' "$VOL")
   MOUNT=$(findmnt -T "$DIR" -no TARGET)                    # the XFS filesystem's own mount point
   PROJID=$(( 0x$(echo -n "$VOL" | sha256sum | cut -c1-7) )) # any stable project id, unique per volume
   echo "${PROJID}:${DIR}" >> /etc/projects
   echo "${VOL}:${PROJID}" >> /etc/projid
   xfs_quota -x -c "project -s ${VOL}" "$MOUNT"
   ```

3. **Set the hard limit, and confirm it actually refuses a write:**

   ```
   xfs_quota -x -c "limit -p bhard=20g ${VOL}" "$MOUNT"
   xfs_quota -x -c "report -p" "$MOUNT"
   ```

   - A run whose agent then writes past the limit meets the filesystem's own `ENOSPC` — the identical error path a genuinely full disk already takes.
   - Wardyn adds nothing to it and catches nothing from it; that is the whole point of a ceiling that lives below the product rather than in it.

- Recreating the volume — a restore, or Wardyn re-minting one after a delete — does not carry the quota forward:
  - step 2 keys on the volume's directory, which changes,
  - so re-run it (or script it as a step your own restore/create tooling runs after Wardyn's).
- A `host_path` share on an XFS-backed NAS can be capped the identical way, against the directory the NAS exports; that quota is the NAS's own, which is already what `enforcement: external` reports.

- **And a ceiling bounds what you may ALLOCATE, not what the volume will hold.**
- Two numbers can cap a drive, and they are refused and applied in different places.
- `storage.user_drive.max_size_mib` on the **Workspace providers** screen is the deployment's: a drive or an allocation override above it is refused at the write with **422 `size_mib … exceeds this deployment's drive ceiling`**.
- Not a 403, because nobody was denied anything, the deployment simply will not hold it.
- A governance profile's `max_drive_size_mib` is the **per-principal** one.
- And it cannot be refused at a write at all: the profile binding a subject is resolved from their claims, and a group or `all` allocation names no single principal.
- It is CLAMPED when the drive is resolved (`newResolvedDrive`), folded with the deployment's in one expression —
  - the smaller of the two wins, the daemon logs which one bit (`bound_by=deployment` or `bound_by=governance_profile`),
  - and the run, `GET /me` and `POST /drives/preview` all report the clamped number,
  - so the card cannot offer a size the run will not give.
- Lowering the deployment ceiling after drives exist refuses no run and rewrites no row; it clamps from the next resolve onward.

- On Docker that clamp is a number and nothing more:
  - `enforcement: none` on a managed volume, `external` on a share — so treat the ceiling as governance over what admins may write down, never as a cap on bytes.

- **Turning drives OFF deployment-wide** is `storage.user_drive.disabled` on the same screen, and it is a different question from a profile's `deny_user_drive`.
- The switch is asked FIRST and says *this install offers no drives*:
  - every drive and allocation write answers **422 "drives are disabled for this deployment"**,
  - a run asking for its drive is refused 422 in the same family,
  - and no `authz.denied` row is written, because no profile denied anybody.
- Every drive row and every allocation is KEPT — the screen still lists them, above a banner — so turning it back on restores exactly what was there.
- **Deletes are deliberately not refused**:
  - `DELETE /drives/{id}` and `DELETE /drives/grants/{id}` keep working while the switch is off,
  - so an operator can still tidy up or offboard somebody without turning drives back on first (the `ON DELETE RESTRICT` between the two is unchanged, so a drive still cannot be deleted out from under an allocation).
- What the switch refuses is every write that CREATES or EDITS one.
- The per-profile door is unchanged and still answers 403 with its `authz.denied` row.
- **All three read surfaces answer the switch identically**, off one site (`driveSizeCeilingFor`, [`internal/api/user_drives_resolve.go`](../../internal/api/user_drives_resolve.go)) so they cannot drift:
  - a run asking for its drive is refused 422,
  - `POST /drives/preview` answers the same 422 with the same sentence,
  - and `GET /me` reports **no** `user_drive` with `user_drive_unavailable: "unavailable"` — never an allocation the create path would then refuse.
### The two lists name the same tree

- ``WARDYN_USER_WORKSPACE_ROOTS and WARDYN_USER_DRIVE_HOST_ROOTS both name "<p>": a member can onboard that directory as a workspace and bind the WHOLE share, every other person's home included, without a drive allocation. Point the drive ceiling at the share and the member ceiling somewhere else``
### A member root CONTAINS a drive root

- ``WARDYN_USER_WORKSPACE_ROOTS contains "<m>", which holds the WARDYN_USER_DRIVE_HOST_ROOTS entry "<d>": a member can onboard that share as a workspace and bind it whole, every other person's home included, without a drive allocation. Point the member ceiling at a tree that does not contain the share``
