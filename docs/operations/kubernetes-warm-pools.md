> Part of the [Operations](../OPERATIONS.md) split (task pages under `docs/operations/`).

# Kubernetes: warm pools (experimental)

- A warm pool keeps idle placeholder pods on nodes that already hold a pool's agent image and have room for a run.
- A run that fits a pool removes one placeholder and prefers its node. Its own pods are still made fresh and checked before start.
- Off by default. Experimental. Server mode only. These are Wardyn's own placeholders, not the agent-sandbox `SandboxWarmPool`.

## Settings

| Setting | Env | Meaning |
| --- | --- | --- |
| `k8s.warmPools[].name` | `WARDYN_K8S_WARM_POOLS` (JSON) | The pool's name: a DNS label of at most 30 characters, unique. |
| `k8s.warmPools[].image` | the same JSON | The agent image. A run matches only the exact same string. |
| `k8s.warmPools[].class` | the same JSON | `CC1`, `CC2` or `CC3`. A `CC2` or `CC3` pool needs its RuntimeClass pinned. |
| `k8s.warmPools[].cpu` and `.memory` | the same JSON | The agent's room each placeholder holds. A run fits when its requests are at or below these. |
| `k8s.warmPools[].size` | the same JSON | Placeholders to keep: 0 to 20, at most 50 across all pools. |
| `k8s.warmPoolPriorityClass.create` | none | `true` by default: the chart renders a PriorityClass of value -10 that never preempts. |
| `k8s.warmPoolPriorityClass.name` | `WARDYN_K8S_WARM_PRIORITY_CLASS` | Defaults to `<fullname>-warm-placeholder`. With `create=false`, name a class you made. |

- Boot refuses a malformed value, an unpinned `CC2` or `CC3` class, and any pool under `wardyn-runnerd`.

## What a placeholder holds

| It holds | It does not hold |
| --- | --- |
| One container running the pool's image idle. | Settings, secrets, files or drives. |
| The pool's `cpu` and `memory` plus a proxy's share, with requests equal to limits. | A service-account token, or any run's labels. |
| Priority -10 from the chart's class, so any run evicts it. | Any network allowance from Wardyn. Where the namespace enforces NetworkPolicy, Wardyn adds `wardyn-warm-deny`, a policy that allows no traffic for placeholders, and it stands before the first one is made. Other matching policies and the platform's network exceptions still apply. |

- A webhook in the runs namespace can change any pod. Wardyn checks each placeholder as the API server stored it, when its create returns and on every pass. It deletes one that holds anything, has more than one container, runs another image or shares a host namespace. When a create of its name returns, it also deletes one that lost its `wardyn.component` or `wardyn.warm-pool` label, which the deny-all policy and each pass find it by. That pool then waits 5 minutes. Such a placeholder can start before it is deleted. The deny-all policy covers it unless a webhook removed its `wardyn.component` label, or put it on the host network, where most network plugins apply no policy.
- Each pass reads `wardyn-warm-deny` by name. If it is not exactly the deny-all policy, the placeholders are deleted first, and none is made until the right policy stands.

## How a run claims one

1. The run's image is the same string as a pool's, its class is the pool's, and its cpu and memory requests fit. The first such pool in the list is used.
2. Wardyn removes one Ready placeholder of that pool. The delete names the placeholder's UID and version, so two runs never take the same one.
3. Wardyn waits up to 15 seconds for the placeholder to be gone, so its room is free.
4. The run's proxy and agent pods prefer that node. It is a preference: the placement's required rules still decide, and another node is used if the room is taken.
5. The pool makes a replacement at its next pass; passes run every 10 seconds.

- A run that fits no pool, or finds no Ready placeholder, starts as it would without pools.
- The pool's image needs a working `sh` and `sleep`. Without them the placeholder's container exits and restarts. It can briefly show Ready before it exits, so a run can still claim it.

## What it saves, and when it does not

- Expected: the image download and the wait for room or a new node. No time saving has been measured.
- It does not help when the run's pods land on another node: the room was taken first, or the placement's required rules leave out the placeholder's node.
- The run's pods are still made new and checked before start. It is not instant.
- A `ResourceQuota` in the runs namespace counts placeholders. A quota refuses a pod at create and evicts nothing, so a run can be refused while placeholders hold the quota. Size it for runs plus every placeholder.

## Cost

- Each placeholder holds its pool's room plus a proxy's share, all the time.
- Size 0, or removing a pool, deletes its placeholders. Changing a pool's image deletes them too, and the pool refills after 5 minutes. Turning warm pools off deletes the leftovers at the next boot.
- Every replica reconciles every 10 seconds. Names are fixed and every delete names the exact pod it read, so replicas need no leader.

## On the agent-sandbox backend

- A placeholder is a plain pod on this backend too, checked as on the plain backend. The agent-sandbox controller never makes one.
- The run's own agent pod is still a fresh gated `Sandbox`. The placeholder only held room on a node.

## Not built

- The agent-sandbox `SandboxWarmPool` and `SandboxClaim` objects, and handing a run a pod that already ran:
  - a pod that already ran cannot pass Wardyn's check before start;
  - its secrets and proxy address cannot be added after it starts;
  - the upstream controller needs NetworkPolicy write rights in every namespace.

## Troubleshooting

| Symptom | Cause | What to do |
| --- | --- | --- |
| No placeholder appears | The egress canary has not proven NetworkPolicy in the namespace. | Fix the CNI; see [re-checks after boot](kubernetes-known-gaps.md#re-checks-after-boot). |
| No placeholder appears, and one warning shows every 5 minutes | A create was refused (a missing PriorityClass, a default PriorityClass, or a webhook), or a webhook changed the placeholder: it added something, or rewrote the image (a registry mirror or a digest pin). | Read the warning. The pool tries again after 5 minutes. A pool holds no room while a webhook rewrites its image. |
| Placeholders are made and deleted over and over | Replicas run different pool settings during a rollout. | It stops when the rollout ends. |
| Runs start on other nodes | The room was taken, or the placement leaves out the node. | Expected: see what it saves, above. |
