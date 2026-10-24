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
| The agent | The Wardyn guest agent in the guest starts the harness and serves terminals, as `agent` (uid 1000). It accepts the run's setup once per boot, even after it restarts |
| Shared folders | The folders a container on this runner would get, at the same paths and mode, one unprivileged `virtiofsd` container each. No other host folder is shared, and a guest write lands as the runner's user |
| Synced folders | A folder synced from another computer lands in the guest as in a container: through the guest agent and the image's `sftp-server`, as `agent`. The agent waits for the first sync |
| Confinement | The VM boundary, which the runner treats as the `CC3` family (hardware virtualisation). The run's recorded `confinement_class` stays the one it asked for |
| Network | On a Docker runner, QEMU's only link is the network relay (`wardyn-vmnet`), which hands connections to allowed hosts to the run's proxy. A Kubernetes VM pod has no relay yet |
| Refused traffic | A port or host not allowed, a raw IP or an outside DNS resolver: a deny decision each. Dropped IPv6 and ICMP frames: one row a second per kind, with a repeat count. Global IPv6 connects find no route |
| Sign-ins | The desktop's keychain and browser profiles are kept in the guest's memory and are gone when the guest shuts down ([Sign-ins in the desktop](#sign-ins-in-the-desktop)) |

- Not in this release: drives, session recording, reviving a stopped VM, replacing its proxy, and recovering its output after the VM is gone. Each is refused with a reason.
- Linux VMs need a pool where every runner offers them; a mixed pool refuses them.
- A VM's terminal is a plain shell. Opening it in the agent CLI (`interactive_start: agent`) and watching or taking over the agent's terminal are refused too.

## Sign-ins in the desktop

- A person may sign in to websites and apps inside a VM's desktop. Those sign-ins enter the sandbox: they are kept, time-limited and audited, not kept out.
- At Init, before any exec, the guest agent mounts an empty tmpfs, the agent user's and mode `0700`, over each path in the table below.
- The guest has no swap, so the tmpfs stays in the guest's memory. It is gone when the guest shuts down: at the run's end, a kill, or a full-spool stop.
- The run's disk, kept for `WARDYN_ENDED_RUN_GRACE` after the run ends, holds none of it. A file the person saves elsewhere is on that disk, where the agent can read it.
- While the run goes on, the agent can use every session the person opened. Closing the desktop tab removes nothing.

| Path under `/home/agent` | What keeps sign-ins there |
|---|---|
| `.local/share/keyrings` | The Secret Service keychain (GNOME Keyring) |
| `.local/share/kwalletd` | KWallet |
| `.mozilla`, `.config/mozilla` | Firefox profiles |
| `.config/chromium`, `.config/google-chrome` | Chromium and Chrome profiles |

| Admin view › Providers › Agents | What a VM desktop running that agent does |
|---|---|
| Sign-ins in a desktop: Allowed (the default) | The paths are writable tmpfs. The run page offers "Pause the agent to sign in" |
| Sign-ins in a desktop: Not allowed | The paths are empty, read-only tmpfs, so the desktop keeps no sign-ins. The pause is refused (`403` `desktop_sign_ins_off`) |

- The setting is read when the run starts. An agent set Not allowed later keeps its running desktops' store, and its pause is refused from then on.
- A run whose runner or guest image predates the store reports none, and the run page says nothing about sign-ins.

### Pause the agent to sign in

1. The person presses "Pause the agent to sign in" on the run page. The guest agent stops the agent's process group (`SIGSTOP`).
2. The desktop keeps running, so the person signs in. This is not the run's freeze, which would stop the desktop too.
3. The person presses "Resume the agent". The guest agent continues the agent's process group (`SIGCONT`).

- Each pause-to-resume writes one `desktop.sign_in.pause` audit row, "paused the agent to sign in inside the desktop". It names who paused and when, never a secret.
- The pause is the person's choice. A sign-in made without it is neither paused nor audited.
- On a run whose desktop screen is recorded, the recording keeps no frames while the pause is open. A desktop that connects while it is open, after a restart too, keeps none from its first frame.
- A desktop open on another `wardynd` replica re-reads the pause every second. It stops keeping frames within 2 seconds of the pause, also when a re-read fails or does not answer, and keeps them again once a re-read after the resume answers.
- A recorded desktop that cannot read whether the agent is paused is refused (`503` `desktop_sign_ins_unavailable`).
- A process the agent moved out of its process group (`setsid`) is not stopped.
- If `wardynd` crashes after the guest agent stopped the agent but before the pause was recorded, the agent stays stopped and the run page offers the pause again. Press it, which records the pause (the agent is already stopped), then "Resume the agent".
- A stop that reaches the guest only after a failed pause has continued the agent leaves it stopped the same way, and is recovered the same way.
- A failed pause continues the agent, as a resume does. Once `wardynd` sees that it lost the run's lock meanwhile, it cancels a continue the guest agent has not received yet. Holding the lock again, it stops the agent again if another pause has marked the row since, as a continue already received may have landed after that pause's stop.
- These steps narrow the gap and do not close it. The guest agent signals each request as it handles it, in no set order, and ignores a cancel once a request has reached it. A continue it received earlier can still take effect after a later stop, even the stop again.
- A `wardynd` that exits, cannot take the lock again within 30 seconds, or fails to stop the agent again leaves such a continue in place. Nothing retries the stop, and the agent runs while the run page says paused. A failed stop again answers that the agent could not be paused. Press "Resume the agent", then pause again.

### Host memory

- QEMU does not lock guest memory (`mem-lock`): the runner starts it unprivileged, with no locked-memory allowance to pin a whole guest.
- So the runner host may page guest memory, the sign-in store with it, to its own swap or hibernation file. On a personal runner that is the developer's own disk.
- To keep guest memory off a host's disk, run VMs on a host with no swap and no hibernation.

## On a Kubernetes runner

- A VM on a Kubernetes runner is the run's pod under the `CC3` RuntimeClass, so a microVM runtime such as Kata boots it in a guest kernel of its own.
- Its network, proxy and Secret are a container run's. There is no QEMU image, `/dev/kvm` rule or probe; the node's runtime picks the virtual hardware.

| Setting | Detail |
|---|---|
| `k8s.linuxVM` | Runner chart value, `false` by default. `true` sets `WARDYN_RUNNER_LINUX_VM=1` on the runner |
| `k8s.runtimeClasses.CC3` | The RuntimeClass every VM pod runs under, whatever class the run named. It sets `CC3=<name>` in `WARDYN_CONFINEMENT_MAP`. No name is built in |
| `WARDYN_RUNNER_LINUX_VM` | `1` or unset; any other value stops the runner at boot |

1. Install a RuntimeClass whose handler boots a guest kernel, such as Kata.
2. Upgrade the runner chart with `--set k8s.runtimeClasses.CC3=<name> --set k8s.linuxVM=true`.
3. Restart the runner. It logs `Linux VM sandboxes are on` with the machine type, or `Linux VM sandboxes are off on this runner` with the reason.

- The reason is a missing `CC3` pin or a RuntimeClass that does not exist. The runner then offers no VM, and a `linux_vm` run is refused before anything is created.
- The machine recorded on the run is `runtimeclass:<handler>` in every field, so two runs compare equal only when the same handler started both.
- Drives and recording are refused, as on a Docker runner.

## When the organisation is unreachable

| Situation | Behaviour |
|---|---|
| The link is down when a VM would start | Refused `runner_offline`, as for a container. No VM, intent or run row exists |
| The VM's first lease has lapsed when it would start | Refused "runner create requires a live initial lease", as for a container. Nothing is created |
| The link drops while a VM runs | The VM keeps running with its files. Its proxy keeps enforcing the policy it holds and spools decisions |
| The run token goes unrenewed for 1h5m | The runner stops the VM's proxy. The guest keeps running with no network path |
| A credential the proxy injects from an expiring grant | Dropped at its expiry plus 15 minutes if it cannot be renewed. A fixed one (an API key) is held until the proxy stops at 1h5m |
| The VM's audit spool fills (50,000 decisions, or about 29 MiB once the proxy is refused) | The runner asks the guest to power down, keeps the stopped VM and removes the proxy. The runner's log names the run |

- These rules apply only on a runner that offers Linux VM sandboxes. A container run keeps the hold-then-`503` for a full spool.
- The full-spool stop covers Docker-hosted VMs on a person's runner. Kubernetes VMs are not covered.
- Today the console shows a run stopped for a full spool as an exited agent, with no named reason.
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
| The proxy image | Also carries the network relay (`wardyn-vmnet`), so a `proxy_image` pin in `runnerd.json` pins the relay too |

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
- It turns on no swap device, swap file or zram: `/proc/swaps` holds only its header. The guest agent refuses to initialise a guest with any swap on, and the run fails to start.
- Its keychain and browsers keep their data at the sign-in store's paths ([Sign-ins in the desktop](#sign-ins-in-the-desktop)), none of them a symbolic link. The guest agent mounts a tmpfs over each.
- It runs systemd, `qemu-guest-agent` (with `hwclock`, which sets the guest clock), and the Wardyn guest agent at `/usr/local/bin/wardyn-guest-agent` with its unit enabled.
- Rebuild the image, and so its guest agent, with each runner upgrade: an image older than the runner fails to boot, because its guest agent does not answer the runner's session handshake.
- It has the user `agent` (uid 1000, home `/home/agent`), as a container agent image does, and `/usr/lib/openssh/sftp-server`, which a folder sync writes through.
- It installs the run's CA from `/sys/firmware/qemu_fw_cfg/by_name/opt/org.wardyn/ca.pem/raw` on every boot, so a VM trusts its own run's CA and never another's.
- Its one NIC is `10.0.2.15/24`, with `10.0.2.2` as gateway and resolver (the network relay's address).

## What the host can see

| On the host | Who can use it |
|---|---|
| `<state>/vm/runs/<run>/ctl/` (QMP, qemu-ga, the guest agent, VNC) | The runner's user only (mode `0700`). VNC has no password, so this directory's mode is its control |
| `<state>/vm/runs/<run>/net/` (the network relay's socket) | The runner's user only |
| `<state>/vm/images/<image id>/` (the converted base) | The runner's user only; shared read-only by every run of that image |

- QEMU listens on no TCP port. Its serial console goes to the QEMU container's log.
- fw_cfg carries the run id and the run's CA certificates, never a key or a secret.

## The run record

- Migration `0217_run_machine_kind` adds `agent_runs.machine_kind` (empty for every container run, as before) and `agent_runs.vm_machine`.
- A Linux VM run whose runner does not say which machine it started is stopped and fails; it never runs unrecorded.
