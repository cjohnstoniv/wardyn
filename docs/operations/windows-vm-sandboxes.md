> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Windows VM sandboxes on a runner

- A Windows VM sandbox (`machine_kind: windows_vm` on a run) is a Windows guest under QEMU/KVM, started by an organization runner that offers it.
- It is desktop-only: a person signs in to the Windows desktop in the browser. There is no agent, terminal, exec or folder in the guest.
- Windows VM sandboxes have not yet been tried with a real Windows image on a real host. The checks that run on every backend change cover the VM's settings, the licence pool's limits, the network path and the start of the VM's firmware. They do not boot Windows.

## Licensing

- Wardyn does not supply, download or activate Windows. Your organisation registers its own image and is responsible for its licences.
- Licensing is your organisation's: the licence pool records who may launch and is not a licence. The audit is the record of who ran which image, and when.

## Turning it on

Two keys are needed, and both are off by default:

| Key | Who sets it |
|---|---|
| `features.windows_vm.enabled` | An admin, in the site config |
| `WARDYN_RUNNER_WINDOWS_VM=1` | The runner's operator, on a Docker runner that also offers [Linux VM sandboxes](linux-vm-sandboxes.md) ([ENV](../ENV.md)) |

- While the site key is off, a Windows VM launch is refused `403` `windows_vms_off`. The pool and image routes answer reads, refuse adds and allow removals.
- Turning the key off ends no running VM.

## Pools, holders and images

| Route | Who | What it does |
|---|---|---|
| `/windows-licence-pools` | Admin | A pool is a count of seats. `seats` is never below its holder count |
| `/windows-licence-pools/{id}/holders` | Admin | A holder is one person with one seat. A pool takes no more holders than seats |
| `/windows-licence-pools/{id}/seats` | Admin | The live seats: the run, its holder and since when |
| `/windows-images` | Admin or `security_admin` | An image is a digest-pinned ref in one pool, by a name people launch it by |
| `GET /me/windows-images` | Any person | The active images in pools you hold a seat in |

- A pool update and an image update take the `revision` they read. A stale one is refused `409` `windows_licence_conflict`.
- An image's ref never changes. Register a new image, then retire the old one.
- A retire, a pool delete, a pool move or a holder removal is refused `409` `windows_licence_seat_live` while a live run holds that seat.

## What a run takes

- A run names its image by name in `windows_image`, and starts as an interactive desktop on an organization runner.
- Each holder runs one Windows VM at a time. A second launch is refused `409` `windows_licence_in_use`, naming the run that holds the seat.
- A run that ends frees its seat. A run marked lost keeps it until it is killed, so a VM that may still be running is never counted twice.
- A Windows VM run is never recorded. A launch that would be recorded is refused, with `field` `recording`.
- A Windows VM run needs recording off in the policy that launches it. On an install that keeps recordings and sets no level, a plain launch is refused until that policy says off.
- An agent, a task, a workspace, a folder, a drive, web apps, exec, SSH, revive and support sessions are refused `windows_vm_feature_refused`.
- Every refused launch writes a `run.create` row with status `denied` and the reason ([audit actions](../AUDIT-ACTIONS.md)).
