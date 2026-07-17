<!-- Copyright 2025 The Wardyn Authors -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Fresh start: stand up a pristine local Wardyn and drive it end to end

A from-scratch bring-up on **this** box (WSL2 + two Docker daemons), written to be
copy-pasted. It is the path that has actually been walked here, including the two
gotchas that bite every time.

**Two things that will silently cost you an hour if you skip them:**

1. **Pick the right daemon.** This box runs **two**: Docker Desktop (the default
   socket, `runc` only → Fence/CC1 only) and a dedicated native daemon at
   `/run/wardyn-docker.sock` (`runc` + `runsc` + `kata` + `krun` → CC1/CC2/CC3).
   Wardyn's scripts prefer the native one automatically (`wardyn_pick_docker_host`
   in `scripts/lib/common.sh`), but anything **you** type — `docker ps`,
   `docker inspect` — hits Docker Desktop unless you export `DOCKER_HOST`. Export
   it in every shell you use, or you will inspect an empty daemon and conclude
   nothing is running.
2. **Use containerized (compose) mode.** In host mode the sandbox cannot reach
   `host.docker.internal:8080` under WSL2 NAT, so Record/Verify callbacks hang.
   The compose stack puts wardynd on the `wardyn-internal` network and the
   sandbox reaches `wardynd:8080` directly. `make setup` recommends this when it
   detects WSL2 — take the recommendation.

---

## 0. One-time: point your shell at the tier-capable daemon

```bash
cd ~/containerized-agent-envs
export DOCKER_HOST=unix:///run/wardyn-docker.sock

# Sanity: you want runsc + kata/krun here, not just runc.
docker info -f '{{range $k,$v := .Runtimes}}{{$k}} {{end}}'
# => io.containerd.runc.v2 kata krun runc runsc
```

If that prints only `runc`, you are on Docker Desktop — re-export `DOCKER_HOST`.

## 1. Clean slate (destructive — read the manifest first)

```bash
make reset-all ARGS='--dry-run'   # prints exactly what WOULD be removed
make reset-all                    # do it (WARDYN_FORCE_RESET=1 to skip the prompt)
```

Wipes the compose stack + volumes (runs, audit log, recordings) and the
`~/.wardyn` install files. It keeps `deploy/compose/.env` and your built
`:local` images (`ARGS='--purge-images'` / `'--purge-env'` to go further).

**Then clear stale leftovers it deliberately will not touch.** A dead run's
sandbox pair keeps the `wardyn-internal` network alive, and the next `compose up`
refuses it ("incorrect label"):

```bash
docker ps -aq --filter name=wardyn- | xargs -r docker rm -f
docker network ls --format '{{.Name}}' | grep '^wardyn' | xargs -r docker network rm
```

## 2. Preflight (read-only)

```bash
make doctor
```

Reports OS, daemon reachability, available confinement classes, `/dev/kvm`,
and whether `:8080` / `:5432` are free. Exits non-zero on anything blocking.

## 3. Stage your Claude login (for real agent runs)

```bash
make stage-claude
```

Stages the resident Claude credential for per-run subscription mounts. Skip it
only if you intend to run the `$0` oracle lane rather than a real agent.

## 4. Bring it up (containerized)

```bash
WARDYN_SETUP_MODE=container make setup
```

Builds, starts postgres + dex + wardynd on `wardyn-internal`, and opens the UI.
Takes a few minutes on a cold image cache.

## 5. Smoke it, bottom-up

Climb the rungs in order — each one tells you where a failure actually is:

```bash
curl -fsS http://localhost:8080/healthz          # 1. control plane: {"status":"ok"}
docker ps --format '{{.Names}}\t{{.Status}}'     # 2. containers healthy
./wardyn runs list                               # 3. CLI + auth (exit 0, empty list)
```

`wardyn` reads `WARDYN_URL` (default `http://localhost:8080`) and
`WARDYN_ADMIN_TOKEN`. Then open **http://localhost:8080**.

## 6. Drive it

**Getting Started wizard** (`/setup`) walks: pick a barrier (it reports Fence /
Wall / Vault honestly per what your daemon actually registered) → connect a model
→ onboard a workspace → review. The "done" ticks are real state, not decoration:
a workspace only goes green once its status is genuinely `ready`.

**A first run, from the CLI:**

```bash
./wardyn run --agent claude-code --repo <org/name> --task "..." --confinement CC2
./wardyn attach <run-id>          # live terminal into the sandbox
./wardyn audit <run-id> --json | head
```

**Proving containment** (this is the point of the product — check it yourself):

```bash
# the sandbox is on the second kernel, not just a namespace
docker inspect --format '{{.Name}} runtime={{.HostConfig.Runtime}}' wardyn-agent-<run-id>
# => runtime=runsc      (CC2/Wall; runc = CC1/Fence, kata/krun = CC3/Vault)

# both an allow AND a deny — a deny-everything box proves nothing
./wardyn audit <run-id> --json | grep -E 'egress.(allow|deny)'
```

Cloud metadata (`169.254.169.254`) and every private range are unconditionally
denied regardless of policy, including NAT64-smuggled forms.

## 7. Tear down

```bash
make compose-down      # stop the stack, keep the data
make reset             # wipe volumes (runs/audit/recordings), re-up
make reset-all         # full undo (see step 1)
```

---

## When it goes wrong

| Symptom | Cause | Fix |
|---|---|---|
| `compose up` fails on network labels | stale `wardyn-internal` from a dead run | the two `docker rm -f` / `network rm` lines in step 1 |
| `docker ps` shows nothing but the UI works | you are looking at Docker Desktop | `export DOCKER_HOST=unix:///run/wardyn-docker.sock` |
| Only Fence/CC1 offered | ditto — Desktop registers no `runsc`/`kata` | same |
| Verify/Record hangs forever | host mode under WSL2 NAT | use `WARDYN_SETUP_MODE=container` |
| Run fails "issue with selected model" | Claude login not staged, or stale | `make stage-claude` |
| Port 5432 in use | another Postgres | stop it, or set `WARDYN_UP_PORT` |
