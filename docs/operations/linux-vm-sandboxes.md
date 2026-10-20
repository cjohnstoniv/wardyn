> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Linux VM sandboxes on a runner

- A Linux VM sandbox (`machine_kind: linux_vm` on a run) is a Linux guest under QEMU/KVM, started by a registered `wardyn-runnerd` that offers it.
- It is off by default. The runner's operator turns it on, and the runner offers it only after its start-up probe passes.
- This page is for the operator of a Linux runner host. There is no console control in this release: a run asks for it through the API or the CLI request body.

## What a run gets

| Part | What it is |
|---|---|
| The machine | A versioned q35 machine (for example `pc-q35-10.0`) and a named CPU model, both recorded on the run as `vm_machine` |
| The disk | The run's image converted once to a read-only ext4 base, plus the run's own copy-on-write overlay |
| The boot | Direct kernel boot of the image's own kernel and initrd, with `noresume` |
| The agent | The Wardyn guest agent in the guest starts the harness and serves terminals, as `agent` (uid 1000) |
| Shared folders | The folders a container on this runner would get (a member's folder under the runner's roots), at the same paths and read-only or writable alike, one unprivileged `virtiofsd` container each. Nothing else on the host is shared |
| Synced folders | A folder synced from another computer lands in the guest as in a container: through the guest agent and the image's `sftp-server`, as `agent`. The agent waits for the first sync |
| Confinement | The VM boundary, which the runner treats as the `CC3` family (hardware virtualisation). The run's recorded `confinement_class` stays the one it asked for |
| Network | None until the network relay ships: QEMU's only link goes to a socket nothing answers. Then every connection the desktop makes goes through Wardyn's network relay, which hands allowed ones to the proxy and drops the rest |

- Not in this release: drives, session recording, reviving a stopped VM, replacing its proxy, and recovering its output after the VM is gone. Each is refused with a reason.
- A VM's terminal is a plain shell. Opening it in the agent CLI (`interactive_start: agent`) and watching or taking over the agent's terminal are refused too.

## When the organisation is unreachable

| Situation | Behaviour |
|---|---|
| The link is down when a VM would start, or its first lease has lapsed | Refused `runner_offline`, as for a container. No VM, intent or run row exists |
| The link drops while a VM runs | The VM keeps running with its files. Its proxy keeps enforcing the policy it holds and spools decisions |
| The run token goes unrenewed for 1h5m | The runner stops the VM's proxy. The guest keeps running with no network path |
| A credential the proxy injects from an expiring grant | Dropped at its expiry plus 15 minutes if it cannot be renewed. A fixed one (an API key) is held until the proxy stops at 1h5m |
| The VM's audit spool fills (50,000 decisions or 32 MiB) | The runner asks the guest to power down, keeps the stopped VM and removes the proxy. The runner's log names the run |

- These rules apply only on a runner that offers Linux VM sandboxes. A container run keeps the hold-then-`503` for a full spool.
- The spool is self-reported by the runner, so it is as trustworthy as the computer. A resident credential stops at its own cap ([hybrid laptops](hybrid-laptops.md#offline-and-revocation)).
- Not in this release: a window that says the organisation is unreachable.

## Requirements

| Requirement | Detail |
|---|---|
| Host | x86-64 Linux with hardware virtualisation and the `kvm` module loaded (`/dev/kvm` exists) |
| Docker engine | A local engine that shares the runner's filesystem. Docker Desktop and a remote `DOCKER_HOST` are refused by the probe |
| `/dev/kvm` access | Its group is not `root`, or it is open to all (mode `0666`). The QEMU container gets that group, never root's |
| Runner user | Any user. A runner that runs as root starts its VM containers as uid 65532 |
| The QEMU image | Built from [`deploy/runner/vm-qemu`](../../deploy/runner/vm-qemu/Dockerfile) and named in `WARDYN_RUNNER_VM_QEMU_IMAGE` |

## Turn it on

1. Give `/dev/kvm` a group the runner can hand to its containers. Stock distributions ship it as `root:kvm 0660`, which already works. If it is `root:root`, install a udev rule and reload it:
   ```sh
   echo 'KERNEL=="kvm", GROUP="kvm", MODE="0660", OPTIONS+="static_node=kvm"' | sudo tee /etc/udev/rules.d/99-wardyn-kvm.rules
   sudo udevadm control --reload-rules && sudo udevadm trigger --name-match=kvm
   ```
2. Build the QEMU image on the runner's engine: `docker build -f deploy/runner/vm-qemu/Dockerfile -t wardyn/vm-qemu:local .`
3. Set `WARDYN_RUNNER_LINUX_VM=1` and `WARDYN_RUNNER_VM_QEMU_IMAGE=wardyn/vm-qemu:local` in the runner's environment.
4. Restart the runner. It logs `Linux VM sandboxes are on` with the machine type, CPU model and QEMU version, or `Linux VM sandboxes are off on this runner` with the reason and its fix.

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `WARDYN_RUNNER_LINUX_VM` | (unset, off) | `1` turns Linux VM sandboxes on |
| `WARDYN_RUNNER_VM_QEMU_IMAGE` | (unset) | The QEMU image every VM container starts from |
| `WARDYN_RUNNER_VM_CPU_MODEL` | `Haswell-v4` | The named CPU model every VM gets. QEMU starts it with `enforce`, so a host that lacks a feature refuses instead of giving the guest another CPU |

## The probe's refusals

| Log reason | Fix |
|---|---|
| this host has no /dev/kvm | Turn on virtualisation in the firmware and load `kvm_intel` or `kvm_amd` |
| /dev/kvm belongs to group root | Install the udev rule in step 1 |
| QEMU cannot open /dev/kvm in its container | Fix the group and mode of `/dev/kvm` (step 1), then restart |
| this host's CPU cannot run the VM CPU model | Set `WARDYN_RUNNER_VM_CPU_MODEL` to a model the host supports, for example `Skylake-Client-v4` or `EPYC-v3` |
| the Docker engine does not share this runner's filesystem | Use a local engine |
| no QEMU image | Build the image (step 2) and set `WARDYN_RUNNER_VM_QEMU_IMAGE` |
| cannot share a folder | The QEMU image's `virtiofsd` did not run. VMs stay on, but a VM run that carries a folder is refused. Use an image built from step 2 |

## The guest image contract

- A run's image is an ordinary OCI image. The reference image is [`deploy/images/vm-linux`](../../deploy/images/vm-linux/Dockerfile) (Debian 13, no desktop).
- It holds exactly one `/boot/vmlinuz-*` and one `/boot/initrd.img-*`, with virtio, ext4 and `qemu_fw_cfg` available. The root filesystem is `/dev/vda`.
- It runs systemd, `qemu-guest-agent` (with `hwclock`, which sets the guest clock), and the Wardyn guest agent at `/usr/local/bin/wardyn-guest-agent` with its unit enabled.
- It has the user `agent` (uid 1000, home `/home/agent`), as a container agent image does, and `/usr/lib/openssh/sftp-server`, which a folder sync writes through.
- It installs the run's CA from `/sys/firmware/qemu_fw_cfg/by_name/opt/org.wardyn/ca.pem/raw` on every boot, so a VM trusts its own run's CA and never another's.

## What the host can see

| On the host | Who can use it |
|---|---|
| `<state>/vm/runs/<run>/ctl/` (QMP, qemu-ga, the guest agent, VNC) | The runner's user only (mode `0700`). VNC has no password, so this directory's mode is its control |
| `<state>/vm/runs/<run>/net/` (the network relay's socket) | The runner's user only |
| `<state>/vm/images/<image id>/` (the converted base) | The runner's user only; shared read-only by every run of that image |

- QEMU listens on no TCP port. Its serial console goes to the QEMU container's log.
- fw_cfg carries the run id and the run's CA certificates, never a key or a secret.

## The run record

- Migration `0196_run_machine_kind` adds `agent_runs.machine_kind` (empty for every container run, as before) and `agent_runs.vm_machine`.
- A Linux VM run whose runner does not say which machine it started is stopped and fails; it never runs unrecorded.
