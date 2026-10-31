# Services — keep a command running

- A service is a background run of one command that Wardyn keeps running: it restarts the command when it exits with an error, and checks it on a port inside the sandbox.
- It lives as long as the walls around it allow. A healthy service under a lifetime cap has its lease renewed for it; an unhealthy one, or one with no cap, ends at its lease.
- Health is the sandbox's own report. Read [Health is the sandbox's claim](#health-is-the-sandboxs-claim) before you rely on it.

## Start a service

A service is a background run of a command on a container sandbox. Anything else is refused: a virtual machine with `422 request_field_unavailable`, any other shape with `422 run_service_shape`.

| Where | How |
|---|---|
| Console | New Run, Application sandbox, step 2 ([NEW-RUN.md](NEW-RUN.md#step-2-run)). Choose **Automation / script**, enter the command, then fill **Keep it running** |
| CLI | `wardyn run --experience background --command "node server.js" --service --port 8080 --health-path /healthz` |
| API | `service` on `POST /runs`, beside `experience: background` and `workload.kind: command` |

The spec is frozen on the run at create, with its defaults filled. It cannot change afterwards.

| Field | Console label | CLI flag | Default and range |
|---|---|---|---|
| `restart` | **Restart when it fails** or **Don't restart** | `--restart on-failure\|never` | `on_failure` |
| `port` | **Port it listens on** | `--port` | none; 1 to 65535, on 127.0.0.1 inside the sandbox |
| `health.path` | **Health check path** | `--health-path` | none: a TCP connect to the port. A path starts with `/` and is at most 256 URL characters |
| `health.interval_sec` | **Check every** | `--health-every` | 15 seconds; 5 to 300 |
| `health.failure_threshold` | **Unhealthy after** N **failed checks in a row** | `--health-failures` | 3; 1 to 10 |

- A `health` block needs a `port`. A `port` alone is not checked: the supervisor probes only when `health` is set.
- `port` names the port the health check connects to. It opens that port to nobody.
- To open a service's port from the run page, mark it **Callable from outside** in New Run. A background run opens only inbound ports, and its owner is always one of the callers. Inbound apps must be turned on for you.
- A service created under an idle pause gets this warning on its 201: "Your admin pauses runs nobody is using, so this service will be paused while it waits for requests."

## What runs in the sandbox

A supervisor in the image's [`deploy/images/common/agent-run-lib.sh`](../deploy/images/common/agent-run-lib.sh) runs your command inside the run's one exec, on every substrate. Wardyn does not restart anything from the control plane.

| Event | What the supervisor does |
|---|---|
| The command exits 0 on its own | The run ends COMPLETED. Nothing restarts, whatever `restart` says |
| It exits with an error, `restart: on_failure` | Waits, then starts it again. The wait is 1, 2, 4 seconds and so on, capped at 5 minutes |
| It exits with an error, `restart: never` | The run ends with the command's exit code, so FAILED |
| The command stays up 10 minutes | The next wait starts again at 1 second |
| Health fails `failure_threshold` times in a row | The service reads unhealthy. With `on_failure` the supervisor also stops the command's process group and restarts it |
| The run is stopped (TERM or INT) | Stops the process group, escalating to KILL after 10 seconds. Never restarts |

- It never gives up on `on_failure`. Only the run's end, the walls or a person stop it.
- The check is a TCP connect, or with a path an HTTP/1.0 GET on the same connection. A 2xx or 3xx status line passes, and the supervisor waits 5 seconds for it.
- A service with no `health` reads healthy while its process runs.
- It restarts the command, not the sandbox.

## What the console shows

The run list and the run page show the service's health in place of "Running", then the restarts: "Healthy · 1 restart".

| State | Reason | Means |
|---|---|---|
| Starting | none | The command has just started, or its first check has not run yet. A missing state file also reads this for the first 10 minutes |
| Healthy | none | The last check passed, or there is no check and the process is up |
| Unhealthy | `restarting` | The command exited with an error and waits to restart |
| Unhealthy | `probe_failing` | The health check failed `failure_threshold` times in a row |
| Health unknown | `supervisor_missing` | No state file after 10 minutes: the image has no supervisor. See [Images](#images) |
| Health unknown | `unreachable` | Wardyn could not read the file. The runner may be offline |

The run page's **Service** panel adds **Restart**, **Health check**, **Health** ("checked 40 s ago"), **Restarts** (with the last exit code) and **Lease**.

## Keep it alive within the walls

A service is gated as any background command is: `deny_task_mode_exec`, autonomy L3, the pool's run types, `deny_placements` and the concurrency caps. Its lifetime is the walls', and no service field widens them.

Three limits apply:

- **The cap.** The pool's `background.lifetime.max_sec` or the deployment's [`WARDYN_RUN_MAX_AGE`](ENV.md#wardyn_run_max_age), whichever is shorter, is captured as the run's lifetime cap. A run still live at the cap is killed. Nothing renews past it, and with no cap nothing renews at all: a service with no cap ends at its lease like any run, and New Run says so before launch.
- **The lease unit.** The governance profile's `max_end_ahead_sec`, or `default_end_sec` when that is 0. A service whose profile names neither is never renewed.
- **The first end.** The run starts with the end any background command gets. Renewal takes over from there.

To let a team run services for at most 30 days:

1. Create a pool whose use policy admits the team and set its background lifetime. A pool lifetime limit (or `WARDYN_RUN_MAX_AGE`) is required to keep a service up: `wardyn runner pool update <pool> --background-lifetime 24h:720h`. The flag takes DEFAULT:MAX as Go durations, so 30 days is `720h` ([Limits: the strictest wins](operations/runner-pools.md#limits-the-strictest-wins)).
2. On the team's unit profile, set `max_end_ahead_sec` to the lease you want a failing service to keep its slot for, for example 24 hours ([Three roles](operations/three-roles.md)).
3. Leave `deny_task_mode_exec` off. Set `pause_idle_after_sec` to 0, or a service that waits for requests is frozen.

### Renewal while healthy

Every minute the lease sweep looks at each running service. It renews the lease when all of these hold:

- The service is `starting` or `healthy`, and the observation is fresh. One older than 15 minutes counts as unknown.
- The run has a lifetime cap, is not paused or lost, and has an end.
- The end is within half a lease unit of now.
- The run's owner could still extend it: the check a `PATCH` does.
- For a service identity's run: the identity is not suspended or retired, and its sponsor is unchanged since the run was created and not revoked or deactivated: a session-only sign-out does not count.

Renewal moves the end to now plus the lease unit, or to the cap if that is sooner. The unit is cut to the boundary's maximum as every `PATCH` is. A lost race with a person's edit renews nothing. A ceiling tightened later lowers the unit at the next renewal; one loosened frees nothing on a run already created.

- An unhealthy or unknown service is not renewed. It ends at its lease, with the usual `run.ending_soon` warnings at 24 hours, 1 hour and 10 minutes.
- So the lease length is how long a failing service keeps its slot.
- The owner can still extend the run by hand with `PATCH /runs/{id}`, whatever its health ([Extend](operations/run-lifetime.md#extend)). No `wardyn` command does it.
- A service that stays unknown for a whole lease, such as one on an offline runner, therefore ends.
- When the cap is the limit, a healthy service is killed at the cap with the reason `max_lifetime_reached`.
- Changing a service identity's sponsor stops renewal of the services it already runs. They end at their current lease end. Relaunch or apply them again under the new sponsor.

## Health is the sandbox's claim

The supervisor writes `/home/agent/.wardyn/service.json`, and wardynd reads it every minute through the exec channel the console already uses. The file sits in the agent user's home.

| What wardynd does | Bound |
|---|---|
| Runs `cat` on the file, with fixed arguments, no environment and no terminal | 5 seconds, at most 4 KiB |
| Decodes it strictly: version 1, known keys, a health and reason the supervisor can write | Anything else reads unreachable |
| Clamps the restart count and exit code | 1,000,000 and 255 |
| Stores the status on the run | Written on a change, and at least every 5 minutes |

- Whatever runs in the sandbox can write that file. A workload that writes `healthy` is believed.
- So health decides renewal and nothing else. It cannot pass the cap, and the owner could extend the run by hand anyway.
- The reading never counts as activity, so it does not keep a run clear of an idle pause.
- Every health and restart row from this path carries `source: sandbox`.

For an admin, the cap is the real limit on a service. Set the pool's `background.lifetime.max_sec` to what you accept for a service that lies about its health.

## Images

The supervisor ships in the runner tools of the image. An image wrapped or built before this release has an `agent-run-lib.sh` without it.

- A service on such an image runs the command once. It is not restarted or checked, and no state file is written.
- After 10 minutes the run reads Health unknown, reason `supervisor_missing`. It is no longer renewed and ends at its lease.
- The run page says so: "This image's runner tools are older than services, so the command is not restarted or checked. Wrap the image again with this release."
- Re-wrap with this release: rebuild a convention image at the tag, build a pre-wrapped image again `FROM` this release's agent-base, and wrap a BYOI image again ([deploy/images/README.md](../deploy/images/README.md#bring-your-own-image-byoi)).

## Audit

| Action | When | Data |
|---|---|---|
| `run.service.health` | The health changes | `from`, `to`, `reason`, `source` |
| `run.service.restart` | The restart count rises | `restarts`, `delta`, `last_exit_code`, `source` |
| `run.end.set` | Reason `service_renewed`: a lease renewed | `from`, `to`, `max`, `source`, `health` |
| `run.create` | A service starts | `service`, the frozen spec |

At most one `run.service.health` and one `run.service.restart` row are written per run per sweep. [AUDIT-ACTIONS.md](AUDIT-ACTIONS.md) has the full rows.

## Limits

- Container sandboxes only, and only a background command.
- The health check reaches 127.0.0.1 inside the sandbox. It cannot check another host or port.
- One command per service. A command that backgrounds its own work is stopped as one process group.
- The restart wait grows to 5 minutes and never ends, so a command that always fails keeps restarting until its lease ends.
- A runner that goes offline leaves the supervisor running. Its last reading stays; once that is 15 minutes old, Health reads unknown and renewal stops.
- A paused or lost service is not renewed.
- Nothing in Wardyn renews a lease except a healthy service's own sweep and the owner's `PATCH`.
